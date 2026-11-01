package deflate

import (
	"bytes"
	stdflate "compress/flate"
	"io"
	"testing"
)

func fuzzSeeds(f *testing.F) {
	f.Add(Deflate(nil, []byte("hello hello hello hello")))
	f.Add(Deflate(nil, bytes.Repeat([]byte{7}, 5000)))
	f.Add(bombStream(300, true))
	f.Add([]byte{0x01, 0x00, 0x00, 0xff, 0xff}) // empty stored block
	f.Add([]byte{0x05, 0xe0, 0x81})             // fixed block, garbage
}

// FuzzInflate: no input panics, nothing past the ceiling is returned, and
// where compress/flate also accepts the stream it agrees with us.
func FuzzInflate(f *testing.F) {
	fuzzSeeds(f)
	f.Fuzz(func(t *testing.T, src []byte) {
		const ceiling = 1 << 20
		out, err := InflateLimit(nil, src, ceiling)
		if len(out) > ceiling {
			t.Fatalf("returned %d bytes past the %d ceiling", len(out), ceiling)
		}
		if err != nil {
			return
		}
		ref, rerr := io.ReadAll(io.LimitReader(stdflate.NewReader(bytes.NewReader(src)), ceiling+1))
		if rerr == nil && len(ref) <= ceiling && !bytes.Equal(ref, out) {
			t.Fatalf("compress/flate decodes %d bytes, we decode %d", len(ref), len(out))
		}
	})
}

// FuzzReader: the streaming reader never panics and never buffers past its
// bound, whatever the stream, read in odd sizes.
func FuzzReader(f *testing.F) {
	fuzzSeeds(f)
	f.Fuzz(func(t *testing.T, src []byte) {
		r := NewReader(bytes.NewReader(src))
		buf := make([]byte, 1000)
		for total := 0; total < 1<<22; {
			k, err := r.Read(buf)
			total += k
			if l := len(r.(*reader).buf); l > 1<<19 {
				t.Fatalf("decoder holds %d bytes", l)
			}
			if err != nil {
				return
			}
		}
	})
}

// FuzzRoundTrip: what Deflate writes, Inflate reads back.
func FuzzRoundTrip(f *testing.F) {
	f.Add([]byte("abcabcabcabcabc"))
	f.Add(bytes.Repeat([]byte{0}, 70000))
	f.Fuzz(func(t *testing.T, in []byte) {
		got, err := Inflate(nil, Deflate(nil, in))
		if err != nil || !bytes.Equal(got, in) {
			t.Fatalf("round trip of %d bytes: err=%v", len(in), err)
		}
	})
}
