package recovery

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"maps"
	"reflect"
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
// cut it — its last verifying record's end, the offset the tail-cutting
// phases truncate at.
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

// TestMarkProvesARottedCloneTargetBelowThrough plants a segment whose only
// record is a clone target whose header verifies and whose body fails — the
// shape the classing reads as a torn tail, its header naming the record —
// in a segment no successor names, with the mark's Through covering the
// record: the sync reached it, so it is corruption its own header names, a
// corrupt drop, never a refusal, and the segment joins the damaged set. The
// same plant with the Through at the drop's own slot leaves the record torn:
// a mark proves only up to it.
func TestMarkProvesARottedCloneTargetBelowThrough(t *testing.T) {
	for _, leg := range []struct {
		name    string
		through func(droppedAt, droppedEnd int64) int64
		want    bool
	}{
		{"below the Through, corrupt", func(drop, dropEnd int64) int64 { return dropEnd }, true},
		{"above the Through, torn", func(_, _ int64) int64 { return format.SegmentHeaderSize }, false},
	} {
		t.Run(leg.name, func(t *testing.T) {
			r := newRig(t)
			r.writeFormat()
			id := core.SegmentID(2)
			seg, droppedAt := appendClone(raw.SegmentHeader{Identity: journalID, ID: uint64(id)}.Bytes(), id,
				func(int64) int64 { return 0 })
			droppedEnd := int64(len(seg))
			seg = append(seg, make([]byte, testCap-droppedEnd)...)
			r.plantSeg(id, seg)
			through := leg.through(droppedAt, droppedEnd)

			st := runWithMarks(t, r, []core.SyncMark{
				{Stream: 1, Segment: uint64(id), Through: through, Offset: through, Sequence: 200},
			})

			if got, want := len(st.Tails.Corrupt), map[bool]int{true: 1, false: 0}[leg.want]; got != want {
				t.Fatalf("the %s leg holds %d corrupt drops, want %d: a record below the mark's Through %s", leg.name, got, want,
					map[bool]string{true: "is corruption its header names", false: "was never proven synced"}[leg.want])
			}
			if leg.want {
				if st.Tails.Corrupt[0].Segment != id {
					t.Errorf("Corrupt got %+v, want the one drop of segment %d", st.Tails.Corrupt, id)
				}
				if got, want := st.Tails.Damaged, []core.SegmentID{id}; !slices.Equal(got, want) {
					t.Errorf("Damaged got %v, want %v: a mark-proven dropped record is synced damage", got, want)
				}
			} else if len(st.Tails.Damaged) != 0 {
				t.Errorf("Damaged got %v, want none: a drop above the mark's Through was never proven synced", st.Tails.Damaged)
			}
			wantFate(t, st, id, fateTornTail, format.SegmentHeaderSize)
		})
	}
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

	t.Run("a predecessor no disk segment holds is gone", func(t *testing.T) {
		r := newRig(t)
		r.writeFormat()
		gone, take := core.SegmentID(99), core.SegmentID(3)
		plantTake(r, take, gone)

		st := runWithMarks(t, r, nil)

		if f, ok := st.Marks.fates[gone]; !ok || f != predGone {
			t.Errorf("marks: the absent predecessor %d got verdict %v (entry present %v), want %v: the map names every predecessor the topology did", gone, f, ok, predGone)
		}
		if got, want := st.Spares.Adopted, []AdoptedSpare{{ID: take, Class: segment.Content}}; !slices.Equal(got, want) {
			t.Errorf("spares: the take over the gone predecessor got %v, want %v: a name no file holds holds no take back", got, want)
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

// seqSyncedRec is syncedRec with the record's own sequence number set: the
// lost-tail fixtures plant records the marks name by number, one number per
// record, so the check's answers read at a glance.
func seqSyncedRec(seq, off uint64, synced int64) raw.Record {
	r := syncedRec(seq, off, synced)
	r.Seq = seq
	return r
}

// lostTailMark builds the mark of a stream whose sync covered one record past
// A: it names C, of sequence seq, ending at endC, the offset where the
// segment the stream lost C from would have ended.
func lostTailMark(id core.SegmentID, seq uint64, endC int64) []core.SyncMark {
	return []core.SyncMark{{Stream: 1, Segment: uint64(id), Through: endC, Offset: endC, Sequence: seq}}
}

// TestLostTailRefusesTheOpen plants the durable shape of a device that
// acknowledged a sync and then lost the last record it covered: the segment's
// file ends at A, the record the sync proved, while the mark — the caller's
// durable record of the sync — names C past A's end, with C's number and its
// end. No copy of C verifies anywhere, so the open refuses with
// ErrNotImplemented naming the mark's segment, its Offset and its Sequence —
// a lost tail must never be read as a torn tail — and the refusal writes
// nothing: the bytes the directory held are the bytes it keeps, and the run
// answers no state at all.
func TestLostTailRefusesTheOpen(t *testing.T) {
	r := newRig(t)
	r.writeFormat()
	id := core.SegmentID(2)
	endA := endAfter(seqSyncedRec(199, 0x4100, 0))
	r.plantSeg(id, buildSeg(id, seqSyncedRec(199, 0x4100, endA)))
	endC := endA + int64(len(seqSyncedRec(200, 0x8100, endA).Bytes()))
	name := format.SegmentName(id)
	if int64(len(r.fileBytes(name))) != endA {
		t.Fatalf("fixture: the lost tail's file holds %d bytes, want %d: the device held A and lost C", len(r.fileBytes(name)), endA)
	}
	wantListing := slices.Clone(r.listing())
	segBytes := r.fileBytes(name)
	removes := r.seam.Calls(kit.OpRemove)
	writes := r.seam.Calls(kit.OpWrite)
	dirSyncs := r.seam.Calls(kit.OpSyncDir)

	st, err := Run(context.Background(), r.runDeps(), core.Version{}, journalID, lostTailMark(id, 200, endC))

	if !errors.Is(err, core.ErrNotImplemented) {
		t.Fatalf("the lost tail answered err %v, want ErrNotImplemented refusing the open", err)
	}
	for _, want := range [3]string{
		fmt.Sprintf("segment %d", id),
		fmt.Sprintf("sequence %d", 200),
		fmt.Sprintf("ending at %d", endC),
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal %q does not name %s, the mark's own fact", err.Error(), want)
		}
	}
	if !slices.Equal(r.listing(), wantListing) {
		t.Errorf("the refused open changed the directory %v, want the names it held %v", r.listing(), wantListing)
	}
	if got := r.fileBytes(name); !slices.Equal(got, segBytes) {
		t.Errorf("the refused open changed the lost segment's bytes, got %x..., want the bytes it held", got[:format.SegmentHeaderSize])
	}
	if got, want := r.seam.Calls(kit.OpRemove), removes; got != want {
		t.Errorf("the refused open removed something, got %d removes, want %d, the count before the run", got, want)
	}
	if got, want := r.seam.Calls(kit.OpWrite), writes; got != want {
		t.Errorf("the refused open wrote something, got %d writes, want %d, the count before the run", got, want)
	}
	if got, want := r.seam.Calls(kit.OpSyncDir), dirSyncs; got != want {
		t.Errorf("the refused open synced the directory, got %d syncs, want %d, the count before the run", got, want)
	}
	if !reflect.DeepEqual(st, State{}) {
		t.Errorf("the refused open returned state %+v, want the zero State: a refusal serves nothing", st)
	}
}

// TestLostTailWithCachedCopyReappends plants the same loss with C whole in the
// cache alone: the device's image of C is rotted, the pages a dead process
// held it in serve it whole, and the sync the tail proof makes writes nothing
// past them. C verifies at its first read and the proof hands it to the
// re-append — the one set of records the lost-tail check must never read as
// lost — so the open succeeds, and the record is duly in the re-append set
// with the copy its first read verified.
func TestLostTailWithCachedCopyReappends(t *testing.T) {
	r := newRig(t)
	r.writeFormat()
	id := core.SegmentID(2)
	name := format.SegmentName(id)
	endA := endAfter(seqSyncedRec(199, 0x4100, 0))
	full := buildSeg(id, seqSyncedRec(199, 0x4100, endA), seqSyncedRec(200, 0x8100, endA))
	durable := slices.Clone(full)
	durable[endA+format.RecordHeaderSize+8+2] ^= 0xff // C's payload, rotted where the device alone sees it
	r.plantSeg(id, durable)
	r.writeLiveOnly(name, endA, full[endA:]) // the good C, whole in the cache alone
	r.dropSyncs(name)                        // the open's sync makes nothing durable

	st, err := Run(context.Background(), r.runDeps(), core.Version{}, journalID,
		lostTailMark(id, 200, int64(len(full))))

	if err != nil {
		t.Fatalf("the cached copy answered err %v, want the open to succeed over a record the re-append carries", err)
	}
	if len(st.Proof.Reappends) != 1 || st.Proof.Reappends[0].Segment != id || st.Proof.Reappends[0].At != endA {
		t.Errorf("Reappends got %+v, want the one cached record of segment %d at %d", st.Proof.Reappends, id, endA)
	}
}

// TestLostTailRefusedRetriesBurnNothing runs the refused open three times in a
// row over the same directory, an interrupted recycle planted beside the lost
// tail — a candidate whose dirty header and trailer slots the spares phase
// would zero and sync were it ever to run before the refusal — and compares
// the directory after each run: every name, every file's bytes and the count
// of the segment files are what they were before the first, so each refused
// retry burned nothing: no spare was taken, zeroed or unlinked on any retry,
// for the check runs before the open's first writing phase.
func TestLostTailRefusedRetriesBurnNothing(t *testing.T) {
	r := newRig(t)
	r.writeFormat()
	id := core.SegmentID(2)
	endA := endAfter(seqSyncedRec(199, 0x4100, 0))
	r.plantSeg(id, buildSeg(id, seqSyncedRec(199, 0x4100, endA)))
	endC := endA + int64(len(seqSyncedRec(200, 0x8100, endA).Bytes()))
	plantInterruptedRecycle(r, core.SegmentID(5), core.SegmentID(9))
	marks := lostTailMark(id, 200, endC)
	before := r.snapshot()
	segsOf := func() int {
		n := 0
		for _, name := range r.listing() {
			if kind, _ := format.ClassifyName(name); kind == format.NameSegment {
				n++
			}
		}
		return n
	}
	segCount := segsOf()

	for open := 1; open <= 3; open++ {
		_, err := Run(context.Background(), r.runDeps(), core.Version{}, journalID, marks)

		if !errors.Is(err, core.ErrNotImplemented) {
			t.Fatalf("refused open %d answered err %v, want ErrNotImplemented: every retry meets the same loss", open, err)
		}
		got := r.snapshot()
		if !maps.EqualFunc(got, before, bytes.Equal) {
			t.Errorf("refused open %d changed the directory: got the files %v, want the bytes the first open found", open, slices.Sorted(maps.Keys(got)))
		}
		if count := segsOf(); count != segCount {
			t.Errorf("refused open %d left %d segment files, want %d, the count before the first: no retry takes, zeroes or unlinks a spare", open, count, segCount)
		}
	}
}

// TestLostTailWithACopyElsewhereIsNotLost plants the lost tail beside a second
// segment holding a copy of C, the record the mark names — the shape a repack
// or a re-append from an earlier open leaves: the copy verifies in the other
// segment even though the mark still names the segment it was appended in,
// with its Offset past the last record that one holds. No copy anywhere is
// one leg of a lost tail, and this copy answers it, so the open proceeds.
func TestLostTailWithACopyElsewhereIsNotLost(t *testing.T) {
	r := newRig(t)
	r.writeFormat()
	id, other := core.SegmentID(2), core.SegmentID(4)
	endA := endAfter(seqSyncedRec(199, 0x4100, 0))
	r.plantSeg(id, buildSeg(id, seqSyncedRec(199, 0x4100, endA)))
	endC := endA + int64(len(seqSyncedRec(200, 0x8100, endA).Bytes()))
	cEnd := endAfter(seqSyncedRec(200, 0x8100, 0))
	r.plantSeg(other, buildSeg(other, seqSyncedRec(200, 0x8100, cEnd)))

	st, err := Run(context.Background(), r.runDeps(), core.Version{}, journalID, lostTailMark(id, 200, endC))

	if err != nil {
		t.Fatalf("the copy in segment %d answered err %v, want the open to proceed over a record that verifies elsewhere", other, err)
	}
	if !st.Kinds.MarkedOnDisk[200] {
		t.Errorf("the feed left the mark's sequence 200 unraised, so a record the walks attached carrying it went unseen")
	}
}

// TestMarklessSegmentIsRetired plants one ordinary segment and a mark whose
// segment no file names — an id above every one on disk — with the Offset
// past a header's end and a sequence number nothing on disk carries: the very
// shape that would be a lost tail, had the segment been there to lose. The
// absence is read as a retirement, and a retirement is not a loss, so the open
// proceeds and no fate names the retired id.
func TestMarklessSegmentIsRetired(t *testing.T) {
	r := newRig(t)
	r.writeFormat()
	id := core.SegmentID(3)
	endA := endAfter(seqSyncedRec(199, 0x4100, 0))
	r.plantSeg(id, buildSeg(id, seqSyncedRec(199, 0x4100, endA)))

	st, err := Run(context.Background(), r.runDeps(), core.Version{}, journalID,
		lostTailMark(42, 300, 4096))

	if err != nil {
		t.Fatalf("the retired mark answered err %v, want the open to read the mark's absent segment as a retirement", err)
	}
	for _, f := range st.Tails.Fates {
		if f.ID == 42 {
			t.Errorf("the retired id 42 took the fate %v, want none: an absent segment is retired, never ended", f.Kind)
		}
	}
}
