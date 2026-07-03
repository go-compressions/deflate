package deflate

import (
	"bytes"
	stdflate "compress/flate"
	"io"
	"math/rand"
	"testing"
)

// roundTrip verifies our encoder/decoder and bidirectional compatibility with
// compress/flate for one input at one level.
func roundTrip(t *testing.T, data []byte, level int) {
	t.Helper()
	var buf bytes.Buffer
	w, err := NewWriter(&buf, level)
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	if _, err := w.Write(data); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	enc := buf.Bytes()

	if got, err := io.ReadAll(NewReader(bytes.NewReader(enc))); err != nil || !bytes.Equal(got, data) {
		t.Fatalf("our decode: err=%v equal=%v", err, bytes.Equal(got, data))
	}
	if got, err := io.ReadAll(stdflate.NewReader(bytes.NewReader(enc))); err != nil || !bytes.Equal(got, data) {
		t.Fatalf("flate decode of our output: err=%v equal=%v", err, bytes.Equal(got, data))
	}

	var sb bytes.Buffer
	sw, _ := stdflate.NewWriter(&sb, level)
	_, _ = sw.Write(data)
	_ = sw.Close()
	if got, err := io.ReadAll(NewReader(bytes.NewReader(sb.Bytes()))); err != nil || !bytes.Equal(got, data) {
		t.Fatalf("our decode of flate output: err=%v equal=%v", err, bytes.Equal(got, data))
	}
}

var levels = []int{NoCompression, BestSpeed, 2, 3, 4, 5, 6, 7, 8, BestCompression, DefaultCompression}

func TestRoundTripEdgeCases(t *testing.T) {
	cases := map[string][]byte{
		"empty":      nil,
		"single":     {0x42},
		"twobytes":   {1, 2},
		"text":       []byte("The quick brown fox jumps over the lazy dog."),
		"repeat":     bytes.Repeat([]byte("A"), 300),
		"repeat2":    bytes.Repeat([]byte("abcd"), 5000),
		"overwindow": bytes.Repeat([]byte("0123456789abcdef"), 5000),
		"nullbytes":  make([]byte, 1000),
		"maxmatch":   bytes.Repeat([]byte{7}, 258*4),
	}
	for name, data := range cases {
		for _, lvl := range levels {
			t.Run(name, func(t *testing.T) { roundTrip(t, data, lvl) })
		}
	}
}

func TestRoundTripStructuredFuzz(t *testing.T) {
	r := rand.New(rand.NewSource(2024))
	for iter := 0; iter < 120; iter++ {
		n := r.Intn(90000)
		data := make([]byte, n)
		switch r.Intn(5) {
		case 0:
			r.Read(data)
		case 1:
			for i := range data {
				data[i] = byte('a' + r.Intn(1+r.Intn(6)))
			}
		case 2:
			for i := 0; i < n; {
				b := byte(r.Intn(256))
				l := 1 + r.Intn(400)
				for j := 0; j < l && i < n; j++ {
					data[i] = b
					i++
				}
			}
		case 3:
			for i := range data {
				if i > 1000 && r.Intn(2) == 0 {
					data[i] = data[i-1-r.Intn(1000)]
				} else {
					data[i] = byte(r.Intn(50))
				}
			}
		case 4:
			base := []byte("the quick brown fox 0123456789 ")
			for i := range data {
				data[i] = base[i%len(base)]
			}
		}
		lvl := levels[r.Intn(len(levels))]
		roundTrip(t, data, lvl)
	}
}

func TestDeflateInflateHelpers(t *testing.T) {
	data := []byte("hello hello hello world, hello hello hello world")
	comp := Deflate(nil, data)
	got, err := Inflate(nil, comp)
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("Deflate/Inflate: err=%v equal=%v", err, bytes.Equal(got, data))
	}
	// Prefix in dst is preserved.
	comp2 := Deflate([]byte("PREFIX"), data)
	if !bytes.HasPrefix(comp2, []byte("PREFIX")) {
		t.Fatal("Deflate did not preserve dst prefix")
	}
	out, err := Inflate([]byte("HEAD"), comp)
	if err != nil || !bytes.Equal(out, append([]byte("HEAD"), data...)) {
		t.Fatalf("Inflate prefix: err=%v", err)
	}
}
