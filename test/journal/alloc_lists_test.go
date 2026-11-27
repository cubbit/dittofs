package journal

import (
	"strings"
	"testing"

	"github.com/cubbit/dittofs/test/harness/registry"
)

// The counted-allocation tests of RFC 1 §9.7 cannot pass with -race, so the
// raced journal-conformance cell excludes them with -skip while the norace
// cell of the unit suite runs them with -run. The comment beside the two
// cells binds the lists by hand; this check makes the bind loud, in both
// directions: a name reaching only -run loses its enforcement everywhere,
// and one reaching only -skip is never run at all.

// cellFlag returns the list a cell's one command pins after flag, its value
// the word FLAG=VALUE among the command's words.
func cellFlag(t *testing.T, cell *registry.Suite, flag string) string {
	t.Helper()
	if cell == nil {
		t.Fatalf("test/suites.yaml: no cell to pin %s to", flag)
	}
	if len(cell.Cmd) == 0 {
		t.Fatalf("cell %s: no command pins %s", cell.Name, flag)
	}
	for w := range strings.FieldsSeq(cell.Cmd[0].Line) {
		if v, ok := strings.CutPrefix(w, flag+"="); ok {
			return v
		}
	}
	t.Fatalf("cell %s: its command names no %s= list", cell.Name, flag)
	return "" // unreachable: the Fatalf above returns never
}

// TestAllocCellsShareOneList holds the raced cell's -skip list and the
// norace cell's -run list to the same tests.
func TestAllocCellsShareOneList(t *testing.T) {
	reg, err := registry.Load(suitesRoot)
	if err != nil {
		t.Fatalf("test/suites.yaml: %v", err)
	}

	raced := reg.Suite("journal-conformance")
	var norace *registry.Suite
	for _, u := range reg.Units(reg.Suite("unit")) {
		if u.Name == "unit/journal-conformance-norace" {
			norace = u
			break
		}
	}

	want := cellFlag(t, norace, "-run")
	got := cellFlag(t, raced, "-skip")
	if got != want {
		t.Errorf("the raced cell's -skip list (got %s) must equal the norace cell's -run list (want %s): one list reaching only -run is enforced nowhere, and one reaching only -skip is never run", got, want)
	}
}
