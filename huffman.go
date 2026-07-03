package deflate

import "errors"

// errOversubscribed reports a set of Huffman code lengths that assigns more
// codes than a prefix code of that depth can hold.
var errOversubscribed = errors.New("deflate: oversubscribed Huffman code")

// huffDecoder decodes canonical Huffman codes using the count/symbol method
// (as in zlib's puff.c): it is small, allocation-light and easy to reason
// about. count[n] is the number of codes of length n; symbols are the alphabet
// symbols ordered by (length, symbol).
type huffDecoder struct {
	count  [maxCodeLen + 1]uint16
	symbol []uint16
}

// build constructs a decoder from per-symbol code lengths (0 = symbol unused).
// It rejects over-subscribed code sets but permits incomplete ones (an
// incomplete code is only ever reached by corrupt input, which decode() then
// reports).
func (h *huffDecoder) build(lengths []int) error {
	for i := range h.count {
		h.count[i] = 0
	}
	for _, l := range lengths {
		h.count[l]++
	}
	if h.count[0] == uint16(len(lengths)) {
		// No codes at all: a valid (empty) code. decode() will error if used.
		h.symbol = h.symbol[:0]
		return nil
	}
	// Check for an over-subscribed code set.
	left := 1
	for l := 1; l <= maxCodeLen; l++ {
		left <<= 1
		left -= int(h.count[l])
		if left < 0 {
			return errOversubscribed
		}
	}
	// Compute the offset of each length group and place symbols.
	var offs [maxCodeLen + 2]uint16
	for l := 1; l <= maxCodeLen; l++ {
		offs[l+1] = offs[l] + h.count[l]
	}
	if cap(h.symbol) < len(lengths) {
		h.symbol = make([]uint16, len(lengths))
	}
	h.symbol = h.symbol[:len(lengths)]
	n := 0
	for sym, l := range lengths {
		if l != 0 {
			h.symbol[offs[l]] = uint16(sym)
			offs[l]++
			n++
		}
	}
	h.symbol = h.symbol[:n]
	return nil
}

// huffEncoder holds canonical Huffman codes for one alphabet. code[sym] is the
// code value pre-reversed to bit length length[sym], ready to be written LSB
// first into the DEFLATE bit stream.
type huffEncoder struct {
	code   []uint16
	length []uint8
}

// reverseBits reverses the low width bits of v.
func reverseBits(v uint16, width uint8) uint16 {
	var r uint16
	for i := uint8(0); i < width; i++ {
		r = (r << 1) | (v & 1)
		v >>= 1
	}
	return r
}

// assign fills in canonical code values from the per-symbol lengths, reversing
// each so the writer emits it most-significant-bit first (RFC 1951 §3.1.1).
func (e *huffEncoder) assign(lengths []uint8) {
	e.length = lengths
	if cap(e.code) < len(lengths) {
		e.code = make([]uint16, len(lengths))
	}
	e.code = e.code[:len(lengths)]
	var blCount [maxCodeLen + 1]int
	for _, l := range lengths {
		blCount[l]++
	}
	var nextCode [maxCodeLen + 1]uint16
	var code uint16
	blCount[0] = 0
	for bits := 1; bits <= maxCodeLen; bits++ {
		code = (code + uint16(blCount[bits-1])) << 1
		nextCode[bits] = code
	}
	for sym, l := range lengths {
		if l != 0 {
			e.code[sym] = reverseBits(nextCode[l], l)
			nextCode[l]++
		}
	}
}

// codeLengths computes length-limited (<= maxCodeLen) Huffman code lengths for
// an alphabet with the given symbol frequencies. The returned slice always
// describes a complete prefix code (Kraft sum == 1) covering at least two
// symbols, so any RFC 1951 decoder — including compress/flate — accepts it.
func codeLengths(freq []int) []uint8 {
	n := len(freq)
	lengths := make([]uint8, n)

	// Collect symbols that actually occur.
	nz := make([]int, 0, n)
	for sym, f := range freq {
		if f > 0 {
			nz = append(nz, sym)
		}
	}
	// Force at least two symbols so the code is complete (Kraft == 1). This
	// keeps degenerate blocks (empty input, no matches, a single distance)
	// decodable by strict decoders without a single-code special case.
	for i := 0; len(nz) < 2; i++ {
		if freq[i] == 0 {
			nz = append(nz, i)
		}
	}

	// Build an ordinary Huffman tree with a simple heap keyed on frequency.
	assignHuffmanLengths(freq, nz, lengths)

	// Repair the lengths into a complete, depth-limited code.
	limitAndComplete(nz, lengths)
	return lengths
}

// heapNode is a node in the Huffman construction heap.
type heapNode struct {
	freq        int
	left, right int // child indices into nodes, or -1 for a leaf
	sym         int // symbol for a leaf, else -1
}

// assignHuffmanLengths builds a Huffman tree over the symbols in nz (using
// their frequencies) and writes each symbol's depth into lengths.
func assignHuffmanLengths(freq []int, nz []int, lengths []uint8) {
	if len(nz) == 1 {
		lengths[nz[0]] = 1
		return
	}
	nodes := make([]heapNode, 0, 2*len(nz))
	// Min-heap of node indices ordered by frequency.
	heap := make([]int, 0, len(nz))
	push := func(idx int) {
		heap = append(heap, idx)
		i := len(heap) - 1
		for i > 0 {
			p := (i - 1) / 2
			if nodes[heap[p]].freq <= nodes[heap[i]].freq {
				break
			}
			heap[p], heap[i] = heap[i], heap[p]
			i = p
		}
	}
	pop := func() int {
		top := heap[0]
		last := len(heap) - 1
		heap[0] = heap[last]
		heap = heap[:last]
		i := 0
		for {
			l, r, s := 2*i+1, 2*i+2, i
			if l < len(heap) && nodes[heap[l]].freq < nodes[heap[s]].freq {
				s = l
			}
			if r < len(heap) && nodes[heap[r]].freq < nodes[heap[s]].freq {
				s = r
			}
			if s == i {
				break
			}
			heap[i], heap[s] = heap[s], heap[i]
			i = s
		}
		return top
	}
	for _, sym := range nz {
		nodes = append(nodes, heapNode{freq: freq[sym], left: -1, right: -1, sym: sym})
		push(len(nodes) - 1)
	}
	for len(heap) > 1 {
		a := pop()
		b := pop()
		nodes = append(nodes, heapNode{freq: nodes[a].freq + nodes[b].freq, left: a, right: b, sym: -1})
		push(len(nodes) - 1)
	}
	// Walk the tree assigning depths.
	var walk func(idx, depth int)
	walk = func(idx, depth int) {
		nd := &nodes[idx]
		if nd.sym >= 0 {
			lengths[nd.sym] = uint8(depth)
			return
		}
		walk(nd.left, depth+1)
		walk(nd.right, depth+1)
	}
	walk(heap[0], 0)
}

// limitAndComplete clamps every length in nz to maxCodeLen and then adjusts the
// lengths so they form a complete prefix code (Kraft sum == 2^maxCodeLen).
func limitAndComplete(nz []int, lengths []uint8) {
	const target = 1 << maxCodeLen
	for _, sym := range nz {
		if lengths[sym] > maxCodeLen {
			lengths[sym] = maxCodeLen
		}
		if lengths[sym] == 0 {
			lengths[sym] = 1
		}
	}
	kraft := func() int {
		k := 0
		for _, sym := range nz {
			k += 1 << (maxCodeLen - lengths[sym])
		}
		return k
	}
	// Over-subscribed: lengthen codes (prefer the currently longest that can
	// still grow) until the Kraft sum fits.
	for kraft() > target {
		best := -1
		for _, sym := range nz {
			if lengths[sym] < maxCodeLen && (best < 0 || lengths[sym] > lengths[best]) {
				best = sym
			}
		}
		lengths[best]++
	}
	// Incomplete: shorten codes until the Kraft sum is exactly complete. Pick
	// the deepest code whose promotion does not overshoot the target.
	for {
		deficit := target - kraft()
		if deficit == 0 {
			break
		}
		best := -1
		for _, sym := range nz {
			inc := 1 << (maxCodeLen - lengths[sym])
			if lengths[sym] > 1 && inc <= deficit && (best < 0 || lengths[sym] > lengths[best]) {
				best = sym
			}
		}
		lengths[best]--
	}
}
