package deflate

import (
	"bytes"
	stdflate "compress/flate"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"
)

// fastSeeds returns streams covering every block type and decoder path: stored,
// fixed and dynamic blocks, subtables (skewed alphabet), offset 1, short and
// long offsets, and a bomb.
func fastSeeds(t testing.TB) [][]byte {
	var seeds [][]byte
	c := corpus()
	for _, name := range []string{"empty", "one", "zeros", "abab", "abcdef", "skewed", "mixed"} {
		d := c[name]
		if len(d) > 20000 {
			d = d[:20000]
		}
		for _, lvl := range []int{stdflate.NoCompression, stdflate.BestSpeed, stdflate.BestCompression, stdflate.HuffmanOnly} {
			seeds = append(seeds, compressStd(t, d, lvl, 0), compressStd(t, d, lvl, 3001))
		}
	}
	seeds = append(seeds, bombStream(300, true))
	return seeds
}

// TestWriteFuzzSeeds regenerates the committed corpus:
// DEFLATE_WRITE_SEEDS=1 go test -run TestWriteFuzzSeeds.
func TestWriteFuzzSeeds(t *testing.T) {
	if os.Getenv("DEFLATE_WRITE_SEEDS") == "" {
		t.Skip("set DEFLATE_WRITE_SEEDS=1 to rewrite testdata/fuzz")
	}
	dir := filepath.Join("testdata", "fuzz", "FuzzInflateAgrees")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for i, s := range fastSeeds(t) {
		body := fmt.Sprintf("go test fuzz v1\n[]byte(%q)\n", s)
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("seed%02d", i)), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// FuzzInflateAgrees checks the whole-buffer decoder against two others: the
// streaming reader of this package (an independent implementation) and
// compress/flate. It must also give the same bytes whatever the destination's
// capacity, and honour a ceiling exactly.
//
// Known, deliberate differences: the whole-buffer decoder follows libdeflate in
// accepting one lone length-1 codeword (decoded from both bit values) and in
// rejecting every other incomplete code, which the streaming reader tolerates
// until it reaches the hole.
func FuzzInflateAgrees(f *testing.F) {
	for _, s := range fastSeeds(f) {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, src []byte) {
		const ceiling = 1 << 20
		out, err := InflateLimit(nil, src, ceiling)
		if len(out) > ceiling {
			t.Fatalf("returned %d bytes past the %d ceiling", len(out), ceiling)
		}
		if err == nil {
			// Same bytes into an exactly-sized and a prefixed destination.
			exact, err2 := InflateLimit(make([]byte, 0, len(out)), src, ceiling)
			if err2 != nil || !bytes.Equal(exact, out) {
				t.Fatalf("exact dst: err=%v", err2)
			}
			pre, err2 := InflateLimit([]byte("xyz"), src, ceiling)
			if err2 != nil || !bytes.Equal(pre, append([]byte("xyz"), out...)) {
				t.Fatalf("prefixed dst: err=%v", err2)
			}
			if len(out) > 0 {
				lim, err2 := InflateLimit(nil, src, len(out)-1)
				if err2 != ErrTooLarge || !bytes.Equal(lim, out[:len(out)-1]) {
					t.Fatalf("ceiling len-1: err=%v len=%d", err2, len(lim))
				}
			}
		}

		ref, rerr := io.ReadAll(io.LimitReader(NewReader(bytes.NewReader(src)), ceiling+1))
		if err == nil && rerr == nil && len(ref) <= ceiling && !bytes.Equal(ref, out) {
			t.Fatalf("streaming reader decodes %d bytes, we decode %d", len(ref), len(out))
		}
		if err != nil && rerr == nil && len(ref) <= ceiling && err != ErrHuffman && err != errOversubscribed {
			t.Fatalf("we fail with %v where the streaming reader decodes %d bytes", err, len(ref))
		}
		sref, serr := io.ReadAll(io.LimitReader(stdflate.NewReader(bytes.NewReader(src)), ceiling+1))
		if err == nil && serr == nil && len(sref) <= ceiling && !bytes.Equal(sref, out) {
			t.Fatalf("compress/flate decodes %d bytes, we decode %d", len(sref), len(out))
		}
		if err != nil && serr == nil && len(sref) <= ceiling && err != ErrHuffman && err != errOversubscribed {
			t.Fatalf("we fail with %v where compress/flate decodes %d bytes", err, len(sref))
		}
	})
}
