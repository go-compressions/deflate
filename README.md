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

Reproducible via the committed benchmarks — no external corpus, the inputs are
generated deterministically in-process:

```sh
go test -run=^$ -bench=. -benchmem
```

Numbers below are from an **Apple M4 Max** (`darwin/arm64`), Go 1.26.4, on four
512 KiB corpora, **as of 2026-07-03**. Throughput is MB/s (higher is better);
*ratio* is compressed ÷ original (lower is better); *allocs* is allocations per
operation. **These are the exact values `go test -bench` prints on this host;
they are what the claims below are based on — nothing is cherry-picked.**

### Encode (this package vs `compress/flate`)

| corpus | level | ours MB/s | `flate` MB/s | ours vs `flate` | ratio ours | ratio `flate` | allocs ours | allocs `flate` |
|---|---|--:|--:|--:|--:|--:|--:|--:|
| text       | default | 18.2 | 16.9 | **1.08×** | 0.203  | 0.205  | 398 | 27 |
| text       | best    | 17.2 | 11.5 | **1.49×** | 0.203  | 0.202  | 398 | 27 |
| json       | default | 103  | 131  | 0.79×     | 0.105  | 0.100  | 394 | 26 |
| json       | best    | 97.7 | 45.6 | **2.14×** | 0.105  | 0.091  | 394 | 26 |
| binary     | default | 71.5 | 99.7 | 0.72×     | 0.834  | 0.835  | 393 | 29 |
| binary     | best    | 71.4 | 99.8 | 0.72×     | 0.834  | 0.835  | 393 | 29 |
| repetitive | default | 690  | 790  | 0.87×     | 0.0034 | 0.0030 | 287 | 21 |
| repetitive | best    | 686  | 790  | 0.87×     | 0.0034 | 0.0030 | 287 | 21 |

### Decode (both decoders reading the *same* `flate`-produced stream)

| corpus | level | ours MB/s | `flate` MB/s | ours vs `flate` |
|---|---|--:|--:|--:|
| text       | default | 253 | 460   | 0.55× |
| text       | best    | 266 | 472   | 0.56× |
| json       | default | 343 | 973   | 0.35× |
| json       | best    | 342 | 1067  | 0.32× |
| binary     | default | 68.7 | 216  | 0.32× |
| binary     | best    | 68.8 | 216  | 0.32× |
| repetitive | default | 464 | 10971 | 0.04× |
| repetitive | best    | 465 | 10961 | 0.04× |

**Honest verdict.**

- **Encode is a mixed picture, not a blanket win.** We are faster on `text`
  (1.08× default, **1.49× best**) and on `json` at the best level (**2.14×**,
  because `flate`'s BestCompression pays for exhaustive lazy matching), at
  essentially equal ratio on `text`. We are **slower on `binary` (0.72×),
  `repetitive` (0.87×) and `json` at the default level (0.79×)**. On a *mixed*
  corpus these average out to roughly parity-to-slightly-slower (~0.85–0.9×).
  Ratios track `flate` within ~1% except `json` and `repetitive`, where ours is
  a little larger. We also **allocate far more per operation (~390–400 vs ~27)**
  — a per-block allocation cost (frequency tables, code tables, token buffers)
  that is the clearest thing left to optimize.
- **`flate` wins decode across the board (≈1.8–3×, and ~25× on the highly
  repetitive input).** Our decoder is a small, correct, bit-serial count/symbol
  Huffman design that copies back-references byte-by-byte; `flate`'s is a mature
  table-driven decoder with fast bulk copies. Decode is inherently bit-serial and
  not SIMD-amenable, so this package targets correctness and wire-compatibility
  here rather than raw speed.
- **The SIMD `matchlen` kernel is in the encoder's hot path**, but — as with the
  sibling `lz4` — end-to-end gains are bounded because encode time is dominated
  by match-*finding* (hash chains), not match-*extension*.

In short: **correctness-first and wire-compatible; competitive encode on text,
slower decode than the standard library.** If raw throughput on arbitrary data
is the priority, `compress/flate` is still the better choice; the value here is
a clean, fully-tested, SIMD-`matchlen` DEFLATE that interoperates with it
exactly.

## License

BSD-3-Clause. See [LICENSE](LICENSE).
