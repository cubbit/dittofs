package engine_test

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/block/engine"
	"github.com/cubbit/dittofs/pkg/block/engine/enginetest"
	"github.com/cubbit/dittofs/pkg/block/journal"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/metadata/kvstore"
	"github.com/cubbit/dittofs/pkg/vfs/vfserr"
)

// forgetEvent is the event a Forget of the share's tag records.
func forgetEvent(s engine.Share) string {
	return fmt.Sprintf("forget %d", s.Tag)
}

// dropped unbinds the unit's share and drops it, failing the test on an
// error.
func (u *unit) dropped(t *testing.T) {
	t.Helper()

	u.unbind(t)

	if err := u.e.Drop(t.Context(), u.share.ID); err != nil {
		t.Fatalf("Drop: %v", err)
	}
}

// wantEvents fails the test unless the unit's events are exactly want.
func (u *unit) wantEvents(t *testing.T, want ...string) {
	t.Helper()

	if got := u.ev.list(); !slices.Equal(got, want) {
		t.Errorf("events %v, want %v", got, want)
	}
}

// A Drop closes the handle, then forgets the tag, and keeps the share's
// binding as a removed one: every call still answers ErrStale, a repeated Drop
// succeeds and the handle is closed once.
func TestDropClosesTheHandleThenForgetsAndKeepsTheBindingRemoved(t *testing.T) {
	u := newUnit(t)
	f := u.create(t, "f")

	u.write(t, f, 0, []byte("abc"))
	u.dropped(t)
	u.wantEvents(t, handleClose(u.share), forgetEvent(u.share))

	if got := u.rec.forgets.Load(); got != 1 {
		t.Errorf("Forget ran %d times, want 1", got)
	}

	if _, ok := u.j.Stats().Shares[u.share.Tag]; ok {
		t.Error("the journal still names the tag after Drop")
	}

	h, err := u.j.Share(u.share.Tag, 0, journal.Version{})
	if err != nil {
		t.Fatal(err)
	}

	for id := range h.Files() {
		t.Errorf("the tag still holds file %v after Drop", id)
	}

	if err := h.Close(); err != nil {
		t.Fatal(err)
	}

	if _, err := u.e.Draw(t.Context(), u.share.ID, f); !isStale(err) {
		t.Errorf("Draw after Drop = %v, want exactly ErrStale", err)
	}

	if err := u.e.Bind(t.Context(), u.share); !isStale(err) {
		t.Errorf("Bind after Drop = %v, want exactly ErrStale", err)
	}

	if err := u.e.Drop(t.Context(), u.share.ID); err != nil {
		t.Errorf("a second Drop = %v, want nil", err)
	}

	if got := u.rec.forgets.Load(); got != 2 {
		t.Errorf("Forget ran %d times after the second Drop, want 2", got)
	}

	if err := u.e.Close(t.Context()); err != nil {
		t.Fatalf("Close: %v", err)
	}

	u.wantEvents(
		t,
		handleClose(u.share),
		forgetEvent(u.share),
		forgetEvent(u.share),
		"journal close",
	)
}

// A Drop of a share Start left removed closes no handle and forgets its tag
// once; the share still answers ErrStale.
func TestDropOfAShareStartLeftRemovedForgetsItsTagWithNoHandle(t *testing.T) {
	s, _, offloaded := startedWithRemovals(t)
	f := entity.NewFileID()

	if _, err := s.e.Draw(t.Context(), offloaded, f); !isStale(err) {
		t.Fatalf("Draw before Drop = %v, want exactly ErrStale", err)
	}

	if err := s.e.Drop(t.Context(), offloaded); err != nil {
		t.Fatalf("Drop = %v, want nil", err)
	}

	log := s.j.log()

	if got := countOf(log, "forget 92"); got != 1 {
		t.Errorf("forget 92 ran %d times in %v, want 1", got, log)
	}

	if s.j.called("close handle 92") {
		t.Errorf("Drop closed a handle of the removed share: %v", log)
	}

	if _, ok := s.j.Stats().Shares[92]; ok {
		t.Error("the journal still names tag 92 after Drop")
	}

	h, err := s.j.FakeJournal.Share(92, 0, journal.Version{})
	if err != nil {
		t.Fatal(err)
	}

	for id := range h.Files() {
		t.Errorf("the journal still holds file %v of tag 92 after Drop", id)
	}

	if _, err := s.e.Draw(t.Context(), offloaded, f); !isStale(err) {
		t.Errorf("Draw after Drop = %v, want exactly ErrStale", err)
	}
}

// A Drop of a share whose Unbind has not completed is refused with ErrInvalid
// and touches nothing: not a Serving share, not one whose Unbind ended on its
// context while a call was still inside it.
func TestDropRefusesAShareNotFullyUnboundAndTouchesNothing(t *testing.T) {
	t.Run("a serving share", func(t *testing.T) {
		u := newUnit(t)
		f := u.create(t, "f")

		if err := u.e.Drop(t.Context(), u.share.ID); !errors.Is(err, vfserr.ErrInvalid) {
			t.Errorf("Drop of a serving share = %v, want ErrInvalid", err)
		}

		if got := u.rec.open(); got != 1 {
			t.Errorf("open handles = %d, want 1", got)
		}

		if got := u.rec.forgets.Load(); got != 0 {
			t.Errorf("Forget ran %d times, want 0", got)
		}

		if err := u.writeTo(t, f); err != nil {
			t.Errorf("Write after the refused Drop = %v, want nil", err)
		}
	})

	t.Run("an unbind that ended on its context", func(t *testing.T) {
		c := holdAWrite(t)
		u := c.u
		cancelled, cancel := context.WithCancel(t.Context())
		cancel()

		if err := u.e.Unbind(cancelled, u.share.ID, false); !errors.Is(err, context.Canceled) {
			t.Fatalf("Unbind under a cancelled context = %v, want context.Canceled", err)
		}

		if err := u.e.Drop(t.Context(), u.share.ID); !errors.Is(err, vfserr.ErrInvalid) {
			t.Errorf("Drop with a call still inside = %v, want ErrInvalid", err)
		}

		if got := u.rec.open(); got != 1 {
			t.Errorf("open handles = %d, want 1", got)
		}

		if got := u.rec.forgets.Load(); got != 0 {
			t.Errorf("Forget ran %d times, want 0", got)
		}
	})
}

// A Drop of a share with no binding answers nil and calls no journal method.
func TestDropOfAShareWithNoBindingAnswersNil(t *testing.T) {
	u := newUnit(t)

	if err := u.e.Drop(t.Context(), entity.NewShareID()); err != nil {
		t.Errorf("Drop of a share with no binding = %v, want nil", err)
	}

	if got := u.rec.forgets.Load(); got != 0 {
		t.Errorf("Forget ran %d times, want 0", got)
	}

	u.wantEvents(t)
}

// After Close began, Drop answers what a content call does.
func TestDropAfterCloseAnswersClosed(t *testing.T) {
	u := newUnit(t)
	f := u.create(t, "f")

	u.unbind(t)

	if err := u.e.Close(t.Context()); err != nil {
		t.Fatal(err)
	}

	_, want := u.e.Write(t.Context(), u.share.ID, f, 0, []byte("x"), engine.WriteOpts{})
	got := u.e.Drop(t.Context(), u.share.ID)

	if got == nil || want == nil || got.Error() != want.Error() || !unavailable(got) {
		t.Errorf("Drop after Close = %v, want what Write answers: %v, an ErrUnavailable", got, want)
	}

	if errors.Is(got, engine.ErrUnbuilt) {
		t.Error("Drop answers the stub's error")
	}

	if n := u.rec.forgets.Load(); n != 0 {
		t.Errorf("Forget ran %d times after Close, want 0", n)
	}
}

// held is a unit whose share is unbound while a clone item of it is held
// running in the loop, and the Drop that waits for that item.
func dropHeldByAnItem(t *testing.T) (*unit, *loopItem, <-chan error) {
	t.Helper()

	u := newUnit(t)

	u.unbind(t)

	held := u.postItem(u.share.ID, entity.RemoveClone, true, true)

	t.Cleanup(func() {
		select {
		case <-held.release:
		default:
			close(held.release)
		}
	})

	<-held.entered

	joined := joinOf(u.e, "a removal batch")
	done := make(chan error, 1)

	go func() {
		done <- u.e.Drop(t.Context(), u.share.ID)
	}()

	reached(t, joined, done)

	return u, held, done
}

// A Drop waits for the item the loop is running for the share, and ends it
// once.
func TestDropJoinsTheRunningItemOfTheShareAndEndsIt(t *testing.T) {
	u, held, done := dropHeldByAnItem(t)

	if got := u.rec.forgets.Load(); got != 0 {
		t.Errorf("Forget ran %d times while Drop waits, want 0", got)
	}

	close(held.release)

	if err := <-done; err != nil {
		t.Fatalf("Drop = %v, want nil", err)
	}

	if got := held.ended.Load(); got != 1 {
		t.Errorf("the item ended %d times, want 1", got)
	}
}

// Close waits for a Drop in progress and closes the journal only after the
// Drop closed the handle.
func TestCloseWaitsForADropInProgress(t *testing.T) {
	u, held, done := dropHeldByAnItem(t)
	past, cancel := context.WithDeadline(t.Context(), time.Now().Add(-time.Second))

	defer cancel()

	err := u.e.Close(past)
	if !unavailable(err) {
		t.Errorf("Close = %v, want ErrUnavailable naming the lifecycle calls", err)
	}

	u.wantEvents(t)
	close(held.release)

	if err := <-done; err != nil {
		t.Fatalf("Drop = %v, want nil", err)
	}

	if err := u.e.Close(t.Context()); err != nil {
		t.Fatalf("second Close = %v, want nil", err)
	}

	u.wantEvents(t, handleClose(u.share), forgetEvent(u.share), "journal close")
}

// The items queued for an unbound share are ended by Drop, once each and
// never run; the clone item that ran before it is not ended.
func TestADropEndsTheParkedItemsOfTheShareOnce(t *testing.T) {
	u := newUnit(t)
	other := u.bindShare(t, "other", entity.NamespaceID{})
	block := u.post(other, entity.RemoveTruncate, true)

	t.Cleanup(func() {
		select {
		case <-block.release:
		default:
			close(block.release)
		}
	})

	<-block.entered

	parked := u.post(u.share.ID, entity.RemoveRelease, false)
	clone := u.post(u.share.ID, entity.RemoveClone, false)

	u.unbind(t)
	close(block.release)
	<-clone.entered

	if err := u.e.Drop(t.Context(), u.share.ID); err != nil {
		t.Fatalf("Drop = %v, want nil", err)
	}

	if got := parked.ran.Load(); got != 0 {
		t.Errorf("the parked item ran %d times, want 0", got)
	}

	if got := parked.ended.Load(); got != 1 {
		t.Errorf("the parked item ended %d times, want 1", got)
	}

	if got := clone.ended.Load(); got != 0 {
		t.Errorf("the clone item that finished ended %d times, want 0", got)
	}
}

// writesOver writes n bytes to each of files through res and answers nothing:
// every write is accepted.
func (u *unit) writesOver(
	t *testing.T,
	res *enginetest.Reservations,
	files []entity.FileID,
	n int,
) {
	t.Helper()

	for _, f := range files {
		for i := range n {
			o, answered := res.Opts()
			_, err := u.e.Write(t.Context(), u.share.ID, f, int64(i), []byte("x"), o)

			answered(err)

			if err != nil {
				t.Fatalf("write: %v", err)
			}
		}
	}
}

// A Drop with a batch due as it joins calls each Done once: the batch's files
// by the batch, the rest by the Drop, and nothing is left in the entries.
func TestDropUnderWritesWithABatchDueAsItJoinsCallsEachDoneOnce(t *testing.T) {
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

	t.Cleanup(func() {
		open.Do(func() {
			close(release)
		})
	})

	if err := u.e.Apply(engine.Settings{
		ExistenceAge:        time.Hour,
		GroupCommitMaxFiles: 2,
		CloneMaxLen:         1 << 30,
	}); err != nil {
		t.Fatal(err)
	}

	res := enginetest.NewReservations(t)
	files := []entity.FileID{u.create(t, "a"), u.create(t, "b"), u.create(t, "c"), u.create(t, "d")}

	u.writesOver(t, res, files, 2)
	u.clock.Advance(2 * time.Hour)
	<-entered
	u.unbind(t)

	joined := joinOf(u.e, "a group commit batch")
	done := make(chan error, 1)

	go func() {
		done <- u.e.Drop(t.Context(), u.share.ID)
	}()

	select {
	case <-joined:
	case err := <-done:
		t.Fatalf("Drop = %v with a batch in flight, want it to wait", err)
	}

	open.Do(func() {
		close(release)
	})

	if err := <-done; err != nil {
		t.Fatalf("Drop = %v, want nil", err)
	}

	if got := res.Outstanding(); got != 0 {
		t.Errorf("writes without their Done after Drop = %d, want 0", got)
	}

	u.wantEvents(t, "batch commit returned", handleClose(u.share), forgetEvent(u.share))

	if got := u.e.FileEntries(); got != 0 {
		t.Errorf("file entries after Drop = %d, want 0", got)
	}
}

// A Forget that fails leaves the writes to the retry: no Done ran, the handle
// was closed once, and the retry runs each Done once.
func TestAFailedForgetLeavesTheWritesToTheRetry(t *testing.T) {
	u := newUnit(t)
	res := enginetest.NewReservations(t)
	files := []entity.FileID{u.create(t, "a"), u.create(t, "b"), u.create(t, "c"), u.create(t, "d")}

	u.writesOver(t, res, files, 2)
	u.unbind(t)

	u.rec.mu.Lock()
	u.rec.failForgets = 1
	u.rec.mu.Unlock()

	if err := u.e.Drop(t.Context(), u.share.ID); err == nil {
		t.Fatal("Drop with a failing Forget = nil, want its error")
	}

	if got := res.Outstanding(); got != 8 {
		t.Errorf("writes without their Done after the failed Drop = %d, want 8", got)
	}

	u.wantEvents(t, handleClose(u.share), forgetEvent(u.share))

	if err := u.e.Drop(t.Context(), u.share.ID); err != nil {
		t.Fatalf("the retried Drop = %v, want nil", err)
	}

	if got := res.Outstanding(); got != 0 {
		t.Errorf("writes without their Done after the retry = %d, want 0", got)
	}

	if got := u.rec.forgets.Load(); got != 2 {
		t.Errorf("Forget ran %d times, want 2", got)
	}

	if err := u.e.Close(t.Context()); err != nil {
		t.Fatalf("Close: %v", err)
	}

	u.wantEvents(
		t,
		handleClose(u.share),
		forgetEvent(u.share),
		forgetEvent(u.share),
		"journal close",
	)
}

// A Drop takes the share's files off the group commit's due set: a batch that
// commits afterwards sees another share's files and none of its.
func TestDropTakesTheSharesFilesOffTheDueSetAndNoLaterBatchCapturesOne(t *testing.T) {
	var (
		mu   sync.Mutex
		seen []entity.FileID
	)

	batch := make(chan struct{}, 8)

	u := newUnit(t, onCommit(func(
		ctx context.Context,
		next engine.Existence,
		p []kvstore.PendingExistence,
		marks *kvstore.MarksWrite,
	) (map[entity.FileID]error, error) {
		mu.Lock()

		for _, x := range p {
			seen = append(seen, x.File)
		}

		mu.Unlock()

		batch <- struct{}{}

		return next.Commit(ctx, p, marks)
	}))
	if err := u.e.Apply(engine.Settings{
		ExistenceAge:        time.Second,
		GroupCommitMaxFiles: 64,
		CloneMaxLen:         1 << 30,
	}); err != nil {
		t.Fatal(err)
	}

	other := u.bindShare(t, "other", entity.NamespaceID{})
	mine := []entity.FileID{u.create(t, "a"), u.create(t, "b")}
	theirs := entity.NewFileID()

	_, err := u.e.Write(t.Context(), other, theirs, 0, []byte("x"), engine.WriteOpts{})
	if err != nil {
		t.Fatal(err)
	}

	u.writesOver(t, enginetest.NewReservations(t), mine, 1)
	u.dropped(t)

	u.clock.Advance(2 * time.Second)
	<-batch

	mu.Lock()
	defer mu.Unlock()

	if !slices.Contains(seen, theirs) {
		t.Errorf("the batch saw %v, want the other share's file %v", seen, theirs)
	}

	for _, f := range mine {
		if slices.Contains(seen, f) {
			t.Errorf("a batch captured the dropped share's file %v", f)
		}
	}
}

// A Drop ends the retry of a release that stalled: its channel yields
// ErrUnavailable once, no Done runs for it, nothing is retried again, and no
// condition names the share afterwards.
func TestDropEndsAReleasesRetryWithUnavailableAndNoDone(t *testing.T) {
	r := newRig(t)

	r.begin(t)
	r.run()

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

	for !r.e.RemovalStalled(f) {
		r.clock.Advance(time.Second)
		runtime.Gosched()
	}

	if !namesShare(r.e.Stats(), r.one.ID, "removal_stalled") {
		t.Fatal("Stats does not name removal_stalled for the share before Drop")
	}

	if err := r.e.Unbind(ctx, r.one.ID, false); err != nil {
		t.Fatalf("Unbind: %v", err)
	}

	if err := r.e.Drop(ctx, r.one.ID); err != nil {
		t.Fatalf("Drop: %v", err)
	}

	select {
	case got := <-retry:
		if !unavailable(got) {
			t.Errorf("the release yields %v, want ErrUnavailable", got)
		}
	default:
		t.Error("the release's channel yields nothing after Drop")
	}

	select {
	case extra := <-retry:
		t.Errorf("the release yields a second value %v", extra)
	default:
	}

	if got := r.e.RemovalsPending(); got != 0 {
		t.Errorf("removal items left on the loop after Drop = %d, want 0", got)
	}

	removes := len(r.calls(0, "remove"))

	r.clock.Advance(time.Minute)

	if got := len(r.calls(0, "remove")); got != removes {
		t.Errorf("Remove calls after Drop and a minute later = %d, want %d", got, removes)
	}

	if got := res.Outstanding(); got != 0 {
		t.Errorf("writes without their Done after Drop = %d, want 0", got)
	}

	if got := r.e.FileEntries(); got != 0 {
		t.Errorf("file entries after Drop = %d, want 0: the file's entry or guard is left", got)
	}

	if namesShare(r.e.Stats(), r.one.ID, "removal_stalled") {
		t.Error("Stats still names removal_stalled for the dropped share")
	}
}

// A file whose commit failed alone is named by existence_commit_failing; a
// Drop of its share clears the condition.
func TestDropClearsTheCommitFailingConditionOfItsFiles(t *testing.T) {
	u := newUnit(t)
	_, dir := u.fs.Create(t, u.fs.Admin, u.fs.Root, "d", kvstore.Dir(0o755))

	if err := u.e.Apply(engine.Settings{
		ExistenceAge:        time.Second,
		GroupCommitMaxFiles: 64,
		CloneMaxLen:         1 << 30,
	}); err != nil {
		t.Fatal(err)
	}

	u.write(t, dir.ID, 0, []byte("not content"))

	for !u.e.Failing(u.rec, dir.ID) {
		u.clock.Advance(time.Second)
		runtime.Gosched()
	}

	if !namesShare(u.e.Stats(), u.share.ID, "existence_commit_failing") {
		t.Fatal("Stats does not name existence_commit_failing before Drop")
	}

	u.dropped(t)

	if namesShare(u.e.Stats(), u.share.ID, "existence_commit_failing") {
		t.Error("Stats still names existence_commit_failing for the dropped share")
	}
}

// namesShare reports whether a condition called name is about share id.
func namesShare(st engine.Stats, id entity.ShareID, name string) bool {
	return slices.ContainsFunc(st.Shares[id].Conditions, func(c engine.Condition) bool {
		return c.Name == name
	})
}

// A close of the handle that fails without closing it, as the journal's does,
// leaves the binding, the tag and the writes as they were; the retry closes it
// again and goes on, and a Close with no retry closes it itself.
func TestADropWhoseHandleCloseFailsWithoutClosingKeepsTheBindingAndSkipsForget(t *testing.T) {
	t.Run("the retry", func(t *testing.T) {
		u := newUnit(t)
		res := enginetest.NewReservations(t)

		u.writesOver(t, res, []entity.FileID{u.create(t, "a")}, 1)
		u.unbind(t)
		u.rec.handle(0).failNext.Store(true)

		if err := u.e.Drop(t.Context(), u.share.ID); !unavailable(err) {
			t.Fatalf("Drop with a failing close = %v, want ErrUnavailable from the close", err)
		}

		if _, err := u.e.Draw(t.Context(), u.share.ID, entity.NewFileID()); !isStale(err) {
			t.Errorf("Draw after the failed Drop = %v, want exactly ErrStale", err)
		}

		if got := u.rec.forgets.Load(); got != 0 {
			t.Errorf("Forget ran %d times after a failed close, want 0", got)
		}

		if got := res.Outstanding(); got != 1 {
			t.Errorf("writes without their Done after the failed Drop = %d, want 1", got)
		}

		if err := u.e.Drop(t.Context(), u.share.ID); err != nil {
			t.Fatalf("the retried Drop = %v, want nil", err)
		}

		u.wantEvents(t, handleClose(u.share), handleClose(u.share), forgetEvent(u.share))

		if got := u.rec.forgets.Load(); got != 1 {
			t.Errorf("Forget ran %d times, want 1", got)
		}
	})

	t.Run("a close with no retry", func(t *testing.T) {
		u := newUnit(t)

		u.unbind(t)
		u.rec.handle(0).failNext.Store(true)

		if err := u.e.Drop(t.Context(), u.share.ID); !unavailable(err) {
			t.Fatalf("Drop with a failing close = %v, want ErrUnavailable from the close", err)
		}

		if err := u.e.Close(t.Context()); err != nil {
			t.Fatalf("Close = %v, want nil", err)
		}

		u.wantEvents(t, handleClose(u.share), handleClose(u.share), "journal close")
	})
}

// A close that fails after it closed the handle, as the test journal's does,
// answers journal.ErrClosed to the retry, which Drop takes as closed.
func TestADropWhoseHandleCloseFailedAfterClosingTreatsErrClosedAsClosed(t *testing.T) {
	u := newUnit(t)
	h := u.rec.handle(0)

	h.closeErr = errInjected

	u.unbind(t)

	if err := u.e.Drop(t.Context(), u.share.ID); !unavailable(err) {
		t.Fatalf("Drop with a failing close = %v, want ErrUnavailable from the close", err)
	}

	if got := u.rec.forgets.Load(); got != 0 {
		t.Errorf("Forget ran %d times after a failed close, want 0", got)
	}

	h.closeErr = nil

	if err := u.e.Drop(t.Context(), u.share.ID); err != nil {
		t.Fatalf("the retried Drop = %v, want nil: journal.ErrClosed is a closed handle", err)
	}

	u.wantEvents(t, handleClose(u.share), handleClose(u.share), forgetEvent(u.share))

	if got := u.rec.forgets.Load(); got != 1 {
		t.Errorf("Forget ran %d times, want 1", got)
	}

	if _, err := u.e.Draw(t.Context(), u.share.ID, entity.NewFileID()); !isStale(err) {
		t.Errorf("Draw after the Drop = %v, want exactly ErrStale", err)
	}
}

// countOf is how many times what is in log.
func countOf(log []string, what string) int {
	n := 0

	for _, l := range log {
		if l == what {
			n++
		}
	}

	return n
}

// A release admitted before Unbind whose phase 1 fails while Unbind joins
// posts its retry to a loop that refuses the share's items: the retry is
// parked, and Drop ends it, with ErrUnavailable once, leaving neither the
// file's entry nor its guard behind. The file holds no pending write, so the
// group commit's due set does not name it.
func TestADeleteWhoseRetryIsPostedWhileUnbindJoinsIsEndedByDrop(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})

	var once sync.Once

	u := newUnit(t, onRemove(func(
		context.Context,
		engine.Existence,
		entity.ShareID,
		entity.FileID,
		uint64,
		kvstore.RemoveOp,
		[]kvstore.PendingOp,
	) error {
		once.Do(func() {
			close(entered)
		})

		<-release

		return kvstore.ErrConflict
	}))

	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
	})

	f := u.create(t, "a")
	names := u.fs.Store.Namespace()

	if _, err := names.Unlink(t.Context(), u.fs.Admin, u.fs.Root, []byte("a")); err != nil {
		t.Fatal(err)
	}

	type deleted struct {
		retry <-chan error
		err   error
	}

	answer := make(chan deleted, 1)

	go func() {
		retry, err := u.e.Delete(context.Background(), u.share.ID, f)
		answer <- deleted{retry, err}
	}()

	<-entered

	joined := joins(u.e)
	unbound := u.unbinding(t)

	select {
	case <-joined:
	case err := <-unbound:
		t.Fatalf("Unbind = %v with a Delete inside it, want it to wait", err)
	}

	close(release)

	d := <-answer
	if d.err != nil || d.retry == nil {
		t.Fatalf("Delete = %v, %v; want a pending channel and nil", d.retry, d.err)
	}

	if err := <-unbound; err != nil {
		t.Fatalf("Unbind = %v, want nil", err)
	}

	if err := u.e.Drop(t.Context(), u.share.ID); err != nil {
		t.Fatalf("Drop = %v, want nil", err)
	}

	if got := u.e.FileEntries(); got != 0 {
		t.Errorf("file entries after Drop = %d, want 0: the file's entry or guard is left", got)
	}

	select {
	case got := <-d.retry:
		if !unavailable(got) {
			t.Errorf("the release yields %v, want ErrUnavailable", got)
		}
	default:
		t.Error("the release's channel yields nothing after Drop")
	}

	select {
	case extra := <-d.retry:
		t.Errorf("the release yields a second value %v", extra)
	default:
	}
}
