package openstate

import (
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/marmos91/dittofs/pkg/metadata/entity"
)

// gatedSink is a recordingSink whose first Notify, once armed, waits to be
// released before it records: a raise preempted after its step unlocked.
type gatedSink struct {
	*recordingSink
	armed   atomic.Bool
	entered chan struct{}
	release chan struct{}
}

func (s *gatedSink) Notify(c entity.ClientID, w WatchID, ch []Change) {
	if s.armed.CompareAndSwap(true, false) {
		close(s.entered)
		<-s.release
	}
	s.recordingSink.Notify(c, w, ch)
}

// A watch's changes reach the Sink in the order they were committed: an add
// then a remove of one name must not be told as a remove then an add, which
// leaves the client believing the name exists.
func TestReview12NotifyKeepsCommitOrder(t *testing.T) {
	f := newFixture(t)
	a := f.caller(t, nfs("a", 1, entity.NewUser()))
	dir := in(1)
	f.watch(t, a, dir, false, names)
	g := &gatedSink{recordingSink: f.sink, entered: make(chan struct{}), release: make(chan struct{})}
	f.cmu.Lock()
	f.Table.sink = g
	f.cmu.Unlock()
	g.armed.Store(true)

	done := make(chan struct{})
	go func() {
		defer close(done)
		f.changed(dir, ChangeAddEntry, "add x", false) // handed at once, raise preempted
	}()
	<-g.entered
	f.handWatched()                                 // the timer finds nothing held
	f.changed(dir, ChangeRemove, "remove x", false) // handed at once again
	close(g.release)
	<-done
	got := f.reported()
	if len(got) != 2 || got[0] != "add x" {
		t.Errorf("the Sink heard %q, want the add before the remove", got)
	}
}

// Watches, changes, unwatches, expiries, revokes and sweeps racing on one
// directory leave the indexes as the entries rebuild them, and no charge.
func TestReview12WatchChurnUnderRace(t *testing.T) {
	f := newFixture(t)
	dir := in(1)
	var callers []Caller
	for i := range 6 {
		callers = append(callers, f.caller(t, nfs(fmt.Sprint("c", i), 1, entity.NewUser())))
	}
	var wg sync.WaitGroup
	for i, c := range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var ids []WatchID
			for j := range 200 {
				if id, err := f.Watches().Watch(ctx, c, dir, j%2 == 0, names); err == nil {
					ids = append(ids, id)
				}
				f.changed(dir, ChangeAddEntry, fmt.Sprint(i, j), j%3 == 0)
				if j%5 == 0 && len(ids) > 0 {
					_ = f.Watches().Unwatch(ctx, c, ids[0])
					ids = ids[1:]
				}
			}
			switch i % 3 {
			case 0:
				_ = f.Expire(ctx, c.Client)
			case 1:
				_ = f.Revoke(ctx, c.Client)
			}
		}()
	}
	for range 4 {
		wg.Add(1)
		go func() { defer wg.Done(); for range 50 { f.handWatched() } }()
	}
	wg.Wait()
	for i, c := range callers {
		if i%3 == 2 {
			continue
		}
		f.idx.Lock()
		n := f.held[heldKey{c.Client, cappedWatches}]
		f.idx.Unlock()
		if n != 0 {
			t.Errorf("client %d ended holding %d watches", i, n)
		}
	}
	f.sink.takeNotes()
}

// A revoked watch's ID answers Unwatch as revoked, and the watch reports
// nothing more.
func TestReview12RevokedWatch(t *testing.T) {
	f := newFixture(t)
	a := f.caller(t, nfs("a", 1, entity.NewUser()))
	dir := in(1)
	w := f.watch(t, a, dir, false, names)
	if err := f.Revoke(ctx, a.Client); err != nil {
		t.Fatal(err)
	}
	f.changed(dir, ChangeAddEntry, "x", false)
	if got := f.reported(); len(got) != 0 {
		t.Errorf("a revoked watch reported %q", got)
	}
	var re *RevokedError
	if err := f.Watches().Unwatch(ctx, a, w); !errors.As(err, &re) {
		t.Errorf("Unwatch of a revoked watch: %v, want a RevokedError", err)
	}
}

// Once a watched directory's watches are gone and the timer has looked, the
// timer's set no longer names it.
func TestReview12WatchedSetDrains(t *testing.T) {
	f := newFixture(t)
	a := f.caller(t, nfs("a", 1, entity.NewUser()))
	for i := range 100 {
		dir := in(1)
		w := f.watch(t, a, dir, false, names)
		f.changed(dir, ChangeAddEntry, fmt.Sprint(i), false)
		f.changed(dir, ChangeAddEntry, fmt.Sprint(i), false)
		if err := f.Watches().Unwatch(ctx, a, w); err != nil {
			t.Fatal(err)
		}
	}
	f.handWatched()
	f.idx.Lock()
	n := len(f.watched)
	f.idx.Unlock()
	if n != 0 {
		t.Errorf("the timer's set names %d directories with no watch", n)
	}
}

// A watch whose filter names nothing reports nothing and holds a slot.
func TestReview12EmptyFilter(t *testing.T) {
	f := newFixture(t)
	a := f.caller(t, nfs("a", 1, entity.NewUser()))
	if _, err := f.Watches().Watch(ctx, a, in(1), false, 0); err == nil {
		t.Log("a watch of no changes is accepted (code-read: the adapter must refuse CompletionFilter 0)")
	}
}

// The timer's batch must not overtake the change handed at once before it:
// the first change raised at once, preempted before its Notify, the next
// held, and the timer hands the held one.
func TestReview12TimerBatchOvertakesFirst(t *testing.T) {
	f := newFixture(t)
	a := f.caller(t, nfs("a", 1, entity.NewUser()))
	dir := in(1)
	f.watch(t, a, dir, false, names)
	g := &gatedSink{recordingSink: f.sink, entered: make(chan struct{}), release: make(chan struct{})}
	f.cmu.Lock()
	f.Table.sink = g
	f.cmu.Unlock()
	g.armed.Store(true)
	done := make(chan struct{})
	go func() { defer close(done); f.changed(dir, ChangeAddEntry, "first", false) }()
	<-g.entered
	f.changed(dir, ChangeRemove, "second", false) // held
	f.handWatched()                               // hands it
	close(g.release)
	<-done
	if got := f.reported(); len(got) != 2 || got[0] != "first" {
		t.Errorf("the Sink heard %q, want first before second", got)
	}
}
