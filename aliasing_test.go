package deflate

import (
	"bytes"
	"io"
	"sync"
	"testing"
)

// buildFixedThenDynamic emits one stream containing a non-final FIXED block
// followed by a final DYNAMIC block, and returns it plus the expected decoded
// bytes. Decoding it exercises the path where a reader's litHuff/distHuff are
// first aliased to the package-global fixed decoders (BTYPE=01) and then rebuilt
// for the dynamic block (BTYPE=10).
func buildFixedThenDynamic() (stream, want []byte) {
	var z Writer

	// Fixed block: literals "AB".
	ftok := []token{{literal: 'A'}, {literal: 'B'}}
	z.writeFixed(ftok, false)

	// Dynamic block: two literals then an overlapping match.
	dtok := []token{{literal: 'X'}, {literal: 'Y'}, {length: 4, dist: 2}}
	litFreq := make([]int, numLitLen)
	distFreq := make([]int, numDist)
	for _, t := range dtok {
		if t.length == 0 {
			litFreq[t.literal]++
		} else {
			litFreq[257+int(lengthCode[t.length])]++
			distFreq[distanceCode(int(t.dist))]++
		}
	}
	litFreq[endBlock]++
	litLen := codeLengths(litFreq)
	distLen := codeLengths(distFreq)
	dyn := buildDynamic(litLen, distLen)
	z.writeDynamic(dtok, true, litLen, distLen, dyn)
	z.bw.alignByte()

	return z.bw.out, []byte("ABXYXYXY")
}

// fixedStream emits a single final fixed-Huffman block of the given literals.
func fixedStream(data []byte) []byte {
	var z Writer
	toks := make([]token, len(data))
	for i, b := range data {
		toks[i] = token{literal: b}
	}
	z.writeFixed(toks, true)
	z.bw.alignByte()
	return z.bw.out
}

func decodeAll(t *testing.T, stream []byte) []byte {
	t.Helper()
	got, err := io.ReadAll(NewReader(bytes.NewReader(stream)))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	return got
}

// TestFixedGlobalsNotCorruptedByDynamic guards against the fixed-decoder tables
// being mutated in place when a dynamic block follows a fixed block: after such
// a decode, an independent fixed-block stream must still decode correctly.
func TestFixedGlobalsNotCorruptedByDynamic(t *testing.T) {
	mixed, want := buildFixedThenDynamic()
	if got := decodeAll(t, mixed); !bytes.Equal(got, want) {
		t.Fatalf("mixed stream: got %q want %q", got, want)
	}

	fixed := fixedStream([]byte("hello, fixed world!"))
	if got := decodeAll(t, fixed); !bytes.Equal(got, []byte("hello, fixed world!")) {
		t.Fatalf("fixed stream after mixed decode corrupted: got %q", got)
	}

	// Snapshot the shared fixed decoder's symbol table and verify a mixed decode
	// leaves it byte-for-byte unchanged.
	before := append([]uint16(nil), fixedLit.symbol...)
	_ = decodeAll(t, mixed)
	if !equalU16(before, fixedLit.symbol) {
		t.Fatalf("fixedLit.symbol mutated by dynamic block decode")
	}
}

func equalU16(a, b []uint16) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestConcurrentReaders decodes fixed and mixed streams from many goroutines at
// once; with -race this fails if the fixed decoder tables are shared-mutable.
func TestConcurrentReaders(t *testing.T) {
	mixed, mixedWant := buildFixedThenDynamic()
	fixed := fixedStream([]byte("the quick brown fox"))
	fixedWant := []byte("the quick brown fox")

	var wg sync.WaitGroup
	for g := 0; g < 16; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				if g%2 == 0 {
					if got, err := io.ReadAll(NewReader(bytes.NewReader(mixed))); err != nil || !bytes.Equal(got, mixedWant) {
						t.Errorf("mixed: err=%v got=%q", err, got)
						return
					}
				} else {
					if got, err := io.ReadAll(NewReader(bytes.NewReader(fixed))); err != nil || !bytes.Equal(got, fixedWant) {
						t.Errorf("fixed: err=%v got=%q", err, got)
						return
					}
				}
			}
		}(g)
	}
	wg.Wait()
}
