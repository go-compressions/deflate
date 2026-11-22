package deflate

import (
	"bufio"
	"bytes"
	stdflate "compress/flate"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"
	"testing/iotest"
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
	dir = filepath.Join("testdata", "fuzz", "FuzzReaderAgrees")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for i, s := range fastSeeds(t) {
		sizes := []byte{1}
		if i%3 == 1 {
			sizes = []byte{7, 200, 3}
		}
		body := fmt.Sprintf("go test fuzz v1\n[]byte(%q)\n[]byte(%q)\nuint8(%d)\n", s, sizes, i%4)
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("seed%02d", i)), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// FuzzReaderAgrees drives NewReader with read sizes and source behaviour taken
// from the fuzzer and holds it to the whole-buffer decoder, which is the same
// table decoder run once over everything: same bytes, same error.
func FuzzReaderAgrees(f *testing.F) {
	for _, s := range fastSeeds(f) {
		f.Add(s, []byte{1}, uint8(0))
		f.Add(s, []byte{7, 200, 3}, uint8(1))
		f.Add(s, []byte{255, 255, 17}, uint8(2))
	}
	f.Fuzz(func(t *testing.T, src, sizes []byte, mode uint8) {
		const ceiling = 1 << 20
		want, werr := InflateLimit(nil, src, ceiling)
		if werr == ErrTooLarge {
			return
		}
		var in io.Reader
		switch mode % 4 {
		case 0:
			in = bytes.NewReader(src)
		case 1:
			in = iotest.OneByteReader(plainReader{bytes.NewReader(src)})
		case 2:
			in = iotest.HalfReader(plainReader{bytes.NewReader(src)})
		default:
			in = bufio.NewReaderSize(bytes.NewReader(src), 16+len(sizes))
		}
		i := 0
		next := func() int {
			if len(sizes) == 0 {
				return 1
			}
			i++
			return 1 + int(sizes[i%len(sizes)])*int(sizes[(i+1)%len(sizes)])
		}
		got, err := readAllSizes(io.LimitReader(NewReader(in), ceiling+1), next)
		if len(got) > ceiling {
			return
		}
		if err != werr || !bytes.Equal(got, want) {
			t.Fatalf("stream: err=%v, %d bytes; whole buffer: err=%v, %d bytes", err, len(got), werr, len(want))
		}
	})
}

// FuzzInflateAgrees checks the whole-buffer decoder against two others: the
// bit-at-a-time reader of this package (an independent implementation) and
// compress/flate. It must also give the same bytes whatever the destination's
// capacity, and honour a ceiling exactly.
//
// Known, deliberate differences: the whole-buffer decoder follows libdeflate in
// accepting one lone length-1 codeword (decoded from both bit values) and in
// rejecting every other incomplete code, which the bit-at-a-time reader
// tolerates until it reaches the hole.
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

		ref, rerr := io.ReadAll(io.LimitReader(newByteReader(bytes.NewReader(src)), ceiling+1))
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
