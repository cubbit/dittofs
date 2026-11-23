//go:build engine_conformance

package conformance

import (
	"slices"
	"testing"

	"github.com/cubbit/dittofs/pkg/block/engine/enginetest"
)

// RFC 8 §5.2: the group commit records the journal's sync marks. Over the real
// journal, a commit that waits on a recorded mark leaves the store holding the
// marks the journal serves, and the next start hands them back to Open.
func TestRFC8_TheGroupCommitRecordsTheJournalsMarks(t *testing.T) {
	f := enginetest.New(t)
	_, id := f.File("a")
	// The second commit overwrites committed content, so it returns only once
	// a recorded mark covers the record it proved.
	for _, p := range []string{"a record the commit proves", "a record over it, the same size"} {
		write(t, f, id, 0, []byte(p))
		if _, _, err := f.E.Commit(t.Context(), f.Share.ID, id, 0, 0); err != nil {
			t.Fatalf("Commit: %v", err)
		}
	}
	served := f.JournalSyncMarks()
	if len(served) == 0 {
		t.Fatal("the journal serves no sync mark after a committed write")
	}

	f.Restart()
	if handed := f.OpenedWithMarks(); !slices.Equal(handed, served) {
		t.Fatalf("the restart handed Open the marks %+v, want the journal's %+v the group commit recorded", handed, served)
	}
}
