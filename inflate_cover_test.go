package deflate

import (
	"errors"
	"math"
	"testing"
)

// dynStream builds a final dynamic block with the given code lengths and a
// body written with their encoders.
func dynStream(lit, dist []uint8, body func(bw *bitWriter, litEnc, distEnc *huffEncoder)) []byte {
	var litEnc, distEnc huffEncoder
	litEnc.assign(lit)
	distEnc.assign(dist)
	var syms []clSym
	for _, l := range lit {
		syms = append(syms, clSym{sym: l})
	}
	for _, l := range dist {
		syms = append(syms, clSym{sym: l})
	}
	return craftDynamicHeader(len(lit), len(dist), syms, func(bw *bitWriter) {
		body(bw, &litEnc, &distEnc)
	})
}

// TestInflateFastRefusesInvalidSymbolsReachedThroughSubtables: the length
// symbols 286 and 287 and the offsets 30 and 31 can be given codewords by a
// dynamic header; a codeword longer than the first-level table is found
// through a subtable, and using it must be refused whether the decoder is in
// its fast loop (plenty of input and output room) or in the checked one.
func TestInflateFastRefusesInvalidSymbolsReachedThroughSubtables(t *testing.T) {
	// a:1, 256:2, 257..265: 3..11, then 286 and 287 at 12: complete, and the
	// last two are longer than litTableBits.
	longLit := make([]uint8, 288)
	longLit['a'] = 1
	longLit[256] = 2
	for i := 0; i < 9; i++ {
		longLit[257+i] = uint8(3 + i)
	}
	longLit[286], longLit[287] = 12, 12
	oneDist := []uint8{1}
	for _, tc := range []struct {
		name string
		pad  int // literals around the bad symbol: many keep the fast loop running
	}{{"fast loop", 400}, {"checked loop", 0}} {
		src := dynStream(longLit, oneDist, func(bw *bitWriter, l, d *huffEncoder) {
			for range tc.pad {
				bw.writeCode(l, 'a')
			}
			bw.writeCode(l, 286)
			for range tc.pad {
				bw.writeCode(l, 'a')
			}
			bw.writeCode(l, 256)
		})
		if _, err := InflateLimit(nil, src, 1<<20); !errors.Is(err, ErrHuffman) {
			t.Errorf("length symbol 286 through a subtable, %s: err = %v", tc.name, err)
		}
	}

	// Offsets 0..7 at 1..8 bits, 30 and 31 at 9: complete, the last two reached
	// through a subtable (offTableBits is 8).
	longDist := make([]uint8, 32)
	for i := 0; i < 8; i++ {
		longDist[i] = uint8(i + 1)
	}
	longDist[30], longDist[31] = 9, 9
	shortLit := make([]uint8, 258)
	shortLit['a'], shortLit[256], shortLit[257] = 1, 2, 2
	for _, tc := range []struct {
		name string
		pad  int
	}{{"fast loop", 400}, {"checked loop", 0}} {
		src := dynStream(shortLit, longDist, func(bw *bitWriter, l, d *huffEncoder) {
			for range tc.pad + 1 {
				bw.writeCode(l, 'a')
			}
			bw.writeCode(l, 257) // length 3
			bw.writeCode(d, 30)  // offset symbol 30: invalid
			for range tc.pad {
				bw.writeCode(l, 'a')
			}
			bw.writeCode(l, 256)
		})
		if _, err := InflateLimit(nil, src, 1<<20); !errors.Is(err, ErrHuffman) {
			t.Errorf("offset symbol 30 through a subtable, %s: err = %v", tc.name, err)
		}
	}
}

// TestInflateLimitDestinationShapes: a destination with more capacity than the
// ceiling is cut back to it, and a ceiling of MaxInt after a non-empty prefix
// does not overflow.
func TestInflateLimitDestinationShapes(t *testing.T) {
	z := Deflate(nil, []byte("hello, hello, hello"))
	got, err := InflateLimit(make([]byte, 0, 1000), z, 5)
	if !errors.Is(err, ErrTooLarge) || string(got) != "hello" {
		t.Errorf("capacity above the ceiling: %q, %v", got, err)
	}
	got, err = InflateLimit([]byte("abc"), z, math.MaxInt)
	if err != nil || string(got) != "abchello, hello, hello" {
		t.Errorf("ceiling MaxInt after a prefix: %q, %v", got, err)
	}
}

func TestMustBuildPanicsOnError(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("mustBuild(err) did not panic")
		}
	}()
	mustBuild(ErrHuffman)
}
