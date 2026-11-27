package keystest

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/metadata/entity"
)

// view is a key-record view: the three calls the provider makes of the
// metadata store's key records, in plain types, so any view that has them
// satisfies it without importing anything of this package.
type view = interface {
	Load(ctx context.Context, ns entity.NamespaceID) ([]record, error)
	Update(ctx context.Context, ns entity.NamespaceID, fn func(rows []record) ([]record, error)) error
	RaiseWrapCount(ctx context.Context, masterKeyID []byte, n, limit uint64) (bool, error)
}

// RunKeyRecords holds the key-record view newView builds to RFC 5 Appendix
// B.1's wrap count and Appendix B.2's key records, one subtest per row, each
// on a fresh view holding no record and no count.
func RunKeyRecords[V view](t *testing.T, newView func(t *testing.T) V) {
	t.Helper()

	for _, r := range recordRows {
		t.Run(r.name, func(t *testing.T) {
			if err := r.check(t.Context(), newView(t)); err != nil {
				t.Error(err)
			}
		})
	}
}

// recordRow is one check of the view suite. It returns an error naming what
// it found wrong, or nil.
type recordRow struct {
	name  string
	check func(ctx context.Context, v view) error
}

// recordRows is the view suite, in the order RunKeyRecords runs it.
var recordRows = []recordRow{
	{"conflict", conflicting},
	{"namespaces", namespacesApart},
	{"replace", replacing},
	{"refusals", refusals},
	{"wrapcount", wrapCount},
}

// backstop bounds a wait on the other transaction of a pair, so a view that
// runs one Update at a time fails the row rather than hanging it.
const backstop = 30 * time.Second

// pair runs a and b as two concurrent Updates, each holding its first run of
// fn until both have taken their snapshot, and returns how many times each fn
// ran.
func pair(ctx context.Context, v view, a, b entity.NamespaceID, fa, fb func([]record) []record) (runsA, runsB int, err error) {
	var arrived sync.WaitGroup
	arrived.Add(2)

	both := make(chan struct{})
	go func() { arrived.Wait(); close(both) }()

	hold := func(runs *atomic.Int32, f func([]record) []record) func([]record) ([]record, error) {
		return func(rows []record) ([]record, error) {
			if runs.Add(1) == 1 {
				arrived.Done()
				select {
				case <-both:
				case <-time.After(backstop):
					return nil, errors.New("the other Update never took its snapshot while this one held its own open")
				}
			}
			return f(rows), nil
		}
	}

	var ra, rb atomic.Int32
	errs := make(chan error, 2)

	go func() { errs <- v.Update(ctx, a, hold(&ra, fa)) }()
	go func() { errs <- v.Update(ctx, b, hold(&rb, fb)) }()

	err = errors.Join(<-errs, <-errs)

	return int(ra.Load()), int(rb.Load()), err
}

// sorted returns rows ordered by kind and ID, so two sets compare as sets.
func sorted(rows []record) []record {
	rows = slices.Clone(rows)
	slices.SortFunc(rows, func(a, b record) int { return cmp.Or(strings.Compare(a.Kind, b.Kind), strings.Compare(a.ID, b.ID)) })

	return rows
}

func sameRows(got, want []record) bool {
	return slices.EqualFunc(sorted(got), sorted(want), func(a, b record) bool {
		return a.Kind == b.Kind && a.ID == b.ID && a.State == b.State && bytes.Equal(a.Record, b.Record)
	})
}

// onlyIfNone returns fn for an Update that writes row only when it sees no
// record.
func onlyIfNone(row record) func([]record) []record {
	return func(rows []record) []record {
		if len(rows) > 0 {
			return nil
		}
		return []record{row}
	}
}

// Two Updates of one namespace, each writing a record only when it sees none,
// both holding their snapshots open until the other has taken its own, leave
// one record: the second to commit conflicts and, run again, sees the first's.
func conflicting(ctx context.Context, v view) error {
	ns := namespace(1)
	a := record{"data-key", "a", "current", []byte("a")}
	b := record{"data-key", "b", "current", []byte("b")}

	if _, _, err := pair(ctx, v, ns, ns, onlyIfNone(a), onlyIfNone(b)); err != nil {
		return fmt.Errorf("two Updates of one namespace: %w", err)
	}

	rows, err := v.Load(ctx, ns)
	if err != nil {
		return fmt.Errorf("Load after two Updates: %w", err)
	}

	if !sameRows(rows, []record{a}) && !sameRows(rows, []record{b}) {
		return fmt.Errorf("two Updates of one namespace, each writing a record only if it saw none, left %q, want exactly one of them", sorted(rows))
	}

	return nil
}

// Updates of two namespaces held open together commit without conflict, each
// running fn once, and Load of one never returns the other's records.
func namespacesApart(ctx context.Context, v view) error {
	a := record{"data-key", "a", "current", []byte("a")}
	b := record{"data-key", "b", "current", []byte("b")}

	runsA, runsB, err := pair(ctx, v, namespace(1), namespace(2), onlyIfNone(a), onlyIfNone(b))
	if err != nil {
		return fmt.Errorf("two namespaces' Updates: %w", err)
	}

	// A transaction a stalled scheduler holds past the backend's age limit
	// also runs fn again, and reads here as a conflict.
	if runsA != 1 || runsB != 1 {
		return fmt.Errorf("two namespaces' Updates ran fn %d and %d times, want once each: they conflicted", runsA, runsB)
	}

	for _, c := range []struct {
		ns   entity.NamespaceID
		want record
	}{{namespace(1), a}, {namespace(2), b}} {
		rows, err := v.Load(ctx, c.ns)
		if err != nil {
			return fmt.Errorf("Load(namespace %v): %w", c.ns, err)
		}

		if !sameRows(rows, []record{c.want}) {
			return fmt.Errorf("Load(namespace %v) = %q, want only %q", c.ns, sorted(rows), c.want)
		}
	}

	return nil
}

// A row written replaces the record of its kind and ID, state and record
// alike, and every record it does not name stays, whatever its kind, its
// state or an ID another kind shares.
func replacing(ctx context.Context, v view) error {
	ns := namespace(1)
	first := []record{
		{"chunk-id-key", "x", "current", []byte("chunk id")},
		{"chunking-key", "x", "current", []byte("chunking")},
		{"export-key", "x", "retiring", []byte("export")},
		{"data-key", "x", "retired", []byte("old data")},
		{"data-key", "y", "current", []byte("data")},
	}

	if err := v.Update(ctx, ns, func([]record) ([]record, error) { return first, nil }); err != nil {
		return fmt.Errorf("Update writing %d records: %w", len(first), err)
	}

	rows, err := v.Load(ctx, ns)
	if err != nil {
		return fmt.Errorf("Load after the first Update: %w", err)
	}

	if !sameRows(rows, first) {
		return fmt.Errorf("Load after writing %q = %q", sorted(first), sorted(rows))
	}

	rewritten := record{"data-key", "y", "destroyed", []byte("gone")}

	var seen []record

	err = v.Update(ctx, ns, func(rows []record) ([]record, error) {
		seen = rows
		return []record{rewritten}, nil
	})
	if err != nil {
		return fmt.Errorf("Update rewriting %q: %w", rewritten, err)
	}

	if !sameRows(seen, first) {
		return fmt.Errorf("the rewriting Update's fn saw %q, want the records written, %q", sorted(seen), sorted(first))
	}

	want := append(slices.Clone(first[:4]), rewritten)

	if rows, err = v.Load(ctx, ns); err != nil {
		return fmt.Errorf("Load after the rewrite: %w", err)
	}

	if !sameRows(rows, want) {
		return fmt.Errorf("Load after rewriting %q = %q, want %q", rewritten, sorted(rows), sorted(want))
	}

	return nil
}

// An Update returning a row whose kind or state RFC 5 does not name, or whose
// fn fails, fails and writes nothing of what it returned.
func refusals(ctx context.Context, v view) error {
	ns := namespace(1)
	good := record{"data-key", "good", "current", []byte("good")}
	errFn := errors.New("fn failed")

	for _, c := range []struct {
		name string
		row  record
		err  error
	}{
		{"a kind RFC 5 does not name", record{"header-key", "h", "current", []byte("h")}, nil},
		{"a state RFC 5 does not name", record{"data-key", "s", "pending", []byte("s")}, nil},
		{"a failing fn", good, errFn},
	} {
		err := v.Update(ctx, ns, func([]record) ([]record, error) { return []record{good, c.row}, c.err })
		if err == nil {
			return fmt.Errorf("Update returning %s succeeded, want it refused", c.name)
		}

		if c.err != nil && !errors.Is(err, c.err) {
			return fmt.Errorf("Update with %s = %v, want fn's error", c.name, err)
		}

		rows, err := v.Load(ctx, ns)
		if err != nil {
			return fmt.Errorf("Load after %s: %w", c.name, err)
		}

		if len(rows) != 0 {
			return fmt.Errorf("Update refused for %s wrote %q, want nothing", c.name, sorted(rows))
		}
	}

	return nil
}

// RaiseWrapCount raises a master key's count only while the sum stays within
// the limit, never in part, one raise at a time under contention, and each
// master key's count on its own.
func wrapCount(ctx context.Context, v view) error {
	// IDs holding zero bytes, one the other's prefix, so a key encoding that
	// stops at a zero byte, or reads one ID as the other's prefix, is seen.
	contended, stepped, near := []byte("\x00m"), []byte("\x00m\x00"), []byte("m\x00\xff")

	var raised atomic.Int32

	var wg sync.WaitGroup
	errs := make(chan error, 8)

	for range 8 {
		wg.Go(func() {
			ok, err := v.RaiseWrapCount(ctx, contended, 1, 3)
			if err != nil {
				errs <- err
			}
			if ok {
				raised.Add(1)
			}
		})
	}

	wg.Wait()
	close(errs)

	if err, failed := <-errs; failed {
		return fmt.Errorf("RaiseWrapCount(%q, 1, 3) from 8 goroutines: %w", contended, err)
	}

	if n := raised.Load(); n != 3 {
		return fmt.Errorf("RaiseWrapCount(%q, 1, 3) from 8 goroutines raised %d times, want 3", contended, n)
	}

	for _, s := range []struct {
		id       []byte
		n, limit uint64
		want     bool
	}{
		{stepped, 2, 3, true},
		{stepped, 2, 3, false},
		{stepped, 1, 3, true},
		{stepped, 1, 3, false},
		{near, 1<<32 - 1, 1 << 32, true},
		{near, 1, 1 << 32, true},
		{near, 1, 1 << 32, false},
		{near, 1<<64 - 1, 1 << 32, false},
	} {
		ok, err := v.RaiseWrapCount(ctx, s.id, s.n, s.limit)
		if err != nil {
			return fmt.Errorf("RaiseWrapCount(%q, %d, %d): %w", s.id, s.n, s.limit, err)
		}

		if ok != s.want {
			return fmt.Errorf("RaiseWrapCount(%q, %d, %d) = %v, want %v, in the sequence from a count of 0", s.id, s.n, s.limit, ok, s.want)
		}
	}

	return nil
}
