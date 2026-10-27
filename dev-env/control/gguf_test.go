package main

import (
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
)

// ggufFile writes a minimal GGUF version 3 file: an integer, an array of
// strings like the tokenizer's, then general.name, then tensor bytes.
func ggufFile(t *testing.T, name string) string {
	t.Helper()
	var b bytes.Buffer
	put := func(v any) { binary.Write(&b, binary.LittleEndian, v) }
	str := func(s string) { put(uint64(len(s))); b.WriteString(s) }
	b.WriteString("GGUF")
	put(uint32(3))
	put(uint64(0)) // tensors
	put(uint64(3)) // metadata
	str("general.quantization_version")
	put(uint32(4))
	put(uint32(2))
	str("tokenizer.ggml.tokens")
	put(uint32(ggufTypeArray))
	put(uint32(ggufTypeString))
	put(uint64(2))
	str("a")
	str("bc")
	str("general.name")
	put(uint32(ggufTypeString))
	str(name)
	b.WriteString("tensor bytes")
	path := filepath.Join(t.TempDir(), "m.gguf")
	if err := os.WriteFile(path, b.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// general.name is found past an array of strings, at the offset of its
// first byte, so a copy can overwrite it in place.
func TestGGUFString_FindsAValueAndItsOffset(t *testing.T) {
	path := ggufFile(t, "Qwen3 0.6B Instruct")
	value, offset, err := ggufString(path, "general.name")
	if err != nil || value != "Qwen3 0.6B Instruct" {
		t.Fatalf("got %q, %v", value, err)
	}
	data, _ := os.ReadFile(path)
	if got := string(data[offset : offset+int64(len(value))]); got != value {
		t.Errorf("offset %d holds %q", offset, got)
	}
	if _, _, err := ggufString(path, "general.description"); err == nil {
		t.Error("a missing key: no error")
	}
}

// A copy's name keeps the original's length, so nothing after it moves.
func TestCopyName_KeepsTheLength(t *testing.T) {
	got, err := copyName("Qwen3 0.6B Instruct", 2)
	if err != nil || got != "Qwen3 0.6B Instr(2)" || len(got) != len("Qwen3 0.6B Instruct") {
		t.Errorf("got %q, %v", got, err)
	}
	if _, err := copyName("ab", 10); err == nil {
		t.Error("a name too short: no error")
	}
}
