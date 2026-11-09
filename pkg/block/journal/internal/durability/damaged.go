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
	// cannot produce, so a reclamation pass must not select it: while the
	// index may still name a corrupt record in it (blocksLocked).
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

// countUnproduced adds delta to the count of held records seg cannot produce,
// dropping the entry at zero so the map follows the live holds.
func (d *Durability) countUnproduced(seg core.SegmentID, delta int) {
	d.dmu.Lock()
	defer d.dmu.Unlock()
	n := d.unproduced[seg] + delta
	if n <= 0 {
		delete(d.unproduced, seg)
		return
	}
	if d.unproduced == nil {
		d.unproduced = map[core.SegmentID]int{}
	}
	d.unproduced[seg] = n
}

// blocksLocked reports whether seg's row r blocks reclamation. The caller
// holds dmu.
//
// decision: a damaged segment blocks only while the index may still name a
// corrupt record in it, which is a record served from a copy whose repoint has
// not moved it yet: a hold's owed claim, or a repoint the index refused. A
// record dropped as corrupt leaves the index in the same call that marks the
// segment, so it never blocks, and a readable neighbour in the segment is
// content the segment can produce, which a repack carries like any other. A
// retired segment blocks nothing. In a read-only journal, which takes no hold,
// a refused repoint is not counted: reclamation does not run there, and the
// next read of the record tries the repoint again. Overturned if a corrupt
// record can stay in the index past its Corrupt call some other way, which
// would then have to be counted here too.
func (d *Durability) blocksLocked(seg core.SegmentID, r *damagedRow) bool {
	return !r.retired && d.unproduced[seg] > 0
}

// ReclaimBlocked reports whether seg is damaged and still backs held extents
// it cannot produce. It turns false once none is held, which is the signal
// that the segment is ready to be repacked; the row and the count stay.
//
// decision: nothing but tests calls this; its signature is held up only by the
// later reclamation work that selects segments.
func (d *Durability) ReclaimBlocked(seg core.SegmentID) bool {
	d.dmu.Lock()
	defer d.dmu.Unlock()
	r, ok := d.damaged[seg]
	return ok && d.blocksLocked(seg, r)
}

// damagedReport returns the count and the rows in segment order. It reads the
// rows and the counts they block on under dmu; it takes no stream lock, makes
// no index call and walks nothing.
func (d *Durability) damagedReport() (uint64, []DamagedRow) {
	d.dmu.Lock()
	rows := make([]DamagedRow, 0, len(d.damaged))
	for seg, r := range d.damaged {
		rows = append(rows, DamagedRow{Segment: seg, Retired: r.retired, BlocksReclaim: d.blocksLocked(seg, r)})
	}
	d.dmu.Unlock()
	slices.SortFunc(rows, func(a, b DamagedRow) int { return cmp.Compare(a.Segment, b.Segment) })
	return uint64(len(rows)), rows
}
