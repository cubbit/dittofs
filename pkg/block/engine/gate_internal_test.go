package engine

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
)

// idle is the gate's count with the shut bit cleared.
func idle(g *gate) int64 {
	return g.st.Load() &^ shutBit
}

// The last leave of a shut gate closes its idle channel, and a wait on a gate
// that is idle already is not a wait: a context that has ended does not
// change its answer.
func TestTheLastLeaveBeforeWaitIsNotLost(t *testing.T) {
	t.Run("the last leave closes the idle channel", func(t *testing.T) {
		var g gate

		if !g.enter() {
			t.Fatal("an open gate refused a unit")
		}

		g.shut()
		g.leave()

		if got := g.st.Load(); got != shutBit {
			t.Errorf("state after the last leave = %#x, want %#x", got, int64(shutBit))
		}

		select {
		case <-g.idle:
		default:
			t.Error("the idle channel is not closed after the last leave")
		}
	})

	t.Run("a wait on an idle gate returns nil under a cancelled context", func(t *testing.T) {
		var g gate

		g.shut()

		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		if err := g.wait(ctx); err != nil {
			t.Errorf("wait on an idle gate = %v, want nil", err)
		}
	})
}

// A wait returns only once every unit is withdrawn, and the last withdrawal
// wakes it.
func TestWaitReturnsOnlyWhenEveryUnitIsWithdrawn(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var g gate

		g.enter()
		g.enter()
		g.shut()

		done := make(chan error, 1)

		go func() {
			done <- g.wait(context.Background())
		}()

		synctest.Wait()
		g.leave()
		synctest.Wait()

		select {
		case err := <-done:
			t.Fatalf("wait returned %v with one unit still entered", err)
		default:
		}

		if got := idle(&g); got != 1 {
			t.Errorf("units entered after one withdrawal = %d, want 1", got)
		}

		g.leave()
		synctest.Wait()

		select {
		case err := <-done:
			if err != nil {
				t.Errorf("wait after the last withdrawal = %v, want nil", err)
			}
		default:
			t.Error("wait did not return after the last withdrawal")
		}
	})
}

// A wait that ends on its context leaves the gate shut with its unit counted,
// and a later wait joins the same unit again.
func TestAWaitEndsWithItsContextAndALaterWaitJoinsAgain(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var g gate

		g.enter()
		g.shut()

		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		if err := g.wait(ctx); err != context.Canceled {
			t.Errorf("wait under a cancelled context = %v, want context.Canceled", err)
		}

		if g.enter() {
			t.Error("a shut gate admitted a unit after a wait ended on its context")
		}

		if got := idle(&g); got != 1 {
			t.Errorf("units entered after the wait ended = %d, want 1", got)
		}

		done := make(chan error, 1)

		go func() {
			done <- g.wait(context.Background())
		}()

		synctest.Wait()

		select {
		case err := <-done:
			t.Fatalf("the second wait returned %v with a unit still entered", err)
		default:
		}

		g.leave()
		synctest.Wait()

		if err := <-done; err != nil {
			t.Errorf("the second wait = %v, want nil", err)
		}

		if err := g.wait(context.Background()); err != nil {
			t.Errorf("a third wait on an idle gate = %v, want nil", err)
		}
	})
}

// A release that is called twice, from two goroutines, withdraws each of its
// gates once: it never withdraws another caller's unit.
func TestLeaveOnceWithdrawsEachGateOnce(t *testing.T) {
	var x, y gate

	x.enter()
	x.enter()
	y.enter()

	release := leaveOnce(&x, &y)

	var wg sync.WaitGroup

	wg.Add(1)

	go func() {
		defer wg.Done()

		release()
	}()

	release()
	wg.Wait()

	if got := idle(&x); got != 1 {
		t.Errorf("units on the first gate = %d, want 1", got)
	}

	if got := idle(&y); got != 0 {
		t.Errorf("units on the second gate = %d, want 0", got)
	}

	x.shut()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := x.wait(ctx); err != context.Canceled {
		t.Errorf("wait with the other unit still entered = %v, want context.Canceled", err)
	}

	x.leave()

	if err := x.wait(context.Background()); err != nil {
		t.Errorf("wait after that unit left = %v, want nil", err)
	}
}

// Once a wait has returned, no unit is admitted: a worker that holds a unit
// after the wait returned would have been let in after the shut.
func TestNoUnitIsAdmittedOnceTheWaitReturns(t *testing.T) {
	const (
		rounds  = 200
		workers = 8
		loops   = 1000
	)

	for round := range rounds {
		var (
			g        gate
			unbound  atomic.Bool
			violated atomic.Bool
			started  sync.WaitGroup
			done     sync.WaitGroup
		)

		for range workers {
			started.Add(1)
			done.Add(1)

			go func() {
				defer done.Done()

				started.Done()

				for range loops {
					if !g.enter() {
						return
					}

					if unbound.Load() {
						violated.Store(true)
					}

					g.leave()
				}
			}()
		}

		started.Wait()
		g.shut()

		if err := g.wait(context.Background()); err != nil {
			t.Fatalf("round %d: wait = %v", round, err)
		}

		unbound.Store(true)
		done.Wait()

		if violated.Load() {
			t.Fatalf("round %d: a unit was held when the wait returned", round)
		}

		if got := idle(&g); got != 0 {
			t.Fatalf("round %d: units entered after the wait = %d, want 0", round, got)
		}

		if g.enter() {
			t.Fatalf("round %d: the gate admitted a unit after the wait", round)
		}
	}
}

// A release called twice withdraws its unit once, so a unit entered by
// someone else on the same gate stays counted: a wait on the shut gate still
// finds it inside.
func TestASecondCallOfAReleaseLeavesAnotherUnitCounted(t *testing.T) {
	var g gate

	g.enter()

	release := leaveOnce(&g)

	g.enter()
	release()
	release()
	g.shut()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := g.wait(ctx); err != context.Canceled {
		t.Errorf("wait with the other unit still inside = %v, want context.Canceled", err)
	}
}
