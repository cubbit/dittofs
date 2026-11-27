package carvetest

import (
	_ "embed"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"testing"
)

// vectorsJSON is gen_vectors.py's output.
//
//go:embed testdata/vectors.json
var vectorsJSON []byte

// Vectors is the script's output: values computed from RFC 2 §3.2 and §4.2 by
// BLAKE3's reference binding, with their inputs.
type Vectors struct {
	GearUnkeyed [256]uint64 // decoded little-endian from the file's bytes
	ChunkingKey [32]byte
	GearKeyed   [256]uint64
	ChunkIDKey  [32]byte
	ChunkInput  []byte // the bytes the chunk-ID vector hashes
	ChunkID     [32]byte
	Name        NameVector
}

// NameVector is RFC 2 §4.2's golden name with its inputs.
type NameVector struct {
	Scope   [16]byte // the namespace ID's raw bytes
	Nonce   [16]byte
	ChainID [32]byte   // computed from the chain's inputs by RFC 5 §2.8's table
	IDs     [][32]byte // in block order
	Name    [32]byte
}

// vectorsFile is the file's layout, every byte string in hex.
type vectorsFile struct {
	GearUnkeyed struct {
		Context string `json:"context"`
		gearFile
	} `json:"gear_unkeyed"`
	GearKeyed struct {
		Key   string `json:"key"`
		Input string `json:"input"`
		gearFile
	} `json:"gear_keyed"`
	ChunkID struct {
		Key   string `json:"key"`
		Input string `json:"input"`
		ID    string `json:"id"`
	} `json:"chunk_id"`
	Name struct {
		Context string `json:"context"`
		Scope   string `json:"scope"`
		Nonce   string `json:"nonce"`
		Chain   struct {
			Context    string `json:"context"`
			Transforms []struct {
				ID        uint16 `json:"id"`
				Version   uint8  `json:"version"`
				LengthKey string `json:"length_key"`
			} `json:"transforms"`
			Encoded string `json:"encoded"`
			ID      string `json:"id"`
		} `json:"chain"`
		IDs  []string `json:"ids"`
		Name string   `json:"name"`
	} `json:"name"`
}

type gearFile struct {
	Bytes     string `json:"bytes"`
	FirstWord string `json:"first_word"` // decimal, decoded little-endian by the script
	LastWord  string `json:"last_word"`
}

// LoadVectors decodes the embedded file and fails tb on a malformed or short
// value.
func LoadVectors(tb testing.TB) Vectors {
	tb.Helper()

	f := loadFile(tb)

	v := Vectors{
		GearUnkeyed: gear(tb, "gear_unkeyed.bytes", f.GearUnkeyed.Bytes),
		GearKeyed:   gear(tb, "gear_keyed.bytes", f.GearKeyed.Bytes),
		ChunkInput:  decode(tb, "chunk_id.input", f.ChunkID.Input),
	}
	copy(v.ChunkingKey[:], fixed(tb, "gear_keyed.key", f.GearKeyed.Key, 32))
	copy(v.ChunkIDKey[:], fixed(tb, "chunk_id.key", f.ChunkID.Key, 32))
	copy(v.ChunkID[:], fixed(tb, "chunk_id.id", f.ChunkID.ID, 32))
	copy(v.Name.Scope[:], fixed(tb, "name.scope", f.Name.Scope, 16))
	copy(v.Name.Nonce[:], fixed(tb, "name.nonce", f.Name.Nonce, 16))
	copy(v.Name.ChainID[:], fixed(tb, "name.chain.id", f.Name.Chain.ID, 32))
	copy(v.Name.Name[:], fixed(tb, "name.name", f.Name.Name, 32))

	if len(f.Name.IDs) == 0 {
		tb.Fatal("vectors: name.ids is empty, want the block's chunk IDs")
	}

	v.Name.IDs = make([][32]byte, len(f.Name.IDs))
	for i, s := range f.Name.IDs {
		copy(v.Name.IDs[i][:], fixed(tb, "name.ids", s, 32))
	}

	return v
}

func loadFile(tb testing.TB) vectorsFile {
	tb.Helper()

	var f vectorsFile
	if err := json.Unmarshal(vectorsJSON, &f); err != nil {
		tb.Fatalf("vectors: decoding testdata/vectors.json: %v", err)
	}

	return f
}

func decode(tb testing.TB, field, s string) []byte {
	tb.Helper()

	b, err := hex.DecodeString(s)
	if err != nil {
		tb.Fatalf("vectors: %s: %v", field, err)
	}

	return b
}

// fixed decodes s, failing tb unless it is exactly n bytes.
func fixed(tb testing.TB, field, s string, n int) []byte {
	tb.Helper()

	b := decode(tb, field, s)
	if len(b) != n {
		tb.Fatalf("vectors: %s is %d bytes, want %d", field, len(b), n)
	}

	return b
}

// gear decodes a 2,048-byte table into its 256 words, little-endian whatever
// the host's byte order (RFC 2 §3.2).
func gear(tb testing.TB, field, s string) [256]uint64 {
	tb.Helper()

	var g [256]uint64

	b := fixed(tb, field, s, 8*len(g))
	for i := range g {
		g[i] = binary.LittleEndian.Uint64(b[8*i:])
	}

	return g
}
