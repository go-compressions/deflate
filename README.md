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

Measured on an **Apple M4 Max** (native `darwin/arm64`), Go 1.26.4, best of
several `-count` runs, **as of 2026-07-03**. Three corpora: `text` (1.35 MB of
Go stdlib `net/http` source), `json` (1.4 MB synthetic record array), `binary`
(3 MB prefix of the `go` tool binary). Encode/decode throughput in MB/s
(higher is better); *ratio* is compressed size ÷ original (lower is better).

| corpus | level | encode ours | encode `flate` | vs `flate` | decode ours | decode `flate` | vs `flate` | ratio ours | ratio `flate` |
|---|---|--:|--:|--:|--:|--:|--:|--:|--:|
| text   | speed   | 102 | 176 | 0.58× | 163 | 278  | 0.59× | 0.275 | 0.329 |
| text   | default | 65  | 51  | **1.26×** | 173 | 343  | 0.50× | 0.268 | 0.263 |
| text   | best    | 63  | 44  | **1.44×** | 170 | 339  | 0.50× | 0.268 | 0.263 |
| json   | speed   | 261 | 480 | 0.54× | 358 | 741  | 0.48× | 0.106 | 0.133 |
| json   | default | 116 | 150 | 0.77× | 370 | 1028 | 0.36× | 0.098 | 0.096 |
| json   | best    | 111 | 50  | **2.21×** | 378 | 1132 | 0.33× | 0.098 | 0.089 |
| binary | speed   | 75  | 132 | 0.56× | 98  | 214  | 0.46× | 0.453 | 0.492 |
| binary | default | 63  | 61  | **1.03×** | 99  | 235  | 0.42× | 0.449 | 0.449 |
| binary | best    | 61  | 56  | **1.10×** | 100 | 238  | 0.42× | 0.449 | 0.448 |

**Honest verdict.**

- **Encode, default/best levels — competitive to faster than `compress/flate`.**
  At the default level our encoder matches `flate` on `binary` (1.03×) and beats
  it on `text` (1.26×) at essentially equal ratio; on `json` it trails (0.77×) at
  equal ratio. At the best level it is 1.1–2.2× faster, though there part of the
  speed comes from a slightly larger output (e.g. `json` 0.098 vs 0.089) — a fair
  trade, not a free win. The SIMD `matchlen` kernel is in the hot path; as with
  the sibling `lz4`, end-to-end gains are bounded because encode time is
  dominated by match-*finding* (hash chains), not match-*extension*.
- **Encode, `BestSpeed` — `flate` wins (~2×).** The standard library ships a
  hand-specialized fast-path encoder for level 1; our general hash-chain parse
  does not beat it.
- **Decode — `flate` is ~2× faster.** Our decoder is a small, correct,
  bit-serial count/symbol Huffman design; `flate`'s is a mature table-driven,
  chunked decoder. Decode is inherently bit-serial and not SIMD-amenable, so we
  target correctness and compatibility here rather than raw speed.

Reproduce with the program under `benchmarks/`. Numbers are single-host; the
relative picture is what matters.

## License

BSD-3-Clause. See [LICENSE](LICENSE).
