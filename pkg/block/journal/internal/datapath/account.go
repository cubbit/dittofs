package datapath

import "github.com/cubbit/dittofs/pkg/block/journal/internal/index"

// Account applies one index update's change to the core's held-state counters.
// A zero delta changes nothing. A delta with no byte change still updates the
// dirty-file list when it moves its file across the dirty line or changes its
// oldest dirty time.
//
// The counters are atomics, which commute, so the order two callers apply
// their deltas in does not matter to them. The dirty-file list does not
// commute: apply takes its own mutex and sets state, so two deltas of one file
// applied in opposite order can leave a stale entry. Every caller but one
// holds the changed file's append domain, which orders them; the exception is
// the loss path's markLoss, when the loss is not already Marked (a synced
// drop whose synchronous marker failed).
//
// decision: the dirty list is ordered by the append domain alone, and the
// unmarked loss path is not. apply re-adds a file only on BecameDirty, so a
// BecameClean applied after a concurrent BecameDirty of the same file leaves
// it dirty but unlisted. Overturned if that path is seen to race a write to
// the same file, which would need the list ordered by a per-file sequence.
//
// decision: the counters follow the index's delta rather than each caller's
// own arithmetic, because a caller that adjusts a counter itself is a caller
// that can forget one. The index measures the delta inside the update, from
// the leaf functions bytes enter and leave through, so every path that moves
// bytes is counted once. Overturned if an update can move held bytes without
// returning a delta.
func (c *Core) Account(d index.HeldDelta) {
	if d == (index.HeldDelta{}) {
		return
	}
	c.dirtyFiles.apply(d)
	c.held.Add(d.HeldBytes)
	c.dirty.Add(d.DirtyBytes)
	c.retained.Add(d.RetainedBytes)
	if c.accountSpy != nil {
		c.accountSpy(d)
	}
}

// heldBytes returns the counters: held, dirty and retained bytes.
func (c *Core) heldBytes() (held, dirty, retained int64) {
	return c.held.Load(), c.dirty.Load(), c.retained.Load()
}
