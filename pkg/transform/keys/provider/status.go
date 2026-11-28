package provider

import (
	"cmp"
	"maps"
	"slices"
	"sync"
	"sync/atomic"
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

// matKey names one piece of material for the status maps.
type matKey struct {
	ns entity.NamespaceID
	slot
}

// status holds the material reads refused, each until its next successful
// read or until it is found destroyed.
//
// ponytail: an ID asked for and never given stays in unknown until it is
// given and read, so a caller asking for made-up IDs grows it without
// bound. Cap it when Get can be reached by IDs no census listed.
type status struct {
	pending     atomic.Int64 // len(unavailable) + len(unknown), so a read with nothing to clear takes no lock
	mu          sync.Mutex
	unavailable map[matKey]struct{}
	unknown     map[matKey]struct{}
}

// refuse records k as unknown or as unavailable, whichever its latest
// refused read found it, until its next successful read.
func (s *status) refuse(k matKey, unknown bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.unknown == nil {
		s.unknown, s.unavailable = make(map[matKey]struct{}), make(map[matKey]struct{})
	}
	in, out := s.unavailable, s.unknown
	if unknown {
		in, out = out, in
	}
	if _, ok := out[k]; ok {
		delete(out, k)
		s.pending.Add(-1)
	}
	if _, ok := in[k]; !ok {
		in[k] = struct{}{}
		s.pending.Add(1)
	}
}

// settled clears k after a read that served it, or found it destroyed,
// which no condition reports as unavailable.
func (s *status) settled(k matKey) {
	if s.pending.Load() == 0 {
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	for _, m := range []map[matKey]struct{}{s.unavailable, s.unknown} {
		if _, ok := m[k]; ok {
			delete(m, k)
			s.pending.Add(-1)
		}
	}
}

// Calls is how many times Current and Get have been called, failing calls
// included, so a test can assert the provider was asked nothing.
func (p *Provider) Calls() uint64 { return p.calls.Load() }

// Status returns the material the provider last failed to serve and has not
// served since, the IDs asked for and never given since, and the time each
// loaded namespace's current data key became current, read from its record.
func (p *Provider) Status() Status {
	st := Status{CurrentSince: make(map[entity.NamespaceID]time.Time)}

	p.status.mu.Lock()
	for _, c := range []struct {
		m   map[matKey]struct{}
		out *[]keys.MaterialID
	}{{p.status.unavailable, &st.Unavailable}, {p.status.unknown, &st.Unknown}} {
		for _, k := range slices.SortedFunc(maps.Keys(c.m), compareMatKey) {
			*c.out = append(*c.out, keys.MaterialID{Namespace: k.ns, Kind: k.kind, ID: k.id})
		}
	}
	p.status.mu.Unlock()

	p.cache.mu.RLock()
	defer p.cache.mu.RUnlock()

	for ns, set := range p.cache.sets {
		for s, e := range set {
			if s.kind != keys.KindDataKey || e.row.State != stateCurrent {
				continue
			}
			// A record that does not decode is reported unavailable at its
			// first read; it has no start time to give.
			if rec, err := decodeKeyRecord(e.row.Record); err == nil {
				st.CurrentSince[ns] = time.Unix(0, rec.since)
			}
		}
	}
	return st
}

// compareMatKey orders material by namespace, kind and ID, so Status lists
// it the same way each time.
func compareMatKey(a, b matKey) int {
	return cmp.Or(slices.Compare(a.ns[:], b.ns[:]), cmp.Compare(a.kind, b.kind), cmp.Compare(a.id, b.id))
}
