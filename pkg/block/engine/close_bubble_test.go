package engine_test

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"strings"
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

// bubble is the engine composed inside a bubble: the fake journal and a stub
// store, with no real store and so no goroutine outside the bubble.
type bubble struct {
	e     *engine.Engine
	rec   *recJournal
	ev    *events
	clock *sim.TestClock
}

// newBubble builds the engine over store. Every handle the fake journal opens
// goes through wrap when it is set.
func newBubble(
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
		Devices:   []string{"bubble"},
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

	e.SetJournalsForTest(b.rec)
	b.e = e

	return b
}

// open is Deps.Open: the one fake journal.
func (b *bubble) open(
	context.Context,
	string,
	journal.Version,
	journal.JournalID,
) (engine.Journal, error) {
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
