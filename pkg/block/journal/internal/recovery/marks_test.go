package recovery

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/format"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/segment"
	"github.com/cubbit/dittofs/pkg/block/journal/journaltest/kit"
	"github.com/cubbit/dittofs/pkg/block/journal/journaltest/raw"
)

// runWithMarks is the rig's Run under the marks a caller's group commit
// recorded, the way Open hands them over.
func runWithMarks(t *testing.T, r *rig, marks []core.SyncMark) State {
	t.Helper()
	st, err := Run(context.Background(), r.runDeps(), core.Version{}, journalID, marks)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	return st
}

// marksSlotSeg returns the bytes of segment id holding A — a verifying record
// naming its own end as its synced-through — a 16-byte zero slot past it and
// R, which verifies and names no sync: the shape the classing reads as a
// torn slot when nothing proves the slot, with the slot's and the record's
// end offsets, the one proof a mark's Through can add.
func marksSlotSeg(id core.SegmentID) (seg []byte, slotAt, slotEnd, endR int64) {
	seg = slotSeg(id, func(slot int64) int64 { return 0 })
	endA := int64(len(buildSeg(id, tailRec(200, 0x4100))))
	return seg, endA, endA + 16, int64(len(seg))
}

// TestMarksThroughProvesBelowIt plants the torn slot a mark's Through covers,
// in a segment whose catalog does not verify: nothing on the segment names
// the record that sat in the slot, so the open refuses on the range whole,
// with ErrNotImplemented naming the segment and the range — a mark is the
// caller's durable record that its stream synced through it, and cutting the
// range as a torn tail would cut a record the sync acknowledged.
func TestMarksThroughProvesBelowIt(t *testing.T) {
	r := newRig(t)
	r.writeFormat()
	id := core.SegmentID(3)
	seg, slotAt, slotEnd, endR := marksSlotSeg(id)
	seg = append(seg, make([]byte, testCap-int64(len(seg)))...)
	r.plantSeg(id, seg)
	removes := r.seam.Calls(kit.OpRemove)

	_, err := Run(context.Background(), r.runDeps(), core.Version{}, journalID,
		[]core.SyncMark{{Stream: 1, Segment: uint64(id), Through: endR, Offset: endR, Sequence: 200}})

	if !errors.Is(err, core.ErrNotImplemented) {
		t.Fatalf("marks: the slot below a mark's Through answered err %v, want ErrNotImplemented refusing the range", err)
	}
	name := fmt.Sprintf("segment %d", id)
	if !strings.Contains(err.Error(), name) {
		t.Errorf("marks: the refusal %q does not name %s, the mark's segment", err.Error(), name)
	}
	rng := fmt.Sprintf("[%d,%d)", slotAt, slotEnd)
	if !strings.Contains(err.Error(), rng) {
		t.Errorf("marks: the refusal %q does not name the range %s the mark proves synced", err.Error(), rng)
	}
	if got, want := r.seam.Calls(kit.OpRemove), removes; got != want {
		t.Errorf("marks: the refused open removed something, got %d removes, want %d, the count before the run", got, want)
	}
}

// TestMarksTornSlotAboveThroughIsTorn plants the same slot above the mark's
// Through: the mark proves only up to Through, so the slot stays a torn
// write, unreported and undropped, and the segment ends where the classing
// cut it, at the slot the run's tail-cutting phases truncate.
func TestMarksTornSlotAboveThroughIsTorn(t *testing.T) {
	r := newRig(t)
	r.writeFormat()
	id := core.SegmentID(3)
	seg, slotAt, _, endR := marksSlotSeg(id)
	seg = append(seg, make([]byte, testCap-int64(len(seg)))...)
	r.plantSeg(id, seg)

	st := runWithMarks(t, r, []core.SyncMark{
		{Stream: 1, Segment: uint64(id), Through: slotAt, Offset: slotAt, Sequence: 200},
	})

	if got := planted(st); got != 2 {
		t.Errorf("the index holds %d entries, want 2: A and R attach with the slot above the mark's Through", got)
	}
	wantFate(t, st, id, fateEnded, endR)
	if got := st.Tails.Damaged; len(got) != 0 {
		t.Errorf("Damaged got %v, want none: a slot above every mark's Through was never proven synced", got)
	}
	if got := st.Tails.Corrupt; len(got) != 0 {
		t.Errorf("Corrupt got %+v, want none: a torn record is never reported or dropped", got)
	}
}

// TestMarkCountsTornHeaderAsSynced plants a segment whose header slot is
// torn — damaged bytes, classed torn because no proof covers them — and a
// mark whose Through passes the header: the sync reached what the slot held,
// so the header counts as synced damage and DamagedSegments reports the
// segment it did not report before.
func TestMarkCountsTornHeaderAsSynced(t *testing.T) {
	r := newRig(t)
	r.writeFormat()
	id := core.SegmentID(3)
	seg := damageHeader(buildSeg(id, tailRec(200, 0x4100)))
	seg = append(seg, make([]byte, testCap-int64(len(seg)))...)
	r.plantSeg(id, seg)

	st := runWithMarks(t, r, []core.SyncMark{
		{Stream: 1, Segment: uint64(id), Through: format.SegmentHeaderSize, Offset: format.SegmentHeaderSize, Sequence: 200},
	})

	wantDamaged(t, st, id)
	wantFate(t, st, id, fateEnded, endAfter(tailRec(200, 0x4100)))
}

// TestMarkWithheldSegment plants the record-less whole segment a mark names,
// the mark's Offset at its header's end so nothing was lost: a mark's segment
// is never re-adopted, for a re-append landing in it would hide the loss it
// guards, so the raise withholds it from the spare test and ends it — the
// fate that takes it no append.
func TestMarkWithheldSegment(t *testing.T) {
	r := newRig(t)
	r.writeFormat()
	id := core.SegmentID(4)
	name := format.SegmentName(id)
	r.write(name, make([]byte, testCap))
	writes := r.seam.CallsFor(kit.OpWrite, name)

	marks := []core.SyncMark{{Stream: 1, Segment: uint64(id), Offset: format.SegmentHeaderSize, Sequence: 1}}
	st := runWithMarks(t, r, marks)

	cands, _ := spareCandidates(st.Attached, st.Rebuilt.Scans, successorSet(st.Attached.Own), marks, st.Marks.fates)
	if slices.Contains(cands, id) {
		t.Errorf("spareCandidates: the mark's record-less segment %d was a candidate, got %v: a re-adopted mark segment hides the loss it guards", id, cands)
	}
	wantFate(t, st, id, fateEnded, 0)
	if len(st.Spares.Adopted) != 0 || len(st.Spares.Unlinked) != 0 {
		t.Errorf("spares: the mark's segment got adopted %v and unlinked %v, want neither: its bytes are the evidence the later marks steps read", st.Spares.Adopted, st.Spares.Unlinked)
	}
	if got, want := r.seam.CallsFor(kit.OpWrite, name), writes; got != want {
		t.Errorf("spares: the mark's segment was written to, got %d writes, want %d, the count before the run", got, want)
	}
}

// TestMarksCountersAboveEveryMark runs the open over a directory that holds
// one segment and low numbers, with marks naming segments and numbers
// nothing on disk carries: the next sequence number is issued above every
// mark's Sequence and the attach pass's id seed ends above every mark's
// Segment, so the next spare made after the open takes a higher id — a
// drained directory never reissues a mark's numbers.
func TestMarksCountersAboveEveryMark(t *testing.T) {
	r := newRig(t)
	r.writeFormat()
	id := core.SegmentID(3)
	seg := buildSeg(id, tailRec(200, 0x4100))
	seg = append(seg, make([]byte, testCap-int64(len(seg)))...)
	r.plantSeg(id, seg)
	marks := []core.SyncMark{
		{Stream: 1, Segment: 42, Sequence: 500},
		{Stream: 2, Segment: 800, Sequence: 900},
	}

	st := runWithMarks(t, r, marks)

	next, err := st.Rebuilt.Counters.NextSeq()
	if err != nil {
		t.Fatalf("counters: the next sequence number was refused: %v", err)
	}
	if next <= 900 {
		t.Errorf("counters: the next sequence number after the open is %d, want above 900, every mark's Sequence", next)
	}
	if got := st.Attached.MaxID; got < 800 {
		t.Errorf("attach: the id seed after the open is %d, want at least 800, every mark's Segment: the next spare made takes a higher id", got)
	}
}

// TestPredecessorVerdictReachesTheSparesPhase pins the verdict's wiring end
// to end: P a predecessor whose header slot is torn and whose trailer names a
// footer just past the rotted marker slot — the shape the classing reads
// through its marker-alone exemption, and a mark names, for it holds no
// verifying record, so the raise ends it — and W a record-less take naming P.
// The verdict says predEnded, and the spares phase holds W: the open ends P
// but has unlinked nothing, and a take adopted before P's unlink is durable
// would zero the only header naming P and leave the next open reading its
// predecessor with less evidence than this one had. W joins the pool only
// when the emptying has unlinked P with a synced unlink, the moment its
// record-less test runs over a name no file holds. A predecessor whose seal
// its own evidence proves — P sealed with a verifying marker and a record —
// says predSealed, and its take is re-adopted at once: the hold ends where a
// durable seal proves the predecessor survived, and a predecessor no file
// names is gone, its name answering nothing on disk.
func TestPredecessorVerdictReachesTheSparesPhase(t *testing.T) {
	p, w := core.SegmentID(1), core.SegmentID(2)

	t.Run("a predecessor the open ends keeps its take held", func(t *testing.T) {
		r := newRig(t)
		r.writeFormat()
		b := damageHeader(raw.SegmentHeader{Identity: journalID, ID: uint64(p)}.Bytes())
		b = append(b, make([]byte, testCap-raw.TrailerSize-int64(len(b)))...)
		b = append(b, raw.Trailer(1, uint64(format.SegmentHeaderSize+format.SealMarkerSize))...)
		r.plantSeg(p, b)
		plantTake(r, w, p)
		takeName := format.SegmentName(w)
		writes := r.seam.CallsFor(kit.OpWrite, takeName)
		before := r.fileBytes(takeName)

		st := runWithMarks(t, r, []core.SyncMark{
			{Stream: 1, Segment: uint64(p), Offset: format.SegmentHeaderSize, Sequence: 200},
		})

		if got := st.Marks.fates[p]; got != predEnded {
			t.Errorf("marks: the ended predecessor %d's verdict got %v, want %v", p, got, predEnded)
		}
		wantFate(t, st, p, fateEnded, 0)
		if got, want := st.Spares.Held, []core.SegmentID{w}; !slices.Equal(got, want) {
			t.Fatalf("spares: the take naming the ended predecessor got held %v, want %v: the open has unlinked nothing", got, want)
		}
		if len(st.Spares.Adopted) != 0 || len(st.Spares.Unlinked) != 0 {
			t.Errorf("spares: over an ended predecessor the open got adopted %v and unlinked %v, want neither", st.Spares.Adopted, st.Spares.Unlinked)
		}
		if got, want := r.seam.CallsFor(kit.OpWrite, takeName), writes; got != want {
			t.Errorf("spares: the held take was written to, got %d writes, want %d, the count before the run: no slot of a held take is zeroed", got, want)
		}
		if got := r.fileBytes(takeName); !slices.Equal(got, before) {
			t.Errorf("spares: the held take's bytes changed, got %x..., want the bytes it arrived with", got[:format.SegmentHeaderSize])
		}
		if !slices.Contains(r.listing(), format.SegmentName(p)) {
			t.Errorf("spares: the ended predecessor %s is gone from the directory, want it on disk until the emptying unlinks it", format.SegmentName(p))
		}
	})

	t.Run("a predecessor whose end its evidence seals frees its take", func(t *testing.T) {
		r := newRig(t)
		r.writeFormat()
		r.plantSeg(p, sealedFull(p))
		plantTake(r, w, p)

		st := runWithMarks(t, r, nil)

		if got := st.Marks.fates[p]; got != predSealed {
			t.Errorf("marks: the sealed predecessor %d's verdict got %v, want %v", p, got, predSealed)
		}
		if got := st.Spares.Held; len(got) != 0 {
			t.Errorf("spares: the take naming the sealed predecessor got held %v, want none: its ending is proved, and the pool takes the segment at once", got)
		}
		if got, want := st.Spares.Adopted, []AdoptedSpare{{ID: w, Class: segment.Content}}; !slices.Equal(got, want) {
			t.Errorf("spares: the take over the sealed predecessor got adoptions %v, want %v", got, want)
		}
	})
}
