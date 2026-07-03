package deflate

import (
	"bytes"
	stdflate "compress/flate"
	"io"
	"testing"
)

func rt(t *testing.T, data []byte, level int) {
	t.Helper()
	var buf bytes.Buffer
	w, err := NewWriter(&buf, level)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	// our decoder
	got, err := io.ReadAll(NewReader(bytes.NewReader(buf.Bytes())))
	if err != nil {
		t.Fatalf("our inflate: %v", err)
	}
	if !bytes.Equal(got, data) {
		t.Fatalf("our round-trip mismatch: got %d want %d bytes", len(got), len(data))
	}
	// stdlib decodes our output
	sg, err := io.ReadAll(stdflate.NewReader(bytes.NewReader(buf.Bytes())))
	if err != nil {
		t.Fatalf("stdlib inflate of our output: %v", err)
	}
	if !bytes.Equal(sg, data) {
		t.Fatalf("stdlib round-trip mismatch")
	}
	// we decode stdlib output
	var sb bytes.Buffer
	sw, _ := stdflate.NewWriter(&sb, level)
	sw.Write(data)
	sw.Close()
	og, err := io.ReadAll(NewReader(bytes.NewReader(sb.Bytes())))
	if err != nil {
		t.Fatalf("our inflate of stdlib output: %v", err)
	}
	if !bytes.Equal(og, data) {
		t.Fatalf("our decode of stdlib output mismatch")
	}
}

func TestSmoke(t *testing.T) {
	cases := [][]byte{
		nil,
		[]byte("a"),
		[]byte("hello hello hello hello world"),
		bytes.Repeat([]byte("A"), 100000),
		[]byte("The quick brown fox jumps over the lazy dog. " +
			"The quick brown fox jumps over the lazy dog."),
	}
	for _, level := range []int{0, 1, 6, 9, -1} {
		for i, c := range cases {
			t.Run("", func(t *testing.T) {
				_ = i
				rt(t, c, level)
			})
		}
	}
}
