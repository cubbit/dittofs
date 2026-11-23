package recovery

import (
	"slices"
	"testing"

	"github.com/cubbit/dittofs/pkg/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/journal/internal/format"
)

// planExtent is the extent each piece of the plan table names: one per seq,
// so the (seq, extent) key of the piece is the seq alone.
func planExtent(seq uint64) core.Extent {
	return core.Extent{Off: int64(seq) * 32, Len: 32}
}

// planFailed is one failed record of the table: a whole record of seq the
// feed found on disk, the shape the failed leg's inputs reach it through.
func planFailed(seq uint64) ReadOnlyDrop {
	return ReadOnlyDrop{
		Ev: core.LossEvent{
			Share:   1,
			File:    core.NewFileID([16]byte{1}),
			Extent:  planExtent(seq),
			Version: core.Version{Counter: seq},
		},
		Dropped: core.Seq(seq),
		// The stands-word: the failing read-only journal lives in this
		// process, its ring still readable.
		Reported: true,
	}
}

// unreportedFailedDrop is one failed-leg row whose report nobody answered:
// the failing instance stopped before any reader reached its event, so the
// planner's re-append publishes the loss instead of holding it back.
func unreportedFailedDrop(seq uint64) ReadOnlyDrop {
	d := planFailed(seq)
	d.Reported = false
	return d
}

// planCorrupt is one corrupt drop of the table: a rotted record of seq the
// classing confined in its own segment, the header the corrupt leg's Loss
// columns read.
func planCorrupt(seg, seq uint64) corruptRecord {
	return corruptRecord{
		Segment: core.SegmentID(seg),
		Drop: droppedRecord{
			Header: format.RecordHeader{
				Kind:    format.KindWrite,
				File:    core.NewFileID([16]byte{1}),
				Extent:  planExtent(seq),
				Seq:     core.Seq(seq),
				Version: core.Version{Counter: seq},
				Tag:     1,
			},
			Synced: true,
		},
	}
}

// TestPlanOpenLossesSpendsTheReserveOnce holds the planner's one accounting
// of the open reserve — the records the failed leg planned, the corrupt
// drops after them, nothing twice:
//
//   - the failed leg's records take the reserve first, and a corrupt drop
//     past what they left is planned as a deferral against its own segment;
//   - an exact reserve — what the failed records and the corrupt drops want
//     together — plans them all as appends, and one record less defers the
//     corrupt drop;
//   - a piece both lists name is spent once, by the failed leg when the
//     reserve held it, by neither when it ran past — so the corrupt twin of
//     a failed piece beyond the reserve plans no deferral;
//   - a piece a record on disk already names plans no append, draws nothing
//     and defers nothing;
//   - the same corrupt piece twice — the same drop reaching both the marks'
//     and the proof's corrupt lists — is one plan entry.
func TestPlanOpenLossesSpendsTheReserveOnce(t *testing.T) {
	type wanted struct {
		seq      core.Seq
		failed   bool           // the failed leg planned it; the corrupt leg otherwise
		reported bool           // the suppression the failed leg's entry still carries
		defers   core.SegmentID // the segment a deferred corrupt drop defers against; zero appends
	}
	cases := []struct {
		name     string
		failed   []ReadOnlyDrop
		corrupt  []corruptRecord
		reserve  int
		named    []core.Seq // the pieces a record on disk already names
		unsynced []core.Seq // of those, the pieces a placed but unsynced record holds
		want     []wanted
	}{
		{
			name:    "an exact reserve holds the failed records and the corrupt drop",
			failed:  []ReadOnlyDrop{planFailed(10), planFailed(11)},
			corrupt: []corruptRecord{planCorrupt(951, 12)},
			reserve: 3,
			want:    []wanted{{10, true, true, 0}, {11, true, true, 0}, {12, false, true, 0}},
		},
		{
			name:    "one record less defers the corrupt drop",
			failed:  []ReadOnlyDrop{planFailed(10), planFailed(11)},
			corrupt: []corruptRecord{planCorrupt(951, 12)},
			reserve: 2,
			want:    []wanted{{10, true, true, 0}, {11, true, true, 0}, {12, false, true, 951}},
		},
		{
			// The failed leg's records take the reserve whole before any
			// corrupt drop's: none is left for the drop, whose record is owed
			// to the copy that retires its segment, so it defers.
			name:    "the failed leg takes the reserve and the corrupt drop defers",
			failed:  []ReadOnlyDrop{planFailed(10)},
			corrupt: []corruptRecord{planCorrupt(951, 12)},
			reserve: 1,
			want:    []wanted{{10, true, true, 0}, {12, false, true, 951}},
		},
		{
			name:   "a corrupt twin of a failed piece past the reserve defers nothing",
			failed: []ReadOnlyDrop{planFailed(10), planFailed(11)},
			// The twin of the second failed record: the failed leg ran past
			// the reserve before it, so only the first fits — and the twin
			// is the failed leg's own piece, an open's answer for the next
			// open's failed list, never a deferral of this one.
			corrupt: []corruptRecord{planCorrupt(951, 11)},
			reserve: 1,
			want:    []wanted{{10, true, true, 0}},
		},
		{
			name:    "a corrupt twin of a failed piece the reserve held plans no second",
			failed:  []ReadOnlyDrop{planFailed(10)},
			corrupt: []corruptRecord{planCorrupt(951, 10)},
			reserve: 1,
			want:    []wanted{{10, true, true, 0}},
		},
		{
			name:    "a piece a disk record already names plans nothing",
			failed:  []ReadOnlyDrop{planFailed(10)},
			corrupt: []corruptRecord{planCorrupt(951, 12)},
			reserve: 2,
			named:   []core.Seq{10, 12},
		},
		{
			// A loss record placed but never synced holds its piece named,
			// yet nothing durable names the loss: the corrupt leg appends it
			// again, already reported and raised, on the reserve.
			name:     "a piece only a placed but unsynced record names plans its append",
			corrupt:  []corruptRecord{planCorrupt(951, 12), planCorrupt(951, 13)},
			reserve:  2,
			named:    []core.Seq{12, 13},
			unsynced: []core.Seq{12},
			want:     []wanted{{12, false, true, 0}},
		},
		{
			// Past the reserve such a piece defers like any unnamed drop,
			// still reported and raised.
			name:     "a piece only a placed but unsynced record names defers past the reserve",
			corrupt:  []corruptRecord{planCorrupt(951, 12)},
			reserve:  0,
			named:    []core.Seq{12},
			unsynced: []core.Seq{12},
			want:     []wanted{{12, false, true, 951}},
		},
		{
			// The registry's word on the failed entry drives the
			// suppression: a failure whose reporter stopped before any
			// reader reached its event plans the loss published, not
			// suppressed — the open that re-appends the record the
			// failure owed republishes it itself.
			name:    "a failed record whose report nobody answered plans its republish",
			failed:  []ReadOnlyDrop{unreportedFailedDrop(10)},
			reserve: 1,
			want:    []wanted{{10, true, false, 0}},
		},
		{
			// The failed leg's piece is the failed list's to answer, not the
			// corrupt leg's, so a held piece there plans nothing new.
			name:     "a failed-leg piece a placed but unsynced record names plans nothing",
			failed:   []ReadOnlyDrop{planFailed(10)},
			reserve:  1,
			named:    []core.Seq{10},
			unsynced: []core.Seq{10},
		},
		{
			name: "the same corrupt piece twice plans one entry",
			// The same (seq, extent) reaching the corrupt list through two
			// lists, as the marks' and the proof's each answer one drop.
			corrupt: []corruptRecord{planCorrupt(950, 12), planCorrupt(951, 12)},
			reserve: 2,
			want:    []wanted{{12, false, true, 0}},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			st := State{}
			st.Kinds.ReadOnlyDrops = c.failed
			st.Tails.Corrupt = c.corrupt
			namedSeqs := make(map[core.Seq]bool, len(c.named))
			for _, seq := range c.named {
				namedSeqs[seq] = true
			}
			named := func(seq core.Seq, ext core.Extent) bool {
				return namedSeqs[seq] && ext == planExtent(uint64(seq))
			}
			unsynced := func(seq core.Seq, ext core.Extent) bool {
				return slices.Contains(c.unsynced, seq) && ext == planExtent(uint64(seq))
			}

			plan, _ := planOpenLosses(st, named, unsynced, c.reserve)

			if len(plan) != len(c.want) {
				t.Fatalf("the plan holds %d losses (%+v), want the %d of the reserve's arithmetic", len(plan), plan, len(c.want))
			}
			for i, w := range c.want {
				got := plan[i]
				if got.Seq != w.seq {
					t.Errorf("plan entry %d = seq %d, want %d", i, got.Seq, w.seq)
				}
				if w.failed && (got.Reason != core.LossFailedWindow || got.Reported != w.reported) {
					t.Errorf("plan entry %d = %+v, want the failed leg's: reason %d and the entry's own reported %t", i, got, core.LossFailedWindow, w.reported)
				}
				// A corrupt-leg piece a placed but unsynced record holds was
				// published by the failed placement: it appends reported and
				// raised, so no second event or raise goes out for it.
				reported := slices.Contains(c.unsynced, w.seq)
				if !w.failed && (got.Reason != core.LossCorrupt || got.Reported != reported || got.Raised != reported) {
					t.Errorf("plan entry %d = %+v, want the corrupt leg's: reason %d, reported and raised %v", i, got, core.LossCorrupt, reported)
				}
				if got.Deferred != (w.defers != 0) || got.Seg != w.defers {
					t.Errorf("plan entry %d = deferred %t against segment %d, want deferred against segment %d (zero appends)", i, got.Deferred, got.Seg, w.defers)
				}
				if !got.AtOpen {
					t.Errorf("plan entry %d is not marked AtOpen", i)
				}
			}
		})
	}
}
