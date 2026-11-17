package datapath

import (
	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/segment"
)

// SegmentInfo is one segment's accounting as the inspection serves it: the
// set's own row — id, state, accounted size — beside the held and retained
// bytes the index counts for it and the damaged flag the durability layer
// holds. The public type the journal serves is its own; this is the core's
// answer the journal maps.
type SegmentInfo struct {
	ID      core.SegmentID
	State   segment.State
	Size    int64
	Held    int64
	Retain  int64
	Damaged bool
}

// SegmentInfos answers one row per segment the set holds, ascending by id,
// spares excluded: the held and retained bytes each backs filled from the
// index, the damaged count from the durability layer. It is a snapshot of
// three sources, each consistent on its own, with no fall between them
// held: a write in flight appears in the bytes before the set's row names
// it, the same window Stats documents, and a caller that acts on a row
// re-checks it as every claim does.
func (c *Core) SegmentInfos() []SegmentInfo {
	rows := c.set.Snapshot()
	_, damaged := c.dur.DamagedReport()
	dmg := make(map[core.SegmentID]bool, len(damaged))
	for _, row := range damaged {
		dmg[row.Segment] = true
	}
	out := make([]SegmentInfo, 0, len(rows))
	for _, r := range rows {
		b := c.index.SegmentBytes(r.ID)
		out = append(out, SegmentInfo{ID: r.ID, State: r.State, Size: r.Size,
			Held: b.Held, Retain: b.Retained, Damaged: dmg[r.ID]})
	}
	return out
}
