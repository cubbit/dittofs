package engine_test

import (
	"bytes"
	"context"
	"errors"
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
)

// blockedIn waits until one goroutine has every one of fns in its stack.
func blockedIn(t *testing.T, fns ...string) {
	t.Helper()

	deadline := time.Now().Add(10 * time.Second)

	for time.Now().Before(deadline) {
		var buf bytes.Buffer

		if err := pprof.Lookup("goroutine").WriteTo(&buf, 2); err != nil {
			t.Fatal(err)
		}

		for _, g := range strings.Split(buf.String(), "\n\n") {
			if !slices.ContainsFunc(fns, func(fn string) bool { return !strings.Contains(g, fn) }) {
				return
			}
		}

		time.Sleep(time.Millisecond)
	}

	t.Fatalf("no goroutine is in %v", fns)
}

// syncHold is a handle whose first Sync waits for the test.
type syncHold struct {
	engine.Handle
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (h *syncHold) Sync(ctx context.Context, ids ...journal.FileID) ([]entity.SyncMark, error) {
	h.once.Do(func() {
		close(h.entered)
		<-h.release
	})

	return h.Handle.Sync(ctx, ids...)
}

// The last group commit must wait for a batch the loop's own timer takes
// while Close is marking the files: Close read the batch in flight before the
// marking, found the file gone from the due set, and returned over a batch
// that the stop then cancelled, with the write still pending.
func TestTheLastGroupCommitWaitsForABatchTheLoopTookWhileItMarkedTheFiles(t *testing.T) {
	var (
		cut      atomic.Bool
		done     atomic.Int32
		entered  = make(chan struct{})
		release  = make(chan struct{})
		enter    sync.Once
		open     sync.Once
		openHold = func() { open.Do(func() { close(release) }) }
	)

	u := newUnit(t, onCommit(func(
		ctx context.Context,
		next engine.Existence,
		p []kvstore.PendingExistence,
		marks *kvstore.MarksWrite,
	) (map[entity.FileID]error, error) {
		enter.Do(func() { close(entered) })

		<-release

		if err := ctx.Err(); err != nil {
			cut.Store(true)

			return nil, err
		}

		return next.Commit(ctx, p, marks)
	}))

	t.Cleanup(openHold)

	f := u.create(t, "a")

	if _, err := u.e.Write(t.Context(), u.share.ID, f, 0, []byte("abc"), engine.WriteOpts{
		Done: func() { done.Add(1) },
	}); err != nil {
		t.Fatal(err)
	}

	u.e.OnJoining(func(what string) {
		if what == "the last group commit" || what == "a group commit" {
			openHold()
		}
	})

	taken := seam(u.e.OnTake)
	unlock := u.e.HoldEntry(f)

	// A deadline of its own keeps the stop bound off the test clock, which
	// the test moves.
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)

	defer cancel()

	closed := closing(u.e, ctx)

	// The last group commit has taken its snapshot of the due set and waits
	// on the file's entry, which the test holds.
	blockedIn(t, "commitAll", "guardOf")
	u.clock.Advance(2 * time.Minute)

	select {
	case <-taken:
	case err := <-closed:
		t.Fatalf("Close = %v before the loop took the file", err)
	}

	unlock()

	if err := <-closed; err != nil {
		t.Errorf("Close = %v, want nil", err)
	}

	if cut.Load() {
		t.Error("the batch's commit ran on a cancelled context: Close cut a batch it never waited for")
	}

	if n := done.Load(); n != 1 {
		t.Errorf("the write's Done ran %d times, want 1: its existence was committed", n)
	}

	if got := u.e.Pending(f); len(got) != 0 {
		t.Errorf("%d writes still pending after a clean Close", len(got))
	}
}

// A journal that turns read-only while the last group commit runs is not
// waited for: its syncs are refused, so the files would be asked again until
// the stop bound ended.
func TestCloseDoesNotWaitOnAJournalThatTurnsReadOnlyDuringTheLastGroupCommit(t *testing.T) {
	hold := &syncHold{entered: make(chan struct{}), release: make(chan struct{})}

	u := newUnitWith(t, unitConfig{wrap: func(h engine.Handle) engine.Handle {
		hold.Handle = h

		return hold
	}})

	t.Cleanup(func() {
		select {
		case <-hold.release:
		default:
			close(hold.release)
		}
	})

	f := u.create(t, "a")
	u.write(t, f, 0, []byte("abc"))

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)

	defer cancel()

	waiting := joinOf(u.e, "the last group commit")
	closed := closing(u.e, ctx)

	<-hold.entered
	reached(t, waiting, closed)
	u.j.ReadOnly(syscall.EROFS)
	close(hold.release)

	if err := <-closed; err != nil {
		t.Errorf("Close = %v, want nil: it waited on a read-only journal", err)
	}

	want := []string{handleClose(u.share), "journal close"}
	if got := u.ev.list(); !slices.Equal(got, want) {
		t.Errorf("events %v, want %v", got, want)
	}
}

// A batch still in its store commit on a read-only journal is cut by the
// stop, not waited out to the conflict bound.
func TestCloseCutsABatchInItsStoreCommitOnAReadOnlyJournal(t *testing.T) {
	var (
		entered = make(chan struct{})
		once    sync.Once
		seen    atomic.Pointer[error]
	)

	u := newUnit(t, onCommit(func(
		ctx context.Context,
		_ engine.Existence,
		_ []kvstore.PendingExistence,
		_ *kvstore.MarksWrite,
	) (map[entity.FileID]error, error) {
		once.Do(func() { close(entered) })

		<-ctx.Done()

		err := ctx.Err()
		seen.Store(&err)

		return nil, err
	}))
	f := u.create(t, "a")

	u.write(t, f, 0, []byte("abc"))

	finished := make(chan struct{})

	go func() {
		defer close(finished)

		_ = u.e.CommitExistence(context.Background(), u.share.ID, f)
	}()

	t.Cleanup(func() { <-finished })

	<-entered
	u.j.ReadOnly(syscall.EROFS)

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)

	defer cancel()

	if err := u.e.Close(ctx); err != nil {
		t.Errorf("Close = %v, want nil", err)
	}

	got := seen.Load()
	if got == nil || !errors.Is(*got, context.Canceled) {
		t.Errorf("the batch's context ended with %v, want it cancelled by the stop", got)
	}
}

// Close waits for every lifecycle call in progress, not only the first one it
// finds: the journal does not close under a second Bind.
func TestCloseWaitsForEveryLifecycleCallInProgress(t *testing.T) {
	fl := &faultLifecycle{}
	u := newUnitWith(t, unitConfig{unbound: true, deps: []func(*engine.Deps){faultDeps(fl)}})
	a := enginetest.AddShare(t, u.fs.Store, "a", 1<<30)
	b := enginetest.AddShare(t, u.fs.Store, "b", 1<<30)
	heldA := holdBind(t, u.e, fl, a)
	heldB := holdBind(t, u.e, fl, b)
	drained := seam(u.e.OnDraining)
	closed := closing(u.e, context.Background())

	reached(t, drained, closed)
	heldA.g.open()

	if err := <-heldA.done; !unavailable(err) {
		t.Errorf("the first Bind = %v, want ErrUnavailable", err)
	}

	select {
	case err := <-closed:
		t.Fatalf("Close = %v while the second Bind is in progress", err)
	case <-time.After(100 * time.Millisecond):
	}

	if slices.Contains(u.ev.list(), "journal close") {
		t.Fatal("the journal closed under the second Bind")
	}

	heldB.g.open()

	if err := <-heldB.done; !unavailable(err) {
		t.Errorf("the second Bind = %v, want ErrUnavailable", err)
	}

	if err := <-closed; err != nil {
		t.Errorf("Close = %v, want nil", err)
	}

	got := u.ev.list()
	if len(got) != 3 || got[2] != "journal close" {
		t.Errorf("events %v, want both handles closed and then the journal", got)
	}
}

// A removal batch that honours its context is cut by the stop: Close ends
// with no one releasing it.
func TestCloseCancelsARemovalBatchThatHonoursItsContext(t *testing.T) {
	u := newUnit(t)
	entered := make(chan struct{})
	stuck := make(chan struct{})

	t.Cleanup(func() { close(stuck) })

	if err := u.e.EnqueueHeld(u.share.ID, func(ctx context.Context) {
		close(entered)

		select {
		case <-ctx.Done():
			u.ev.add("batch cut")
		case <-stuck:
		}
	}, nil); err != nil {
		t.Fatal(err)
	}

	<-entered

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)

	defer cancel()

	if err := u.e.Close(ctx); err != nil {
		t.Fatalf("Close = %v, want nil: the stop did not cancel the batch", err)
	}

	want := []string{"batch cut", handleClose(u.share), "journal close"}
	if got := u.ev.list(); !slices.Equal(got, want) {
		t.Errorf("events %v, want %v", got, want)
	}
}

// The items a stop left are ended one at a time and the bound is checked
// between them: a Close whose bound ended stops ending, and a later Close
// ends the rest, each once.
func TestACloseThatRanOutOfBoundStopsEndingItemsAndALaterOneEndsTheRest(t *testing.T) {
	u := newUnit(t)
	h := newLoopHold()

	t.Cleanup(h.openAll)

	if err := u.e.EnqueueHeld(u.share.ID, h.run(u.ev), nil); err != nil {
		t.Fatal(err)
	}

	<-h.entered

	ctx, cancel := context.WithCancel(t.Context())

	defer cancel()

	var ended [2]atomic.Int32

	if err := u.e.EnqueueHeld(u.share.ID, func(context.Context) {}, func() {
		ended[0].Add(1)
		cancel()
	}); err != nil {
		t.Fatal(err)
	}

	if err := u.e.EnqueueHeld(u.share.ID, func(context.Context) {}, func() {
		ended[1].Add(1)
	}); err != nil {
		t.Fatal(err)
	}

	waiting := joinOf(u.e, "a removal loop")
	closed := closing(u.e, ctx)

	reached(t, waiting, closed)
	h.openAll()

	err := <-closed
	if !unavailable(err) || !strings.Contains(err.Error(), "removal loop") {
		t.Errorf("Close = %v, want ErrUnavailable naming the removal loop", err)
	}

	if a, b := ended[0].Load(), ended[1].Load(); a != 1 || b != 0 {
		t.Errorf("ended %d and %d items after the bound ended, want 1 and 0", a, b)
	}

	if err := u.e.Close(t.Context()); err != nil {
		t.Errorf("the later Close = %v, want nil", err)
	}

	if a, b := ended[0].Load(), ended[1].Load(); a != 1 || b != 1 {
		t.Errorf("ended %d and %d items after the later Close, want 1 and 1", a, b)
	}
}

// The last group commit overrides a file's retry backoff once: a backoff
// earned before the Close began would otherwise hold the closing batch off
// until the stop bound ended, and backoffMax equals that bound. A backoff
// earned by a batch the Close itself asked for is kept.
func TestTheLastGroupCommitOverridesAStaleRetryBackoff(t *testing.T) {
	var (
		cut  atomic.Bool
		done atomic.Int32
	)

	u := newUnit(t, onCommit(func(
		ctx context.Context,
		next engine.Existence,
		p []kvstore.PendingExistence,
		marks *kvstore.MarksWrite,
	) (map[entity.FileID]error, error) {
		if cut.CompareAndSwap(false, true) {
			errs := make(map[entity.FileID]error, len(p))
			for _, pe := range p {
				errs[pe.File] = errInjected
			}
			return errs, nil
		}
		return next.Commit(ctx, p, marks)
	}))

	f := u.create(t, "a")
	if _, err := u.e.Write(t.Context(), u.share.ID, f, 0, []byte("abc"),
		engine.WriteOpts{Done: func() { done.Add(1) }}); err != nil {
		t.Fatal(err)
	}

	// ExistenceAge is 24 h in a unit: once the wait is armed the file is
	// made due by advancing the clock, and its first batch fails, settling
	// a 100 ms backoff on the frozen clock. The backoff is settled state,
	// waited on as such, not a count of the loop's passes.
	eventually(t, "the group commit's timer is set", func() bool { return u.clock.Pending() > 0 })
	u.clock.Advance(25 * time.Hour)
	eventually(t, "the failed batch backs the file off", func() bool { return u.e.Failing(u.rec, f) })

	// The clock never moves again: only overriding the backoff can take
	// the file. A deadline of its own keeps the stop bound off the test
	// clock, which the test never advances.
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	closed := closing(u.e, ctx)

	if err := <-closed; err != nil {
		t.Errorf("Close = %v, want nil: the last group commit waited out a retry backoff", err)
	}
	if n := done.Load(); n != 1 {
		t.Errorf("%d Dones ran, want 1: the write's existence committed at the close", n)
	}
	if got := u.e.Pending(f); len(got) != 0 {
		t.Errorf("%d writes still pending after a clean Close", len(got))
	}
}

// A store that keeps failing keeps the backoff the Close's own batch earned:
// the closing engine overrides each file's backoff once, so a failing store
// is waited out in one backoff, not retried hot for the whole stop bound.
func TestAFailingStoreKeepsTheBackoffItsCloseEarned(t *testing.T) {
	var (
		commits atomic.Int32
		done    atomic.Int32
	)

	u := newUnit(t, onCommit(func(
		ctx context.Context,
		next engine.Existence,
		p []kvstore.PendingExistence,
		marks *kvstore.MarksWrite,
	) (map[entity.FileID]error, error) {
		commits.Add(1)

		errs := make(map[entity.FileID]error, len(p))
		for _, pe := range p {
			errs[pe.File] = errInjected
		}
		return errs, nil
	}))

	f := u.create(t, "a")
	if _, err := u.e.Write(t.Context(), u.share.ID, f, 0, []byte("abc"),
		engine.WriteOpts{Done: func() { done.Add(1) }}); err != nil {
		t.Fatal(err)
	}

	eventually(t, "the group commit's timer is set", func() bool { return u.clock.Pending() > 0 })
	u.clock.Advance(25 * time.Hour)
	eventually(t, "the failed batch backs the file off", func() bool { return u.e.Failing(u.rec, f) })

	if n := commits.Load(); n != 1 {
		t.Fatalf("%d commits before the Close, want only the backing-off one", n)
	}

	// The clock never moves, so the backoff the Close's own batch earns is
	// the last word: the bound ends at the deadline with the file still
	// backed off, as the stuck error reports — not hot-retried each round.
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	closed := closing(u.e, ctx)

	if err := <-closed; err == nil || !strings.Contains(err.Error(), "still running at the end of the stop bound") {
		t.Errorf("Close = %v, want the last group commit stuck at its bound", err)
	}
	if n := commits.Load(); n != 2 {
		t.Errorf("%d commits, want 2: the backing-off batch and one overridden retry, not a hot loop", n)
	}
	if n := done.Load(); n != 0 {
		t.Errorf("%d Dones ran, want 0: nothing committed against the failing store", n)
	}
}
