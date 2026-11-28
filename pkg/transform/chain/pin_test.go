package chain_test

import (
	"context"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"testing"

	"github.com/cubbit/dittofs/internal/prof"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/transform"
	"github.com/cubbit/dittofs/pkg/transform/chain"
	"github.com/cubbit/dittofs/pkg/transform/keys"
	"github.com/cubbit/dittofs/pkg/transform/keystest"
)

// keyed is a test transform that uses material: its Encode writes the ID of
// each piece of material it was given, after a length byte, before the input,
// and its bounds count those bytes. Its ID, stage, version and kinds are
// parameters; its length key is its settings' level as one byte, and it
// ignores every other setting.
type keyed struct {
	id      transform.ID
	stage   transform.Stage
	version uint8
	kinds   []string
	level   []byte
}

func (k keyed) ID() transform.ID       { return k.id }
func (k keyed) Version() uint8         { return k.version }
func (k keyed) Stage() transform.Stage { return k.stage }
func (k keyed) Kinds() []string        { return k.kinds }
func (k keyed) LengthKey() []byte      { return k.level }

// extra is the bytes Encode adds under m; under nil, the most any material
// could add, an ID being at most 255 bytes.
func (k keyed) extra(m []keys.MaterialID) int {
	if m == nil {
		return len(k.kinds) * (1 + 255)
	}
	e := 0
	for _, id := range m {
		e += 1 + len(id.ID)
	}
	return e
}

func (k keyed) MaxEncodedLen(n int, m []keys.MaterialID) int    { return n + k.extra(m) }
func (k keyed) LengthOf(n int, m []keys.MaterialID) (int, bool) { return n + k.extra(m), true }

func (k keyed) Encode(_ context.Context, _ entity.NamespaceID, m []keys.MaterialID, dst []byte, _ [32]byte, plain []byte) (transform.Encoded, error) {
	body := dst[:0]
	for _, id := range m {
		body = append(append(body, byte(len(id.ID))), id.ID...)
	}
	return transform.Encoded{Body: append(body, plain...), Applied: true, Version: k.version, Material: m}, nil
}

// The decoding half lands with the chain's decode, and its tests.
func (k keyed) Decode(context.Context, entity.NamespaceID, []byte, [32]byte, []byte, int, keys.Provider) ([]byte, error) {
	return nil, errors.ErrUnsupported
}

// factory builds k, its length key the settings' level when they carry one.
func (k keyed) factory() transform.Factory {
	return func(_ context.Context, s map[string]any, _ keys.Provider) (transform.Transform, error) {
		if lv, ok := s["level"].(int); ok {
			k.level = []byte{byte(lv)}
		}
		return k, nil
	}
}

var ns = entity.NamespaceID{1}

// pin builds cfg over r with provider p and pins it for ns.
func pin(t *testing.T, r chain.Registry, p keys.Provider, cfg ...transform.Config) (transform.Attempt, error) {
	t.Helper()
	c, err := r.NewChain(context.Background(), cfg, p, chain.Deps{
		Logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		Recorder: sink{new([]string)},
		Encrypts: func(entity.NamespaceID) bool { return false },
	})
	if err != nil {
		t.Fatalf("NewChain: %v", err)
	}
	return c.Pin(context.Background(), ns)
}

func mustPin(t *testing.T, r chain.Registry, p keys.Provider, cfg ...transform.Config) transform.Attempt {
	t.Helper()
	a, err := pin(t, r, p, cfg...)
	if err != nil {
		t.Fatalf("Pin: %v", err)
	}
	return a
}

// ponytail: the chain-ID vectors and their wrong variants are pinned here,
// from testdata/chain-id-v1.py, an implementation of RFC 5 §2.8's byte table
// that shares no code with the chain. Read them from RFC 5's golden vector
// file through transformtest.LoadVectors once it carries the chain IDs, so one
// vector serves every implementation.
const (
	vectorEmpty               = "3ad00d4bd555a96956c44a56c210d8f6916f3e29f19fa691e0aad2eddcc21166"
	vectorEncrypt             = "bcfb4d470a88cdbe4bc2f0a1270a3f15f19e8329da1ad526435ca40656100797"
	vectorCompressThenEncrypt = "b1d65dfaedf4aa77595f80353d70c06b6dc9af7fa64fa04d2494579368315289"
)

// The compress-then-encrypt vector's chain ID with one of §8.2's §2.8 row's
// mistakes made.
var wrongCompressThenEncrypt = map[string]string{
	"materials in the order declared": "729ebcfca78d84102765735892eea05cefcf007c647edd16aff3e575ef5ad24a",
	"a fingerprint byte flipped":      "563983fbd39a876933a0e50fe96962c1b6b6f08ed9328cf4bcbbfaed7d304cfb",
	"2-byte length prefixes":          "d046fcef184a05ff47a371eae69df37a4c94a7c2789ed45ced4af98a01a86409",
	"little-endian length prefixes":   "728e57688a325174dd82ebeafd9881f7ceb06e15ed220be8580a3ec79c814598",
}

// span is bytes lo to hi-1, a vector's material.
func span(lo, hi byte) []byte {
	b := make([]byte, 0, hi-lo)
	for i := lo; i < hi; i++ {
		b = append(b, i)
	}
	return b
}

// TestChainIDVectors pins Pin's chain ID to RFC 5 §2.8's vectors, over test
// transforms declaring the vectors' IDs, versions, length keys and material:
// the encrypt transform's 0x0010 and zstd's 0x0001 at level 3. The second
// chain's encrypt entry declares its kinds in the order kind-then-ID order
// reverses, so an unsorted encoding, and a step order other than the
// configured one, give another ID.
func TestChainIDVectors(t *testing.T) {
	prof.Watch(t, prof.Options{})
	zstd := keyed{id: 0x0001, stage: transform.StageCompress, version: 1}
	encrypt := keyed{id: 0x0010, stage: transform.StageEncrypt, version: 1}
	for _, tc := range []struct {
		name  string
		kinds []string
		give  func(p *keystest.Memory)
		cfg   []transform.Config
		want  string
	}{
		{"the empty chain", nil, func(*keystest.Memory) {}, nil, vectorEmpty},
		{"the default encrypting chain", []string{"data-key"}, func(p *keystest.Memory) {
			p.Give(ns, "data-key", "d1", span(0x00, 0x20))
		}, []transform.Config{{Name: "encrypt"}}, vectorEncrypt},
		{"compress then encrypt, two materials", []string{"data-key", "chunk-id-key"}, func(p *keystest.Memory) {
			p.Give(ns, "data-key", "d2", span(0x20, 0x40))
			p.Give(ns, "chunk-id-key", "c1", span(0x40, 0x60))
		}, []transform.Config{{Name: "zstd", Settings: map[string]any{"level": 3, "with_encryption": "accept"}}, {Name: "encrypt"}}, vectorCompressThenEncrypt},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := chain.NewRegistry()
			r.Register(zstd.id, "zstd", zstd.factory())
			e := encrypt
			e.kinds = tc.kinds
			r.Register(e.id, "encrypt", e.factory())
			p := keystest.NewMemory()
			tc.give(p)
			id := mustPin(t, r, p, tc.cfg...).ID()
			if got := hex.EncodeToString(id[:]); got != tc.want {
				t.Errorf("chain ID = %s, want the vector %s", got, tc.want)
			}
		})
	}
	for mistake, id := range wrongCompressThenEncrypt {
		if id == vectorCompressThenEncrypt {
			t.Errorf("the vector is the chain ID with %s, so it cannot catch that mistake", mistake)
		}
	}
}

// TestChainIDChangesWithWhatDecidesABody holds RFC 5 §2.8's inputs: a
// transform's format version, a length-affecting setting and the current
// material each change the chain ID, and a setting outside the length key
// does not.
func TestChainIDChangesWithWhatDecidesABody(t *testing.T) {
	prof.Watch(t, prof.Options{})
	chainOf := func(version uint8) chain.Registry {
		r := chain.NewRegistry()
		r.Register(idA, "c", keyed{id: idA, stage: transform.StageCompress, version: version}.factory())
		r.Register(idB, "e", keyed{id: idB, stage: transform.StageEncrypt, version: 1, kinds: []string{"data-key"}}.factory())
		return r
	}
	cfg := func(s map[string]any) []transform.Config {
		s["with_encryption"] = "accept"
		return []transform.Config{{Name: "c", Settings: s}, {Name: "e"}}
	}
	p := keystest.NewMemory()
	p.Give(ns, "data-key", "d1", span(0, 32))
	idOf := func(r chain.Registry, s map[string]any) [32]byte { return mustPin(t, r, p, cfg(s)...).ID() }

	base := idOf(chainOf(1), map[string]any{"level": 3})
	for _, tc := range []struct {
		change string
		id     [32]byte
		same   bool
	}{
		{"a setting outside the length key", idOf(chainOf(1), map[string]any{"level": 3, "threads": 8}), true},
		{"the format version", idOf(chainOf(2), map[string]any{"level": 3}), false},
		{"a length-affecting setting", idOf(chainOf(1), map[string]any{"level": 4}), false},
		{"the current material", func() [32]byte {
			p.Give(ns, "data-key", "d2", span(32, 64))
			return idOf(chainOf(1), map[string]any{"level": 3})
		}(), false},
	} {
		if same := tc.id == base; same != tc.same {
			t.Errorf("after changing %s, chain ID unchanged = %v, want %v", tc.change, same, tc.same)
		}
	}
}

// TestAttemptKeepsItsPin pins a chain whose two transforms both declare
// data-key, then rotates the data key and changes the settings map the chain
// was built from: the attempt's ID and bounds hold, Pin asked for each kind
// once, and nothing after Pin asks the provider again. A fresh pin sees the
// rotation.
func TestAttemptKeepsItsPin(t *testing.T) {
	prof.Watch(t, prof.Options{})
	r := chain.NewRegistry()
	r.Register(idA, "c", keyed{id: idA, stage: transform.StageCompress, version: 1, kinds: []string{"data-key"}}.factory())
	r.Register(idB, "e", keyed{id: idB, stage: transform.StageEncrypt, version: 1, kinds: []string{"data-key", "chunk-id-key"}}.factory())
	p := keystest.NewMemory()
	p.Give(ns, "data-key", "d1", span(0, 32))
	p.Give(ns, "chunk-id-key", "c1", span(32, 64))
	settings := map[string]any{"level": 3, "with_encryption": "accept"}
	cfg := []transform.Config{{Name: "c", Settings: settings}, {Name: "e"}}

	before := p.Calls()
	a := mustPin(t, r, p, cfg...)
	if got := p.Calls() - before; got != 2 {
		t.Errorf("Pin made %d provider calls, want 2, one Current per kind", got)
	}
	id := a.ID()
	// 100 bytes, then 1+2 for d1, then 1+2 for d1 and 1+2 for c1, then an
	// envelope of two IDs.
	const wantMax = 100 + 3 + 3 + 3 + 2 + 2*2

	p.Give(ns, "data-key", "data-key-2", span(64, 96))
	settings["level"] = 9
	after := p.Calls()
	if got := a.ID(); got != id {
		t.Errorf("attempt ID changed after a rotation and a settings change: %x, want %x", got, id)
	}
	if got := a.MaxEncodedLen(100); got != wantMax {
		t.Errorf("MaxEncodedLen(100) = %d, want %d, under the pinned material", got, wantMax)
	}
	if got, ok := a.LengthOf(100); got != wantMax || !ok {
		t.Errorf("LengthOf(100) = %d, %v, want %d, true, under the pinned material", got, ok, wantMax)
	}
	if got := p.Calls() - after; got != 0 {
		t.Errorf("the attempt made %d provider calls after Pin, want 0", got)
	}
	if fresh := mustPin(t, r, p, cfg...); fresh.ID() == id || fresh.MaxEncodedLen(100) == wantMax {
		t.Errorf("a pin after the rotation kept the old chain ID or bound, so this test cannot tell a re-read from a pin")
	}
}

// TestPinFailsWithTheProvidersError holds that a provider failure in Current
// fails Pin with that error, unchanged, and no attempt.
func TestPinFailsWithTheProvidersError(t *testing.T) {
	prof.Watch(t, prof.Options{})
	r := chain.NewRegistry()
	r.Register(idB, "e", keyed{id: idB, stage: transform.StageEncrypt, version: 1, kinds: []string{"data-key"}}.factory())
	for _, tc := range []struct {
		name string
		give func(p *keystest.Memory)
		want error
	}{
		{"an empty provider", func(*keystest.Memory) {}, keys.ErrUnknownMaterial},
		{"a destroyed key", func(p *keystest.Memory) { p.Destroy(p.Give(ns, "data-key", "d1", span(0, 32))) }, keys.ErrMaterialDestroyed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := keystest.NewMemory()
			tc.give(p)
			_, want := p.Current(context.Background(), ns, "data-key")
			a, err := pin(t, r, p, transform.Config{Name: "e"})
			if a != nil {
				t.Errorf("Pin returned an attempt beside its error")
			}
			if !errors.Is(err, tc.want) || err == nil || err.Error() != want.Error() {
				t.Errorf("Pin error = %v, want the provider's %v", err, want)
			}
		})
	}
}
