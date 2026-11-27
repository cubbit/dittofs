package carve_test

import (
	"testing"

	"github.com/cubbit/dittofs/internal/blake3"
	"github.com/cubbit/dittofs/pkg/carver/carve"
	"github.com/cubbit/dittofs/pkg/carver/internal/carvetest"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
)

// nameInputs is a block name's inputs, as BlockName takes them.
type nameInputs struct {
	scope entity.NamespaceID
	nonce [16]byte
	chain [32]byte
	ids   []entity.ChunkHash
}

func (in nameInputs) name() entity.BlockName {
	return carve.BlockName(in.scope, in.nonce, in.chain, in.ids)
}

// goldenName is the committed name vector's inputs and the name they give.
func goldenName(t *testing.T) (nameInputs, entity.BlockName) {
	t.Helper()
	v := carvetest.LoadVectors(t).Name
	in := nameInputs{scope: v.Scope, nonce: v.Nonce, chain: v.ChainID}
	for _, id := range v.IDs {
		in.ids = append(in.ids, entity.ChunkHash(id))
	}

	return in, v.Name
}

// The committed vector's inputs give its name: a changed construction would
// orphan every stored block.
func TestBlockNameGivesTheGoldenName(t *testing.T) {
	in, want := goldenName(t)
	if got := in.name(); got != want {
		t.Errorf("BlockName over the golden inputs: got %s, want %s", got, want)
	}
}

// Changing any one input changes the name, compared with the unperturbed
// name rather than the golden one, which a name that ignores the input would
// miss as well.
func TestBlockNameMovesWithEachInput(t *testing.T) {
	in, _ := goldenName(t)
	if len(in.ids) < 2 || in.ids[0] == in.ids[1] {
		t.Fatalf("the golden name holds %d IDs, want at least two distinct first ones to swap", len(in.ids))
	}
	base := in.name()

	for _, tc := range []struct {
		what    string
		perturb func(*nameInputs)
	}{
		{"the first two IDs swapped", func(in *nameInputs) { in.ids[0], in.ids[1] = in.ids[1], in.ids[0] }},
		{"one scope byte changed", func(in *nameInputs) { in.scope[7] ^= 1 }},
		{"one nonce byte changed", func(in *nameInputs) { in.nonce[7] ^= 1 }},
		{"one chain ID byte changed", func(in *nameInputs) { in.chain[7] ^= 1 }},
	} {
		p := in
		p.ids = append([]entity.ChunkHash(nil), in.ids...)
		tc.perturb(&p)
		if got := p.name(); got == base {
			t.Errorf("BlockName with %s: got the unperturbed name %s, want another", tc.what, got)
		}
	}
}

// BlockName is BLAKE3's key derivation under the name's context over
// 0x10 ‖ scope ‖ nonce ‖ 0x20 ‖ chain ID ‖ IDs, for the golden block and for
// a block of one chunk. A scope encoded with another length byte gives
// another name: that restates RFC 2 §9's name row and cannot fail once the
// derivation check passes.
func TestBlockNameIsTheDerivationOverItsInputs(t *testing.T) {
	golden, _ := goldenName(t)
	one := golden
	one.ids = golden.ids[:1]

	for _, tc := range []struct {
		what string
		in   nameInputs
	}{
		{"the golden block", golden},
		{"a block of one chunk", one},
	} {
		got := tc.in.name()
		if want := derive(tc.in, 0x10); got != want {
			t.Errorf("BlockName over %s: got %s, want the derivation %s", tc.what, got, want)
		}
		if other := derive(tc.in, 0x11); got == other {
			t.Errorf("BlockName over %s: got %s, also the derivation with the scope's length byte 0x11", tc.what, got)
		}
	}
}

// derive is the name's construction written out, with scopeLen as the
// scope's length byte.
func derive(in nameInputs, scopeLen byte) entity.BlockName {
	material := append([]byte{scopeLen}, in.scope[:]...)
	material = append(material, in.nonce[:]...)
	material = append(material, 0x20)
	material = append(material, in.chain[:]...)
	for _, id := range in.ids {
		material = append(material, id[:]...)
	}

	var h blake3.Hasher
	h.InitDeriveKey("dittofs content-defined block name v2")
	h.Write(material)

	return h.Sum256()
}
