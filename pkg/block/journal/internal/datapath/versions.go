package datapath

import "github.com/cubbit/dittofs/pkg/block/journal/internal/core"

// Draw issues the next content version and appends nothing: no record is
// written, no segment byte is spent and no index entry is published, so a
// drawn version is recorded nowhere on disk. It is how a change stored
// elsewhere takes its version, whose recovery finds it in the file's floor
// rather than in a record. The caller serialises a draw against the writes
// whose versions must order against it, across the draw and whatever it
// captures with it.
//
// The counters refuse a draw before any limit is set, and again at the
// version limit, with a NoSpaceError naming the version limit. Either
// refusal is a retry-later one: raising the limit is the caller's own work,
// and a journal that waited would deadlock on it.
//
// ponytail: the limit's trigger looks back one minute, so a burst at a
// stall's start faster than the larger of about 280 000 versions a second
// and 1.07 × the last minute's peak spends the headroom before the self-fence
// would refuse. The cost is a retry-later refusal, never a failed write.
// Upgrade to a larger block, or to a margin sized from the device's peak
// IOPS rather than the last minute's, when a deployment reaches the burst
// check's refusal rate.
func (c *Core) Draw() (core.Version, error) {
	return c.counters.Draw()
}

// SetVersionLimit sets the version limit: versions are issued, by Draw and
// by WriteAt alike, only below mark, and none at all until one is set. It
// sets rather than raises, so a mark below the current limit takes effect at
// once. A mark with a non-zero epoch half is refused with core.ErrUnknownFormat
// and the limit it found still binds.
func (c *Core) SetVersionLimit(mark core.Version) error {
	return c.counters.SetVersionLimit(mark)
}
