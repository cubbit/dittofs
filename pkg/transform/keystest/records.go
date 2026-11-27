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

// recordSet is one namespace's key records and the number of commits that
// have written them, which a commit checks against the number its snapshot
// saw.
type recordSet struct {
	version uint64
	rows    map[recordSlot]record
}

// Records is a key-record view held in memory, verified by RunKeyRecords as
// the metadata store's is. It behaves as a transactional store does: Update
// runs fn on a snapshot outside any lock and commits only if no other write
// of the namespace's records committed since, running fn again otherwise.
// Its methods are safe for concurrent use.
type Records struct {
	mu     sync.Mutex
	sets   map[entity.NamespaceID]*recordSet
	counts map[string]uint64
}

// NewRecords returns a Records holding no key record and no wrap count.
func NewRecords() *Records {
	return &Records{sets: make(map[entity.NamespaceID]*recordSet), counts: make(map[string]uint64)}
}

// snapshot returns a copy of ns's rows, in no order, and the version they
// were read at.
func (r *Records) snapshot(ns entity.NamespaceID) ([]record, uint64) {
	r.mu.Lock()
	defer r.mu.Unlock()

	set := r.sets[ns]
	if set == nil {
		return nil, 0
	}

	rows := make([]record, 0, len(set.rows))
	for _, row := range set.rows {
		row.Record = slices.Clone(row.Record)
		rows = append(rows, row)
	}

	return rows, set.version
}

// Load returns every key record of ns, in no order a caller may rely on.
func (r *Records) Load(_ context.Context, ns entity.NamespaceID) ([]record, error) {
	rows, _ := r.snapshot(ns)
	return rows, nil
}

// Update runs fn on ns's key records and writes the rows fn returns, each
// replacing the record of its kind and ID; it deletes nothing. A write of
// ns's records committed between the snapshot fn saw and this commit makes
// it run fn again on a fresh snapshot, as a conflicting transaction is
// retried. A row whose kind or state RFC 5 does not name fails the call and
// writes nothing.
func (r *Records) Update(ctx context.Context, ns entity.NamespaceID, fn func(rows []record) ([]record, error)) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}

		rows, seen := r.snapshot(ns)

		out, err := fn(rows)
		if err != nil {
			return err
		}

		for _, row := range out {
			if err := checkNames(row); err != nil {
				return err
			}
		}

		if r.commit(ns, seen, out) {
			return nil
		}
	}
}

// commit writes out over ns's records if their version is still seen, and
// reports whether it did.
func (r *Records) commit(ns entity.NamespaceID, seen uint64, out []record) bool {
	r.mu.Lock()
	defer r.mu.Unlock()

	set := r.sets[ns]
	if set == nil {
		set = &recordSet{rows: make(map[recordSlot]record)}
		r.sets[ns] = set
	}

	if set.version != seen {
		return false
	}

	if len(out) == 0 {
		return true
	}

	for _, row := range out {
		row.Record = slices.Clone(row.Record)
		set.rows[recordSlot{row.Kind, row.ID}] = row
	}

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
