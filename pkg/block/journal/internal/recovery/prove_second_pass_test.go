package recovery

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/format"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/index"
	"github.com/cubbit/dittofs/pkg/block/journal/journaltest/kit"
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
		t.Fatalf("Lookup of the tail's file at %d answered nothing, want the placement %+v: the torn record's exclusion restored nothing", off, want)
	}
	if got != want {
		t.Errorf("Lookup of the tail's file at %d got %+v, want %+v: excluding the torn record restores the placement of the record it overwrote", off, got, want)
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

// TestATornRecordOverASyncedWriteRestoresItsPlacement holds the torn rule's
// own claim: a torn record is treated as never written, so the index must
// come out exactly as it would had the rebuild never been fed the record —
// and the record it overwrote, whose piece the replay cut, holds the extent
// again. The overwrite runs in both shapes the journal writes them in: B
// over A in one segment, and B in the stream's newest segment over synced A
// sealed in the named one before it.
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

// TestTheRepairFoldsNoRecordTheCatalogueConfined holds the confinement's
// reach through the second pass: segment 1 is named and sealed, read by its
// catalogue, whose confined clone target C never feeds the first rebuild —
// the record the catalogue judged dropped stays out of a scan's own records
// — and the torn write T over C's file and extent must not put it back
// either: the second pass re-reads segment 1 by the same catalogue, which
// confines C again, so the rebuilt index holds C's neighbour G over the
// extent C would have cut. C's clone spec body is rotted where it sits,
// which is what confined it; the catalogue's count and the confined drop
// pin that the second pass took the same source the first did.
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
	// G's placement holds the extent T displaced — the confined C cut
	// nothing, for neither rebuild pass ever fed it.
	wantPlacement(t, st, overwrite, index.Placed{
		Extent:  core.Extent{Off: overwrite, Len: 16},
		Record:  index.RecordAt{Segment: 1, Offset: format.SegmentHeaderSize},
		Version: core.Version{Counter: 200},
		Seq:     100,
	})
}

// TestTheRepairReadsACataloguedSegmentTheWayTheRebuildDid holds the second
// pass's source rule: every segment is read the same way the first rebuild
// read it, or the two disagree at the records only the catalogue can name.
// Segment 1 is named and sealed, holding the write G whose record header
// rotted; the first rebuild indexed G from its catalogue entry, while the
// segment's header walk refuses the rotted header and answers nothing. The
// stream's newest segment holds the torn write T over G's whole extent, and
// the second pass — re-reading segment 1 by its catalogue again — feeds G
// the same way: a sealed durable write is not silently lost, and nothing is
// served stale from T's excluded placement.
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
	// G holds its extent: the second pass feeds the record the first
	// rebuild indexed, and its placement names the same segment and offset
	// the first pass's entry did.
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
// second pass: a record the feed withheld whole — here one whose seq the
// directory's failed list names, sealed with a synced segment before the torn
// tail — was never fed, so the second pass over the torn segment's stream may
// not re-place it either. Its fresh header reads healthy, so no scan's judged
// set ever finds it; only the withheld set does, and the index comes out
// holding nothing of what the force of the failed list kept out of it.
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

// TestTheRepairFoldsTheCopyTheOpenKeeps holds the second pass's placement
// rule against the repoint's: a record carried in two homes, one the open
// keeps and one its classing ends, was re-pointed to the surviving home
// when the first rebuild finished — and the second pass's fold keeps the
// answer the repoint gave, for its own classing reads the same settled
// fates. The fold's collapse keeps the greatest (segment, offset), so the
// piece stays with the kept home, never one whose emptying takes the bytes
// away.
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
	// repoint moved the piece there and the second pass's fold, reading the
	// same fates, did not hand it back to the ended home.
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
// loss cuts and the rebuild's fold answer in: a synced superseded loss cuts
// the write W over [0,4K) once the fold finishes, and the torn write T over
// the same extent never feeds the second pass — so the cut takes its extent
// over the second pass's own fold, and the open serves the same hole a
// rebuild that saw no torn record holds.
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
		t.Errorf("Lookup at 0 got %+v, want a miss: the synced superseded cut takes the extent over the second pass's fold", p)
	}
	p, ok := st.Rebuilt.Index.Lookup(file, 4*KiB)
	if !ok {
		t.Fatalf("Lookup at 4096 missed, want the cut to leave W's [4K,8K) run held")
	}
	if p.Extent != (core.Extent{Off: 4 * KiB, Len: 4 * KiB}) || p.Version != (core.Version{Counter: 3}) || p.Seq != 5 || p.Record != (index.RecordAt{Segment: 1, Offset: format.SegmentHeaderSize}) {
		t.Errorf("the kept run got %+v, want W's [4K,8K) at v3 seq 5 from segment 1: the cut takes the loss extent alone, over the second pass's answer", p)
	}
}

// TestTheRepairReportsAReappliedLossOnce holds the marker dedup over a loss
// that leaves one: a synced corrupt loss drops the extent of the write it
// names and leaves a marker saying so, and the second pass's fold over the
// torn record's exclusion applies the cut over its own answer — the one
// change Since owes the caller, never one per application.
func TestTheRepairReportsAReappliedLossOnce(t *testing.T) {
	const KiB = 1024
	file := tailFile()
	w := writeRec(5, 3, 0, 8*KiB)                      // W, v3 seq5 over [0,8K)
	l := plantLossRec(file, 5, 8, 4, 0, 4*KiB, 1|1<<8) // a synced corrupt loss dropping seq5 over [0,4K) at v4, leaving its marker
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
	if p, ok := st.Rebuilt.Index.Lookup(file, 0); ok {
		t.Errorf("Lookup at 0 got %+v, want a miss: the synced corrupt cut takes the extent over the second pass's fold", p)
	}
	if got := len(st.Rebuilt.Index.Since(file, core.Version{})); got != 1 {
		t.Errorf("Since: got %d changes, want 1: the loss cut over the second pass's fold reports once", got)
	}
}

// TestTheSecondPassReadsEverySegmentOnceHoweverManyStreamsTore holds the
// second pass's read shape: a power loss tears the unsynced tail of the
// newest segment in EVERY stream, so the torn placements a whole open
// collects — one stream's or a dozen — drive ONE second rebuild pass, never
// one per proved segment. Two journals identical except one more TORN
// STREAM must open the first journal's oldest segment's file the same
// number of times: a rebuild run per proved segment pays a full walk of
// every attached segment for each, and the difference a second stream adds
// is the whole walk again.
func TestTheSecondPassReadsEverySegmentOnceHoweverManyStreamsTore(t *testing.T) {
	run := func(t *testing.T, twoStreams bool) int {
		t.Helper()
		// Stream one: segment 1, twenty-four synced writes of the tail's
		// file — the survivors of any repair — whose synced-through lies
		// past the segment's own end, so the proof never reaches the
		// segment; then segment 2, the stream's newest tail, holding a
		// holding record and one the proof finds torn.
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

		a, b, bAt := tailSeg()
		seg2 := buildSeg(2, a, b)
		seg2[bAt+format.RecordHeaderSize+8+2] ^= 0xff // B's payload, torn in both views

		r := newRig(t)
		r.writeFormat()
		r.plantSeg(1, seg1)
		r.plantSeg(2, seg2)

		// Stream two, planted only where the leg asks for it: a sealed,
		// named segment 3 — one synced survivor, proving nothing on its
		// own — and its newest tail segment 4, whose one record is torn.
		if twoStreams {
			seg3, _ := buildSealed(3, syncedRec(300, 0x9100, endAfter(tailRec(300, 0x9100))))
			x := tailRec(301, 0xa100)
			seg4 := buildSeg(4, x)
			xAt := int64(format.SegmentHeaderSize)
			seg4[xAt+format.RecordHeaderSize+8+2] ^= 0xff
			copy(seg4, raw.SegmentHeader{Identity: journalID, ID: 4, Predecessor: 3}.Bytes())
			r.plantSeg(3, seg3)
			r.plantSeg(4, seg4)
		}

		before := r.seam.CallsFor(kit.OpOpen, format.SegmentName(1))
		if _, err := Run(context.Background(), r.runDeps(), core.Version{}, journalID, nil); err != nil {
			t.Fatalf("Run over a torn tail: %v", err)
		}
		return r.seam.CallsFor(kit.OpOpen, format.SegmentName(1)) - before
	}
	one := run(t, false)
	two := run(t, true)
	if d := two - one; d != 0 {
		t.Errorf("one more torn stream added %d opens of the oldest segment, want 0: the second pass runs once per open, however many streams tore", d)
	}
}

// TestTheOpenOverATornTailComesOutExactlyTheOpenWithoutIt holds the second
// pass's whole claim at the recovery level, where no index seam has to
// stand in for it: an open whose newest tail holds a torn record — the
// second pass excluding it — answers every index question the open over
// the same directory without the torn record's bytes answers. The placement,
// the entry count and the change list of the tail's file must be equal
// between the two opens: that equality is the meaning of the second pass's
// "a rebuild never fed the record", compared against a real rebuild that
// never saw it.
func TestTheOpenOverATornTailComesOutExactlyTheOpenWithoutIt(t *testing.T) {
	const overwrite = 0x4100
	openBoth := func(t *testing.T, withT, withoutT []byte, namedSeg2 bool) (State, State) {
		t.Helper()
		torn := func() State {
			r := newRig(t)
			r.writeFormat()
			if namedSeg2 {
				seg1, _ := buildSealed(1, syncedRec(200, overwrite, endAfter(tailRec(200, overwrite))))
				r.plantSeg(1, seg1)
			}
			r.plantSeg(2, withT)
			return runTails(t, r)
		}()
		plain := func() State {
			r := newRig(t)
			r.writeFormat()
			if namedSeg2 {
				seg1, _ := buildSealed(1, syncedRec(200, overwrite, endAfter(tailRec(200, overwrite))))
				r.plantSeg(1, seg1)
			}
			r.plantSeg(2, withoutT)
			return runTails(t, r)
		}()
		return torn, plain
	}
	compare := func(t *testing.T, torn, plain State) {
		t.Helper()
		pt, okT := torn.Rebuilt.Index.Lookup(tailFile(), overwrite)
		pp, okP := plain.Rebuilt.Index.Lookup(tailFile(), overwrite)
		if !okT || !okP {
			t.Fatalf("Lookup at %d answered torn=%v(%+v) plain=%v(%+v), want both to answer: the torn record's exclusion answers exactly what its absence does", overwrite, okT, pt, okP, pp)
		}
		if pt != pp {
			t.Errorf("the placement over the torn record's extent got %+v over the torn open, %+v over the open without it, want them equal", pt, pp)
		}
		if gotT, gotP := planted(torn), planted(plain); gotT != gotP {
			t.Errorf("the index holds %d entries over the torn open, %d over the open without it, want them equal", gotT, gotP)
		}
		chT, chP := torn.Rebuilt.Index.Since(tailFile(), core.Version{}), plain.Rebuilt.Index.Since(tailFile(), core.Version{})
		if !reflect.DeepEqual(chT, chP) {
			t.Errorf("Since over the tail's file got %+v over the torn open, %+v over the open without it, want them equal", chT, chP)
		}
	}

	t.Run("the torn record in the segment that holds what it overwrote", func(t *testing.T) {
		a := syncedRec(200, overwrite, endAfter(tailRec(200, overwrite)))
		withT := buildSeg(2, a, tailRec(201, overwrite))
		withT[endAfter(tailRec(200, overwrite))+format.RecordHeaderSize+8+2] ^= 0xff
		torn, plain := openBoth(t, withT, buildSeg(2, a), false)
		compare(t, torn, plain)
	})
	t.Run("the torn record in the stream's newest segment", func(t *testing.T) {
		withT := buildSeg(2, tailRec(201, overwrite))
		copy(withT, raw.SegmentHeader{Identity: journalID, ID: 2, Predecessor: 1}.Bytes())
		withT[format.SegmentHeaderSize+format.RecordHeaderSize+8+2] ^= 0xff
		withoutT := raw.SegmentHeader{Identity: journalID, ID: 2, Predecessor: 1}.Bytes()
		torn, plain := openBoth(t, withT, withoutT, true)
		compare(t, torn, plain)
	})
}

// TestTheSecondPassPlacesTheCopyTheSettledFatesKeep holds the point of
// running the second pass after every fate is settled: a proved segment
// that ends through a LOST record can hold the greatest copy of a record
// the directory carries twice, and the second pass's repoint — reading the
// settled fates, where a fresh classing would answer the pre-proof
// verdicts — moves the piece to the copy a sealed, named segment holds,
// never leaves it in the home the proof ends and its emptying would take
// away.
func TestTheSecondPassPlacesTheCopyTheSettledFatesKeep(t *testing.T) {
	const overwrite = 0x4100
	xRaw := rec(1, func(rc *raw.Record) {
		rc.Seq = 9
		rc.Version = raw.Version{Counter: 201}
		rc.Offset = overwrite
		rc.Write = &raw.WriteBody{MTime: 1, Payload: bytes16()}
	})
	seg1, _ := buildSealed(1, xRaw) // X's kept home, named by segment 2's header

	// Segment 3, the newest: X again at its greater home, then T, a torn
	// write over X's extent, then L, a record whole in the cache alone —
	// the device lost it, and a lost record ends the segment whatever its
	// other verdicts.
	x3, tornT, lostL := xRaw, tailRec(250, overwrite), tailRec(251, 0x8100)
	seg3 := buildSeg(3, x3, tornT, lostL)
	tAt := endAfter(x3)
	lAt := endAfter(x3, tornT)
	seg3[tAt+format.RecordHeaderSize+8+2] ^= 0xff // T's payload, torn in both views
	goodL := slices.Clone(seg3[lAt:])
	seg3[lAt+format.RecordHeaderSize+8+2] ^= 0xff // L's payload, durable garbage

	r := newRig(t)
	r.writeFormat()
	r.plantSeg(1, seg1)
	r.plantSeg(2, raw.SegmentHeader{Identity: journalID, ID: 2, Predecessor: 1}.Bytes())
	r.plantSeg(3, seg3)
	name := format.SegmentName(3)
	r.writeLiveOnly(name, lAt, goodL) // L whole in the cache alone
	r.dropSyncs(name)

	st := runTails(t, r)

	wantReappends(t, st, reappendOf(t, 3, lAt, goodL))
	if len(st.Proof.FailedWindows) != 0 {
		t.Errorf("FailedWindows got %v, want none: the segment's syncs reported success", st.Proof.FailedWindows)
	}
	if len(st.Tails.Damaged) != 0 {
		t.Errorf("Damaged got %v, want none: a handed-over record and a torn one are never damage", st.Tails.Damaged)
	}
	// The lost record ends segment 3, so its copy of X is a home the open
	// takes away: the placement answers from segment 1, the sealed home.
	wantPlacement(t, st, overwrite, index.Placed{
		Extent:  core.Extent{Off: overwrite, Len: 16},
		Record:  index.RecordAt{Segment: 1, Offset: format.SegmentHeaderSize},
		Version: core.Version{Counter: 201},
		Seq:     9,
	})
	if got := planted(st); got != 2 {
		t.Errorf("the index holds %d entries, want 2: X at its surviving home and L, whose extent the re-append claims", got)
	}
}

// TestAProofUnsealedSegmentCarriesAnUnsealedClassingsVerdicts holds the
// reclassification: a segment whose marker only the cache holds loses its
// seal on the device, and from that moment it must answer what the same
// segment without a marker answers — a loss record whose body rotted in
// both views is the torn tail at its own start, never a corrupt drop, and
// nothing about the segment reports damaged. Both legs plant the same
// bytes; one carries a marker only the live view serves, one carries none.
func TestAProofUnsealedSegmentCarriesAnUnsealedClassingsVerdicts(t *testing.T) {
	a := writeRec(1, 200, 0x4100, 16)                                 // A, verifying and fed
	loss := plantLossRec(tailFile(), 11, 12, 201, 0x4100, 16, 1|1<<8) // a loss record, its body rotted below
	base := buildSeg(2, a, loss)
	lossAt := endAfter(a)
	base[lossAt+format.RecordHeaderSize] ^= 0xff // the loss record's body, rotted in both views

	verdicts := func(t *testing.T, st State) {
		t.Helper()
		wantFate(t, st, 2, fateTornTail, lossAt)
		if len(st.Tails.Corrupt) != 0 {
			t.Errorf("Corrupt got %+v, want none: the rotted loss body answers a torn tail at its own start, never a corrupt drop", st.Tails.Corrupt)
		}
		if len(st.Tails.Damaged) != 0 {
			t.Errorf("Damaged got %v, want none: the verdicts of the never-sealed segment report nothing damaged", st.Tails.Damaged)
		}
		if len(st.Proof.Reappends) != 0 || len(st.Proof.FailedWindows) != 0 {
			t.Errorf("Reappends got %+v, FailedWindows got %v, want none: nothing was lost or failed", st.Proof.Reappends, st.Proof.FailedWindows)
		}
	}

	t.Run("no marker at all", func(t *testing.T) {
		r := newRig(t)
		r.writeFormat()
		r.plantSeg(2, base)

		verdicts(t, runTails(t, r))
	})
	t.Run("a marker only the cache holds", func(t *testing.T) {
		name := format.SegmentName(2)
		markerOff := int64(len(base))
		marker := raw.SealMarker{Identity: journalID, Segment: 2, Offset: uint64(markerOff)}.Bytes()
		durable := append(slices.Clone(base), marker...)
		durable[markerOff+format.SealMarkerSize-1] ^= 0xff // the marker's checksum, rotted on the device

		r := newRig(t)
		r.writeFormat()
		r.plantSeg(2, durable)
		r.writeLiveOnly(name, markerOff, marker)
		r.dropSyncs(name)

		verdicts(t, runTails(t, r))
	})
}

// TestAFateNoScanAnswersIsAnInternalInconsistencyNeverCorruption holds the
// two unanswerable-fate paths' answer: the classing writes one fate per
// scan over the same segment list the rebuild scanned, so a fate naming a
// segment no scan answers is a disagreement between the phases of one run —
// an internal break — never a corruption verdict that would send an
// operator to recovery of the directory's bytes.
func TestAFateNoScanAnswersIsAnInternalInconsistencyNeverCorruption(t *testing.T) {
	neverCorrupt := func(t *testing.T, err error) {
		t.Helper()
		if err == nil {
			t.Fatalf("the proof answered no error, want the internal break named")
		}
		if errors.Is(err, core.ErrCorrupt) {
			t.Errorf("a fate no scan answers got %v, want an internal error: the checksum verdict sends an operator to recovery of bytes the recovery never read", err)
		}
		var ure *core.UnreadableRangesError
		if errors.As(err, &ure) {
			t.Errorf("a fate no scan answers got %v, want an internal error: no read refused, no range was collected", err)
		}
		if !strings.Contains(err.Error(), "recovery: segment 42") {
			t.Errorf("the error got %q, want it to name the recovery's own break and the segment it could not answer for", err)
		}
	}

	t.Run("the marker flow", func(t *testing.T) {
		r := newRig(t)
		rd := newReader(context.Background(), r.seam.FS(), r.clk, nil)
		st := State{Tails: tails{Fates: []segmentFate{{ID: 42, Kind: fateSealToProve}}}}
		neverCorrupt(t, runTailProof(context.Background(), r.runDeps(), rd, &st, journalID, core.Version{}, nil))
	})
	t.Run("the tail flow", func(t *testing.T) {
		r := newRig(t)
		rd := newReader(context.Background(), r.seam.FS(), r.clk, nil)
		st := State{Tails: tails{Fates: []segmentFate{{ID: 42, Kind: fateIntact}}}}
		neverCorrupt(t, runTailProof(context.Background(), r.runDeps(), rd, &st, journalID, core.Version{}, nil))
	})
}
