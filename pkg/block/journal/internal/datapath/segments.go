package datapath

import (
	"cmp"
	"slices"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/reclaim"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/segment"
)

// SegmentRow is one segment's accounting: what the set holds of it, what the
// index's per-segment counters say it backs, whether it is damaged and what
// the unread set says of it.
type SegmentRow struct {
	ID        core.SegmentID
	Stream    uint32
	Sealed    bool
	Failed    bool
	Allocated int64
	Held      int64
	Retained  int64
	Damaged   bool
	// Retired is a damaged segment that was retired: its row stays until the
	// journal closes, with nothing else to report.
	Retired bool
	Suspect bool
	Unread  []core.SegmentRange

	// Unreclaimed is the part of the segment backing nothing, as selection
	// reads it. It stays here, off the journal's exported rows.
	Unreclaimed int64
}

// The row builder lives here, not in package segment: it reads the index, the
// unread set and the durability layer, all of which import segment.

// Segments returns one row per segment the journal owns, in segment order,
// followed in the same order by the rows of damaged segments already retired.
// Each row reads the set, the index's counters, the durability layer's
// damaged rows and the unread set in turn, so a row is as consistent as those
// reads are one after another: it takes no lock across them and does no I/O.
//
// decision: the rows are not one snapshot. A write landing between the set's
// read and the index's moves a segment's held bytes after its allocation was
// read, so a caller summing the rows while the journal takes writes may see
// a total a moment off Stats. Overturned by a snapshot of the set and the
// counters under one lock, which a hot write path would then wait on.
func (c *Core) Segments() []SegmentRow {
	damaged := map[core.SegmentID]bool{}
	var retired []SegmentRow
	for _, d := range c.dur.Stats().Segments {
		damaged[d.Segment] = true
		if d.Retired {
			retired = append(retired, SegmentRow{ID: d.Segment, Damaged: true, Retired: true})
		}
	}
	rows := c.set.Snapshot()
	out := make([]SegmentRow, 0, len(rows)+len(retired))
	for _, r := range rows {
		b := c.index.SegmentBytes(r.ID)
		row := SegmentRow{
			ID:          r.ID,
			Stream:      r.Stream,
			Sealed:      r.State == segment.Sealed,
			Failed:      r.State == segment.Ended,
			Allocated:   r.Size,
			Held:        b.Held,
			Retained:    b.Retained,
			Damaged:     damaged[r.ID],
			Unreclaimed: reclaim.Unreclaimed(r.Size, b.Held+b.Retained),
		}
		if c.unread.Suspect(r.ID) {
			row.Suspect, row.Unread = true, c.unread.Ranges(r.ID)
		}
		out = append(out, row)
	}
	slices.SortFunc(retired, func(a, b SegmentRow) int { return cmp.Compare(a.ID, b.ID) })
	return append(out, retired...)
}
