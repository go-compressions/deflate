package deflate

import (
	"bytes"
	stdflate "compress/flate"
	"errors"
	"io"
	"math/rand/v2"
	"testing"
)

// corpus returns inputs that exercise literals-only, long runs (offset 1),
// short offsets (2..7), long offsets, incompressible data and mixed text.
func corpus() map[string][]byte {
	r := rand.New(rand.NewPCG(1, 2))
	c := map[string][]byte{
		"empty":  {},
		"one":    {'x'},
		"zeros":  make([]byte, 100000),
		"abab":   bytes.Repeat([]byte("ab"), 50000),
		"abcdef": bytes.Repeat([]byte("abcdefg"), 20000),
	}
	rnd := make([]byte, 70000)
	for i := range rnd {
		rnd[i] = byte(r.Uint32())
	}
	c["random"] = rnd
	text := make([]byte, 0, 300000)
	vocab := make([][]byte, 3000)
	for i := range vocab {
		w := make([]byte, 1+r.IntN(12))
		for j := range w {
			w[j] = byte('a' + r.IntN(26))
		}
		vocab[i] = w
	}
	for len(text) < 300000 {
		text = append(text, vocab[r.IntN(len(vocab))]...)
		text = append(text, ' ')
	}
	c["text"] = text
	// Skewed alphabet: long codewords, so subtables are used.
	sk := make([]byte, 200000)
	for i := range sk {
		n := 0
		for r.IntN(2) == 0 && n < 30 {
			n++
		}
		sk[i] = byte(n * 7)
	}
	c["skewed"] = sk
	mix := append(append([]byte{}, text[:50000]...), rnd[:20000]...)
	mix = append(mix, make([]byte, 40000)...)
	mix = append(mix, text[1000:60000]...)
	c["mixed"] = mix
	return c
}

func compressStd(t testing.TB, data []byte, level int, flushEvery int) []byte {
	var b bytes.Buffer
	w, err := stdflate.NewWriter(&b, level)
	if err != nil {
		t.Fatal(err)
	}
	if flushEvery == 0 {
		w.Write(data)
	} else {
		for len(data) > 0 {
			n := min(flushEvery, len(data))
			w.Write(data[:n])
			w.Flush()
			data = data[n:]
		}
	}
	w.Close()
	return b.Bytes()
}

func TestInflateFastRoundTrip(t *testing.T) {
	for name, data := range corpus() {
		for _, level := range []int{stdflate.NoCompression, stdflate.BestSpeed, 5, stdflate.BestCompression, stdflate.HuffmanOnly} {
			for _, flush := range []int{0, 997, 40000} {
				z := compressStd(t, data, level, flush)
				got, err := Inflate(nil, z)
				if err != nil {
					t.Fatalf("%s level %d flush %d: %v", name, level, flush, err)
				}
				if !bytes.Equal(got, data) {
					t.Fatalf("%s level %d flush %d: output differs", name, level, flush)
				}
				// Exactly-sized and oversized destinations take other paths
				// through the buffer growth.
				got, err = Inflate(make([]byte, 0, len(data)), z)
				if err != nil || !bytes.Equal(got, data) {
					t.Fatalf("%s level %d flush %d exact dst: %v", name, level, flush, err)
				}
				pre := []byte("prefix")
				got, err = Inflate(append([]byte{}, pre...), z)
				if err != nil || !bytes.Equal(got, append(pre, data...)) {
					t.Fatalf("%s level %d flush %d prefix dst: %v", name, level, flush, err)
				}
			}
		}
	}
}

// Whatever the bytes, Inflate and compress/flate must agree: both fail, or both
// succeed with the same output. (The only deliberate difference: a lone
// length-1 codeword decodes from both bit values, as in libdeflate.)
func TestInflateFastAgreesWithStdlibOnDamage(t *testing.T) {
	r := rand.New(rand.NewPCG(7, 8))
	data := corpus()
	for _, name := range []string{"text", "mixed", "skewed", "abcdef"} {
		z := compressStd(t, data[name][:30000], 6, 4000)
		for i := 0; i < 3000; i++ {
			c := append([]byte{}, z...)
			switch i % 3 {
			case 0:
				c[r.IntN(len(c))] ^= 1 << r.IntN(8)
			case 1:
				c = c[:r.IntN(len(c))]
			default:
				for k := 0; k < 4; k++ {
					c[r.IntN(len(c))] = byte(r.Uint32())
				}
			}
			compareWithStd(t, c)
		}
	}
}

func compareWithStd(t testing.TB, c []byte) {
	t.Helper()
	want, werr := io.ReadAll(stdflate.NewReader(bytes.NewReader(c)))
	got, gerr := Inflate(nil, c)
	if (werr == nil) != (gerr == nil) {
		// libdeflate accepts a lone length-1 codeword that zlib and Go reject.
		if werr != nil && gerr == nil {
			return
		}
		t.Fatalf("std err=%v, ours err=%v (input %x)", werr, gerr, c)
	}
	if werr == nil && !bytes.Equal(want, got) {
		t.Fatalf("outputs differ (input %x)", c)
	}
}

func TestInflateFastLimit(t *testing.T) {
	data := corpus()["text"]
	z := compressStd(t, data, 6, 0)
	for _, lim := range []int{0, 1, 7, 300, 4095, 65536, len(data) - 1} {
		got, err := InflateLimit(nil, z, lim)
		if !errors.Is(err, ErrTooLarge) {
			t.Fatalf("limit %d: err %v", lim, err)
		}
		if len(got) != lim || !bytes.Equal(got, data[:lim]) {
			t.Fatalf("limit %d: got %d bytes", lim, len(got))
		}
	}
	got, err := InflateLimit(nil, z, len(data))
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("exact limit: %v", err)
	}
}

func BenchmarkInflateFastText(b *testing.B) {
	data := corpus()["text"]
	z := compressStd(b, data, 6, 0)
	out := make([]byte, 0, len(data))
	b.SetBytes(int64(len(data)))
	b.ReportAllocs()
	for b.Loop() {
		if _, err := Inflate(out, z); err != nil {
			b.Fatal(err)
		}
	}
}

// TestInflateFastBombIsRefusedWithinBudget: a 258 MB declared expansion with a
// 16 MiB ceiling is refused with ErrTooLarge, the partial output is exactly the
// ceiling, and the decoder allocated no more than the doubling growth allows
// (at most 2x the ceiling plus slack). Run it under GOMEMLIMIT and ulimit -v.
func TestInflateFastBombIsRefusedWithinBudget(t *testing.T) {
	const ceiling = 16 << 20
	bomb := bombStream(1_000_000, true)
	var got []byte
	var err error
	n := allocated(func() { got, err = InflateLimit(nil, bomb, ceiling) })
	if !errors.Is(err, ErrTooLarge) || len(got) != ceiling {
		t.Fatalf("err=%v len=%d, want ErrTooLarge and %d bytes", err, len(got), ceiling)
	}
	if n > 3*ceiling {
		t.Errorf("allocated %d bytes for a %d ceiling", n, ceiling)
	}
}
