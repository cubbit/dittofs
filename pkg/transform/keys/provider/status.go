package provider

import (
	"cmp"
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/transform/keys"
)

// Status is what the provider cannot serve now and how old each loaded
// namespace's current data key is, as plain values the content wiring turns
// into RFC 5 Appendix B.4's conditions and Appendix B.6's gauges. Each
// MaterialID names its material by namespace, kind and ID, with no
// fingerprint: a refused record's fingerprint is the thing in doubt.
type Status struct {
	Unavailable  []keys.MaterialID                // material_unavailable; encryption_keys_unavailable is its length
	Unknown      []keys.MaterialID                // material_unknown
	CurrentSince map[entity.NamespaceID]time.Time // encryption_current_key_age_seconds
}

// misses holds the IDs Get refused before reaching a record: those its
// namespace's records lacked, and those whose records could not be loaded.
// Status drops each once its record is cached and served or destroyed. A
// record refused once reached is not held here: its cache entry says so.
//
// ponytail: an ID asked for and never given or never loadable stays until it
// is, so a caller asking for made-up IDs, or for a namespace whose records
// never load again, grows it without bound. Cap it when Get can be reached by
// IDs no census listed.
type misses struct {
	mu          sync.Mutex
	unknown     map[keys.MaterialID]struct{}
	unavailable map[keys.MaterialID]struct{}
}

// miss records id as unknown, or as unavailable when its records could not
// be loaded, whichever the latest refused Get found.
func (m *misses) miss(id keys.MaterialID, unknown bool) {
	id.Fingerprint = [32]byte{}

	m.mu.Lock()
	defer m.mu.Unlock()

	delete(m.unknown, id)
	delete(m.unavailable, id)
	if unknown {
		m.unknown[id] = struct{}{}
	} else {
		m.unavailable[id] = struct{}{}
	}
}

// Calls is how many times Current and Get have been called, failing calls
// included, so a test can assert the provider was asked nothing.
func (p *Provider) Calls() uint64 { return p.calls.Load() }

// Status reads, from the cache, the records whose last serve failed and the
// time each loaded namespace's current data key became current, and adds the
// IDs Get missed that the cache has not served or found destroyed since.
func (p *Provider) Status() Status {
	st := Status{CurrentSince: make(map[entity.NamespaceID]time.Time)}
	unavailable := make(map[keys.MaterialID]struct{})

	p.misses.mu.Lock()
	defer p.misses.mu.Unlock()
	p.cache.mu.RLock()
	defer p.cache.mu.RUnlock()

	settled := func(id keys.MaterialID) bool {
		e := p.cache.sets[id.Namespace][slot{id.Kind, id.ID}]
		return e != nil && (e.key != nil || e.row.State == stateDestroyed)
	}
	maps.DeleteFunc(p.misses.unknown, func(id keys.MaterialID, _ struct{}) bool { return settled(id) })
	maps.DeleteFunc(p.misses.unavailable, func(id keys.MaterialID, _ struct{}) bool { return settled(id) })
	maps.Copy(unavailable, p.misses.unavailable)

	for ns, set := range p.cache.sets {
		current := make(map[string][]keys.MaterialID)
		var since int64
		for s, e := range set {
			id := keys.MaterialID{Namespace: ns, Kind: s.kind, ID: s.id}
			if e.failing {
				unavailable[id] = struct{}{}
			}
			if e.row.State != stateCurrent {
				continue
			}
			current[s.kind] = append(current[s.kind], id)
			// A record that does not decode has no start time to give.
			if s.kind != keys.KindDataKey {
				continue
			}
			if rec, err := decodeKeyRecord(e.row.Record); err == nil && rec.since > since {
				since = rec.since
				st.CurrentSince[ns] = time.Unix(0, since)
			}
		}
		// Current refuses a kind with two current records, so both are
		// unavailable, and the kind's data key has no one start time.
		for kind, ids := range current {
			if len(ids) < 2 {
				continue
			}
			for _, id := range ids {
				unavailable[id] = struct{}{}
			}
			if kind == keys.KindDataKey {
				delete(st.CurrentSince, ns)
			}
		}
	}

	st.Unavailable = slices.SortedFunc(maps.Keys(unavailable), compareMaterial)
	st.Unknown = slices.SortedFunc(maps.Keys(p.misses.unknown), compareMaterial)
	return st
}

// compareMaterial orders material by namespace, kind and ID, so Status lists
// it the same way each time.
func compareMaterial(a, b keys.MaterialID) int {
	return cmp.Or(slices.Compare(a.Namespace[:], b.Namespace[:]), cmp.Compare(a.Kind, b.Kind), cmp.Compare(a.ID, b.ID))
}
