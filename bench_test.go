package deflate

import (
	"bytes"
	stdflate "compress/flate"
	"encoding/json"
	"io"
	"math/rand"
	"testing"
)

// The benchmark corpora are generated deterministically in-process so the suite
// is self-contained and reproducible on every architecture and CI runner. Each
// is ~512 KiB. Run with:
//
//	go test -run=^$ -bench=. -benchmem
//
// Benchmarks report throughput via b.SetBytes (ns/op -> MB/s), allocations via
// ReportAllocs, and, for encoders, the compression "ratio" (compressed/original,
// lower is better) via a custom metric.

const benchSize = 512 << 10

var benchWords = []string{
	"the", "quick", "brown", "fox", "jumps", "over", "a", "lazy", "dog", "and",
	"then", "runs", "through", "green", "fields", "under", "bright", "summer",
	"skies", "while", "birds", "sing", "softly", "in", "the", "distant", "trees",
	"as", "rivers", "flow", "toward", "the", "wide", "and", "restless", "ocean",
}

func benchText() []byte {
	r := rand.New(rand.NewSource(1))
	var b bytes.Buffer
	for b.Len() < benchSize {
		n := 6 + r.Intn(10)
		for i := 0; i < n; i++ {
			if i > 0 {
				b.WriteByte(' ')
			}
			b.WriteString(benchWords[r.Intn(len(benchWords))])
		}
		b.WriteString(".\n")
	}
	return b.Bytes()[:benchSize]
}

func benchJSON() []byte {
	r := rand.New(rand.NewSource(2))
	type rec struct {
		ID     int      `json:"id"`
		Name   string   `json:"name"`
		Email  string   `json:"email"`
		Active bool     `json:"active"`
		Score  float64  `json:"score"`
		Tags   []string `json:"tags"`
	}
	tags := []string{"alpha", "beta", "gamma", "delta"}
	var rows []rec
	for i := 0; len(rows)*80 < benchSize; i++ {
		var tg []string
		for j := 0; j < r.Intn(4); j++ {
			tg = append(tg, tags[r.Intn(len(tags))])
		}
		rows = append(rows, rec{
			ID: i, Name: "user_" + benchWords[r.Intn(len(benchWords))],
			Email:  "user@example.com",
			Active: r.Intn(2) == 0, Score: float64(r.Intn(100000)) / 1000, Tags: tg,
		})
	}
	out, _ := json.Marshal(rows)
	if len(out) > benchSize {
		out = out[:benchSize]
	}
	return out
}

func benchBinary() []byte {
	// Partially compressible: interleave random spans with repeated spans, like
	// real executables (code entropy plus repeated padding/tables).
	r := rand.New(rand.NewSource(3))
	out := make([]byte, 0, benchSize)
	block := make([]byte, 256)
	for len(out) < benchSize {
		if r.Intn(3) == 0 {
			// a repeated/structured span
			r.Read(block[:16])
			for i := 0; i < 8; i++ {
				out = append(out, block[:16]...)
			}
		} else {
			r.Read(block)
			out = append(out, block...)
		}
	}
	return out[:benchSize]
}

func benchRepetitive() []byte {
	pat := []byte("The quick brown fox jumps over the lazy dog. ")
	out := make([]byte, 0, benchSize)
	for len(out) < benchSize {
		out = append(out, pat...)
	}
	return out[:benchSize]
}

type benchCorpus struct {
	name string
	data []byte
}

func benchCorpora() []benchCorpus {
	return []benchCorpus{
		{"text", benchText()},
		{"json", benchJSON()},
		{"binary", benchBinary()},
		{"repetitive", benchRepetitive()},
	}
}

var benchLevels = []struct {
	name      string
	ours, std int
}{
	{"default", DefaultCompression, stdflate.DefaultCompression},
	{"best", BestCompression, stdflate.BestCompression},
}

func encodeOurs(data []byte, level int) []byte {
	var buf bytes.Buffer
	w, _ := NewWriter(&buf, level)
	_, _ = w.Write(data)
	_ = w.Close()
	return buf.Bytes()
}

func encodeStd(data []byte, level int) []byte {
	var buf bytes.Buffer
	w, _ := stdflate.NewWriter(&buf, level)
	_, _ = w.Write(data)
	_ = w.Close()
	return buf.Bytes()
}

func runEncode(b *testing.B, data []byte, level int, ours bool) {
	b.SetBytes(int64(len(data)))
	b.ReportAllocs()
	var size int
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		var out []byte
		if ours {
			out = encodeOurs(data, level)
		} else {
			out = encodeStd(data, level)
		}
		size = len(out)
	}
	b.StopTimer()
	b.ReportMetric(float64(size)/float64(len(data)), "ratio")
}

// runDecode benchmarks a decoder on an identical stream (produced by
// compress/flate) so the comparison isolates decoder speed on the same input.
func runDecode(b *testing.B, comp []byte, origLen int, ours bool) {
	b.SetBytes(int64(origLen))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if ours {
			r := NewReader(bytes.NewReader(comp))
			_, _ = io.Copy(io.Discard, r)
			_ = r.Close()
		} else {
			r := stdflate.NewReader(bytes.NewReader(comp))
			_, _ = io.Copy(io.Discard, r)
			_ = r.Close()
		}
	}
}

func BenchmarkEncode(b *testing.B) {
	for _, c := range benchCorpora() {
		for _, lv := range benchLevels {
			b.Run(c.name+"/"+lv.name+"/ours", func(b *testing.B) { runEncode(b, c.data, lv.ours, true) })
			b.Run(c.name+"/"+lv.name+"/flate", func(b *testing.B) { runEncode(b, c.data, lv.std, false) })
		}
	}
}

func BenchmarkDecode(b *testing.B) {
	for _, c := range benchCorpora() {
		for _, lv := range benchLevels {
			comp := encodeStd(c.data, lv.std)
			b.Run(c.name+"/"+lv.name+"/ours", func(b *testing.B) { runDecode(b, comp, len(c.data), true) })
			b.Run(c.name+"/"+lv.name+"/flate", func(b *testing.B) { runDecode(b, comp, len(c.data), false) })
		}
	}
}
