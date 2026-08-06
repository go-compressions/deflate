package deflate

import (
	"errors"
	"fmt"
	"io"

	"github.com/go-simd/matchlen"
)

// hashBits sizes the match-finder hash table (2^hashBits entries).
const (
	hashBits    = 17
	hashSize    = 1 << hashBits
	blockBudget = 1 << 15 // target raw bytes per emitted block (< 64 KiB)
)

// token is one LZ77 output element: a literal (length == 0) or a back-reference.
type token struct {
	literal byte
	length  uint16 // 0 for a literal, else match length in 3..258
	dist    uint16 // back-reference distance in 1..32768 (match only)
}

// bitWriter accumulates a DEFLATE bit stream, LSB first, into an output buffer.
type bitWriter struct {
	out []byte
	b   uint64
	nb  uint
}

func (w *bitWriter) writeBits(v uint32, n uint) {
	w.b |= uint64(v) << w.nb
	w.nb += n
	for w.nb >= 8 {
		w.out = append(w.out, byte(w.b))
		w.b >>= 8
		w.nb -= 8
	}
}

func (w *bitWriter) writeCode(e *huffEncoder, sym int) {
	w.writeBits(uint32(e.code[sym]), uint(e.length[sym]))
}

func (w *bitWriter) alignByte() {
	if w.nb > 0 {
		w.out = append(w.out, byte(w.b))
		w.b = 0
		w.nb = 0
	}
}

// Writer is a streaming DEFLATE (RFC 1951) compressor. Data passed to Write is
// buffered and compressed when Close is called; the emitted stream is readable
// by NewReader and by compress/flate.
type Writer struct {
	w      io.Writer
	level  int
	input  []byte
	bw     bitWriter
	closed bool
	err    error

	// match-finder state (allocated on first Close).
	head []int32
	prev []int32
}

// fixed-block encoders, shared by all writers.
var fixedLitEnc, fixedDistEnc huffEncoder

func init() {
	litLengths := make([]uint8, 288)
	for i := 0; i < 144; i++ {
		litLengths[i] = 8
	}
	for i := 144; i < 256; i++ {
		litLengths[i] = 9
	}
	for i := 256; i < 280; i++ {
		litLengths[i] = 7
	}
	for i := 280; i < 288; i++ {
		litLengths[i] = 8
	}
	fixedLitEnc.assign(litLengths)
	distLengths := make([]uint8, 30)
	for i := range distLengths {
		distLengths[i] = 5
	}
	fixedDistEnc.assign(distLengths)
}

// NewWriter returns a Writer that compresses to w at the given level. Valid
// levels are DefaultCompression, NoCompression and BestSpeed..BestCompression
// (-1 and 0..9); any other value is an error.
func NewWriter(w io.Writer, level int) (*Writer, error) {
	if level < DefaultCompression || level > BestCompression {
		return nil, fmt.Errorf("deflate: invalid compression level %d", level)
	}
	if level == DefaultCompression {
		level = 6
	}
	return &Writer{w: w, level: level}, nil
}

func (z *Writer) Write(p []byte) (int, error) {
	if z.err != nil {
		return 0, z.err
	}
	if z.closed {
		return 0, errors.New("deflate: Write after Close")
	}
	z.input = append(z.input, p...)
	return len(p), nil
}

// Close compresses all buffered data, writes the final block and flushes.
func (z *Writer) Close() error {
	if z.err != nil {
		return z.err
	}
	if z.closed {
		return nil
	}
	z.closed = true
	z.compress()
	z.bw.alignByte()
	if _, err := z.w.Write(z.bw.out); err != nil {
		z.err = err
		return err
	}
	return nil
}

// maxChain returns the hash-chain search depth for the writer's level.
func (z *Writer) maxChain() int {
	switch {
	case z.level <= 1:
		return 8
	case z.level == 2:
		return 16
	case z.level == 3:
		return 32
	case z.level == 4:
		return 64
	case z.level == 5:
		return 128
	case z.level == 6:
		return 256
	case z.level == 7:
		return 512
	case z.level == 8:
		return 1024
	default:
		return 4096
	}
}

func hash4(b []byte) uint32 {
	v := uint32(b[0]) | uint32(b[1])<<8 | uint32(b[2])<<16 | uint32(b[3])<<24
	return (v * 2654435761) >> (32 - hashBits)
}

// compress runs the LZ77 parse and emits blocks.
func (z *Writer) compress() {
	in := z.input
	if z.level == NoCompression {
		z.storeAll(in)
		return
	}

	z.head = make([]int32, hashSize)
	for i := range z.head {
		z.head[i] = -1
	}
	z.prev = make([]int32, len(in))
	chain := z.maxChain()

	var tokens []token
	blkStart := 0
	i := 0
	insert := func(p int) {
		if p+4 <= len(in) {
			h := hash4(in[p:])
			z.prev[p] = z.head[h]
			z.head[h] = int32(p)
		}
	}
	for i < len(in) {
		bestLen, bestDist := 0, 0
		if len(in)-i >= minMatch && i+4 <= len(in) {
			bestLen, bestDist = z.findMatch(in, i, chain)
		}
		if bestLen >= minMatch {
			tokens = append(tokens, token{length: uint16(bestLen), dist: uint16(bestDist)})
			for j := 0; j < bestLen; j++ {
				insert(i + j)
			}
			i += bestLen
		} else {
			tokens = append(tokens, token{literal: in[i]})
			insert(i)
			i++
		}
		if i-blkStart >= blockBudget && i < len(in) {
			z.writeBlock(tokens, false, in, blkStart, i)
			tokens = tokens[:0]
			blkStart = i
		}
	}
	z.writeBlock(tokens, true, in, blkStart, i)
}

// findMatch returns the best match (length, distance) for position i.
func (z *Writer) findMatch(in []byte, i, chain int) (int, int) {
	limit := maxMatch
	if rem := len(in) - i; rem < limit {
		limit = rem
	}
	bestLen, bestDist := 0, 0
	cand := z.head[hash4(in[i:])]
	for cand >= 0 && chain > 0 {
		c := int(cand)
		dist := i - c
		if dist > windowSize {
			break
		}
		if bestLen == 0 || in[c+bestLen] == in[i+bestLen] {
			n := matchlen.MatchLen(in[c:c+limit], in[i:i+limit])
			if n > bestLen {
				bestLen, bestDist = n, dist
				if n >= limit {
					break
				}
			}
		}
		cand = z.prev[c]
		chain--
	}
	return bestLen, bestDist
}

// storeAll emits input as stored (uncompressed) blocks, used at NoCompression.
func (z *Writer) storeAll(in []byte) {
	if len(in) == 0 {
		z.writeStored(nil, true)
		return
	}
	for off := 0; off < len(in); off += 65535 {
		end := off + 65535
		if end > len(in) {
			end = len(in)
		}
		z.writeStored(in[off:end], end == len(in))
	}
}

// writeStored writes a single stored block.
func (z *Writer) writeStored(data []byte, final bool) {
	var bf uint32
	if final {
		bf = 1
	}
	z.bw.writeBits(bf, 3) // BFINAL, BTYPE=00
	z.bw.alignByte()
	n := len(data)
	z.bw.out = append(z.bw.out, byte(n), byte(n>>8), byte(^n), byte(^n>>8))
	z.bw.out = append(z.bw.out, data...)
}
