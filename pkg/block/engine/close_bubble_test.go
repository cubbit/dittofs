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
	"github.com/cubbit/dittofs/pkg/metadata/kv"
	"github.com/cubbit/dittofs/pkg/metadata/kv/kvtest"
	"github.com/cubbit/dittofs/pkg/metadata/kvstore"
	"github.com/cubbit/dittofs/pkg/metadata/kvstore/kvstoretest"
	"github.com/cubbit/dittofs/pkg/sim"
)

// The tests in this file run in a testing/synctest bubble, so a Close that
// waits without end is reported at once as a deadlock, and no real time
// passes. They cover the joins that, left unbounded, can only be seen as a
// Close that never returns.
//
// The store is the real one on an in-memory kv.KV, opened before the bubble
// so its goroutines are outside it: their tickers neither keep the bubble
// from going idle nor advance its clock. Its transactions run outside the
// bubble too, through outsideKV, since a badger commit signals the caller
// from badger's own goroutine, on a channel and a WaitGroup the caller made,
// which the runtime refuses across a bubble's edge.

// bubbleDevice is the one journal device of a bubble.
const bubbleDevice = "bubble"

// bubbleNode is the node of a bubble, and bubbleJournal the identity its
// store records for bubbleDevice.
var (
	bubbleNode    = entity.NodeID{1}
	bubbleJournal = [16]byte{'B'}
)

// bubbleFS opens the store on an in-memory kv.KV, wrapped by faults, with
// bubbleJournal recorded for the bubble's device. Call it outside
// synctest.Test.
func bubbleFS(t *testing.T) (*kvstoretest.FS, *kvtest.FaultKV) {
	t.Helper()

	var faults *kvtest.FaultKV

	fs := kvstoretest.NewFS(t, func(tb testing.TB) kv.KV {
		faults = kvtest.NewFaultKV(memKV(tb))

		return newOutsideKV(tb, faults)
	})

	err := fs.Store.RecordJournal(t.Context(), bubbleJournal, bubbleNode, entity.JournalDir(bubbleDevice))
	if err != nil {
		t.Fatal(err)
	}

	return fs, faults
}

// outsideKV runs each transaction of another kv.KV on a goroutine outside any
// bubble. A caller in a bubble waits on channels made outside it, so it is
// not durably blocked: the bubble neither advances its clock nor reports a
// deadlock while a transaction is in flight, and the transaction finishes in
// real time.
type outsideKV struct {
	inner kv.KV
	calls chan func()     // run by the goroutine newOutsideKV starts
	reply chan chan error // a free list of reply channels made outside
}

// outsideCalls is how many transactions an outsideKV runs at once.
const outsideCalls = 64

// newOutsideKV starts the goroutine that runs inner's transactions. Call it
// outside synctest.Test; the goroutine ends when the test does.
func newOutsideKV(tb testing.TB, inner kv.KV) *outsideKV {
	o := &outsideKV{
		inner: inner,
		calls: make(chan func()),
		reply: make(chan chan error, outsideCalls),
	}

	for range outsideCalls {
		o.reply <- make(chan error, 1)
	}

	stop := make(chan struct{})

	go func() {
		for {
			select {
			case call := <-o.calls:
				go call()
			case <-stop:
				return
			}
		}
	}()

	tb.Cleanup(func() { close(stop) })

	return o
}

// run runs call outside the bubble and returns its error.
func (o *outsideKV) run(call func() error) error {
	r := <-o.reply
	o.calls <- func() { r <- call() }
	err := <-r
	o.reply <- r

	return err
}

func (o *outsideKV) Update(ctx context.Context, fn func(kv.Txn) error) error {
	return o.run(func() error { return o.inner.Update(ctx, fn) })
}

func (o *outsideKV) View(ctx context.Context, fn func(kv.Reader) error) error {
	return o.run(func() error { return o.inner.View(ctx, fn) })
}

func (o *outsideKV) Limits() kv.TxnLimits { return o.inner.Limits() }

func (o *outsideKV) Close() error { return o.inner.Close() }

// failUpdates makes every store update through faults answer errInjected.
func failUpdates(t *testing.T, faults *kvtest.FaultKV) {
	t.Helper()

	sched, err := sim.NewSchedule(1, nil, sim.Rule{
		Op:     kvtest.OpUpdate,
		Key:    "*",
		Action: sim.FaultAction{Err: errInjected},
	})
	if err != nil {
		t.Fatal(err)
	}

	faults.SetFault(sched)
}

// bubble is the engine composed inside a bubble: the fake journal and the
// store bubbleFS opened.
type bubble struct {
	e     *engine.Engine
	rec   *recJournal
	ev    *events
	clock *sim.TestClock

	// openFn, when set, answers Deps.Open in place of the fake journal, as
	// a test's wrapper around it.
	openFn func(context.Context) (engine.Journal, error)
}

// newBubble builds the engine over fs's store. Every handle the fake journal opens
// goes through wrap when it is set.
func newBubble(
	t *testing.T,
	fs *kvstoretest.FS,
	wrap func(engine.Handle) engine.Handle,
) *bubble {
	t.Helper()

	b := buildBubble(t, fs, wrap)
	b.e.SetJournalsForTest(b.rec)

	return b
}

// buildBubble is the engine of a bubble over fs's store before Start, with
// no journal open. Each edit changes the engine's Deps before it is built.
func buildBubble(
	t *testing.T,
	fs *kvstoretest.FS,
	wrap func(engine.Handle) engine.Handle,
	edits ...func(*engine.Deps),
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

	d := engine.Deps{
		Node:      bubbleNode,
		Devices:   []string{bubbleDevice},
		Open:      b.open,
		Existence: fs.Store.Existence(),
		Content:   fs.Store.Content(),
		Lifecycle: fs.Store.Lifecycle(),
		Holds:     noHolds{},
		Clock:     b.clock,
		Logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
		Tracer: func(ctx context.Context, _ string, _ ...slog.Attr) (context.Context, func(error)) {
			return ctx, func(error) {
			}
		},
	}

	for _, edit := range edits {
		edit(&d)
	}

	e, err := engine.New(d)
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

// bubbleShare is the store's share.
func bubbleShare(t *testing.T, fs *kvstoretest.FS) engine.Share {
	t.Helper()

	return enginetest.ShareOf(t, fs.Store, fs.Share.ID, 1<<30)
}

// A Bind that failed after it opened its handle, whose handle never finishes
// closing, is reported by a Close that has given up at the stop bound: it
// names the share, and leaves the handle and the journal open.
func TestAFailedBindsHandleCloseNeverReleasedIsReportedAtTheBound(t *testing.T) {
	fs, faults := bubbleFS(t)
	s := bubbleShare(t, fs)

	// Every store update fails, so the raise at attach does.
	failUpdates(t, faults)

	synctest.Test(t, func(t *testing.T) {
		hc := &heldClose{entered: make(chan struct{}), release: make(chan struct{})}
		b := newBubble(t, fs, func(h engine.Handle) engine.Handle {
			hc.Handle = h

			return hc
		})
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

// heldLoop is a bubble over fs with its share bound and a removal item held
// in its batch on the share's journal loop.
func heldLoop(t *testing.T, fs *kvstoretest.FS) (*bubble, engine.Share, *loopHold) {
	t.Helper()

	b := newBubble(t, fs, nil)
	s := bubbleShare(t, fs)
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
	fs, _ := bubbleFS(t)

	synctest.Test(t, func(t *testing.T) {
		b, s, h := heldLoop(t, fs)

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
		fs, _ := bubbleFS(t)

		synctest.Test(t, func(t *testing.T) {
			b, s, h := heldLoop(t, fs)
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
		fs, _ := bubbleFS(t)

		synctest.Test(t, func(t *testing.T) {
			b, _, h := heldLoop(t, fs)
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
	fs, _ := bubbleFS(t)
	s := bubbleShare(t, fs)
	_, made := fs.Create(t, fs.Admin, fs.Root, "a", kvstore.Regular(0o644))
	f := made.ID

	synctest.Test(t, func(t *testing.T) {
		b := newBubble(t, fs, nil)

		if err := b.e.Bind(context.Background(), s); err != nil {
			t.Fatal(err)
		}
		opts := engine.WriteOpts{Done: func() {}}

		if _, err := b.e.Write(context.Background(), s.ID, f, 0, []byte("x"), opts); err != nil {
			t.Fatal(err)
		}

		b.e.HoldGuard(f)

		if err := b.e.Close(context.Background()); err != nil {
			t.Errorf("Close = %v, want nil", err)
		}

		n, err := fs.Store.Existence().Size(context.Background(), s.ID, f)
		if err != nil {
			t.Fatal(err)
		}

		if n != 0 {
			t.Errorf("size %d after Close, want 0: the guarded file was committed", n)
		}

		b.e.CloseGroupCommits()
	})
}
