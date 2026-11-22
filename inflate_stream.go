package deflate

import (
	"bufio"
	"encoding/binary"
	"errors"
	"io"
	"sync"
)

// The streaming decoder behind [NewReader]: the table decoder of inflate_fast.go
// run over a window of the input and into a fixed output buffer.
//
//   - Input is read in chunks into a buffer (or, for a *bufio.Reader, borrowed
//     from it with Peek, so no byte is read that the stream does not use). The
//     fastloop runs while fastReadMargin bytes of the chunk remain; near its end
//     the generic loop decodes one symbol at a time, and a symbol that would
//     need bytes beyond the chunk is undone and retried once more input is in.
//   - Output goes to a buffer holding the 32 KiB window then a chunk. When the
//     chunk is full the block decoder stops, Read delivers it, and the window
//     is moved to the front. A match is never split, so a block of any size,
//     even a 1032:1 bomb, is decoded in constant memory.
//   - A block header is parsed from the bytes in hand; if it runs past them it
//     is undone and parsed again with more, so a stream flushed after a short
//     block is not waited on for bytes the writer has not sent.

const (
	streamInSize  = 1 << 16
	streamOutSize = windowSize + 1<<18
)

// streamBufs is what a reader holds while it decodes; pooled across readers.
type streamBufs struct {
	in  [streamInSize]byte
	out [streamOutSize]byte
	dec decoder
}

var streamPool = sync.Pool{New: func() any { return new(streamBufs) }}

// peeker is the part of *bufio.Reader that lends its buffer.
type peeker interface {
	Peek(n int) ([]byte, error)
	Discard(n int) (int, error)
	Buffered() int
}

type phase uint8

const (
	phHeader phase = iota
	phStored
	phHuffman
	phDone
)

// errClosed is returned by Read after Close.
var errClosed = errors.New("deflate: read on closed reader")

// streamReader is the io.ReadCloser NewReader returns for every source but a
// bare io.ByteReader.
type streamReader struct {
	r  io.Reader
	pk peeker // r as a peeker, when it is one
	sk io.Seeker

	s    state
	bufs *streamBufs
	end  int // bytes of bufs.in in use (copy mode)
	rpos int // out[rpos:s.op] awaits delivery

	phase      phase
	final      bool
	storedLeft int
	lit        *[litEnough]uint32
	off        *[offEnough]uint32

	err error // sticky; io.EOF at the end of the stream
}

func newStreamReader(r io.Reader) *streamReader {
	z := &streamReader{r: r, bufs: streamPool.Get().(*streamBufs)}
	z.s.stream = true
	z.s.out = z.bufs.out[:]
	z.s.limit = len(z.s.out)
	if pk, ok := r.(peeker); ok {
		z.pk = pk
	} else if _, ok := r.(io.ByteReader); ok {
		z.sk, _ = r.(io.Seeker)
	}
	return z
}

// release returns the buffers once nothing in them is wanted any more.
func (z *streamReader) release() {
	if z.bufs != nil {
		streamPool.Put(z.bufs)
		z.bufs, z.lit, z.off = nil, nil, nil
		z.s.out, z.s.src = nil, nil
	}
}

func (z *streamReader) Close() error {
	z.rpos, z.s.op = 0, 0
	z.release()
	if z.err == nil {
		z.err = errClosed
	}
	return nil
}

func (z *streamReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, z.err
	}
	for z.rpos >= z.s.op && z.err == nil {
		z.advance()
	}
	if z.rpos < z.s.op {
		n := copy(p, z.s.out[z.rpos:z.s.op])
		z.rpos += n
		return n, nil
	}
	z.release()
	return 0, z.err
}

// advance decodes until some output is waiting or the stream has ended.
func (z *streamReader) advance() {
	for z.rpos >= z.s.op && z.err == nil {
		z.slide()
		var err error
		switch z.phase {
		case phHeader:
			err = z.header()
		case phStored:
			err = z.stored()
		case phHuffman:
			err = z.huffman()
		default:
			z.err = io.EOF
			z.finish()
			return
		}
		switch err {
		case nil, errOutFull:
		case errNeedInput:
			if z.rpos < z.s.op {
				return // deliver what is decoded before waiting for more input
			}
			z.refill()
		default:
			z.err = err
		}
	}
}

// slide moves the window to the front of the output buffer once everything
// before it has been delivered and the chunk is nearly full.
func (z *streamReader) slide() {
	s := &z.s
	if z.rpos == s.op && len(s.out)-s.op < 2*fastWriteMargin && s.op > windowSize {
		copy(s.out, s.out[s.op-windowSize:s.op])
		s.op, z.rpos = windowSize, windowSize
	}
}

// dropPhantom forgets the zero bytes the bit buffer was topped up with at the
// end of the input window: more input may follow, so they are not real.
func (z *streamReader) dropPhantom() {
	s := &z.s
	if !s.eof && s.overread > 0 {
		nb := uint(uint8(s.bitsleft)) - 8*uint(s.overread)
		s.bitbuf &= 1<<nb - 1
		s.bitsleft = uint32(nb)
		s.overread = 0
	}
}

// header reads a block header and, for a dynamic block, its tables.
func (z *streamReader) header() error {
	s := &z.s
	in, bitbuf, bitsleft, overread := s.in, s.bitbuf, s.bitsleft, s.overread
	undo := func() error {
		s.in, s.bitbuf, s.bitsleft, s.overread = in, bitbuf, bitsleft, overread
		return errNeedInput
	}
	s.fill()
	z.final = s.bitbuf&1 == 1
	typ := (s.bitbuf >> 1) & 3
	s.bitbuf >>= 3
	s.bitsleft -= 3
	d := &z.bufs.dec
	var err error
	switch typ {
	case 0:
		// Align to the next input byte, as decoder.stored does.
		left := int(uint8(s.bitsleft)) >> 3
		s.in -= left - s.overread
		s.overread = 0
		s.bitbuf, s.bitsleft = 0, 0
		if len(s.src)-s.in < 4 {
			if !s.eof {
				return undo()
			}
			return io.ErrUnexpectedEOF
		}
		n := int(binary.LittleEndian.Uint16(s.src[s.in:]))
		nn := int(binary.LittleEndian.Uint16(s.src[s.in+2:]))
		s.in += 4
		if n != nn^0xffff {
			return ErrStored
		}
		z.storedLeft = n
		z.phase = phStored
		return nil
	case 1:
		z.lit, z.off = &staticLit, &staticOff
	case 2:
		err = d.dynamic(s)
		z.lit, z.off = &d.lit, &d.off
	default:
		err = ErrHeader
	}
	if s.truncated() {
		if !s.eof {
			return undo()
		}
		return io.ErrUnexpectedEOF
	}
	if err != nil {
		return err
	}
	z.dropPhantom()
	z.phase = phHuffman
	return nil
}

// stored copies a stored block, as much as input and output room allow.
func (z *streamReader) stored() error {
	s := &z.s
	if z.storedLeft > 0 {
		k := min(z.storedLeft, len(s.src)-s.in, len(s.out)-s.op)
		if k == 0 {
			// The output has room: advance slides first, and slide opens
			// room whenever less than a fast-loop margin is left.
			if s.eof {
				return io.ErrUnexpectedEOF
			}
			return errNeedInput
		}
		copy(s.out[s.op:], s.src[s.in:s.in+k])
		s.op += k
		s.in += k
		z.storedLeft -= k
	}
	if z.storedLeft == 0 {
		z.blockDone()
	}
	return nil
}

// huffman resumes the current Huffman block.
func (z *streamReader) huffman() error {
	err := z.bufs.dec.huffman(&z.s, z.lit, z.off)
	switch err {
	case nil:
		if z.s.truncated() {
			return io.ErrUnexpectedEOF
		}
		z.dropPhantom()
		z.blockDone()
	case errNeedInput, errOutFull:
		z.dropPhantom()
	}
	return err
}

func (z *streamReader) blockDone() {
	if z.final {
		z.phase = phDone
	} else {
		z.phase = phHeader
	}
}

// finish hands back the input the stream did not use, for sources that can take
// it: a *bufio.Reader (it was only lent) and a bytes.Reader or strings.Reader.
func (z *streamReader) finish() {
	s := &z.s
	used := s.in - (int(uint8(s.bitsleft))>>3 - s.overread)
	switch {
	case z.pk != nil:
		z.pk.Discard(used)
	case z.sk != nil:
		z.sk.Seek(int64(used-len(s.src)), io.SeekCurrent)
	}
}

// back is how many input bytes before the cursor the bit buffer holds whole.
func (z *streamReader) back() int {
	return min(z.s.in, int(uint8(z.s.bitsleft))>>3)
}

// refill brings in more input, blocking until some arrives or the source ends.
func (z *streamReader) refill() {
	s := &z.s
	if z.pk != nil {
		z.refillPeek()
		return
	}
	// The window keeps the whole bytes the bit buffer holds (stored blocks give
	// them back) and what is not consumed yet.
	in := z.bufs.in[:]
	back := z.back()
	z.end = copy(in, in[s.in-back:z.end])
	s.in = back
	for range 100 {
		n, err := z.r.Read(in[z.end:])
		z.end += n
		s.src = in[:z.end]
		switch {
		case err == io.EOF:
			s.eof = true
			return
		case err != nil:
			z.err = err
			return
		case n > 0:
			return
		}
	}
	z.err = io.ErrNoProgress
}

// refillPeek is refill for a source that lends its buffer: what the decoder
// used is discarded, what it did not is kept at the front of the next window.
func (z *streamReader) refillPeek() {
	s := &z.s
	back := z.back()
	keep := len(s.src) - s.in + back
	z.pk.Discard(s.in - back)
	s.in = back
	b, err := z.pk.Peek(keep + 1)
	if err == bufio.ErrBufferFull {
		// A buffer too small to hold the bytes kept: take them and carry on,
		// reading the source normally (it may then be read past the stream).
		in := z.bufs.in[:]
		z.end = copy(in, b)
		z.pk.Discard(len(b))
		z.pk, s.src = nil, in[:z.end]
		return
	}
	if err != nil {
		if err == io.EOF {
			s.eof = true
		} else {
			z.err = err
		}
		s.src = b
		return
	}
	s.src, _ = z.pk.Peek(z.pk.Buffered())
}
