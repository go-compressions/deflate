package deflate

import (
	"bytes"
	stdflate "compress/flate"
	"errors"
	"io"
	"math/rand"
	"testing"
)

// errByteReader is an io.ByteReader that returns n good bytes then a custom
// (non-EOF) error, to exercise the non-EOF error branches.
type errByteReader struct {
	data []byte
	pos  int
}

var errBoom = errors.New("boom")

func (r *errByteReader) ReadByte() (byte, error) {
	if r.pos >= len(r.data) {
		return 0, errBoom
	}
	b := r.data[r.pos]
	r.pos++
	return b, nil
}

// Read lets errByteReader satisfy io.Reader; NewReader uses ReadByte directly.
func (r *errByteReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	b, err := r.ReadByte()
	if err != nil {
		return 0, err
	}
	p[0] = b
	return 1, nil
}

// plainReader wraps an io.Reader hiding any ByteReader interface.
type plainReader struct{ r io.Reader }

func (p plainReader) Read(b []byte) (int, error) { return p.r.Read(b) }

func compress(t *testing.T, data []byte, level int) []byte {
	t.Helper()
	var buf bytes.Buffer
	w, _ := NewWriter(&buf, level)
	_, _ = w.Write(data)
	_ = w.Close()
	return buf.Bytes()
}

func TestReaderClose(t *testing.T) {
	r := NewReader(bytes.NewReader(compress(t, []byte("hi"), 6)))
	if err := r.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func TestNewReaderNonByteReader(t *testing.T) {
	enc := compress(t, []byte("hello world hello world"), 6)
	got, err := io.ReadAll(NewReader(plainReader{bytes.NewReader(enc)}))
	if err != nil || string(got) != "hello world hello world" {
		t.Fatalf("non-ByteReader decode: err=%v got=%q", err, got)
	}
}

func TestReadZeroLength(t *testing.T) {
	r := NewReader(bytes.NewReader(compress(t, []byte("abc"), 6)))
	n, err := r.Read(nil)
	if n != 0 || err != nil {
		t.Fatalf("Read(nil) = %d, %v", n, err)
	}
	if got, _ := io.ReadAll(r); string(got) != "abc" {
		t.Fatalf("subsequent read got %q", got)
	}
}

func TestInflateError(t *testing.T) {
	if _, err := Inflate(nil, []byte{0x07}); err == nil { // BTYPE=11 reserved
		t.Fatal("expected error from reserved block type")
	}
}

func TestReservedBlockType(t *testing.T) {
	_, err := io.ReadAll(NewReader(bytes.NewReader([]byte{0x07})))
	if err != ErrHeader {
		t.Fatalf("got %v want ErrHeader", err)
	}
}

func TestTruncatedStreamEmpty(t *testing.T) {
	_, err := io.ReadAll(NewReader(bytes.NewReader(nil)))
	if err != io.ErrUnexpectedEOF {
		t.Fatalf("got %v want ErrUnexpectedEOF", err)
	}
}

func TestStoredCorruptLength(t *testing.T) {
	// BFINAL=1 BTYPE=00 -> 0x01, then LEN=0x0001, NLEN=0x0000 (should be 0xFFFE).
	stream := []byte{0x01, 0x01, 0x00, 0x00, 0x00, 0x41}
	_, err := io.ReadAll(NewReader(bytes.NewReader(stream)))
	if err != ErrStored {
		t.Fatalf("got %v want ErrStored", err)
	}
}

func TestStoredTruncated(t *testing.T) {
	// header + truncation at each point inside the stored header/data.
	for _, s := range [][]byte{
		{0x01},                               // EOF reading NLEN
		{0x01, 0x05, 0x00, 0xFA, 0xFF},       // EOF reading data
		{0x01, 0x05, 0x00, 0xFA, 0xFF, 0x41}, // partial data
	} {
		if _, err := io.ReadAll(NewReader(bytes.NewReader(s))); err != io.ErrUnexpectedEOF {
			t.Fatalf("stream %v: got %v want ErrUnexpectedEOF", s, err)
		}
	}
}

func TestNonEOFReadError(t *testing.T) {
	// A ByteReader that fails with a non-EOF error at various offsets.
	base := compress(t, bytes.Repeat([]byte("abcdefgh"), 200), 9)
	for cut := 0; cut < len(base); cut += 3 {
		r := &errByteReader{data: base[:cut]}
		_, err := io.ReadAll(NewReader(r))
		if err == nil {
			t.Fatalf("cut %d: expected an error", cut)
		}
	}
	// Stored block hitting the custom error.
	stored := []byte{0x01, 0x05, 0x00, 0xFA, 0xFF} // needs 5 data bytes
	if _, err := io.ReadAll(NewReader(&errByteReader{data: stored})); err != errBoom {
		t.Fatalf("stored non-EOF: got %v want errBoom", err)
	}
	if _, err := io.ReadAll(NewReader(&errByteReader{data: nil})); err != errBoom {
		t.Fatalf("empty non-EOF: got %v want errBoom", err)
	}
}

func TestTruncationSweep(t *testing.T) {
	// This byte-by-byte sweep is O(n^2); skip it under -short (used by the
	// qemu-emulated CI lanes, which validate big-endian correctness instead).
	if testing.Short() {
		t.Skip("skipping exhaustive truncation sweep under -short")
	}
	// For representative streams (stored, fixed, dynamic), every truncation
	// must error rather than panic or silently succeed.
	inputs := [][]byte{
		bytes.Repeat([]byte{0x00, 0x11, 0x22, 0x33}, 20000), // random-ish -> stored/dynamic
		[]byte("hello"), // tiny -> fixed
		bytes.Repeat([]byte("The quick brown fox. "), 2000), // -> dynamic
	}
	for _, in := range inputs {
		for _, lvl := range []int{0, 6, 9} {
			full := compress(t, in, lvl)
			for cut := 0; cut < len(full); cut++ {
				func() {
					defer func() {
						if r := recover(); r != nil {
							t.Fatalf("panic on truncation cut=%d: %v", cut, r)
						}
					}()
					got, err := io.ReadAll(NewReader(bytes.NewReader(full[:cut])))
					if err == nil && bytes.Equal(got, in) {
						t.Fatalf("truncation cut=%d decoded full input", cut)
					}
				}()
			}
		}
	}
}

func TestCorruptionFuzz(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping corruption fuzz under -short")
	}
	rng := rand.New(rand.NewSource(5))
	inputs := [][]byte{
		bytes.Repeat([]byte("abcdefghij 0123456789 "), 500),
		bytes.Repeat([]byte{1, 2, 3, 4, 5, 6}, 4000),
		[]byte("small dynamic-ish block with some repetition repetition repetition"),
	}
	for _, in := range inputs {
		for _, lvl := range []int{6, 9} {
			base := compress(t, in, lvl)
			for iter := 0; iter < 4000; iter++ {
				b := append([]byte(nil), base...)
				// flip 1-3 random bits
				for f := 0; f < 1+rng.Intn(3); f++ {
					pos := rng.Intn(len(b))
					b[pos] ^= 1 << uint(rng.Intn(8))
				}
				func() {
					defer func() {
						if r := recover(); r != nil {
							t.Fatalf("panic on corruption: %v", r)
						}
					}()
					_, _ = io.ReadAll(NewReader(bytes.NewReader(b)))
				}()
			}
		}
	}
}

// --- writer error paths ---

func TestNewWriterInvalidLevel(t *testing.T) {
	for _, lvl := range []int{-2, 10, 100} {
		if _, err := NewWriter(io.Discard, lvl); err == nil {
			t.Fatalf("level %d: expected error", lvl)
		}
	}
}

type failWriter struct{ after int }

func (f *failWriter) Write(p []byte) (int, error) {
	if f.after <= 0 {
		return 0, errBoom
	}
	f.after -= len(p)
	return len(p), nil
}

func TestWriterUnderlyingError(t *testing.T) {
	w, _ := NewWriter(&failWriter{after: 0}, 6)
	if _, err := w.Write([]byte("data")); err != nil {
		t.Fatalf("Write before Close: %v", err)
	}
	if err := w.Close(); err != errBoom {
		t.Fatalf("Close: got %v want errBoom", err)
	}
	// Subsequent Close and Write observe the sticky error.
	if err := w.Close(); err != errBoom {
		t.Fatalf("second Close: got %v want errBoom", err)
	}
	if _, err := w.Write([]byte("more")); err != errBoom {
		t.Fatalf("Write after error: got %v want errBoom", err)
	}
}

func TestWriteAfterClose(t *testing.T) {
	var buf bytes.Buffer
	w, _ := NewWriter(&buf, 6)
	_, _ = w.Write([]byte("x"))
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil { // idempotent
		t.Fatalf("second Close: %v", err)
	}
	if _, err := w.Write([]byte("y")); err == nil {
		t.Fatal("Write after Close should error")
	}
}

// --- white-box Huffman tests ---

func TestBuildOversubscribed(t *testing.T) {
	var h huffDecoder
	if err := h.build([]int{1, 1, 1}); err != errOversubscribed {
		t.Fatalf("got %v want errOversubscribed", err)
	}
}

func TestBuildAllZero(t *testing.T) {
	var h huffDecoder
	if err := h.build([]int{0, 0, 0}); err != nil {
		t.Fatalf("all-zero build: %v", err)
	}
	if len(h.symbol) != 0 {
		t.Fatalf("expected empty symbol table")
	}
}

func TestDecodeErrors(t *testing.T) {
	// An incomplete code: a single length-2 symbol (canonical code 00).
	var h huffDecoder
	if err := h.build([]int{2}); err != nil {
		t.Fatalf("build: %v", err)
	}
	// Feed 16 one-bits -> walks all 15 lengths without matching -> ErrHuffman.
	z := &reader{src: bytes.NewReader([]byte{0xFF, 0xFF})}
	if _, err := z.decode(&h); err != ErrHuffman {
		t.Fatalf("mismatch: got %v want ErrHuffman", err)
	}
	// Empty source -> EOF.
	z2 := &reader{src: bytes.NewReader(nil)}
	if _, err := z2.decode(&h); err != io.ErrUnexpectedEOF {
		t.Fatalf("empty decode: got %v want ErrUnexpectedEOF", err)
	}
}

func TestCodeLengthsExtremeSkew(t *testing.T) {
	// Frequencies whose unconstrained Huffman tree is deeper than the limit,
	// forcing the over-subscription and completion repair loops.
	for _, maxBits := range []int{7, 15} {
		freq := make([]int, 40)
		f := 1
		for i := range freq {
			freq[i] = f
			f *= 2 // Fibonacci-like growth guarantees a very deep tree
		}
		lengths := codeLengthsLimited(freq, maxBits)
		var cnt [16]int
		for _, l := range lengths {
			if int(l) > maxBits {
				t.Fatalf("length %d exceeds maxBits %d", l, maxBits)
			}
			cnt[l]++
		}
		k := 0
		for l := 1; l <= maxBits; l++ {
			k += cnt[l] << (maxBits - l)
		}
		if k != 1<<maxBits {
			t.Fatalf("maxBits %d: incomplete code kraft=%d", maxBits, k)
		}
	}
}

func TestStdlibDynamicIntoOurDecoder(t *testing.T) {
	// Ensure we decode stdlib dynamic blocks (including possible empty distance
	// codes) across several inputs.
	inputs := [][]byte{
		bytes.Repeat([]byte("a"), 1000),            // few/no distances edge
		[]byte("abcdefghijklmnopqrstuvwxyz"),       // literals only
		bytes.Repeat([]byte("hello world "), 3000), // dynamic
	}
	for _, in := range inputs {
		for _, lvl := range []int{stdflate.BestSpeed, stdflate.DefaultCompression, stdflate.BestCompression} {
			var sb bytes.Buffer
			sw, _ := stdflate.NewWriter(&sb, lvl)
			_, _ = sw.Write(in)
			_ = sw.Close()
			got, err := io.ReadAll(NewReader(bytes.NewReader(sb.Bytes())))
			if err != nil || !bytes.Equal(got, in) {
				t.Fatalf("lvl %d: err=%v equal=%v", lvl, err, bytes.Equal(got, in))
			}
		}
	}
}
