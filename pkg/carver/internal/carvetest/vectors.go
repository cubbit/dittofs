package carvetest

import (
	_ "embed"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
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

	v, err := parseVectors(vectorsJSON)
	if err != nil {
		tb.Fatalf("vectors: %v", err)
	}

	return v
}

// parseVectors decodes raw, refusing a value that is not hex or not its
// field's length, and a name with no chunk IDs.
func parseVectors(raw []byte) (Vectors, error) {
	var f vectorsFile
	if err := json.Unmarshal(raw, &f); err != nil {
		return Vectors{}, fmt.Errorf("decoding testdata/vectors.json: %w", err)
	}

	var d decoder

	v := Vectors{
		GearUnkeyed: d.gear("gear_unkeyed.bytes", f.GearUnkeyed.Bytes),
		GearKeyed:   d.gear("gear_keyed.bytes", f.GearKeyed.Bytes),
		ChunkInput:  d.hex("chunk_id.input", f.ChunkID.Input),
	}
	d.fixed(v.ChunkingKey[:], "gear_keyed.key", f.GearKeyed.Key)
	d.fixed(v.ChunkIDKey[:], "chunk_id.key", f.ChunkID.Key)
	d.fixed(v.ChunkID[:], "chunk_id.id", f.ChunkID.ID)
	d.fixed(v.Name.Scope[:], "name.scope", f.Name.Scope)
	d.fixed(v.Name.Nonce[:], "name.nonce", f.Name.Nonce)
	d.fixed(v.Name.ChainID[:], "name.chain.id", f.Name.Chain.ID)
	d.fixed(v.Name.Name[:], "name.name", f.Name.Name)

	if len(v.ChunkInput) == 0 {
		d.fail(errors.New("chunk_id.input is empty, want the bytes the chunk ID hashes"))
	}

	if len(f.Name.IDs) == 0 {
		d.fail(errors.New("name.ids is empty, want the block's chunk IDs"))
	}

	v.Name.IDs = make([][32]byte, len(f.Name.IDs))
	for i, s := range f.Name.IDs {
		d.fixed(v.Name.IDs[i][:], fmt.Sprintf("name.ids[%d]", i), s)
	}

	return v, d.err
}

// decoder keeps the first error of a run of decodes.
type decoder struct{ err error }

func (d *decoder) fail(err error) {
	if d.err == nil {
		d.err = err
	}
}

func (d *decoder) hex(field, s string) []byte {
	b, err := hex.DecodeString(s)
	if err != nil {
		d.fail(fmt.Errorf("%s: %w", field, err))
	}

	return b
}

// fixed decodes s into dst, refusing it unless it is exactly len(dst) bytes.
func (d *decoder) fixed(dst []byte, field, s string) {
	b := d.hex(field, s)
	if len(b) != len(dst) {
		d.fail(fmt.Errorf("%s is %d bytes, want %d", field, len(b), len(dst)))
		return
	}

	copy(dst, b)
}

// gear decodes a 2,048-byte table into its 256 words, little-endian whatever
// the host's byte order (RFC 2 §3.2).
func (d *decoder) gear(field, s string) [256]uint64 {
	var g [256]uint64

	b := make([]byte, 8*len(g))
	d.fixed(b, field, s)

	for i := range g {
		g[i] = binary.LittleEndian.Uint64(b[8*i:])
	}

	return g
}
