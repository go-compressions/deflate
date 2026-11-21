package deflate

import (
	stdflate "compress/flate"
	"errors"
	"io"
	"testing"
	"time"
)

// bitsOf packs (value, width) pairs LSB-first, as DEFLATE does, and pads to a
// whole byte.
func bitsOf(pairs ...[2]int) []byte {
	var out []byte
	var acc, n uint
	for _, p := range pairs {
		acc |= uint(p[0]) << n
		n += uint(p[1])
		for n >= 8 {
			out = append(out, byte(acc))
			acc >>= 8
			n -= 8
		}
	}
	if n > 0 {
		out = append(out, byte(acc))
	}
	return out
}

// TestInflateFastErrors pins which error each malformed stream gets, so that
// every refusal in the block parsers is exercised.
func TestInflateFastErrors(t *testing.T) {
	stored := func(final int, n, nn int, payload string) []byte {
		b := bitsOf([2]int{final, 1}, [2]int{0, 2})
		b = append(b, byte(n), byte(n>>8), byte(nn), byte(nn>>8))
		return append(b, payload...)
	}
	precode := func(lens ...int) []byte {
		p := [][2]int{{1, 1}, {2, 2}, {0, 5}, {0, 5}, {len(lens) - 4, 4}}
		for _, l := range lens {
			p = append(p, [2]int{l, 3})
		}
		return bitsOf(p...)
	}
	// A header whose literal/length code is valid ('a' and end-of-block, one
	// bit each) and whose offset code has the given lengths.
	offsetCode := func(dist ...uint8) []byte {
		var syms []clSym
		for i := 0; i < 257; i++ {
			l := uint8(0)
			if i == 'a' || i == 256 {
				l = 1
			}
			syms = append(syms, clSym{sym: l})
		}
		for _, l := range dist {
			syms = append(syms, clSym{sym: l})
		}
		return craftDynamicHeader(257, len(dist), syms, func(*bitWriter) {})
	}
	cases := []struct {
		name string
		src  []byte
		want error
	}{
		{"empty input", nil, io.ErrUnexpectedEOF},
		{"header only", []byte{0x01}, io.ErrUnexpectedEOF},
		{"reserved block type", bitsOf([2]int{1, 1}, [2]int{3, 2}), ErrHeader},
		{"stored, LEN/NLEN mismatch", stored(1, 5, 5, "hello"), ErrStored},
		{"stored, LEN and NLEN cut", append(bitsOf([2]int{1, 1}, [2]int{0, 2}), 5, 0), io.ErrUnexpectedEOF},
		{"stored, payload cut", stored(1, 5, 5^0xffff, "he"), io.ErrUnexpectedEOF},
		{"stored, not final, then nothing", stored(0, 2, 2^0xffff, "hi"), io.ErrUnexpectedEOF},
		{"precode over-subscribed", precode(1, 1, 1, 1), errOversubscribed},
		{"precode incomplete", precode(2, 0, 0, 0), ErrHuffman},
		{"precode cut", precode(3, 3, 3, 3)[:3], io.ErrUnexpectedEOF},
		{"precode empty", precode(0, 0, 0, 0), ErrHuffman},
		{"repeat previous with no previous", precode(1, 0, 0, 0), ErrCodeLength},
		{"repeat overruns, input cut", precode(0, 0, 1, 0), io.ErrUnexpectedEOF},
		{"lengths run into the implicit zeros", precode(0, 0, 0, 1), io.ErrUnexpectedEOF},
		{"offset code over-subscribed", offsetCode(1, 1, 1), errOversubscribed},
		{"offset code incomplete", offsetCode(2), ErrHuffman},
	}
	for _, c := range cases {
		// A decoder that ignored one of these errors would loop on the
		// implicit zero bytes forever; make that a failure, not a hang.
		type result struct {
			out []byte
			err error
		}
		done := make(chan result, 1)
		go func() {
			out, err := InflateLimit(nil, c.src, 1<<20)
			done <- result{out, err}
		}()
		var out []byte
		var err error
		select {
		case r := <-done:
			out, err = r.out, r.err
		case <-time.After(3 * time.Second):
			t.Fatalf("%s: no answer in 3s", c.name)
		}
		if !errors.Is(err, c.want) {
			t.Errorf("%s: err = %v, want %v (out %q)", c.name, err, c.want, out)
		}
	}
	// A stored block whose payload is complete decodes.
	if out, err := InflateLimit(nil, stored(1, 5, 5^0xffff, "hello"), 100); err != nil || string(out) != "hello" {
		t.Errorf("good stored block: %q, %v", out, err)
	}
}

// TestInflateFastEveryPrefixIsRefused cuts valid streams of every kind at every
// byte: the decoder must refuse each strict prefix of a stream whose last block
// is final, and never hang or return success.
func TestInflateFastEveryPrefixIsRefused(t *testing.T) {
	c := corpus()
	for _, name := range []string{"one", "abab", "abcdef", "skewed", "mixed"} {
		d := c[name]
		if len(d) > 3000 {
			d = d[:3000]
		}
		for _, lvl := range []int{stdflate.NoCompression, stdflate.BestCompression, stdflate.HuffmanOnly} {
			z := compressStd(t, d, lvl, 0)
			for k := 0; k < len(z); k++ {
				out, err := InflateLimit(nil, z[:k], 1<<20)
				if err == nil {
					t.Fatalf("%s level %d: prefix %d/%d decoded without error (%d bytes)", name, lvl, k, len(z), len(out))
				}
				// Zero bytes are fed past the end, so a cut inside a block can
				// meet a corrupt symbol before the cut is noticed (libdeflate
				// does the same); any refusal is acceptable, success is not.
			}
		}
	}
}

// TestBuildTableRefusesATableTooSmallForItsSubtables: a complete code with
// codewords longer than the table's index width needs subtables; a table that
// cannot hold them is refused rather than overrun, and the right size builds.
func TestBuildTableRefusesATableTooSmallForItsSubtables(t *testing.T) {
	// Lengths 1..14 once, 15 twice: a complete code of 16 symbols.
	lens := make([]uint8, 16)
	for i := range 14 {
		lens[i] = uint8(i + 1)
	}
	lens[14], lens[15] = 15, 15
	results := make([]uint32, 16)
	sorted := make([]uint16, 16)
	small := make([]uint32, 1<<litTableBits+1)
	if err := buildTable(small, lens, results, litTableBits, maxCodeLen, sorted, false); !errors.Is(err, ErrHuffman) {
		t.Errorf("undersized table: err = %v, want ErrHuffman", err)
	}
	if err := buildTable(make([]uint32, litEnough), lens, results, litTableBits, maxCodeLen, sorted, false); err != nil {
		t.Errorf("table of litEnough entries: %v", err)
	}
}
