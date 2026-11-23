package journal

import "github.com/cubbit/dittofs/pkg/block/journal/internal/core"

// MarksRecorded hands the journal the marks a committed metadata transaction
// recorded, the ones Stats.SyncMarks gave the group commit. It raises the
// figure Stats.SyncMarks serves to them where they are newer, so that after
// MarksRecorded(m) no later Stats names an entry of m's stream older than m,
// and it hands each mark to the reclaim tracker, which releases an ended
// segment held for the copies a recorded mark now proves. It changes nothing
// after Close, and it never blocks: it takes no lock a write holds.
func (j *Journal) MarksRecorded(marks []SyncMark) {
	if j.closed.Load() || j.core == nil || len(marks) == 0 {
		return
	}
	j.core.Durability().RaiseMarks(marks)
	for _, m := range marks {
		j.core.Liveness().MarksRecorded(core.SegmentID(m.Segment), m.Through)
	}
}
