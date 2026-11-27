package carvetest

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"os"
	"strconv"
	"strings"
	"testing"

	"lukechampine.com/blake3"
)

// The constants of RFC 2 §3.2 and §4.2 and RFC 5 §2.8, written here from the
// RFCs' text rather than read from the file, so a script that drifts from the
// text fails as a hand edit of the file does.
const (
	gearContext   = "dittofs gear table v1"
	chunkingInput = "dittofs chunking key v1"
	nameContext   = "dittofs content-defined block name v2"
	chainContext  = "dittofs transform chain id v1"
)

// loadFile is the embedded file as the script wrote it.
func loadFile(t *testing.T) vectorsFile {
	t.Helper()

	var f vectorsFile
	if err := json.Unmarshal(vectorsJSON, &f); err != nil {
		t.Fatalf("decoding testdata/vectors.json: %v", err)
	}

	return f
}

// words fails t naming field and the first index where got is not want.
func words(t *testing.T, field string, got, want [256]uint64) {
	t.Helper()

	for i := range got {
		if got[i] != want[i] {
			t.Errorf("%s: word %d is %d, recomputed %d", field, i, got[i], want[i])
			return
		}
	}
}

// le reads b as little-endian 64-bit words, the way RFC 2 §3.2 defines a table.
func le(b []byte) (g [256]uint64) {
	for i := range g {
		g[i] = binary.LittleEndian.Uint64(b[8*i:])
	}

	return g
}

// same fails t naming field when got is not want.
func same(t *testing.T, field string, got, want []byte) {
	t.Helper()

	if bytes.Equal(got, want) {
		return
	}

	i := 0
	for i < min(len(got), len(want)) && got[i] == want[i] {
		i++
	}

	t.Errorf("%s: recomputed %d bytes, file holds %d, first differing at byte %d: recomputed %x, file %x",
		field, len(got), len(want), i, got[i:min(i+8, len(got))], want[i:min(i+8, len(want))])
}

func unhex(t *testing.T, field, s string) []byte {
	t.Helper()

	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatalf("%s: %v", field, err)
	}

	return b
}

// xof is the first n bytes of h's extended output over data.
func xof(h *blake3.Hasher, data []byte, n int) []byte {
	_, _ = h.Write(data) // a Hasher's Write never fails

	out := make([]byte, n)
	if _, err := h.XOF().Read(out); err != nil {
		panic(err) // an OutputReader fails only past 2⁶⁴ bytes
	}

	return out
}

// encodeChain is RFC 5 §2.8's encoded chain for transforms that use no
// material, the only kind the name vector's chain holds.
func encodeChain(t *testing.T, f vectorsFile) []byte {
	t.Helper()

	// The one transform the name vector's chain holds: ID 0x0001, format
	// version 1, a one-byte length key.
	ts := f.Name.Chain.Transforms
	if len(ts) != 1 || ts[0].ID != 1 || ts[0].Version != 1 || len(ts[0].LengthKey) != 2 {
		t.Fatalf("name.chain.transforms: %+v, want one transform, ID 1, version 1, a one-byte length key", ts)
	}

	out := []byte{byte(len(ts))}

	for _, tr := range ts {
		lk := unhex(t, "name.chain.transforms.length_key", tr.LengthKey)
		out = binary.BigEndian.AppendUint16(out, tr.ID)
		out = append(out, tr.Version)
		out = binary.BigEndian.AppendUint32(out, uint32(len(lk)))
		out = append(out, lk...)
		out = append(out, 0) // material count
	}

	return out
}

// Every value in vectors.json equals its recomputation in Go from its inputs,
// as LoadVectors hands them to the checks that read them.
func TestVectorsRecompute(t *testing.T) {
	v := LoadVectors(t)
	f := loadFile(t)

	for _, c := range []struct{ field, got, want string }{
		{"gear_unkeyed.context", f.GearUnkeyed.Context, gearContext},
		{"gear_keyed.input", f.GearKeyed.Input, chunkingInput},
		{"name.context", f.Name.Context, nameContext},
		{"name.chain.context", f.Name.Chain.Context, chainContext},
	} {
		if c.got != c.want {
			t.Errorf("%s: file holds %q, want %q", c.field, c.got, c.want)
		}
	}

	g := make([]byte, 2048)
	blake3.DeriveKey(g, gearContext, nil)
	words(t, "GearUnkeyed", v.GearUnkeyed, le(g))
	words(t, "GearKeyed", v.GearKeyed, le(xof(blake3.New(2048, v.ChunkingKey[:]), []byte(chunkingInput), 2048)))

	in := make([]byte, 4096)
	for i := range in {
		in[i] = byte(i % 251)
	}

	same(t, "chunk_id.input", in, v.ChunkInput)
	h := blake3.New(32, v.ChunkIDKey[:])
	_, _ = h.Write(v.ChunkInput)
	same(t, "chunk_id.id", h.Sum(nil), v.ChunkID[:])

	enc := encodeChain(t, f)
	same(t, "name.chain.encoded", enc, unhex(t, "name.chain.encoded", f.Name.Chain.Encoded))

	chain := make([]byte, 32)
	blake3.DeriveKey(chain, chainContext, enc)
	same(t, "name.chain.id", chain, v.Name.ChainID[:])

	n := v.Name
	material := append([]byte{0x10}, n.Scope[:]...)
	material = append(material, n.Nonce[:]...)
	material = append(material, 0x20)
	material = append(material, chain...)

	for _, id := range n.IDs {
		material = append(material, id[:]...)
	}

	name := make([]byte, 32)
	blake3.DeriveKey(name, nameContext, material)
	same(t, "name.name", name, n.Name[:])
}

// LoadVectors' first and last words of each table equal the script's own
// little-endian decode, so neither side reads the table in the host's byte
// order.
func TestVectorsWords(t *testing.T) {
	v := LoadVectors(t)
	f := loadFile(t)

	for _, tc := range []struct {
		field string
		got   [256]uint64
		file  gearFile
	}{
		{"gear_unkeyed", v.GearUnkeyed, f.GearUnkeyed.gearFile},
		{"gear_keyed", v.GearKeyed, f.GearKeyed.gearFile},
	} {
		for _, w := range []struct {
			name string
			got  uint64
			want string
		}{{"first_word", tc.got[0], tc.file.FirstWord}, {"last_word", tc.got[255], tc.file.LastWord}} {
			want, err := strconv.ParseUint(w.want, 10, 64)
			if err != nil {
				t.Fatalf("%s.%s: %v", tc.field, w.name, err)
			}

			if w.got != want {
				t.Errorf("%s.%s: LoadVectors decoded %d, the script %d", tc.field, w.name, w.got, want)
			}
		}
	}
}

// TestBLAKE3Modes holds lukechampine.com/blake3 to BLAKE3's published vectors
// in its three modes, each with 131 bytes of extended output, over every input
// length the BLAKE3 tree splits differently, from 0 to 102,400 bytes.
// testdata/blake3/test_vectors.json is test_vectors/test_vectors.json of
// github.com/BLAKE3-team/BLAKE3 at tag 1.8.7 (commit f3149ec5bb54), unchanged
// there since ae3e8e6b3a5a; it is dedicated to the public domain under CC0 1.0
// or licensed under Apache 2.0, at the user's choice.
func TestBLAKE3Modes(t *testing.T) {
	raw, err := os.ReadFile("testdata/blake3/test_vectors.json")
	if err != nil {
		t.Fatal(err)
	}

	var file struct {
		Key     string `json:"key"`
		Context string `json:"context_string"`
		Cases   []struct {
			InputLen  int    `json:"input_len"`
			Hash      string `json:"hash"`
			KeyedHash string `json:"keyed_hash"`
			DeriveKey string `json:"derive_key"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(raw, &file); err != nil {
		t.Fatalf("decoding the published vectors: %v", err)
	}

	// A truncated copy would drop the lengths past one BLAKE3 chunk.
	if n := len(file.Cases); n != 35 || file.Cases[n-1].InputLen != 102400 {
		t.Fatalf("published vectors: %d cases, want 35 ending at 102400 bytes", n)
	}

	key := []byte(file.Key)
	if len(key) != 32 {
		t.Fatalf("published key is %d bytes, want 32", len(key))
	}

	for _, c := range file.Cases {
		in := make([]byte, c.InputLen)
		for i := range in {
			in[i] = byte(i % 251)
		}

		hash := unhex(t, "hash", c.Hash)
		if got := xof(blake3.New(len(hash), nil), in, len(hash)); !bytes.Equal(got, hash) {
			t.Errorf("hash of %d bytes: got %x, want %x", c.InputLen, got, hash)
		}

		if got := blake3.Sum256(in); !bytes.Equal(got[:], hash[:32]) {
			t.Errorf("Sum256 of %d bytes: got %x, want %x", c.InputLen, got, hash[:32])
		}

		keyed := unhex(t, "keyed_hash", c.KeyedHash)
		if got := xof(blake3.New(len(keyed), key), in, len(keyed)); !bytes.Equal(got, keyed) {
			t.Errorf("keyed hash of %d bytes: got %x, want %x", c.InputLen, got, keyed)
		}

		// Sum is the sized path a 32-byte ID takes, apart from the XOF.
		for _, m := range []struct {
			mode string
			key  []byte
			want []byte
		}{{"hash", nil, hash}, {"keyed hash", key, keyed}} {
			h := blake3.New(32, m.key)
			_, _ = h.Write(in)

			if got := h.Sum(nil); !bytes.Equal(got, m.want[:32]) {
				t.Errorf("%s Sum of %d bytes: got %x, want %x", m.mode, c.InputLen, got, m.want[:32])
			}
		}

		derived := unhex(t, "derive_key", c.DeriveKey)

		got := make([]byte, len(derived))
		if blake3.DeriveKey(got, file.Context, in); !bytes.Equal(got, derived) {
			t.Errorf("derived key from %d bytes: got %x, want %x", c.InputLen, got, derived)
		}
	}
}

// parseVectors refuses a value that is not hex or not its field's length, and
// a name with no chunk IDs, so a malformed edit fails at LoadVectors rather
// than reaching a check as zeros.
func TestParseVectorsRefuses(t *testing.T) {
	for _, tc := range []struct {
		name, from, to, want string
	}{
		{"a short key", `"key": "4041`, `"key": "41`, "gear_keyed.key is 31 bytes, want 32"},
		{"a long scope", `"scope": "10`, `"scope": "0010`, "name.scope is 17 bytes, want 16"},
		{"a table that is not hex", `"bytes": "d5`, `"bytes": "zz`, "gear_unkeyed.bytes: encoding/hex"},
		{"a short table", `"bytes": "d5`, `"bytes": "`, "gear_unkeyed.bytes is 2047 bytes, want 2048"},
		{"no chunk IDs", `"ids": [`, `"ids": [], "x": [`, "name.ids is empty"},
		{"no chunk input", `"input": "0001`, `"input": "", "x": "0001`, "chunk_id.input is empty"},
	} {
		raw := strings.Replace(string(vectorsJSON), tc.from, tc.to, 1)
		if raw == string(vectorsJSON) {
			t.Fatalf("%s: %q is not in the file", tc.name, tc.from)
		}

		_, err := parseVectors([]byte(raw))
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: parseVectors error %v, want one containing %q", tc.name, err, tc.want)
		}
	}
}
