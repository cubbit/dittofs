package datapath

import "github.com/cubbit/dittofs/pkg/block/journal/internal/index"

// Account applies one index update's change to the core's held-state counters.
// A zero delta changes nothing.
//
// It is called with the changed file's append domain held, by writers and by
// the durability worker alike, so it takes no lock and waits on nothing: it
// only adds to atomics.
//
// decision: the counters follow the index's delta rather than each caller's
// own arithmetic, because a caller that adjusts a counter itself is a caller
// that can forget one. The index measures the delta inside the update, from
// the leaf functions bytes enter and leave through, so every path that moves
// bytes is counted once. Overturned if an update can move held bytes without
// returning a delta.
func (c *Core) Account(d index.HeldDelta) {
	if d.HeldBytes == 0 && d.DirtyBytes == 0 && d.RetainedBytes == 0 {
		return
	}
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
