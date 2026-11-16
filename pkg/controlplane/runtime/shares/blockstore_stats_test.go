package shares

import (
	"testing"

	"github.com/cubbit/dittofs/pkg/block/engine"
)

// The totals are what `dfsctl store block stats` shows first, so one share
// whose uploads fail must make the totals say so, whatever the other shares
// report and in whichever order the registry yields them.
func TestAddBlockStoreStats_UploadsFailingSurvivesTheSum(t *testing.T) {
	failing := engine.BlockStoreStats{UploadsFailing: true, ConsecutiveUploadFailures: 4}
	// A run of its own, below the failing threshold, so summing the runs
	// (5) would differ from keeping the longest (4).
	fine := engine.BlockStoreStats{ConsecutiveUploadFailures: 1}

	for _, order := range [][]engine.BlockStoreStats{{failing, fine}, {fine, failing}} {
		var totals engine.BlockStoreStats
		for _, st := range order {
			addBlockStoreStats(&totals, st)
		}

		if !totals.UploadsFailing || totals.ConsecutiveUploadFailures != 4 {
			t.Fatalf("totals: UploadsFailing = %v, ConsecutiveUploadFailures = %d, want true, 4",
				totals.UploadsFailing, totals.ConsecutiveUploadFailures)
		}
	}
}
