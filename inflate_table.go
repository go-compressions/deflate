package deflate

import "math/bits"

// Decode tables, after libdeflate's lib/deflate_decompress.c (v1.23): one
// table lookup per symbol, a primary table of 2^tableBits entries and, for
// codewords longer than that, subtables reached through a pointer entry.
//
// Entry layout (a uint32; bits not named are zero):
//
//	literal:    bit 31 fLiteral, bits 23-16 value, bits 3-0 codeword length
//	length:     bits 24-16 length base, bits 11-8 codeword length,
//	            bits 4-0 codeword length + extra-bit count
//	offset:     bits 31-16 offset base, bits 11-8 codeword length,
//	            bits 4-0 codeword length + extra-bit count
//	end of blk: fExceptional|fEOB, bits 3-0 codeword length
//	subtable:   fExceptional|fSubtable, bits 30-16 subtable index,
//	            bits 11-8 subtable bits, bits 3-0 primary table bits
//	invalid:    fExceptional alone (symbols 286/287 and offsets 30/31, which
//	            RFC 1951 forbids, and the empty offset code)
//
// The low byte is what the decoder shifts out of the bit buffer, so it is
// always the number of bits the entry consumes.
const (
	fLiteral     uint32 = 0x80000000
	fExceptional uint32 = 0x00008000
	fSubtable    uint32 = 0x00004000
	fEOB         uint32 = 0x00002000

	litTableBits = 11
	offTableBits = 8
	preTableBits = 7

	// Worst-case table sizes (libdeflate's LITLEN_ENOUGH and OFFSET_ENOUGH for
	// these table widths, 15-bit codewords, 288 and 32 symbols).
	litEnough = 2342
	offEnough = 402
)

// litResults and offResults give the entry for each symbol before the codeword
// length is added.
var (
	litResults [288]uint32
	offResults [32]uint32
	preResults [19]uint32

	// Static-block tables, built once and shared read-only.
	staticLit [litEnough]uint32
	staticOff [offEnough]uint32
)

func init() {
	for i := 0; i < 256; i++ {
		litResults[i] = fLiteral | uint32(i)<<16
	}
	litResults[256] = fExceptional | fEOB
	for i := 0; i < 29; i++ {
		litResults[257+i] = uint32(lengthBase[i])<<16 | uint32(lengthExtra[i])
	}
	litResults[286] = fExceptional
	litResults[287] = fExceptional
	for i := 0; i < 30; i++ {
		offResults[i] = uint32(distBase[i])<<16 | uint32(distExtra[i])
	}
	offResults[30] = fExceptional
	offResults[31] = fExceptional
	for i := range preResults {
		preResults[i] = uint32(i) << 16
	}

	var lens [288 + 32]uint8
	var sorted [288]uint16
	for i := 0; i < 144; i++ {
		lens[i] = 8
	}
	for i := 144; i < 256; i++ {
		lens[i] = 9
	}
	for i := 256; i < 280; i++ {
		lens[i] = 7
	}
	for i := 280; i < 288; i++ {
		lens[i] = 8
	}
	for i := 288; i < 320; i++ {
		lens[i] = 5
	}
	if err := buildTable(staticLit[:], lens[:288], litResults[:], litTableBits, maxCodeLen, sorted[:], false); err != nil {
		panic(err)
	}
	if err := buildTable(staticOff[:], lens[288:], offResults[:], offTableBits, maxCodeLen, sorted[:], true); err != nil {
		panic(err)
	}
}

// buildTable builds a decode table from per-symbol codeword lengths.
//
// Like libdeflate (and zlib) it rejects over-subscribed codes and every
// incomplete code except a single codeword of length 1 and, when allowEmpty,
// the empty code. A single length-1 codeword decodes from both bit values.
// lens must hold values no larger than maxLen; sorted is scratch space with
// room for len(lens) symbols.
func buildTable(table []uint32, lens []uint8, results []uint32, tableBits, maxLen uint, sorted []uint16, allowEmpty bool) error {
	var count [maxCodeLen + 2]uint32
	for _, l := range lens {
		count[l]++
	}
	count[0] = 0
	var used uint32
	for l := uint(1); l <= maxLen; l++ {
		used += count[l] << (maxLen - l)
	}
	full := uint32(1) << maxLen
	if used > full {
		return errOversubscribed
	}
	if used < full {
		var e uint32
		switch {
		case used == 0:
			if !allowEmpty {
				return ErrHuffman
			}
			e = fExceptional | 1
		case used != full>>1 || count[1] != 1:
			return ErrHuffman
		default:
			sym := 0
			for lens[sym] != 1 {
				sym++
			}
			e = results[sym] + 0x101
		}
		t := table[:1<<tableBits]
		for i := range t {
			t[i] = e
		}
		return nil
	}

	var offs [maxCodeLen + 2]uint32
	for l := uint(1); l <= maxLen; l++ {
		offs[l+1] = offs[l] + count[l]
	}
	for sym, l := range lens {
		if l != 0 {
			sorted[offs[l]] = uint16(sym)
			offs[l]++
		}
	}

	rem := count
	idx := 0
	code := uint32(0)
	next := 1 << tableBits
	curPrefix := ^uint32(0)
	var subStart int
	var subBits uint
	tmask := uint32(1)<<tableBits - 1
	for l := uint(1); l <= maxLen; l++ {
		for c := count[l]; c > 0; c-- {
			sym := sorted[idx]
			idx++
			rev := bits.Reverse32(code) >> (32 - l)
			if l <= tableBits {
				e := results[sym] + uint32(l)*0x101
				for i := rev; i < 1<<tableBits; i += 1 << l {
					table[i] = e
				}
			} else {
				prefix := rev & tmask
				if prefix != curPrefix {
					sb := l - tableBits
					left := int32(1) << sb
					for sb+tableBits < maxLen {
						left -= int32(rem[sb+tableBits])
						if left <= 0 {
							break
						}
						sb++
						left <<= 1
					}
					if next+1<<sb > len(table) {
						return ErrHuffman
					}
					curPrefix, subStart, subBits = prefix, next, sb
					table[prefix] = fExceptional | fSubtable | uint32(next)<<16 | uint32(sb)<<8 | uint32(tableBits)
					next += 1 << sb
				}
				// subBits covers every length that shares this prefix (see the
				// loop that sizes it), so rl <= subBits.
				rl := l - tableBits
				e := results[sym] + uint32(rl)*0x101
				for i := rev >> tableBits; i < 1<<subBits; i += 1 << rl {
					table[subStart+int(i)] = e
				}
			}
			rem[l]--
			code++
		}
		code <<= 1
	}
	return nil
}
