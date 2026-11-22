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

// bare is an io.ByteReader and nothing else that NewReader recognises, so it
// gets the bit-by-bit reader.
type bare struct {
	data []byte
	pos  int
}

func (b *bare) ReadByte() (byte, error) {
	if b.pos >= len(b.data) {
		return 0, io.EOF
	}
	c := b.data[b.pos]
	b.pos++
	return c, nil
}

func (b *bare) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	c, err := b.ReadByte()
	if err != nil {
		return 0, err
	}
	p[0] = c
	return 1, nil
}

// bothReaders is b behind each of the two decoders NewReader chooses between.
func bothReaders(b []byte) []io.Reader {
	return []io.Reader{bytes.NewReader(b), &bare{data: b}}
}

// bitStream builds a DEFLATE stream bit by bit: put is LSB-first as header
// fields and extra bits are, code is MSB-first as Huffman codes are.
type bitStream struct {
	out []byte
	n   uint
}

func (b *bitStream) bit(v uint) {
	if b.n%8 == 0 {
		b.out = append(b.out, 0)
	}
	b.out[len(b.out)-1] |= byte(v&1) << (b.n % 8)
	b.n++
}

func (b *bitStream) put(v uint, width int) {
	for i := 0; i < width; i++ {
		b.bit(v >> i)
	}
}

func (b *bitStream) code(v uint, width int) {
	for i := width - 1; i >= 0; i-- {
		b.bit(v >> i)
	}
}

// TestReservedDistanceSymbol: the fixed code has distance symbols 30 and 31,
// which no distance uses; every decoder refuses a match that names one.
func TestReservedDistanceSymbol(t *testing.T) {
	var b bitStream
	b.put(1, 1)         // BFINAL
	b.put(1, 2)         // BTYPE fixed
	b.code(0x30+'a', 8) // literal a
	b.code(1, 7)        // length symbol 257
	b.code(30, 5)       // distance symbol 30
	b.put(0, 24)        // the bit-by-bit reader looks this far for a code's end
	if _, err := Inflate(nil, b.out); err != ErrHuffman {
		t.Fatalf("Inflate: got %v want ErrHuffman", err)
	}
	for _, src := range bothReaders(b.out) {
		if _, err := io.ReadAll(NewReader(src)); err != ErrHuffman {
			t.Fatalf("%T: got %v want ErrHuffman", src, err)
		}
	}
}

// TestBareReaderReadNil: a zero-length Read on the bit-by-bit reader reports its
// error and nothing else.
func TestBareReaderReadNil(t *testing.T) {
	r := NewReader(&bare{data: compressStd(t, []byte("abc"), 6, 0)})
	if n, err := r.Read(nil); n != 0 || err != nil {
		t.Fatalf("Read(nil) = %d, %v", n, err)
	}
}

// TestHeaderTruncations cuts every stream at every byte of its first 300, which
// is where the dynamic tables are, so that each field of a header is the one the
// input ends in. All decoders must refuse, none panic.
func TestHeaderTruncations(t *testing.T) {
	for name, d := range corpus() {
		if len(d) > 40000 {
			d = d[:40000]
		}
		for _, lvl := range []int{6, 9} {
			full := compressStd(t, d, lvl, 0)
			for cut := 0; cut < len(full) && cut < 300; cut++ {
				for _, src := range bothReaders(full[:cut]) {
					got, err := io.ReadAll(NewReader(src))
					if err == nil && len(d) > 0 && bytes.Equal(got, d) {
						t.Fatalf("%s level %d cut %d decoded everything", name, lvl, cut)
					}
				}
			}
		}
	}
}

// reservedDistanceDynamic is a dynamic block whose distance code has a symbol
// 30, which a dynamic header may declare (up to 32 distance codes) although no
// distance uses it, followed by a match that names it.
func reservedDistanceDynamic() []byte {
	var b bitStream
	b.put(1, 1)  // BFINAL
	b.put(2, 2)  // BTYPE dynamic
	b.put(2, 5)  // HLIT: 259 literal/length codes
	b.put(30, 5) // HDIST: 31 distance codes
	b.put(14, 4) // HCLEN: 18 code-length code lengths
	for _, l := range []uint{0, 0, 0, 1, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 2, 0, 2} {
		b.put(l, 3)
	}
	// Code-length code: symbol 0 is "0", symbol 1 is "10", symbol 2 is "11".
	emit := func(l int) {
		switch l {
		case 0:
			b.code(0, 1)
		case 1:
			b.code(2, 2)
		default:
			b.code(3, 2)
		}
	}
	for i := 0; i < 259; i++ {
		if i == 97 || i >= 256 {
			emit(2)
		} else {
			emit(0)
		}
	}
	for i := 0; i < 31; i++ {
		if i == 0 || i == 30 {
			emit(1)
		} else {
			emit(0)
		}
	}
	b.code(0, 2) // literal a
	b.code(2, 2) // length symbol 257
	b.code(1, 1) // distance symbol 30
	b.put(0, 24) // room for whatever follows
	return b.out
}

// TestReservedDistanceDynamic: no decoder accepts a match whose distance symbol
// is 30 or 31, however the code declares it.
func TestReservedDistanceDynamic(t *testing.T) {
	s := reservedDistanceDynamic()
	if _, err := Inflate(nil, s); err == nil {
		t.Fatal("Inflate accepted distance symbol 30")
	}
	for _, src := range bothReaders(s) {
		if _, err := io.ReadAll(NewReader(src)); err == nil {
			t.Fatalf("%T accepted distance symbol 30", src)
		}
	}
	if _, err := io.ReadAll(NewReader(&bare{data: s})); err != ErrHuffman {
		t.Fatalf("bit-by-bit reader: got %v want ErrHuffman", err)
	}
}

// TestBareReaderCompactsWithPendingOutput: the bit-by-bit reader trims its
// buffer to the window even when more than a window of output is undelivered.
func TestBareReaderCompactsWithPendingOutput(t *testing.T) {
	data := make([]byte, 1<<20)
	z := compressStd(t, data, 6, 0)
	got, err := readAllSizes(NewReader(&bare{data: z}), fixedSize(33000))
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("err=%v, %d bytes", err, len(got))
	}
}
