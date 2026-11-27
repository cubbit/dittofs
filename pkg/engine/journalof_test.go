package engine

import (
	"errors"
	"slices"
	"testing"

	"github.com/cubbit/dittofs/pkg/journal"
	"github.com/cubbit/dittofs/pkg/journal/journaltest"
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
	// The fixture opens a journal with small segments, a version limit and its
	// spares made, so the first write finds a spare and the retry below does not
	// spin.
	fix := journaltest.New(t)
	j := JournalOf(fix.J)
	if got := j.SyncMarks(); len(got) != 0 {
		t.Fatalf("SyncMarks = %+v before any sync, want none", got)
	}
	h, err := j.Share(1, 0, journal.Version{})
	if err != nil {
		t.Fatalf("Share: %v", err)
	}
	f := journal.NewFileID([16]byte{'m'})
	if err := fix.RetrySpare(func() error {
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
}
