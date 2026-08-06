// Package deflate implements the DEFLATE compressed data format (RFC 1951) in
// pure Go, with no cgo.
//
// # Overview
//
// DEFLATE combines LZ77 back-references with Huffman coding. This package
// provides both halves:
//
//   - a decoder (inflate) via [NewReader] and the [Inflate] convenience helper;
//   - an encoder (deflate) via [NewWriter] and the [Deflate] convenience helper.
//
// The stream is the raw DEFLATE bit stream — the same payload carried inside
// zlib (RFC 1950) and gzip (RFC 1952) containers, and exactly what the standard
// library's compress/flate reads and writes. This package is verified
// bidirectionally compatible with compress/flate: it decodes any stream
// flate.NewWriter produces, and flate.NewReader decodes any stream this
// package's Writer produces.
//
// # SIMD match extension
//
// The encoder's LZ77 parse delegates its hot "how far do these two positions
// match" inner loop to github.com/go-compressions/matchlen, whose common-prefix
// kernel is SIMD-accelerated on four of Go's 64-bit targets (amd64, arm64,
// riscv64 and loong64) and a portable word-at-a-time scalar elsewhere — all on
// a plain go build.
//
// # Levels
//
// NewWriter accepts DefaultCompression, NoCompression and the range
// BestSpeed..BestCompression, mirroring compress/flate. Higher levels search
// longer hash chains for better ratio; NoCompression emits stored blocks only.
package deflate
