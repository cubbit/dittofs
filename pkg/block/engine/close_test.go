package engine_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"maps"
	"runtime"
	"runtime/pprof"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/block/engine"
	"github.com/cubbit/dittofs/pkg/block/engine/enginetest"
	"github.com/cubbit/dittofs/pkg/block/journal"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/metadata/kvstore"
	"github.com/cubbit/dittofs/pkg/vfs/vfserr"
)

// faultDeps hands the unit's lifecycle view to fl, which the unit then calls.
func faultDeps(fl *faultLifecycle) func(*engine.Deps) {
	return func(d *engine.Deps) {
		fl.Lifecycle = d.Lifecycle
		d.Lifecycle = fl
	}
}

// closing runs Close in a goroutine and answers its result.
func closing(e *engine.Engine, ctx context.Context) <-chan error {
	out := make(chan error, 1)

	go func() {
		out <- e.Close(ctx)
	}()

	return out
}

// seam answers a channel closed the first time the seam it is given to fires.
func seam(set func(func())) <-chan struct{} {
	var once sync.Once

	fired := make(chan struct{})

	set(func() {
		once.Do(func() {
			close(fired)
		})
	})

	return fired
}

// heldBind is a Bind waiting in the store call after its share's handle was
// opened.
type heldBind struct {
	g    *gate
	done chan error
}

// holdBind starts a Bind of s that waits in the lifecycle fl until the
// returned gate opens, and returns once it waits.
func holdBind(t *testing.T, e *engine.Engine, fl *faultLifecycle, s engine.Share) *heldBind {
	t.Helper()

	h := &heldBind{
		g:    newGate(t),
		done: make(chan error, 1),
	}
	finished := make(chan struct{})

	fl.gate.Store(h.g)

	go func() {
		defer close(finished)

		h.done <- e.Bind(context.Background(), s)
	}()

	// Registered after the gate's, so it runs first: it releases the Bind and
	// waits for it, so no goroutine outlives the test.
	t.Cleanup(func() {
		h.g.open()
		<-finished
	})

	select {
	case <-h.g.entered:
	case err := <-h.done:
		t.Fatalf("the Bind ended (%v) before it reached the store", err)
	}

	return h
}

// joinOf answers a channel closed the first time Close has to wait for what.
func joinOf(e *engine.Engine, what string) <-chan struct{} {
	var once sync.Once

	fired := make(chan struct{})

	e.OnJoining(func(got string) {
		if got == what {
			once.Do(func() {
				close(fired)
			})
		}
	})

	return fired
}

// reached waits for the seam fired, or fails the test if Close returns first.
func reached(t *testing.T, fired <-chan struct{}, closed <-chan error) {
	t.Helper()

	select {
	case <-fired:
	case err := <-closed:
		t.Fatalf("Close returned %v before it had to wait", err)
	}
}

// stays fails the test if Close returns, or closes anything recorded in ev,
// within a short real wait: Close has reached a join and what it waits for is
// still running, so a join removed from Close shows here and not as a Close
// that merely finished a moment later.
func stays(t *testing.T, closed <-chan error, ev *events) {
	t.Helper()

	select {
	case err := <-closed:
		t.Fatalf("Close = %v while what it waits for still runs", err)
	case <-time.After(100 * time.Millisecond):
	}

	if got := ev.list(); len(got) != 0 {
		t.Fatalf("closes %v under work that still runs", got)
	}
}

// cloneEnded waits for a clone's answer, which Close's stop of the removal
// loop the clone runs under makes ErrUnavailable.
func cloneEnded(t *testing.T, answer <-chan error) {
	t.Helper()

	if err := <-answer; !unavailable(err) {
		t.Errorf("Clone = %v, want ErrUnavailable: the stop ended its loop", err)
	}
}

// unavailable reports whether err is ErrUnavailable.
func unavailable(err error) bool {
	return errors.Is(err, vfserr.ErrUnavailable)
}

// loopHold is a removal item held in its batch: it ignores its context and
// ends when both its holds are open.
type loopHold struct {
	entered chan struct{}
	first   chan struct{}
	second  chan struct{}
	opened  [2]sync.Once
	started sync.Once
}

func newLoopHold() *loopHold {
	return &loopHold{
		entered: make(chan struct{}),
		first:   make(chan struct{}),
		second:  make(chan struct{}),
	}
}

// run is the item: it records that it ended in ev.
func (h *loopHold) run(ev *events) func(context.Context) {
	return func(context.Context) {
		h.started.Do(func() {
			close(h.entered)
		})

		<-h.first
		<-h.second

		ev.add("loop item ended")
	}
}

// openFirst lets the item go on to its second hold.
func (h *loopHold) openFirst() {
	h.opened[0].Do(func() {
		close(h.first)
	})
}

// openSecond lets the item end.
func (h *loopHold) openSecond() {
	h.opened[1].Do(func() {
		close(h.second)
	})
}

// openAll lets the item end.
func (h *loopHold) openAll() {
	h.openFirst()
	h.openSecond()
}

// handleClose is the event a handle of tag's close records.
func handleClose(s engine.Share) string {
	return fmt.Sprintf("handle close %d", s.Tag)
}

// Every call is refused with ErrUnavailable from the first step of Close,
// while it still waits for a lifecycle call in progress, and no version is
// drawn after it.
func TestCloseRefusesEveryCallFromItsFirstStep(t *testing.T) {
	fl := &faultLifecycle{}
	u := newUnit(t, faultDeps(fl))
	f := u.create(t, "a")
	b := enginetest.AddShare(t, u.fs.Store, "b", 1<<30)
	c := enginetest.AddShare(t, u.fs.Store, "c", 1<<30)
	held := holdBind(t, u.e, fl, b)
	drained := seam(u.e.OnDraining)
	closed := closing(u.e, context.Background())

	reached(t, drained, closed)

	next := u.j.Stats().NextVersion
	ctx := t.Context()
	s := u.share.ID
	opts := engine.WriteOpts{Done: func() {}}

	_, writeErr := u.e.Write(ctx, s, f, 0, []byte("x"), opts)
	_, readErr := u.e.Read(ctx, s, f, 0, make([]byte, 1))
	_, _, commitErr := u.e.Commit(ctx, s, f, 0, 0)
	_, _, overlayErr := u.e.Overlay(ctx, s, f)
	_, allocationErr := u.e.Allocation(ctx, s, f, 0)
	_, drawErr := u.e.Draw(ctx, s, f)
	_, deleteErr := u.e.Delete(ctx, s, f)
	cloneErr := u.e.Clone(ctx, s, f, s, f, vfserr.Extent{Len: 1})

	for name, err := range map[string]error{
		"Write":      writeErr,
		"Read":       readErr,
		"Truncate":   u.e.Truncate(ctx, s, f, 0),
		"Deallocate": u.e.Deallocate(ctx, s, f, 0, 1),
		"Delete":     deleteErr,
		"Commit":     commitErr,
		"Clone":      cloneErr,
		"Draw":       drawErr,
		"Overlay":    overlayErr,
		"Allocation": allocationErr,
		"Bind new":   u.e.Bind(ctx, c),
		"Bind state": u.e.Bind(ctx, u.share),
	} {
		if !unavailable(err) {
			t.Errorf("%s while Close waits = %v, want ErrUnavailable", name, err)
		}
	}

	if got := u.j.Stats().NextVersion; got != next {
		t.Errorf("next version %v after the refused calls, want %v: a call drew one", got, next)
	}

	held.g.open()

	if err := <-closed; err != nil {
		t.Errorf("Close = %v, want nil", err)
	}
}

// A Bind held in the store when Close begins is refused at its publish, and
// Close waits for it: its handle is closed before the journal, and no share
// is left served.
func TestABindHeldInShareIsNotPublishedAfterCloseBegan(t *testing.T) {
	fl := &faultLifecycle{}
	u := newUnitWith(t, unitConfig{unbound: true, deps: []func(*engine.Deps){faultDeps(fl)}})
	b := enginetest.AddShare(t, u.fs.Store, "b", 1<<30)
	held := holdBind(t, u.e, fl, b)
	drained := seam(u.e.OnDraining)
	closed := closing(u.e, context.Background())

	reached(t, drained, closed)
	held.g.open()

	if err := <-held.done; !unavailable(err) {
		t.Errorf("the Bind = %v, want ErrUnavailable", err)
	}

	if err := <-closed; err != nil {
		t.Errorf("Close = %v, want nil", err)
	}

	if u.e.Served(b.ID) {
		t.Error("the share is served after Close")
	}

	want := []string{handleClose(b), "journal close"}
	if got := u.ev.list(); !slices.Equal(got, want) {
		t.Errorf("closes %v, want %v", got, want)
	}
}

// Handles close newest first, and every handle before the journal.
func TestCloseClosesEveryHandleBeforeEveryJournalInReverseOpenOrder(t *testing.T) {
	u := newUnitWith(t, unitConfig{unbound: true})
	shares := []engine.Share{
		enginetest.AddShare(t, u.fs.Store, "a", 1<<30),
		enginetest.AddShare(t, u.fs.Store, "b", 1<<30),
		enginetest.AddShare(t, u.fs.Store, "c", 1<<30),
	}

	for _, s := range shares {
		if err := u.e.Bind(t.Context(), s); err != nil {
			t.Fatal(err)
		}
	}

	if err := u.e.Close(t.Context()); err != nil {
		t.Fatalf("Close = %v, want nil", err)
	}

	want := []string{
		handleClose(shares[2]),
		handleClose(shares[1]),
		handleClose(shares[0]),
		"journal close",
	}
	if got := u.ev.list(); !slices.Equal(got, want) {
		t.Errorf("closes %v, want %v", got, want)
	}
}

// heldClose holds a handle's Close until release is closed.
type heldClose struct {
	engine.Handle
	entered chan struct{}
	release chan struct{}
	once    sync.Once
	opened  sync.Once
}

// open lets the held Close go, once.
func (h *heldClose) open() {
	h.opened.Do(func() {
		close(h.release)
	})
}

func (h *heldClose) Close() error {
	h.once.Do(func() {
		close(h.entered)
	})

	<-h.release

	return h.Handle.Close()
}

// A Bind that failed after it opened its handle is a lifecycle call in
// progress while that handle closes: Close waits for it, so the journal is
// never closed under the handle's Close.
func TestAFailedBindsHandleCloseIsJoinedByClose(t *testing.T) {
	fl := &faultLifecycle{raiseErr: errInjected}
	hc := &heldClose{entered: make(chan struct{}), release: make(chan struct{})}
	u := newUnitWith(t, unitConfig{
		unbound: true,
		deps:    []func(*engine.Deps){faultDeps(fl)},
		wrap: func(h engine.Handle) engine.Handle {
			hc.Handle = h

			return hc
		},
	})
	b := enginetest.AddShare(t, u.fs.Store, "b", 1<<30)
	bound := make(chan error, 1)
	finished := make(chan struct{})

	t.Cleanup(func() {
		hc.open()
		<-finished
	})

	go func() {
		defer close(finished)

		bound <- u.e.Bind(context.Background(), b)
	}()

	<-hc.entered

	drained := seam(u.e.OnDraining)
	closed := closing(u.e, context.Background())

	reached(t, drained, closed)

	if got := u.ev.list(); len(got) != 0 {
		t.Errorf("closes %v while the handle's Close is held, want none", got)
	}

	hc.open()

	if err := <-bound; !unavailable(err) {
		t.Errorf("the failed Bind = %v, want ErrUnavailable", err)
	}

	if err := <-closed; err != nil {
		t.Errorf("Close = %v, want nil", err)
	}

	want := []string{handleClose(b), "journal close"}
	if got := u.ev.list(); !slices.Equal(got, want) {
		t.Errorf("closes %v, want %v", got, want)
	}
}

// A Close after one whose handle failed to close answers the same error, and
// closes nothing twice.
func TestALaterCloseAnswersTheErrorOfTheFirst(t *testing.T) {
	u := newUnitWith(t, unitConfig{unbound: true})
	u.rec.closeErr = errClose
	b := enginetest.AddShare(t, u.fs.Store, "b", 1<<30)

	if err := u.e.Bind(t.Context(), b); err != nil {
		t.Fatal(err)
	}

	first := u.e.Close(t.Context())
	if first == nil {
		t.Fatal("Close = nil, want the handle's close error")
	}

	if second := u.e.Close(t.Context()); second != first {
		t.Errorf("the second Close = %v, want the first's error %v", second, first)
	}

	want := []string{handleClose(b), "journal close"}
	if got := u.ev.list(); !slices.Equal(got, want) {
		t.Errorf("closes %v, want %v", got, want)
	}
}

// closeEvents are the closes the starter's fake journal logged.
func closeEvents(s *starter) []string {
	var out []string

	for _, c := range s.j.log() {
		if strings.HasPrefix(c, "close") {
			out = append(out, c)
		}
	}

	return out
}

// Close of an engine never started closes nothing; of a started one, closes
// each handle and the journal once, however often it is called; and a Start
// after Close opens nothing.
func TestCloseBeforeStartAndTwice(t *testing.T) {
	t.Run("never started", func(t *testing.T) {
		s := newStarter(t)

		if err := s.e.Close(t.Context()); err != nil {
			t.Fatalf("Close = %v, want nil", err)
		}

		if got := closeEvents(s); len(got) != 0 {
			t.Errorf("closes %v, want none", got)
		}
	})

	t.Run("twice", func(t *testing.T) {
		s := newStarter(t)

		if err := s.start(t); err != nil {
			t.Fatal(err)
		}

		opens := len(s.openCalls())

		for i := range 2 {
			if err := s.e.Close(t.Context()); err != nil {
				t.Fatalf("Close %d = %v, want nil", i+1, err)
			}
		}

		want := []string{
			fmt.Sprintf("close handle %d", s.two.Tag),
			fmt.Sprintf("close handle %d", s.one.Tag),
			"close journal",
		}
		if got := closeEvents(s); !slices.Equal(got, want) {
			t.Errorf("closes %v, want %v", got, want)
		}

		if err := s.start(t); !unavailable(err) {
			t.Errorf("Start after Close = %v, want ErrUnavailable", err)
		}

		if got := len(s.openCalls()); got != opens {
			t.Errorf("Start after Close opened %d journals, want none", got-opens)
		}
	})
}

// A Start that began before Close is waited for: Close closes what it opened
// and the group commits it created, and never the journal under it.
func TestCloseWaitsForAStartThatBeganBeforeIt(t *testing.T) {
	s := newStarter(t)
	g := newGate(t)

	s.mu.Lock()
	s.gate = g
	s.mu.Unlock()

	started := make(chan error, 1)

	go func() {
		started <- s.start(t)
	}()

	<-g.entered

	drained := seam(s.e.OnDraining)
	closed := closing(s.e, context.Background())

	reached(t, drained, closed)

	if got := closeEvents(s); len(got) != 0 {
		t.Errorf("closes %v while Start waits, want none", got)
	}

	g.open()

	if err := <-started; err != nil {
		t.Fatalf("Start = %v, want nil", err)
	}

	if err := <-closed; err != nil {
		t.Fatalf("Close = %v, want nil", err)
	}

	want := []string{
		fmt.Sprintf("close handle %d", s.two.Tag),
		fmt.Sprintf("close handle %d", s.one.Tag),
		"close journal",
	}
	if got := closeEvents(s); !slices.Equal(got, want) {
		t.Errorf("closes %v, want %v", got, want)
	}
}

// armedClock tells a test when Close has armed its default stop bound.
type armedClock struct {
	engine.Clock
	armed chan struct{}
}

func (c *armedClock) After(d time.Duration) <-chan struct{} {
	if d == engine.StopBoundDefault {
		select {
		case c.armed <- struct{}{}:
		default:
		}
	}

	return c.Clock.After(d)
}

// Two Close calls at the same time are one stop: the second waits for the
// first and answers its result, or its own error when its context ends first.
func TestTwoConcurrentCloseCallsReturnOneResult(t *testing.T) {
	newHeld := func(t *testing.T) (*unit, *armedClock, *heldBind, engine.Share) {
		t.Helper()

		fl := &faultLifecycle{}
		ac := &armedClock{armed: make(chan struct{}, 4)}
		u := newUnitWith(t, unitConfig{
			unbound: true,
			deps: []func(*engine.Deps){
				faultDeps(fl),
				func(d *engine.Deps) {
					ac.Clock = d.Clock
					d.Clock = ac
				},
			},
		})
		b := enginetest.AddShare(t, u.fs.Store, "b", 1<<30)

		return u, ac, holdBind(t, u.e, fl, b), b
	}

	t.Run("the second waits and answers the same result", func(t *testing.T) {
		u, _, held, b := newHeld(t)
		drained := seam(u.e.OnDraining)
		first := closing(u.e, context.Background())

		reached(t, drained, first)

		waiting := joinOf(u.e, "the other Close")
		second := make(chan error, 1)

		go func() {
			err := u.e.Close(context.Background())

			u.ev.add("B returned")

			second <- err
		}()

		select {
		case <-waiting:
		case err := <-second:
			t.Fatalf("the second Close returned %v before the first finished", err)
		}

		held.g.open()

		if err := <-first; err != nil {
			t.Errorf("the first Close = %v, want nil", err)
		}

		if err := <-second; err != nil {
			t.Errorf("the second Close = %v, want nil", err)
		}

		got := u.ev.list()
		if got[0] != handleClose(b) || got[1] != "journal close" ||
			!slices.Contains(got[2:], "B returned") {
			t.Errorf("events %v, want the Bind's handle, the journal, then B returned", got)
		}

		if len(got) != 3 {
			t.Errorf("events %v: a component closed twice", got)
		}
	})

	t.Run("a first Close past its bound is the second's result", func(t *testing.T) {
		u, ac, _, _ := newHeld(t)
		drained := seam(u.e.OnDraining)
		first := closing(u.e, context.Background())

		reached(t, drained, first)
		<-ac.armed

		waiting := joinOf(u.e, "the other Close")

		// A deadline an hour away arms no default timer of its own, so the
		// second Close ends only with the first.
		ctx, cancel := context.WithTimeout(t.Context(), time.Hour)

		defer cancel()

		second := closing(u.e, ctx)

		select {
		case <-waiting:
		case err := <-second:
			t.Fatalf("the second Close returned %v before the first finished", err)
		}

		u.clock.Advance(engine.StopBoundDefault)

		errFirst := <-first
		errSecond := <-second

		if !unavailable(errFirst) {
			t.Fatalf("the first Close = %v, want ErrUnavailable", errFirst)
		}

		if errSecond != errFirst {
			t.Errorf("the second Close = %v, want the first's error %v", errSecond, errFirst)
		}

		if got := u.ev.list(); len(got) != 0 {
			t.Errorf("closes %v after a Close past its bound, want none", got)
		}
	})

	t.Run("the second's own context ends first", func(t *testing.T) {
		u, _, held, _ := newHeld(t)
		drained := seam(u.e.OnDraining)
		first := closing(u.e, context.Background())

		reached(t, drained, first)

		past, cancel := context.WithDeadline(t.Context(), time.Now().Add(-time.Second))

		defer cancel()

		err := u.e.Close(past)
		if !unavailable(err) ||
			!strings.Contains(err.Error(), "while waiting for the other Close") {
			t.Errorf("the second Close = %v, want its own ErrUnavailable", err)
		}

		held.g.open()

		if err := <-first; err != nil {
			t.Errorf("the first Close = %v, want nil", err)
		}
	})
}

// With a deadline the stop bound is the context's alone: a caller who gave
// 120 s is not cut at the 30 s default.
func TestACloseWithA120sDeadlineKeepsWaitingPastTheDefault(t *testing.T) {
	u := newUnit(t)
	h := newLoopHold()

	t.Cleanup(h.openAll)

	if err := u.e.EnqueueHeld(u.share.ID, h.run(u.ev), nil); err != nil {
		t.Fatal(err)
	}

	<-h.entered

	ctx, cancel := context.WithTimeout(t.Context(), 120*time.Second)

	defer cancel()

	waiting := joinOf(u.e, "a removal loop")
	closed := closing(u.e, ctx)

	reached(t, waiting, closed)
	u.clock.Advance(31 * time.Second)
	h.openAll()

	if err := <-closed; err != nil {
		t.Errorf("Close = %v, want nil: the default bound cut a 120 s deadline", err)
	}

	want := []string{"loop item ended", handleClose(u.share), "journal close"}
	if got := u.ev.list(); !slices.Equal(got, want) {
		t.Errorf("events %v, want %v", got, want)
	}
}

// A removal batch held when a Close whose deadline passed begins: it gives up
// at the loop's join and leaves the handle and the journal open, and a later
// Close joins the loop again before it closes anything. This twin runs over
// the real store; the bubble twin is in close_bubble_test.go.
func TestACloseHeldInARemovalBatchPastTheDeadlineOverTheRealStore(t *testing.T) {
	u := newUnit(t)
	h := newLoopHold()

	t.Cleanup(h.openAll)

	if err := u.e.EnqueueHeld(u.share.ID, h.run(u.ev), nil); err != nil {
		t.Fatal(err)
	}

	<-h.entered

	var joins atomic.Int32

	u.e.OnJoining(func(what string) {
		if what == "a removal loop" && joins.Add(1) == 2 {
			h.openSecond()
		}
	})

	u.ev.onAdd = func(what string) {
		if strings.HasPrefix(what, "handle close") {
			h.openSecond()
		}
	}

	past, cancel := context.WithDeadline(t.Context(), time.Now().Add(-time.Second))

	defer cancel()

	err := u.e.Close(past)
	if !unavailable(err) || !strings.Contains(err.Error(), "removal loop") {
		t.Errorf("Close = %v, want ErrUnavailable naming the removal loop", err)
	}

	if got := u.ev.list(); len(got) != 0 {
		t.Errorf("closes %v after a Close that gave up, want none", got)
	}

	h.openFirst()

	if err := u.e.Close(context.Background()); err != nil {
		t.Errorf("the second Close = %v, want nil", err)
	}

	want := []string{"loop item ended", handleClose(u.share), "journal close"}
	if got := u.ev.list(); !slices.Equal(got, want) {
		t.Errorf("events %v, want %v", got, want)
	}
}

// Without a deadline the stop bound is 30 s of the engine's clock: 29 s is
// still waiting, 30 s is the bound. This twin runs over the real store.
func TestACloseWithNoDeadlineGivesUpAtThirtySecondsOverTheRealStore(t *testing.T) {
	hold := func(t *testing.T) (*unit, *loopHold) {
		t.Helper()

		u := newUnit(t)
		h := newLoopHold()

		t.Cleanup(h.openAll)

		if err := u.e.EnqueueHeld(u.share.ID, h.run(u.ev), nil); err != nil {
			t.Fatal(err)
		}

		<-h.entered

		return u, h
	}

	t.Run("29 s is still waiting", func(t *testing.T) {
		u, h := hold(t)
		waiting := joinOf(u.e, "a removal loop")
		closed := closing(u.e, context.Background())

		reached(t, waiting, closed)
		u.clock.Advance(29 * time.Second)
		h.openAll()

		if err := <-closed; err != nil {
			t.Errorf("Close = %v after 29 s, want nil", err)
		}

		want := []string{"loop item ended", handleClose(u.share), "journal close"}
		if got := u.ev.list(); !slices.Equal(got, want) {
			t.Errorf("events %v, want %v", got, want)
		}
	})

	t.Run("30 s is the bound", func(t *testing.T) {
		u, h := hold(t)
		waiting := joinOf(u.e, "a removal loop")
		closed := closing(u.e, context.Background())

		reached(t, waiting, closed)
		u.clock.Advance(30 * time.Second)

		if err := <-closed; !unavailable(err) {
			t.Errorf("Close = %v after 30 s, want ErrUnavailable", err)
		}

		if got := u.ev.list(); len(got) != 0 {
			t.Errorf("closes %v after a Close that gave up, want none", got)
		}

		h.openAll()
	})
}

// A Close waits for a clone goroutine that ignores its context, and gives up
// at the stop bound leaving the journal and the handle open.
func TestCloseJoinsACloneGoroutineThatIgnoresItsContext(t *testing.T) {
	t.Run("past the bound", func(t *testing.T) {
		u := clonedUnit(t)
		src := u.committedFile(t, "src", fill(mib, 's'))
		dst := u.create(t, "dst")
		release, answer := u.heldBeforePhase1(t, src, dst, vfserr.Extent{Len: mib})
		waiting := joinOf(u.e, "a clone goroutine")
		closed := closing(u.e, context.Background())

		reached(t, waiting, closed)
		stays(t, closed, u.ev)
		u.clock.Advance(engine.StopBoundDefault)

		err := <-closed
		if !unavailable(err) || !strings.Contains(err.Error(), "clone goroutine") {
			t.Errorf("Close = %v, want ErrUnavailable naming the clone goroutine", err)
		}

		if got := u.ev.list(); len(got) != 0 {
			t.Errorf("closes %v while the clone goroutine runs, want none", got)
		}

		release()
		cloneEnded(t, answer)
	})

	t.Run("released", func(t *testing.T) {
		u := clonedUnit(t)
		src := u.committedFile(t, "src", fill(mib, 's'))
		dst := u.create(t, "dst")
		release, answer := u.heldBeforePhase1(t, src, dst, vfserr.Extent{Len: mib})
		waiting := joinOf(u.e, "a clone goroutine")
		closed := closing(u.e, context.Background())

		reached(t, waiting, closed)
		stays(t, closed, u.ev)
		release()
		cloneEnded(t, answer)

		if err := <-closed; err != nil {
			t.Errorf("Close = %v, want nil", err)
		}

		want := []string{handleClose(u.share), "journal close"}
		if got := u.ev.list(); !slices.Equal(got, want) {
			t.Errorf("events %v, want %v", got, want)
		}
	})
}

// A batch in its store commit when Close begins is waited for: the handle and
// the journal close only after it returned.
func TestCloseWaitsForAGroupCommitBatchInItsStoreCommit(t *testing.T) {
	closeOverAHeldBatch(t, false)
}

// The same when the journal turned read-only after the batch began its commit:
// the last group commit is skipped, so the join of the group commit's
// goroutine alone keeps the handle and the journal open until the batch
// returns.
func TestCloseWaitsForAGroupCommitBatchOfAReadOnlyJournal(t *testing.T) {
	closeOverAHeldBatch(t, true)
}

// closeOverAHeldBatch holds a batch in its store commit, turns the journal
// read-only when readOnly, and closes the engine.
func closeOverAHeldBatch(t *testing.T, readOnly bool) {
	t.Helper()

	entered := make(chan struct{})
	release := make(chan struct{})

	var (
		u    *unit
		once sync.Once
		open sync.Once
	)

	u = newUnit(t, onCommit(func(
		ctx context.Context,
		next engine.Existence,
		p []kvstore.PendingExistence,
		marks *kvstore.MarksWrite,
	) (map[entity.FileID]error, error) {
		once.Do(func() {
			close(entered)
		})

		<-release
		u.ev.add("batch commit returned")

		return next.Commit(ctx, p, marks)
	}))
	f := u.create(t, "a")

	u.write(t, f, 0, []byte("abc"))

	finished := make(chan struct{})

	t.Cleanup(func() {
		open.Do(func() {
			close(release)
		})
		<-finished
	})

	go func() {
		defer close(finished)

		_ = u.e.CommitNow(context.Background(), u.share.ID, f)
	}()

	<-entered

	join := "the last group commit"

	if readOnly {
		u.j.ReadOnly(syscall.EROFS)

		join = "a group commit"
	}

	waiting := joinOf(u.e, join)
	closed := closing(u.e, context.Background())

	reached(t, waiting, closed)
	open.Do(func() {
		close(release)
	})

	if err := <-closed; err != nil {
		t.Errorf("Close = %v, want nil", err)
	}

	want := []string{"batch commit returned", handleClose(u.share), "journal close"}
	if got := u.ev.list(); !slices.Equal(got, want) {
		t.Errorf("events %v, want %v", got, want)
	}
}

// A clean Close commits every file's pending existence, never forgets a
// share's tag, and leaves the next start nothing to re-apply.
func TestACleanCloseNeverForgetsAndLeavesNothingToReapply(t *testing.T) {
	r := newRig(t)
	r.begin(t)

	ctx := t.Context()
	_, f := r.newFile(t, "a")
	want := []byte("synced")
	opts := engine.WriteOpts{Done: func() {}}

	if _, err := r.e.Write(ctx, r.one.ID, f, 0, want, opts); err != nil {
		t.Fatal(err)
	}

	if _, _, err := r.e.Commit(ctx, r.one.ID, f, 0, 0); err != nil {
		t.Fatal(err)
	}

	version := entity.JournalVersion(r.e.Pending(f)[0].Version.Bytes())

	if err := r.e.Close(ctx); err != nil {
		t.Fatalf("Close = %v, want nil", err)
	}

	if got := r.calls(0, "forget"); len(got) != 0 {
		t.Errorf("Close forgot %v, want nothing", got)
	}

	if _, ok := r.j.Stats().Shares[r.one.Tag]; !ok {
		t.Errorf("the journal no longer lists tag %d after a clean Close", r.one.Tag)
	}

	if got := r.applied(t, f); got != version {
		t.Errorf("applied %v after Close, want the write's %v", got, version)
	}

	r.restart(t)

	mark := len(r.j.log())

	r.atRestart()

	if err := r.start(t); err != nil {
		t.Fatalf("Start after a clean Close = %v", err)
	}

	if got := r.calls(mark, "commit"); len(got) != 0 {
		t.Errorf("the restart re-applied %v, want nothing", got)
	}

	got := make([]byte, len(want))
	if _, err := r.e.Read(ctx, r.one.ID, f, 0, got); err != nil || !bytes.Equal(got, want) {
		t.Errorf("read %q, %v; want %q", got, err, want)
	}
}

// A release whose retry the stop cut short answers ErrUnavailable and runs no
// Done; its file stays out of the last group commit, which does not wait for
// the guard the release keeps; and the next start finishes it.
func TestACloseCutsAReleasesRetryWithUnavailableAndLeavesTheMarker(t *testing.T) {
	r := newRig(t)
	r.begin(t)

	ctx := t.Context()
	res := enginetest.NewReservations(t)
	_, f := r.newFile(t, "a")
	o, answered := res.Opts()
	_, err := r.e.Write(ctx, r.one.ID, f, 0, fill(8, 'a'), o)

	answered(err)

	names := r.fs.Store.Namespace()

	if _, err := names.Unlink(ctx, r.fs.Admin, r.fs.Root, []byte("a")); err != nil {
		t.Fatal(err)
	}

	r.ex.mu.Lock()
	r.ex.removeErr = kvstore.ErrConflict
	r.ex.mu.Unlock()

	retry, err := r.e.Delete(ctx, r.one.ID, f)
	if retry == nil || err != nil {
		t.Fatalf("Delete = %v, %v; want a pending channel and nil", retry, err)
	}

	before := res.Outstanding()

	if err := r.e.Close(ctx); err != nil {
		t.Fatalf("Close = %v, want nil", err)
	}

	select {
	case got := <-retry:
		if !unavailable(got) {
			t.Errorf("the release yields %v, want ErrUnavailable", got)
		}
	default:
		t.Error("the release's channel yields nothing after Close")
	}

	select {
	case extra := <-retry:
		t.Errorf("the release yields a second value %v", extra)
	default:
	}

	if got := res.Outstanding(); got != before {
		t.Errorf("%d writes without their Done after Close, want %d: a Done ran", got, before)
	}

	if got := r.applied(t, f); !got.IsZero() {
		t.Errorf("applied %v after Close, want zero: the last group commit committed the file", got)
	}

	r.ex.mu.Lock()
	r.ex.removeErr = nil
	r.ex.mu.Unlock()
	r.restart(t)

	mark := len(r.j.log())

	r.atRestart()

	if err := r.start(t); err != nil {
		t.Fatalf("Start = %v", err)
	}

	if got, want := r.calls(mark, "remove"), []string{"remove " + fname(f)}; !slices.Equal(
		got,
		want,
	) {
		t.Errorf("the restart removed %v, want %v", got, want)
	}

	if _, err := r.fs.Store.Existence().Size(ctx, r.one.ID, f); !errors.Is(err, entity.ErrStale) {
		t.Errorf("the released file after the restart: %v, want it gone", err)
	}
}

// reserveHold holds the first copy write of a clone.
type reserveHold struct {
	entered chan struct{}
	release chan struct{}
	once    sync.Once
	opened  sync.Once
}

func newReserveHold() *reserveHold {
	return &reserveHold{
		entered: make(chan struct{}),
		release: make(chan struct{}),
	}
}

// open lets the held write go, once.
func (h *reserveHold) open() {
	h.opened.Do(func() {
		close(h.release)
	})
}

// holdingHandle hands out reservations whose writes the hold holds.
type holdingHandle struct {
	engine.Handle
	hold *reserveHold
}

func (h *holdingHandle) Reserve(n int64, writes int) (engine.Reservation, error) {
	r, err := h.Handle.Reserve(n, writes)
	if err != nil {
		return nil, err
	}

	return &holdingReservation{Reservation: r, hold: h.hold}, nil
}

type holdingReservation struct {
	engine.Reservation
	hold *reserveHold
}

func (r *holdingReservation) WriteAt(
	ctx context.Context,
	id journal.FileID,
	off int64,
	p []byte,
	mtime int64,
) (journal.Version, error) {
	r.hold.once.Do(func() {
		close(r.hold.entered)
	})

	<-r.hold.release

	return r.Reservation.WriteAt(ctx, id, off, p, mtime)
}

// spyLifecycle hands the unit's lifecycle view to sl.
func spyLifecycle(sl *startLifecycle) func(*engine.Deps) {
	return func(d *engine.Deps) {
		sl.Lifecycle = d.Lifecycle
		d.Lifecycle = sl
	}
}

// A version drawn by loop work while Close joins it wakes the raise, which
// must run on a live context and be cancelled and joined only afterwards:
// the last group commit is held until the raise's store call is entered, so a
// raise cancelled before the work queue stops shows as a call on a cancelled
// context, or as none.
func TestTheRaiseWokenByLoopWorkDuringTheStopRunsAndIsJoinedAfterIt(t *testing.T) {
	var (
		u         *unit
		armed     atomic.Bool
		started0  int64
		hold      = newReserveHold()
		sl        = &startLifecycle{}
		entered   = make(chan struct{})
		exited    = make(chan struct{})
		enterOnce sync.Once
		exitOnce  sync.Once
	)

	u = clonedUnitWith(t, unitConfig{
		wrap: func(h engine.Handle) engine.Handle {
			return &holdingHandle{Handle: h, hold: hold}
		},
		deps: []func(*engine.Deps){
			spyLifecycle(sl),
			onCommit(func(
				ctx context.Context,
				next engine.Existence,
				p []kvstore.PendingExistence,
				marks *kvstore.MarksWrite,
			) (map[entity.FileID]error, error) {
				if armed.Load() && u.e.RaisesStarted() > started0 {
					select {
					case <-entered:
					case <-exited:
					}
				}

				return next.Commit(ctx, p, marks)
			}),
		},
	})
	t.Cleanup(hold.open)

	u.e.TrackRaisesForTest()
	u.e.OnRaiseExit(func() {
		exitOnce.Do(func() {
			close(exited)
		})
	})

	src := u.committedFile(t, "src", fill(mib, 's'))
	dst := u.create(t, "dst")
	w := u.create(t, "w")

	u.write(t, w, 0, fill(8, 'w'))

	next := u.j.Stats().NextVersion.Counter

	dueOnVersion(t, u, 2)

	block := 2 * (unitLimit - next - 1)
	calls0 := len(sl.raiseCalls())
	started0 = u.e.RaisesStarted()
	sl.onRaise = func(int) {
		enterOnce.Do(func() {
			close(entered)
		})
	}

	armed.Store(true)

	answer := u.startClone(context.Background(), src, dst, vfserr.Extent{Len: mib})

	<-hold.entered

	waiting := joinOf(u.e, "a clone goroutine")
	closed := closing(u.e, context.Background())

	reached(t, waiting, closed)
	hold.open()

	if err := <-closed; err != nil {
		t.Errorf("Close = %v, want nil", err)
	}

	cloneEnded(t, answer)

	calls := sl.raiseCalls()[calls0:]
	if len(calls) != 1 {
		t.Fatalf("%d raise calls, want exactly 1", len(calls))
	}

	if calls[0].ctxErr != nil {
		t.Errorf("the raise's store call began on a context that ended: %v", calls[0].ctxErr)
	}

	want := entity.JournalVersion(journal.Version{Counter: unitLimit + block}.Bytes())
	if got := calls[0].to[u.share.ID]; got != want {
		t.Errorf("the raise asked for the limit %v, want %v", got, want)
	}

	if n := u.e.RaisesAlive(); n != 0 {
		t.Errorf("%d raise goroutines alive after Close, want 0", n)
	}
}

// A raise waiting on the store when Close reaches it is cancelled before it
// is joined, and the join ends once it returns.
func TestCloseCancelsAndJoinsARaiseBlockedInTheStore(t *testing.T) {
	sl := &startLifecycle{}
	u := newUnit(t, spyLifecycle(sl))

	u.e.TrackRaisesForTest()

	entered := make(chan struct{})

	var (
		enterOnce sync.Once
		openOnce  sync.Once
	)

	sl.onRaise = func(int) {
		enterOnce.Do(func() {
			close(entered)
		})
	}
	sl.untilCancel = make(chan struct{})

	open := func() {
		openOnce.Do(func() {
			close(sl.untilCancel)
		})
	}

	t.Cleanup(open)
	dueOnVersion(t, u, 1)

	if _, err := u.e.Draw(t.Context(), u.share.ID, drawFile); err != nil {
		t.Fatal(err)
	}

	<-entered

	joined := joinOf(u.e, "a version limit raise")
	closed := closing(u.e, context.Background())

	reached(t, joined, closed)

	calls := sl.raiseCalls()
	if err := calls[len(calls)-1].ctx.Err(); !errors.Is(err, context.Canceled) {
		t.Errorf("the raise's context is %v when Close joins it, want cancelled", err)
	}

	stays(t, closed, u.ev)
	open()

	if err := <-closed; err != nil {
		t.Errorf("Close = %v, want nil", err)
	}

	if n := u.e.RaisesAlive(); n != 0 {
		t.Errorf("%d raise goroutines alive after Close, want 0", n)
	}
}

// A Bind that fails in flight while Close waits for it wakes the raise, as a
// failed Bind does; that raise runs on a live context, so the lifecycle calls
// are waited for before the raise is cancelled and joined.
func TestAFailedBindDuringCloseWakesARaiseThatCloseJoins(t *testing.T) {
	var (
		s         *starter
		started0  int64
		armed     atomic.Bool
		entered   = make(chan struct{})
		exited    = make(chan struct{})
		enterOnce sync.Once
		exitOnce  sync.Once
	)

	s = newRaiser(t, time.Second, withSpare(), withStartDeps(func(d *engine.Deps) {
		d.Existence = commitHook{d.Existence, func(
			ctx context.Context,
			next engine.Existence,
			p []kvstore.PendingExistence,
			marks *kvstore.MarksWrite,
		) (map[entity.FileID]error, error) {
			if armed.Load() && s.e.RaisesStarted() > started0 {
				select {
				case <-entered:
				case <-exited:
				}
			}

			return next.Commit(ctx, p, marks)
		}}
	}))
	s.e.OnRaiseExit(func() {
		exitOnce.Do(func() {
			close(exited)
		})
	})

	l0 := marks(s)[0].Counter

	setLimit(t, s, s.spare.ID, l0+3*testBlock)

	_, made := s.fs.Create(t, s.fs.Admin, s.fs.Root, "w", kvstore.Regular(0o644))
	opts := engine.WriteOpts{Done: func() {}}

	if _, err := s.e.Write(t.Context(), s.one.ID, made.ID, 0, []byte("w"), opts); err != nil {
		t.Fatal(err)
	}

	calls0 := len(s.lc.raiseCalls())
	started0 = s.e.RaisesStarted()
	g := newGate(t)

	s.lc.hold.Store(&heldRaise{g: g, share: s.spare.ID})
	s.lc.failRaises(errors.New("store down"))
	s.lc.onRaise = func(n int) {
		if n == calls0+2 {
			enterOnce.Do(func() {
				close(entered)
			})
		}
	}

	armed.Store(true)

	bound := bindInBackground(t, s, s.spare)

	waitForRaise(t, g)

	drained := seam(s.e.OnDraining)
	closed := closing(s.e, context.Background())

	reached(t, drained, closed)
	g.open()

	if err := <-bound; err == nil {
		t.Error("the Bind passed a failing limit write")
	}

	if err := <-closed; err != nil {
		t.Errorf("Close = %v, want nil", err)
	}

	calls := s.lc.raiseCalls()[calls0:]
	if len(calls) != 2 {
		t.Fatalf(
			"%d raise calls, want 2: the Bind's and the raise the failed Bind woke",
			len(calls),
		)
	}

	if calls[1].ctxErr != nil {
		t.Errorf("the woken raise began on a context that ended: %v", calls[1].ctxErr)
	}

	if n := s.e.RaisesAlive(); n != 0 {
		t.Errorf("%d raise goroutines alive after Close, want 0", n)
	}
}

// engineStacks are the frames of the goroutines the engine runs in the
// background.
var engineStacks = []string{
	"(*removalLoop).run",
	"(*groupCommit).run",
	"(*Engine).raiseLoop",
	"(*Engine).runClone",
}

// background is the engine's background goroutines alive now, by frame.
func background() []string {
	var buf bytes.Buffer

	_ = pprof.Lookup("goroutine").WriteTo(&buf, 1)

	var out []string

	for _, f := range engineStacks {
		if strings.Contains(buf.String(), f) {
			out = append(out, f)
		}
	}

	return out
}

// Every kind of background work the engine runs, once run, is gone when Close
// returns: no loop, group commit, raise or clone goroutine is left.
func TestCloseLeavesNoGoroutineAfterEveryKindOfBackgroundWork(t *testing.T) {
	u := clonedUnit(t)

	u.e.TrackRaisesForTest()

	src := u.committedFile(t, "src", fill(mib, 's'))
	dst := u.create(t, "dst")

	u.write(t, dst, 4*mib, fill(8, 'w'))
	u.clone(t, src, dst, vfserr.Extent{Len: mib})

	if err := u.e.Truncate(t.Context(), u.share.ID, src, 4); err != nil {
		t.Fatal(err)
	}

	dueOnVersion(t, u, 1)

	if _, err := u.e.Draw(t.Context(), u.share.ID, drawFile); err != nil {
		t.Fatal(err)
	}

	if err := u.e.Close(t.Context()); err != nil {
		t.Fatalf("Close = %v, want nil", err)
	}

	// A goroutine whose work is joined may still be returning from its last
	// frame; give the scheduler turns until it has, bounded.
	deadline := time.Now().Add(5 * time.Second)

	for left := background(); len(left) != 0; left = background() {
		if time.Now().After(deadline) {
			t.Fatalf("after Close, goroutines still run %v", left)
		}

		runtime.Gosched()
	}
}

// A source's Release that keeps failing after the clone was answered does not
// hold Close: the loop is stopped at once, and the one Release attempt the
// loop's end makes (under its own bounded context) is the last. Close returns
// nil and the clone goroutine is gone. The store
// recovers after 50 attempts only so that a retry that never ends cannot hang
// the test.
func TestCloseDoesNotWaitBehindAFailingReleaseOfACloneSource(t *testing.T) {
	h := &recHolds{}
	first := make(chan struct{})
	gate := make(chan struct{})

	var firstOnce, gateOnce sync.Once

	open := func() {
		gateOnce.Do(func() {
			close(gate)
		})
	}

	h.onRel = func(context.Context) error {
		_, n := h.counts()
		if n == 1 {
			firstOnce.Do(func() {
				close(first)
			})

			<-gate
		}

		if n > 50 {
			return nil
		}

		return errors.New("an injected release failure")
	}

	u := clonedUnit(t, func(d *engine.Deps) {
		d.Holds = h
	})
	src := u.committedFile(t, "src", fill(mib, 's'))
	dst := u.create(t, "dst")

	t.Cleanup(open)

	answer := u.startClone(context.Background(), src, dst, vfserr.Extent{Len: mib})

	if err := <-answer; err != nil {
		t.Fatalf("Clone = %v, want it answered before its Release", err)
	}

	<-first

	waiting := joinOf(u.e, "a removal loop")
	closed := closing(u.e, context.Background())

	reached(t, waiting, closed)
	open()

	if err := <-closed; err != nil {
		t.Errorf("Close = %v, want nil", err)
	}

	// The first attempt, which the loop was in when Close began, and the one
	// the loop's end makes.
	if _, n := h.counts(); n != 2 {
		t.Errorf("%d Release attempts when Close returned, want 2", n)
	}

	if left := background(); slices.Contains(left, "(*Engine).runClone") {
		t.Errorf("the clone goroutine still runs after Close: %v", left)
	}

	if h.released() != 0 {
		t.Errorf(
			"%d Releases answered nil, want none: the source stays held for the next start",
			h.released(),
		)
	}
}

// A Close of a read-only journal does not sync it: its pending existence is
// left to the next start, which re-applies exactly those writes, and the
// handles and the journal close in reverse order. Without the skip the last
// group commit waits for the stop bound, because the journal's syncs are
// refused.
func TestCloseOfAReadOnlyJournalLeavesItsPendingExistenceToTheNextStart(t *testing.T) {
	r := newRig(t)
	r.begin(t)

	ctx := t.Context()
	opts := engine.WriteOpts{Done: func() {}}
	pending := map[string]entity.JournalVersion{}

	for _, name := range []string{"a", "b"} {
		_, f := r.newFile(t, name)

		if _, err := r.e.Write(ctx, r.one.ID, f, 0, fill(8, 'a'), opts); err != nil {
			t.Fatal(err)
		}

		pending[fname(f)] = entity.JournalVersion(r.e.Pending(f)[0].Version.Bytes())
	}

	r.j.ro.Store(&journal.ReadOnlyState{ReadOnly: true, Err: syscall.EROFS})

	mark := len(r.j.log())
	waiting := joinOf(r.e, "the last group commit")
	closed := closing(r.e, ctx)

	var err error

	select {
	case err = <-closed:
	case <-waiting:
		r.clock.Advance(engine.StopBoundDefault)

		err = <-closed

		t.Error("Close waited for the last group commit of a read-only journal")
	}

	if err != nil {
		t.Errorf("Close = %v, want nil", err)
	}

	if got := r.calls(mark, "sync"); len(got) != 0 {
		t.Errorf("Close synced the read-only journal: %v", got)
	}

	want := []string{
		fmt.Sprintf("close handle %d", r.two.Tag),
		fmt.Sprintf("close handle %d", r.one.Tag),
		"close journal",
	}
	if got := closeEvents(r.starter); !slices.Equal(got, want) {
		t.Errorf("closes %v, want %v", got, want)
	}

	// The reopened journal writes again.
	r.j.ro.Store(nil)
	r.restart(t)

	mark = len(r.j.log())

	r.atRestart()

	if err := r.start(t); err != nil {
		t.Fatalf("Start = %v", err)
	}

	got := map[string]entity.JournalVersion{}

	for _, c := range r.calls(mark, "commit") {
		got[strings.TrimPrefix(c, "commit ")] = entity.JournalVersion{}
	}

	for _, ops := range r.ex.committedOps() {
		for _, op := range ops {
			for name, v := range pending {
				if op.Version == v {
					got[name] = v
				}
			}
		}
	}

	if !maps.Equal(got, pending) {
		t.Errorf("the restart re-applied %v, want exactly the pending writes %v", got, pending)
	}
}
