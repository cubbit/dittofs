package keystest

import (
	"context"
	"errors"
	"slices"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/cubbit/dittofs/internal/prof"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/transform/keys"
)

// Memory passes the provider suite.
func TestMemoryPassesTheSuite(t *testing.T) {
	prof.Watch(t, prof.Options{})

	Run(t, func(*testing.T) Harness { return NewMemory() })
}

// noNamespace drops the namespace from every lookup, so material is found by
// kind and ID alone.
type noNamespace struct{ *Memory }

func (p noNamespace) Give(ns entity.NamespaceID, kind, id string, material []byte) keys.MaterialID {
	return p.Memory.Give(entity.NamespaceID{}, kind, id, material)
}

func (p noNamespace) Current(ctx context.Context, _ entity.NamespaceID, kind string) (keys.MaterialID, error) {
	return p.Memory.Current(ctx, entity.NamespaceID{}, kind)
}

func (p noNamespace) Get(ctx context.Context, id keys.MaterialID) ([]byte, error) {
	id.Namespace = entity.NamespaceID{}
	return p.Memory.Get(ctx, id)
}

func (p noNamespace) Replace(id keys.MaterialID, material []byte) {
	id.Namespace = entity.NamespaceID{}
	p.Memory.Replace(id, material)
}

func (p noNamespace) Destroy(id keys.MaterialID) {
	id.Namespace = entity.NamespaceID{}
	p.Memory.Destroy(id)
}

// firstStaysCurrent answers Current with the first material given of a kind
// in a namespace, whenever Memory's own Current succeeds.
type firstStaysCurrent struct {
	*Memory

	mu    sync.Mutex
	first map[current]keys.MaterialID
}

func (p *firstStaysCurrent) Give(ns entity.NamespaceID, kind, id string, material []byte) keys.MaterialID {
	got := p.Memory.Give(ns, kind, id, material)

	p.mu.Lock()
	defer p.mu.Unlock()

	if _, ok := p.first[current{ns, kind}]; !ok {
		p.first[current{ns, kind}] = got
	}

	return got
}

func (p *firstStaysCurrent) Current(ctx context.Context, ns entity.NamespaceID, kind string) (keys.MaterialID, error) {
	if _, err := p.Memory.Current(ctx, ns, kind); err != nil {
		return keys.MaterialID{}, err
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	return p.first[current{ns, kind}], nil
}

// refingerprints records the new content's fingerprint on Replace, so it
// serves replaced material.
type refingerprints struct{ *Memory }

func (p refingerprints) Replace(id keys.MaterialID, material []byte) {
	p.Give(id.Namespace, id.Kind, id.ID, material)
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

// Every row fails on the broken provider written for it and on no other, and
// passes on Memory, so a row failing for the wrong reason fails here too; and
// every row has a broken provider.
func TestEachBrokenProviderFailsItsRow(t *testing.T) {
	prof.Watch(t, prof.Options{})

	covered := make(map[string]bool)

	for _, c := range []struct {
		name string
		new  func() Harness
		want []string
	}{
		{"Memory", func() Harness { return NewMemory() }, nil},
		{"a lookup without the namespace", func() Harness { return noNamespace{NewMemory()} }, []string{"namespaces"}},
		{"the first material kept current", func() Harness {
			return &firstStaysCurrent{Memory: NewMemory(), first: make(map[current]keys.MaterialID)}
		}, []string{"current"}},
		{"replaced material served", func() Harness { return refingerprints{NewMemory()} }, []string{"replaced"}},
		{"destroyed reported as unavailable", func() Harness { return destroyedAsUnavailable{NewMemory()} }, []string{"destroyed"}},
		{"a kind never given reported as unavailable", func() Harness { return unknownKindUnavailable{NewMemory()} }, []string{"unknown"}},
		{"failing calls left uncounted", func() Harness { return &countsSuccesses{Memory: NewMemory()} }, []string{"calls"}},
	} {
		var failed []string

		for _, r := range rows {
			if err := r.check(t.Context(), c.new()); err != nil {
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
