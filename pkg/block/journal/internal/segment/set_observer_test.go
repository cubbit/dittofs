package segment

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
)

// events collects what the set's observers are told, safe for the callbacks
// running from any goroutine.
type events struct {
	mu      sync.Mutex
	sealed  []core.SegmentID
	retired []core.SegmentID
}

func (e *events) seal(id core.SegmentID) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.sealed = append(e.sealed, id)
}

func (e *events) retire(id core.SegmentID) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.retired = append(e.retired, id)
}

// seals returns a copy of the seal events so far.
func (e *events) seals() []core.SegmentID {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]core.SegmentID(nil), e.sealed...)
}

// retires returns a copy of the retire events so far.
func (e *events) retires() []core.SegmentID {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]core.SegmentID(nil), e.retired...)
}

// taken seals a spare into the set: the two ledger steps every observer
// test's segments go through first.
func taken(t *testing.T, s *Set, id core.SegmentID) {
	t.Helper()
	if err := s.AddSpare(id, noMax); err != nil {
		t.Fatalf("AddSpare: %v", err)
	}
	if err := s.Take(id); err != nil {
		t.Fatalf("Take: %v", err)
	}
}

// TestSetObserverSeesSettledState: each observer runs once the state change
// it names is whole, so what it reads of the set at callback time is the
// state after the change, never a half-applied one. The recycle is the case
// that pins it: the fresh spare and the footprint the recycle restores are
// in place when its retire callback runs.
func TestSetObserverSeesSettledState(t *testing.T) {
	const segCap = 4096
	s := NewSet(segCap, nil)
	a, b := core.SegmentID(1), core.SegmentID(2)
	fresh := core.SegmentID(3)
	var saw events
	s.OnSeal(func(id core.SegmentID) {
		st, err := s.State(id)
		if err != nil || st != Sealed {
			t.Errorf("at the seal callback of %d, State = %v, %v, want Sealed: the callback reads a half-sealed set", id, st, err)
		}
		saw.seal(id)
	})
	s.OnRetire(func(id core.SegmentID) {
		c := s.Counters()
		switch id {
		case a:
			// An unlink: the freed cap is off the footprint — the other
			// segment's cap is what remains — and the segment is out of
			// the set.
			if c.UsedBytes != segCap {
				t.Errorf("at the unlink callback of %d, UsedBytes = %d, want the other segment's cap %d: the freed cap is not off the footprint", id, c.UsedBytes, segCap)
			}
			if c.Segments != 1 {
				t.Errorf("at the unlink callback of %d, Segments = %d, want 1", id, c.Segments)
			}
			if _, err := s.State(id); !errors.Is(err, ErrNotInSet) {
				t.Errorf("at the unlink callback of %d, State = %v, want ErrNotInSet", id, err)
			}
		case b:
			// A recycle: the footprint does not move and the fresh spare
			// is owned when the callback runs.
			if c.UsedBytes != segCap {
				t.Errorf("at the recycle callback of %d, UsedBytes = %d, want the cap %d: the callback reads a half-recycled set", id, c.UsedBytes, segCap)
			}
			if c.Spares != 1 {
				t.Errorf("at the recycle callback of %d, Spares = %d, want 1: the fresh spare is not in place", id, c.Spares)
			}
			if err := s.AddSpare(fresh, noMax); !errors.Is(err, ErrDuplicateID) {
				t.Errorf("at the recycle callback of %d, AddSpare(fresh) = %v, want ErrDuplicateID: the fresh id is not owned yet", id, err)
			}
		}
		saw.retire(id)
	})

	taken(t, s, a)
	taken(t, s, b)
	if err := s.Seal(a, true); err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if _, err := s.Retire(a); err != nil {
		t.Fatalf("Retire: %v", err)
	}
	if err := s.Seal(b, true); err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if err := s.Recycle(b, fresh); err != nil {
		t.Fatalf("Recycle: %v", err)
	}
	if got, want := saw.seals(), []core.SegmentID{a, b}; len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("the seal observer was told %v, want %v", got, want)
	}
	if got, want := saw.retires(), []core.SegmentID{a, b}; len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("the retire observer was told %v, want %v", got, want)
	}
	if err := s.Check(); err != nil {
		t.Fatalf("Check after the observer-read lifecycle: %v", err)
	}
}

// TestSetObserverRetireWaitsForTheSealCallback: a retire callback for a
// segment never runs before that segment's seal callback has returned. The
// seal callback parks inside itself, and the retire callback checks it has
// the seal's blessing before it runs: a retire whose delivery did not wait
// on the seal's notices the park still in place and fails on its own
// assertion.
func TestSetObserverRetireWaitsForTheSealCallback(t *testing.T) {
	const segCap = 4096
	s := NewSet(segCap, nil)
	id := core.SegmentID(1)
	taken(t, s, id)

	sealStarted := make(chan struct{})
	sealDone := make(chan struct{})
	releaseSeal := make(chan struct{})
	var saw events
	s.OnSeal(func(core.SegmentID) {
		close(sealStarted)
		// The park is bounded because the test releases it or fails first;
		// t.Error rather than t.Fatal, for this runs on the goroutine that
		// called Seal, not on the test's own.
		select {
		case <-releaseSeal:
		case <-time.After(hookDeadline):
			t.Errorf("the parked seal callback was never released within %v", hookDeadline)
		}
		saw.seal(id)
		close(sealDone)
	})
	retiredEarly := make(chan struct{}, 1)
	s.OnRetire(func(core.SegmentID) {
		// The retire callback runs only once the seal callback has
		// returned: a delivery that let it run inside the seal callback's
		// park window is the ordering bug this test catches, from inside
		// the callback itself so no goroutine timing hides it.
		select {
		case <-sealDone:
		default:
			retiredEarly <- struct{}{}
		}
		saw.retire(id)
	})

	go func() {
		if err := s.Seal(id, true); err != nil {
			t.Errorf("Seal: %v", err)
		}
	}()
	waitFor(t, sealStarted, "the seal callback to start")

	// The segment is Sealed now, so the retire is legal. Its call runs in
	// its own goroutine while the seal callback stays parked: the bounded
	// window below asserts the retire does not happen. The window is a
	// second, well inside the park's own bound so the parked callback is
	// never released by its timer first; a retire whose callback ran early
	// completes inside it — the goroutine has a core and no lock to wait
	// on under an unordered delivery — and fails on the assertion below,
	// never the package timeout.
	retireDone := make(chan error, 1)
	go func() {
		_, err := s.Retire(id)
		retireDone <- err
	}()
	select {
	case err := <-retireDone:
		t.Fatalf("Retire returned %v while the seal callback was still parked: the retire callback ran before the seal callback returned", err)
	case <-time.After(time.Second):
		// The retire did not run in the park window, as guaranteed.
	}
	close(releaseSeal)
	if err := waitFor(t, retireDone, "the retire to finish once the seal callback returned"); err != nil {
		t.Fatalf("Retire: %v", err)
	}
	select {
	case <-retiredEarly:
		t.Fatal("the retire callback ran before the seal callback returned")
	default:
	}
	if got := saw.seals(); len(got) != 1 {
		t.Fatalf("after the release, the seal observer was told %v, want the one seal", got)
	}
	if got := saw.retires(); len(got) != 1 {
		t.Fatalf("after the release, the retire observer was told %v, want the one retire", got)
	}
}

// TestSetObserverReadsInsideTheCallback: an observer may read the set — a
// counters copy, a state read — from inside its callback, and the reads
// answer with the whole state change on a bounded wait, so a deadlock fails
// the test on its own assertion rather than the package's timeout.
func TestSetObserverReadsInsideTheCallback(t *testing.T) {
	const segCap = 4096
	s := NewSet(segCap, nil)
	id := core.SegmentID(1)
	taken(t, s, id)

	// Seal runs in its own goroutine so the callback's channel sends have a
	// receiver: the test's goroutine is the one that drains them, and a
	// delivery that held the entry lock across the callback would deadlock
	// here and fail on the bounded wait instead of hanging.
	read := make(chan Counters)
	state := make(chan State, 1)
	sealed := make(chan error, 1)
	s.OnSeal(func(seg core.SegmentID) {
		read <- s.Counters()
		st, err := s.State(seg)
		if err != nil {
			t.Errorf("State inside the seal callback: %v", err)
		}
		state <- st
	})
	go func() { sealed <- s.Seal(id, true) }()
	c := waitFor(t, read, "the counters read inside the callback to answer")
	if c.Sealed != 1 || c.UsedBytes != segCap {
		t.Fatalf("the counters read inside the callback = %+v, want one sealed segment at the cap", c)
	}
	if st := waitFor(t, state, "the state read inside the callback to answer"); st != Sealed {
		t.Fatalf("the state read inside the callback = %v, want Sealed", st)
	}
	if err := waitFor(t, sealed, "the seal to return once its callback is drained"); err != nil {
		t.Fatalf("Seal: %v", err)
	}
}

// TestSetObserverRefusalsTellNothing: a refused seal or retire names no
// observer event, and a spare's drop retires nothing.
func TestSetObserverRefusalsTellNothing(t *testing.T) {
	const segCap = 4096
	s := NewSet(segCap, nil)
	out := core.SegmentID(1)
	var saw events
	s.OnSeal(saw.seal)
	s.OnRetire(saw.retire)

	// Refused calls on a segment out of the set.
	if err := s.Seal(out, true); !errors.Is(err, ErrNotInSet) {
		t.Fatalf("a seal of a segment out of the set = %v, want ErrNotInSet", err)
	}
	if _, err := s.Retire(out); !errors.Is(err, ErrNotInSet) {
		t.Fatalf("a retire of a segment out of the set = %v, want ErrNotInSet", err)
	}
	a, fresh := core.SegmentID(2), core.SegmentID(3)
	taken(t, s, a)
	// A retire of an Active segment is refused and tells nothing.
	if _, err := s.Retire(a); !errors.Is(err, ErrWrongState) {
		t.Fatalf("a retire of an Active segment = %v, want ErrWrongState", err)
	}
	if err := s.Seal(a, true); err != nil {
		t.Fatalf("Seal: %v", err)
	}
	// A seal of a Sealed segment, and a recycle onto the id it recycles,
	// are refused and tell nothing either.
	if err := s.Seal(a, true); !errors.Is(err, ErrWrongState) {
		t.Fatalf("a second seal of the same segment = %v, want ErrWrongState", err)
	}
	if err := s.Recycle(a, a); !errors.Is(err, ErrDuplicateID) {
		t.Fatalf("a recycle onto the id it recycles = %v, want ErrDuplicateID", err)
	}
	// A second retire is refused and tells nothing, and a drop of a spare
	// the set does not hold retires nothing.
	if _, err := s.Retire(a); err != nil {
		t.Fatalf("Retire: %v", err)
	}
	if _, err := s.Retire(a); !errors.Is(err, ErrNotInSet) {
		t.Fatalf("a second retire of the same segment = %v, want ErrNotInSet", err)
	}
	if _, err := s.DropSpare(fresh); !errors.Is(err, ErrNotInSet) {
		t.Fatalf("a drop of a spare the set does not hold = %v, want ErrNotInSet", err)
	}
	if got := saw.seals(); len(got) != 1 || got[0] != a {
		t.Fatalf("the seal observer was told %v, want only [%d]", got, a)
	}
	if got := saw.retires(); len(got) != 1 || got[0] != a {
		t.Fatalf("the retire observer was told %v, want only [%d]", got, a)
	}
}

// TestSetObserverRegisteredMidLife: the observers can be registered once
// the set already serves — the calls before the registration tell nothing,
// the calls after it tell exactly what happened, and the ledger closes the
// life with its own checks green.
func TestSetObserverRegisteredMidLife(t *testing.T) {
	const segCap = 4096
	s := NewSet(segCap, nil)
	a := core.SegmentID(1)
	taken(t, s, a)
	if err := s.Seal(a, false); err != nil {
		t.Fatalf("Seal: %v", err)
	}

	var saw events
	s.OnSeal(saw.seal)
	s.OnRetire(saw.retire)
	// The seal that ran before the registration is not replayed to it.
	if got := saw.seals(); len(got) != 0 {
		t.Fatalf("the seal observer was told %v of calls before its registration, want none", got)
	}
	b := core.SegmentID(2)
	taken(t, s, b)
	if err := s.Seal(b, true); err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if _, err := s.Retire(b); err != nil {
		t.Fatalf("Retire: %v", err)
	}
	if got := saw.seals(); len(got) != 1 || got[0] != b {
		t.Fatalf("the seal observer was told %v, want only [%d]", got, b)
	}
	if got := saw.retires(); len(got) != 1 || got[0] != b {
		t.Fatalf("the retire observer was told %v, want only [%d]", got, b)
	}
	if err := s.Check(); err != nil {
		t.Fatalf("Check after the mid-life lifecycle: %v", err)
	}
}
