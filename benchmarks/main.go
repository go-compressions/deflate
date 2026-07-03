package main

import (
	"bytes"
	stdflate "compress/flate"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/go-compressions/deflate"
)

func timeIt(f func()) time.Duration {
	best := time.Hour
	for i := 0; i < 5; i++ {
		t := time.Now()
		f()
		if d := time.Since(t); d < best {
			best = d
		}
	}
	return best
}

func mbps(n int, d time.Duration) float64 { return float64(n) / 1e6 / d.Seconds() }

func main() {
	files := []struct{ name, path string }{
		{"text", "corpus/text.txt"},
		{"json", "corpus/data.json"},
		{"binary", "corpus/binary.bin"},
	}
	levels := []struct {
		name      string
		ours, std int
	}{
		{"speed", deflate.BestSpeed, stdflate.BestSpeed},
		{"default", deflate.DefaultCompression, stdflate.DefaultCompression},
		{"best", deflate.BestCompression, stdflate.BestCompression},
	}
	fmt.Printf("%-8s %-8s | %-28s | %-28s | %-16s\n", "corpus", "level", "ENCODE ours vs flate MB/s", "DECODE ours vs flate MB/s", "ratio ours/flate")
	for _, f := range files {
		data, err := os.ReadFile(f.path)
		if err != nil {
			fmt.Println("skip", f.path, err)
			continue
		}
		n := len(data)
		for _, lv := range levels {
			// our encode
			var ourComp []byte
			encOurs := timeIt(func() {
				var b bytes.Buffer
				w, _ := deflate.NewWriter(&b, lv.ours)
				w.Write(data)
				w.Close()
				ourComp = b.Bytes()
			})
			// flate encode
			var stdComp []byte
			encStd := timeIt(func() {
				var b bytes.Buffer
				w, _ := stdflate.NewWriter(&b, lv.std)
				w.Write(data)
				w.Close()
				stdComp = b.Bytes()
			})
			// our decode
			decOurs := timeIt(func() {
				r := deflate.NewReader(bytes.NewReader(ourComp))
				io.Copy(io.Discard, r)
			})
			// flate decode (of flate's output)
			decStd := timeIt(func() {
				r := stdflate.NewReader(bytes.NewReader(stdComp))
				io.Copy(io.Discard, r)
			})
			ourRatio := float64(len(ourComp)) / float64(n)
			stdRatio := float64(len(stdComp)) / float64(n)
			fmt.Printf("%-8s %-8s | %7.0f vs %7.0f  (%.2fx) | %7.0f vs %7.0f  (%.2fx) | %.3f / %.3f\n",
				f.name, lv.name,
				mbps(n, encOurs), mbps(n, encStd), mbps(n, encOurs)/mbps(n, encStd),
				mbps(n, decOurs), mbps(n, decStd), mbps(n, decOurs)/mbps(n, decStd),
				ourRatio, stdRatio)
		}
	}
}
