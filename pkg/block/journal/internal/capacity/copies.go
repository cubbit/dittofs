package capacity

import "github.com/cubbit/dittofs/pkg/block/journal/internal/core"

// CopyLanded holds n record bytes of a repack copy of tag's records, made out
// of source segment src, out of the share's tally: the set already holds them
// in the tag's settled figure, but the tally, which the share's limit admits
// against, takes them only once src is retired, so a copy is never refused by
// that limit. This is a deferral ledger for the share tally alone; the repack
// reserve's bound is the pool's ready spares and does not read it.
//
// ponytail: the count is made when the unit's copies have all landed, not at
// each append, so a tag seeded in that window reads the copies in its settled
// figure and counts them again when they transfer: the tally stands high by
// that unit's copy bytes, failing closed, until the tally is next reseeded.
// Upgrade to counting at the append when a share's first write racing a pass
// over its records shows in the field.
func (c *Capacity) CopyLanded(src core.SegmentID, tag core.ShareTag, n int64) {
	if n <= 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.copies == nil {
		c.copies = make(map[core.SegmentID]map[core.ShareTag]int64)
		c.copyHeld = make(map[core.ShareTag]int64)
	}
	byTag := c.copies[src]
	if byTag == nil {
		byTag = make(map[core.ShareTag]int64)
		c.copies[src] = byTag
	}
	byTag[tag] += n
	c.copyHeld[tag] += n
}

// CopiesTransfer hands every copy counted against src to its record's share:
// the share's tally takes the bytes, for the source's own retirement lowers it
// by the source's bytes by the same step and the share holds the copies in
// their place. It runs before that lowering, so the tally only ever stands
// above the truth, never below it. A tag whose figure is not seeded takes
// nothing: its seed reads the set's settled figure, which already holds the
// copies. Safe to call for a segment nothing was counted against.
func (c *Capacity) CopiesTransfer(src core.SegmentID) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for tag, n := range c.copies[src] {
		if c.seeded[tag] {
			c.shareTally[tag] += n
		}
		c.copyHeld[tag] -= n
		if c.copyHeld[tag] <= 0 {
			delete(c.copyHeld, tag)
		}
	}
	delete(c.copies, src)
}

// CopyHeldBytes returns the record bytes of repack copies held out of the
// share tallies, whose sources are not yet retired.
func (c *Capacity) CopyHeldBytes() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	var n int64
	for _, v := range c.copyHeld {
		n += v
	}
	return n
}
