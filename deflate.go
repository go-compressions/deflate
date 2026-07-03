// Package deflate is a pure-Go (CGO=0) implementation of the DEFLATE
// compressed data format defined by RFC 1951 — both the encoder (deflate) and
// the decoder (inflate).
//
// It is wire-compatible with the standard library's compress/flate in both
// directions: this package's decoder reads any stream produced by
// flate.NewWriter, and flate.NewReader reads any stream produced by this
// package's Writer.
//
// The encoder's LZ77 match-finder delegates its hot "how far do these two
// positions match" inner loop to github.com/go-compressions/matchlen, whose
// SIMD common-prefix kernel accelerates match extension on all six of Go's
// 64-bit targets (amd64, arm64, riscv64, loong64, ppc64le and s390x).
package deflate

// DEFLATE format constants (RFC 1951).
const (
	maxMatch   = 258   // longest LZ77 back-reference length
	minMatch   = 3     // shortest LZ77 back-reference length
	windowSize = 32768 // maximum back-reference distance (32 KiB)

	maxCodeLen = 15 // maximum bits in a Huffman code
	endBlock   = 256 // end-of-block symbol in the literal/length alphabet

	numLitLen   = 286 // literal/length alphabet size (0..285)
	numDist     = 30  // distance alphabet size (0..29)
	numCodeLen  = 19  // code-length alphabet size (0..18)
	maxNumLit   = 286
	maxNumDist  = 30
)

// Compression levels, mirroring compress/flate.
const (
	NoCompression      = 0
	BestSpeed          = 1
	BestCompression    = 9
	DefaultCompression = -1
)

// Length codes 257..285: base length and number of extra bits (RFC 1951 §3.2.5).
var lengthBase = [29]uint16{
	3, 4, 5, 6, 7, 8, 9, 10, 11, 13, 15, 17, 19, 23, 27, 31,
	35, 43, 51, 59, 67, 83, 99, 115, 131, 163, 195, 227, 258,
}
var lengthExtra = [29]uint8{
	0, 0, 0, 0, 0, 0, 0, 0, 1, 1, 1, 1, 2, 2, 2, 2,
	3, 3, 3, 3, 4, 4, 4, 4, 5, 5, 5, 5, 0,
}

// Distance codes 0..29: base distance and number of extra bits (RFC 1951 §3.2.5).
var distBase = [30]uint16{
	1, 2, 3, 4, 5, 7, 9, 13, 17, 25, 33, 49, 65, 97, 129, 193,
	257, 385, 513, 769, 1025, 1537, 2049, 3073, 4097, 6145,
	8193, 12289, 16385, 24577,
}
var distExtra = [30]uint8{
	0, 0, 0, 0, 1, 1, 2, 2, 3, 3, 4, 4, 5, 5, 6, 6,
	7, 7, 8, 8, 9, 9, 10, 10, 11, 11, 12, 12, 13, 13,
}

// Order in which code-length-alphabet code lengths are stored in a dynamic
// block header (RFC 1951 §3.2.7).
var codeLengthOrder = [19]uint8{
	16, 17, 18, 0, 8, 7, 9, 6, 10, 5, 11, 4, 12, 3, 13, 2, 14, 1, 15,
}

// lengthCode maps a match length (3..258) to its length-code index (0..28).
var lengthCode [maxMatch + 1]uint8

// distCode maps a distance (1..32768) to its distance-code index (0..29).
// distances 1..256 are indexed directly; larger distances use distCodeLarge
// indexed by (dist-1)>>7.
var distCodeSmall [256]uint8
var distCodeLarge [256]uint8

func init() {
	// Build length -> length-code lookup.
	for code := 0; code < 29; code++ {
		lo := int(lengthBase[code])
		hi := lo + (1 << lengthExtra[code]) - 1
		if code == 28 {
			hi = maxMatch // code 28 covers exactly 258
		}
		for l := lo; l <= hi && l <= maxMatch; l++ {
			lengthCode[l] = uint8(code)
		}
	}
	// Build distance -> distance-code lookup.
	for code := 0; code < 30; code++ {
		lo := int(distBase[code])
		hi := lo + (1 << distExtra[code]) - 1
		for d := lo; d <= hi && d <= windowSize; d++ {
			if d <= 256 {
				distCodeSmall[d-1] = uint8(code)
			} else {
				distCodeLarge[(d-1)>>7] = uint8(code)
			}
		}
	}
}

// distanceCode returns the distance-code index for a distance in 1..32768.
func distanceCode(dist int) uint8 {
	if dist <= 256 {
		return distCodeSmall[dist-1]
	}
	return distCodeLarge[(dist-1)>>7]
}
