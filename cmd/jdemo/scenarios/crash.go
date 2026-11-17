package scenarios

import (
	"github.com/cubbit/dittofs/pkg/block/journal"

	"github.com/cubbit/dittofs/cmd/jdemo/jdemo"
)

// CrashStory is the durability half of the story: retention, the sync
// mark, and the honest restart refusal on a closed journal.
func CrashStory() ([]jdemo.Beat, journal.Settings) {
	beats, settings := BasicStory()
	// The placeholder list rides Task 1's registry; Task 3 writes the
	// story. One beat so the registry test sees a story, not a hole.
	return beats[:1], settings
}
