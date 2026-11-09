package engine

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

)

// errStoreDown is a store's failure of the whole call.
var errStoreDown = errors.New("the store is down")

// newClockFence is a fence over a test clock that has run for an idle minute.
func newClockFence() (*fence, *stepClock) {
	clock := &stepClock{}
	clock.Advance(time.Minute)
	return &fence{clock: clock, logger: slog.New(slog.DiscardHandler)}, clock
}

// A node with nothing outstanding or failing never fences, however long it
// stays idle.
func TestFenceIdleNeverHolds(t *testing.T) {
	f, clock := newClockFence()
	clock.Advance(time.Hour)
	if f.holds() {
		t.Error("an idle node fenced")
	}
}

// A transaction outstanding since t0 fences at t0+fenceAfter exactly, and not
// a nanosecond before; the fence counts once.
func TestFenceHoldsAtTheBound(t *testing.T) {
	f, clock := newClockFence()
	f.begin()
	clock.Advance(fenceAfter - 1)
	if f.holds() {
		t.Fatal("fenced a nanosecond before the bound")
	}
	clock.Advance(1)
	if !f.holds() {
		t.Fatal("not fenced at the bound")
	}
	f.holds()
	if n := f.fences.Load(); n != 1 {
		t.Errorf("fences %d, want 1", n)
	}
}

// A commit beside an outstanding transaction restarts the bound from the
// commit.
func TestFenceCommitRestarts(t *testing.T) {
	f, clock := newClockFence()
	f.begin()
	clock.Advance(20 * time.Second)
	f.begin()
	f.end(context.Background(), nil)
	clock.Advance(fenceAfter - 1)
	if f.holds() {
		t.Fatal("fenced before the bound counted from the commit")
	}
	clock.Advance(1)
	if !f.holds() {
		t.Fatal("not fenced at the bound counted from the commit")
	}
}

// A retrying owner failing holds the node busy with no transaction
// outstanding: it fences fenceAfter after its first attempt began, a retry
// between attempts not restarting the bound.
func TestFenceFailingWhileIdle(t *testing.T) {
	f, clock := newClockFence()
	ctx := withRetryingOwner(context.Background())
	f.begin()
	f.end(ctx, errStoreDown)
	clock.Advance(10 * time.Second)
	f.begin()
	f.end(ctx, errStoreDown)
	if f.outstanding.Load() != 0 {
		t.Fatal("a transaction still outstanding")
	}
	clock.Advance(fenceAfter - 10*time.Second)
	if !f.holds() {
		t.Error("a retrying owner failing for the bound did not fence")
	}
}

// A transaction starting after an idle minute, raced against gate reads:
// none fences, each start beginning a fresh bound. A start that raises the
// count before it stores since is seen busy with the last spell's since.
func TestFenceBeginRacesAReader(t *testing.T) {
	clock := &stepClock{}
	f := &fence{clock: clock, logger: slog.New(slog.DiscardHandler)}
	stop := make(chan struct{})
	var seen atomic.Int64
	var wg sync.WaitGroup
	wg.Go(func() {
		for {
			select {
			case <-stop:
				return
			default:
			}
			if f.holds() {
				seen.Add(1)
			}
		}
	})
	oneShot := errors.New("a one-shot call's failure")
	for range 20000 {
		clock.Advance(time.Minute)
		f.begin()
		f.end(context.Background(), oneShot) // neither a commit nor a failure
	}
	close(stop)
	wg.Wait()
	if n := seen.Load(); n > 0 {
		t.Errorf("%d gate reads fenced a transaction just begun", n)
	}
}
