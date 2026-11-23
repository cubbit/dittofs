package durability

import (
	"cmp"
	"slices"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
)

// SeedMarks sets the figure of each stream's last completed sync to what the
// open recovered of it, one mark per stream. It is called once, before the
// layer serves, and the figure only rises from there.
func (d *Durability) SeedMarks(marks []core.SyncMark) {
	m := slices.Clone(marks)
	slices.SortFunc(m, func(a, b core.SyncMark) int { return cmp.Compare(a.Stream, b.Stream) })
	d.lastMarks.Store(&m)
}

// Marks returns the figure: per stream, the mark of its last completed sync,
// or, until the stream's next sync after an open, what that open recovered of
// it, ordered by stream. The slice belongs to the caller. It reads one atomic
// pointer, so it takes no lock a write holds.
//
// decision: the figure follows the marks Sync returns, so a sync no caller
// named a file for, the timer's, raises nothing: a mark proves the records a
// call named, not the stream's whole synced prefix. It never falls after the
// seed, which is the open's own lowering of what the device lost.
func (d *Durability) Marks() []core.SyncMark {
	if p := d.lastMarks.Load(); p != nil {
		return slices.Clone(*p)
	}
	return nil
}

// RaiseMarks raises each stream's entry of the figure to the mark given for
// it where that mark is newer, and adds a stream the figure has no entry for.
// A mark not newer than the entry changes nothing. Concurrent calls merge:
// each retries over the figure it lost the swap to.
func (d *Durability) RaiseMarks(marks []core.SyncMark) {
	if len(marks) == 0 {
		return
	}
	for {
		old := d.lastMarks.Load()
		var cur []core.SyncMark
		if old != nil {
			cur = *old
		}
		next := slices.Clone(cur)
		changed := false
		for _, m := range marks {
			i, found := slices.BinarySearchFunc(next, m.Stream, func(e core.SyncMark, s uint32) int { return cmp.Compare(e.Stream, s) })
			switch {
			case !found:
				next = slices.Insert(next, i, m)
				changed = true
			case markNewer(m, next[i]):
				next[i] = m
				changed = true
			}
		}
		if !changed || d.lastMarks.CompareAndSwap(old, &next) {
			return
		}
	}
}

// markNewer reports that a is a later mark of a stream than b: it names a
// later record, by sequence number, or, naming the same one, the copy in a
// later segment, or the same segment synced through further.
//
// decision: sequence numbers order a stream's marks, not segment ids. A
// segment id says when the segment was made, not when its records were
// appended: a spare made early is used after a copy segment made later, so a
// new record can sit in a lower segment than the open's recovered mark names,
// and an order by segment would keep the older mark for good. Overturned if
// a record can ever be appended with a lower sequence number than one already
// synced on its stream.
func markNewer(a, b core.SyncMark) bool {
	if a.Sequence != b.Sequence {
		return a.Sequence > b.Sequence
	}
	if a.Segment != b.Segment {
		return a.Segment > b.Segment
	}
	if a.Through != b.Through {
		return a.Through > b.Through
	}
	return a.Offset > b.Offset
}
