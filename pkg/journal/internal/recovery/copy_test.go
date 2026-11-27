package recovery

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"syscall"
	"testing"

	"github.com/cubbit/dittofs/pkg/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/journal/internal/format"
	"github.com/cubbit/dittofs/pkg/journal/internal/index"
	"github.com/cubbit/dittofs/pkg/journal/internal/segment"
	"github.com/cubbit/dittofs/pkg/journal/journaltest/kit"
)

// reserveDeps plants a whole cap-sized record-less spare under each id, the
// way an untaken spare a crash left behind stands, and returns the deps whose
// spare targets class exactly the planted count into the repack reserve: the
// class the re-appending open draws on, the spares past that count taking the
// content class the fill order gives them.
func (r *rig) reserveDeps(ids ...core.SegmentID) Deps {
	r.t.Helper()
	for _, id := range ids {
		r.write(format.SegmentName(id), make([]byte, testCap))
	}
	d := r.runDeps()
	d.SpareTargets = segment.Targets{Reserve: len(ids)}
	return d
}

// runReserve opens the rig's directory with the reserve spares planted, the
// answer the re-append checks read.
func runReserve(t *testing.T, r *rig, ids ...core.SegmentID) State {
	t.Helper()
	st, err := Run(context.Background(), r.reserveDeps(ids...), core.Version{}, journalID, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	return st
}

// runTailsReserve runs the open the tail fixtures read, with one repack
// reserve spare planted beside the fixture: a tail those fixtures hand to
// the re-append has the spare it draws, and the fixture's own assertions —
// the proof's fates, re-append sets and per-name seam counts — read the
// same, the copy writing only its own target's file.
const copySpare = core.SegmentID(9)

func runTailsReserve(t *testing.T, r *rig) State {
	t.Helper()
	return runReserve(t, r, copySpare)
}

// deviceLostB plants the tail the device half-lost: A durable and naming its
// own sync point, B whole in the cache a dead process held it in and rotted
// where the device alone reads it, the open's syncs dropped so the proof's
// re-read answers the device's bytes alone. It returns the rig over that
// directory, B's whole bytes and the offset the good record starts at.
func deviceLostB(t *testing.T) (r *rig, good []byte, bAt int64) {
	t.Helper()
	a, b, bAt := tailSeg()
	full := buildSeg(2, a, b)
	name := format.SegmentName(2)
	damaged := slices.Clone(full)
	damaged[bAt+format.RecordHeaderSize+8+2] ^= 0xff // B's payload, rotted where the device alone reads it
	r = newRig(t)
	r.writeFormat()
	r.plantSeg(2, damaged)
	r.writeLiveOnly(name, bAt, full[bAt:]) // the good B, whole in the cache alone
	r.dropSyncs(name)                      // the open's sync makes nothing durable
	return r, full[bAt:], bAt
}

// wantTheCopy holds the two facts every read-back check asserts together:
// the index serves the record's extent from the copy's placement, and the
// target that placement names holds the record's whole bytes on the device,
// verifying under the target's own identity and offset while keeping the
// record's original numbers and kind, file, extent and tag.
func wantTheCopy(t *testing.T, r *rig, st State, off uint64, want tailRecord, at int64, target core.SegmentID) {
	t.Helper()
	p, ok := st.Rebuilt.Index.Lookup(tailFile(), int64(off))
	if !ok {
		t.Fatalf("Lookup over the re-appended record's extent at file offset %d missed, want the copy's placement", off)
	}
	if got := p.Record; got.Segment != target || int64(got.Offset) != at {
		t.Errorf("Lookup over the extent at file offset %d names the record at %+v, want the re-append's placement in segment %d at %d", off, got, target, at)
	}
	if p.Seq != want.Header.Seq || p.Version != want.Header.Version {
		t.Errorf("the served piece carries seq %d version %v, want the record's own %d and %v", p.Seq, p.Version, want.Header.Seq, want.Header.Version)
	}
	h := rereadCopy(t, r, target, at, want)
	if h.Seq != want.Header.Seq || h.Version != want.Header.Version {
		t.Errorf("the copy keeps seq %d version %v, want the record's own %d and %v: a re-append never renumbers", h.Seq, h.Version, want.Header.Seq, want.Header.Version)
	}
	if h.File != want.Header.File || h.Extent != want.Header.Extent || h.Tag != want.Header.Tag || h.Kind != want.Header.Kind {
		t.Errorf("the copy carries file %v extent %+v tag %d kind %d, want the record's own file %v extent %+v tag %d kind %d", h.File, h.Extent, h.Tag, h.Kind, want.Header.File, want.Header.Extent, want.Header.Tag, want.Header.Kind)
	}
}

// rereadCopy reads the record the target holds at at back from the rig's
// directory, whole and verified, the bytes a later open would scan: the sealed
// copy's own placement and the numbers it kept.
func rereadCopy(t *testing.T, r *rig, target core.SegmentID, at int64, want tailRecord) format.RecordHeader {
	t.Helper()
	b := r.fileBytes(format.SegmentName(target))
	if int64(len(b)) < at+want.Header.Size() {
		t.Fatalf("the target %d holds %d bytes, want the copy of seq %d whole at %d", target, len(b), want.Header.Seq, at)
	}
	h, derr := format.DecodeRecordHeader(b[at:at+format.RecordHeaderSize], journalID, target, at)
	if derr != nil {
		t.Fatalf("decoding the copy of seq %d in segment %d at %d: %v", want.Header.Seq, target, at, derr)
	}
	if h.Size() != want.Header.Size() {
		t.Fatalf("the copy of seq %d grew: %d bytes, want the record's own %d", want.Header.Seq, h.Size(), want.Header.Size())
	}
	if _, verr := format.VerifyRecordBody(b[at+format.RecordHeaderSize:at+want.Header.Size()], journalID, h); verr != nil {
		t.Fatalf("the copy of seq %d does not verify: %v", want.Header.Seq, verr)
	}
	return h
}

// TestDeviceLostTailReappendsFromItsFirstCopy holds R49's read-back half: a
// record the cache serves whole and the device does not hold is re-appended
// from the copy its first read verified — never left at the placement whose
// bytes the device dropped, which a re-read from the device would answer
// lost — keeping its numbers, and its old segment ends where the tail's
// proof cut it (R51).
func TestDeviceLostTailReappendsFromItsFirstCopy(t *testing.T) {
	r, good, bAt := deviceLostB(t)
	want := reappendOf(t, 2, bAt, good)
	spare := core.SegmentID(9)

	st := runReserve(t, r, spare)

	wantTheCopy(t, r, st, 0x8100, want, format.SegmentHeaderSize, spare)
	wantFate(t, st, 2, fateEnded, bAt+want.Header.Size())
	if len(st.Spares.Adopted) != 0 {
		t.Errorf("spares: the re-append left adoptions %v, want none: the spare it drew is a data segment now, not a pool spare", st.Spares.Adopted)
	}
}

// TestReappendIsIdempotentAcrossOpens opens the device-lost directory
// twice: the first open re-appends the lost B into a sealed spare — the
// crash shape between the copy's seal and its source's unlink, for the open
// that seals keeps the source on disk — and the second open meets the same
// loss over the same bytes. A copy of the record's own numbers already
// verifying in a sealed segment is the answer the re-append exists to
// write, so the second open draws nothing for it: the spare planted for it
// stays a pool spare, no second target is drawn or sealed, the index
// serves B off the sealed copy — and where the second open
// has no spare at all, it serves instead of refusing.
func TestReappendIsIdempotentAcrossOpens(t *testing.T) {
	t.Run("the second open draws nothing", func(t *testing.T) {
		r, good, bAt := deviceLostB(t)
		want := reappendOf(t, 2, bAt, good)
		first, next := core.SegmentID(9), core.SegmentID(10)
		st := runReserve(t, r, first) // the first open seals B's copy into 9
		wantTheCopy(t, r, st, 0x8100, want, format.SegmentHeaderSize, first)
		nameNext := format.SegmentName(next)
		d := r.reserveDeps(next) // one fresh reserve spare planted for the second open

		st, err := Run(context.Background(), d, core.Version{}, journalID, nil)

		if err != nil {
			t.Fatalf("the second open over the same directory answered err %v, want it to serve off the sealed copy", err)
		}
		if got, wantN := len(st.Proof.Reappends), 1; got != wantN {
			t.Fatalf("Reappends got %d records, want %d: the tail's proof still hands the lost B over — the skip is the phase's, not the proof's", got, wantN)
		}
		if got, want := st.Spares.Adopted, []AdoptedSpare{{ID: next, Class: segment.Reserve}}; !slices.Equal(got, want) {
			t.Errorf("spares: the second open left adoptions %v, want %v: it drew nothing, so the planted spare stays a pool spare", got, want)
		}
		if got, wantAt := st.Rebuilt.Index.Lookup(tailFile(), 0x8100); !wantAt || got.Record.Segment != first || int64(got.Record.Offset) != format.SegmentHeaderSize {
			t.Errorf("the second open serves B at %+v (found %v), want the sealed copy of segment %d at %d", got.Record, wantAt, first, format.SegmentHeaderSize)
		}
		if b := r.fileBytes(nameNext); !allZero(b) {
			t.Errorf("the fresh spare holds %d non-zero bytes, want none: no second open drew and sealed a target any copy needed", countNonZero(b))
		}
	})

	t.Run("the no-spare second open serves instead of refusing", func(t *testing.T) {
		r, good, bAt := deviceLostB(t)
		want := reappendOf(t, 2, bAt, good)
		first := core.SegmentID(9)
		st := runReserve(t, r, first) // the first open seals B's copy into 9
		wantTheCopy(t, r, st, 0x8100, want, format.SegmentHeaderSize, first)
		before := r.fileBytes(format.SegmentName(first))

		st, err := Run(context.Background(), r.runDeps(), core.Version{}, journalID, nil)

		if err != nil {
			t.Fatalf("the second open with no spare answered err %v, want it to serve off the sealed copy instead of refusing for a spare the sealed copy made redundant", err)
		}
		if len(st.Proof.Reappends) != 1 {
			t.Fatalf("Reappends got %+v, want B alone again: the skip is the phase's answer to it", st.Proof.Reappends)
		}
		if got, wantAt := st.Rebuilt.Index.Lookup(tailFile(), 0x8100); !wantAt || got.Record.Segment != first || int64(got.Record.Offset) != format.SegmentHeaderSize {
			t.Errorf("the second open serves B at %+v (found %v), want the sealed copy of segment %d at %d", got.Record, wantAt, first, format.SegmentHeaderSize)
		}
		if after := r.fileBytes(format.SegmentName(first)); !slices.Equal(after, before) {
			t.Errorf("the second open changed the sealed copy's bytes, want them unchanged: it drew and sealed nothing")
		}
	})
}

// TestFailedTailSyncCopiesEverything holds the failed window's half of R49:
// a tail whose proof sync fails hands every record the first read verified
// over — the device is never asked for them — so each is re-appended with
// its numbers, and the segment stays what the proof named it: a failed
// segment ended at its last holding record, never sealed.
func TestFailedTailSyncCopiesEverything(t *testing.T) {
	endA := endAfter(tailRec(200, 0x4100))
	a := syncedRec(200, 0x4100, endA)
	b := syncedRec(201, 0x8100, endA)
	c := syncedRec(202, 0xc100, endA)
	full := buildSeg(2, a, b, c)
	name := format.SegmentName(2)
	r := newRig(t)
	r.writeFormat()
	r.plantSeg(2, full[:endA]) // A durable; B and C whole in the cache alone
	r.writeLiveOnly(name, endA, full[endA:])
	r.failSyncs(name, 1) // the proof's one sync fails: the whole tail is a failed window

	spare := core.SegmentID(9)
	st := runReserve(t, r, spare)

	if !slices.Equal(st.Proof.FailedWindows, []core.SegmentID{2}) {
		t.Fatalf("FailedWindows got %v, want [2]: the window the sync failed is what hands the tail over", st.Proof.FailedWindows)
	}
	if len(st.Proof.Reappends) != 2 {
		t.Fatalf("Reappends got %+v, want the window's two records, every record the first read verified", st.Proof.Reappends)
	}
	wantB := reappendOf(t, 2, endA, full[endA:endA+2*tailLen])
	wantC := reappendOf(t, 2, endA+tailLen, full[endA+tailLen:])
	wantTheCopy(t, r, st, 0x8100, wantB, format.SegmentHeaderSize, spare)
	wantTheCopy(t, r, st, 0xc100, wantC, format.SegmentHeaderSize+tailLen, spare)
	// A failed window's segment is ended — truncated without a marker by the
	// later cutting phase, never sealed — at the end of its last record.
	wantFate(t, st, 2, fateEnded, int64(len(full)))
}

// TestReappendTargetSyncFailureRetriesOnce holds D4's retry half: a target
// whose sync fails is a failed segment — nothing of it reached the device —
// and the same records go once more into another reserve spare; a second
// refusal in a row fails the open with that error, drawing no third spare and
// unlinking no source before its copy.
func TestReappendTargetSyncFailureRetriesOnce(t *testing.T) {
	t.Run("the retry lands in a second spare", func(t *testing.T) {
		r, good, bAt := deviceLostB(t)
		want := reappendOf(t, 2, bAt, good)
		first, second := core.SegmentID(8), core.SegmentID(9)
		d := r.reserveDeps(first, second)
		r.failSyncs(format.SegmentName(first), 0) // every sync of the first target fails

		st, err := Run(context.Background(), d, core.Version{}, journalID, nil)
		if err != nil {
			t.Fatalf("Run over a target whose first sync fails: %v", err)
		}

		wantTheCopy(t, r, st, 0x8100, want, format.SegmentHeaderSize, second)
		if got := r.fileBytes(format.SegmentName(first)); len(got) == 0 || !allZero(got) {
			t.Errorf("the failed target's durable bytes hold %d non-zero bytes, want none: nothing a failed sync carried reached the device", countNonZero(got))
		}
		if len(st.Spares.Adopted) != 0 {
			t.Errorf("spares: the two targets drawn left adoptions %v, want none: neither is a pool spare any more", st.Spares.Adopted)
		}
	})
	t.Run("the second refusal fails the open and takes no third spare", func(t *testing.T) {
		r, _, _ := deviceLostB(t)
		first, second, third := core.SegmentID(8), core.SegmentID(9), core.SegmentID(10)
		d := r.reserveDeps(first, second, third)
		r.failSyncs(format.SegmentName(first), 0)
		r.failSyncs(format.SegmentName(second), 0)
		before := r.fileBytes(format.SegmentName(2))
		writes := r.seam.CallsFor(kit.OpWrite, format.SegmentName(third))

		st, err := Run(context.Background(), d, core.Version{}, journalID, nil)
		if err == nil {
			t.Fatalf("Run over two failing targets got no error, want the second refusal to fail the open")
		}
		if !errors.Is(err, syscall.EIO) {
			t.Errorf("the refused open answers %v, want the sync's own EIO inside it", err)
		}
		if !reflect.DeepEqual(st, State{}) {
			t.Errorf("the refused open returned state %+v, want the zero State: a refusal serves nothing", st)
		}
		if got, want := r.seam.CallsFor(kit.OpWrite, format.SegmentName(third)), writes; got != want {
			t.Errorf("the third spare was written to %d times, want %d: no third spare is taken after two refusals", got, want)
		}
		if after := r.fileBytes(format.SegmentName(2)); !slices.Equal(after, before) {
			t.Errorf("the refused open changed the source's bytes, got %x, want %x: no source is touched before its copy", after, before)
		}
		if !slices.Contains(r.listing(), format.SegmentName(2)) {
			t.Errorf("the source's file is gone from %v, want it left in place", r.listing())
		}
	})
}

// TestAFailedReappendTargetTakesAnEndedFate holds the failed target's second
// half: the first target, whose sync failed, is a segment now — never a pool
// spare, for the phase removed its adoption — and a segment the open keeps
// only when a fate names it: the copier ended it in the seal writer's own
// throwaway ledger, which no later phase reads, so the phase ends it in the
// rebuilt fates too, or nothing seeds it, nothing ends it and no phase unlinks
// it. The ended fate is what the assembly seeds — Ended, a segment the
// emptying takes — and the copy itself lands in the second spare as the
// retry's own test reads.
func TestAFailedReappendTargetTakesAnEndedFate(t *testing.T) {
	r, good, bAt := deviceLostB(t)
	want := reappendOf(t, 2, bAt, good)
	first, second := core.SegmentID(8), core.SegmentID(9)
	d := r.reserveDeps(first, second)
	r.failSyncs(format.SegmentName(first), 0) // every sync of the first target fails

	st, err := Run(context.Background(), d, core.Version{}, journalID, nil)
	if err != nil {
		t.Fatalf("Run over a target whose first sync fails: %v", err)
	}

	wantTheCopy(t, r, st, 0x8100, want, format.SegmentHeaderSize, second)
	wantFate(t, st, first, fateEnded, 0)
	if state, serr := st.Assembled.Set.State(first); serr != nil || state != segment.Ended {
		t.Errorf("the assembly seeded the failed target %v (err %v), want %v: its ended fate is what the emptying reads", state, serr, segment.Ended)
	}
	for _, a := range st.Spares.Adopted {
		if a.ID == first {
			t.Errorf("spares: the failed target %d is among the adoptions %+v, want it a data segment every phase reads by its fate", first, st.Spares.Adopted)
		}
	}
}

// TestOpenAtMaximumStillReappends holds the reserve-only answer: the open's
// re-appends draw on the repack reserve — never on a share's limit, never a
// headroom or content spare, and charging no share a byte — so a journal
// whose every other spare class is spent before the crash still opens and
// serves the record the device lost from its re-append. The phase drawing
// the headroom or the content spare instead fails the adoption assertion
// below. The reserve spare is the lowest id planted, as the adoption order
// gives the lowest ids to the reserve, so a re-append that simply took the
// lowest id would pass here: the order's own tests in the segment package
// pin which id the reserve gets.
func TestOpenAtMaximumStillReappends(t *testing.T) {
	r, good, bAt := deviceLostB(t)
	want := reappendOf(t, 2, bAt, good)
	// One spare for each class in the fill order — the repack reserve, then
	// the headroom, then content — so the class each remaining adoption
	// names pins the one the re-append drew from.
	reserve, head, content := core.SegmentID(3), core.SegmentID(4), core.SegmentID(5)
	d := r.reserveDeps(reserve, head, content)
	d.SpareTargets = segment.Targets{Headroom: 1, Reserve: 1} // the first adopted spare takes the reserve, the second the headroom, the third the content class

	st, err := Run(context.Background(), d, core.Version{}, journalID, nil)
	if err != nil {
		t.Fatalf("Run at the journal's maximum: %v", err)
	}

	wantTheCopy(t, r, st, 0x8100, want, format.SegmentHeaderSize, reserve)
	if got, wantAd := st.Spares.Adopted, []AdoptedSpare{
		{ID: head, Class: segment.Headroom}, {ID: content, Class: segment.Content},
	}; !slices.Equal(got, wantAd) {
		t.Errorf("spares: the re-append left adoptions %v, want %v: it draws the repack reserve alone, so the headroom and content spares both survive it", got, wantAd)
	}
	// No share gained the record's bytes: the figures the feed charged — A's
	// and B's whole slots on their source, which holds them below its cut
	// until the emptying unlinks it — are the figures the assembly seeded,
	// the copy charging nothing to the tag it serves.
	if got, want := st.Assembled.Set.ShareBytes(core.ShareTag(7)), 2*slotLen(format.KindWrite, 16); got != want {
		t.Errorf("the assembly charged tag 7 %d bytes, want %d: the record's own slots alone, the re-append charging no share", got, want)
	}
}

// TestFailedListRecordsNeverReappended holds the failed list's boundary: a
// record whose sequence number the directory's failed list names is withheld
// whole at the feed — neither proven nor re-appended, its fate its segment's
// own — while the records around it in the same tail are proven and
// re-appended as usual: the phase's input holds no withheld record.
func TestFailedListRecordsNeverReappended(t *testing.T) {
	endA := endAfter(tailRec(200, 0x4100))
	endW := endA + tailLen
	a := syncedRec(200, 0x4100, endA)
	// W, the withheld record: seq 100, on the failed list, its payload rotted
	// where every read takes it — a first read of it would judge it torn,
	// which the withholding stands above.
	w := tailRec(201, 0x8100)
	w.Seq = 100
	// C, a lost record of its own number beside it, over another extent.
	c := tailRec(202, 0xc100)
	c.Seq = 101
	full := buildSeg(2, a, w, c)
	name := format.SegmentName(2)
	durable := slices.Clone(full)
	durable[endW+format.RecordHeaderSize+8+2] ^= 0xff // W's payload, rotted everywhere
	r := newRig(t)
	r.writeFormat()
	r.plantSeg(2, durable[:endW])            // A and the rotted W durable; C past the device's end
	r.writeLiveOnly(name, endW, full[endW:]) // the good C, whole in the cache alone
	r.dropSyncs(name)

	spare := core.SegmentID(9)
	d := r.reserveDeps(spare)
	d.Failed = failedWords([]core.Seq{100})
	st, err := Run(context.Background(), d, core.Version{}, journalID, nil)
	if err != nil {
		t.Fatalf("Run over a withheld record's tail: %v", err)
	}

	// The feed withheld W whole: its placement joined the withheld set, its
	// number the withheld fold, and it is judged and re-appended nowhere.
	if !st.Kinds.WithheldAt[index.RecordAt{Segment: 2, Offset: uint32(endA)}] {
		t.Errorf("the withheld set holds nothing at segment 2's offset %d, want the failed list's record there", endA)
	}
	if st.Kinds.Withheld.Seq < 100 {
		t.Errorf("the withheld numbers' sequence is %d, want 100 folded in", st.Kinds.Withheld.Seq)
	}
	if len(st.Proof.Reappends) != 1 || st.Proof.Reappends[0].Segment != 2 || st.Proof.Reappends[0].At != endW {
		t.Fatalf("Reappends got %+v, want C alone of segment 2 at %d: the withheld W never reaches the re-append", st.Proof.Reappends, endW)
	}
	wantC := reappendOf(t, 2, endW, full[endW:])
	wantTheCopy(t, r, st, 0xc100, wantC, format.SegmentHeaderSize, spare)
	// W's extent serves nothing, and W is nowhere in the target: its number
	// never reached the phase's input, so the copy holds one record, C's,
	// and the seal marker follows it — no second record slot for a W the
	// withholding never handed over.
	if _, ok := st.Rebuilt.Index.Lookup(tailFile(), 0x8100); ok {
		t.Errorf("Lookup over the withheld record's extent hit, want a miss: the feed serves nothing of a failed-list record")
	}
	b := r.fileBytes(format.SegmentName(spare))
	if _, merr := format.DecodeSealMarker(b[format.SegmentHeaderSize+tailLen:format.SegmentHeaderSize+tailLen+format.SealMarkerSize], journalID, spare, format.SegmentHeaderSize+tailLen); merr != nil {
		t.Errorf("no seal marker follows C's copy at %d: %v, want the marker of a target holding C's record alone", format.SegmentHeaderSize+tailLen, merr)
	}
}

// allZero reports whether every byte of b is zero.
func allZero(b []byte) bool {
	for _, x := range b {
		if x != 0 {
			return false
		}
	}
	return true
}

// countNonZero answers how many of b's bytes are not zero.
func countNonZero(b []byte) int {
	n := 0
	for _, x := range b {
		if x != 0 {
			n++
		}
	}
	return n
}

// TestSyncMarksAfterASealedCopySkip opens the device-lost directory twice,
// the second open skipping B's re-append for the sealed copy the first
// sealed. The source segment's scan still verifies B at its old placement,
// which the open's emptying unlinks, so no stream's mark may name it: the
// entry for B's stream names the sealed copy, at the copy's end and B's own
// sequence number, though the second open copied nothing.
func TestSyncMarksAfterASealedCopySkip(t *testing.T) {
	r, good, bAt := deviceLostB(t)
	first := core.SegmentID(9)
	runReserve(t, r, first)

	st, err := Run(context.Background(), r.runDeps(), core.Version{}, journalID, nil)
	if err != nil {
		t.Fatalf("the second open: %v", err)
	}
	if len(st.Proof.Replaced) != 0 {
		t.Fatalf("Replaced got %+v, want none: the second open skipped the sealed copy", st.Proof.Replaced)
	}

	b := reappendOf(t, 2, bAt, good)
	want := []core.SyncMark{{
		Stream:   0,
		Segment:  uint64(first),
		Through:  format.SegmentHeaderSize + b.Header.Size(),
		Offset:   format.SegmentHeaderSize + b.Header.Size(),
		Sequence: uint64(b.Header.Seq),
	}}
	if got := st.SyncMarks(); !slices.Equal(got, want) {
		t.Errorf("SyncMarks after the skipped re-append = %+v, want %+v: B's stream is credited with the sealed copy the first open wrote", got, want)
	}
}

// TestReappendFallsBackWhenNoReserveSpareWasAdopted holds the open's
// fallbacks: a directory whose adopted spares hold no repack reserve one
// re-appends into the headroom's spare, counting the borrow; one with only a
// content spare, or none, makes a spare of its own and raises the id seed the
// pool resumes from past it, never borrowing a content spare; and a make the
// device refuses is the device's own full-device answer.
func TestReappendFallsBackWhenNoReserveSpareWasAdopted(t *testing.T) {
	spare := core.SegmentID(9)
	t.Run("a headroom spare takes the copy", func(t *testing.T) {
		r, good, bAt := deviceLostB(t)
		want := reappendOf(t, 2, bAt, good)
		r.write(format.SegmentName(spare), make([]byte, testCap))
		d := r.runDeps()
		d.SpareTargets = segment.Targets{Headroom: 1} // the one spare adopted is the headroom's, the reserve has none

		st, err := Run(context.Background(), d, core.Version{}, journalID, nil)
		if err != nil {
			t.Fatalf("Run with only a headroom spare adopted: %v, want the re-append borrowed into it", err)
		}
		wantTheCopy(t, r, st, 0x8100, want, format.SegmentHeaderSize, spare)
		if len(st.Spares.Adopted) != 0 {
			t.Errorf("spares: the borrowed headroom spare left adoptions %v, want none: it is a data segment now", st.Spares.Adopted)
		}
		if st.Spares.Borrowed != 1 {
			t.Errorf("the open counts %d borrowed headroom spares, want 1", st.Spares.Borrowed)
		}
	})
	for _, leg := range []struct {
		name    string
		targets segment.Targets
		planted []core.SegmentID
	}{
		{"a content spare is never borrowed", segment.Targets{}, []core.SegmentID{spare}},
		{"no spare at all is made", segment.Targets{Headroom: 1}, nil},
	} {
		t.Run(leg.name, func(t *testing.T) {
			r, good, bAt := deviceLostB(t)
			want := reappendOf(t, 2, bAt, good)
			for _, id := range leg.planted {
				r.write(format.SegmentName(id), make([]byte, testCap))
			}
			d := r.runDeps()
			d.SpareTargets = leg.targets
			before := st0MaxID(t, r)

			st, err := Run(context.Background(), d, core.Version{}, journalID, nil)
			if err != nil {
				t.Fatalf("Run with no reserve or headroom spare adopted: %v, want a spare made for the re-append", err)
			}
			made := st.Proof.ReappendTarget
			if made <= before || made == spare {
				t.Fatalf("the copy went into segment %d, want a spare made above the directory's highest id %d and never the content spare %d", made, before, spare)
			}
			wantTheCopy(t, r, st, 0x8100, want, format.SegmentHeaderSize, made)
			if st.Attached.MaxID < made {
				t.Errorf("the id seed stands at %d, below the made spare %d: the pool's maker would draw its name again", st.Attached.MaxID, made)
			}
			if st.Spares.Borrowed != 0 {
				t.Errorf("the open counts %d borrowed headroom spares, want none", st.Spares.Borrowed)
			}
		})
	}
	t.Run("a make the device refuses is a full device", func(t *testing.T) {
		r, _, _ := deviceLostB(t)
		r.faults = append(r.faults, kit.Failing(kit.OpCreate, "*.spare.tmp", 0, syscall.ENOSPC))
		r.scheduleFaults()
		d := r.runDeps()
		d.SpareTargets = segment.Targets{Headroom: 1}
		before := r.fileBytes(format.SegmentName(2))

		_, err := Run(context.Background(), d, core.Version{}, journalID, nil)
		if !errors.Is(err, syscall.ENOSPC) || errors.Is(err, core.ErrNoSpace) {
			t.Fatalf("Run answered %v, want the device's full-device refusal (ENOSPC), never the journal's own spare refusal that retries", err)
		}
		if after := r.fileBytes(format.SegmentName(2)); !slices.Equal(after, before) {
			t.Errorf("the refused open changed the source's bytes: no source is touched before its copy")
		}
	})
}

// st0MaxID is the highest segment id the rig's directory names now.
func st0MaxID(t *testing.T, r *rig) core.SegmentID {
	t.Helper()
	return segment.HighestID(r.listing())
}
