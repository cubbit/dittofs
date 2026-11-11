package index

import (
	"testing"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
)

// TestGettersExtentFigures checks the maintained extent figures read what the
// writes and the configured bound set.
func TestGettersExtentFigures(t *testing.T) {
	ix := New(1 << 20)
	if got := ix.ExtentLimit(); got != 1<<20 {
		t.Fatalf("ExtentLimit = %d, want %d", got, 1<<20)
	}
	if got := ix.ExtentCount(); got != 0 {
		t.Fatalf("ExtentCount of an empty index = %d, want 0", got)
	}
	if got := ix.DropRanges(); got != 0 {
		t.Fatalf("DropRanges of an empty index = %d, want 0", got)
	}
	const n = 5
	for i := range n {
		p := write(int64(i)*8192, 4096, rec(1, uint32(i)*4096), uint64(i+1))
		if _, err := ix.WriteRetain(testTag, testFile, p, core.Seq(i+1)); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
	}
	if got := ix.ExtentCount(); got != n {
		t.Fatalf("ExtentCount after %d disjoint writes = %d, want %d", n, got, n)
	}
}
