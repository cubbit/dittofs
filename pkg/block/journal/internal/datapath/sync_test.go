package datapath

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/format"
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

	if err := c.Sync(context.Background(), s, f, g); err != nil {
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

	err := c.Sync(context.Background(), s, f)
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
	if err := c.Sync(context.Background(), s, f); err != nil {
		t.Fatalf("the Sync after the report = %v, want nil: a failed window is reported once", err)
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
		err := c.Sync(context.Background(), b, ids...)
		var other *core.OtherShareError
		if !errors.As(err, &other) {
			t.Fatalf("Sync through the other share (%s) = %v, want an OtherShareError", name, err)
		}
		if !errors.Is(err, core.ErrOtherShare) {
			t.Fatalf("Sync through the other share (%s) = %v, want it to match ErrOtherShare", name, err)
		}
	}
	if err := c.Sync(context.Background(), a, f); !errors.Is(err, core.ErrSyncFailed) {
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
	if err := c.Sync(done, b, f); !errors.Is(err, context.Canceled) {
		t.Fatalf("Sync with a done context of another share's file = %v, want context.Canceled before the share check", err)
	}
	if err := c.Sync(done, a, f); !errors.Is(err, context.Canceled) {
		t.Fatalf("Sync with a done context = %v, want context.Canceled", err)
	}
	if err := c.Sync(context.Background(), a, f); !errors.Is(err, core.ErrSyncFailed) {
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
	if err := c.Sync(context.Background(), s, testFile(1)); !errors.Is(err, core.ErrClosed) {
		t.Fatalf("Sync through a closed handle = %v, want ErrClosed", err)
	}
}
