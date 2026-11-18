package recovery

import (
	"testing"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/format"
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
	}
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
// drops after them, nothing twice, and a refusal that answers before any
// record the plan would strand:
//
//   - the failed leg's records take the reserve first, and a corrupt drop
//     past what they left refuses the whole plan, naming its own segment;
//   - an exact reserve — what the failed records and the corrupt drops want
//     together — plans them all, and one record less refuses;
//   - a piece both lists name is spent once, by the failed leg when the
//     reserve held it, by neither when it ran past — so the corrupt twin of
//     a failed piece beyond the reserve turns no open into a refusal;
//   - a piece a record on disk already names plans no append, draws nothing
//     and refuses nothing;
//   - the same corrupt piece twice — the same drop reaching both the marks'
//     and the proof's corrupt lists — is one plan entry.
func TestPlanOpenLossesSpendsTheReserveOnce(t *testing.T) {
	type wanted struct {
		seq    core.Seq
		failed bool // the failed leg planned it; the corrupt leg otherwise
	}
	cases := []struct {
		name      string
		failed    []ReadOnlyDrop
		corrupt   []corruptRecord
		reserve   int
		named     []core.Seq // the pieces a record on disk already names
		want      []wanted
		refuseN   int
		refuseSeg core.SegmentID
	}{
		{
			name:    "an exact reserve holds the failed records and the corrupt drop",
			failed:  []ReadOnlyDrop{planFailed(10), planFailed(11)},
			corrupt: []corruptRecord{planCorrupt(951, 12)},
			reserve: 3,
			want:    []wanted{{10, true}, {11, true}, {12, false}},
		},
		{
			name:      "one record less refuses the corrupt drop",
			failed:    []ReadOnlyDrop{planFailed(10), planFailed(11)},
			corrupt:   []corruptRecord{planCorrupt(951, 12)},
			reserve:   2,
			refuseN:   1,
			refuseSeg: 951,
		},
		{
			// The failed leg's records take the reserve whole before any
			// corrupt drop's: none is left for the drop, whose record is owed
			// to the copy that retires its segment, so the open refuses
			// rather than defer it.
			name:      "the failed leg takes the reserve and the corrupt drop refuses",
			failed:    []ReadOnlyDrop{planFailed(10)},
			corrupt:   []corruptRecord{planCorrupt(951, 12)},
			reserve:   1,
			refuseN:   1,
			refuseSeg: 951,
		},
		{
			name:   "a corrupt twin of a failed piece past the reserve refuses nothing",
			failed: []ReadOnlyDrop{planFailed(10), planFailed(11)},
			// The twin of the second failed record: the failed leg ran past
			// the reserve before it, so only the first fits — and the twin
			// is the failed leg's own piece, an open's answer for the next
			// open's failed list, never a refusal of this one.
			corrupt: []corruptRecord{planCorrupt(951, 11)},
			reserve: 1,
			want:    []wanted{{10, true}},
		},
		{
			name:    "a corrupt twin of a failed piece the reserve held plans no second",
			failed:  []ReadOnlyDrop{planFailed(10)},
			corrupt: []corruptRecord{planCorrupt(951, 10)},
			reserve: 1,
			want:    []wanted{{10, true}},
		},
		{
			name:    "a piece a disk record already names plans nothing",
			failed:  []ReadOnlyDrop{planFailed(10)},
			corrupt: []corruptRecord{planCorrupt(951, 12)},
			reserve: 2,
			named:   []core.Seq{10, 12},
		},
		{
			name: "the same corrupt piece twice plans one entry",
			// The same (seq, extent) reaching the corrupt list through two
			// lists, as the marks' and the proof's each answer one drop.
			corrupt: []corruptRecord{planCorrupt(950, 12), planCorrupt(951, 12)},
			reserve: 2,
			want:    []wanted{{12, false}},
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

			plan, refuse := planOpenLosses(st, named, c.reserve)

			if c.refuseN > 0 {
				if refuse == nil {
					t.Fatalf("the plan answered no refusal, want %d losses against segment %d past the reserve", c.refuseN, c.refuseSeg)
				}
				if refuse.N != c.refuseN || refuse.Seg != c.refuseSeg {
					t.Fatalf("the refusal = %d against segment %d, want %d against %d", refuse.N, refuse.Seg, c.refuseN, c.refuseSeg)
				}
				if plan != nil {
					t.Fatalf("a refusing plan holds %+v, want none: the refusal strands no record", plan)
				}
				return
			}
			if refuse != nil {
				t.Fatalf("the plan refused with %d losses against segment %d, want none", refuse.N, refuse.Seg)
			}
			if len(plan) != len(c.want) {
				t.Fatalf("the plan holds %d losses (%+v), want the %d of the reserve's arithmetic", len(plan), plan, len(c.want))
			}
			for i, w := range c.want {
				got := plan[i]
				if got.Seq != w.seq {
					t.Errorf("plan entry %d = seq %d, want %d", i, got.Seq, w.seq)
				}
				if w.failed && (got.Reason != core.LossFailedWindow || !got.Reported) {
					t.Errorf("plan entry %d = %+v, want the failed leg's: reason %d and reported", i, got, core.LossFailedWindow)
				}
				if !w.failed && (got.Reason != core.LossCorrupt || got.Reported) {
					t.Errorf("plan entry %d = %+v, want the corrupt leg's: reason %d and unreported", i, got, core.LossCorrupt)
				}
				if !got.AtOpen {
					t.Errorf("plan entry %d is not marked AtOpen", i)
				}
			}
		})
	}
}
