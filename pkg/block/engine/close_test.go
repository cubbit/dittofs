package engine_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/block/engine"
	"github.com/cubbit/dittofs/pkg/block/engine/enginetest"
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

// reached waits for the seam fired, or fails the test if Close returns first.
func reached(t *testing.T, fired <-chan struct{}, closed <-chan error) {
	t.Helper()

	select {
	case <-fired:
	case err := <-closed:
		t.Fatalf("Close returned %v before it had to wait", err)
	}
}

// unavailable reports whether err is ErrUnavailable.
func unavailable(err error) bool {
	return errors.Is(err, vfserr.ErrUnavailable)
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

		waiting := make(chan struct{}, 4)
		u.e.OnJoining(func() {
			waiting <- struct{}{}
		})

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

		waiting := make(chan struct{}, 4)
		u.e.OnJoining(func() {
			waiting <- struct{}{}
		})

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
