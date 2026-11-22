//go:build libdeflate

package deflate

// Differential test against the real libdeflate (the oracle the fast decoder
// is ported from). Not part of the normal build: it needs a C compiler and a
// libdeflate source tree.
//
//	LIBDEFLATE_SRC=<clone> go test -tags libdeflate -run TestAgreesWithLibdeflate .
//
// Every input is decoded by both; they must agree on accept/refuse and, when
// they accept, on the output (length and FNV-1a hash).

import (
	"bytes"
	cflate "compress/flate"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

const ldCap = 4 << 20

func buildLdcheck(t *testing.T) string {
	src := os.Getenv("LIBDEFLATE_SRC")
	if src == "" {
		t.Skip("LIBDEFLATE_SRC is not set")
	}
	exe := filepath.Join(t.TempDir(), "ldcheck")
	args := []string{"-O2", "-I", src, "-I", filepath.Join(src, "lib"), "-o", exe,
		filepath.Join("testdata", "tools", "ldcheck", "ldcheck.c"),
		filepath.Join(src, "lib", "deflate_decompress.c"),
		filepath.Join(src, "lib", "utils.c")}
	if matches, _ := filepath.Glob(filepath.Join(src, "lib", "*", "cpu_features.c")); len(matches) > 0 {
		for _, m := range matches {
			args = append(args, m)
		}
	}
	if out, err := exec.Command("cc", args...).CombinedOutput(); err != nil {
		t.Fatalf("building ldcheck: %v\n%s", err, out)
	}
	return exe
}

// stdRefuses reports whether compress/flate refuses the stream (any error, or
// output past the cap).
func stdRefuses(src []byte) bool {
	r := cflate.NewReader(bytes.NewReader(src))
	n, err := io.Copy(io.Discard, io.LimitReader(r, ldCap+1))
	return err != nil || n > ldCap
}

func fnv64(b []byte) uint64 {
	h := uint64(0xcbf29ce484222325)
	for _, c := range b {
		h ^= uint64(c)
		h *= 0x100000001b3
	}
	return h
}

// ours returns libdeflate's result code, length and hash for our decoder.
func ours(src []byte) (int, int, uint64) {
	out, err := InflateLimit(nil, src, ldCap)
	switch {
	case err == nil:
		return 0, len(out), fnv64(out)
	case errors.Is(err, ErrTooLarge):
		return 3, 0, fnv64(nil)
	}
	return 1, 0, fnv64(nil)
}

func committedSeeds(t *testing.T) [][]byte {
	files, _ := filepath.Glob(filepath.Join("testdata", "fuzz", "*", "*"))
	var seeds [][]byte
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		_, rest, _ := strings.Cut(string(b), "\n")
		rest = strings.TrimSpace(rest)
		rest = strings.TrimSuffix(strings.TrimPrefix(rest, "[]byte("), ")")
		s, err := strconv.Unquote(rest)
		if err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		seeds = append(seeds, []byte(s))
	}
	return seeds
}

func TestAgreesWithLibdeflate(t *testing.T) {
	exe := buildLdcheck(t)
	var inputs [][]byte
	var names []string
	add := func(name string, b []byte) {
		inputs = append(inputs, b)
		names = append(names, name)
	}
	cp := corpus()
	cnames := make([]string, 0, len(cp))
	for name := range cp {
		cnames = append(cnames, name)
	}
	slices.Sort(cnames) // the damage below is drawn from one stream: fix the order
	for _, name := range cnames {
		data := cp[name]
		for _, level := range []int{1, 3, 6, 9} {
			for _, flush := range []int{0, 1000} {
				add(fmt.Sprintf("%s/l%d/f%d", name, level, flush), compressStd(t, data, level, flush))
			}
		}
		add(name+"/ours", Deflate(nil, data))
	}
	for i, s := range committedSeeds(t) {
		add(fmt.Sprintf("seed%d", i), s)
	}
	mutants := 60 // per stream; LD_MUTANTS raises it for a longer run
	if v, err := strconv.Atoi(os.Getenv("LD_MUTANTS")); err == nil && v > 0 {
		mutants = v
	}
	// Damage: bit flips and truncations of every small stream so far.
	r := rand.New(rand.NewPCG(7, 8))
	base := len(inputs)
	for i := 0; i < base; i++ {
		if len(inputs[i]) > 20000 {
			continue
		}
		for k := 0; k < mutants; k++ {
			m := bytes.Clone(inputs[i])
			if len(m) == 0 {
				continue
			}
			for f := 1 + r.IntN(3); f > 0; f-- {
				m[r.IntN(len(m))] ^= 1 << r.IntN(8)
			}
			if r.IntN(4) == 0 {
				m = m[:r.IntN(len(m)+1)]
			}
			add(fmt.Sprintf("%s/mut%d", names[i], k), m)
		}
	}
	// Arbitrary bytes, mostly short.
	for k := 0; k < 3000; k++ {
		m := make([]byte, r.IntN(64))
		for j := range m {
			m[j] = byte(r.Uint32())
		}
		add(fmt.Sprintf("rand%d", k), m)
	}

	dir := t.TempDir()
	const batch = 200
	var agree, accepted, laxer int
	for lo := 0; lo < len(inputs); lo += batch {
		hi := min(lo+batch, len(inputs))
		args := []string{strconv.Itoa(ldCap)}
		for i := lo; i < hi; i++ {
			p := filepath.Join(dir, strconv.Itoa(i))
			if err := os.WriteFile(p, inputs[i], 0o600); err != nil {
				t.Fatal(err)
			}
			args = append(args, p)
		}
		out, err := exec.Command(exe, args...).Output()
		if err != nil {
			t.Fatalf("ldcheck: %v", err)
		}
		lines := strings.Split(strings.TrimSpace(string(out)), "\n")
		if len(lines) != hi-lo {
			t.Fatalf("ldcheck printed %d lines for %d files", len(lines), hi-lo)
		}
		for i := lo; i < hi; i++ {
			var rc, n int
			var h uint64
			if _, err := fmt.Sscanf(lines[i-lo], "%d %d %x", &rc, &n, &h); err != nil {
				t.Fatalf("%q: %v", lines[i-lo], err)
			}
			orc, on, oh := ours(inputs[i])
			if rc == 2 {
				rc = 1
			}
			if rc == 0 && orc == 1 && stdRefuses(inputs[i]) {
				// libdeflate decodes the symbols RFC 1951 forbids (litlen 286
				// and 287 as length 258, offsets 30 and 31 as 24577+13 bits,
				// an empty offset code as offset 1); zlib and compress/flate
				// refuse them, and so do we. Only that difference is allowed.
				laxer++
				continue
			}
			if orc != rc || on != n || oh != h {
				_, oerr := InflateLimit(nil, inputs[i], ldCap)
				t.Errorf("%s (%d bytes): libdeflate rc=%d n=%d h=%x, ours rc=%d n=%d h=%x (%v)",
					names[i], len(inputs[i]), rc, n, h, orc, on, oh, oerr)
				continue
			}
			agree++
			if rc == 0 {
				accepted++
			}
		}
	}
	t.Logf("%d inputs agree with libdeflate, %d of them accepted; %d accepted by libdeflate only (symbols RFC 1951 forbids)",
		agree, accepted, laxer)
	if laxer == 0 {
		t.Error("no stream used a forbidden symbol: the damage no longer reaches them")
	}
}
