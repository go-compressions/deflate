package deflate

import (
	"bufio"
	"bytes"
	"errors"
	"io"
)

// Errors returned by the decoder for malformed input.
var (
	ErrHeader     = errors.New("deflate: invalid block header (reserved BTYPE)")
	ErrStored     = errors.New("deflate: corrupt stored block length")
	ErrHuffman    = errors.New("deflate: invalid Huffman code")
	ErrCodeLength = errors.New("deflate: invalid code-length sequence")
	ErrDistance   = errors.New("deflate: distance too far back")
)

// reader is the streaming DEFLATE decoder returned by NewReader.
type reader struct {
	src io.ByteReader

	// bit accumulator, filled LSB first.
	b  uint32
	nb uint

	// buf holds decoded bytes: [dropped | history | undelivered]. rpos is the
	// index of the next byte to hand to Read. Bytes before rpos that are older
	// than windowSize are periodically compacted away.
	buf  []byte
	rpos int

	litHuff  huffDecoder
	distHuff huffDecoder

	final bool  // last block already started
	eof   bool  // stream fully decoded
	err   error // sticky error
}

// NewReader returns an io.ReadCloser that decompresses the raw DEFLATE (RFC
// 1951) stream read from r. It is the inverse of NewWriter and also reads
// streams produced by compress/flate. Close never returns an error; it exists
// so a *reader satisfies io.ReadCloser.
func NewReader(r io.Reader) io.ReadCloser {
	var br io.ByteReader
	if b, ok := r.(io.ByteReader); ok {
		br = b
	} else {
		br = bufio.NewReader(r)
	}
	return &reader{src: br, buf: make([]byte, 0, windowSize)}
}

func (z *reader) Close() error { return nil }

// moreBits pulls one byte into the accumulator.
func (z *reader) moreBits() error {
	c, err := z.src.ReadByte()
	if err != nil {
		if err == io.EOF {
			err = io.ErrUnexpectedEOF
		}
		return err
	}
	z.b |= uint32(c) << z.nb
	z.nb += 8
	return nil
}

// bits reads n (0..16) bits LSB first.
func (z *reader) bits(n uint) (uint32, error) {
	for z.nb < n {
		if err := z.moreBits(); err != nil {
			return 0, err
		}
	}
	v := z.b & (1<<n - 1)
	z.b >>= n
	z.nb -= n
	return v, nil
}

// decode reads one symbol using the count/symbol method.
func (z *reader) decode(h *huffDecoder) (int, error) {
	code, first, index := 0, 0, 0
	for l := 1; l <= maxCodeLen; l++ {
		if z.nb < 1 {
			if err := z.moreBits(); err != nil {
				return 0, err
			}
		}
		code |= int(z.b & 1)
		z.b >>= 1
		z.nb--
		count := int(h.count[l])
		if code-first < count {
			return int(h.symbol[index+code-first]), nil
		}
		index += count
		first += count
		first <<= 1
		code <<= 1
	}
	return 0, ErrHuffman
}

// fixed Huffman decoders are shared across all readers.
var fixedLit, fixedDist huffDecoder

func init() {
	litLengths := make([]int, 288)
	for i := 0; i < 144; i++ {
		litLengths[i] = 8
	}
	for i := 144; i < 256; i++ {
		litLengths[i] = 9
	}
	for i := 256; i < 280; i++ {
		litLengths[i] = 7
	}
	for i := 280; i < 288; i++ {
		litLengths[i] = 8
	}
	_ = fixedLit.build(litLengths)
	distLengths := make([]int, 30)
	for i := range distLengths {
		distLengths[i] = 5
	}
	_ = fixedDist.build(distLengths)
}

// readBlockHeader begins a block and prepares Huffman decoders. It returns the
// block type (0 stored, 1 fixed, 2 dynamic).
func (z *reader) readBlock() error {
	fin, err := z.bits(1)
	if err != nil {
		return err
	}
	z.final = fin == 1
	typ, err := z.bits(2)
	if err != nil {
		return err
	}
	switch typ {
	case 0:
		return z.storedBlock()
	case 1:
		z.litHuff = fixedLit
		z.distHuff = fixedDist
		return z.huffmanBlock()
	case 2:
		if err := z.dynamicTables(); err != nil {
			return err
		}
		return z.huffmanBlock()
	default:
		return ErrHeader
	}
}

// storedBlock copies a type-0 block verbatim.
func (z *reader) storedBlock() error {
	// Discard bits up to the next byte boundary.
	z.b = 0
	z.nb = 0
	nlen := [4]byte{}
	for i := 0; i < 4; i++ {
		c, err := z.src.ReadByte()
		if err != nil {
			if err == io.EOF {
				err = io.ErrUnexpectedEOF
			}
			return err
		}
		nlen[i] = c
	}
	length := int(nlen[0]) | int(nlen[1])<<8
	comp := int(nlen[2]) | int(nlen[3])<<8
	if length^0xffff != comp {
		return ErrStored
	}
	for i := 0; i < length; i++ {
		c, err := z.src.ReadByte()
		if err != nil {
			if err == io.EOF {
				err = io.ErrUnexpectedEOF
			}
			return err
		}
		z.buf = append(z.buf, c)
	}
	return nil
}

// dynamicTables reads a type-2 block header and builds its Huffman decoders.
func (z *reader) dynamicTables() error {
	hlit, err := z.bits(5)
	if err != nil {
		return err
	}
	hdist, err := z.bits(5)
	if err != nil {
		return err
	}
	hclen, err := z.bits(4)
	if err != nil {
		return err
	}
	nlit := int(hlit) + 257
	ndist := int(hdist) + 1
	ncode := int(hclen) + 4

	var clLengths [numCodeLen]int
	for i := 0; i < ncode; i++ {
		v, err := z.bits(3)
		if err != nil {
			return err
		}
		clLengths[codeLengthOrder[i]] = int(v)
	}
	var clHuff huffDecoder
	if err := clHuff.build(clLengths[:]); err != nil {
		return err
	}

	lengths := make([]int, nlit+ndist)
	for n := 0; n < len(lengths); {
		sym, err := z.decode(&clHuff)
		if err != nil {
			return err
		}
		switch {
		case sym < 16:
			lengths[n] = sym
			n++
		case sym == 16:
			if n == 0 {
				return ErrCodeLength
			}
			rep, err := z.bits(2)
			if err != nil {
				return err
			}
			cnt := int(rep) + 3
			if n+cnt > len(lengths) {
				return ErrCodeLength
			}
			prev := lengths[n-1]
			for i := 0; i < cnt; i++ {
				lengths[n] = prev
				n++
			}
		case sym == 17:
			rep, err := z.bits(3)
			if err != nil {
				return err
			}
			cnt := int(rep) + 3
			if n+cnt > len(lengths) {
				return ErrCodeLength
			}
			n += cnt
		default: // sym == 18
			rep, err := z.bits(7)
			if err != nil {
				return err
			}
			cnt := int(rep) + 11
			if n+cnt > len(lengths) {
				return ErrCodeLength
			}
			n += cnt
		}
	}
	if err := z.litHuff.build(lengths[:nlit]); err != nil {
		return err
	}
	return z.distHuff.build(lengths[nlit:])
}

// huffmanBlock decodes literals and back-references until end-of-block.
func (z *reader) huffmanBlock() error {
	for {
		sym, err := z.decode(&z.litHuff)
		if err != nil {
			return err
		}
		switch {
		case sym < 256:
			z.buf = append(z.buf, byte(sym))
		case sym == endBlock:
			return nil
		default:
			if sym > 285 {
				return ErrHuffman
			}
			li := sym - 257
			extra, err := z.bits(uint(lengthExtra[li]))
			if err != nil {
				return err
			}
			length := int(lengthBase[li]) + int(extra)

			dsym, err := z.decode(&z.distHuff)
			if err != nil {
				return err
			}
			if dsym >= numDist {
				return ErrHuffman
			}
			dextra, err := z.bits(uint(distExtra[dsym]))
			if err != nil {
				return err
			}
			dist := int(distBase[dsym]) + int(dextra)
			if dist > len(z.buf) {
				return ErrDistance
			}
			start := len(z.buf) - dist
			for i := 0; i < length; i++ {
				z.buf = append(z.buf, z.buf[start+i])
			}
		}
	}
}

// fill decodes more blocks until at least some undelivered output exists or the
// stream ends.
func (z *reader) fill() {
	for z.rpos >= len(z.buf) && !z.eof && z.err == nil {
		if z.final {
			z.eof = true
			return
		}
		if err := z.readBlock(); err != nil {
			z.err = err
			return
		}
	}
}

func (z *reader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, z.err
	}
	if z.rpos >= len(z.buf) {
		z.fill()
	}
	if z.rpos >= len(z.buf) {
		if z.eof {
			return 0, io.EOF
		}
		return 0, z.err
	}
	n := copy(p, z.buf[z.rpos:])
	z.rpos += n
	z.compact()
	return n, nil
}

// compact drops delivered bytes that are older than the back-reference window.
func (z *reader) compact() {
	if z.rpos > windowSize && len(z.buf)-z.rpos < z.rpos {
		keep := len(z.buf) - windowSize
		if keep > z.rpos {
			keep = z.rpos
		}
		if keep > 0 {
			n := copy(z.buf, z.buf[keep:])
			z.buf = z.buf[:n]
			z.rpos -= keep
		}
	}
}

// Inflate decompresses a whole DEFLATE stream from src, appending the result to
// dst and returning the extended slice.
func Inflate(dst, src []byte) ([]byte, error) {
	r := NewReader(bytes.NewReader(src))
	buf := dst
	tmp := make([]byte, 4096)
	for {
		n, err := r.Read(tmp)
		buf = append(buf, tmp[:n]...)
		if err == io.EOF {
			return buf, nil
		}
		if err != nil {
			return buf, err
		}
	}
}
