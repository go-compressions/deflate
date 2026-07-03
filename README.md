<p align="center"><img src="https://raw.githubusercontent.com/go-compressions/brand/main/social/go-compressions-deflate.png" alt="go-compressions/deflate" width="720"></p>

# deflate

[![ci](https://github.com/go-compressions/deflate/actions/workflows/ci.yml/badge.svg)](https://github.com/go-compressions/deflate/actions/workflows/ci.yml)
![coverage](https://img.shields.io/badge/coverage-100%25-brightgreen)
[![Go Reference](https://pkg.go.dev/badge/github.com/go-compressions/deflate.svg)](https://pkg.go.dev/github.com/go-compressions/deflate)
[![License](https://img.shields.io/badge/License-BSD--3--Clause-blue.svg)](LICENSE)

A pure-Go (`CGO=0`) implementation of the **DEFLATE** compressed data format
([RFC 1951](https://www.rfc-editor.org/rfc/rfc1951)) — both the encoder
(*deflate*) and the decoder (*inflate*). The DEFLATE bit stream is the payload
carried inside zlib (RFC 1950) and gzip (RFC 1952), and exactly what the
standard library's `compress/flate` reads and writes.

This package is **bidirectionally wire-compatible with `compress/flate`**,
verified by differential fuzzing: `flate.NewReader` decodes every stream our
`Writer` produces, and our `NewReader` decodes every stream `flate.NewWriter`
produces — both back to the original bytes.

The encoder's LZ77 parse delegates its hot "how far do these two positions
match" inner loop to [matchlen](https://github.com/go-compressions/matchlen),
whose SIMD common-prefix kernel accelerates match extension on **all six** of
Go's 64-bit targets — amd64 (SSE2), arm64 (NEON), riscv64 (RVV), loong64 (LSX),
ppc64le (VSX) and s390x (vector facility) — on a plain `go build`.

## Install

```sh
go get github.com/go-compressions/deflate
```

## Usage

```go
// Streaming.
var buf bytes.Buffer
w, _ := deflate.NewWriter(&buf, deflate.DefaultCompression)
w.Write(data)
w.Close()

r := deflate.NewReader(&buf)
out, _ := io.ReadAll(r)

// One-shot convenience helpers.
comp := deflate.Deflate(nil, src)          // src -> DEFLATE
orig, _ := deflate.Inflate(nil, comp)      // DEFLATE -> src
```

`NewWriter` accepts `DefaultCompression`, `NoCompression` and the range
`BestSpeed`..`BestCompression` (`-1`, `0`, `1`..`9`), mirroring `compress/flate`.

## How it works

**Decoder.** A bit reader feeds a compact count/symbol canonical-Huffman decoder
(the zlib *puff* method). It handles all three block types — stored, fixed
Huffman and dynamic Huffman (including the code-length run-length alphabet) —
plus LZ77 back-reference copies over a 32 KiB sliding window, with every
malformed-input path (reserved block type, bad stored length, over-subscribed or
truncated codes, out-of-range distances) returning a typed error.

**Encoder.** A hash-chain match-finder (4-byte hash, per-level chain depth)
produces LZ77 tokens; match *extension* runs through `matchlen`'s SIMD kernel.
Each block is emitted as whichever of **stored / fixed / dynamic** is smallest,
with length-limited (≤ 15-bit, ≤ 7-bit for the code-length alphabet) Huffman
codes generated so they are always *complete* — accepted by any RFC 1951 decoder
without single-code special cases.

## Benchmarks

<!-- BENCH -->

## License

BSD-3-Clause. See [LICENSE](LICENSE).
