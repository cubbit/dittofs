package recovery

import (
	"slices"
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

// TestATornCopyNeverDropsAGoodCopyWithItsNumbers holds the exclusion at the
// repair's heart: the torn verdict drops the PLACEMENT the record was torn
// at, never the record's numbers, so a good copy of the record elsewhere —
// the copy a sealed segment carried beside the original, or a later copy
// beside both — re-places and the record stays held by it. The rebuild keeps
// a record's greater placement as its home, so before the repair the index
// answers the record from the torn placement the tail read; after it, the
// surviving copy answers, and the copy the rebuild had displaced holds
// nothing.
func TestATornCopyNeverDropsAGoodCopyWithItsNumbers(t *testing.T) {
	const overwrite = 0x4100
	rotPayload := func(seg []byte, at int64) {
		seg[at+format.RecordHeaderSize+8+2] ^= 0xff // the cache reads the durable rot
	}
	t.Run("one good copy sealed before the torn tail", func(t *testing.T) {
		a := tailRec(200, overwrite)
		w := tailRec(201, overwrite) // over A's whole extent, R's first home
		seg1, _ := buildSealed(1, a, w)
		rAt1 := int64(format.SegmentHeaderSize) + tailLen // R's offset in segment 1

		tornSeg := buildSeg(2, tailRec(201, overwrite)) // R's second home, same numbers
		copy(tornSeg, raw.SegmentHeader{Identity: journalID, ID: 2, Predecessor: 1}.Bytes())
		tornAt := int64(format.SegmentHeaderSize)
		rotPayload(tornSeg, tornAt)

		r := newRig(t)
		r.writeFormat()
		r.plantSeg(1, seg1)
		r.plantSeg(2, tornSeg)

		st := runTails(t, r)

		noTornReport(t, st)
		wantPlacement(t, st, overwrite, index.Placed{
			Extent:  core.Extent{Off: overwrite, Len: 16},
			Record:  index.RecordAt{Segment: 1, Offset: uint32(rAt1)},
			Version: core.Version{Counter: 201},
			Seq:     100,
		})
		if got := planted(st); got != 1 {
			t.Errorf("the index holds %d entries, want 1: the good copy of R alone, the torn placement's dropped as never written", got)
		}
		wantFate(t, st, 2, fateTornTail, tornAt)
	})
	t.Run("two good copies reach the fold, the greater re-places", func(t *testing.T) {
		a := tailRec(200, overwrite)
		w := tailRec(201, overwrite)
		seg1, _ := buildSealed(1, a, w)
		seg2, _ := buildSealed(2, tailRec(201, overwrite)) // R carried forward, same numbers
		rAt2 := int64(format.SegmentHeaderSize)

		tornSeg := buildSeg(3, tailRec(201, overwrite)) // R beside both, same numbers
		copy(tornSeg, raw.SegmentHeader{Identity: journalID, ID: 3, Predecessor: 2}.Bytes())
		tornAt := int64(format.SegmentHeaderSize)
		rotPayload(tornSeg, tornAt)

		r := newRig(t)
		r.writeFormat()
		r.plantSeg(1, seg1)
		r.plantSeg(2, seg2)
		r.plantSeg(3, tornSeg)

		st := runTails(t, r)

		noTornReport(t, st)
		// Both surviving copies reach the fold, and it keeps what the rebuild
		// keeps of two copies of one record: the one at the greater
		// (segment, offset), the copy in segment 2.
		wantPlacement(t, st, overwrite, index.Placed{
			Extent:  core.Extent{Off: overwrite, Len: 16},
			Record:  index.RecordAt{Segment: 2, Offset: uint32(rAt2)},
			Version: core.Version{Counter: 201},
			Seq:     100,
		})
		if got := planted(st); got != 1 {
			t.Errorf("the index holds %d entries, want 1: the good copy of R alone, the torn placement's dropped as never written", got)
		}
		wantFate(t, st, 3, fateTornTail, tornAt)
	})
}

// TestTheRepairFoldsNoRecordTheCatalogueConfined holds the two paths' shared
// record set: a repair re-walks a segment's headers fresh while the rebuild
// may have read the segment by its catalogue — the two sources can then sit
// over different verdicts for one record some day — so the placements the
// rebuild judged dropped, here a clone target whose spec the catalogue
// confined, must stay out of the fold whichever way the fresh walk reads
// them. Segment 1 is named and sealed, read by its catalogue; its confined
// clone target C cuts the extent G — the write beside it, of the same file —
// while segment 2's newest tail holds a torn write T over the same extent.
// The repair of T's displacement re-walks segment 1 and may not re-apply
// what the rebuild never fed: G holds its extent again, C cuts nothing.
func TestTheRepairFoldsNoRecordTheCatalogueConfined(t *testing.T) {
	const overwrite = 0x4100
	g := tailRec(200, overwrite)
	c := rec(7, func(r *raw.Record) { // a clone target of the tail's own file, cutting G's extent
		r.FileID = rec(1, nil).FileID
		r.Version = raw.Version{Counter: 300}
		r.Clone = &raw.CloneBody{Share: srcShare(), File: srcFile(), Off: 0x101, AsOf: raw.Version{Counter: 9}}
	})
	b, _ := buildSealed(1, g, c)
	// C's spec is the one body a content record carries: rotted where it
	// sits, the catalogue reads it, confines C and feeds the rest.
	cAt := int64(len(buildSeg(1, g)))
	b[cAt+format.RecordHeaderSize] ^= 0xff // C's spec

	tornSeg := buildSeg(2, tailRec(250, overwrite)) // T, over G's whole extent
	copy(tornSeg, raw.SegmentHeader{Identity: journalID, ID: 2, Predecessor: 1}.Bytes())
	tornAt := int64(format.SegmentHeaderSize)
	tornSeg[tornAt+format.RecordHeaderSize+8+2] ^= 0xff // T's payload

	r := newRig(t)
	r.writeFormat()
	r.plantSeg(1, b)
	r.plantSeg(2, tornSeg)

	st := runTails(t, r)

	if got := st.Rebuilt.ByCatalog; got != 1 {
		t.Fatalf("Run rebuilt %d segments by catalog, want 1: the confined record never demotes its segment", got)
	}
	if sc, ok := scanOf(st, 1); !ok || len(sc.Dropped) != 1 || sc.Dropped[0].Header.Offset != cAt {
		t.Fatalf("segment 1's rebuild scan got %d drops (found %v), want the confined clone target at %d", len(sc.Dropped), ok, cAt)
	}
	if len(st.Tails.Corrupt) != 1 || st.Tails.Corrupt[0].Segment != 1 {
		t.Errorf("Corrupt got %+v, want exactly segment 1's confined clone target", st.Tails.Corrupt)
	}
	if !slices.Equal(st.Tails.Damaged, []core.SegmentID{1}) {
		t.Errorf("Damaged got %v, want [1]: the confined record was sealed over", st.Tails.Damaged)
	}
	// The torn report is T's alone: no re-append, no failed window, and the
	// segment's fate is the torn tail the classing owned.
	if len(st.Proof.Reappends) != 0 || len(st.Proof.FailedWindows) != 0 {
		t.Errorf("Reappends got %+v, FailedWindows got %v, want none: the torn T is never written, never re-appended", st.Proof.Reappends, st.Proof.FailedWindows)
	}
	wantFate(t, st, 2, fateTornTail, tornAt)
	// G's placement holds the extent T displaced again — the confined C cut
	// nothing, for the rebuild never fed it and the repair may not either.
	wantPlacement(t, st, overwrite, index.Placed{
		Extent:  core.Extent{Off: overwrite, Len: 16},
		Record:  index.RecordAt{Segment: 1, Offset: format.SegmentHeaderSize},
		Version: core.Version{Counter: 200},
		Seq:     100,
	})
}
