package deflate

import (
	"bufio"
	"bytes"
	stdflate "compress/flate"
	"fmt"
	"io"
	"math/rand/v2"
	"strings"
	"testing"
	"testing/iotest"
)

// readAllSizes reads r to the end with reads of sizes drawn from sizes (cycled),
// returning what it got and the error that ended it (nil for io.EOF).
func readAllSizes(r io.Reader, sizes func() int) ([]byte, error) {
	var out, buf []byte
	for {
		n := sizes()
		if n > len(buf) {
			buf = make([]byte, n)
		}
		p := buf[:n]
		n, err := r.Read(p)
		out = append(out, p[:n]...)
		if err == io.EOF {
			return out, nil
		}
		if err != nil {
			return out, err
		}
	}
}

func fixedSize(n int) func() int { return func() int { return n } }

func randomSize(seed uint64, max int) func() int {
	r := rand.New(rand.NewPCG(seed, 99))
	return func() int { return 1 + r.IntN(max) }
}

// sources wraps the same bytes in every kind of source NewReader must serve.
func sources(b []byte) map[string]func() io.Reader {
	return map[string]func() io.Reader{
		"bytes.Reader":   func() io.Reader { return bytes.NewReader(b) },
		"strings.Reader": func() io.Reader { return strings.NewReader(string(b)) },
		"bufio":          func() io.Reader { return bufio.NewReader(bytes.NewReader(b)) },
		"bufio16":        func() io.Reader { return bufio.NewReaderSize(bytes.NewReader(b), 16) },
		"plain":          func() io.Reader { return plainReader{bytes.NewReader(b)} },
		"onebyte":        func() io.Reader { return iotest.OneByteReader(plainReader{bytes.NewReader(b)}) },
		"half":           func() io.Reader { return iotest.HalfReader(plainReader{bytes.NewReader(b)}) },
		"dataerr":        func() io.Reader { return iotest.DataErrReader(plainReader{bytes.NewReader(b)}) },
		"onebyte+bufio":  func() io.Reader { return bufio.NewReader(iotest.OneByteReader(plainReader{bytes.NewReader(b)})) },
		"bytebyte":       func() io.Reader { return &errByteReader{data: b} }, // io.ByteReader only: the legacy path
	}
}

// TestStreamEqualsWholeBuffer: NewReader gives the bytes Inflate gives, whatever
// the block types, the read sizes and the way the source hands out its input.
func TestStreamEqualsWholeBuffer(t *testing.T) {
	sizes := map[string]func() func() int{
		"1":      func() func() int { return fixedSize(1) },
		"7":      func() func() int { return fixedSize(7) },
		"random": func() func() int { return randomSize(5, 70000) },
		"all":    func() func() int { return fixedSize(1 << 20) },
	}
	for name, data := range corpus() {
		for _, level := range []int{stdflate.NoCompression, stdflate.BestSpeed, 5, stdflate.BestCompression, stdflate.HuffmanOnly} {
			for _, flush := range []int{0, 997} {
				d := data
				short := len(d) > 20000
				if short {
					d = d[:20000]
				}
				z := compressStd(t, d, level, flush)
				want, err := Inflate(nil, z)
				if err != nil || !bytes.Equal(want, d) {
					t.Fatalf("%s: whole-buffer reference failed: %v", name, err)
				}
				for sname, mk := range sources(z) {
					for szname, sz := range sizes {
						if testing.Short() && (sname == "onebyte+bufio" || szname == "1" && sname != "bytes.Reader") {
							continue
						}
						got, err := readAllSizes(NewReader(mk()), sz())
						if err != nil || !bytes.Equal(got, want) {
							t.Fatalf("%s level %d flush %d source %s reads %s: err=%v, %d bytes, want %d",
								name, level, flush, sname, szname, err, len(got), len(want))
						}
					}
				}
			}
		}
	}
}

// TestStreamLarge: a stream far bigger than the output buffer, with matches that
// reach back across every window slide, in each read size.
func TestStreamLarge(t *testing.T) {
	c := corpus()
	var data []byte
	for _, k := range []string{"text", "mixed", "skewed", "random", "text", "abcdef"} {
		data = append(data, c[k]...)
	}
	z := compressStd(t, data, 6, 0)
	for _, sz := range []func() int{fixedSize(7), fixedSize(4096), randomSize(3, 100000), fixedSize(1 << 22)} {
		for _, src := range []io.Reader{bytes.NewReader(z), plainReader{bytes.NewReader(z)}, iotest.HalfReader(plainReader{bytes.NewReader(z)})} {
			if testing.Short() && sz() == 7 {
				continue
			}
			got, err := readAllSizes(NewReader(src), sz)
			if err != nil || !bytes.Equal(got, data) {
				t.Fatalf("err=%v, %d bytes, want %d", err, len(got), len(data))
			}
		}
	}
}

// TestStreamRefusals: every truncation and a sweep of corruptions give the error
// the whole-buffer decoder gives, with the same bytes before it.
func TestStreamRefusals(t *testing.T) {
	c := corpus()
	inputs := [][]byte{c["text"][:3000], c["skewed"][:3000], c["random"][:3000], c["abab"][:3000], []byte("hello hello hello"), {}}
	for i, in := range inputs {
		for _, level := range []int{stdflate.NoCompression, stdflate.BestSpeed, stdflate.BestCompression, stdflate.HuffmanOnly} {
			full := compressStd(t, in, level, 0)
			check := func(what string, b []byte) {
				t.Helper()
				want, werr := Inflate(nil, b)
				for sname, mk := range map[string]func() io.Reader{
					"bytes":   func() io.Reader { return bytes.NewReader(b) },
					"onebyte": func() io.Reader { return iotest.OneByteReader(plainReader{bytes.NewReader(b)}) },
				} {
					got, err := readAllSizes(NewReader(mk()), randomSize(uint64(i), 50))
					// Past the end of a truncated stream the whole-buffer decoder may
					// still decode symbols out of the zeros it feeds; the reader
					// stops before them, so what it returns is a prefix.
					if err != werr || !bytes.HasPrefix(want, got) || (werr == nil && len(got) != len(want)) {
						t.Fatalf("input %d level %d %s via %s: stream err=%v (%d bytes), whole err=%v (%d bytes)",
							i, level, what, sname, err, len(got), werr, len(want))
					}
				}
			}
			for cut := 0; cut < len(full); cut++ {
				check(fmt.Sprintf("cut %d", cut), full[:cut])
				if cut > 0 && level != stdflate.NoCompression {
					if _, err := Inflate(nil, full[:cut]); err == nil {
						t.Fatalf("input %d level %d: truncation at %d decoded", i, level, cut)
					}
				}
			}
			rng := rand.New(rand.NewPCG(uint64(i), uint64(level+3)))
			for range 300 {
				b := append([]byte(nil), full...)
				for range 1 + rng.IntN(3) {
					b[rng.IntN(len(b))] ^= 1 << rng.IntN(8)
				}
				check("corrupt", b)
			}
		}
	}
}

// TestStreamTrailerLeftAlone: a stream followed by something else is consumed
// exactly, for the sources that promise it, so a gzip or zlib trailer can be read
// from the same reader.
func TestStreamTrailerLeftAlone(t *testing.T) {
	data := corpus()["text"][:50000]
	for _, level := range []int{stdflate.NoCompression, stdflate.BestSpeed, stdflate.BestCompression} {
		z := compressStd(t, data, level, 0)
		for _, trailer := range []string{"", "T", "TRAILER-bytes-follow-here", strings.Repeat("x", 100000)} {
			stream := append(append([]byte(nil), z...), trailer...)
			for _, mk := range []func() (io.Reader, func() []byte){
				func() (io.Reader, func() []byte) {
					r := bytes.NewReader(stream)
					return r, func() []byte { rest, _ := io.ReadAll(r); return rest }
				},
				func() (io.Reader, func() []byte) {
					r := bufio.NewReader(bytes.NewReader(stream))
					return r, func() []byte { rest, _ := io.ReadAll(r); return rest }
				},
				func() (io.Reader, func() []byte) {
					r := strings.NewReader(string(stream))
					return r, func() []byte { rest, _ := io.ReadAll(r); return rest }
				},
				func() (io.Reader, func() []byte) {
					r := &errByteReader{data: stream}
					return r, func() []byte { return stream[r.pos:] }
				},
			} {
				src, rest := mk()
				got, err := io.ReadAll(NewReader(src))
				if err != nil || !bytes.Equal(got, data) {
					t.Fatalf("level %d: err=%v", level, err)
				}
				if string(rest()) != trailer {
					t.Fatalf("level %d trailer %d bytes: %d bytes left over", level, len(trailer), len(rest()))
				}
			}
		}
	}
}

// TestStreamInteractive: output reaches the reader as soon as the writer has
// flushed it, without waiting for input the writer has not sent: a short block
// followed by a pause must decode, headers and last symbols included.
func TestStreamInteractive(t *testing.T) {
	pr, pw := io.Pipe()
	w, _ := stdflate.NewWriter(pw, 6)
	msgs := []string{"a", "hello, world", strings.Repeat("abc", 200), "x", strings.Repeat("quick brown fox ", 40)}
	go func() {
		for _, m := range msgs {
			w.Write([]byte(m))
			w.Flush() // returns once the reader has taken the bytes
		}
		w.Close()
		pw.Close()
	}()
	r := NewReader(pr)
	for _, m := range msgs {
		got := make([]byte, len(m))
		if _, err := io.ReadFull(r, got); err != nil || string(got) != m {
			t.Fatalf("message %q: err=%v got %q", m, err, got)
		}
	}
	if n, err := r.Read(make([]byte, 1)); n != 0 || err != io.EOF {
		t.Fatalf("end: %d, %v", n, err)
	}
}

// errAfter is a plain io.Reader that fails with errBoom after its data.
type errAfter struct {
	data []byte
	err  error
}

func (e *errAfter) Read(p []byte) (int, error) {
	if len(e.data) == 0 {
		return 0, e.err
	}
	n := copy(p, e.data)
	e.data = e.data[n:]
	return n, nil
}

// TestStreamSourceErrors: an error of the source ends the stream as it is, a
// clean end of the source inside a block is an unexpected EOF, and a source
// that never makes progress is refused.
func TestStreamSourceErrors(t *testing.T) {
	base := compressStd(t, corpus()["text"][:5000], 6, 0)
	for cut := 0; cut < len(base); cut += 5 {
		if _, err := io.ReadAll(NewReader(&errAfter{data: base[:cut], err: errBoom})); err != errBoom {
			t.Fatalf("cut %d: got %v want errBoom", cut, err)
		}
		if _, err := io.ReadAll(NewReader(&errAfter{data: base[:cut], err: io.EOF})); err != io.ErrUnexpectedEOF {
			t.Fatalf("cut %d: got %v want ErrUnexpectedEOF", cut, err)
		}
	}
	stored := []byte{0x01, 0x05, 0x00, 0xFA, 0xFF, 'a', 'b'}
	if _, err := io.ReadAll(NewReader(&errAfter{data: stored, err: errBoom})); err != errBoom {
		t.Fatalf("stored: got %v", err)
	}
	if _, err := io.ReadAll(NewReader(&errAfter{err: nil})); err != io.ErrNoProgress {
		t.Fatalf("no progress: got %v", err)
	}
	// The error is sticky, and Read(nil) reports it.
	r := NewReader(&errAfter{err: errBoom})
	if _, err := r.Read(make([]byte, 1)); err != errBoom {
		t.Fatal(err)
	}
	if n, err := r.Read(make([]byte, 1)); n != 0 || err != errBoom {
		t.Fatalf("second read: %d, %v", n, err)
	}
	if n, err := r.Read(nil); n != 0 || err != errBoom {
		t.Fatalf("Read(nil): %d, %v", n, err)
	}
}

// TestStreamClose: Close ends the reader, later reads fail, and the buffers it
// gave back are not reachable through it.
func TestStreamClose(t *testing.T) {
	data := corpus()["text"][:100000]
	z := compressStd(t, data, 6, 0)
	r := NewReader(bytes.NewReader(z))
	p := make([]byte, 10)
	if _, err := io.ReadFull(r, p); err != nil {
		t.Fatal(err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	// Another reader takes the pooled buffers and decodes while the first is poked.
	other, err := io.ReadAll(NewReader(bytes.NewReader(z)))
	if err != nil || !bytes.Equal(other, data) {
		t.Fatalf("other reader: %v", err)
	}
	if n, err := r.Read(p); n != 0 || err == nil {
		t.Fatalf("Read after Close: %d, %v", n, err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	// Close after the end, and a drained reader releasing its buffers, are fine.
	r = NewReader(bytes.NewReader(z))
	if _, err := io.ReadAll(r); err != nil {
		t.Fatal(err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	if n, err := r.Read(p); n != 0 || err != io.EOF {
		t.Fatalf("Read after the end: %d, %v", n, err)
	}
}

// TestStreamErrorsAreSticky: after a corrupt stream, every read repeats the error.
func TestStreamErrorsAreSticky(t *testing.T) {
	r := NewReader(bytes.NewReader([]byte{0x07}))
	for range 3 {
		if n, err := r.Read(make([]byte, 4)); n != 0 || err != ErrHeader {
			t.Fatalf("got %d, %v", n, err)
		}
	}
}

// TestStreamStoredAcrossChunks: a stored block larger than the output buffer and
// the input window, split in both by tiny reads, and a run of empty blocks.
func TestStreamStoredAcrossChunks(t *testing.T) {
	r := rand.New(rand.NewPCG(8, 8))
	data := make([]byte, 300000)
	for i := range data {
		data[i] = byte(r.Uint32())
	}
	var z bytes.Buffer
	for i := 0; i < len(data); i += 65535 {
		n := min(65535, len(data)-i)
		final := byte(0)
		if i+n == len(data) {
			final = 1
		}
		z.Write([]byte{final, byte(n), byte(n >> 8), ^byte(n), ^byte(n >> 8)})
		z.Write(data[i : i+n])
		z.Write([]byte{0, 0, 0, 0xff, 0xff}) // an empty stored block between
	}
	z.Write([]byte{1, 0, 0, 0xff, 0xff})
	for _, src := range []io.Reader{bytes.NewReader(z.Bytes()), iotest.HalfReader(plainReader{bytes.NewReader(z.Bytes())})} {
		for _, sz := range []func() int{fixedSize(1 << 20), randomSize(2, 90000), fixedSize(9999)} {
			got, err := readAllSizes(NewReader(src), sz)
			if err != nil || !bytes.Equal(got, data) {
				t.Fatalf("err=%v %d bytes", err, len(got))
			}
			src = bytes.NewReader(z.Bytes())
		}
	}
}

// TestStreamBombConstantMemory: a 1032:1 bomb read through a source that cannot
// give it back is decoded in the fixed buffers, not in memory proportional to
// the output.
func TestStreamBombConstantMemory(t *testing.T) {
	stream := bombStream(50_000, true)
	var total int64
	n := allocated(func() {
		var err error
		total, err = io.Copy(io.Discard, NewReader(plainReader{bytes.NewReader(stream)}))
		if err != nil {
			t.Error(err)
		}
	})
	if want := int64(1 + 258*50_000); total != want {
		t.Fatalf("decoded %d bytes, want %d", total, want)
	}
	if n >= 1<<20 {
		t.Errorf("decoding %d bytes allocated %d", total, n)
	}
}

// TestStreamAllocations: a reader costs a handful of allocations whatever the size
// of the stream, once the pool is warm.
func TestStreamAllocations(t *testing.T) {
	z := compressStd(t, corpus()["text"][:100000], 6, 0)
	src := bytes.NewReader(z)
	buf := make([]byte, 32768)
	allocs := testing.AllocsPerRun(20, func() {
		src.Reset(z)
		r := NewReader(src)
		for {
			if _, err := r.Read(buf); err != nil {
				break
			}
		}
		r.Close()
	})
	if allocs > 4 {
		t.Errorf("%v allocations per stream, want at most 4", allocs)
	}
}

// legacyAgrees decodes with the bit-at-a-time reader, the independent oracle.
func legacyDecode(b []byte) ([]byte, error) {
	return io.ReadAll(newByteReader(bytes.NewReader(b)))
}

// TestStreamAgreesWithLegacy: on valid streams the fast reader and the
// bit-at-a-time reader agree.
func TestStreamAgreesWithLegacy(t *testing.T) {
	for name, data := range corpus() {
		z := compressStd(t, data, 6, 0)
		a, aerr := legacyDecode(z)
		b, berr := io.ReadAll(NewReader(bytes.NewReader(z)))
		if aerr != nil || berr != nil || !bytes.Equal(a, b) {
			t.Fatalf("%s: legacy %v, fast %v", name, aerr, berr)
		}
	}
}

// countSource counts the calls made on it, and fails with err once data is out.
type countSource struct {
	data  []byte
	err   error
	reads int
}

func (c *countSource) Read(p []byte) (int, error) {
	c.reads++
	if len(c.data) == 0 {
		return 0, c.err
	}
	n := copy(p, c.data)
	c.data = c.data[n:]
	return n, nil
}

// TestStreamReadNilTouchesNothing: a zero-length Read neither decodes nor reads
// the source, so it reports no error the source has not been asked about yet.
func TestStreamReadNilTouchesNothing(t *testing.T) {
	src := &countSource{data: compressStd(t, []byte("abc"), 6, 0), err: errBoom}
	r := NewReader(src)
	if n, err := r.Read(nil); n != 0 || err != nil || src.reads != 0 {
		t.Fatalf("Read(nil) = %d, %v after %d source reads", n, err, src.reads)
	}
	bad := NewReader(&countSource{err: errBoom})
	if n, err := bad.Read(nil); n != 0 || err != nil {
		t.Fatalf("Read(nil) on a failing source = %d, %v", n, err)
	}
	if _, err := bad.Read(make([]byte, 1)); err != errBoom {
		t.Fatalf("Read = %v, want errBoom", err)
	}
}

// byteOnly is an io.ByteReader that must be read through ReadByte: its Read
// fails the test, and it reports how far it was read.
type byteOnly struct {
	t    *testing.T
	data []byte
	pos  int
}

func (b *byteOnly) ReadByte() (byte, error) {
	if b.pos >= len(b.data) {
		return 0, io.EOF
	}
	c := b.data[b.pos]
	b.pos++
	return c, nil
}

func (b *byteOnly) Read(p []byte) (int, error) {
	b.t.Helper()
	b.t.Error("Read called on a bare io.ByteReader")
	return 0, io.EOF
}

// TestStreamBareByteReaderExact: a source that is only an io.ByteReader is read
// byte by byte, and the byte after the stream is left unread.
func TestStreamBareByteReaderExact(t *testing.T) {
	data := corpus()["text"][:3000]
	z := compressStd(t, data, 6, 0)
	src := &byteOnly{t: t, data: append(append([]byte{}, z...), "TRAILER"...)}
	got, err := io.ReadAll(NewReader(src))
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("decode: err=%v, %d bytes", err, len(got))
	}
	if src.pos != len(z) {
		t.Fatalf("consumed %d bytes, the stream is %d", src.pos, len(z))
	}
}

// TestStreamBufioSourceEnds: through a *bufio.Reader, an error of the source
// ends the stream as it is, and a clean end inside the stream is an unexpected
// EOF, for a buffer that holds the input and for one too small to.
func TestStreamBufioSourceEnds(t *testing.T) {
	base := compressStd(t, corpus()["text"][:5000], 6, 0)
	for _, size := range []int{16, 64, 4096} {
		for cut := 1; cut < len(base); cut += 7 {
			_, err := io.ReadAll(NewReader(bufio.NewReaderSize(&errAfter{data: base[:cut], err: errBoom}, size)))
			if err != errBoom {
				t.Fatalf("size %d cut %d: got %v want errBoom", size, cut, err)
			}
			_, err = io.ReadAll(NewReader(bufio.NewReaderSize(&errAfter{data: base[:cut], err: io.EOF}, size)))
			if err != io.ErrUnexpectedEOF {
				t.Fatalf("size %d cut %d: got %v want ErrUnexpectedEOF", size, cut, err)
			}
		}
	}
}

// gateSource hands out head, then refuses to be read again until open is set.
type gateSource struct {
	t    *testing.T
	head []byte
	tail []byte
	open *bool
}

func (g *gateSource) Read(p []byte) (int, error) {
	if len(g.head) > 0 {
		n := copy(p, g.head)
		g.head = g.head[n:]
		return n, nil
	}
	if !*g.open {
		g.t.Error("the source was read again before the decoded output was delivered")
	}
	if len(g.tail) == 0 {
		return 0, io.EOF
	}
	n := copy(p, g.tail)
	g.tail = g.tail[n:]
	return n, nil
}

// TestStreamDeliversBeforeWaiting: when the input in hand is used up in the
// middle of a block, what it decoded is delivered before the source is read
// again, which may block for as long as the peer likes.
func TestStreamDeliversBeforeWaiting(t *testing.T) {
	data := corpus()["text"][:20000]
	z := compressStd(t, data, 6, 0)
	open := false
	src := &gateSource{t: t, head: z[:len(z)/2], tail: z[len(z)/2:], open: &open}
	r := NewReader(src)
	buf := make([]byte, 1<<16)
	n, err := r.Read(buf)
	if n == 0 || err != nil {
		t.Fatalf("first Read = %d, %v", n, err)
	}
	open = true
	rest, err := io.ReadAll(r)
	if err != nil || !bytes.Equal(append(buf[:n], rest...), data) {
		t.Fatalf("decode: err=%v", err)
	}
}
