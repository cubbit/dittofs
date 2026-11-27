package keystest

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/cubbit/dittofs/internal/prof"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/transform/keys"
)

// Memory passes the provider suite, and Run runs every row, each on a fresh
// harness.
func TestMemoryPassesTheSuite(t *testing.T) {
	prof.Watch(t, prof.Options{})

	var built []string

	Run(t, func(t *testing.T) Harness {
		built = append(built, t.Name())
		return NewMemory()
	})

	var want []string
	for _, r := range rows {
		want = append(want, t.Name()+"/"+r.name)
	}

	if !slices.Equal(built, want) {
		t.Errorf("Run built harnesses for %q, want one for each of %q", built, want)
	}
}

// Memory's calls are safe from many goroutines at once, as the conformance
// suites that share one Memory call them; the race detector is the assertion.
func TestMemoryConcurrentCalls(t *testing.T) {
	prof.Watch(t, prof.Options{})

	m := NewMemory()

	var wg sync.WaitGroup
	for n := range 16 {
		wg.Go(func() {
			ns := namespace(byte(n))
			id := m.Give(ns, keys.KindDataKey, "k", materialA)

			if got, err := m.Current(t.Context(), ns, keys.KindDataKey); err != nil || got != id {
				t.Errorf("Current(namespace %s) = %+v, %v, want %+v", ns, got, err, id)
			}

			if err := served(t.Context(), m, id, materialA); err != nil {
				t.Error(err)
			}

			m.Replace(id, materialA)
			m.Destroy(id)
		})
	}
	wg.Wait()

	if got := m.Calls(); got != 32 {
		t.Errorf("Calls = %d after 16 Currents and 16 Gets, want 32", got)
	}
}

// Memory's salvage wrap opens under its master key and the associated data
// naming its export, and under nothing else: another export, another Memory's
// master key, or any byte of the sealed wrap flipped. Catches a wrap that
// binds nothing.
func TestMemorysSalvageWrapUnwrapsUnderItsAADOnly(t *testing.T) {
	prof.Watch(t, prof.Options{})

	m := NewMemory()
	export := []byte("salvage export")

	key, wrap, err := m.WrapSalvageKey(t.Context(), export)
	if err != nil {
		t.Fatalf("WrapSalvageKey: %v", err)
	}

	kind, masterID, sealed, err := parseStoredWrap(wrap)
	if err != nil {
		t.Fatalf("wrap %x: %v", wrap, err)
	}

	if kind != 0x01 || !bytes.Equal(masterID, m.masterID) {
		t.Errorf("wrap names provider kind %#02x, master key %x; want 0x01, %x", kind, masterID, m.masterID)
	}

	open := func(m *Memory, sealed, export []byte) ([]byte, error) {
		n := m.master.NonceSize()
		return m.master.Open(nil, sealed[:n], sealed[n:], salvageAAD(export))
	}

	if got, err := open(m, sealed, export); err != nil || !bytes.Equal(got, key) {
		t.Errorf("unwrap under its master key and export = %x, %v; want %x", got, err, key)
	}

	if _, err := open(m, sealed, []byte("another export")); err == nil {
		t.Error("unwrap under another export's associated data succeeded, want it refused")
	}

	if _, err := open(NewMemory(), sealed, export); err == nil {
		t.Error("unwrap under another Memory's master key succeeded, want it refused")
	}

	for i := range sealed {
		flipped := slices.Clone(sealed)
		flipped[i] ^= 0x01

		if _, err := open(m, flipped, export); err == nil {
			t.Errorf("unwrap with byte %d of the sealed wrap flipped succeeded, want it refused", i)
		}
	}
}

// remembers keeps every ID given, for the broken providers that look past
// what they were asked for.
type remembers struct {
	*Memory

	given []keys.MaterialID
}

func (p *remembers) Give(ns entity.NamespaceID, kind, id string, material []byte) keys.MaterialID {
	got := p.Memory.Give(ns, kind, id, material)
	p.given = append(p.given, got)

	return got
}

// noNamespace drops the namespace from every lookup, so material is found by
// kind and ID alone; the IDs it returns still name the caller's namespace.
type noNamespace struct{ *Memory }

func (p noNamespace) Give(ns entity.NamespaceID, kind, id string, material []byte) keys.MaterialID {
	got := p.Memory.Give(entity.NamespaceID{}, kind, id, material)
	got.Namespace = ns

	return got
}

func (p noNamespace) Current(ctx context.Context, ns entity.NamespaceID, kind string) (keys.MaterialID, error) {
	got, err := p.Memory.Current(ctx, entity.NamespaceID{}, kind)
	if err == nil {
		got.Namespace = ns
	}

	return got, err
}

func zeroNamespace(id keys.MaterialID) keys.MaterialID {
	id.Namespace = entity.NamespaceID{}
	return id
}

func (p noNamespace) Get(ctx context.Context, id keys.MaterialID) ([]byte, error) {
	return p.Memory.Get(ctx, zeroNamespace(id))
}

func (p noNamespace) Replace(id keys.MaterialID, material []byte) {
	p.Memory.Replace(zeroNamespace(id), material)
}

func (p noNamespace) Destroy(id keys.MaterialID) { p.Memory.Destroy(zeroNamespace(id)) }

// getAnyNamespace answers a Get of an ID its namespace was never given with
// the same kind and ID of another namespace.
type getAnyNamespace struct{ *remembers }

func (p getAnyNamespace) Get(ctx context.Context, id keys.MaterialID) ([]byte, error) {
	b, err := p.Memory.Get(ctx, id)
	for _, g := range p.given {
		if errors.Is(err, keys.ErrUnknownMaterial) && g.Kind == id.Kind && g.ID == id.ID {
			return p.Memory.Get(ctx, g)
		}
	}

	return b, err
}

// currentAnyNamespace answers Current of a kind never given in a namespace
// with another namespace's current material of that kind.
type currentAnyNamespace struct{ *remembers }

func (p currentAnyNamespace) Current(ctx context.Context, ns entity.NamespaceID, kind string) (keys.MaterialID, error) {
	got, err := p.Memory.Current(ctx, ns, kind)
	for _, g := range p.given {
		if errors.Is(err, keys.ErrUnknownMaterial) && g.Kind == kind {
			return p.Memory.Current(ctx, g.Namespace, kind)
		}
	}

	return got, err
}

// kindBlind answers a Get of an ID never given as its kind with the same ID
// given as another kind.
type kindBlind struct{ *remembers }

func (p kindBlind) Get(ctx context.Context, id keys.MaterialID) ([]byte, error) {
	b, err := p.Memory.Get(ctx, id)
	for _, g := range p.given {
		if errors.Is(err, keys.ErrUnknownMaterial) && g.Namespace == id.Namespace && g.ID == id.ID {
			return p.Memory.Get(ctx, g)
		}
	}

	return b, err
}

// firstStaysCurrent answers Current with the first material given of a kind
// in a namespace, whenever Memory's own Current succeeds.
type firstStaysCurrent struct{ *remembers }

func (p firstStaysCurrent) Current(ctx context.Context, ns entity.NamespaceID, kind string) (keys.MaterialID, error) {
	if _, err := p.Memory.Current(ctx, ns, kind); err != nil {
		return keys.MaterialID{}, err
	}

	i := slices.IndexFunc(p.given, func(g keys.MaterialID) bool { return g.Namespace == ns && g.Kind == kind })

	return p.given[i], nil
}

// latestOnly loses the material a Give of the same kind and namespace
// replaces as current.
type latestOnly struct{ *remembers }

func (p latestOnly) Give(ns entity.NamespaceID, kind, id string, material []byte) keys.MaterialID {
	for _, g := range p.given {
		if g.Namespace == ns && g.Kind == kind {
			p.Destroy(g)
		}
	}

	return p.remembers.Give(ns, kind, id, material)
}

// refingerprints records the new content's fingerprint on Replace, so it
// serves replaced material.
type refingerprints struct{ *Memory }

func (p refingerprints) Replace(id keys.MaterialID, material []byte) {
	p.Give(id.Namespace, id.Kind, id.ID, material)
}

// callerFingerprint serves replaced material when the caller names the new
// content's fingerprint.
type callerFingerprint struct {
	*Memory

	content map[keys.MaterialID][]byte
}

func (p callerFingerprint) Replace(id keys.MaterialID, material []byte) {
	p.Memory.Replace(id, material)
	p.content[zeroFingerprint(id)] = material
}

func zeroFingerprint(id keys.MaterialID) keys.MaterialID {
	id.Fingerprint = [32]byte{}
	return id
}

func (p callerFingerprint) Get(ctx context.Context, id keys.MaterialID) ([]byte, error) {
	b, err := p.Memory.Get(ctx, id)
	if c, ok := p.content[zeroFingerprint(id)]; ok && errors.Is(err, keys.ErrMaterialUnavailable) && fingerprint(c) == id.Fingerprint {
		return c, nil
	}

	return b, err
}

// aliased returns the slice it holds from Get.
type aliased struct{ *Memory }

func (p aliased) Get(ctx context.Context, id keys.MaterialID) ([]byte, error) {
	if _, err := p.Memory.Get(ctx, id); err != nil {
		return nil, err
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	return p.entries[slot{id.Namespace, id.Kind, id.ID}].content, nil
}

// wipes zeroes on Destroy every slice its Get handed out for that ID.
type wipes struct {
	*Memory

	handed map[keys.MaterialID][][]byte
}

func (p wipes) Get(ctx context.Context, id keys.MaterialID) ([]byte, error) {
	b, err := p.Memory.Get(ctx, id)
	if err == nil {
		p.handed[zeroFingerprint(id)] = append(p.handed[zeroFingerprint(id)], b)
	}

	return b, err
}

func (p wipes) Destroy(id keys.MaterialID) {
	p.Memory.Destroy(id)

	for _, b := range p.handed[zeroFingerprint(id)] {
		clear(b)
	}
}

// destroyedAsUnavailable reports destroyed material as unavailable.
type destroyedAsUnavailable struct{ *Memory }

func asUnavailable(err error) error {
	if errors.Is(err, keys.ErrMaterialDestroyed) {
		return keys.ErrMaterialUnavailable
	}

	return err
}

func (p destroyedAsUnavailable) Current(ctx context.Context, ns entity.NamespaceID, kind string) (keys.MaterialID, error) {
	id, err := p.Memory.Current(ctx, ns, kind)
	return id, asUnavailable(err)
}

func (p destroyedAsUnavailable) Get(ctx context.Context, id keys.MaterialID) ([]byte, error) {
	b, err := p.Memory.Get(ctx, id)
	return b, asUnavailable(err)
}

// unknownKindUnavailable answers Current of a kind never given as unavailable.
type unknownKindUnavailable struct{ *Memory }

func (p unknownKindUnavailable) Current(ctx context.Context, ns entity.NamespaceID, kind string) (keys.MaterialID, error) {
	id, err := p.Memory.Current(ctx, ns, kind)
	if errors.Is(err, keys.ErrUnknownMaterial) {
		return id, keys.ErrMaterialUnavailable
	}

	return id, err
}

// countsSuccesses counts only the calls that succeed.
type countsSuccesses struct {
	*Memory

	ok atomic.Uint64
}

func (p *countsSuccesses) count(err error) {
	if err == nil {
		p.ok.Add(1)
	}
}

func (p *countsSuccesses) Current(ctx context.Context, ns entity.NamespaceID, kind string) (keys.MaterialID, error) {
	id, err := p.Memory.Current(ctx, ns, kind)
	p.count(err)

	return id, err
}

func (p *countsSuccesses) Get(ctx context.Context, id keys.MaterialID) ([]byte, error) {
	b, err := p.Memory.Get(ctx, id)
	p.count(err)

	return b, err
}

func (p *countsSuccesses) Calls() uint64 { return p.ok.Load() }

// countsGives counts each Give as a call.
type countsGives struct {
	*Memory

	gives uint64
}

func (p *countsGives) Give(ns entity.NamespaceID, kind, id string, material []byte) keys.MaterialID {
	p.gives++
	return p.Memory.Give(ns, kind, id, material)
}

func (p *countsGives) Calls() uint64 { return p.Memory.Calls() + p.gives }

// salvageBroken answers WrapSalvageKey through wrap, over Memory.
type salvageBroken struct {
	*remembers

	wrap func(p *remembers, export []byte) (key, wrap []byte)
}

func (p salvageBroken) WrapSalvageKey(_ context.Context, export []byte) (key, wrap []byte, err error) {
	key, wrap = p.wrap(p.remembers, export)
	return key, wrap, nil
}

// The broken salvage-key wraps, by what they get wrong.
var (
	// fixedKey wraps one fixed key on every call.
	fixedKey = func(p *remembers, export []byte) ([]byte, []byte) {
		key := bytes.Repeat([]byte{0x5a}, 32)
		return key, p.wrapSalvage(key, export)
	}
	// fixedWrap draws a fresh key and returns the first wrap it made.
	fixedWrap = func() func(p *remembers, export []byte) ([]byte, []byte) {
		var first []byte
		return func(p *remembers, export []byte) ([]byte, []byte) {
			key := random(32)
			if first == nil {
				first = p.wrapSalvage(key, export)
			}
			return key, first
		}
	}
	// inClear returns the key as its own wrap bytes, in the layout. The key
	// is 0xfb bytes, whose base64 holds the characters the two alphabets
	// differ in.
	inClear = func(encode func([]byte) []byte) func(p *remembers, export []byte) ([]byte, []byte) {
		return func(p *remembers, _ []byte) ([]byte, []byte) {
			key := bytes.Repeat([]byte{0xfb}, 32)
			return key, storedWrap(0x01, p.masterID, encode(key))
		}
	}
	// mangled changes Memory's wrap after it is laid out.
	mangled = func(change func([]byte) []byte) func(p *remembers, export []byte) ([]byte, []byte) {
		return func(p *remembers, export []byte) ([]byte, []byte) {
			key, wrap, _ := p.WrapSalvageKey(context.Background(), export)
			return key, change(wrap)
		}
	}
	// asMaterial gives the key as current material of kind in the namespace
	// the row gave last, or the zero namespace.
	asMaterial = func(kind string, zero bool) func(p *remembers, export []byte) ([]byte, []byte) {
		return func(p *remembers, export []byte) ([]byte, []byte) {
			key, wrap, _ := p.WrapSalvageKey(context.Background(), export)

			ns := entity.NamespaceID{}
			if !zero {
				ns = p.given[len(p.given)-1].Namespace
			}

			p.Give(ns, kind, hex.EncodeToString(key[:4]), key)

			return key, wrap
		}
	}
)

// salvageCases are the broken salvage-key wraps, each with the text of the
// check it must fail on.
var salvageCases = []struct {
	name string
	wrap func(p *remembers, export []byte) ([]byte, []byte)
	want string
}{
	{"one fixed key", fixedKey, "calls returned one key"},
	{"one fixed wrap", fixedWrap(), "calls returned one wrap"},
	{"a short key", func(p *remembers, export []byte) ([]byte, []byte) {
		key := random(16)
		return key, p.wrapSalvage(key, export)
	}, "returned a 16-byte key, want 32"},
	{"the key in the clear", inClear(func(k []byte) []byte { return k }), "fbfb raw"},
	{"the key in hex", inClear(func(k []byte) []byte { return []byte(hex.EncodeToString(k)) }), "in hex"},
	{"the key in upper-case hex", inClear(func(k []byte) []byte { return []byte(strings.ToUpper(hex.EncodeToString(k))) }), "in upper-case hex"},
	{"the key in base64", inClear(func(k []byte) []byte { return []byte(base64.StdEncoding.EncodeToString(k)) }), "in base64"},
	{"the key in URL base64", inClear(func(k []byte) []byte { return []byte(base64.URLEncoding.EncodeToString(k)) }), "in URL base64"},
	{"a byte past the wrap", mangled(func(w []byte) []byte { return append(w, 0) }), "wrap length 60, but 61 bytes follow"},
	{"a byte short of the wrap", mangled(func(w []byte) []byte { return w[:len(w)-1] }), "wrap length 60, but 59 bytes follow"},
	{"an unknown provider kind", mangled(func(w []byte) []byte { w[0] = 0x03; return w }), "provider kind 0x03"},
	{"an empty master-key ID", func(p *remembers, export []byte) ([]byte, []byte) {
		key := random(32)
		return key, storedWrap(0x01, nil, random(28))
	}, "empty master-key ID"},
	{"empty wrap bytes", func(p *remembers, _ []byte) ([]byte, []byte) {
		return random(32), storedWrap(0x01, p.masterID, nil)
	}, "empty wrap bytes"},
	{"a truncated layout", mangled(func(w []byte) []byte { return w[:2] }), "shorter than a provider kind"},
	{"a master-key ID past the end", mangled(func(w []byte) []byte { return w[:10] }), "master-key ID length 16"},
	{"the key as the namespace's material", asMaterial(keys.KindChunkingKey, false), "chunking-key) ="},
	{"the key as the zero namespace's material", asMaterial(keys.KindDataKey, true), "namespace 00000000-0000-0000-0000-000000000000, data-key) ="},
}

// Every row fails on the broken providers written for it and on no other,
// and passes on Memory, so a row failing for the wrong reason fails here too;
// and every row has a broken provider.
func TestEachBrokenProviderFailsItsRow(t *testing.T) {
	prof.Watch(t, prof.Options{})

	mem := func() *remembers { return &remembers{Memory: NewMemory()} }
	covered := make(map[string]bool)

	cases := []struct {
		name  string
		build func() Harness
		want  []string
	}{
		{"Memory", func() Harness { return NewMemory() }, nil},
		{"a lookup without the namespace", func() Harness { return noNamespace{NewMemory()} }, []string{"namespaces"}},
		{"a Get falling back to another namespace", func() Harness { return getAnyNamespace{mem()} }, []string{"namespaces"}},
		{"a Current falling back to another namespace", func() Harness { return currentAnyNamespace{mem()} }, []string{"namespaces"}},
		{"a Get ignoring the kind", func() Harness { return kindBlind{mem()} }, []string{"kinds"}},
		{"the first material kept current", func() Harness { return firstStaysCurrent{mem()} }, []string{"current"}},
		{"only the latest material kept", func() Harness { return latestOnly{mem()} }, []string{"current"}},
		{"replaced material served", func() Harness { return refingerprints{NewMemory()} }, []string{"replaced"}},
		{"replaced material served under the caller's fingerprint", func() Harness {
			return callerFingerprint{NewMemory(), make(map[keys.MaterialID][]byte)}
		}, []string{"replaced"}},
		{"destroyed reported as unavailable", func() Harness { return destroyedAsUnavailable{NewMemory()} }, []string{"destroyed"}},
		{"the held slice returned", func() Harness { return aliased{NewMemory()} }, []string{"owned"}},
		{"handed-out slices wiped on Destroy", func() Harness {
			return wipes{NewMemory(), make(map[keys.MaterialID][][]byte)}
		}, []string{"owned"}},
		{"a kind never given reported as unavailable", func() Harness { return unknownKindUnavailable{NewMemory()} }, []string{"unknown"}},
		{"failing calls left uncounted", func() Harness { return &countsSuccesses{Memory: NewMemory()} }, []string{"calls"}},
		{"Give counted as a call", func() Harness { return &countsGives{Memory: NewMemory()} }, []string{"calls"}},
	}
	for _, sc := range salvageCases {
		cases = append(cases, struct {
			name  string
			build func() Harness
			want  []string
		}{"a salvage wrap with " + sc.name, func() Harness { return salvageBroken{mem(), sc.wrap} }, []string{"salvage"}})
	}

	for _, c := range cases {
		var failed []string

		for _, r := range rows {
			if err := r.check(t.Context(), c.build()); err != nil {
				failed = append(failed, r.name)
				t.Logf("%s, row %s: %v", c.name, r.name, err)
			}
		}

		if !slices.Equal(failed, c.want) {
			t.Errorf("%s: rows failed %q, want %q", c.name, failed, c.want)
		}

		for _, name := range c.want {
			covered[name] = true
		}
	}

	for _, r := range rows {
		if !covered[r.name] {
			t.Errorf("row %s has no broken provider that must fail it", r.name)
		}
	}
}

// The rows a broken provider fails are logged above; this pins that each
// fails where it was written to, not on an earlier check of its row.
func TestEachBrokenProviderFailsOnItsCheck(t *testing.T) {
	mem := func() *remembers { return &remembers{Memory: NewMemory()} }

	cases := []struct {
		name  string
		build func() Harness
		row   func(context.Context, Harness) error
		want  string
	}{
		{"a lookup without the namespace", func() Harness { return noNamespace{NewMemory()} }, namespaces, "Current(namespace 00000000-0000-0000-0000-000000000001, data-key) ="},
		{"a Get falling back to another namespace", func() Harness { return getAnyNamespace{mem()} }, namespaces, `Get of namespace`},
		{"a Current falling back to another namespace", func() Harness { return currentAnyNamespace{mem()} }, namespaces, "given only in namespace"},
		{"only the latest material kept", func() Harness { return latestOnly{mem()} }, currentIsLatest, `Get(data-key "first"`},
		{"replaced material served under the caller's fingerprint", func() Harness {
			return callerFingerprint{NewMemory(), make(map[keys.MaterialID][]byte)}
		}, replaced, "naming its new content's fingerprint"},
		{"Give counted as a call", func() Harness { return &countsGives{Memory: NewMemory()} }, calls, "over a Give, a Replace and a Destroy"},
		{"the held slice returned", func() Harness { return aliased{NewMemory()} }, owned, "after the caller changed the slice Get returned"},
		{"handed-out slices wiped on Destroy", func() Harness {
			return wipes{NewMemory(), make(map[keys.MaterialID][][]byte)}
		}, owned, "after a Replace and a Destroy"},
	}
	for _, sc := range salvageCases {
		cases = append(cases, struct {
			name  string
			build func() Harness
			row   func(context.Context, Harness) error
			want  string
		}{"a salvage wrap with " + sc.name, func() Harness { return salvageBroken{mem(), sc.wrap} }, salvage, sc.want})
	}

	for _, c := range cases {
		err := c.row(t.Context(), c.build())
		if got := fmt.Sprint(err); err == nil || !strings.Contains(got, c.want) {
			t.Errorf("%s: row error %q, want one containing %q", c.name, got, c.want)
		}
	}
}
