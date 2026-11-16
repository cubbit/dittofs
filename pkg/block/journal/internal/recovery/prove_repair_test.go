package recovery

import (
	"context"
	"math"
	"slices"
	"sync/atomic"
	"testing"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/format"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/index"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/storage"
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
		r.Seq = 103 // no other record of C's numbers anywhere: the copies rule masks no drop of its seq
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

// TestTheRepairReadsACataloguedSegmentTheWayTheRebuildDid holds the repair's
// source rule: the survivors come from the same source the rebuild read each
// segment by, or the two disagree at the records only the catalogue can name.
// Segment 1 is named and sealed, holding the write G whose record header
// rotted; the rebuild index G from its catalogue entry, while the segment's
// header walk refuses the rotted header and answers nothing. The stream's
// newest segment holds the torn write T over G's whole extent, so T's repair
// collects G from the segment — and G, read the catalogue's way, survives
// the fold: its index entry is re-placed, and a sealed durable write is not
// silently lost, or served stale from T's dropped placement.
func TestTheRepairReadsACataloguedSegmentTheWayTheRebuildDid(t *testing.T) {
	const overwrite = 0x4100
	g := tailRec(200, overwrite)
	seg1, _ := buildSealed(1, g)
	gAt := int64(format.SegmentHeaderSize)
	seg1[gAt] ^= 0xff // G's record header, rotted where it sits: its catalogue entry stands in

	tornSeg := buildSeg(2, tailRec(250, overwrite)) // T, over G's whole extent
	copy(tornSeg, raw.SegmentHeader{Identity: journalID, ID: 2, Predecessor: 1}.Bytes())
	tornAt := int64(format.SegmentHeaderSize)
	tornSeg[tornAt+format.RecordHeaderSize+8+2] ^= 0xff // T's payload, torn in both views

	r := newRig(t)
	r.writeFormat()
	r.plantSeg(1, seg1)
	r.plantSeg(2, tornSeg)

	st := runTails(t, r)

	if got := st.Rebuilt.ByCatalog; got != 1 {
		t.Fatalf("Run rebuilt %d segments by catalog, want 1: G's rotted header never refuses or demotes its segment", got)
	}
	// T alone is the tear: no re-append, no failed window, and segment 2's
	// fate is the torn tail the classing owned.
	noTornReport(t, st)
	wantFate(t, st, 2, fateTornTail, tornAt)
	// G holds its extent again: the repair re-places the record the rebuild
	// indexed, and its placement names the same segment and offset the
	// rebuild's entry did.
	wantPlacement(t, st, overwrite, index.Placed{
		Extent:  core.Extent{Off: overwrite, Len: 16},
		Record:  index.RecordAt{Segment: 1, Offset: uint32(gAt)},
		Version: core.Version{Counter: 200},
		Seq:     100,
	})
	if got := planted(st); got != 1 {
		t.Errorf("the index holds %d entries, want 1: G alone, the torn T dropped as never written", got)
	}
}

// TestTheRepairFoldsNoRecordTheFeedWithheld holds the withheld rule over the
// repair: a record the feed withheld whole — here one whose seq the
// directory's failed list names, sealed with a synced segment before the torn
// tail — was never fed, so the repair of the torn record over its extent may
// not re-place it either. Its fresh header reads healthy, so the rebuild's
// judged set never finds it; only the withheld set does, and the index comes
// out holding nothing of what the force of the failed list kept out of it.
func TestTheRepairFoldsNoRecordTheFeedWithheld(t *testing.T) {
	const overwrite = 0x4100
	endA := endAfter(tailRec(200, 0x4100))
	a := syncedRec(200, overwrite, endA) // A, withheld whole by its seq on the failed list
	seg1, _ := buildSealed(1, a)
	b := writeRec(101, 201, overwrite, 16) // T over A's whole extent, seq the list does not name
	seg2 := buildSeg(2, b)
	copy(seg2, raw.SegmentHeader{Identity: journalID, ID: 2, Predecessor: 1}.Bytes())
	bAt := int64(format.SegmentHeaderSize)
	seg2[bAt+format.RecordHeaderSize+8+2] ^= 0xff // T's payload, torn to the first read

	r := newRig(t)
	r.writeFormat()
	r.plantSeg(1, seg1)
	r.plantSeg(2, seg2)

	st, err := r.asideRun([]core.Seq{100})
	if err != nil {
		t.Fatalf("Run over a withheld record beside a torn tail: %v", err)
	}

	noTornReport(t, st)
	wantFate(t, st, 2, fateTornTail, bAt)
	if got := planted(st); got != 0 {
		t.Errorf("the index holds %d entries, want 0: A is withheld whole and the repair may not fold it back, T torn as never written", got)
	}
	if _, ok := st.Rebuilt.Index.Lookup(tailFile(), overwrite); ok {
		t.Errorf("Lookup over the withheld record's extent hit, want a miss: the repair hands back nothing the feed withheld")
	}
}

// TestTheRepairFoldsTheCopyTheOpenKeeps holds the fold's copy rule against
// the repoint's: a record carried in two homes, one the open keeps and one
// its classing ends, was re-pointed to the surviving home when the rebuild
// finished — and the repair of a torn record over its extent must not hand
// the placement back. The fold's collapse keeps the greatest (segment,
// offset), so the survivors filter out a copy in a home the open ends while
// its numbers hold a copy in a home it keeps; the placement then names the
// segment the open keeps, never one whose emptying takes the bytes away.
func TestTheRepairFoldsTheCopyTheOpenKeeps(t *testing.T) {
	const overwrite = 0x4100
	// X, one record seq 9 v201 over the tail's file: the raw body both homes
	// carry, byte-identical numbers with it.
	xRaw := rec(1, func(rc *raw.Record) {
		rc.Seq = 9
		rc.Version = raw.Version{Counter: 201}
		rc.Offset = overwrite
		rc.Write = &raw.WriteBody{MTime: 1, Payload: bytes16()}
	})
	seg1, _ := buildSealed(1, xRaw) // X's kept home, named by segment 2's header

	// Segment 2, the home the classing ends: X again at its greater home,
	// then a torn slot a sync never proved, then a record holding past the
	// slot — the torn-slot rule ends the segment however healthy X's copy is.
	seg2 := buildSeg(2, xRaw)
	endX := int64(len(seg2))
	seg2 = append(seg2, make([]byte, 16)...)
	hold := syncedRec(202, 0x8100, endX) // synced below the slot, so nothing proves the torn bytes
	hold.Identity, hold.Segment, hold.RecordOffset = journalID, 2, uint32(len(seg2))
	seg2 = append(seg2, hold.Bytes()...)
	copy(seg2, raw.SegmentHeader{Identity: journalID, ID: 2, Predecessor: 1}.Bytes())

	// Segment 3, the newest: the torn write over X's extent.
	tornSeg := buildSeg(3, tailRec(250, overwrite))
	copy(tornSeg, raw.SegmentHeader{Identity: journalID, ID: 3}.Bytes())
	tornAt := int64(format.SegmentHeaderSize)
	tornSeg[tornAt+format.RecordHeaderSize+8+2] ^= 0xff // T's payload, torn to the first read

	r := newRig(t)
	r.writeFormat()
	r.plantSeg(1, seg1)
	r.plantSeg(2, seg2)
	r.plantSeg(3, tornSeg)

	st := runTails(t, r)

	noTornReport(t, st)
	wantFate(t, st, 2, fateEnded, 0)
	wantFate(t, st, 3, fateTornTail, tornAt)
	// The placement answers from segment 1, the home the open keeps: the
	// repoint moved the piece there and the repair's fold did not hand it
	// back to the ended home.
	wantPlacement(t, st, overwrite, index.Placed{
		Extent:  core.Extent{Off: overwrite, Len: 16},
		Record:  index.RecordAt{Segment: 1, Offset: format.SegmentHeaderSize},
		Version: core.Version{Counter: 201},
		Seq:     9,
	})
	if got := planted(st); got != 2 {
		t.Errorf("the index holds %d entries, want 2: X at its surviving home and the holding record, the torn write's placement dropped as never written", got)
	}
}

// TestTheRepairReappliesTheSyncedLossCutsItsFoldUndid holds the order the
// loss cuts and the repair's fold answer in: a synced superseded loss cut
// the write W over [0,4K) once the rebuild finished, and a torn write over
// the same extent makes the repair's fold re-place W's piece there — the
// cut must land again after the fold, or the open serves bytes the loss
// record dropped, never the hole a rebuild that saw no torn record holds.
func TestTheRepairReappliesTheSyncedLossCutsItsFoldUndid(t *testing.T) {
	const KiB = 1024
	file := tailFile()
	w := writeRec(5, 3, 0, 8*KiB)                      // W, v3 seq5 over [0,8K)
	l := plantLossRec(file, 5, 8, 4, 0, 4*KiB, 1|4<<8) // a synced superseded loss dropping seq5 over [0,4K) at v4
	seg1 := buildSeg(1, w, l)
	tornSeg := buildSeg(2, writeRec(9, 6, 0, 4*KiB)) // the torn write over the loss extent
	copy(tornSeg, raw.SegmentHeader{Identity: journalID, ID: 2}.Bytes())
	tornAt := int64(format.SegmentHeaderSize)
	tornSeg[tornAt+format.RecordHeaderSize+8+2] ^= 0xff

	r := newRig(t)
	r.writeFormat()
	r.plantSeg(1, seg1)
	r.plantSeg(2, tornSeg)

	st := runTails(t, r)

	noTornReport(t, st)
	wantFate(t, st, 2, fateTornTail, tornAt)
	if p, ok := st.Rebuilt.Index.Lookup(file, 0); ok {
		t.Errorf("Lookup at 0 got %+v, want a miss: the synced superseded cut takes the extent the repair's fold re-placed, again", p)
	}
	p, ok := st.Rebuilt.Index.Lookup(file, 4*KiB)
	if !ok {
		t.Fatalf("Lookup at 4096 missed, want the cut to leave W's [4K,8K) run held")
	}
	if p.Extent != (core.Extent{Off: 4 * KiB, Len: 4 * KiB}) || p.Version != (core.Version{Counter: 3}) || p.Seq != 5 || p.Record != (index.RecordAt{Segment: 1, Offset: format.SegmentHeaderSize}) {
		t.Errorf("the kept run got %+v, want W's [4K,8K) at v3 seq 5 from segment 1: the cut takes the loss extent alone, again over the fold's answer", p)
	}
}

// TestTheTornRecordsOwnCopyKeepsItsMarkerWhenItsReachIsEmpty holds the
// survivor rule over a reach that spans no byte: a truncate past every
// offset reaches nothing, yet its copy in a surviving home names a record
// the journal wrote, so the repair must see that copy — a rebuild that was
// never fed the torn placement keeps the marker the surviving copy stands
// for, and the teardown that never sees the copy deletes it instead. A
// removal carries no body, so the open's own verdicts cannot tear one —
// its header either verifies whole or attaches nothing — and the test
// drives the repair with the verdict the flow's reads cannot produce, over
// the planted second copy whose numbers say the record was written.
func TestTheTornRecordsOwnCopyKeepsItsMarkerWhenItsReachIsEmpty(t *testing.T) {
	xRaw := rec(4, func(rc *raw.Record) { // a truncate past every offset
		rc.FileID = tailFile().Bytes()
		rc.Seq = 9
		rc.Version = raw.Version{Counter: 9}
		rc.Offset = math.MaxInt64
		rc.Length = 0
	})
	seg1, _ := buildSealed(1, xRaw) // the surviving copy, named by segment 2's header
	seg2 := buildSeg(2, xRaw)       // the torn placement, same numbers
	x2At := int64(format.SegmentHeaderSize)
	copy(seg2, raw.SegmentHeader{Identity: journalID, ID: 2, Predecessor: 1}.Bytes())

	r := newRig(t)
	r.writeFormat()
	r.plantSeg(1, seg1)
	r.plantSeg(2, seg2)

	st := runTails(t, r)

	noTornReport(t, st)
	truncates := 0
	for _, ch := range st.Rebuilt.Index.Since(tailFile(), core.Version{}) {
		if ch.Kind == core.ChangeTruncate {
			truncates++
		}
	}
	if truncates != 1 {
		t.Fatalf("the open left %d truncate changes, want the one the surviving copy owns", truncates)
	}

	// The fabricated torn verdict: segment 2's copy of the truncate, a
	// record whose no-body shape cannot tear at the open's own reads — via
	// the repair's entry exactly as the flow reaches it after its verdict.
	rd := newReader(context.Background(), r.seam.FS(), r.clk, nil)
	tornSoFar := map[index.RecordAt]bool{}
	h := format.RecordHeader{
		Kind: format.KindTruncate, Segment: 2, File: tailFile(),
		Extent: core.Extent{Off: math.MaxInt64, Len: 0},
		Seq:    9, Version: core.Version{Counter: 9}, Tag: 7, Offset: x2At,
	}
	if err := repairTornPlacements(rd, &st, journalID, []format.RecordHeader{h}, tornSoFar); err != nil {
		t.Fatalf("the repair of a torn no-body truncate: %v", err)
	}

	truncates = 0
	for _, ch := range st.Rebuilt.Index.Since(tailFile(), core.Version{}) {
		if ch.Kind == core.ChangeTruncate {
			truncates++
		}
	}
	if truncates != 1 {
		t.Errorf("the repaired index keeps %d truncate changes, want the 1 the surviving copy owns: a copy with the torn record's numbers reaches the repair wherever its reach spans no byte", truncates)
	}
}

// countingFS wraps the rig's filesystem, counting the opens of one segment's
// file as they pass through — the repair's read cost answered by the door
// every read goes through, without answering anything on the store's behalf.
type countingFS struct {
	storage.FS
	name  string
	opens atomic.Int64
}

// Open counts the watched name's opens and passes the open through.
func (c *countingFS) Open(name string) (storage.File, error) {
	if name == c.name {
		c.opens.Add(1)
	}
	return c.FS.Open(name)
}

// TestTheRepairReadsEachSegmentOnceHoweverManyTornRecordsWereJudged holds
// the repair's read shape: a power loss tears a whole unsynced tail, so the
// records one segment's proof judged torn share one read of every attached
// segment — never one read per torn record. Two journals identical except
// one more torn record in the tail must open the OLDER segment's file the
// same number of times: a repair that re-reads pays a full walk of every
// attached segment per torn record, and the difference a second torn record
// adds is the whole walk again.
func TestTheRepairReadsEachSegmentOnceHoweverManyTornRecordsWereJudged(t *testing.T) {
	run := func(t *testing.T, rotTwo bool) int64 {
		t.Helper()
		// Segment 1: twenty-four synced writes of the tail's file over the
		// torn records' extent, the survivors every repair re-places; each
		// one's synced-through lies past the segment's own end, so the proof
		// never reaches the segment.
		surv := make([]raw.Record, 0, 24)
		for i := range 24 {
			surv = append(surv, rec(1, func(rc *raw.Record) {
				rc.Seq = uint64(i + 1)
				rc.Version = raw.Version{Counter: 200 + uint64(i)}
				rc.Offset = 0x4100
				rc.SyncedThrough = 1 << 28
				rc.Write = &raw.WriteBody{MTime: 1, Payload: bytes16()}
			}))
		}
		seg1 := buildSeg(1, surv...)

		// Segment 2, the tail: a holding write, then two writes the proof
		// may find torn — the first rotted in both runs, the second only in
		// the second run.
		tails := []raw.Record{
			tailRec(250, 0x100),            // holds wherever the tail lands
			writeRec(260, 260, 0x4100, 16), // the first torn write
			writeRec(270, 270, 0x4100, 16), // the second, written over the first
		}
		t1At := endAfter(tails[0])
		t2At := endAfter(tails[0], tails[1])
		seg2 := buildSeg(2, tails...)
		seg2[t1At+format.RecordHeaderSize+8+2] ^= 0xff
		if rotTwo {
			seg2[t2At+format.RecordHeaderSize+8+2] ^= 0xff
		}

		r := newRig(t)
		r.writeFormat()
		r.plantSeg(1, seg1)
		r.plantSeg(2, seg2)
		d := r.runDeps()
		c := &countingFS{FS: d.FS, name: format.SegmentName(1)}
		d.FS = c
		if _, err := Run(context.Background(), d, core.Version{}, journalID, nil); err != nil {
			t.Fatalf("Run over a tail of torn records: %v", err)
		}
		return c.opens.Load()
	}
	one := run(t, false)
	two := run(t, true)
	if d := two - one; d > 6 {
		t.Errorf("one more torn record added %d opens of the older segment, want at most 6: the repairs of one proof share one read of every attached segment", d)
	}
}
