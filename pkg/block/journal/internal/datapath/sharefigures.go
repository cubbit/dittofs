package datapath

import (
	"sync"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/index"
)

// shareHeld is one share's held-state figures: held, dirty and retained bytes.
type shareHeld struct{ Held, Dirty, Retained int64 }

// shareLedger keeps shareHeld per share tag, from the same deltas the core's
// counters follow.
//
// decision: mu is a leaf. It guards the map and the figures, held only for a
// map lookup and three additions — never across I/O or any other lock. A
// reader therefore takes a share's three figures as one step, and takes no
// lock a write holds across its append. Keyed by the delta's tag, not its
// file, because the unmarked loss path is not ordered by the file's append
// domain. Overturned if Stats is read often enough for this mutex to show in
// a profile; the upgrade is a copy-on-write map of atomics.
type shareLedger struct {
	mu sync.Mutex
	m  map[core.ShareTag]*shareHeld
}

// apply adds d's byte changes to its share. The three figures change under one
// hold of the mutex and a reader takes it too, so no order among them shows.
// Tag 0 marks the journal's own records and is not a share, so it is not kept.
func (l *shareLedger) apply(d index.HeldDelta) {
	if d.Tag == 0 {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.m == nil {
		l.m = make(map[core.ShareTag]*shareHeld)
	}
	f := l.m[d.Tag]
	if f == nil {
		f = new(shareHeld)
		l.m[d.Tag] = f
	}
	f.Held += d.HeldBytes
	f.Dirty += d.DirtyBytes
	f.Retained += d.RetainedBytes
}

// shareFigures returns a copy of every share's figures, keyed by tag.
func (c *Core) shareFigures() map[core.ShareTag]shareHeld {
	c.shares.mu.Lock()
	defer c.shares.mu.Unlock()
	out := make(map[core.ShareTag]shareHeld, len(c.shares.m))
	for t, f := range c.shares.m {
		out[t] = *f
	}
	return out
}
