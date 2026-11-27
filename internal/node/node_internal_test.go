package node

import (
	"slices"
	"testing"

	"github.com/cubbit/dittofs/pkg/bootstrap"
)

// journalPaths hands the engine every journal directory, in order.
func TestJournalPathsKeepsEveryJournalInOrder(t *testing.T) {
	t.Parallel()
	journals := []bootstrap.Journal{{Path: "/data/j1"}, {Path: "/data/j2"}}

	got := journalPaths(journals)

	want := []string{"/data/j1", "/data/j2"}
	if !slices.Equal(got, want) {
		t.Errorf("journalPaths of two journals: got %q, want %q", got, want)
	}
}
