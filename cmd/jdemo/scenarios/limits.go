package scenarios

import (
	"github.com/cubbit/dittofs/pkg/block/journal"

	"github.com/cubbit/dittofs/cmd/jdemo/jdemo"
)

// limitsMaxBytes is the limits story's world: two caps rollable above the
// pool's own quarter. The pool reserves SpareTarget(2)+headroom+reserve ≈ 4
// caps against the maximum at open, so 2 GiB leaves ~1 GiB of content —
// the share's 512 MiB budget sits safely inside it, and the story's
// writes meet the SHARE's limit, not the journal's.
const (
	limitsMaxBytes  = 2 << 30
	limitsShareEach = 512 << 20
)

// LimitsStory is the space half: the share budget, the reserve before the
// burst, and what truncate does and does not give back.
func LimitsStory() ([]jdemo.Beat, journal.Settings) {
	beats, _ := BasicStory()
	// The placeholder list rides Task 1's registry; Task 4 writes the
	// story and its budget pair.
	return beats[:1], journal.Settings{
		Streams:         2,
		SpareTarget:     2,
		HeadroomRecords: 1024,
		MaxBytes:        limitsMaxBytes,
	}
}
