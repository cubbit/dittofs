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
	if c.closed() {
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

// closed reports whether the core's Close has begun.
func (c *Core) closed() bool {
	return c.closedFlag.Load()
}
