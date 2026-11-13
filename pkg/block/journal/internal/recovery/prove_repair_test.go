package recovery

import (
	"testing"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/format"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/index"
	"github.com/cubbit/dittofs/pkg/block/journal/journaltest/raw"
)

// wantPlacement checks the placement the rebuilt index answers for off of the
// tail's own file: the record that holds it, where its bytes sit, the extent
// covered and the version carried, so a leg names the whole answer a read
// would serve from, not only that something is held there.
func wantPlacement(t *testing.T, st State, off int64, want index.Placed) {
	t.Helper()
	got, ok := st.Rebuilt.Index.Lookup(tailFile(), off)
	if !ok {
		t.Fatalf("Lookup of the tail's file at %d answered nothing, want the placement %+v: the torn record's drop restored nothing", off, want)
	}
	if got != want {
		t.Errorf("Lookup of the tail's file at %d got %+v, want %+v: the torn record's drop restores the placement of the record it overwrote", off, got, want)
	}
}

// noTornReport checks that the torn record reached no reporter: the re-append
// takes none — a torn record has no copy its numbers could be kept from — and
// the damaged set names none, for torn is never written and never damage.
func noTornReport(t *testing.T, st State) {
	t.Helper()
	if len(st.Proof.Reappends) != 0 {
		t.Errorf("Reappends got %+v, want none: a torn record is re-appended nowhere", st.Proof.Reappends)
	}
	if len(st.Tails.Damaged) != 0 {
		t.Errorf("Damaged got %v, want none: a record the proof finds torn is never damage", st.Tails.Damaged)
	}
	if len(st.Proof.FailedWindows) != 0 {
		t.Errorf("FailedWindows got %v, want none: the window's sync succeeded", st.Proof.FailedWindows)
	}
}

// TestATornRecordOverASyncedWriteRestoresItsPlacement holds the repair's own
// rule: a torn record is treated as never written, so the index must come out
// exactly as it would had the rebuild never been fed the record — and the
// record it overwrote, whose piece the replay cut, holds the extent again.
// The overwrite runs in both shapes the journal writes them in: B over A in
// one segment, and B in the stream's newest segment over synced A sealed in
// the named one before it.
func TestATornRecordOverASyncedWriteRestoresItsPlacement(t *testing.T) {
	endA := endAfter(tailRec(200, 0x4100))
	const overwrite = 0x4100 // A's whole extent is B's too
	t.Run("in the same segment", func(t *testing.T) {
		a := syncedRec(200, overwrite, endA)
		b := syncedRec(201, overwrite, endA)
		seg := buildSeg(2, a, b)
		seg[endA+format.RecordHeaderSize+8+2] ^= 0xff // B's payload, rotted where the cache reads the same bytes
		r := newRig(t)
		r.writeFormat()
		r.plantSeg(2, seg)

		st := runTails(t, r) // a torn record never refuses the open

		noTornReport(t, st)
		wantPlacement(t, st, overwrite, index.Placed{
			Extent:  core.Extent{Off: overwrite, Len: 16},
			Record:  index.RecordAt{Segment: 2, Offset: format.SegmentHeaderSize},
			Version: core.Version{Counter: 200},
			Seq:     100,
		})
		if got := planted(st); got != 1 {
			t.Errorf("the index holds %d entries, want 1: A's entry alone, the torn B's dropped as never written", got)
		}
		// The torn tail rule the classing owns applies here too: nothing
		// holds past B, so the segment seals at B's own start rather than
		// ending there.
		wantFate(t, st, 2, fateTornTail, endA)
	})
	t.Run("in the segment after A's", func(t *testing.T) {
		a := syncedRec(200, overwrite, endA)
		b := tailRec(201, overwrite) // over A's whole extent, this segment's own tail
		seg1, _ := buildSealed(1, a)
		seg2 := buildSeg(2, b)
		copy(seg2, raw.SegmentHeader{Identity: journalID, ID: 2, Predecessor: 1}.Bytes())
		bAt := int64(format.SegmentHeaderSize) // B is segment 2's first record
		seg2[bAt+format.RecordHeaderSize+8+2] ^= 0xff
		r := newRig(t)
		r.writeFormat()
		r.plantSeg(1, seg1)
		r.plantSeg(2, seg2)

		st := runTails(t, r)

		noTornReport(t, st)
		// The repair reaches across the stream: A's record sits in the named
		// segment sealed before, and its placement names that segment still.
		wantPlacement(t, st, overwrite, index.Placed{
			Extent:  core.Extent{Off: overwrite, Len: 16},
			Record:  index.RecordAt{Segment: 1, Offset: format.SegmentHeaderSize},
			Version: core.Version{Counter: 200},
			Seq:     100,
		})
		if got := planted(st); got != 1 {
			t.Errorf("the index holds %d entries, want 1: A's entry alone, the torn B's dropped as never written", got)
		}
		wantFate(t, st, 2, fateTornTail, bAt)
	})
}
