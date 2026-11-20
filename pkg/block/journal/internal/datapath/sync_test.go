package datapath

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/format"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/segment"
	"github.com/cubbit/dittofs/pkg/block/journal/journaltest/kit"
)

// filesOnTwoStreams returns two files the core places on different streams,
// so a sync of one never covers the other's record.
func filesOnTwoStreams(t *testing.T, c *Core) (core.FileID, core.FileID) {
	t.Helper()
	f := testFile(1)
	for b := byte(2); b != 0; b++ {
		if g := testFile(b); c.Streams().IndexFor(g) != c.Streams().IndexFor(f) {
			return f, g
		}
	}
	t.Fatal("no file lands on a stream other than the first file's")
	return f, f
}

// failWindowOf fails the next sync of the segment holding f's write at 0, and
// every read of it, so the window cannot re-append the write and fails it.
// The rules are cleared when the test ends.
func failWindowOf(t *testing.T, c *Core, seam *kit.Seam, f core.FileID) {
	t.Helper()
	seg := format.SegmentName(recordOf(t, c, f, 0).Record.Segment)
	if err := seam.Schedule(
		kit.Failing(kit.OpSync, seg, 1, errors.New("sync boom")),
		kit.Failing(kit.OpRead, seg, 0, errors.New("read boom")),
	); err != nil {
		t.Fatalf("the seam refused the failing sync and read rules: %v", err)
	}
	t.Cleanup(func() { _ = seam.Schedule() })
}

// TestSyncMakesEveryNamedFileSurviveACrash writes two files on two streams
// with the sync timer stopped, syncs both, and loses every unsynced write
// beneath the core: both writes are in the durable image.
func TestSyncMakesEveryNamedFileSurviveACrash(t *testing.T) {
	c, seam := newTestCore(t)
	s := mustShare(t, c, 1)
	f, g := filesOnTwoStreams(t, c)
	vf := mustWrite(t, c, s, f, 0, []byte("ffff"))
	vg := mustWrite(t, c, s, g, 0, []byte("gggg"))

	if _, err := c.Sync(context.Background(), s, f, g); err != nil {
		t.Fatalf("Sync(f, g) = %v, want nil", err)
	}
	crashLosingUnsynced(t, seam)
	for _, w := range []struct {
		name string
		id   core.FileID
		v    core.Version
	}{{"f", f, vf}, {"g", g, vg}} {
		recs := fileWrites(t, seam, w.id)
		if !slices.ContainsFunc(recs, func(r onDisk) bool { return r.Header.Version == w.v }) {
			t.Errorf("after Sync and a crash, %s's write at %v is not on disk (records: %d)", w.name, w.v, len(recs))
		}
	}
}

// TestSyncReportsAFailedWindowOnce fails the window holding f's write: the
// Sync that waited on it returns a SyncError naming f, never nil, and the
// next Sync of f, the report taken, succeeds.
func TestSyncReportsAFailedWindowOnce(t *testing.T) {
	c, seam := newTestCore(t)
	s := mustShare(t, c, 1)
	f := testFile(1)
	mustWrite(t, c, s, f, 0, []byte("ffff"))
	failWindowOf(t, c, seam, f)

	_, err := c.Sync(context.Background(), s, f)
	var se *core.SyncError
	if !errors.As(err, &se) {
		t.Fatalf("Sync over a failed window = %v, want a *SyncError", err)
	}
	if !slices.Contains(se.Files, f) {
		t.Fatalf("SyncError.Files = %v, want it to name %v", se.Files, f)
	}
	if err := seam.Schedule(); err != nil {
		t.Fatalf("the seam refused to clear its rules: %v", err)
	}
	if _, err := c.Sync(context.Background(), s, f); err != nil {
		t.Fatalf("the Sync after the report = %v, want nil: a failed window is reported once", err)
	}
}

// TestTheCoresSpareWakeAnswersAParkedWindow covers the core's own wake wiring,
// the pool.OnSpare(d.SpareLanded) of its build: a content record of a failed
// window whose re-append the worker's pass could not roll — no content spare,
// the makes the shortage kicks held at the seam — is re-appended the moment
// the held make completes and the pool lands its spare, without a tick of the
// sync bound the core's frozen clock never advances. The Sync waiting on the
// window answers on the wake alone; unwire the core's line and this test
// hangs that Sync instead.
func TestTheCoresSpareWakeAnswersAParkedWindow(t *testing.T) {
	c, seam := newTestCore(t)
	s := mustShare(t, c, 1)
	f := testFile(1)
	mustWrite(t, c, s, f, 0, []byte("ffff"))

	// The write's roll took a spare and kicked a make; rest first, so the
	// hold below goes up with every ready spare landed and no make in
	// flight — a make already past its create would land beside the hold and
	// serve the roll the withheld spares exist to refuse. The drain then
	// takes every ready content spare; the make its takes kick parks at the
	// held create, so none can land until the hold is released.
	awaitSpares(t, c, 0)
	made := seam.Hold(t, kit.OpCreate, "*.spare.tmp")
	for {
		_, err := c.pool.Take(segment.Content)
		var short *core.NoSpaceError
		if errors.As(err, &short) {
			break
		}
		if err != nil {
			t.Fatalf("Take of a content spare: %v", err)
		}
	}
	select {
	case <-made.Reached():
	case <-time.After(5 * time.Second):
		t.Fatal("the pool's spare make never reached the seam")
	}

	// The write's sync fails and its window parks: the re-append's roll is
	// refused for want of a content spare, and the Sync waits on the parked
	// window rather than answering — the write is re-appended, not failed, so
	// the failure reaches the Sync only if the window is failed whole.
	seg := format.SegmentName(recordOf(t, c, f, 0).Record.Segment)
	if err := seam.Schedule(kit.Failing(kit.OpSync, seg, 1, errors.New("sync boom"))); err != nil {
		t.Fatalf("the seam refused the failing sync rule: %v", err)
	}
	t.Cleanup(func() { _ = seam.Schedule() })
	done := make(chan error, 1)
	// The Sync the test parks answers a cancelled context, so a missing wake
	// fails the test at its own deadline instead of hanging its cleanup on
	// the wedged call.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		_, err := c.Sync(ctx, s, f)
		done <- err
	}()
	select {
	case err := <-done:
		t.Fatalf("the Sync over the parked window returned (%v) while no spare could land", err)
	case <-time.After(30 * time.Millisecond):
	}

	// The spare lands and the wake the core wired answers the parked window.
	made.Release()
	select {
	case err := <-done:
		if err != nil {
			var se *core.SyncError
			if errors.As(err, &se) {
				t.Fatalf("Sync over the re-appended window = %v, want nil", se)
			}
			t.Fatalf("Sync over the re-appended window = %v, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the Sync over the parked window did not return once the spare landed")
	}
	if got, want := c.Durability().Stats().ReappendedBytes, uint64(format.RecordSize(format.KindWrite, int64(len("ffff")))); got != want {
		t.Errorf("ReappendedBytes = %d over the re-appended write, want %d", got, want)
	}
}

// TestAClosedHoldFreesTheParkedWindow covers the other door a parked window
// waits behind: a hold that owes every content spare keeps the re-append's
// unreserved take refused — no spare is missing from the pool, it is only
// held, and the pool refills an open hold's spares beside its target, so the
// makes are withheld at the seam to keep the park more than a race with the
// refills. Hold.Close frees what it holds with no landing of anything, and
// the wake Close fires answers the parked window without a tick of the sync
// bound, so the Sync waiting on it returns at once instead of at the bound
// the core's frozen clock never advances.
func TestAClosedHoldFreesTheParkedWindow(t *testing.T) {
	c, seam := newTestCore(t)
	s := mustShare(t, c, 1)
	f := testFile(1)
	mustWrite(t, c, s, f, 0, []byte("ffff"))

	// The write's roll took a spare and kicked a make; rest first, so no make
	// is in flight when the withheld ones go up — a make already past its
	// create would land beside the hold and serve the take the hold exists to
	// refuse. At rest every ready content spare is landed, so holding them
	// all leaves an unreserved take nothing to draw.
	awaitSpares(t, c, 0)
	seam.Hold(t, kit.OpCreate, "*.spare.tmp")
	hold, err := c.pool.Hold(int(c.pool.Counters().Spares))
	if err != nil {
		var ns *core.NoSpaceError
		if errors.As(err, &ns) {
			t.Fatalf("Hold of the %d ready spares was refused: %+v", c.pool.Counters().Spares, ns)
		}
		t.Fatalf("Hold: %v", err)
	}
	t.Cleanup(hold.Close)

	seg := format.SegmentName(recordOf(t, c, f, 0).Record.Segment)
	if err := seam.Schedule(kit.Failing(kit.OpSync, seg, 1, errors.New("sync boom"))); err != nil {
		t.Fatalf("the seam refused the failing sync rule: %v", err)
	}
	t.Cleanup(func() { _ = seam.Schedule() })
	done := make(chan error, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		_, err := c.Sync(ctx, s, f)
		done <- err
	}()
	select {
	case err := <-done:
		t.Fatalf("the Sync over the parked window returned (%v) while a hold owed every spare", err)
	case <-time.After(30 * time.Millisecond):
	}

	// Closing the hold frees the spares for the unreserved take and fires
	// the spare wake the pool's Close runs, the same door a landing opens.
	hold.Close()
	select {
	case err := <-done:
		if err != nil {
			var se *core.SyncError
			if errors.As(err, &se) {
				t.Fatalf("Sync over the re-appended window = %v, want nil", se)
			}
			t.Fatalf("Sync over the re-appended window = %v, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the Sync over the parked window did not return once the hold closed")
	}
	if got, want := c.Durability().Stats().ReappendedBytes, uint64(format.RecordSize(format.KindWrite, int64(len("ffff")))); got != want {
		t.Errorf("ReappendedBytes = %d over the re-appended write, want %d", got, want)
	}
}

// TestSyncRefusesAnotherSharesFileAndKeepsItsReport syncs, through share B,
// a file share A owns whose next window fails, alone and beside B's own file:
// each call is refused with ErrOtherShare, and A's next Sync gets the failure,
// so the refused calls neither synced the file nor took its report.
func TestSyncRefusesAnotherSharesFileAndKeepsItsReport(t *testing.T) {
	c, seam := newTestCore(t)
	a := mustShare(t, c, 1)
	b := mustShare(t, c, 2)
	f, g := filesOnTwoStreams(t, c)
	mustWrite(t, c, a, f, 0, []byte("ffff"))
	mustWrite(t, c, b, g, 0, []byte("gggg"))
	failWindowOf(t, c, seam, f)
	for name, ids := range map[string][]core.FileID{"alone": {f}, "beside its own": {g, f}} {
		_, err := c.Sync(context.Background(), b, ids...)
		var other *core.OtherShareError
		if !errors.As(err, &other) {
			t.Fatalf("Sync through the other share (%s) = %v, want an OtherShareError", name, err)
		}
		if !errors.Is(err, core.ErrOtherShare) {
			t.Fatalf("Sync through the other share (%s) = %v, want it to match ErrOtherShare", name, err)
		}
	}
	if _, err := c.Sync(context.Background(), a, f); !errors.Is(err, core.ErrSyncFailed) {
		t.Fatalf("the owner's Sync after the refused ones = %v, want the failed window's SyncError", err)
	}
}

// TestSyncChecksTheContextFirst calls Sync with a done context on another
// share's file and on a file whose next window fails: each returns the
// context's error before any other check, and the failure is reported to the
// next Sync, so the cancelled calls took nothing.
func TestSyncChecksTheContextFirst(t *testing.T) {
	c, seam := newTestCore(t)
	a := mustShare(t, c, 1)
	b := mustShare(t, c, 2)
	f := testFile(1)
	mustWrite(t, c, a, f, 0, []byte("ffff"))
	failWindowOf(t, c, seam, f)
	done, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.Sync(done, b, f); !errors.Is(err, context.Canceled) {
		t.Fatalf("Sync with a done context of another share's file = %v, want context.Canceled before the share check", err)
	}
	if _, err := c.Sync(done, a, f); !errors.Is(err, context.Canceled) {
		t.Fatalf("Sync with a done context = %v, want context.Canceled", err)
	}
	if _, err := c.Sync(context.Background(), a, f); !errors.Is(err, core.ErrSyncFailed) {
		t.Fatalf("the Sync after the cancelled ones = %v, want the failed window's SyncError", err)
	}
}

// TestSyncRefusesAClosedHandle closes a handle and syncs through it.
func TestSyncRefusesAClosedHandle(t *testing.T) {
	c, _ := newTestCore(t)
	s := mustShare(t, c, 1)
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := c.Sync(context.Background(), s, testFile(1)); !errors.Is(err, core.ErrClosed) {
		t.Fatalf("Sync through a closed handle = %v, want ErrClosed", err)
	}
}
