package journal_test

import (
	"context"
	"errors"
	"syscall"
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/block/journal"
	"github.com/cubbit/dittofs/pkg/block/journal/journaltest"
	"github.com/cubbit/dittofs/pkg/block/journal/journaltest/kit"
)

// readOnlyFiles are the files readOnlyFixture leaves: a written and synced,
// and r written, truncated and the truncate synced, all before the
// transition; b written in the window that failed, not yet told; u written
// and synced, then truncated in that window, so the transition undid its
// truncate.
type readOnlyFiles struct{ a, r, b, u journal.FileID }

// readOnlyFixture opens a journal and lays out readOnlyFiles. The transition
// is u's truncate: a removal waits until its record is durable, so its wait
// meets the EROFS of the sync and turns the journal read-only.
func readOnlyFixture(t *testing.T) (*journal.Handle, readOnlyFiles) {
	t.Helper()
	f := journaltest.New(t)
	h := f.Share(0)
	fs := readOnlyFiles{journaltest.File(0), journaltest.File(1), journaltest.File(2), journaltest.File(3)}
	ctx := t.Context()
	for _, id := range []journal.FileID{fs.a, fs.r, fs.u} {
		if _, err := h.WriteAt(ctx, id, 0, []byte("written"), 1); err != nil {
			t.Fatalf("WriteAt(%v): %v", id, err)
		}
	}
	if _, err := h.Truncate(ctx, fs.r, 1); err != nil {
		t.Fatalf("Truncate(r): %v", err)
	}
	if _, err := h.Sync(ctx, fs.a, fs.r, fs.u); err != nil {
		t.Fatalf("Sync(a, r, u): %v", err)
	}
	if _, err := h.WriteAt(ctx, fs.b, 0, []byte("w"), 1); err != nil {
		t.Fatalf("WriteAt(b): %v", err)
	}
	if err := f.Seam.Schedule(kit.Failing(kit.OpSync, "*", 0, syscall.EROFS)); err != nil {
		t.Fatalf("Schedule: %v", err)
	}
	if _, err := h.Truncate(ctx, fs.u, 1); !errors.Is(err, journal.ErrJournalReadOnly) {
		t.Fatalf("the truncate whose wait met EROFS returned %v, want ErrJournalReadOnly", err)
	}
	waitUntil(t, "the journal to turn read-only", func() bool { return f.J.Stats().ReadOnly.ReadOnly })
	return h, fs
}

// TestReadOnlyRefusesTheAppendingCalls checks RFC 1 §5.4's gate on the calls
// whose records are not written yet: each that would append a record fails
// at once with ErrJournalReadOnly, while an offload, which goes on uploading
// on a read-only journal, is not refused by it.
func TestReadOnlyRefusesTheAppendingCalls(t *testing.T) {
	h, fs := readOnlyFixture(t)
	a := fs.a
	ctx := t.Context()
	ext := []journal.Extent{{Off: 0, Len: 1}}
	for _, c := range []struct {
		call string
		run  func() error
	}{
		{"Release", func() error { return h.Release(ctx, a, ext) }},
		{"MarkOffloaded", func() error { return h.MarkOffloaded(ctx, a, ext, journal.Version{}, journal.Version{}) }},
		{"Unmark", func() error { return h.Unmark(ctx, a, ext) }},
		{"Hold", func() error { return h.Hold(ctx, journal.ShareID{}, journal.SnapshotCut(0), nil) }},
		{"Unhold", func() error { return h.Unhold(ctx, journal.ShareID{}, journal.SnapshotCut(0)) }},
		{"Stamp", func() error { return h.Stamp(ctx, a, journal.Version{}, journal.SnapshotCut(0)) }},
	} {
		if err := c.run(); !errors.Is(err, journal.ErrJournalReadOnly) {
			t.Errorf("%s on a read-only journal returned %v, want ErrJournalReadOnly", c.call, err)
		}
	}
	offload := func(context.Context, journal.Offer, func([]journal.Extent) error) error { return nil }
	if err := h.Offload(ctx, a, journal.Version{}, 1, 0, offload); errors.Is(err, journal.ErrJournalReadOnly) {
		t.Errorf("Offload on a read-only journal returned %v, want no read-only refusal: offload goes on", err)
	}
}

// TestReadOnlySyncAnswersAsRFC1Says checks each Sync a read-only journal
// answers, by what it waits on (RFC 1 §5.3, §5.4):
//   - a file whose truncate the transition undid fails every Sync with the
//     bare ErrJournalReadOnly, never a SyncError;
//   - a file with a write in the failed window is told once, a SyncError
//     wrapping ErrJournalReadOnly, and its next Sync answers nil;
//   - a file whose records, a truncate among them, were all synced before the
//     transition answers nil with its mark;
//   - a Sync naming no file answers nil.
//
// The engine relies on the bare refusal meaning "the removal was undone"
// (engine.undid) and on nil for a removal synced before the transition, which
// it retries rather than fails: TestATruncateSyncedBeforeTheJournalTurnedReadOnlyIsRetried
// and the fake journal's Sync in enginetest hold the engine to these answers,
// and this test holds the real journal to them.
func TestReadOnlySyncAnswersAsRFC1Says(t *testing.T) {
	h, fs := readOnlyFixture(t)
	ctx := t.Context()
	for i := range 2 {
		marks, err := h.Sync(ctx, fs.u)
		var se *journal.SyncError
		if !errors.Is(err, journal.ErrJournalReadOnly) || errors.As(err, &se) {
			t.Errorf("Sync %d of the file whose truncate was undone returned %v and marks %v, want the bare ErrJournalReadOnly", i+1, err, marks)
		}
	}
	if marks, err := h.Sync(ctx, fs.b); !errors.Is(err, journal.ErrSyncFailed) || !errors.Is(err, journal.ErrJournalReadOnly) {
		t.Errorf("the first Sync of a file in the failed window returned %v and marks %v, want a SyncError wrapping ErrJournalReadOnly", err, marks)
	}
	if marks, err := h.Sync(ctx, fs.b); err != nil {
		t.Errorf("the second Sync of the file in the failed window returned %v and marks %v, want nil: the window is reported once", err, marks)
	}
	for _, c := range []struct {
		what string
		id   journal.FileID
	}{
		{"a written file synced before the transition", fs.a},
		{"a truncated file synced before the transition", fs.r},
	} {
		if marks, err := h.Sync(ctx, c.id); err != nil || len(marks) == 0 {
			t.Errorf("the Sync of %s returned %v and marks %v, want nil with its mark", c.what, err, marks)
		}
	}
	if marks, err := h.Sync(ctx); err != nil {
		t.Errorf("a Sync naming no file returned %v and marks %v, want nil", err, marks)
	}
	// A Sync naming the undone file beside a durable one waits on the undone
	// record, so it answers the bare refusal, not the durable file's mark.
	marks, err := h.Sync(ctx, fs.a, fs.u)
	var se *journal.SyncError
	if !errors.Is(err, journal.ErrJournalReadOnly) || errors.As(err, &se) || len(marks) != 0 {
		t.Errorf("a Sync naming a durable file and the undone one returned %v and marks %v, want the bare ErrJournalReadOnly and no marks", err, marks)
	}
}

// TestReadOnlyProbeMakesItsSpareAtTheMaximum checks that the read-only probe
// of a journal filled to its maximum still makes the reopen's spare once the
// device takes writes again (RFC 1 §5.4): a full journal lives off
// recycling, so a spare held to the maximum could never be made and the
// journal would stay read-only after the device recovered. The reopen then
// serves what the journal held.
func TestReadOnlyProbeMakesItsSpareAtTheMaximum(t *testing.T) {
	f := journaltest.NewWith(t, func(s *journal.Settings) { s.MaxBytes = 8 * journaltest.FixtureSegmentCap })
	h := f.Share(0)
	ctx := t.Context()
	chunk := make([]byte, journaltest.FixtureSegmentCap/2)
	var last journal.FileID
	for i := 0; ; i++ {
		id := journaltest.File(i)
		if _, err := h.WriteAt(ctx, id, 0, chunk, 1); err != nil {
			var ns *journal.NoSpaceError
			if !errors.As(err, &ns) || ns.Limit != journal.LimitJournal {
				t.Fatalf("write %d returned %v, want the journal filled to a NoSpaceError naming the journal maximum", i, err)
			}
			break
		}
		if _, err := h.Sync(ctx, id); err != nil {
			t.Fatalf("Sync of write %d: %v", i, err)
		}
		last = id
	}
	// At the maximum: a spare reserved against it, as the pool's maker
	// reserves one, cannot be made, so the probe's spare must go past it.
	if st := f.J.Stats(); st.UsedBytes+journaltest.FixtureSegmentCap <= st.MaxBytes {
		t.Fatalf("UsedBytes %d, MaxBytes %d: one more segment still fits, want the journal at its maximum", st.UsedBytes, st.MaxBytes)
	}

	if err := f.Seam.Schedule(kit.Failing(kit.OpSync, "*", 0, syscall.EROFS)); err != nil {
		t.Fatalf("Schedule: %v", err)
	}
	if _, err := h.Truncate(ctx, last, 1); !errors.Is(err, journal.ErrJournalReadOnly) {
		t.Fatalf("the truncate whose wait met EROFS returned %v, want ErrJournalReadOnly", err)
	}
	waitUntil(t, "the journal to turn read-only", func() bool { return f.J.Stats().ReadOnly.ReadOnly })

	// The probe arms its timer on its own goroutine; the interval itself is
	// pinned by the datapath and durability probe tests.
	f.Seam.SetFault(nil)
	waitUntil(t, "the probe to make its spare at the maximum", func() bool {
		f.Advance(30 * time.Second)
		return f.J.Stats().ReadOnly.SpareMade
	})

	f.Crash()
	p := make([]byte, 1)
	if _, missing, _, _, err := f.Share(0).ReadAt(ctx, last, 0, p); err != nil || len(missing) != 0 {
		t.Fatalf("a read after the reopen returned missing %v, %v; want the write held", missing, err)
	}
}
