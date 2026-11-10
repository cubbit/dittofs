package journaltest

import (
	"github.com/cubbit/dittofs/pkg/block/journal"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/platform/platformtest"
)

// SuitableMount returns the option that feeds the journal's platform check a
// suitable mount: an ext4 mount with cache flushes on, at the root of the
// filesystem, so it covers whatever directory the check is run over. A
// fixture's journals take it by default, so a host whose own mount table
// refuses a test's temporary directory — or whose platform reads mounts the
// check cannot judge — cannot shape the answer. A test that checks the
// refusal passes its own probe as a later option: options apply in call
// order, so the later one wins.
func SuitableMount() journal.Option {
	return journal.WithMountProbe(platformtest.SuitableGather("/"))
}
