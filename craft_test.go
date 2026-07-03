package deflate

import (
	"bytes"
	"io"
	"testing"
)

// clSym is one code-length-alphabet symbol with its raw extra-bit value.
type clSym struct {
	sym uint8
	nb  uint8
	val uint16
}

// craftDynamicHeader emits a dynamic-block header (BFINAL=1) declaring nlit
// literal/length and ndist distance code lengths, transmitting the given
// code-length symbol stream. It is used to build deliberately malformed
// headers. body is appended after the header (byte stream is bit-continuous).
func craftDynamicHeader(nlit, ndist int, syms []clSym, body func(bw *bitWriter)) []byte {
	freq := make([]int, numCodeLen)
	for _, s := range syms {
		freq[s.sym]++
	}
	clLen := codeLengthsLimited(freq, 7)
	var clEnc huffEncoder
	clEnc.assign(clLen)

	ncode := numCodeLen
	for ncode > 4 && clLen[codeLengthOrder[ncode-1]] == 0 {
		ncode--
	}

	var bw bitWriter
	bw.writeBits(1|(2<<1), 3) // BFINAL=1, BTYPE=10 (dynamic)
	bw.writeBits(uint32(nlit-257), 5)
	bw.writeBits(uint32(ndist-1), 5)
	bw.writeBits(uint32(ncode-4), 4)
	for i := 0; i < ncode; i++ {
		bw.writeBits(uint32(clLen[codeLengthOrder[i]]), 3)
	}
	for _, s := range syms {
		bw.writeCode(&clEnc, int(s.sym))
		if s.nb > 0 {
			bw.writeBits(uint32(s.val), uint(s.nb))
		}
	}
	if body != nil {
		body(&bw)
	}
	bw.alignByte()
	return bw.out
}

func decodeErr(b []byte) error {
	_, err := io.ReadAll(NewReader(bytes.NewReader(b)))
	return err
}

func TestCraftRepeat16Overflow(t *testing.T) {
	// nlit=257, ndist=1 -> 258 code lengths. Fill 255 zeros, one nonzero, then
	// a code 16 whose repeat runs past the end of the array.
	syms := []clSym{
		{sym: 18, nb: 7, val: 127}, // 138 zeros
		{sym: 18, nb: 7, val: 106}, // 117 zeros -> 255
		{sym: 1},                   // one length-1 entry -> 256
		{sym: 16, nb: 2, val: 3},   // repeat previous 6 times -> 262 > 258
	}
	if err := decodeErr(craftDynamicHeader(257, 1, syms, nil)); err != ErrCodeLength {
		t.Fatalf("got %v want ErrCodeLength", err)
	}
}

func TestCraftRepeat17Overflow(t *testing.T) {
	syms := []clSym{
		{sym: 18, nb: 7, val: 127}, // 138 zeros
		{sym: 18, nb: 7, val: 106}, // 117 zeros -> 255
		{sym: 17, nb: 3, val: 7},   // 10 zeros -> 265 > 258
	}
	if err := decodeErr(craftDynamicHeader(257, 1, syms, nil)); err != ErrCodeLength {
		t.Fatalf("got %v want ErrCodeLength", err)
	}
}

func TestCraftRepeat16AtStart(t *testing.T) {
	// Code 16 as the very first symbol has no previous length -> ErrCodeLength.
	syms := []clSym{{sym: 16, nb: 2, val: 0}}
	if err := decodeErr(craftDynamicHeader(257, 1, syms, nil)); err != ErrCodeLength {
		t.Fatalf("got %v want ErrCodeLength", err)
	}
}

// craftTruncatedRep builds a dynamic header ending in a repeat code (16 or 17)
// and truncates the byte stream just before the code's extra (rep) bits, so the
// decoder hits end-of-input while reading them. It searches over the number of
// leading zero-length entries to find a byte-aligned cut.
func craftTruncatedRep(t *testing.T, repSym uint8, repNB uint8) []byte {
	t.Helper()
	for pad := 0; pad < 8; pad++ {
		freq := make([]int, numCodeLen)
		freq[0] = 1
		freq[repSym] = 1
		if repSym == 16 {
			freq[1] = 1 // a nonzero entry so code 16 has a previous length
		}
		clLen := codeLengthsLimited(freq, 7)
		var clEnc huffEncoder
		clEnc.assign(clLen)
		ncode := numCodeLen
		for ncode > 4 && clLen[codeLengthOrder[ncode-1]] == 0 {
			ncode--
		}
		var bw bitWriter
		bw.writeBits(1|(2<<1), 3)
		bw.writeBits(0, 5)  // hlit -> 257
		bw.writeBits(0, 5)  // hdist -> 1
		bw.writeBits(uint32(ncode-4), 4)
		for i := 0; i < ncode; i++ {
			bw.writeBits(uint32(clLen[codeLengthOrder[i]]), 3)
		}
		// Leading entries: for code 16 we need one nonzero length first.
		if repSym == 16 {
			bw.writeCode(&clEnc, 1)
		}
		for i := 0; i < pad; i++ {
			bw.writeCode(&clEnc, 0)
		}
		bw.writeCode(&clEnc, int(repSym))
		// The rep bits are deliberately NOT written. If the code ended exactly
		// on a byte boundary (nb==0), the next read needs a new byte we omit.
		if bw.nb == 0 {
			return bw.out
		}
		_ = repNB
	}
	t.Fatalf("no byte-aligned construction for code %d", repSym)
	return nil
}

func TestCraftRepBitsTruncated(t *testing.T) {
	for _, tc := range []struct {
		sym uint8
		nb  uint8
	}{{16, 2}, {17, 3}} {
		stream := craftTruncatedRep(t, tc.sym, tc.nb)
		if err := decodeErr(stream); err != io.ErrUnexpectedEOF {
			t.Fatalf("code %d: got %v want ErrUnexpectedEOF", tc.sym, err)
		}
	}
}

func TestCraftDistanceSymbolOutOfRange(t *testing.T) {
	// Declare ndist=31 so a distance symbol 30 (>= numDist) can be encoded, then
	// use it in the block body: the decoder must reject it with ErrHuffman.
	nlit := 258
	litLen := make([]uint8, nlit)
	litLen[256] = 1 // end-of-block
	litLen[257] = 1 // length code 0 (length 3)
	ndist := 31
	distLen := make([]uint8, ndist)
	distLen[0] = 1
	distLen[30] = 1 // the out-of-range symbol

	seq := append(append([]uint8{}, litLen...), distLen...)
	syms := make([]clSym, len(seq))
	for i, v := range seq {
		syms[i] = clSym{sym: v}
	}

	body := func(bw *bitWriter) {
		var litEnc, distEnc huffEncoder
		litEnc.assign(litLen)
		distEnc.assign(distLen)
		bw.writeCode(&litEnc, 257) // a length symbol (no extra bits for code 0)
		bw.writeCode(&distEnc, 30) // distance symbol 30 -> out of range
	}
	if err := decodeErr(craftDynamicHeader(nlit, ndist, syms, body)); err != ErrHuffman {
		t.Fatalf("got %v want ErrHuffman", err)
	}
}
