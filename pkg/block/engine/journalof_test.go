package engine

import (
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/block/journal"
	"github.com/cubbit/dittofs/pkg/block/journal/journaltest"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/sim"
)

// A refused Share or Reserve answers a nil interface, never one holding a nil
// pointer, which a caller's nil check would take for a handle.
func TestRefusalAnswersNil(t *testing.T) {
	h, err := JournalOf(&journal.Journal{}).Share(1, 0, journal.Version{})
	if !errors.Is(err, journal.ErrNotImplemented) || h != nil {
		t.Errorf("Share = %v, %v; want nil, the journal's error", h, err)
	}
	r, err := journalHandle{&journal.Handle{}}.Reserve(1, 1)
	if !errors.Is(err, journal.ErrNotImplemented) || r != nil {
		t.Errorf("Reserve = %v, %v; want nil, the journal's error", r, err)
	}
}

// TestJournalOfServesTheJournalsMarks covers the marks over a real journal:
// the mark a Sync through the engine's handle returns is the stream's entry
// in SyncMarks, and MarksRecorded of it leaves that entry where it was.
func TestJournalOfServesTheJournalsMarks(t *testing.T) {
	ctx := t.Context()
	dir := t.TempDir()
	clock := sim.NewTestClock(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	opts := []journal.Option{journaltest.SuitableMount(), journal.WithClock(clock)}
	id, err := journal.Init(ctx, dir, journal.InstallationID{1}, opts...)
	if err != nil {
		t.Fatalf("journal init: %v", err)
	}
	raw, err := journal.Open(ctx, dir, journal.Version{}, id, nil, opts...)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = raw.Close() })
	if err := raw.SetVersionLimit(journal.Version{Counter: 1 << 20}); err != nil {
		t.Fatalf("SetVersionLimit: %v", err)
	}
	j := JournalOf(raw)
	if got := j.SyncMarks(); len(got) != 0 {
		t.Fatalf("SyncMarks = %+v before any sync, want none", got)
	}
	h, err := j.Share(1, 0, journal.Version{})
	if err != nil {
		t.Fatalf("Share: %v", err)
	}
	f := journal.NewFileID([16]byte{'m'})
	if err := journaltest.RetrySpare(func() error {
		_, err := h.WriteAt(ctx, f, 0, []byte("a record the sync proves"), 0)
		return err
	}); err != nil {
		t.Fatalf("WriteAt: %v", err)
	}
	synced, err := h.Sync(ctx, f)
	if err != nil || len(synced) != 1 {
		t.Fatalf("Sync = %+v, %v, want the record's one mark", synced, err)
	}
	if got := j.SyncMarks(); !slices.Equal(got, synced) {
		t.Fatalf("SyncMarks = %+v, want the mark Sync returned: %+v", got, synced)
	}
	j.MarksRecorded(synced)
	if got := j.SyncMarks(); !slices.Equal(got, synced) {
		t.Fatalf("SyncMarks = %+v after MarksRecorded of it, want it unchanged: %+v", got, synced)
	}
	// A recorded mark past the entry reaches the journal: its entry is never
	// older than what the caller says is recorded.
	past := synced[0]
	past.Through, past.Offset, past.Sequence = past.Through+4096, past.Offset+4000, past.Sequence+1
	j.MarksRecorded([]entity.SyncMark{past})
	if got := j.SyncMarks(); !slices.Equal(got, []entity.SyncMark{past}) {
		t.Fatalf("SyncMarks = %+v after MarksRecorded of a mark past the entry, want %+v", got, past)
	}
}
