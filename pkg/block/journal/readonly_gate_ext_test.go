package journal_test

import (
	"context"
	"errors"
	"syscall"
	"testing"

	"github.com/cubbit/dittofs/pkg/block/journal"
	"github.com/cubbit/dittofs/pkg/block/journal/journaltest"
	"github.com/cubbit/dittofs/pkg/block/journal/journaltest/kit"
)

// readOnlyFixture opens a journal, writes and syncs file a, then writes files
// b and c and fails b's sync with EROFS, so the journal turns read-only with
// one synced file and two whose writes it failed. It returns the handle and
// the three files, after the Sync that met the EROFS has reported it to b; c
// has not been told yet.
func readOnlyFixture(t *testing.T) (h *journal.Handle, a, b, c journal.FileID) {
	t.Helper()
	f := journaltest.New(t)
	h, a, b, c = f.Share(0), journaltest.File(0), journaltest.File(1), journaltest.File(2)
	ctx := t.Context()
	if _, err := h.WriteAt(ctx, a, 0, []byte("a"), 1); err != nil {
		t.Fatalf("WriteAt(a): %v", err)
	}
	if _, err := h.Sync(ctx, a); err != nil {
		t.Fatalf("Sync(a): %v", err)
	}
	for _, id := range []journal.FileID{b, c} {
		if _, err := h.WriteAt(ctx, id, 0, []byte("w"), 1); err != nil {
			t.Fatalf("WriteAt(%v): %v", id, err)
		}
	}
	if err := f.Seam.Schedule(kit.Failing(kit.OpSync, "*", 0, syscall.EROFS)); err != nil {
		t.Fatalf("Schedule: %v", err)
	}
	if _, err := h.Sync(ctx, b); !errors.Is(err, journal.ErrJournalReadOnly) {
		t.Fatalf("the Sync that met EROFS returned %v, want ErrJournalReadOnly", err)
	}
	waitUntil(t, "the journal to turn read-only", func() bool { return f.J.Stats().ReadOnly.ReadOnly })
	return h, a, b, c
}

// TestReadOnlyRefusesTheAppendingCalls checks RFC 1 §5.4's gate on the calls
// whose records are not written yet: each that would append a record fails
// at once with ErrJournalReadOnly, while an offload, which goes on uploading
// on a read-only journal, is not refused by it.
func TestReadOnlyRefusesTheAppendingCalls(t *testing.T) {
	h, a, _, _ := readOnlyFixture(t)
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

// TestReadOnlyRefusesEverySync checks that a read-only journal acknowledges
// no Sync: a file whose failed write was not reported yet is still told of
// the failed window, as a SyncError; and the failed file's next Sync, a Sync
// of a file whose records were all synced before the journal turned
// read-only, and a Sync naming no file all fail at once with
// ErrJournalReadOnly.
func TestReadOnlyRefusesEverySync(t *testing.T) {
	h, a, b, c := readOnlyFixture(t)
	ctx := t.Context()
	if marks, err := h.Sync(ctx, c); !errors.Is(err, journal.ErrSyncFailed) || !errors.Is(err, journal.ErrJournalReadOnly) {
		t.Errorf("the first Sync of a file the read-only journal failed returned %v and marks %v, want a SyncError wrapping ErrJournalReadOnly", err, marks)
	}
	if marks, err := h.Sync(ctx, b); !errors.Is(err, journal.ErrJournalReadOnly) {
		t.Errorf("the failed file's second Sync returned %v and marks %v, want ErrJournalReadOnly", err, marks)
	}
	if marks, err := h.Sync(ctx, a); !errors.Is(err, journal.ErrJournalReadOnly) {
		t.Errorf("a Sync of a file synced before the journal turned read-only returned %v and marks %v, want ErrJournalReadOnly", err, marks)
	}
	if marks, err := h.Sync(ctx); !errors.Is(err, journal.ErrJournalReadOnly) {
		t.Errorf("a Sync naming no file returned %v and marks %v, want ErrJournalReadOnly", err, marks)
	}
}
