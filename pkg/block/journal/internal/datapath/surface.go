package datapath

import "github.com/cubbit/dittofs/pkg/block/journal/internal/core"

// Settle drops the removal and loss markers of file id at or below v, and
// never a held extent, then re-tests the records registered live of the
// file with the liveness tracker: a removal, release or loss record whose
// marker the settle covered may now be droppable, and its release is what
// lets its segment retire. It appends nothing, persists nothing, takes the
// file's index lock for the settle and the tracker's registry lock for the
// re-test. It does nothing on a closed handle, and leaves another share's
// markers untouched — the re-test names only records already droppable by
// the index's own answer, the same answer any later event of the file
// would give.
func (c *Core) Settle(s *Share, id core.FileID, v core.Version) {
	exit, err := s.enter()
	if err != nil {
		return
	}
	defer exit()
	c.index.SettleFor(s.Tag, id, v)
	// The settle's relay to the liveness tracker, fired only after the
	// index settle ran: the re-test event a registered removal or loss
	// record of the file waits to be released by.
	c.liveness.Retest(id)
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
