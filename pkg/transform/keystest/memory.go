// Package keystest holds every keys.Provider to RFC 5 §2.5, §2.7 and §3.1: Memory, an
// in-memory provider for the tests of the chain, the stages and the engine,
// and Run, the provider suite Memory and every real provider pass.
package keystest

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"fmt"
	"slices"
	"sync"
	"sync/atomic"

	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/transform/keys"
)

// slot names one piece of material: the namespace it was given in, its kind
// and its ID.
type slot struct {
	ns   entity.NamespaceID
	kind string
	id   string
}

// current names the material new bodies of one kind use in one namespace.
type current struct {
	ns   entity.NamespaceID
	kind string
}

// entry is one piece of material as Memory holds it.
type entry struct {
	content     []byte
	fingerprint [32]byte // recorded by Give, kept by Replace
	destroyed   bool
}

// Memory is a keys.Provider that holds its material in memory. A test sets it
// up through Give, Replace and Destroy, calls a real provider can honour too,
// and reads Calls to assert the provider was or was not asked. Its methods are
// safe for concurrent use.
type Memory struct {
	calls atomic.Uint64

	mu        sync.Mutex
	entries   map[slot]*entry
	current   map[current]slot
	destroyed []keys.MaterialID // in the order Destroy marked them, for DestroyedSince
}

var (
	_ keys.Provider        = (*Memory)(nil)
	_ keys.DestroyedNotice = (*Memory)(nil)
)

// NewMemory returns a Memory holding no material.
func NewMemory() *Memory {
	return &Memory{entries: make(map[slot]*entry), current: make(map[current]slot)}
}

// fingerprint is HMAC-SHA256(material, "dittofs key check v1") (RFC 5 §2.5).
func fingerprint(material []byte) [32]byte {
	mac := hmac.New(sha256.New, material)
	mac.Write([]byte("dittofs key check v1"))

	return [32]byte(mac.Sum(nil))
}

// Current returns the material Give last made current for kind in ns, with
// the errors keys.Provider names: ErrMaterialUnavailable once its content no
// longer matches its fingerprint, ErrMaterialDestroyed once it is destroyed.
//
// decision: Current of a kind never given in ns is ErrUnknownMaterial, as
// keys.Provider states, the error Get gives for the same absence, rather than
// ErrMaterialUnavailable, which names an outage. RFC 5 §3.1 is silent on it,
// and the provider suite holds every provider to the same answer. Withdraw
// this if RFC 5 names another error.
func (m *Memory) Current(_ context.Context, ns entity.NamespaceID, kind string) (keys.MaterialID, error) {
	m.calls.Add(1)

	m.mu.Lock()
	defer m.mu.Unlock()

	s, ok := m.current[current{ns, kind}]
	if !ok {
		return keys.MaterialID{}, fmt.Errorf("current %s of namespace %s: %w", kind, ns, keys.ErrUnknownMaterial)
	}

	id := keys.MaterialID{Namespace: s.ns, Kind: s.kind, ID: s.id, Fingerprint: m.entries[s].fingerprint}
	if _, err := m.read(id); err != nil {
		return keys.MaterialID{}, err
	}

	return id, nil
}

// Get returns a copy of the content of id, looked up by its namespace, kind
// and ID. The content is compared with the fingerprint Give recorded, never
// with id.Fingerprint, since a body names its key by ID.
func (m *Memory) Get(_ context.Context, id keys.MaterialID) ([]byte, error) {
	m.calls.Add(1)

	m.mu.Lock()
	defer m.mu.Unlock()

	return m.read(id)
}

// read returns a copy of id's content, or the error that stops it being
// served. m.mu must be held.
func (m *Memory) read(id keys.MaterialID) ([]byte, error) {
	e, ok := m.entries[slot{id.Namespace, id.Kind, id.ID}]

	switch {
	case !ok:
		return nil, fmt.Errorf("%s %q of namespace %s: %w", id.Kind, id.ID, id.Namespace, keys.ErrUnknownMaterial)
	case e.destroyed:
		return nil, fmt.Errorf("%s %q of namespace %s: %w", id.Kind, id.ID, id.Namespace, keys.ErrMaterialDestroyed)
	case fingerprint(e.content) != e.fingerprint:
		return nil, fmt.Errorf("%s %q of namespace %s no longer matches its fingerprint: %w", id.Kind, id.ID, id.Namespace, keys.ErrMaterialUnavailable)
	}

	return slices.Clone(e.content), nil
}

// Give stores a copy of material as id of kind in ns, records its fingerprint
// and makes it current for kind in ns. Giving an ID again replaces it whole,
// fingerprint and destruction included. The ID is the caller's, since a
// fixture's body names one.
func (m *Memory) Give(ns entity.NamespaceID, kind, id string, material []byte) keys.MaterialID {
	s := slot{ns, kind, id}
	e := &entry{content: slices.Clone(material), fingerprint: fingerprint(material)}

	m.mu.Lock()
	defer m.mu.Unlock()

	m.entries[s] = e
	m.current[current{ns, kind}] = s

	return keys.MaterialID{Namespace: ns, Kind: kind, ID: id, Fingerprint: e.fingerprint}
}

// Replace changes the content held as id and keeps the fingerprint Give
// recorded, so id is refused as ErrMaterialUnavailable unless material is
// what it held. It panics on an ID never given, a mistake in the test.
func (m *Memory) Replace(id keys.MaterialID, material []byte) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.entry(id).content = slices.Clone(material)
}

// Destroy marks id lost for good, so it is ErrMaterialDestroyed from then on,
// and lists it for DestroyedSince unless it is destroyed already. It panics
// on an ID never given, a mistake in the test.
func (m *Memory) Destroy(id keys.MaterialID) {
	m.mu.Lock()
	defer m.mu.Unlock()

	e := m.entry(id)
	if e.destroyed {
		return
	}
	e.destroyed = true
	m.destroyed = append(m.destroyed, keys.MaterialID{Namespace: id.Namespace, Kind: id.Kind, ID: id.ID, Fingerprint: e.fingerprint})
}

// memoryIncarnation is Memory's notice incarnation, fixed, and not 0, so a
// caller that reads a cursor as a count fails against Memory as it would
// against a provider.
const memoryIncarnation = 1

// memoryBatch is the most IDs one DestroyedSince returns, the provider's bound.
const memoryBatch = 256

// DestroyedSince returns, at most 256 at a time, the IDs Destroy marked after
// cursor, each once, and the cursor to pass next, with the cursor a provider
// gives: an incarnation in its high 32 bits and an index into the list in its
// low 32 bits. A cursor of another incarnation, or past the list's end, reads
// from the start.
func (m *Memory) DestroyedSince(_ context.Context, cursor uint64) ([]keys.MaterialID, uint64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	i := uint64(0)
	if cursor>>32 == memoryIncarnation && uint32(cursor) <= uint32(len(m.destroyed)) {
		i = uint64(uint32(cursor))
	}
	j := min(i+memoryBatch, uint64(len(m.destroyed)))

	return slices.Clone(m.destroyed[i:j]), memoryIncarnation<<32 | j, nil
}

// entry returns id's entry, panicking on an ID never given. m.mu must be held.
func (m *Memory) entry(id keys.MaterialID) *entry {
	e, ok := m.entries[slot{id.Namespace, id.Kind, id.ID}]
	if !ok {
		panic(fmt.Sprintf("keystest: %s %q of namespace %s was never given", id.Kind, id.ID, id.Namespace))
	}

	return e
}

// Calls is how many times Current and Get have been called, failing calls
// included.
func (m *Memory) Calls() uint64 { return m.calls.Load() }
