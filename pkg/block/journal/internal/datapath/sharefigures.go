package datapath

import (
	"sync"
	"sync/atomic"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/index"
)

// shareHeld is one share's held-state figures: held, dirty and retained bytes.
type shareHeld struct{ Held, Dirty, Retained int64 }

// shareCells is one share's figures as atomics.
type shareCells struct{ held, dirty, retained atomic.Int64 }

// shareLedger keeps shareHeld per share tag, from the same deltas the core's
// counters follow.
//
// decision: the figures are atomics in an immutable map swapped in whole when
// a tag first appears, so a reader takes no lock a write holds. The mutex
// guards that swap alone: it is a leaf, held for one map copy, and Stats never
// takes it. apply keeps the order the core's counters keep, held raised before
// dirty and dirty lowered before held, so dirty never exceeds held at any
// instant; a reader of the pair re-reads when a fall landed between its two
// loads, as Stats does for the core's pair. Keyed by the delta's tag, not its
// file, because the unmarked loss path is not ordered by the file's append
// domain. Overturned if the three figures must read as one instant, which
// would need a copy-on-write snapshot per apply.
type shareLedger struct {
	mu sync.Mutex
	m  atomic.Pointer[map[core.ShareTag]*shareCells]

	// stepSpy is a test seam that stands between apply's updates of one
	// share's figures, told how many have been applied, so a test can read
	// the figures with one applied and the next not.
	//
	// decision: it is nil outside tests, for the same reason the core's
	// stepSpy is: one nil comparison per step on the publish path.
	stepSpy func(step int)
}

func (l *shareLedger) cells(tag core.ShareTag, create bool) *shareCells {
	if m := l.m.Load(); m != nil {
		if c := (*m)[tag]; c != nil {
			return c
		}
	}
	if !create {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	old := l.m.Load()
	next := make(map[core.ShareTag]*shareCells, 1)
	if old != nil {
		if c := (*old)[tag]; c != nil {
			return c
		}
		for k, v := range *old {
			next[k] = v
		}
	}
	c := new(shareCells)
	next[tag] = c
	l.m.Store(&next)
	return c
}

// apply adds d's byte changes to its share. Tag 0 marks the journal's own
// records and is not a share, so it is not kept.
func (l *shareLedger) apply(d index.HeldDelta) {
	if d.Tag == 0 {
		return
	}
	f := l.cells(d.Tag, true)
	f.held.Add(max(d.HeldBytes, 0))
	if l.stepSpy != nil {
		l.stepSpy(1)
	}
	f.dirty.Add(d.DirtyBytes)
	if l.stepSpy != nil {
		l.stepSpy(2)
	}
	f.held.Add(min(d.HeldBytes, 0))
	f.retained.Add(d.RetainedBytes)
}

// shareReadTries bounds the re-reads of a share's dirty and held pair.
const shareReadTries = 8

// shareFigures returns a copy of every share's figures, keyed by tag. A
// share's pair is re-read, up to shareReadTries times, until dirty does not
// exceed held; a pair still out of order after them is returned as read, the
// bounded answer Stats documents for the core's pair.
func (c *Core) shareFigures() map[core.ShareTag]shareHeld {
	m := c.shares.m.Load()
	if m == nil {
		return map[core.ShareTag]shareHeld{}
	}
	out := make(map[core.ShareTag]shareHeld, len(*m))
	for t, f := range *m {
		var h shareHeld
		for range shareReadTries {
			h.Dirty = f.dirty.Load()
			if h.Held = f.held.Load(); h.Dirty <= h.Held {
				break
			}
		}
		h.Retained = f.retained.Load()
		out[t] = h
	}
	return out
}
