package deflate

// Helpers that craft a DEFLATE stream whose single dynamic block expands
// ~1032:1: literal 'a', then `repeats` copies of (length 258, distance 1), each
// costing two bits, then end-of-block.

// bombStream returns that stream; the output is 1 + 258*repeats bytes.
func bombStream(repeats int, final bool) []byte {
	lit := make([]uint8, 286)
	lit['a'], lit[256], lit[285] = 2, 2, 1
	dist := []uint8{1}
	var litEnc, distEnc huffEncoder
	litEnc.assign(lit)
	distEnc.assign(dist)

	var syms []clSym
	for _, l := range lit {
		syms = append(syms, clSym{sym: l})
	}
	syms = append(syms, clSym{sym: dist[0]})
	out := craftDynamicHeader(286, 1, syms, func(bw *bitWriter) {
		bw.writeCode(&litEnc, 'a')
		for range repeats {
			bw.writeCode(&litEnc, 285)
			bw.writeCode(&distEnc, 0)
		}
		bw.writeCode(&litEnc, 256)
	})
	if !final {
		out[0] &^= 1 // clear BFINAL; callers append their own blocks
	}
	return out
}
