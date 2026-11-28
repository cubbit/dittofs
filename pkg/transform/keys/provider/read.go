package provider

import (
	"bytes"
	"context"
	"fmt"
	"maps"
	"sync"

	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/transform/keys"
)

// The key record states the read path tells apart (RFC 5 Appendix B.2).
// retiring and retired records are served by Get like current ones.
const (
	stateCurrent   = "current"
	stateDestroyed = "destroyed"
)

var _ keys.Provider = (*Provider)(nil)

// slot names one key record within its namespace.
type slot struct{ kind, id string }

// entry is one key record as the provider caches it. row never changes once
// the entry is in a set; key, fp and failing are written under cache.mu.
type entry struct {
	row     Row
	key     []byte   // the unwrapped key, checked against fp; nil until first served
	fp      [32]byte // its recorded fingerprint, set with key
	failing bool     // its last serve failed, and the failure was logged
}

// cache holds each namespace's key records and the keys unwrapped from them.
// A set, once installed, is never changed: a reload or an update installs a
// new one.
//
// ponytail: never evicts, so every namespace's rows and every unwrapped key
// stay until stop, about 250 bytes per key: its row, record and key. Replace
// it with a bounded cache that re-reads on a miss when one node serves about
// 10⁶ namespaces.
type cache struct {
	mu     sync.RWMutex
	epochs map[entity.NamespaceID]uint64 // raised by each update of a namespace, so a load that began before it installs nothing
	sets   map[entity.NamespaceID]map[slot]*entry

	// updateMu orders the provider's own updates, so each one's refresh of
	// the cache lands in the order its commit did.
	//
	// ponytail: one lock for every namespace, so updates of different
	// namespaces wait on each other's transactions. Take one per namespace
	// when concurrent key creations or rotations contend on it.
	updateMu sync.Mutex
}

// Open loads ns's key records and unwraps and checks every current one,
// returning the first that cannot be served as Get would return it, so a
// namespace whose keys cannot be reached does not open. The content wiring
// calls it as it opens each namespace.
func (p *Provider) Open(ctx context.Context, ns entity.NamespaceID) error {
	set, err := p.load(ctx, ns)
	if err != nil {
		return err
	}
	for _, e := range set {
		if e.row.State != stateCurrent {
			continue
		}
		if _, _, err := p.serve(ns, set, e); err != nil {
			return err
		}
	}
	return nil
}

// Current is the current material of kind in ns, served only as Get would
// serve it. A kind ns holds no current record of is ErrMaterialDestroyed when
// it holds a destroyed one, since a current record is replaced only by a
// destroyed one or by a newer current one; otherwise it is ErrUnknownMaterial.
// Both are answered from the cache: a kind is given only through the
// provider's own update, which refreshes it.
func (p *Provider) Current(ctx context.Context, ns entity.NamespaceID, kind string) (keys.MaterialID, error) {
	p.calls.Add(1)

	set, err := p.set(ctx, ns)
	if err != nil {
		return keys.MaterialID{}, err
	}

	var cur *entry
	destroyed := false
	for s, e := range set {
		if s.kind != kind {
			continue
		}
		if e.row.State == stateDestroyed {
			destroyed = true
		}
		if e.row.State != stateCurrent {
			continue
		}
		if cur != nil {
			return keys.MaterialID{}, fmt.Errorf("provider: namespace %s holds two current %s records: %w", ns, kind, keys.ErrMaterialUnavailable)
		}
		cur = e
	}
	if cur == nil && destroyed {
		return keys.MaterialID{}, fmt.Errorf("provider: the current %s of namespace %s is destroyed: %w", kind, ns, keys.ErrMaterialDestroyed)
	}
	if cur == nil {
		return keys.MaterialID{}, fmt.Errorf("provider: namespace %s holds no current %s: %w", ns, kind, keys.ErrUnknownMaterial)
	}

	_, fp, err := p.serve(ns, set, cur)
	if err != nil {
		return keys.MaterialID{}, err
	}
	return keys.MaterialID{Namespace: ns, Kind: kind, ID: cur.row.ID, Fingerprint: fp}, nil
}

// Get returns a copy of the content of id, looked up by its namespace, kind
// and ID; id.Fingerprint is not consulted, since the record's own is checked.
// An ID the cached records lack reloads the namespace once before it is
// ErrUnknownMaterial, so a record another writer added is found.
func (p *Provider) Get(ctx context.Context, id keys.MaterialID) ([]byte, error) {
	p.calls.Add(1)
	s := slot{id.Kind, id.ID}

	p.cache.mu.RLock()
	set := p.cache.sets[id.Namespace]
	p.cache.mu.RUnlock()

	e := set[s]
	if e == nil {
		// One load either way: the first of a namespace never read, or the
		// reload of one whose cached rows lack the ID.
		var err error
		if set, err = p.load(ctx, id.Namespace); err != nil {
			p.status.refuse(matKey{id.Namespace, s}, false)
			return nil, err
		}
		if e = set[s]; e == nil {
			p.status.refuse(matKey{id.Namespace, s}, true)
			return nil, fmt.Errorf("provider: %s %x of namespace %s: %w", id.Kind, id.ID, id.Namespace, keys.ErrUnknownMaterial)
		}
	}

	key, _, err := p.serve(id.Namespace, set, e)
	if err != nil {
		return nil, err
	}
	return bytes.Clone(key), nil
}

// set returns ns's cached records, loading them on first use.
func (p *Provider) set(ctx context.Context, ns entity.NamespaceID) (map[slot]*entry, error) {
	p.cache.mu.RLock()
	set, ok := p.cache.sets[ns]
	p.cache.mu.RUnlock()
	if ok {
		return set, nil
	}
	return p.load(ctx, ns)
}

// load reads ns's records and installs them, keeping the key of every record
// whose state and bytes are unchanged. A failed read is
// ErrMaterialUnavailable and installs nothing. A read that began before an
// update of ns installs nothing either, since the update's set is newer: it
// answers its own call from that set, with the rows it read filling the slots
// the set lacks.
func (p *Provider) load(ctx context.Context, ns entity.NamespaceID) (map[slot]*entry, error) {
	p.cache.mu.RLock()
	epoch := p.cache.epochs[ns]
	p.cache.mu.RUnlock()

	rows, err := p.records.Load(ctx, ns)
	if err != nil {
		return nil, fmt.Errorf("provider: load the key records of namespace %s: %w: %w", ns, keys.ErrMaterialUnavailable, err)
	}

	p.cache.mu.Lock()
	defer p.cache.mu.Unlock()

	installed := p.cache.sets[ns]
	if p.cache.epochs[ns] != epoch {
		set := newSet(rows, nil)
		maps.Copy(set, installed)
		return set, nil
	}
	set := newSet(rows, installed)
	p.cache.sets[ns] = set
	return set, nil
}

// newSet builds a set of rows, a later row replacing an earlier one of its
// kind and ID, and carries over from old the key of every row whose state and
// bytes are unchanged. cache.mu must be held.
func newSet(rows []Row, old map[slot]*entry) map[slot]*entry {
	set := make(map[slot]*entry, len(rows))
	for _, row := range rows {
		s := slot{row.Kind, row.ID}
		e := &entry{row: row}
		if o := old[s]; o != nil && o.row.State == row.State && bytes.Equal(o.row.Record, row.Record) {
			e.key, e.fp, e.failing = o.key, o.fp, o.failing
		}
		set[s] = e
	}
	return set
}

// update runs fn on ns's key records through the view's Update and then makes
// the cached rows those fn last saw with the rows it returned written over
// them, as Update wrote them. After an error ns's rows are dropped, since the
// commit may have happened. Every write of a key record by the provider goes
// through update.
func (p *Provider) update(ctx context.Context, ns entity.NamespaceID, fn func(rows []Row) ([]Row, error)) error {
	p.cache.updateMu.Lock()
	defer p.cache.updateMu.Unlock()

	var seen, out []Row
	err := p.records.Update(ctx, ns, func(rows []Row) ([]Row, error) {
		o, err := fn(rows)
		seen, out = rows, o
		return o, err
	})

	p.cache.mu.Lock()
	defer p.cache.mu.Unlock()

	p.cache.epochs[ns]++
	if err != nil {
		delete(p.cache.sets, ns)
		return err
	}
	p.cache.sets[ns] = newSet(append(seen[:len(seen):len(seen)], out...), p.cache.sets[ns])
	return nil
}

// serve returns e's key, e one of set, the records of ns, and its recorded
// fingerprint once its state, its unwrap and its fingerprint have been
// checked, caching the key.
func (p *Provider) serve(ns entity.NamespaceID, set map[slot]*entry, e *entry) ([]byte, [32]byte, error) {
	if e.row.State == stateDestroyed {
		p.status.settled(matKey{ns, slot{e.row.Kind, e.row.ID}})
		return nil, [32]byte{}, fmt.Errorf("provider: %s %x of namespace %s: %w", e.row.Kind, e.row.ID, ns, keys.ErrMaterialDestroyed)
	}

	p.cache.mu.RLock()
	key, fp := e.key, e.fp
	p.cache.mu.RUnlock()
	if key != nil {
		p.status.settled(matKey{ns, slot{e.row.Kind, e.row.ID}})
		return key, fp, nil
	}

	key, fp, err := p.open(ns, set, e.row)
	if err != nil {
		// decision: a record that fails its codec, whose two master key IDs
		// differ, whose wrap names a master key this provider does not hold,
		// whose unwrap fails or whose key no longer matches its fingerprint is
		// unavailable, never destroyed: the right record or key may come back,
		// and only a destroyed state is loss for good (RFC 5 Appendix B.4).
		// Withdraw it if a record can ever be known lost from its bytes alone.
		err = fmt.Errorf("provider: %s %x of namespace %s: %w: %w", e.row.Kind, e.row.ID, ns, keys.ErrMaterialUnavailable, err)
		p.cache.mu.Lock()
		first := !e.failing
		e.failing = true
		p.cache.mu.Unlock()
		p.status.refuse(matKey{ns, slot{e.row.Kind, e.row.ID}}, false)
		if first {
			p.log.Error("key record cannot be served", "namespace", ns.String(), "kind", e.row.Kind, "id", fmt.Sprintf("%x", e.row.ID), "err", err)
		}
		return nil, [32]byte{}, err
	}

	p.cache.mu.Lock()
	e.key, e.fp, e.failing = key, fp, false
	p.cache.mu.Unlock()
	p.status.settled(matKey{ns, slot{e.row.Kind, e.row.ID}})
	return key, fp, nil
}

// open decodes row's record of ns, whose records are set, and returns its key,
// unwrapped when it is wrapped, and its recorded fingerprint, which the key
// must match.
func (p *Provider) open(ns entity.NamespaceID, set map[slot]*entry, row Row) ([]byte, [32]byte, error) {
	rec, err := decodeKeyRecord(row.Record)
	if err != nil {
		return nil, [32]byte{}, err
	}

	var key []byte
	if len(rec.masterKeyID) == 0 {
		// Only a non-encrypting namespace's chunk-ID key is held in the clear:
		// a clear record of another kind, or in a namespace holding a data or
		// chunking key, is a forged one, since nothing binds a clear key.
		if row.Kind != keys.KindChunkIDKey || encrypts(set) {
			return nil, [32]byte{}, fmt.Errorf("%w: a %s held in the clear", errKeyRecord, row.Kind)
		}
		key = bytes.Clone(rec.key)
	} else {
		s, err := parseStoredWrap(rec.key)
		if err != nil {
			return nil, [32]byte{}, err
		}
		if !bytes.Equal(s.masterKeyID, rec.masterKeyID) {
			return nil, [32]byte{}, fmt.Errorf("%w: record names master key %x, its wrap %x", errKeyRecord, rec.masterKeyID, s.masterKeyID)
		}
		if key, err = p.unwrap(rec.key, wrapAAD(ns[:], row.Kind, row.ID)); err != nil {
			return nil, [32]byte{}, err
		}
	}

	if fingerprint(key) != rec.fingerprint {
		clear(key)
		return nil, [32]byte{}, fmt.Errorf("key no longer matches its recorded fingerprint %x", rec.fingerprint)
	}
	return key, rec.fingerprint, nil
}

// encrypts reports whether set, a namespace's records, holds a data or
// chunking key, which only an encrypting namespace has.
func encrypts(set map[slot]*entry) bool {
	for s := range set {
		if s.kind == keys.KindDataKey || s.kind == keys.KindChunkingKey {
			return true
		}
	}
	return false
}
