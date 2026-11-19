package core

import (
	"sync"
	"sync/atomic"
)

// TagTally is a per-share-tag figure a writer moves and a reader reads without
// taking any lock a writer holds.
//
// The tags live in an immutable map swapped in whole when a tag first
// appears; each tag's figure is an atomic. A writer that moves a known tag
// does one atomic load, one map read and one atomic add; the mutex is taken
// only to add a tag the map has not seen, and no reader takes it.
//
// ponytail: a tag whose figure falls to zero keeps its entry, so the map
// follows the tags ever seen, not the tags live; Snapshot skips zeros.
// Upgrade to dropping idle entries when a journal that cycles through
// unboundedly many tags shows the map in a heap profile.
type TagTally struct {
	// decision: mu is a leaf: it guards the swap of a copied map when a tag
	// first appears, is held for that one copy, takes nothing inside it and is
	// never taken by a reader.
	mu sync.Mutex
	m  atomic.Pointer[map[ShareTag]*atomic.Int64]
}

func (t *TagTally) cell(tag ShareTag, create bool) *atomic.Int64 {
	if m := t.m.Load(); m != nil {
		if c := (*m)[tag]; c != nil {
			return c
		}
	}
	if !create {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	old := t.m.Load()
	if old != nil {
		if c := (*old)[tag]; c != nil {
			return c
		}
	}
	next := make(map[ShareTag]*atomic.Int64, 1)
	if old != nil {
		for k, v := range *old {
			next[k] = v
		}
	}
	c := new(atomic.Int64)
	next[tag] = c
	t.m.Store(&next)
	return c
}

// Add moves tag's figure by n.
func (t *TagTally) Add(tag ShareTag, n int64) { t.cell(tag, true).Add(n) }

// Set replaces tag's figure with n.
func (t *TagTally) Set(tag ShareTag, n int64) { t.cell(tag, true).Store(n) }

// Get returns tag's figure, zero for a tag never seen.
func (t *TagTally) Get(tag ShareTag) int64 {
	if c := t.cell(tag, false); c != nil {
		return c.Load()
	}
	return 0
}

// Snapshot returns a copy of every non-zero figure. The tags are read one
// after another, so a move between two reads shows in one and not the other.
func (t *TagTally) Snapshot() map[ShareTag]int64 {
	out := make(map[ShareTag]int64)
	if m := t.m.Load(); m != nil {
		for k, c := range *m {
			if v := c.Load(); v != 0 {
				out[k] = v
			}
		}
	}
	return out
}
