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
			case d.markNewer(m, next[i]):
				next[i] = m
				changed = true
			}
		}
		if !changed || d.lastMarks.CompareAndSwap(old, &next) {
			return
		}
	}
}

// markNewer reports that a is a later mark of a stream than b.
//
// In one segment the mark synced through further is later, whatever the
// sequence numbers of the records it names: a sequence number is drawn before
// the append, so two files of a stream can place their records in the
// opposite order. Across segments the mark in the segment the stream appends
// to now is later, since a stream only moves forward: a copy the open or a
// failed window made lands in the segment the stream takes next, and a
// segment the stream left is behind it. When neither mark is in the open
// segment the stream is past both, and nothing orders them: the entry the
// figure holds stays, and the stream's next sync in its open segment raises
// it.
//
// ponytail: a mark proved while its segment seals is dropped, because the
// stream reports no open segment then, so the figure stays on the segment
// before until a named sync in the next one. Upgrade when the stream reports
// the last segment it took, so the order holds while it seals.
//
// decision: neither segment ids nor sequence numbers order marks across
// segments. An id says when a segment was made, not when its records were
// appended, a spare made early being used after a copy segment made later;
// and a re-appended copy keeps its record's number, so a copy's mark carries a
// lower one than the ended segment's last. A mark that the open segment cannot
// place is therefore never taken over what is held. Overturned if a stream
// can append to a segment other than the one it reports open.
func (d *Durability) markNewer(a, b core.SyncMark) bool {
	if a.Segment == b.Segment {
		if a.Through != b.Through {
			return a.Through > b.Through
		}
		if a.Offset != b.Offset {
			return a.Offset > b.Offset
		}
		return a.Sequence > b.Sequence
	}
	if open, ok := d.openSegmentOf(a.Stream); ok {
		switch uint64(open) {
		case a.Segment:
			return true
		case b.Segment:
			return false
		}
	}
	return false
}

// openSegmentOf is the segment the stream appends to, false before Start, for
// a stream the layer has none of, and while the stream's segment is sealing,
// sealed or ended.
func (d *Durability) openSegmentOf(stream uint32) (core.SegmentID, bool) {
	streams := d.streams.Load()
	if streams == nil || int(stream) >= streams.Len() {
		return 0, false
	}
	return streams.Stream(int(stream)).OpenSegment()
}
