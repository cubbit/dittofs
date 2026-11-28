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

// view is a key-record view: the calls the provider makes of the metadata
// store's key records, in plain types, so any view that has them satisfies
// it without importing anything of this package.
type view = interface {
	Load(ctx context.Context, ns entity.NamespaceID) ([]record, error)
	Update(ctx context.Context, ns entity.NamespaceID, fn func(seq uint64, rows []record) (uint64, []record, error)) error
	RaiseWrapCount(ctx context.Context, masterKeyID []byte, n, limit uint64) (bool, error)
	Namespaces(ctx context.Context, store [16]byte) ([]entity.NamespaceID, error)
}

// RecordStore is one store of key records as RunKeyRecords drives it.
type RecordStore[V view] struct {
	// View returns a view over the store, each call a fresh one that holds
	// nothing a former view held.
	View func() V
	// AddNamespace creates ns's namespace record, naming the remote store.
	AddNamespace func(ctx context.Context, ns entity.NamespaceID, store [16]byte) error
}

// RunKeyRecords holds the key-record view over the store newStore builds to
// RFC 5 Appendix B.1's wrap count, Appendix B.2's key records and RFC 16
// §4.2's key lock, one subtest per row, each on a fresh store holding no
// record, no count, no sequence and no namespace.
func RunKeyRecords[V view](t *testing.T, newStore func(t *testing.T) RecordStore[V]) {
	t.Helper()

	for _, r := range recordRows {
		t.Run(r.name, func(t *testing.T) {
			s := newStore(t)
			if err := r.check(t.Context(), RecordStore[view]{View: func() view { return s.View() }, AddNamespace: s.AddNamespace}); err != nil {
				t.Error(err)
			}
		})
	}
}

// recordRow is one check of the view suite. It returns an error naming what
// it found wrong, or nil.
type recordRow struct {
	name  string
	check func(ctx context.Context, s RecordStore[view]) error
}

// onView runs check on one view over the store.
func onView(check func(ctx context.Context, v view) error) func(context.Context, RecordStore[view]) error {
	return func(ctx context.Context, s RecordStore[view]) error { return check(ctx, s.View()) }
}

// recordRows is the view suite, in the order RunKeyRecords runs it.
var recordRows = []recordRow{
	{"conflict", onView(conflicting)},
	{"namespaces", onView(namespacesApart)},
	{"replace", onView(replacing)},
	{"refusals", onView(refusals)},
	{"wrapcount", onView(wrapCount)},
	{"lock", onView(locking)},
	{"sequence", sequenceKept},
	{"listing", listing},
}

// backstop bounds a wait on the other transaction of a pair, so a view that
// runs one Update at a time fails the row rather than hanging it.
const backstop = 30 * time.Second

// update is an Update's fn that cannot fail.
type update = func(seq uint64, rows []record) (uint64, []record)

// pair runs a and b as two concurrent Updates, each holding its first run of
// fn until both have taken their snapshot, and returns how many times each fn
// ran.
func pair(ctx context.Context, v view, a, b entity.NamespaceID, fa, fb update) (runsA, runsB int, err error) {
	var arrived sync.WaitGroup
	arrived.Add(2)

	both := make(chan struct{})
	go func() { arrived.Wait(); close(both) }()

	hold := func(runs *atomic.Int32, f update) func(uint64, []record) (uint64, []record, error) {
		return func(seq uint64, rows []record) (uint64, []record, error) {
			if runs.Add(1) == 1 {
				arrived.Done()
				select {
				case <-both:
				case <-time.After(backstop):
					return 0, nil, errors.New("the other Update never took its snapshot while this one held its own open")
				}
			}
			next, out := f(seq, rows)
			return next, out, nil
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
// record, and leaves the sequence as it found it.
func onlyIfNone(row record) update {
	return func(seq uint64, rows []record) (uint64, []record) {
		if len(rows) > 0 {
			return seq, nil
		}
		return seq, []record{row}
	}
}

// writing returns fn for an Update that writes rows and leaves the sequence
// as it found it.
func writing(rows ...record) func(uint64, []record) (uint64, []record, error) {
	return func(seq uint64, _ []record) (uint64, []record, error) { return seq, rows, nil }
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

	if err := v.Update(ctx, ns, writing(first...)); err != nil {
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

	err = v.Update(ctx, ns, func(seq uint64, rows []record) (uint64, []record, error) {
		seen = rows
		return seq, []record{rewritten}, nil
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
		err := v.Update(ctx, ns, func(seq uint64, _ []record) (uint64, []record, error) { return seq + 1, []record{good, c.row}, c.err })
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

		if seq, err := sequenceOf(ctx, v, ns); err != nil || seq != 0 {
			return fmt.Errorf("Update refused for %s left the sequence at %d (%v), want 0", c.name, seq, err)
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

// sequenceOf returns the sequence an Update of ns sees, writing nothing.
func sequenceOf(ctx context.Context, v view, ns entity.NamespaceID) (uint64, error) {
	var seen uint64

	err := v.Update(ctx, ns, func(seq uint64, _ []record) (uint64, []record, error) {
		seen = seq
		return seq, nil, nil
	})

	return seen, err
}

// raiseTo returns fn for an Update that records seq and writes no row.
func raiseTo(seq uint64) func(uint64, []record) (uint64, []record, error) {
	return func(uint64, []record) (uint64, []record, error) { return seq, nil, nil }
}

// Two Updates of one namespace, each raising the sequence by one from the
// value it saw, both holding their snapshots open until the other has taken
// its own, raise it by two: the second to commit conflicts and, run again,
// raises the first's. So two writers never claim one sequence (RFC 16 §4.2's
// key lock). And a write decided on the sequence runs again once a raise
// commits after its snapshot.
func locking(ctx context.Context, v view) error {
	ns := namespace(1)

	const start = 41
	if err := v.Update(ctx, ns, raiseTo(start)); err != nil {
		return fmt.Errorf("Update recording sequence %d: %w", start, err)
	}

	var lastA, lastB atomic.Uint64

	raise := func(last *atomic.Uint64) update {
		return func(seq uint64, _ []record) (uint64, []record) {
			last.Store(seq)
			return seq + 1, nil
		}
	}

	if _, _, err := pair(ctx, v, ns, ns, raise(&lastA), raise(&lastB)); err != nil {
		return fmt.Errorf("two Updates raising one namespace's sequence: %w", err)
	}

	got, err := sequenceOf(ctx, v, ns)
	if err != nil {
		return fmt.Errorf("reading the sequence after two raises: %w", err)
	}

	if got != start+2 {
		return fmt.Errorf("two Updates each raising the sequence from %d by one left it at %d, want %d: both claimed one sequence", start, got, start+2)
	}

	if a, b := lastA.Load(), lastB.Load(); a == b {
		return fmt.Errorf("the two Updates that committed both saw sequence %d, want different values", a)
	}

	return readerRerun(ctx, v, ns, start+2)
}

// readerRerun checks that an Update writing a record named after the
// sequence it saw, whose snapshot a raise of that sequence committed after,
// runs again and writes the record from the raised one: the sequence is read
// tracked, not only written.
func readerRerun(ctx context.Context, v view, ns entity.NamespaceID, at uint64) error {
	snapped, raised := make(chan struct{}), make(chan struct{})

	var runs atomic.Int32

	done := make(chan error, 1)

	go func() {
		done <- v.Update(ctx, ns, func(seq uint64, _ []record) (uint64, []record, error) {
			if runs.Add(1) == 1 {
				close(snapped)
				select {
				case <-raised:
				case <-time.After(backstop):
					return 0, nil, errors.New("the raise never committed while this Update held its snapshot open")
				}
			}
			return seq, []record{{"data-key", fmt.Sprint(seq), "current", nil}}, nil
		})
	}()

	select {
	case <-snapped:
	case err := <-done:
		return fmt.Errorf("the Update writing from the sequence returned before its snapshot was held: %w", err)
	case <-time.After(backstop):
		return errors.New("the Update writing from the sequence never ran fn")
	}

	err := v.Update(ctx, ns, raiseTo(at+1))
	close(raised)

	if err != nil {
		return fmt.Errorf("Update raising the sequence to %d while another held its snapshot: %w", at+1, err)
	}

	if err := <-done; err != nil {
		return fmt.Errorf("the Update writing from the sequence: %w", err)
	}

	rows, err := v.Load(ctx, ns)
	if err != nil {
		return fmt.Errorf("Load after the raise: %w", err)
	}

	want := []record{{"data-key", fmt.Sprint(at + 1), "current", nil}}
	if !sameRows(rows, want) {
		return fmt.Errorf("an Update reading sequence %d, raised to %d after its snapshot, wrote %q, want %q: its read of the sequence was not tracked", at, at+1, sorted(rows), want)
	}

	return nil
}

// A sequence written is read back by a fresh view over the same store, the
// whole of its 64 bits, and a namespace that never had one reads 0.
func sequenceKept(ctx context.Context, s RecordStore[view]) error {
	const written = 1<<63 | 1<<32 | 7

	if err := s.View().Update(ctx, namespace(1), raiseTo(written)); err != nil {
		return fmt.Errorf("Update recording sequence %d: %w", uint64(written), err)
	}

	fresh := s.View()

	got, err := sequenceOf(ctx, fresh, namespace(1))
	if err != nil {
		return fmt.Errorf("reading the sequence from a fresh view: %w", err)
	}

	if got != written {
		return fmt.Errorf("a fresh view over the store read sequence %d, want the %d written", got, uint64(written))
	}

	if got, err = sequenceOf(ctx, fresh, namespace(2)); err != nil || got != 0 {
		return fmt.Errorf("a namespace that never had a sequence read %d (%v), want 0", got, err)
	}

	return nil
}

// Namespaces lists every namespace created on a remote store, one holding no
// key record included, and none created on another; and Load of each returns
// its own records.
func listing(ctx context.Context, s RecordStore[view]) error {
	storeA, storeB := [16]byte{0: 'a'}, [16]byte{0: 'b'}
	// IDs whose bytes sort differently from their creation order.
	onA := []entity.NamespaceID{namespace(3), namespace(1), {0: 0xff, 15: 2}}
	onB := []entity.NamespaceID{namespace(2)}
	empty := namespace(4) // on storeA, holding no record

	for _, c := range []struct {
		ids   []entity.NamespaceID
		store [16]byte
	}{{onA, storeA}, {onB, storeB}, {[]entity.NamespaceID{empty}, storeA}} {
		for _, ns := range c.ids {
			if err := s.AddNamespace(ctx, ns, c.store); err != nil {
				return fmt.Errorf("creating namespace %v on store %x: %w", ns, c.store, err)
			}
		}
	}

	v := s.View()

	own := func(ns entity.NamespaceID) record { return record{"data-key", ns.String(), "current", ns[:]} }

	for _, ns := range append(slices.Clone(onA), onB...) {
		if err := v.Update(ctx, ns, writing(own(ns))); err != nil {
			return fmt.Errorf("Update of namespace %v: %w", ns, err)
		}
	}

	sortIDs := func(ids []entity.NamespaceID) []entity.NamespaceID {
		ids = slices.Clone(ids)
		slices.SortFunc(ids, func(a, b entity.NamespaceID) int { return bytes.Compare(a[:], b[:]) })
		return ids
	}

	for _, c := range []struct {
		store [16]byte
		want  []entity.NamespaceID
	}{{storeA, sortIDs(append(slices.Clone(onA), empty))}, {storeB, onB}, {[16]byte{0: 'c'}, nil}} {
		got, err := v.Namespaces(ctx, c.store)
		if err != nil {
			return fmt.Errorf("Namespaces(%x): %w", c.store, err)
		}

		if !slices.Equal(sortIDs(got), c.want) {
			return fmt.Errorf("Namespaces(%x) = %v, want %v in any order", c.store, got, c.want)
		}

		for _, ns := range got {
			rows, err := v.Load(ctx, ns)
			if err != nil {
				return fmt.Errorf("Load(namespace %v): %w", ns, err)
			}

			var want []record
			if ns != empty {
				want = []record{own(ns)}
			}

			if !sameRows(rows, want) {
				return fmt.Errorf("Load(namespace %v) listed on store %x = %q, want %q", ns, c.store, sorted(rows), want)
			}
		}
	}

	return nil
}
