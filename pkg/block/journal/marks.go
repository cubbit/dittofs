package journal

import "github.com/cubbit/dittofs/pkg/block/journal/internal/core"

// SyncMarks returns Stats.SyncMarks as a slice ordered by stream, without
// building the rest of Stats: the group commit reads it at each commit. The
// slice belongs to the caller; a closed journal returns none.
//
// decision: the method is not in RFC 1's list. It serves the §3.7 field of the
// same name without building the rest of Stats, which the engine's group
// commit would otherwise build once per commit. Overturned if RFC 1 names the
// engine's read of the marks in another form.
func (j *Journal) SyncMarks() []SyncMark {
	if j.closed.Load() || j.core == nil {
		return nil
	}
	return j.core.Durability().Marks()
}

// MarksRecorded hands the journal the marks a committed metadata transaction
// recorded, the ones SyncMarks gave the group commit. It hands each mark to
// the reclaim tracker, which releases an ended segment held for the copies a
// recorded mark now proves. It changes nothing in what SyncMarks serves, which
// only a Sync raises: a mark recorded was read from SyncMarks, and SyncMarks
// never falls, so it is never older than what was recorded. It does nothing
// after Close, and it never blocks: it takes no lock a write holds.
func (j *Journal) MarksRecorded(marks []SyncMark) {
	if j.closed.Load() || j.core == nil {
		return
	}
	for _, m := range marks {
		j.core.Liveness().MarksRecorded(core.SegmentID(m.Segment), m.Through)
	}
}
