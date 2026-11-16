package journaltest

import (
	"github.com/cubbit/dittofs/pkg/block/journal"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
)

// SetSegmentCap makes s name a cap of n bytes for every segment the journal
// takes from it instead of the format's own size, so a harness over many
// journal opens keeps the preallocated tail it reopens over as small as its
// rows need. The cap stays what the journal itself made it — a knob for the
// harnesses alone, no value an operator could set, and none is refused.
func SetSegmentCap(s *journal.Settings, n int64) {
	core.SetSegmentCap(s, n)
}
