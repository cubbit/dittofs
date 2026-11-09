package durability

import (
	"cmp"
	"slices"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
)

// DamagedRow is one damaged segment's row in the report.
type DamagedRow struct {
	Segment core.SegmentID
	// Retired is set once the runtime retired the segment. The row stays.
	Retired bool
	// BlocksReclaim is true while the segment still backs held extents it
	// cannot produce, so a reclamation pass must not select it.
	BlocksReclaim bool
}

// damagedRow is the stored row; BlocksReclaim is derived on read.
type damagedRow struct{ retired bool }

// markDamaged records that seg holds a record that did not verify. It is
// idempotent and never un-counts: the row stays until the journal closes,
// whatever becomes of the extents the segment backed.
//
// ponytail: the map of rows is never freed before Stop, so it follows the
// number of segments that ever held an unreadable record. Upgrade to bounding
// it when a journal with a long damage history shows up in a heap profile.
func (d *Durability) markDamaged(seg core.SegmentID) {
	d.dmu.Lock()
	defer d.dmu.Unlock()
	if _, ok := d.damaged[seg]; ok {
		return
	}
	if d.damaged == nil {
		d.damaged = map[core.SegmentID]*damagedRow{}
	}
	d.damaged[seg] = &damagedRow{}
}

// Retired is the runtime's hook for a retired segment: a damaged segment's row
// is marked retired and kept, so the count survives the retirement, and the
// copies the table holds in the segment are dropped, so the table is bounded
// by the failed segments still on disk. A segment with no row keeps no row.
func (d *Durability) Retired(seg core.SegmentID) {
	d.dmu.Lock()
	if r, ok := d.damaged[seg]; ok {
		r.retired = true
	}
	d.dmu.Unlock()
	d.dropCopies(seg)
}

// ReclaimBlocked reports whether seg is damaged and still backs held extents
// it cannot produce. It turns false once none is held, which is the signal
// that the segment is ready to be repacked; the row and the count stay.
//
// decision: nothing but tests calls this; its signature is held up only by the
// later reclamation work that selects segments.
func (d *Durability) ReclaimBlocked(seg core.SegmentID) bool {
	d.dmu.Lock()
	_, ok := d.damaged[seg]
	d.dmu.Unlock()
	return ok && d.index.SegmentBytes(seg).Held > 0
}

// damagedReport returns the count and the rows in segment order. It reads the
// row map under dmu and each segment's held bytes from the index's counters;
// it takes no stream lock and walks nothing.
func (d *Durability) damagedReport() (uint64, []DamagedRow) {
	d.dmu.Lock()
	rows := make([]DamagedRow, 0, len(d.damaged))
	for seg, r := range d.damaged {
		rows = append(rows, DamagedRow{Segment: seg, Retired: r.retired})
	}
	d.dmu.Unlock()
	slices.SortFunc(rows, func(a, b DamagedRow) int { return cmp.Compare(a.Segment, b.Segment) })
	for i := range rows {
		rows[i].BlocksReclaim = d.index.SegmentBytes(rows[i].Segment).Held > 0
	}
	return uint64(len(rows)), rows
}
