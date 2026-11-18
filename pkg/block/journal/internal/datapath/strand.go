package datapath

import (
	"slices"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
)

// Strand adds tag to the core's set of stranded tags, an in-memory set kept
// for the share the caller says no share claims. It appends nothing, draws
// nothing and touches no storage, so nothing of it survives a close and a
// later core rebuilt over the directory: a caller that strands a tag names it
// again at every start, before serving.
//
// Tag 0 is refused with core.ErrInvalidSetting; once the core's Close began
// the call answers core.ErrClosed. Any other tag returns nil, twice for the
// same tag included: the set keeps each tag once.
func (c *Core) Strand(tag core.ShareTag) error {
	if tag == 0 {
		return core.ErrInvalidSetting
	}
	c.strandMu.Lock()
	defer c.strandMu.Unlock()
	if c.Closed() {
		return core.ErrClosed
	}
	var set []core.ShareTag
	if p := c.stranded.Load(); p != nil {
		set = *p
	}
	if slices.Contains(set, tag) {
		return nil
	}
	next := append(slices.Clone(set), tag)
	c.stranded.Store(&next)
	return nil
}

// unstrand removes tag from the stranded set: the Forget path calls it once
// the forget record is durable, since only the caller that names a tag to
// Forget can say when it stops being stranded. The Forget path holds no
// other lock of the core when it reaches this — the mark was cleared under
// its own mutex, the registry's mutex is down — and strandMu is a leaf that
// takes nothing, so the removal nests inside whatever the caller holds; its
// place in the Forget path's lock order is innermost. A tag nothing stranded
// runs through unharmed.
func (c *Core) unstrand(tag core.ShareTag) {
	c.strandMu.Lock()
	defer c.strandMu.Unlock()
	var set []core.ShareTag
	if p := c.stranded.Load(); p != nil {
		set = *p
	}
	if !slices.Contains(set, tag) {
		return
	}
	next := make([]core.ShareTag, 0, len(set)-1)
	for _, t := range set {
		if t != tag {
			next = append(next, t)
		}
	}
	c.stranded.Store(&next)
}

// Stranded reports whether tag is in the stranded set. It reads the pointer
// alone, so it never waits on a Strand in flight. A tag nothing stranded
// reads false, tag 0 with the rest.
func (c *Core) Stranded(tag core.ShareTag) bool {
	p := c.stranded.Load()
	if p == nil {
		return false
	}
	return slices.Contains(*p, tag)
}

// closed reports whether the core's Close has begun. It reads the flag
// alone, so it never waits on the close itself.
func (c *Core) closed() bool { return c.closedFlag.Load() }

// Closed reports whether the core's Close has begun, the export over the
// flag the core's own paths read through closed above. It never waits on
// the close itself: a reader over the core — the journal's inspection —
// refuses from the moment Close begins, as Stats does from the journal's
// own flag.
func (c *Core) Closed() bool { return c.closed() }
