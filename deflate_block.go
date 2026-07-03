package deflate

// rleItem is one run-length-encoded code-length symbol for a dynamic header.
type rleItem struct {
	sym uint8
	nb  uint8  // number of extra bits
	val uint16 // extra-bits value
}

// dynHeader holds everything needed to size and emit a dynamic-block header.
type dynHeader struct {
	numLit, numD, ncode int
	clEnc               huffEncoder
	clLen               []uint8 // 19 code-length-alphabet lengths
	items               []rleItem
	bits                int // total header bit cost
}

// buildDynamic constructs the dynamic-block header for the given literal/length
// and distance code lengths.
func buildDynamic(litLen, distLen []uint8) dynHeader {
	numLit := numLitLen
	for numLit > 257 && litLen[numLit-1] == 0 {
		numLit--
	}
	numD := numDist
	for numD > 1 && distLen[numD-1] == 0 {
		numD--
	}

	// Combined code-length sequence, RLE-encoded (RFC 1951 §3.2.7).
	seq := make([]uint8, 0, numLit+numD)
	seq = append(seq, litLen[:numLit]...)
	seq = append(seq, distLen[:numD]...)

	var items []rleItem
	clFreq := make([]int, numCodeLen)
	for i := 0; i < len(seq); {
		v := seq[i]
		run := 1
		for i+run < len(seq) && seq[i+run] == v {
			run++
		}
		if v == 0 {
			for run >= 11 {
				n := run
				if n > 138 {
					n = 138
				}
				items = append(items, rleItem{sym: 18, nb: 7, val: uint16(n - 11)})
				clFreq[18]++
				run -= n
				i += n
			}
			// After the code-18 loop above, run < 11, so a single code 17
			// (3..10 zeros) covers whatever is left.
			for run >= 3 {
				n := run
				items = append(items, rleItem{sym: 17, nb: 3, val: uint16(n - 3)})
				clFreq[17]++
				run -= n
				i += n
			}
			for ; run > 0; run-- {
				items = append(items, rleItem{sym: 0})
				clFreq[0]++
				i++
			}
		} else {
			items = append(items, rleItem{sym: v})
			clFreq[v]++
			i++
			run--
			for run >= 3 {
				n := run
				if n > 6 {
					n = 6
				}
				items = append(items, rleItem{sym: 16, nb: 2, val: uint16(n - 3)})
				clFreq[16]++
				run -= n
				i += n
			}
			for ; run > 0; run-- {
				items = append(items, rleItem{sym: v})
				clFreq[v]++
				i++
			}
		}
	}

	clLen := codeLengthsLimited(clFreq, 7)
	ncode := numCodeLen
	for ncode > 4 && clLen[codeLengthOrder[ncode-1]] == 0 {
		ncode--
	}

	var clEnc huffEncoder
	clEnc.assign(clLen)

	bits := 5 + 5 + 4 + ncode*3
	for _, it := range items {
		bits += int(clLen[it.sym]) + int(it.nb)
	}

	return dynHeader{numLit: numLit, numD: numD, ncode: ncode, clEnc: clEnc, clLen: clLen, items: items, bits: bits}
}

// bodyBits returns the bit cost of a token stream (plus end-of-block) under the
// given literal/length and distance code lengths.
func bodyBits(tokens []token, litLen, distLen []uint8) int {
	bits := 0
	for _, t := range tokens {
		if t.length == 0 {
			bits += int(litLen[t.literal])
		} else {
			lc := lengthCode[t.length]
			bits += int(litLen[257+int(lc)]) + int(lengthExtra[lc])
			dc := distanceCode(int(t.dist))
			bits += int(distLen[dc]) + int(distExtra[dc])
		}
	}
	bits += int(litLen[endBlock])
	return bits
}

// fixedLitLen and fixedDistLen are the RFC fixed code lengths (for sizing).
var fixedLitLen [288]uint8
var fixedDistLen [30]uint8

func init() {
	for i := 0; i < 144; i++ {
		fixedLitLen[i] = 8
	}
	for i := 144; i < 256; i++ {
		fixedLitLen[i] = 9
	}
	for i := 256; i < 280; i++ {
		fixedLitLen[i] = 7
	}
	for i := 280; i < 288; i++ {
		fixedLitLen[i] = 8
	}
	for i := range fixedDistLen {
		fixedDistLen[i] = 5
	}
}

// writeBlock chooses the cheapest of stored, fixed and dynamic encodings for a
// token stream and emits it.
func (z *Writer) writeBlock(tokens []token, final bool, in []byte, start, end int) {
	// Frequencies for the dynamic code.
	litFreq := make([]int, numLitLen)
	distFreq := make([]int, numDist)
	for _, t := range tokens {
		if t.length == 0 {
			litFreq[t.literal]++
		} else {
			litFreq[257+int(lengthCode[t.length])]++
			distFreq[distanceCode(int(t.dist))]++
		}
	}
	litFreq[endBlock]++

	litLen := codeLengths(litFreq)
	distLen := codeLengths(distFreq)
	dyn := buildDynamic(litLen, distLen)

	dynBits := dyn.bits + bodyBits(tokens, litLen, distLen)
	fixedBits := bodyBits(tokens, fixedLitLen[:], fixedDistLen[:])

	rawLen := end - start
	// Stored: pad to byte boundary after the 3-bit header, then 4 header bytes.
	pad := (8 - int((z.bw.nb+3)%8)) % 8
	storedBits := 3 + pad + 32 + rawLen*8

	switch {
	case rawLen <= 65535 && storedBits <= dynBits && storedBits <= fixedBits:
		z.writeStored(in[start:end], final)
	case fixedBits <= dynBits:
		z.writeFixed(tokens, final)
	default:
		z.writeDynamic(tokens, final, litLen, distLen, dyn)
	}
}

func (z *Writer) writeFixed(tokens []token, final bool) {
	var bf uint32
	if final {
		bf = 1
	}
	z.bw.writeBits(bf|(1<<1), 3) // BFINAL, BTYPE=01
	z.emitTokens(tokens, &fixedLitEnc, &fixedDistEnc)
}

func (z *Writer) writeDynamic(tokens []token, final bool, litLen, distLen []uint8, dyn dynHeader) {
	var bf uint32
	if final {
		bf = 1
	}
	z.bw.writeBits(bf|(2<<1), 3) // BFINAL, BTYPE=10
	z.bw.writeBits(uint32(dyn.numLit-257), 5)
	z.bw.writeBits(uint32(dyn.numD-1), 5)
	z.bw.writeBits(uint32(dyn.ncode-4), 4)
	for i := 0; i < dyn.ncode; i++ {
		z.bw.writeBits(uint32(dyn.clLen[codeLengthOrder[i]]), 3)
	}
	for _, it := range dyn.items {
		z.bw.writeCode(&dyn.clEnc, int(it.sym))
		if it.nb > 0 {
			z.bw.writeBits(uint32(it.val), uint(it.nb))
		}
	}
	var litEnc, distEnc huffEncoder
	litEnc.assign(litLen)
	distEnc.assign(distLen)
	z.emitTokens(tokens, &litEnc, &distEnc)
}

// emitTokens writes the token stream body and the end-of-block symbol.
func (z *Writer) emitTokens(tokens []token, litEnc, distEnc *huffEncoder) {
	for _, t := range tokens {
		if t.length == 0 {
			z.bw.writeCode(litEnc, int(t.literal))
			continue
		}
		lc := lengthCode[t.length]
		z.bw.writeCode(litEnc, 257+int(lc))
		if eb := lengthExtra[lc]; eb > 0 {
			z.bw.writeBits(uint32(t.length-lengthBase[lc]), uint(eb))
		}
		dc := distanceCode(int(t.dist))
		z.bw.writeCode(distEnc, int(dc))
		if eb := distExtra[dc]; eb > 0 {
			z.bw.writeBits(uint32(int(t.dist)-int(distBase[dc])), uint(eb))
		}
	}
	z.bw.writeCode(litEnc, endBlock)
}

// Deflate compresses src at DefaultCompression, appending to dst and returning
// the extended slice.
func Deflate(dst, src []byte) []byte {
	var b sliceWriter
	b.buf = dst
	w, _ := NewWriter(&b, DefaultCompression)
	_, _ = w.Write(src)
	_ = w.Close()
	return b.buf
}

// sliceWriter is a minimal io.Writer appending to a byte slice.
type sliceWriter struct{ buf []byte }

func (s *sliceWriter) Write(p []byte) (int, error) {
	s.buf = append(s.buf, p...)
	return len(p), nil
}
