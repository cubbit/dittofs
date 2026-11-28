package keystest

import (
	"context"
	"fmt"
	"slices"
	"sync"

	"github.com/cubbit/dittofs/pkg/metadata/entity"
)

// record is one key record as a key-record view holds it: its kind, ID and
// state as RFC 5 Appendix B.2 names them, and the rest of the record in the
// provider's codec. It is an alias of an unnamed struct, so a view written
// with it satisfies the provider's view without importing the provider.
type record = struct {
	Kind, ID, State string
	Record          []byte
}

// recordKinds and recordStates are the names a key record may carry (RFC 5
// Appendix B.2); a view refuses any other.
var (
	recordKinds  = []string{"chunk-id-key", "chunking-key", "export-key", "data-key"}
	recordStates = []string{"current", "retiring", "retired", "destroyed"}
)

// checkNames refuses a row whose kind or state RFC 5 does not name.
func checkNames(r record) error {
	if !slices.Contains(recordKinds, r.Kind) {
		return fmt.Errorf("keystest: key record of kind %q, not one RFC 5 names", r.Kind)
	}
	if !slices.Contains(recordStates, r.State) {
		return fmt.Errorf("keystest: key record in state %q, not one RFC 5 names", r.State)
	}
	return nil
}

// recordSlot names one key record within its namespace.
type recordSlot struct{ kind, id string }

// recordSet is one namespace's key records, the last sequence recorded for
// its keys object, and the number of commits that have written either, which
// a commit checks against the number its snapshot saw.
type recordSet struct {
	version uint64
	seq     uint64
	rows    map[recordSlot]record
}

// Records is a key-record view held in memory, verified by RunKeyRecords as
// the metadata store's is. It behaves as a transactional store does: Update
// runs fn on a snapshot outside any lock and commits only if no other write
// of the namespace's records or sequence committed since, running fn again
// otherwise. Its methods are safe for concurrent use.
type Records struct {
	mu     sync.Mutex
	sets   map[entity.NamespaceID]*recordSet
	counts map[string]uint64
	stores map[entity.NamespaceID][16]byte // the remote store each namespace's record names
}

// NewRecords returns a Records holding no key record, no wrap count and no
// namespace.
func NewRecords() *Records {
	return &Records{
		sets:   make(map[entity.NamespaceID]*recordSet),
		counts: make(map[string]uint64),
		stores: make(map[entity.NamespaceID][16]byte),
	}
}

// AddNamespace records ns as a namespace whose record names the remote store,
// as creating its namespace record does in the metadata store.
func (r *Records) AddNamespace(ns entity.NamespaceID, store [16]byte) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.stores[ns] = store
}

// Namespaces returns every namespace added under the remote store, in no
// order a caller may rely on.
func (r *Records) Namespaces(_ context.Context, store [16]byte) ([]entity.NamespaceID, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	var out []entity.NamespaceID
	for ns, s := range r.stores {
		if s == store {
			out = append(out, ns)
		}
	}

	return out, nil
}

// snapshot returns a copy of ns's rows, in no order, the version they were
// read at, and ns's sequence.
func (r *Records) snapshot(ns entity.NamespaceID) ([]record, uint64, uint64) {
	r.mu.Lock()
	defer r.mu.Unlock()

	set := r.sets[ns]
	if set == nil {
		return nil, 0, 0
	}

	rows := make([]record, 0, len(set.rows))
	for _, row := range set.rows {
		row.Record = slices.Clone(row.Record)
		rows = append(rows, row)
	}

	return rows, set.version, set.seq
}

// Load returns every key record of ns, in no order a caller may rely on.
func (r *Records) Load(_ context.Context, ns entity.NamespaceID) ([]record, error) {
	rows, _, _ := r.snapshot(ns)
	return rows, nil
}

// Update runs fn on ns's key records and on the last sequence recorded for
// ns's keys object, 0 if none was, and writes the rows fn returns, each
// replacing the record of its kind and ID, and the sequence it returns; it
// deletes nothing. A write of ns's records or sequence committed between the
// snapshot fn saw and this commit makes it run fn again on a fresh snapshot,
// as a conflicting transaction is retried. A row whose kind or state RFC 5
// does not name fails the call and writes nothing.
func (r *Records) Update(ctx context.Context, ns entity.NamespaceID, fn func(seq uint64, rows []record) (uint64, []record, error)) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}

		rows, seen, seq := r.snapshot(ns)

		next, out, err := fn(seq, rows)
		if err != nil {
			return err
		}

		for _, row := range out {
			if err := checkNames(row); err != nil {
				return err
			}
		}

		if r.commit(ns, seen, next, out) {
			return nil
		}
	}
}

// commit writes out and seq over ns's records and sequence if their version
// is still seen, and reports whether it did.
func (r *Records) commit(ns entity.NamespaceID, seen, seq uint64, out []record) bool {
	r.mu.Lock()
	defer r.mu.Unlock()

	return r.commitLocked(ns, seen, seq, out)
}

// commitLocked is commit with r.mu held, for a broken view in the tests that
// commits more than Records does under the same lock.
func (r *Records) commitLocked(ns entity.NamespaceID, seen, seq uint64, out []record) bool {
	set := r.sets[ns]
	if set == nil {
		set = &recordSet{rows: make(map[recordSlot]record)}
		r.sets[ns] = set
	}

	if set.version != seen {
		return false
	}

	if len(out) == 0 && seq == set.seq {
		return true
	}

	for _, row := range out {
		row.Record = slices.Clone(row.Record)
		set.rows[recordSlot{row.Kind, row.ID}] = row
	}

	set.seq = seq
	set.version++

	return true
}

// RaiseWrapCount adds n to masterKeyID's wrap count if the sum stays at most
// limit, and reports whether it did.
func (r *Records) RaiseWrapCount(_ context.Context, masterKeyID []byte, n, limit uint64) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	count := r.counts[string(masterKeyID)]
	if n > limit || count > limit-n {
		return false, nil
	}

	r.counts[string(masterKeyID)] = count + n

	return true, nil
}
