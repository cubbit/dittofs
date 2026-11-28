package keystest

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/cubbit/dittofs/internal/prof"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
)

// Records passes the view suite, and RunKeyRecords runs every row, each on a
// fresh view.
func TestRecordsPassesTheSuite(t *testing.T) {
	prof.Watch(t, prof.Options{})

	var built []string

	RunKeyRecords(t, func(t *testing.T) RecordStore[*Records] {
		built = append(built, t.Name())
		return storeOf(NewRecords())
	})

	var want []string
	for _, r := range recordRows {
		want = append(want, t.Name()+"/"+r.name)
	}

	if !slices.Equal(built, want) {
		t.Errorf("RunKeyRecords built views for %q, want one for each of %q", built, want)
	}
}

// storeOf is the store a Records is: every view over it is the Records itself.
func storeOf(r *Records) RecordStore[*Records] {
	return RecordStore[*Records]{
		View: func() *Records { return r },
		AddNamespace: func(_ context.Context, ns entity.NamespaceID, store [16]byte) error {
			r.AddNamespace(ns, store)
			return nil
		},
	}
}

// brokenStore is the store under a broken view: wrap wraps the Records the
// namespaces are added to, each call a fresh view.
func brokenStore(wrap func(*Records) view) RecordStore[view] {
	r := NewRecords()
	return RecordStore[view]{View: func() view { return wrap(r) }, AddNamespace: storeOf(r).AddNamespace}
}

// noConflict commits each Update over whatever committed since its snapshot.
type noConflict struct{ *Records }

func (v noConflict) Update(_ context.Context, ns entity.NamespaceID, fn func(uint64, []record) (uint64, []record, error)) error {
	rows, _, seq := v.snapshot(ns)

	next, out, err := fn(seq, rows)
	if err != nil {
		return err
	}

	for _, row := range out {
		if err := checkNames(row); err != nil {
			return err
		}
	}

	for {
		if _, now, _ := v.snapshot(ns); v.commit(ns, now, next, out) {
			return nil
		}
	}
}

// onePerKind deletes, on each write, the records of the written rows' kinds
// that the write did not return.
type onePerKind struct{ *Records }

func (v onePerKind) Update(ctx context.Context, ns entity.NamespaceID, fn func(uint64, []record) (uint64, []record, error)) error {
	var out []record

	err := v.Records.Update(ctx, ns, func(seq uint64, rows []record) (uint64, []record, error) {
		var (
			next uint64
			err  error
		)
		next, out, err = fn(seq, rows)
		return next, out, err
	})
	if err != nil {
		return err
	}

	returned := make(map[recordSlot]bool, len(out))
	for _, row := range out {
		returned[recordSlot{row.Kind, row.ID}] = true
	}

	v.mu.Lock()
	defer v.mu.Unlock()

	for _, row := range out {
		for slot := range v.sets[ns].rows {
			if slot.kind == row.Kind && !returned[slot] {
				delete(v.sets[ns].rows, slot)
			}
		}
	}

	return nil
}

// raisesWhileBelow raises a count by n whenever it is below the limit, past
// the limit included.
type raisesWhileBelow struct{ *Records }

func (v raisesWhileBelow) RaiseWrapCount(_ context.Context, masterKeyID []byte, n, limit uint64) (bool, error) {
	v.mu.Lock()
	defer v.mu.Unlock()

	if v.counts[string(masterKeyID)] >= limit {
		return false, nil
	}

	v.counts[string(masterKeyID)] += n

	return true, nil
}

// oneNamespace keeps every namespace's records as one namespace's.
type oneNamespace struct{ *Records }

func (v oneNamespace) Load(ctx context.Context, _ entity.NamespaceID) ([]record, error) {
	return v.Records.Load(ctx, entity.NamespaceID{})
}

func (v oneNamespace) Update(ctx context.Context, _ entity.NamespaceID, fn func(uint64, []record) (uint64, []record, error)) error {
	return v.Records.Update(ctx, entity.NamespaceID{}, fn)
}

// loadsAll returns from Load every namespace's records.
type loadsAll struct{ *Records }

func (v loadsAll) Load(context.Context, entity.NamespaceID) ([]record, error) {
	v.mu.Lock()
	defer v.mu.Unlock()

	var rows []record
	for _, set := range v.sets {
		for _, row := range set.rows {
			rows = append(rows, row)
		}
	}

	return rows, nil
}

// anyName writes rows of any kind and state.
type anyName struct{ *Records }

func (v anyName) Update(_ context.Context, ns entity.NamespaceID, fn func(uint64, []record) (uint64, []record, error)) error {
	for {
		rows, seen, seq := v.snapshot(ns)

		next, out, err := fn(seq, rows)
		if err != nil {
			return err
		}

		if v.commit(ns, seen, next, out) {
			return nil
		}
	}
}

// seqOutside reads the sequence once, before the transaction, and writes it
// after the commit, so the conflict covers the records but not the sequence.
type seqOutside struct{ *Records }

func (v seqOutside) Update(ctx context.Context, ns entity.NamespaceID, fn func(uint64, []record) (uint64, []record, error)) error {
	_, _, seq := v.snapshot(ns)

	var next uint64

	err := v.Records.Update(ctx, ns, func(stored uint64, rows []record) (uint64, []record, error) {
		var (
			out []record
			err error
		)
		next, out, err = fn(seq, rows)
		return stored, out, err
	})
	if err != nil {
		return err
	}

	v.mu.Lock()
	defer v.mu.Unlock()

	v.sets[ns].seq = next

	return nil
}

// seqInView keeps each namespace's sequence in the view rather than the
// store, committed with the records, so a fresh view reads every sequence as
// 0.
type seqInView struct {
	*Records
	seqs map[entity.NamespaceID]uint64 // under Records.mu
}

func newSeqInView(r *Records) view { return seqInView{r, make(map[entity.NamespaceID]uint64)} }

func (v seqInView) Update(_ context.Context, ns entity.NamespaceID, fn func(uint64, []record) (uint64, []record, error)) error {
	for {
		rows, seen, _ := v.snapshot(ns)

		v.mu.Lock()
		seq := v.seqs[ns]
		v.mu.Unlock()

		next, out, err := fn(seq, rows)
		if err != nil {
			return err
		}

		for _, row := range out {
			if err := checkNames(row); err != nil {
				return err
			}
		}

		v.mu.Lock()
		ok := v.commitLocked(ns, seen, next, out)
		if ok {
			v.seqs[ns] = next
		}
		v.mu.Unlock()

		if ok {
			return nil
		}
	}
}

// listsLoaded lists only the namespaces of a store whose records it has
// been asked to load or update.
type listsLoaded struct{ *Records }

func (v listsLoaded) Namespaces(_ context.Context, store [16]byte) ([]entity.NamespaceID, error) {
	v.mu.Lock()
	defer v.mu.Unlock()

	var out []entity.NamespaceID
	for ns := range v.sets {
		if v.stores[ns] == store {
			out = append(out, ns)
		}
	}

	return out, nil
}

// Every view row fails on the broken views written for it and on no other,
// and passes on Records; and every row has a broken view.
func TestEachBrokenViewFailsItsRow(t *testing.T) {
	prof.Watch(t, prof.Options{})

	covered := make(map[string]bool)

	for _, c := range []struct {
		name string
		view func(*Records) view
		want []string
	}{
		{"Records", func(r *Records) view { return r }, nil},
		{"an Update without the conflict", func(r *Records) view { return noConflict{r} }, []string{"conflict", "lock"}},
		{"an Update deleting a record it did not return", func(r *Records) view { return onePerKind{r} }, []string{"replace"}},
		{"a raise past the limit", func(r *Records) view { return raisesWhileBelow{r} }, []string{"wrapcount"}},
		{"records shared across namespaces", func(r *Records) view { return oneNamespace{r} }, []string{"namespaces", "sequence", "listing"}},
		{"a Load returning every namespace's records", func(r *Records) view { return loadsAll{r} }, []string{"namespaces", "listing"}},
		{"any kind and state written", func(r *Records) view { return anyName{r} }, []string{"refusals"}},
		{"a sequence read outside the transaction", func(r *Records) view { return seqOutside{r} }, []string{"lock"}},
		{"a sequence kept in the view", newSeqInView, []string{"sequence"}},
		{"a listing of the namespaces loaded", func(r *Records) view { return listsLoaded{r} }, []string{"listing"}},
	} {
		var failed []string

		for _, r := range recordRows {
			if err := r.check(t.Context(), brokenStore(c.view)); err != nil {
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

	for _, r := range recordRows {
		if !covered[r.name] {
			t.Errorf("row %s has no broken view that must fail it", r.name)
		}
	}
}

// Each broken view fails its row on the check written for it, not on an
// earlier one.
func TestEachBrokenViewFailsOnItsCheck(t *testing.T) {
	prof.Watch(t, prof.Options{})

	for _, c := range []struct {
		name string
		view func(*Records) view
		row  func(context.Context, RecordStore[view]) error
		want string
	}{
		{"an Update without the conflict", func(r *Records) view { return noConflict{r} }, onView(conflicting), "want exactly one of them"},
		{"an Update deleting a record it did not return", func(r *Records) view { return onePerKind{r} }, onView(replacing), "Load after rewriting"},
		{"a raise past the limit", func(r *Records) view { return raisesWhileBelow{r} }, onView(wrapCount), `RaiseWrapCount("\x00m\x00", 2, 3) = true, want false`},
		{"records shared across namespaces", func(r *Records) view { return oneNamespace{r} }, onView(namespacesApart), "want once each"},
		{"a Load returning every namespace's records", func(r *Records) view { return loadsAll{r} }, onView(namespacesApart), "want only"},
		{"any kind and state written", func(r *Records) view { return anyName{r} }, onView(refusals), "a kind RFC 5 does not name succeeded"},
		{"a sequence read outside the transaction", func(r *Records) view { return seqOutside{r} }, onView(locking), "both claimed one sequence"},
		{"a sequence kept in the view", newSeqInView, sequenceKept, "want the 9223372041149743111 written"},
		{"a listing of the namespaces loaded", func(r *Records) view { return listsLoaded{r} }, listing, "want [00000000-0000-0000-0000-000000000001"},
	} {
		t.Run(c.name, func(t *testing.T) {
			err := c.row(t.Context(), brokenStore(c.view))
			if got := fmt.Sprint(err); err == nil || !strings.Contains(got, c.want) {
				t.Errorf("%s: row error %q, want one containing %q", c.name, got, c.want)
			}
		})
	}
}
