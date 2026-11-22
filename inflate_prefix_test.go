package deflate

import (
	"bytes"
	"testing"
)

// TestInflatePrefixReportsWhereTheStreamEnds: the count is exactly the length
// of the stream, whatever follows it, for every kind of final block and every
// way of ending (mid-byte, on a byte, through the hot loop or the checked one).
func TestInflatePrefixReportsWhereTheStreamEnds(t *testing.T) {
	trailer := []byte{0xde, 0xad, 0xbe, 0xef}
	cp := corpus()
	for name, data := range cp {
		for _, level := range []int{0, 1, 6, 9} {
			for _, flush := range []int{0, 777} {
				z := compressStd(t, data, level, flush)
				for _, tr := range [][]byte{nil, trailer} {
					in := append(bytes.Clone(z), tr...)
					out, n, err := InflatePrefix(nil, in, DefaultMaxOutput)
					if err != nil || !bytes.Equal(out, data) {
						t.Fatalf("%s l%d f%d: err %v", name, level, flush, err)
					}
					if n != len(z) {
						t.Errorf("%s l%d f%d trailer %d: n = %d, stream is %d bytes", name, level, flush, len(tr), n, len(z))
					}
				}
			}
		}
		z := Deflate(nil, data)
		if _, n, err := InflatePrefix(nil, append(bytes.Clone(z), trailer...), DefaultMaxOutput); err != nil || n != len(z) {
			t.Errorf("%s ours: n = %d want %d, err %v", name, n, len(z), err)
		}
	}
}
