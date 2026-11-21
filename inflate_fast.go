package deflate

import (
	"encoding/binary"
	"io"
	"sync"
)

// The whole-buffer decoder behind [Inflate] and [InflateLimit], a port of
// libdeflate's lib/deflate_decompress.c and decompress_template.h (v1.23):
//
//   - a 64-bit bit buffer refilled without a branch (one unaligned 8-byte load);
//   - one table lookup per symbol (inflate_table.go);
//   - a "fastloop" that decodes up to three literals per refill and checks
//     neither the input nor the output bound inside its body: the loop
//     condition keeps fastReadMargin bytes of input and fastWriteMargin bytes
//     of output in hand, which covers the worst iteration;
//   - word-at-a-time match copies that may write up to 39 bytes past the end of
//     the match (the margin pays for that);
//   - a generic loop, with every check, for the last bytes of either buffer.
//
// Unlike libdeflate the output size is unknown, so the generic loop also grows
// the output buffer, never past the caller's ceiling.

const (
	// Input bytes one fastloop iteration can touch: the bits a refill adds
	// (63), two fast literals, a length with subtable and extra bits (20) and
	// an offset with subtable and extra bits (28), rounded up to bytes, plus the
	// 8 bytes a refill reads past the cursor.
	fastReadMargin = (63+2*litTableBits+20+28+7)/8 + 8
	// Output bytes one iteration can write: two literals, a longest match and
	// the overrun of the five-word copy.
	fastWriteMargin = 2 + maxMatch + 5*8 - 1

	// Past this many implicit zero bytes the input is declared truncated.
	maxOverread = 8
)

// decoder holds the tables of the block being decoded; pooled across calls.
type decoder struct {
	lit    [litEnough]uint32
	off    [offEnough]uint32
	pre    [1 << preTableBits]uint32
	lens   [288 + 32]uint8
	sorted [288]uint16
}

var decoderPool = sync.Pool{New: func() any { return new(decoder) }}

// state is the bit reader and output cursor shared by the block parsers and
// the hot loop; the hot loop keeps copies in locals.
type state struct {
	src      []byte
	in       int
	bitbuf   uint64
	bitsleft uint32 // only the low byte is meaningful in the hot loop
	overread int

	out   []byte // out[:limit] is usable; out[:op] is written
	op    int
	base  int // out[:base] precedes the stream and is not a match source
	limit int
}

// fill tops the bit buffer up to at least 56 bits, a byte at a time, feeding
// zero bytes past the end of the input.
func (s *state) fill() {
	for uint8(s.bitsleft) < 56 {
		if s.in < len(s.src) {
			s.bitbuf |= uint64(s.src[s.in]) << (s.bitsleft & 63)
			s.in++
		} else {
			s.overread++
		}
		s.bitsleft += 8
	}
}

// bits returns the next n (<= 32) bits.
func (s *state) bits(n uint) uint32 {
	if uint8(s.bitsleft) < uint8(n) {
		s.fill()
	}
	v := uint32(s.bitbuf & (1<<n - 1))
	s.bitbuf >>= n
	s.bitsleft -= uint32(n)
	return v
}

// truncated reports whether bits past the end of the input were consumed.
func (s *state) truncated() bool {
	return s.overread > int(uint8(s.bitsleft))>>3 || s.overread > maxOverread
}

// room makes out[:op+n] addressable, growing the buffer up to limit. It
// reports how many of the n bytes fit.
func (s *state) room(n int) int {
	if n > s.limit-s.op {
		n = s.limit - s.op
	}
	need := s.op + n
	if need > len(s.out) {
		newLen := max(len(s.out)*2, need)
		if newLen > s.limit || newLen < 0 {
			newLen = s.limit
		}
		grown := make([]byte, newLen)
		copy(grown, s.out[:s.op])
		s.out = grown
	}
	return n
}

// inflateBuf decodes the raw DEFLATE stream src, appending to dst and never
// growing the result past maxOut bytes. On error the bytes decoded so far are
// returned with it.
func inflateBuf(dst, src []byte, maxOut int) ([]byte, error) {
	base := len(dst)
	room := max(0, maxOut)
	const maxInt = int(^uint(0) >> 1)
	if room > maxInt-base {
		room = maxInt - base
	}
	s := state{src: src, op: base, base: base, limit: base + room}
	s.out = dst[:cap(dst)]
	if len(s.out) > s.limit {
		s.out = s.out[:s.limit]
	}
	if guess := min(s.limit, base+max(4096, len(src)<<2)); len(s.out) < guess {
		grown := make([]byte, guess)
		copy(grown, dst)
		s.out = grown
	}

	d := decoderPool.Get().(*decoder)
	err := d.run(&s)
	decoderPool.Put(d)
	return s.out[:s.op], err
}

func (d *decoder) run(s *state) error {
	for {
		s.fill()
		final := s.bitbuf&1 == 1
		typ := (s.bitbuf >> 1) & 3
		s.bitbuf >>= 3
		s.bitsleft -= 3
		var lit *[litEnough]uint32
		var off *[offEnough]uint32
		switch typ {
		case 0:
			if err := d.stored(s); err != nil {
				return err
			}
			goto next
		case 1:
			lit, off = &staticLit, &staticOff
		case 2:
			if err := d.dynamic(s); err != nil {
				return err
			}
			lit, off = &d.lit, &d.off
		default:
			return ErrHeader
		}
		if err := d.huffman(s, lit, off); err != nil {
			return err
		}
	next:
		if s.truncated() {
			return io.ErrUnexpectedEOF
		}
		if final {
			return nil
		}
	}
}

// stored copies a type-0 block. The 3 header bits are already consumed.
func (d *decoder) stored(s *state) error {
	// Align to the next input byte: give back whole bytes the refill loaded
	// but nobody consumed (zero bytes past the end were never loaded). When
	// the header itself ran into the implicit zeros, s.in lands past the end
	// of the input and the length check below refuses.
	left := int(uint8(s.bitsleft)) >> 3
	s.in -= left - s.overread
	s.overread = 0
	s.bitbuf, s.bitsleft = 0, 0
	if len(s.src)-s.in < 4 {
		return io.ErrUnexpectedEOF
	}
	n := int(binary.LittleEndian.Uint16(s.src[s.in:]))
	nn := int(binary.LittleEndian.Uint16(s.src[s.in+2:]))
	s.in += 4
	if n != nn^0xffff {
		return ErrStored
	}
	avail := len(s.src) - s.in
	take := min(n, avail)
	fit := s.room(take)
	copy(s.out[s.op:], s.src[s.in:s.in+fit])
	s.op += fit
	s.in += fit
	if fit < take {
		return ErrTooLarge
	}
	if take < n {
		return io.ErrUnexpectedEOF
	}
	return nil
}

var precodeOrder = codeLengthOrder

// dynamic reads a type-2 block header and builds d.lit and d.off. The 3 header
// bits are already consumed.
func (d *decoder) dynamic(s *state) error {
	nlit := 257 + int(s.bits(5))
	noff := 1 + int(s.bits(5))
	npre := 4 + int(s.bits(4))

	var prelens [numCodeLen]uint8
	for i := 0; i < npre; i++ {
		prelens[precodeOrder[i]] = uint8(s.bits(3))
	}
	if s.truncated() {
		return io.ErrUnexpectedEOF
	}
	if err := buildTable(d.pre[:], prelens[:], preResults[:], preTableBits, 7, d.sorted[:], false); err != nil {
		return err
	}

	total := nlit + noff
	lens := d.lens[:total]
	for i := 0; i < total; {
		if uint8(s.bitsleft) < 7+7 {
			s.fill()
		}
		e := d.pre[s.bitbuf&(1<<preTableBits-1)]
		n := uint(e & 0xff)
		s.bitbuf >>= n
		s.bitsleft -= uint32(n)
		sym := e >> 16
		if sym < 16 {
			lens[i] = uint8(sym)
			i++
			continue
		}
		var rep int
		var val uint8
		switch sym {
		case 16:
			if i == 0 {
				return ErrCodeLength
			}
			val = lens[i-1]
			rep = 3 + int(s.bits(2))
		case 17:
			rep = 3 + int(s.bits(3))
		default:
			rep = 11 + int(s.bits(7))
		}
		if rep > total-i {
			if s.truncated() {
				return io.ErrUnexpectedEOF
			}
			return ErrCodeLength
		}
		for ; rep > 0; rep-- {
			lens[i] = val
			i++
		}
	}
	if s.truncated() {
		return io.ErrUnexpectedEOF
	}
	if err := buildTable(d.off[:], lens[nlit:], offResults[:], offTableBits, maxCodeLen, d.sorted[:], true); err != nil {
		return err
	}
	// The litlen code needs a table lookup per symbol, so it is built last,
	// into memory the offset table does not share.
	return buildTable(d.lit[:], lens[:nlit], litResults[:], litTableBits, maxCodeLen, d.sorted[:], false)
}

// huffman decodes one Huffman-coded block up to its end-of-block symbol.
func (d *decoder) huffman(s *state, lit *[litEnough]uint32, off *[offEnough]uint32) error {
	src := s.src
	out := s.out
	op := s.op
	in := s.in
	bitbuf := s.bitbuf
	bitsleft := s.bitsleft
	overread := s.overread
	base := s.base

	inFast := len(src) - min(len(src), fastReadMargin)
	var err error

loop:
	for {
		outFast := len(out) - min(len(out), fastWriteMargin)
		if in < inFast && op < outFast {
			// Fastloop. Refill, preload the first entry, then decode with the
			// bit buffer holding at least 56 bits at the top of each pass.
			bitbuf |= binary.LittleEndian.Uint64(src[in:]) << (bitsleft & 63)
			in += 7 - int((bitsleft>>3)&7)
			bitsleft |= 56
			entry := lit[bitbuf&(1<<litTableBits-1)]
			for in < inFast && op < outFast {
				saved := bitbuf
				bitbuf >>= entry & 63
				bitsleft -= entry
				if entry&fLiteral != 0 {
					v1 := byte(entry >> 16)
					entry = lit[bitbuf&(1<<litTableBits-1)]
					saved = bitbuf
					bitbuf >>= entry & 63
					bitsleft -= entry
					out[op] = v1
					op++
					if entry&fLiteral != 0 {
						v2 := byte(entry >> 16)
						entry = lit[bitbuf&(1<<litTableBits-1)]
						saved = bitbuf
						bitbuf >>= entry & 63
						bitsleft -= entry
						out[op] = v2
						op++
						if entry&fLiteral != 0 {
							v3 := byte(entry >> 16)
							entry = lit[bitbuf&(1<<litTableBits-1)]
							bitbuf |= binary.LittleEndian.Uint64(src[in:]) << (bitsleft & 63)
							in += 7 - int((bitsleft>>3)&7)
							bitsleft |= 56
							out[op] = v3
							op++
							continue
						}
					}
				}
				if entry&fExceptional != 0 {
					if entry&fEOB != 0 {
						break loop
					}
					if entry&fSubtable == 0 {
						err = ErrHuffman
						break loop
					}
					entry = lit[(entry>>16)+uint32(bitbuf&(1<<((entry>>8)&63)-1))]
					saved = bitbuf
					bitbuf >>= entry & 63
					bitsleft -= entry
					if entry&fLiteral != 0 {
						v := byte(entry >> 16)
						entry = lit[bitbuf&(1<<litTableBits-1)]
						bitbuf |= binary.LittleEndian.Uint64(src[in:]) << (bitsleft & 63)
						in += 7 - int((bitsleft>>3)&7)
						bitsleft |= 56
						out[op] = v
						op++
						continue
					}
					if entry&fExceptional != 0 {
						if entry&fEOB != 0 {
							break loop
						}
						err = ErrHuffman
						break loop
					}
				}

				length := int(entry>>16) + int((saved&(1<<(entry&63)-1))>>((entry>>8)&63))

				oe := off[bitbuf&(1<<offTableBits-1)]
				if oe&fExceptional != 0 {
					if oe&fSubtable == 0 {
						err = ErrHuffman
						break loop
					}
					if uint8(bitsleft) < 39 {
						bitbuf |= binary.LittleEndian.Uint64(src[in:]) << (bitsleft & 63)
						in += 7 - int((bitsleft>>3)&7)
						bitsleft |= 56
					}
					bitbuf >>= offTableBits
					bitsleft -= offTableBits
					oe = off[(oe>>16)+uint32(bitbuf&(1<<((oe>>8)&63)-1))]
					if oe&fExceptional != 0 {
						err = ErrHuffman
						break loop
					}
				} else if uint8(bitsleft) < 32 {
					bitbuf |= binary.LittleEndian.Uint64(src[in:]) << (bitsleft & 63)
					in += 7 - int((bitsleft>>3)&7)
					bitsleft |= 56
				}
				saved = bitbuf
				bitbuf >>= oe & 63
				bitsleft -= oe
				offset := int(oe>>16) + int((saved&(1<<(oe&63)-1))>>((oe>>8)&63))

				if offset > op-base {
					err = ErrDistance
					break loop
				}
				srcI := op - offset
				dstI := op
				op += length

				entry = lit[bitbuf&(1<<litTableBits-1)]
				bitbuf |= binary.LittleEndian.Uint64(src[in:]) << (bitsleft & 63)
				in += 7 - int((bitsleft>>3)&7)
				bitsleft |= 56

				switch {
				case offset >= 8:
					dw := out[dstI : dstI+24]
					sw := out[srcI : srcI+24]
					binary.LittleEndian.PutUint64(dw[0:], binary.LittleEndian.Uint64(sw[0:]))
					binary.LittleEndian.PutUint64(dw[8:], binary.LittleEndian.Uint64(sw[8:]))
					binary.LittleEndian.PutUint64(dw[16:], binary.LittleEndian.Uint64(sw[16:]))
					for i := 24; i < length; i += 40 {
						dw := out[dstI+i : dstI+i+40]
						sw := out[srcI+i : srcI+i+40]
						binary.LittleEndian.PutUint64(dw[0:], binary.LittleEndian.Uint64(sw[0:]))
						binary.LittleEndian.PutUint64(dw[8:], binary.LittleEndian.Uint64(sw[8:]))
						binary.LittleEndian.PutUint64(dw[16:], binary.LittleEndian.Uint64(sw[16:]))
						binary.LittleEndian.PutUint64(dw[24:], binary.LittleEndian.Uint64(sw[24:]))
						binary.LittleEndian.PutUint64(dw[32:], binary.LittleEndian.Uint64(sw[32:]))
					}
				case offset == 1:
					v := 0x0101010101010101 * uint64(out[srcI])
					for i := dstI; i < op; i += 32 {
						dw := out[i : i+32]
						binary.LittleEndian.PutUint64(dw[0:], v)
						binary.LittleEndian.PutUint64(dw[8:], v)
						binary.LittleEndian.PutUint64(dw[16:], v)
						binary.LittleEndian.PutUint64(dw[24:], v)
					}
				default:
					for dstI < op {
						binary.LittleEndian.PutUint64(out[dstI:], binary.LittleEndian.Uint64(out[srcI:]))
						srcI += offset
						dstI += offset
					}
				}
			}
			continue
		}

		// Generic step: one symbol, every bound checked, output grown on demand.
		if uint8(bitsleft) < 48 {
			if in+8 <= len(src) {
				bitbuf |= binary.LittleEndian.Uint64(src[in:]) << (bitsleft & 63)
				in += 7 - int((bitsleft>>3)&7)
				bitsleft |= 56
			} else {
				for uint8(bitsleft) < 56 {
					if in < len(src) {
						bitbuf |= uint64(src[in]) << (bitsleft & 63)
						in++
					} else {
						overread++
					}
					bitsleft += 8
				}
				if overread > maxOverread {
					err = io.ErrUnexpectedEOF
					break loop
				}
			}
		}
		entry := lit[bitbuf&(1<<litTableBits-1)]
		saved := bitbuf
		bitbuf >>= entry & 63
		bitsleft -= entry
		if entry&fSubtable != 0 {
			entry = lit[(entry>>16)+uint32(bitbuf&(1<<((entry>>8)&63)-1))]
			saved = bitbuf
			bitbuf >>= entry & 63
			bitsleft -= entry
		}
		if entry&fLiteral != 0 {
			if op >= len(out) {
				s.out, s.op = out, op
				if s.room(1) < 1 {
					err = ErrTooLarge
					break loop
				}
				out = s.out
			}
			out[op] = byte(entry >> 16)
			op++
			continue
		}
		if entry&fExceptional != 0 {
			if entry&fEOB != 0 {
				break loop
			}
			err = ErrHuffman
			break loop
		}
		length := int(entry>>16) + int((saved&(1<<(entry&63)-1))>>((entry>>8)&63))

		oe := off[bitbuf&(1<<offTableBits-1)]
		if oe&fExceptional != 0 {
			if oe&fSubtable == 0 {
				err = ErrHuffman
				break loop
			}
			bitbuf >>= offTableBits
			bitsleft -= offTableBits
			oe = off[(oe>>16)+uint32(bitbuf&(1<<((oe>>8)&63)-1))]
			if oe&fExceptional != 0 {
				err = ErrHuffman
				break loop
			}
		}
		saved = bitbuf
		bitbuf >>= oe & 63
		bitsleft -= oe
		offset := int(oe>>16) + int((saved&(1<<(oe&63)-1))>>((oe>>8)&63))
		if offset > op-base {
			err = ErrDistance
			break loop
		}
		s.out, s.op = out, op
		fit := s.room(length)
		out = s.out
		srcI := op - offset
		for i := 0; i < fit; i++ {
			out[op+i] = out[srcI+i]
		}
		op += fit
		if fit < length {
			err = ErrTooLarge
			break loop
		}
	}

	s.out, s.op, s.in = out, op, in
	s.bitbuf, s.bitsleft, s.overread = bitbuf, uint32(uint8(bitsleft)), overread
	return err
}
