package main

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
)

// A GGUF file starts with the magic "GGUF", a version, the tensor count and
// the metadata count, then the metadata as key-value pairs, then the tensors
// (GGUF version 3, ggml's docs/gguf.md). A key and a string value are each a
// little-endian uint64 length followed by that many bytes.

// ggufString finds the string value of key in a GGUF file's metadata and
// returns it with the offset of its first byte, so the value can be replaced
// by one of the same length without moving anything after it.
func ggufString(name, key string) (value string, offset int64, err error) {
	f, err := os.Open(name)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	r := &ggufReader{r: bufio.NewReaderSize(f, 1<<20)}
	if magic := r.bytes(4); string(magic) != "GGUF" {
		return "", 0, fmt.Errorf("'%s' isn't a GGUF file", name)
	}
	if version := r.u32(); version != 3 {
		return "", 0, fmt.Errorf("'%s' is GGUF version %d; only version 3 is read", name, version)
	}
	r.u64() // tensors
	count := r.u64()
	for i := uint64(0); i < count && r.err == nil; i++ {
		k := r.string()
		typ := r.u32()
		if k == key && typ == ggufTypeString {
			length := r.u64()
			at := r.offset
			return string(r.bytes(int(length))), at, r.err
		}
		r.skip(typ)
	}
	if r.err != nil {
		return "", 0, fmt.Errorf("reading '%s': %w", name, r.err)
	}
	return "", 0, fmt.Errorf("'%s' has no string '%s'", name, key)
}

const (
	ggufTypeString = 8
	ggufTypeArray  = 9
)

// ggufSizes are the byte sizes of the fixed-size GGUF value types.
var ggufSizes = map[uint32]int64{0: 1, 1: 1, 2: 2, 3: 2, 4: 4, 5: 4, 6: 4, 7: 1, 10: 8, 11: 8, 12: 8}

// ggufReader reads GGUF fields in order, counting the offset and keeping the
// first error, so a parse reads straight through and checks once.
type ggufReader struct {
	r      *bufio.Reader
	offset int64
	err    error
}

func (g *ggufReader) bytes(n int) []byte {
	if g.err != nil {
		return nil
	}
	b := make([]byte, n)
	read, err := io.ReadFull(g.r, b)
	g.offset += int64(read)
	g.err = err
	return b
}

func (g *ggufReader) u32() uint32 {
	b := g.bytes(4)
	if g.err != nil {
		return 0
	}
	return binary.LittleEndian.Uint32(b)
}

func (g *ggufReader) u64() uint64 {
	b := g.bytes(8)
	if g.err != nil {
		return 0
	}
	return binary.LittleEndian.Uint64(b)
}

func (g *ggufReader) string() string {
	return string(g.bytes(int(g.u64())))
}

func (g *ggufReader) discard(n int64) {
	if g.err != nil {
		return
	}
	discarded, err := g.r.Discard(int(n))
	g.offset += int64(discarded)
	g.err = err
}

// skip reads past one value of type typ.
func (g *ggufReader) skip(typ uint32) {
	switch {
	case typ == ggufTypeString:
		g.discard(int64(g.u64()))
	case typ == ggufTypeArray:
		elem := g.u32()
		n := g.u64()
		if size, ok := ggufSizes[elem]; ok {
			g.discard(size * int64(n))
			return
		}
		for i := uint64(0); i < n && g.err == nil; i++ {
			g.skip(elem)
		}
	default:
		size, ok := ggufSizes[typ]
		if !ok && g.err == nil {
			g.err = errors.New("unknown GGUF value type " + fmt.Sprint(typ))
		}
		g.discard(size)
	}
}
