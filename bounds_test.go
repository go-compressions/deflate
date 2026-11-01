package deflate

import (
	"bytes"
	stdflate "compress/flate"
	"errors"
	"io"
	"math/rand"
	"runtime"
	"testing"
)

// allocated reports the bytes f allocated: TotalAlloc before and after, the
// measurement the lzfse pilot used.
func allocated(f func()) uint64 {
	var a, b runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&a)
	f()
	runtime.ReadMemStats(&b)
	return b.TotalAlloc - a.TotalAlloc
}

// TestBomb_OneBlockDoesNotPileUpItsOutput: a single dynamic block expands
// 1032:1 and has no size limit in the format. Before the block yielded to Read,
// reading ONE byte of a 1 MB stream decoded the whole 1.03 GB block first
// (measured: 6.17 GB allocated, 2.2 GB live, 33 s); now the buffer holds the
// window plus pendingLimit whatever the block's size.
func TestBomb_OneBlockDoesNotPileUpItsOutput(t *testing.T) {
	const repeats = 20_000 // 5.2 MB out of a 5 KB stream
	stream := bombStream(repeats, true)
	want := 1 + 258*repeats

	var got int
	n := allocated(func() {
		r := NewReader(bytes.NewReader(stream))
		one := make([]byte, 1)
		if _, err := io.ReadFull(r, one); err != nil {
			t.Fatal(err)
		}
		got = 1
		rd := r.(*reader)
		if len(rd.buf) > windowSize+pendingLimit+258 {
			t.Errorf("after one byte the decoder holds %d bytes", len(rd.buf))
		}
		if !rd.inBlock {
			t.Error("the block was not interrupted")
		}
	})
	if n >= 1<<20 {
		t.Errorf("reading one byte allocated %d bytes, want < 1 MiB", n)
	}
	_ = got

	// Drained in full, with the right bytes, in constant memory.
	n = allocated(func() {
		r := NewReader(bytes.NewReader(stream))
		buf := make([]byte, 3000)
		total := 0
		for {
			k, err := r.Read(buf)
			for _, c := range buf[:k] {
				if c != 'a' {
					t.Fatalf("byte %d = %q", total, c)
				}
			}
			total += k
			if l := len(r.(*reader).buf); l > 1<<18 {
				t.Fatalf("decoder holds %d bytes", l)
			}
			if err == io.EOF {
				break
			}
			if err != nil {
				t.Fatal(err)
			}
		}
		if total != want {
			t.Errorf("drained %d bytes, want %d", total, want)
		}
	})
	if n >= 1<<20 {
		t.Errorf("draining %d bytes allocated %d bytes, want < 1 MiB", want, n)
	}
}

// TestReader_ResumesBlocksWithHistory: a block interrupted by the yield resumes
// with its tables and its window intact. compress/flate puts up to 16384 tokens
// (so MBs of output) in one block, and the data is a 997-byte pattern, so every
// match reaches back across yield points.
func TestReader_ResumesBlocksWithHistory(t *testing.T) {
	r := rand.New(rand.NewSource(11))
	pattern := make([]byte, 997)
	r.Read(pattern)
	data := bytes.Repeat(pattern, 6000) // ~6 MB
	var comp bytes.Buffer
	w, _ := stdflate.NewWriter(&comp, stdflate.BestCompression)
	w.Write(data)
	w.Close()

	for _, size := range []int{1, 7, 4096, 65536, 1 << 20} {
		rd := NewReader(bytes.NewReader(comp.Bytes()))
		var out bytes.Buffer
		buf := make([]byte, size)
		interrupted := false
		for {
			k, err := rd.Read(buf)
			out.Write(buf[:k])
			interrupted = interrupted || rd.(*reader).inBlock
			if err == io.EOF {
				break
			}
			if err != nil {
				t.Fatalf("read size %d: %v", size, err)
			}
			if size == 1 && out.Len() > 200000 { // enough: 1-byte reads are slow
				break
			}
		}
		if !interrupted {
			t.Errorf("read size %d: no block was interrupted; the test does not exercise resumption", size)
		}
		if !bytes.Equal(out.Bytes(), data[:out.Len()]) {
			t.Errorf("read size %d: output differs from the input", size)
		}
		if size != 1 && out.Len() != len(data) {
			t.Errorf("read size %d: %d bytes, want %d", size, out.Len(), len(data))
		}
	}
}

// TestInflateLimit_IsExact: at the ceiling passes, one over is refused with the
// bytes up to the ceiling, and dst is preserved.
func TestInflateLimit_IsExact(t *testing.T) {
	data := bytes.Repeat([]byte("exact ceiling "), 5000)
	for _, level := range []int{NoCompression, 1, 6, 9} {
		var comp bytes.Buffer
		w, err := NewWriter(&comp, level)
		if err != nil {
			t.Fatal(err)
		}
		w.Write(data)
		w.Close()

		prefix := []byte("keep")
		got, err := InflateLimit(prefix, comp.Bytes(), len(data))
		if err != nil || !bytes.Equal(got, append([]byte("keep"), data...)) {
			t.Errorf("level %d at the ceiling: err=%v len=%d", level, err, len(got))
		}
		got, err = InflateLimit(prefix, comp.Bytes(), len(data)-1)
		if !errors.Is(err, ErrTooLarge) || !bytes.Equal(got, append([]byte("keep"), data[:len(data)-1]...)) {
			t.Errorf("level %d one over: err=%v len=%d", level, err, len(got))
		}
	}
	// A ceiling that is not a multiple of the 4096-byte read size.
	if got, err := InflateLimit(nil, Deflate(nil, data), 4097); !errors.Is(err, ErrTooLarge) || len(got) != 4097 {
		t.Errorf("ceiling 4097: err=%v len=%d", err, len(got))
	}
	// The empty stream needs a ceiling of 0, and a negative one means 0.
	empty := Deflate(nil, nil)
	for _, limit := range []int{0, -3} {
		if got, err := InflateLimit(nil, empty, limit); err != nil || len(got) != 0 {
			t.Errorf("empty stream, limit %d: err=%v len=%d", limit, err, len(got))
		}
		if _, err := InflateLimit(nil, Deflate(nil, []byte{1}), limit); !errors.Is(err, ErrTooLarge) {
			t.Errorf("1 byte, limit %d: err = %v", limit, err)
		}
	}
	// The expanding stream is stopped by the ceiling, not after decoding it.
	bomb := bombStream(1_000_000, true) // 258 MB declared
	var err error
	n := allocated(func() { _, err = InflateLimit(nil, bomb, 1000) })
	if !errors.Is(err, ErrTooLarge) {
		t.Errorf("bomb: err = %v", err)
	}
	if n >= 1<<20 {
		t.Errorf("bomb refused after allocating %d bytes", n)
	}
}

// TestInflate_DefaultCeiling: Inflate is InflateLimit at DefaultMaxOutput.
func TestInflate_DefaultCeiling(t *testing.T) {
	if DefaultMaxOutput != 1<<30 {
		t.Fatalf("DefaultMaxOutput = %d", DefaultMaxOutput)
	}
	got, err := Inflate([]byte{1}, Deflate(nil, []byte("hi")))
	if err != nil || string(got) != "\x01hi" {
		t.Fatalf("got %q, %v", got, err)
	}
}
