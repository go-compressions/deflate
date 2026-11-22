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
whose SIMD common-prefix kernel accelerates match extension on four of Go's
64-bit targets — amd64 (SSE2), arm64 (NEON), riscv64 (RVV) and loong64 (LSX) —
with a portable word-at-a-time scalar fallback elsewhere, on a plain `go build`.

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
orig, _ := deflate.Inflate(nil, comp)      // DEFLATE -> src, at most 1 GiB

// Untrusted input: choose the ceiling.
orig, err := deflate.InflateLimit(nil, comp, 64<<20) // deflate.ErrTooLarge past 64 MiB

// A DEFLATE stream followed by more bytes (zlib's Adler-32, a gzip trailer):
// n is how many input bytes the stream occupied.
orig, n, err := deflate.InflatePrefix(nil, zlibBody, 64<<20)
```

`NewWriter` accepts `DefaultCompression`, `NoCompression` and the range
`BestSpeed`..`BestCompression` (`-1`, `0`, `1`..`9`), mirroring `compress/flate`.

## How it works

**Whole-buffer decoder (v0.2.0).** `Inflate`, `InflateLimit` and `InflatePrefix`
decode through `inflateBuf`, which follows libdeflate's design with a fully
bounds-checked loop near the ends of the buffers. **Streaming decoder (v0.3.0).**
`NewReader` runs the same table decoder over a 64 KiB input window and a 32 KiB
history plus 256 KiB of output, resuming a block whenever it needs more input or the
output is full (a bare `io.ByteReader` source keeps the bit-serial count/symbol
decoder, the zlib *puff* method, because it cannot be read ahead). It handles all three block types — stored, fixed
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

## Untrusted input

DEFLATE expands without limit: a length-258, distance-1 match costs two bits, so
**a megabyte of input is a gigabyte of output (1032:1)**, and a single block has
no size limit in the format.

- **`NewReader` holds a bounded amount whatever the stream says.** A Huffman block
  used to be decoded in one go, so reading *one byte* of a 1 MB stream decoded the
  whole 1.03 GB block first (measured: 6.17 GB allocated, 2.2 GB live, 33 s). The
  block now yields to `Read` every 64 KiB of undelivered output and resumes with its
  tables; draining the same stream allocates 366 KB in total. The expansion itself
  is paid by the caller reading, so a caller that collects everything
  (`io.ReadAll`) should cap it with `io.LimitReader`.
- **`Inflate` stops at `DefaultMaxOutput` (1 GiB)** with `ErrTooLarge`;
  `InflateLimit(dst, src, maxOut)` sets another. Nothing past the ceiling is decoded.
- Header sizes are all bounded by the format (at most 288 + 32 code lengths, stored
  blocks at most 64 KiB), so no allocation follows a declared size.
- Fuzzed: `FuzzInflate` (differential against `compress/flate`), `FuzzReader`,
  `FuzzRoundTrip` and `FuzzInflateAgrees` (the whole-buffer decoder against the
  streaming one). A nightly workflow fuzzes every `FuzzXxx` it finds. The
  whole-buffer decoder is also checked against libdeflate v1.23 by a build-tagged
  differential test (139253 inputs, 0 divergences).
- The encoder buffers its whole input until `Close`, plus a 4-byte-per-input-byte
  match table and its whole output: it is not a streaming encoder, and its memory
  follows what the caller wrote, not any field of a stream.

## Benchmarks

Reproducible via the committed benchmarks — no external corpus, the inputs are
generated deterministically in-process:

```sh
go test -run=^$ -bench=. -benchmem
```

The encode numbers below are from an **Apple M4 Max** (`darwin/arm64`), Go 1.26.4, on four
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

### Decode (whole-buffer `Inflate`, v0.2.0, as of 2026-11-22)

`Inflate`, `InflateLimit` and `InflatePrefix` use a libdeflate-style whole-buffer
decoder. Apple M4 Max, single core, 128 KiB blocks, MB/s of output (higher is
better), ours against klauspost/compress, libdeflate and zlib decoding the same
streams (`synth`, `gosrc` and `binary` are the corpora, `l3` and `l9` the level
that compressed them):

| corpus | ours | klauspost | libdeflate | zlib |
|---|--:|--:|--:|--:|
| synth-l3  | 1435 | 522 | 1714 | 1604 |
| synth-l9  | 1596 | 543 | 1414 | 1735 |
| gosrc-l3  | 1579 | 629 | 1692 | 1798 |
| gosrc-l9  | 2076 | 737 | 2120 | 2135 |
| binary-l3 | 665  | 251 | 704  | 700  |
| binary-l9 | 654  | 244 | 679  | 663  |

That is about 2.5 to 3 times klauspost/compress on every corpus, and within about
17 % of libdeflate and zlib (ahead of libdeflate on `synth-l9`). **Streaming
`NewReader` (v0.3.0)** now runs the same table decoder. Its throughput has not been
re-measured: the benchmark host stayed above the load limit, so no timing is
published. What was measured exactly is allocation per full read of a 512 KiB stream
(4 MiB for `gosrc`), v0.2.0 against v0.3.0: `text` 390 KB / 39 allocs against
138 KB / 3, `json` 384 KB / 31 against 12 KB / 3, `binary` 373 KB / 19 against
12 KB / 3, `repetitive` 373 KB / 19 against 138 KB / 3, `gosrc` 569 KB / 251 against
138 KB / 3.

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
- **Whole-buffer decode is fast.** `Inflate` is a
  libdeflate-style decoder (11-bit literal/length and 8-bit offset tables with
  subtables, a branchless 64-bit refill, word-copy matches) that runs at 2.5 to 3
  times klauspost/compress and close to libdeflate and zlib (table above). The
  streaming `NewReader` runs the same decoder since v0.3.0; its speed is not yet
  measured (see above).
- **The SIMD `matchlen` kernel is in the encoder's hot path**, but — as with the
  sibling `lz4` — end-to-end gains are bounded because encode time is dominated
  by match-*finding* (hash chains), not match-*extension*.

In short: **correctness-first and wire-compatible; competitive encode on text,
libdeflate-class whole-buffer decode.** The encoder on
`binary` and `json` at the default level is still slower than `compress/flate`;
the value here is a clean, fully-tested, SIMD-`matchlen` DEFLATE that
interoperates with it exactly.

## License

BSD-3-Clause. See [LICENSE](LICENSE).
