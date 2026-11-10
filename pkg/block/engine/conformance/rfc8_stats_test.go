//go:build engine_conformance

package conformance

import (
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/block/engine/enginetest"
)

// syncFailed reports whether Stats raises journal_sync_failed for the device.
func syncFailed(f *enginetest.Fixture) bool {
	for _, c := range f.E.Stats().Journals[f.Dir].Conditions {
		if c.Name == "journal_sync_failed" {
			return true
		}
	}
	return false
}

// §15.2, journal_sync_failed: a failed window with loss records, the journal
// still writable, raises it, and it clears an hour after the latest failed
// window on the test clock with no operator step. A restart over the loss
// record on disk raises none for the loss found at open.
func TestRFC8B_JournalSyncFailed(t *testing.T) {
	f := enginetest.New(t)
	_, id := f.File("a")
	write(t, f, id, 0, []byte("in a failed window"))
	f.FailNextSync()
	if _, _, err := f.E.Commit(t.Context(), f.Share.ID, id, 0, 0); err != nil {
		t.Fatalf("Commit over a failed window: %v", err)
	}
	if !syncFailed(f) {
		t.Fatal("no journal_sync_failed after a failed window with loss records")
	}
	write(t, f, id, 0, []byte("still writable"))
	f.Clock.Advance(59 * time.Minute)
	if !syncFailed(f) {
		t.Error("journal_sync_failed cleared within the hour")
	}
	f.Clock.Advance(time.Minute)
	if syncFailed(f) {
		t.Error("journal_sync_failed still raised an hour after the failed window")
	}

	f.Restart()
	write(t, f, id, 0, []byte("after the restart"))
	if _, _, err := f.E.Commit(t.Context(), f.Share.ID, id, 0, 0); err != nil {
		t.Fatalf("Commit after the restart: %v", err)
	}
	if syncFailed(f) {
		t.Error("journal_sync_failed raised for the loss found at open")
	}
}
