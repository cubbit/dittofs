package datapath

import "github.com/cubbit/dittofs/pkg/block/journal/internal/core"

// Settle drops the removal and loss markers of file id at or below v, and
// never a held extent. It appends nothing, persists nothing and takes the
// file's index lock alone. It does nothing on a closed handle, and leaves
// another share's file untouched.
func (c *Core) Settle(s *Share, id core.FileID, v core.Version) {
	exit, err := s.enter()
	if err != nil {
		return
	}
	defer exit()
	c.index.SettleFor(s.Tag, id, v)
}

// Extents returns the extents of file id the journal holds, in offset order.
// Neighbouring pieces of one record that differ only in their offloaded bit
// come back as one extent.
func (c *Core) Extents(s *Share, id core.FileID) ([]core.Extent, error) {
	exit, err := s.enter()
	if err != nil {
		return nil, err
	}
	defer exit()
	if err := c.checkTag(s, id); err != nil {
		return nil, err
	}
	placed, _ := c.index.Span(id, core.Extent{Off: 0, Len: 1<<63 - 1})
	var out []core.Extent
	for _, p := range placed {
		out = append(out, p.Extent)
	}
	return out, nil
}

// Files returns the share's files that hold an extent or an unsettled marker,
// in no order; nothing once the handle is closed.
func (c *Core) Files(s *Share) []core.FileID {
	exit, err := s.enter()
	if err != nil {
		return nil
	}
	defer exit()
	return c.index.Files(s.Tag)
}
