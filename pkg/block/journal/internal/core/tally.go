package core

import (
	"sync"
	"sync/atomic"
)

// TagCells is a set of cells keyed by share tag that a writer finds and a
// reader walks without taking any lock a writer holds.
//
// The cells live in an immutable map swapped in whole when a tag first
// appears. A writer that asks for a known tag does one atomic load and one map
// read; the mutex is taken only to add a tag the map has not seen, and no
// reader takes it.
//
// ponytail: a cell is never dropped, so the map follows the tags ever seen,
// not the tags live. Upgrade to dropping idle cells when a journal that
// cycles through unboundedly many tags shows the map in a heap profile.
type TagCells[V any] struct {
	// decision: mu is a leaf: it guards the swap of a copied map when a tag
	// first appears, is held for that one copy, takes nothing inside it and is
	// never taken by a reader.
	mu sync.Mutex
	m  atomic.Pointer[map[ShareTag]*V]
}

// Find returns tag's cell, nil for a tag never added.
func (t *TagCells[V]) Find(tag ShareTag) *V {
	if m := t.m.Load(); m != nil {
		return (*m)[tag]
	}
	return nil
}

// Ensure returns tag's cell, adding it first when the tag is new.
func (t *TagCells[V]) Ensure(tag ShareTag) *V {
	if c := t.Find(tag); c != nil {
		return c
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	old := t.m.Load()
	next := make(map[ShareTag]*V, 1)
	if old != nil {
		if c := (*old)[tag]; c != nil {
			return c
		}
		for k, v := range *old {
			next[k] = v
		}
	}
	c := new(V)
	next[tag] = c
	t.m.Store(&next)
	return c
}

// Each calls f for every cell added so far, in no order. The tags are visited
// one after another, so a move between two visits shows in one and not the
// other.
func (t *TagCells[V]) Each(f func(tag ShareTag, c *V)) {
	if m := t.m.Load(); m != nil {
		for k, c := range *m {
			f(k, c)
		}
	}
}

// TagTally is a per-share-tag figure a writer moves and a reader reads without
// taking any lock a writer holds: an atomic per tag, in TagCells. A writer
// that moves a known tag does one atomic load, one map read and one atomic
// add.
//
// ponytail: a tag whose figure falls to zero keeps its entry (see TagCells);
// Snapshot skips zeros.
type TagTally struct {
	cells TagCells[atomic.Int64]
}

// Add moves tag's figure by n.
func (t *TagTally) Add(tag ShareTag, n int64) { t.cells.Ensure(tag).Add(n) }

// Set replaces tag's figure with n.
func (t *TagTally) Set(tag ShareTag, n int64) { t.cells.Ensure(tag).Store(n) }

// Get returns tag's figure, zero for a tag never seen.
func (t *TagTally) Get(tag ShareTag) int64 {
	if c := t.cells.Find(tag); c != nil {
		return c.Load()
	}
	return 0
}

// Snapshot returns a copy of every non-zero figure. The tags are read one
// after another, so a move between two reads shows in one and not the other.
func (t *TagTally) Snapshot() map[ShareTag]int64 {
	out := make(map[ShareTag]int64)
	t.cells.Each(func(tag ShareTag, c *atomic.Int64) {
		if v := c.Load(); v != 0 {
			out[tag] = v
		}
	})
	return out
}
