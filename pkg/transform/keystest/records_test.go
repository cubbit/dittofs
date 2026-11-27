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

	RunKeyRecords(t, func(t *testing.T) *Records {
		built = append(built, t.Name())
		return NewRecords()
	})

	var want []string
	for _, r := range recordRows {
		want = append(want, t.Name()+"/"+r.name)
	}

	if !slices.Equal(built, want) {
		t.Errorf("RunKeyRecords built views for %q, want one for each of %q", built, want)
	}
}

// version is ns's commit count, 0 for a namespace never written.
func (r *Records) version(ns entity.NamespaceID) uint64 {
	r.mu.Lock()
	defer r.mu.Unlock()

	if set := r.sets[ns]; set != nil {
		return set.version
	}

	return 0
}

// noConflict commits each Update over whatever committed since its snapshot.
type noConflict struct{ *Records }

func (v noConflict) Update(_ context.Context, ns entity.NamespaceID, fn func([]record) ([]record, error)) error {
	rows, _ := v.snapshot(ns)

	out, err := fn(rows)
	if err != nil {
		return err
	}

	for _, row := range out {
		if err := checkNames(row); err != nil {
			return err
		}
	}

	v.commit(ns, v.version(ns), out)

	return nil
}

// onePerKind deletes, on each write, the other records of the written row's
// kind.
type onePerKind struct{ *Records }

func (v onePerKind) Update(ctx context.Context, ns entity.NamespaceID, fn func([]record) ([]record, error)) error {
	var out []record

	err := v.Records.Update(ctx, ns, func(rows []record) ([]record, error) {
		var err error
		out, err = fn(rows)
		return out, err
	})
	if err != nil {
		return err
	}

	v.mu.Lock()
	defer v.mu.Unlock()

	for _, row := range out {
		for slot := range v.sets[ns].rows {
			if slot.kind == row.Kind && slot.id != row.ID {
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

func (v oneNamespace) Update(ctx context.Context, _ entity.NamespaceID, fn func([]record) ([]record, error)) error {
	return v.Records.Update(ctx, entity.NamespaceID{}, fn)
}

// anyName writes rows of any kind and state.
type anyName struct{ *Records }

func (v anyName) Update(_ context.Context, ns entity.NamespaceID, fn func([]record) ([]record, error)) error {
	for {
		rows, seen := v.snapshot(ns)

		out, err := fn(rows)
		if err != nil {
			return err
		}

		if v.commit(ns, seen, out) {
			return nil
		}
	}
}

// Every view row fails on the broken views written for it and on no other,
// and passes on Records; and every row has a broken view.
func TestEachBrokenViewFailsItsRow(t *testing.T) {
	prof.Watch(t, prof.Options{})

	covered := make(map[string]bool)

	for _, c := range []struct {
		name  string
		build func() view
		want  []string
	}{
		{"Records", func() view { return NewRecords() }, nil},
		{"an Update without the conflict", func() view { return noConflict{NewRecords()} }, []string{"conflict"}},
		{"an Update deleting a record it did not return", func() view { return onePerKind{NewRecords()} }, []string{"replace"}},
		{"a raise past the limit", func() view { return raisesWhileBelow{NewRecords()} }, []string{"wrapcount"}},
		{"records shared across namespaces", func() view { return oneNamespace{NewRecords()} }, []string{"namespaces"}},
		{"any kind and state written", func() view { return anyName{NewRecords()} }, []string{"refusals"}},
	} {
		var failed []string

		for _, r := range recordRows {
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

	for _, r := range recordRows {
		if !covered[r.name] {
			t.Errorf("row %s has no broken view that must fail it", r.name)
		}
	}
}

// Each broken view fails its row on the check written for it, not on an
// earlier one.
func TestEachBrokenViewFailsOnItsCheck(t *testing.T) {
	for _, c := range []struct {
		name string
		v    view
		row  func(context.Context, view) error
		want string
	}{
		{"an Update without the conflict", noConflict{NewRecords()}, conflicting, "want exactly one of them"},
		{"an Update deleting a record it did not return", onePerKind{NewRecords()}, replacing, "Load after writing"},
		{"a raise past the limit", raisesWhileBelow{NewRecords()}, wrapCount, `RaiseWrapCount("\x00m\x00", 2, 3) = true, want false`},
		{"records shared across namespaces", oneNamespace{NewRecords()}, namespacesApart, "want once each"},
		{"any kind and state written", anyName{NewRecords()}, refusals, "a kind RFC 5 does not name succeeded"},
	} {
		err := c.row(t.Context(), c.v)
		if got := fmt.Sprint(err); err == nil || !strings.Contains(got, c.want) {
			t.Errorf("%s: row error %q, want one containing %q", c.name, got, c.want)
		}
	}
}
