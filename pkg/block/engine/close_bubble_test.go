package engine_test

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/cubbit/dittofs/pkg/block/engine"
	"github.com/cubbit/dittofs/pkg/block/engine/enginetest"
	"github.com/cubbit/dittofs/pkg/block/journal"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/sim"
)

// The tests in this file run in a testing/synctest bubble over a stub store,
// so a Close that waits without end is reported at once as a deadlock, and no
// real time passes. They cover the joins that, left unbounded, can only be
// seen as a Close that never returns.

// bubbleDevice is the one journal device of a bubble.
const bubbleDevice = "bubble"

// bubble is the engine composed inside a bubble: the fake journal and a stub
// store, with no real store and so no goroutine outside the bubble.
type bubble struct {
	e     *engine.Engine
	rec   *recJournal
	ev    *events
	clock *sim.TestClock

	// openFn, when set, answers Deps.Open in place of the fake journal, as
	// a test's wrapper around it.
	openFn func(context.Context) (engine.Journal, error)
}

// newBubble builds the engine over store. Every handle the fake journal opens
// goes through wrap when it is set.
func newBubble(
	t *testing.T,
	store *engine.StubStore,
	wrap func(engine.Handle) engine.Handle,
) *bubble {
	t.Helper()

	b := buildBubble(t, store, wrap)
	b.e.SetJournalsForTest(b.rec)

	return b
}

// buildBubble is the engine of a bubble before Start, with no journal open.
func buildBubble(
	t *testing.T,
	store *engine.StubStore,
	wrap func(engine.Handle) engine.Handle,
) *bubble {
	t.Helper()

	b := &bubble{
		ev:    &events{},
		clock: sim.NewTestClock(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)),
	}
	b.rec = &recJournal{
		FakeJournal: enginetest.NewFakeJournal(),
		wrap:        wrap,
		ev:          b.ev,
	}

	e, err := engine.New(engine.Deps{
		Node:      entity.NodeID{1},
		Devices:   []string{bubbleDevice},
		Open:      b.open,
		Existence: store,
		Content:   store,
		Lifecycle: store,
		Holds:     noHolds{},
		Clock:     b.clock,
		Logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
		Tracer: func(ctx context.Context, _ string, _ ...slog.Attr) (context.Context, func(error)) {
			return ctx, func(error) {
			}
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	if err := e.Apply(enginetest.Settings); err != nil {
		t.Fatal(err)
	}

	if err := b.rec.SetVersionLimit(journal.Version{Counter: unitLimit}); err != nil {
		t.Fatal(err)
	}

	b.e = e

	return b
}

// open is Deps.Open: the one fake journal.
func (b *bubble) open(
	ctx context.Context,
	_ string,
	_ journal.Version,
	_ journal.JournalID,
) (engine.Journal, error) {
	if b.openFn != nil {
		return b.openFn(ctx)
	}

	return b.rec, nil
}

// bubbleShare is a share the stub store needs no record of.
func bubbleShare(tag journal.ShareTag) engine.Share {
	return engine.Share{
		ID:    entity.NewShareID(),
		Tag:   tag,
		State: engine.Serving,
		Limit: 1 << 30,
	}
}

// A Bind that failed after it opened its handle, whose handle never finishes
// closing, is reported by a Close that has given up at the stop bound: it
// names the share, and leaves the handle and the journal open.
func TestAFailedBindsHandleCloseNeverReleasedIsReportedAtTheBound(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		store := engine.NewStubStore()
		store.RaiseErr = errInjected
		hc := &heldClose{entered: make(chan struct{}), release: make(chan struct{})}
		b := newBubble(t, store, func(h engine.Handle) engine.Handle {
			hc.Handle = h

			return hc
		})
		s := bubbleShare(7)
		bound := make(chan error, 1)

		go func() {
			bound <- b.e.Bind(context.Background(), s)
		}()

		synctest.Wait()

		closed := closing(b.e, context.Background())

		synctest.Wait()
		b.clock.Advance(engine.StopBoundDefault)

		err := <-closed
		if !unavailable(err) || !strings.Contains(err.Error(), fmt.Sprint(s.ID)) {
			t.Errorf("Close = %v, want ErrUnavailable naming the share %v", err, s.ID)
		}

		if got := b.ev.list(); len(got) != 0 {
			t.Errorf("closes %v after a Close that gave up, want none", got)
		}

		hc.open()

		if err := <-bound; !unavailable(err) {
			t.Errorf("the failed Bind = %v, want ErrUnavailable", err)
		}

		b.e.CloseGroupCommits()
	})
}

// heldLoop is a bubble with one share bound and a removal item held in its
// batch on the share's journal loop.
func heldLoop(t *testing.T) (*bubble, engine.Share, *loopHold) {
	t.Helper()

	b := newBubble(t, engine.NewStubStore(), nil)
	s := bubbleShare(7)
	h := newLoopHold()

	if err := b.e.Bind(context.Background(), s); err != nil {
		t.Fatal(err)
	}

	if err := b.e.EnqueueHeld(s.ID, h.run(b.ev), nil); err != nil {
		t.Fatal(err)
	}

	synctest.Wait()

	return b, s, h
}

// A Close whose deadline passed while a removal batch still runs gives up at
// the loop's join and leaves the handle and the journal open. A later Close
// joins the loop again: the handle closes only after the loop's goroutine
// ended.
func TestACloseHeldInARemovalBatchPastTheContextDeadlineLeavesEverythingOpen(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		b, s, h := heldLoop(t)

		var joins atomic.Int32

		b.e.OnJoining(func(what string) {
			if what == "a removal loop" && joins.Add(1) == 2 {
				h.openSecond()
			}
		})

		b.ev.onAdd = func(what string) {
			if strings.HasPrefix(what, "handle close") {
				h.openSecond()
			}
		}

		past, cancel := context.WithDeadline(t.Context(), time.Now().Add(-time.Second))

		defer cancel()

		err := b.e.Close(past)
		if !unavailable(err) || !strings.Contains(err.Error(), "removal loop") {
			t.Errorf("Close = %v, want ErrUnavailable naming the removal loop", err)
		}

		if got := b.ev.list(); len(got) != 0 {
			t.Errorf("closes %v after a Close that gave up, want none", got)
		}

		h.openFirst()

		if err := b.e.Close(context.Background()); err != nil {
			t.Errorf("the second Close = %v, want nil", err)
		}

		want := []string{"loop item ended", handleClose(s), "journal close"}
		if got := b.ev.list(); !slices.Equal(got, want) {
			t.Errorf("events %v, want %v", got, want)
		}

		b.e.CloseGroupCommits()
	})
}

// Without a deadline the stop bound is 30 s of the engine's clock.
func TestACloseWithNoDeadlineGivesUpAtThirtySecondsOfTheTestClock(t *testing.T) {
	t.Run("29 s is still waiting", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			b, s, h := heldLoop(t)
			closed := closing(b.e, context.Background())

			synctest.Wait()
			b.clock.Advance(29 * time.Second)
			synctest.Wait()
			h.openAll()

			if err := <-closed; err != nil {
				t.Errorf("Close = %v after 29 s, want nil", err)
			}

			want := []string{"loop item ended", handleClose(s), "journal close"}
			if got := b.ev.list(); !slices.Equal(got, want) {
				t.Errorf("events %v, want %v", got, want)
			}

			b.e.CloseGroupCommits()
		})
	})

	t.Run("30 s is the bound", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			b, _, h := heldLoop(t)
			closed := closing(b.e, context.Background())

			synctest.Wait()
			b.clock.Advance(30 * time.Second)

			if err := <-closed; !unavailable(err) {
				t.Errorf("Close = %v after 30 s, want ErrUnavailable", err)
			}

			if got := b.ev.list(); len(got) != 0 {
				t.Errorf("closes %v after a Close that gave up, want none", got)
			}

			h.openAll()
			b.e.CloseGroupCommits()
		})
	})
}

// The last group commit leaves out a file whose removal was cut short, and
// never waits for its guard: nothing releases it once the loop is stopped.
func TestACloseNeverWaitsOnAGuardOfACutShortFile(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		store := engine.NewStubStore()
		b := newBubble(t, store, nil)
		s := bubbleShare(7)

		if err := b.e.Bind(context.Background(), s); err != nil {
			t.Fatal(err)
		}

		f := entity.NewFileID()
		opts := engine.WriteOpts{Done: func() {}}

		if _, err := b.e.Write(context.Background(), s.ID, f, 0, []byte("x"), opts); err != nil {
			t.Fatal(err)
		}

		b.e.HoldGuard(f)

		if err := b.e.Close(context.Background()); err != nil {
			t.Errorf("Close = %v, want nil", err)
		}

		if n := store.Commits.Load(); n != 0 {
			t.Errorf("%d existence commits, want none: the guarded file was committed", n)
		}

		b.e.CloseGroupCommits()
	})
}
