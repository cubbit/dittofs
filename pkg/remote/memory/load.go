package memory

import (
	"bytes"
	"maps"
	"slices"
	"strings"
	"sync"

	"github.com/cubbit/dittofs/pkg/remote"
)

// CallRecord is one call as it reached the store.
type CallRecord struct {
	Op   string      // RFC 4 §4.12's op: put, get, delete, list, health, put_control, get_control
	Name remote.Name // the block's, for put and get; zero otherwise
}

// Load is the store's record of its load (RFC 3 §1.4), an output a test
// asserts on.
type Load struct {
	// InFlight is the calls between their entry and their return, a get's
	// until its reader is closed; Peak is the most InFlight has been.
	InFlight, Peak int

	// Calls lists every call in the order it reached the store.
	Calls []CallRecord

	// Puts counts every put that reached the store, per name, failed and
	// refused ones too.
	Puts map[remote.Name]int

	// Bytes is the size of the blocks held, control objects and keys under
	// blocks/ that are not names excluded.
	Bytes int64

	// Foreign counts each key under blocks/ that is not a name, once for
	// every listing that skips it.
	Foreign int
}

// load is the record a namespace keeps, under the namespace's mutex.
type load struct {
	inFlight, peak int

	// ponytail: calls grows by one entry per call for the store's life.
	// Record it only when a test switches it on if a benchmark over this
	// store shows it in its heap.
	calls   []CallRecord
	puts    map[remote.Name]int
	foreign int
}

// Load returns a copy of the record of the calls that reached the store. A
// call a wrapper refuses never reaches the store, so it is not in the record.
//
// decision: the record is the namespace's, so a store opened again over it
// adds to the same record, as a service sees the load of every client of its
// bucket and the bytes held are the namespace's. A check that reopens its
// store reads both stores' calls here; give each store its own record if a
// check needs one client's load alone.
func (s *Store) Load() Load {
	s.ns.mu.Lock()
	defer s.ns.mu.Unlock()

	l := s.ns.load
	out := Load{InFlight: l.inFlight, Peak: l.peak, Calls: slices.Clone(l.calls), Puts: maps.Clone(l.puts), Foreign: l.foreign}

	for k, o := range s.ns.objects {
		if h, ok := strings.CutPrefix(k, blockPrefix); ok {
			if _, ok := parseName(h); ok {
				out.Bytes += int64(len(o.body))
			}
		}
	}

	return out
}

// enter records a call of op reaching the store, and returns the func that
// records its return. That func may be called more than once and records the
// return once. The mutex is held only for each update, never across the call.
func (s *Store) enter(op string, name remote.Name) func() {
	s.ns.mu.Lock()

	l := &s.ns.load
	l.calls = append(l.calls, CallRecord{Op: op, Name: name})

	if op == "put" {
		if l.puts == nil {
			l.puts = make(map[remote.Name]int)
		}

		l.puts[name]++
	}

	l.inFlight++
	l.peak = max(l.peak, l.inFlight)
	s.ns.mu.Unlock()

	return sync.OnceFunc(func() {
		s.ns.mu.Lock()
		l.inFlight--
		s.ns.mu.Unlock()
	})
}

// reader is a get's body: the call stays in flight until it is closed.
type reader struct {
	*bytes.Reader

	done func()
}

func (r *reader) Close() error {
	r.done()
	return nil
}
