package resources

import (
	"context"
	"errors"
	"log/slog"
	"math/rand/v2"
	"net/netip"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/cubbit/dittofs/internal/prof"
)

var client = netip.MustParseAddr("192.0.2.1")

// kinds runs a test over both kinds of budget through what they share: a
// reserve that fits, and Reserved.
var kinds = []struct {
	name string
	// clamps, when given, is the logger the budget clamps to as production does.
	new func(name string, ceiling int64, clamps *slog.Logger) (reserve func(int64) (Reservation, error), reserved func() int64)
}{
	{"Budget", func(name string, ceiling int64, clamps *slog.Logger) (func(int64) (Reservation, error), func() int64) {
		b := NewBudget(name, ceiling)
		if clamps != nil {
			b.clampTo(clamps)
		}
		return b.Reserve, b.Reserved
	}},
	{"WaitBudget", func(name string, ceiling int64, clamps *slog.Logger) (func(int64) (Reservation, error), func() int64) {
		b := NewWaitBudget(name, ceiling)
		if clamps != nil {
			b.clampTo(clamps)
		}
		return func(n int64) (Reservation, error) { return b.Reserve(context.Background(), client, n) }, b.Reserved
	}},
}

func mustReserve(t *testing.T, reserve func(int64) (Reservation, error), n int64) Reservation {
	t.Helper()
	r, err := reserve(n)
	if err != nil {
		t.Fatalf("reserve %d: %v", n, err)
	}
	return r
}

// A Budget charges what fits, up to and including its ceiling, and a refusal
// charges nothing.
func TestBudgetRefusesPastItsCeiling(t *testing.T) {
	b := NewBudget("hold", 10)
	if _, err := b.Reserve(6); err != nil || b.Reserved() != 6 {
		t.Fatalf("Reserve(6) = %v with %d reserved, want 6 reserved", err, b.Reserved())
	}
	if _, err := b.Reserve(5); err == nil || b.Reserved() != 6 {
		t.Errorf("Reserve(5) over 6 of 10 = %v with %d reserved, want a refusal and 6", err, b.Reserved())
	}
	if _, err := b.Reserve(4); err != nil || b.Reserved() != 10 {
		t.Errorf("Reserve(4) over 6 of 10 = %v with %d reserved, want 10: the ceiling itself fits", err, b.Reserved())
	}
	if _, err := b.Reserve(-1); err == nil || b.Reserved() != 10 {
		t.Errorf("Reserve(-1) = %v with %d reserved, want a refusal and 10", err, b.Reserved())
	}
}

// Release lowers the count once: a second Release and a Release of the zero
// Reservation change nothing.
func TestReleaseReturnsItsBytesOnce(t *testing.T) {
	for _, k := range kinds {
		t.Run(k.name, func(t *testing.T) {
			reserve, reserved := k.new("b", 10, nil)
			first, second := mustReserve(t, reserve, 4), mustReserve(t, reserve, 3)
			first.Release()
			if got := reserved(); got != 3 {
				t.Fatalf("after releasing 4 of 7, %d reserved, want 3", got)
			}
			first.Release()
			var zero Reservation
			zero.Release()
			if got := reserved(); got != 3 {
				t.Errorf("after a second Release and a zero Release, %d reserved, want 3", got)
			}
			second.Release()
			if got := reserved(); got != 0 {
				t.Errorf("at the end %d reserved, want 0", got)
			}
		})
	}
}

// A copy released after its original takes the count past zero: in a test
// binary that panics, and the count reads zero after it.
func TestReleasePastZeroPanicsInTests(t *testing.T) {
	for _, k := range kinds {
		t.Run(k.name, func(t *testing.T) {
			reserve, reserved := k.new("b", 10, nil)
			r := mustReserve(t, reserve, 4)
			twin := r
			r.Release()
			func() {
				defer func() {
					if recover() == nil {
						t.Error("releasing a copy past zero did not panic")
					}
				}()
				twin.Release()
			}()
			// The panic left the copy zeroed too: releasing it again does nothing.
			twin.Release()
			if got := reserved(); got != 0 {
				t.Errorf("after the panic %d reserved, want 0", got)
			}
		})
	}
}

// records keeps what the budgets' logger was handed.
type records struct {
	mu  sync.Mutex
	got []slog.Record
}

func (h *records) Enabled(context.Context, slog.Level) bool { return true }

func (h *records) WithAttrs([]slog.Attr) slog.Handler { return h }

func (h *records) WithGroup(string) slog.Handler { return h }

func (h *records) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.got = append(h.got, r)
	return nil
}

// Outside tests a release past zero clamps the count and logs one Error
// record naming the budget, once per budget.
func TestReleasePastZeroClampsAndLogsOncePerBudget(t *testing.T) {
	for _, k := range kinds {
		t.Run(k.name, func(t *testing.T) {
			h := &records{}
			pastZero := func(name string) {
				reserve, reserved := k.new(name, 10, slog.New(h))
				r := mustReserve(t, reserve, 4)
				twin := r
				r.Release()
				twin.Release()
				if got := reserved(); got != 0 {
					t.Errorf("%s: after a release past zero %d reserved, want 0", name, got)
				}
				// The budget still counts, and logs no second time.
				again := mustReserve(t, reserve, 2)
				twin = again
				again.Release()
				twin.Release()
				if got := reserved(); got != 0 {
					t.Errorf("%s: after a second release past zero %d reserved, want 0", name, got)
				}
			}
			pastZero("first")
			pastZero("second")

			var names []string
			for _, r := range h.got {
				if r.Level != slog.LevelError {
					t.Errorf("record %q at %v, want Error", r.Message, r.Level)
				}
				r.Attrs(func(a slog.Attr) bool {
					if a.Key == "budget" {
						names = append(names, a.Value.String())
					}
					return true
				})
			}
			if len(h.got) != 2 || len(names) != 2 || names[0] != "first" || names[1] != "second" {
				t.Errorf("%d records naming %v, want one for first then one for second", len(h.got), names)
			}
		})
	}
}

// waiting starts a Reserve that is expected to block, and returns once it has.
// done is closed when the Reserve returns.
type waiting struct {
	res  Reservation
	err  error
	done chan struct{}
}

func (w *waiting) returned() bool {
	select {
	case <-w.done:
		return true
	default:
		return false
	}
}

func startWaiting(t *testing.T, ctx context.Context, b *WaitBudget, n int64) *waiting {
	t.Helper()
	w := &waiting{done: make(chan struct{})}
	go func() {
		defer close(w.done)
		w.res, w.err = b.Reserve(ctx, client, n)
	}()
	synctest.Wait()
	if w.returned() {
		t.Fatalf("Reserve(%d) returned (%v) with %d of %d reserved, want it waiting", n, w.err, b.Reserved(), b.c.ceiling)
	}
	return w
}

// Waiters are granted in arrival order, one per release that makes room for
// one.
func TestWaitBudgetGrantsInArrivalOrder(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		b := NewWaitBudget("receive", 10)
		held := []Reservation{mustReserve(t, kindOf(b), 5), mustReserve(t, kindOf(b), 5)}
		ws := []*waiting{
			startWaiting(t, t.Context(), b, 5),
			startWaiting(t, t.Context(), b, 5),
			startWaiting(t, t.Context(), b, 5),
		}
		for i := range ws {
			if i < len(held) {
				held[i].Release()
			} else {
				ws[i-len(held)].res.Release()
			}
			synctest.Wait()
			for j, w := range ws {
				if got, want := w.returned(), j <= i; got != want {
					t.Fatalf("after release %d, waiter %d returned = %v, want %v", i, j, got, want)
				}
			}
		}
		ws[1].res.Release()
		ws[2].res.Release()
		if got := b.Reserved(); got != 0 {
			t.Errorf("at the end %d reserved, want 0", got)
		}
	})
}

func kindOf(b *WaitBudget) func(int64) (Reservation, error) {
	return func(n int64) (Reservation, error) { return b.Reserve(context.Background(), client, n) }
}

// One release grants every waiter that fits, in order, on its own goroutine,
// and stops at the first that does not: nothing passes the head, and a
// Reserve that would fit queues behind a waiter.
func TestWaitBudgetReleaseGrantsEachWaiterThatFits(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		b := NewWaitBudget("receive", 10)
		held := mustReserve(t, kindOf(b), 10)
		three := startWaiting(t, t.Context(), b, 3)
		four := startWaiting(t, t.Context(), b, 4)
		six := startWaiting(t, t.Context(), b, 6)
		one := startWaiting(t, t.Context(), b, 1)

		held.Release()
		if got := b.Reserved(); got != 7 {
			t.Fatalf("on the line after Release %d reserved, want 7: the waiters of 3 and 4 charged by the release itself", got)
		}
		synctest.Wait()
		if !three.returned() || !four.returned() || six.returned() || one.returned() {
			t.Fatalf("returned: 3 %v, 4 %v, 6 %v, 1 %v; want the first two only: 1 fits but is behind 6",
				three.returned(), four.returned(), six.returned(), one.returned())
		}

		three.res.Release()
		synctest.Wait()
		if !six.returned() || one.returned() || b.Reserved() != 10 {
			t.Fatalf("after releasing 3: 6 returned %v, 1 returned %v, %d reserved; want 6 granted, 1 waiting at 10",
				six.returned(), one.returned(), b.Reserved())
		}

		four.res.Release()
		synctest.Wait()
		if !one.returned() {
			t.Fatal("after releasing 4 the waiter of 1 still waits")
		}
		six.res.Release()
		one.res.Release()
		if got := b.Reserved(); got != 0 {
			t.Errorf("at the end %d reserved, want 0", got)
		}
	})
}

// A waiter of exactly the freed size is charged before Release returns.
func TestWaitBudgetReleaseChargesItsWaiterBeforeItReturns(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		b := NewWaitBudget("reply", 10)
		held := mustReserve(t, kindOf(b), 10)
		w := startWaiting(t, t.Context(), b, 10)
		held.Release()
		if got := b.Reserved(); got != 10 {
			t.Errorf("on the line after Release %d reserved, want 10", got)
		}
		synctest.Wait()
		w.res.Release()
	})
}

// ended is a context of each kind that has already ended.
func ended(t *testing.T) map[string]context.Context {
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	late, stop := context.WithDeadline(t.Context(), time.Now().Add(-time.Second))
	t.Cleanup(stop)
	return map[string]context.Context{"cancelled": cancelled, "past its deadline": late}
}

func matches(ctx context.Context, err error) bool {
	return err != nil && errors.Is(err, ctx.Err())
}

// Under a context that has already ended, Reserve tries once: it takes the
// charge when it fits and nobody waits, and otherwise returns at once.
func TestWaitBudgetReserveUnderAnEndedContextTriesOnce(t *testing.T) {
	for _, name := range []string{"cancelled", "past its deadline"} {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx := ended(t)[name]
				b := NewWaitBudget("reply", 10)
				r, err := b.Reserve(ctx, client, 6)
				if err != nil || b.Reserved() != 6 {
					t.Fatalf("with room: Reserve = %v with %d reserved, want 6 charged", err, b.Reserved())
				}
				if _, err := b.Reserve(ctx, client, 5); !matches(ctx, err) || b.Reserved() != 6 {
					t.Errorf("without room: Reserve = %v with %d reserved, want the context's error and 6", err, b.Reserved())
				}

				w := startWaiting(t, t.Context(), b, 5)
				if _, err := b.Reserve(ctx, client, 1); !matches(ctx, err) || b.Reserved() != 6 {
					t.Errorf("with room behind a waiter: Reserve = %v with %d reserved, want the context's error and 6", err, b.Reserved())
				}
				// Neither refusal stayed queued: the waiter alone is granted.
				r.Release()
				synctest.Wait()
				if !w.returned() || b.Reserved() != 5 {
					t.Fatalf("after the release: waiter returned %v with %d reserved, want it granted alone at 5", w.returned(), b.Reserved())
				}
				w.res.Release()
			})
		})
	}
}

// A waiter whose context is cancelled leaves the queue with nothing reserved,
// and those behind it are granted as if it had never queued.
func TestWaitBudgetCancelledWaiterLeavesTheQueue(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		b := NewWaitBudget("receive", 10)
		held := mustReserve(t, kindOf(b), 10)
		ctx, cancel := context.WithCancel(t.Context())
		first := startWaiting(t, t.Context(), b, 5)
		middle := startWaiting(t, ctx, b, 5)
		last := startWaiting(t, t.Context(), b, 5)

		cancel()
		synctest.Wait()
		if !middle.returned() || !errors.Is(middle.err, context.Canceled) || b.Reserved() != 10 {
			t.Fatalf("cancelled waiter: returned %v, err %v, %d reserved; want Canceled and 10", middle.returned(), middle.err, b.Reserved())
		}
		if first.returned() || last.returned() {
			t.Fatal("a waiter beside the cancelled one returned")
		}

		held.Release()
		if got := b.Reserved(); got != 10 {
			t.Fatalf("after the release %d reserved, want 10: the two waiters left, both granted", got)
		}
		synctest.Wait()
		first.res.Release()
		last.res.Release()
		if got, queued := b.Reserved(), b.waiters.Len(); got != 0 || queued != 0 {
			t.Errorf("at the end %d reserved and %d queued, want 0 and 0", got, queued)
		}
	})
}

// When the head of the queue is cancelled, the waiters it was holding back
// are granted what already fits.
func TestWaitBudgetCancelledHeadLetsTheNextFit(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		b := NewWaitBudget("receive", 10)
		held := mustReserve(t, kindOf(b), 6)
		ctx, cancel := context.WithCancel(t.Context())
		head := startWaiting(t, ctx, b, 6)
		behind := startWaiting(t, t.Context(), b, 4)

		cancel()
		synctest.Wait()
		if !head.returned() || !behind.returned() || behind.err != nil || b.Reserved() != 10 {
			t.Fatalf("head returned %v, behind returned %v (%v), %d reserved; want both, and 10",
				head.returned(), behind.returned(), behind.err, b.Reserved())
		}
		held.Release()
		behind.res.Release()
	})
}

// A grant and a cancel that both reach a waiter before it wakes leave nothing
// lost: it returns either the reservation or the context's error, and the
// bytes reach the waiter behind it either way. A waiter woken by its cancel
// finds the grant already made, and gives it back.
func TestWaitBudgetCancelRacingGrant(t *testing.T) {
	orders := map[string]func(release, cancel func()){
		"grant then cancel": func(release, cancel func()) { release(); cancel() },
		"cancel then grant": func(release, cancel func()) { cancel(); release() },
	}
	for name, both := range orders {
		t.Run(name, func(t *testing.T) {
			// Inside synctest each order runs the same way every time: grant
			// then cancel wakes on ready, cancel then grant takes the bytes
			// back. One round of each covers both branches.
			synctest.Test(t, func(t *testing.T) {
				b := NewWaitBudget("receive", 10)
				held := mustReserve(t, kindOf(b), 10)
				ctx, cancel := context.WithCancel(t.Context())
				w := startWaiting(t, ctx, b, 10)
				behind := startWaiting(t, t.Context(), b, 10)
				both(held.Release, cancel)
				synctest.Wait()
				switch {
				case !w.returned():
					t.Fatal("the waiter neither took its grant nor its cancel")
				case w.err == nil:
					w.res.Release()
				case !errors.Is(w.err, context.Canceled):
					t.Fatalf("the waiter returned %v", w.err)
				}
				synctest.Wait()
				if !behind.returned() || behind.err != nil {
					t.Fatalf("the waiter behind returned %v (%v), want it granted the bytes", behind.returned(), behind.err)
				}
				behind.res.Release()
				if got, queued := b.Reserved(), b.waiters.Len(); got != 0 || queued != 0 {
					t.Fatalf("the round ended at %d reserved and %d queued, want 0 and 0", got, queued)
				}
			})
		})
	}
}

// A charge larger than the ceiling can never fit: it is refused at once under
// any context, with an error a caller cannot take for a delay.
func TestWaitBudgetRefusesAChargePastItsCeiling(t *testing.T) {
	for _, name := range []string{"live", "cancelled", "past its deadline"} {
		t.Run(name, func(t *testing.T) {
			// In a bubble, so a Reserve that waited would end the test as a deadlock.
			synctest.Test(t, func(t *testing.T) {
				ctxs := ended(t)
				ctxs["live"] = t.Context()
				ctx := ctxs[name]
				b := NewWaitBudget("receive", 10)
				_, err := b.Reserve(ctx, client, 11)
				if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
					t.Errorf("Reserve(11) of 10 = %v, want an error that matches no context error", err)
				}
				if _, err := b.Reserve(ctx, client, -1); err == nil {
					t.Error("Reserve(-1) was granted")
				}
				if got, queued := b.Reserved(), b.waiters.Len(); got != 0 || queued != 0 {
					t.Errorf("after the refusals %d reserved and %d queued, want 0 and 0", got, queued)
				}
			})
		})
	}
}

// Many goroutines reserve random sizes from a small budget, some under a
// context cancelled at a random moment: the count never passes the ceiling,
// every reservation taken is released, and the budget ends idle.
func TestWaitBudgetModel(t *testing.T) {
	const (
		ceiling    = 16
		goroutines = 8
		rounds     = 2000
	)
	b := NewWaitBudget("receive", ceiling)
	var held atomic.Int64
	var wg sync.WaitGroup
	for g := range goroutines {
		wg.Go(func() {
			rng := rand.New(rand.NewPCG(1, uint64(g)))
			for range rounds {
				n := 1 + rng.Int64N(ceiling/2)
				ctx, cancel := context.WithCancel(context.Background())
				switch rng.IntN(4) {
				case 0:
					cancel()
				case 1:
					go cancel()
				}
				r, err := b.Reserve(ctx, client, n)
				if err == nil {
					if now := held.Add(n); now > ceiling {
						t.Errorf("%d bytes held at once, over the ceiling of %d", now, ceiling)
					}
					if got := b.Reserved(); got > ceiling {
						t.Errorf("%d reserved, over the ceiling of %d", got, ceiling)
					}
					held.Add(-n)
					r.Release()
				} else if !errors.Is(err, context.Canceled) {
					t.Errorf("Reserve(%d) = %v, want a grant or Canceled", n, err)
				}
				cancel()
			}
		})
	}
	// A waiter never granted would otherwise hang the package to its timeout.
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Minute):
		t.Fatal("reservers still running after a minute: a waiter was never granted")
	}
	if got, queued := b.Reserved(), b.waiters.Len(); got != 0 || queued != 0 {
		t.Errorf("at the end %d reserved and %d queued, want 0 and 0", got, queued)
	}
}

// A reservation is a value and the path with no waiter makes none: reserving
// and releasing allocates nothing on either kind.
func TestReserveAndReleaseAllocateNothing(t *testing.T) {
	if prof.RaceEnabled {
		t.Skip("the race detector changes allocation counts")
	}
	ctx := t.Context()
	wait, refuse := NewWaitBudget("receive", 10), NewBudget("hold", 10)
	allocs := testing.AllocsPerRun(100, func() {
		w, err := wait.Reserve(ctx, client, 1)
		if err != nil {
			t.Fatal(err)
		}
		r, err := refuse.Reserve(1)
		if err != nil {
			t.Fatal(err)
		}
		w.Release()
		r.Release()
	})
	if allocs != 0 {
		t.Errorf("a reserve and release on each kind allocate %v times, want 0", allocs)
	}
}
