package chain_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/cubbit/dittofs/internal/prof"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/transform"
	"github.com/cubbit/dittofs/pkg/transform/chain"
	"github.com/cubbit/dittofs/pkg/transform/keys"
	"github.com/cubbit/dittofs/pkg/transform/keystest"
)

// fetching is keyed whose Decode reads each material ID its body names from
// the provider it is given, by kind and ID alone, as a body names it. askIn,
// when set, is the namespace it asks in instead of the one being read;
// current makes it ask for the current material instead, as no decode may.
type fetching struct {
	keyed
	askIn   *entity.NamespaceID
	current bool
}

func (f fetching) Decode(ctx context.Context, ns entity.NamespaceID, dst []byte, hash [32]byte, body []byte, limit int, m keys.Provider) ([]byte, error) {
	if f.current {
		if _, err := m.Current(ctx, ns, keys.KindDataKey); err != nil {
			return nil, err
		}
	}
	if f.askIn != nil {
		ns = *f.askIn
	}
	rest := body
	for _, kind := range f.kinds {
		if len(rest) == 0 || len(rest) < 1+int(rest[0]) {
			return nil, transform.ErrMalformed
		}
		id := string(rest[1 : 1+int(rest[0])])
		if _, err := m.Get(ctx, keys.MaterialID{Namespace: ns, Kind: kind, ID: id}); err != nil {
			return nil, err
		}
		rest = rest[1+int(rest[0]):]
	}
	return f.keyed.Decode(ctx, ns, dst, hash, body, limit, m)
}

func (f fetching) factory() transform.Factory {
	return func(context.Context, map[string]any, keys.Provider) (transform.Transform, error) { return f, nil }
}

// otherNS is a second namespace on the census tests' provider.
var otherNS = entity.NamespaceID{2}

// sealer is the encrypt-stage transform the census tests decode through.
var sealer = fetching{keyed: keyed{id: idB, stage: transform.StageEncrypt, version: 1, kinds: []string{keys.KindDataKey}}}

// censusRegistry registers sealer as seal, a second encrypt-stage transform
// as seal2, an identity compressor and a redundancy step adding half.
func censusRegistry() chain.Registry {
	r := chain.NewRegistry()
	r.Register(idA, "plain", pad{id: idA, stage: transform.StageCompress}.factory())
	r.Register(idB, "seal", sealer.factory())
	r.Register(idB+0x10, "seal2", fetching{keyed: keyed{id: idB + 0x10, stage: transform.StageEncrypt, version: 1, kinds: []string{keys.KindDataKey}}}.factory())
	r.Register(idC, "parity", pad{id: idC, stage: transform.StageRedundancy, half: true}.factory())
	return r
}

// sealed encodes chunk through seal under p's current data key of namespace
// n, and returns the body and its census.
func sealed(t *testing.T, r chain.Registry, p keys.Provider, n entity.NamespaceID) ([]byte, []transform.Encoded) {
	t.Helper()
	c := mustBuild(t, r, p, names("seal")...)
	a, err := c.Pin(context.Background(), n)
	if err != nil {
		t.Fatalf("Pin(%s): %v", n, err)
	}
	body, encs, err := a.Encode(context.Background(), nil, hash, []byte("chunk"))
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	return body, encs
}

// census lists ids as one encrypt-stage Encoded.
func census(ids ...keys.MaterialID) []transform.Encoded {
	return []transform.Encoded{{ID: idB, Applied: true, Version: 1, Material: ids}}
}

// TestDecodeAsksOnlyForCensusMaterial holds RFC 5 §8.2's T12 and T14 rows'
// census halves: a body naming material its census does not list, though the
// provider holds it, or material of another namespace, is ErrMalformed, and
// the provider records no call; so is a decode asking for current material.
func TestDecodeAsksOnlyForCensusMaterial(t *testing.T) {
	prof.Watch(t, prof.Options{})
	ctx := context.Background()
	r, p := censusRegistry(), keystest.NewMemory()
	d1 := p.Give(ns, keys.KindDataKey, "d1", span(0, 32))
	body, encs := sealed(t, r, p, ns)
	d2 := p.Give(ns, keys.KindDataKey, "d2", span(32, 64))
	o1 := p.Give(otherNS, keys.KindDataKey, "d1", span(64, 96))
	otherBody, otherEncs := sealed(t, r, p, otherNS)
	if !bytes.Equal(body, otherBody) {
		t.Fatalf("bodies under each namespace's d1 differ: %x and %x, want one body naming d1", body, otherBody)
	}
	asksOther := chain.NewRegistry()
	asksOther.Register(idB, "seal", fetching{keyed: sealer.keyed, askIn: &otherNS}.factory())
	asksCurrent := chain.NewRegistry()
	asksCurrent.Register(idB, "seal", fetching{keyed: sealer.keyed, current: true}.factory())
	for _, tc := range []struct {
		name   string
		r      chain.Registry
		census []transform.Encoded
	}{
		{"no census", r, nil},
		{"a census of another key the store holds", r, census(d2)},
		{"a census of another kind", r, census(keys.MaterialID{Namespace: ns, Kind: keys.KindChunkIDKey, ID: "d1", Fingerprint: d1.Fingerprint})},
		{"another namespace's census, read in this one", r, otherEncs},
		{"a census of this namespace, read through a transform asking in another", asksOther, census(d1, o1)},
		{"its own census, read through a transform asking for the current key", asksCurrent, encs},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := mustBuild(t, tc.r, p, names("seal")...)
			before := p.Calls()
			_, err := c.Decode(ctx, ns, nil, hash, body, chunkMax, tc.census)
			if !errors.Is(err, transform.ErrMalformed) || errors.Is(err, keys.ErrMaterialUnavailable) {
				t.Errorf("Decode = %v, want ErrMalformed and not unavailable", err)
			}
			if calls := p.Calls() - before; calls != 0 {
				t.Errorf("the provider recorded %d calls, want none", calls)
			}
		})
	}
	t.Run("its own census", func(t *testing.T) {
		c := mustBuild(t, r, p, names("seal")...)
		if out, err := c.Decode(ctx, ns, nil, hash, body, chunkMax, encs); err != nil || string(out) != "chunk" {
			t.Errorf("Decode = %q, %v, want the chunk", out, err)
		}
	})
}

// printChecked is a fault wrapper over a provider: Get refuses, as
// ErrMaterialUnavailable, an ID whose fingerprint is not the one Give
// recorded, as a provider checking the fingerprint it is handed would.
type printChecked struct {
	*keystest.Memory
	given map[string][32]byte
}

func (p printChecked) Get(ctx context.Context, id keys.MaterialID) ([]byte, error) {
	if id.Fingerprint != p.given[id.ID] {
		return nil, fmt.Errorf("%s %q asked with another fingerprint: %w", id.Kind, id.ID, keys.ErrMaterialUnavailable)
	}
	return p.Memory.Get(ctx, id)
}

// TestDecodeHandsTheProviderTheCensusEntry decodes a body that names its key
// by ID alone through a provider refusing a wrong fingerprint: the view asks
// with the census's entry, whose fingerprint comes from block metadata.
func TestDecodeHandsTheProviderTheCensusEntry(t *testing.T) {
	prof.Watch(t, prof.Options{})
	m := keystest.NewMemory()
	d1 := m.Give(ns, keys.KindDataKey, "d1", span(0, 32))
	p := printChecked{m, map[string][32]byte{"d1": d1.Fingerprint}}
	r := censusRegistry()
	body, encs := sealed(t, r, p, ns)
	c := mustBuild(t, r, p, names("seal")...)
	if out, err := c.Decode(context.Background(), ns, nil, hash, body, chunkMax, encs); err != nil || string(out) != "chunk" {
		t.Errorf("Decode = %q, %v, want the chunk, the provider asked with the census's fingerprint", out, err)
	}
}

// failingGet is a fault wrapper over a provider whose Get fails with err.
type failingGet struct {
	keys.Provider
	err error
}

func (f failingGet) Get(context.Context, keys.MaterialID) ([]byte, error) { return nil, f.err }

// TestDecodeReportsMaterialErrors holds RFC 5 §8.2's T7 row's chain half and
// §2.7's material errors: a census-listed key the provider never had is
// unavailable and unknown, never malformed, and the same read decodes once the
// key is loaded; a destroyed key is destroyed only; a provider error that is
// no sentinel, even one wrapping ErrMalformed, is unavailable.
func TestDecodeReportsMaterialErrors(t *testing.T) {
	prof.Watch(t, prof.Options{})
	ctx := context.Background()
	r := censusRegistry()
	writer := keystest.NewMemory()
	writer.Give(ns, keys.KindDataKey, "d1", span(0, 32))
	body, encs := sealed(t, r, writer, ns)

	t.Run("unknown, then loaded", func(t *testing.T) {
		p := keystest.NewMemory()
		c := mustBuild(t, r, p, names("seal")...)
		_, err := c.Decode(ctx, ns, nil, hash, body, chunkMax, encs)
		if !errors.Is(err, keys.ErrMaterialUnavailable) || !errors.Is(err, keys.ErrUnknownMaterial) || errors.Is(err, transform.ErrMalformed) {
			t.Errorf("Decode of a key the provider never had = %v, want unavailable and unknown, not malformed", err)
		}
		p.Give(ns, keys.KindDataKey, "d1", span(0, 32))
		if out, err := c.Decode(ctx, ns, nil, hash, body, chunkMax, encs); err != nil || string(out) != "chunk" {
			t.Errorf("Decode once the key is loaded = %q, %v, want the chunk", out, err)
		}
	})
	t.Run("destroyed", func(t *testing.T) {
		p := keystest.NewMemory()
		p.Destroy(p.Give(ns, keys.KindDataKey, "d1", span(0, 32)))
		_, err := mustBuild(t, r, p, names("seal")...).Decode(ctx, ns, nil, hash, body, chunkMax, encs)
		if !errors.Is(err, keys.ErrMaterialDestroyed) || errors.Is(err, keys.ErrMaterialUnavailable) || errors.Is(err, transform.ErrMalformed) {
			t.Errorf("Decode of a destroyed key = %v, want destroyed only", err)
		}
	})
	for what, perr := range map[string]error{
		"no sentinel":           errors.New("the key service hung up"),
		"wrapping ErrMalformed": fmt.Errorf("the key service: %w", transform.ErrMalformed),
	} {
		t.Run("a provider error of "+what, func(t *testing.T) {
			p := failingGet{writer, perr}
			_, err := mustBuild(t, r, p, names("seal")...).Decode(ctx, ns, nil, hash, body, chunkMax, encs)
			if !errors.Is(err, keys.ErrMaterialUnavailable) || errors.Is(err, transform.ErrMalformed) || errors.Is(err, keys.ErrUnknownMaterial) {
				t.Errorf("Decode = %v, want unavailable only", err)
			}
		})
	}
}

// TestRequire holds RFC 5 §8.2's §2.5 row and T15's decode half: with
// require, a body without the required transform is ErrMalformed before any
// step asks for material; without it the same body decodes; in a namespace
// that encrypts, a body without an encrypt stage is ErrMalformed, and one
// under another encryption transform decodes.
func TestRequire(t *testing.T) {
	prof.Watch(t, prof.Options{})
	ctx := context.Background()
	r, p := censusRegistry(), keystest.NewMemory()
	p.Give(ns, keys.KindDataKey, "d1", span(0, 32))
	p.Give(otherNS, keys.KindDataKey, "d1", span(32, 64))
	encode := func(n entity.NamespaceID, cfg ...string) ([]byte, []transform.Encoded) {
		t.Helper()
		a, err := mustBuild(t, r, p, names(cfg...)...).Pin(ctx, n)
		if err != nil {
			t.Fatalf("Pin: %v", err)
		}
		body, encs, err := a.Encode(ctx, nil, hash, []byte("chunk"))
		if err != nil {
			t.Fatalf("Encode(%v): %v", cfg, err)
		}
		return body, encs
	}
	type body struct {
		ns   entity.NamespaceID
		cfg  []string
		body []byte
		encs []transform.Encoded
	}
	bodies := map[string]*body{
		"none":         {ns: ns},
		"parity":       {ns: ns, cfg: []string{"parity"}},
		"seal, parity": {ns: ns, cfg: []string{"seal", "parity"}},
		"seal":         {ns: ns, cfg: []string{"seal"}},
		"seal2":        {ns: ns, cfg: []string{"seal2"}},
		"other parity": {ns: otherNS, cfg: []string{"parity"}},
	}
	for _, b := range bodies {
		b.body, b.encs = encode(b.ns, b.cfg...)
	}
	encryptsNS := func(n entity.NamespaceID) bool { return n == ns }
	for _, tc := range []struct {
		name     string
		require  []string
		encrypts func(entity.NamespaceID) bool
		body     string
		refused  bool
	}{
		{"no require", nil, nil, "parity", false},
		{"require seal, a body without it", []string{"seal"}, nil, "parity", true},
		{"require seal, a body with it", []string{"seal"}, nil, "seal, parity", false},
		{"require seal, a body of no transform", []string{"seal"}, nil, "none", true},
		{"require seal and parity, a body with seal only", []string{"seal", "parity"}, nil, "seal", true},
		{"encrypting, a body without an encrypt stage", nil, encryptsNS, "parity", true},
		{"encrypting, a body of no transform", nil, encryptsNS, "none", true},
		{"encrypting, a body under seal", nil, encryptsNS, "seal, parity", false},
		{"encrypting, a body under another encryption transform", nil, encryptsNS, "seal2", false},
		{"another namespace that does not encrypt", nil, encryptsNS, "other parity", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := depsFor()
			d.Require = tc.require
			if tc.encrypts != nil {
				d.Encrypts = tc.encrypts
			}
			c, err := r.NewChain(ctx, names("plain"), p, d)
			if err != nil {
				t.Fatalf("NewChain: %v", err)
			}
			b := bodies[tc.body]
			before := p.Calls()
			out, err := c.Decode(ctx, b.ns, nil, hash, b.body, chunkMax, b.encs)
			switch {
			case tc.refused && !errors.Is(err, transform.ErrMalformed):
				t.Errorf("Decode of body %q = %q, %v, want ErrMalformed", tc.body, out, err)
			case tc.refused && p.Calls() != before:
				t.Errorf("Decode of body %q asked the provider %d times before its refusal, want none", tc.body, p.Calls()-before)
			case !tc.refused && (err != nil || string(out) != "chunk"):
				t.Errorf("Decode of body %q = %q, %v, want the chunk", tc.body, out, err)
			}
		})
	}
}
