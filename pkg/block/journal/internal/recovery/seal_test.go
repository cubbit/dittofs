package recovery

import (
	"bytes"
	"errors"
	"slices"
	"syscall"
	"testing"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/format"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/segment"
	"github.com/cubbit/dittofs/pkg/block/journal/journaltest/kit"
	"github.com/cubbit/dittofs/pkg/block/journal/journaltest/raw"
)

// earlySealSize answers the size an early seal of marker holding n catalog
// entries leaves the file at: the marker, the footer and the trailer written
// after the cut, nothing else.
func earlySealSize(marker int64, n int) int64 {
	return marker + format.SealMarkerSize + int64(n)*format.CatalogEntrySize + format.TrailerSize
}

// wantSealed holds the cut-and-seal phase's on-disk answer over one segment:
// every byte below the cut exactly as planted — so the cut never crossed a
// verifying record — the file ending at the early seal's own size, and the
// footer there verifying with its entries under a marker at the cut.
func wantSealed(t *testing.T, r *rig, id core.SegmentID, planted []byte, cut int64, n int) {
	t.Helper()
	f := r.fileBytes(format.SegmentName(id))
	if got, want := int64(len(f)), earlySealSize(cut, n); got != want {
		t.Fatalf("segment %d's file is %d bytes, want the %d-byte early seal of the marker at %d",
			id, got, want, cut)
	}
	if !bytes.Equal(f[:cut], planted[:cut]) {
		t.Fatalf("segment %d's bytes below the cut at %d changed, got %x, want the planted %x",
			id, cut, f[:cut], planted[:cut])
	}
	footer, err := format.ReadFooter(bytes.NewReader(f), int64(len(f)), journalID, id)
	if err != nil {
		t.Fatalf("segment %d's seal holds no verifying catalog: %v", id, err)
	}
	if footer.Offset != cut+format.SealMarkerSize {
		t.Errorf("segment %d's footer runs at %d, want %d: its entries begin past the marker at the cut %d",
			id, footer.Offset, cut+format.SealMarkerSize, cut)
	}
	if len(footer.Entries) != n {
		t.Errorf("segment %d's catalog holds %d entries, want %d", id, len(footer.Entries), n)
	}
	if len(footer.Entries) > 0 {
		last := footer.Entries[len(footer.Entries)-1]
		end := last.RecordOffset + format.RecordHeader{Kind: last.Kind, Extent: core.Extent{Len: last.Extent.Len}}.Size()
		if end != cut {
			t.Errorf("segment %d's last entry runs to %d, want the cut %d: the marker follows the last record exactly", id, end, cut)
		}
	}
}

// wantRow holds the segment ledger row the open continues from: segment id's
// state and whether its seal is full.
func wantRow(t *testing.T, st State, id core.SegmentID, want segment.State, full bool) {
	t.Helper()
	got, err := st.Assembled.Set.State(id)
	if err != nil || got != want {
		t.Errorf("the assembled set holds segment %d as %v (err %v), want %v", id, got, err, want)
	}
	if gotFull := st.Assembled.Set.SealedFull(id); gotFull != full {
		t.Errorf("segment %d carries the full word %v, want %v", id, gotFull, full)
	}
}

// TestTornTailCutAndSealed pins the cut-and-seal phase's whole answer at the
// crash seam: a torn last record is cut at the start of the torn tail — the
// end of the last record that verifies — and the segment early-sealed with a
// verifying catalog over the surviving record, so no write can land after the
// torn bytes again. The reopened directory then reads a sealed segment that
// takes no append and changes nothing: the stream, wherever it continues,
// never returns to it. The journal-level row of the torn-tail cut — a write
// after the reopen synced and crashed again reads back — rides the
// conformance tier; this is its recovery-side half.
func TestTornTailCutAndSealed(t *testing.T) {
	endA := endAfter(tailRec(200, 0x4100))
	full := buildSeg(2, tailRec(200, 0x4100), tailRec(201, 0x8100))
	planted := full[:len(full)-5] // T torn mid-body; its header verifies
	r := newRig(t)
	r.writeFormat()
	r.plantSeg(2, planted)

	st := runTails(t, r)

	if st.Tails.TornTails != 1 {
		t.Fatalf("TornTails = %d after one torn tail, want 1", st.Tails.TornTails)
	}
	wantSealed(t, r, 2, planted, endA, 1)
	if got := len(st.Rebuilt.Index.FileEntries(tailFile())); got != 1 {
		t.Errorf("the index holds %d entries, want 1: the record below the cut survives it", got)
	}
	if f := footerOf(t, r.fileBytes(format.SegmentName(2)), 2); len(f.Entries) != 1 ||
		f.Entries[0].RecordOffset != format.SegmentHeaderSize || f.Entries[0].Kind != format.KindWrite ||
		f.Entries[0].Seq != 100 {
		t.Errorf("the catalog got entries %+v, want the one surviving write at the first record slot", f.Entries)
	}
	wantRow(t, st, 2, segment.Sealed, false)

	// The reopen: the sealed segment proves its own end on the device, so the
	// open reads it whole and writes nothing, counts no torn tail again, and
	// keeps every record below the marker.
	before := r.snapshot()
	st2 := runTails(t, r)
	if slices.Contains(st2.Tails.Damaged, 2) {
		t.Errorf("the reopen reported the sealed segment damaged, want none")
	}
	wantFate(t, st2, 2, fateSealed, 0)
	if st2.Tails.TornTails != 0 {
		t.Errorf("the reopen counted %d torn tails, want 0: the seal's marker is its own proof", st2.Tails.TornTails)
	}
	if got := len(st2.Rebuilt.Index.FileEntries(tailFile())); got != 1 {
		t.Errorf("the reopen's index holds %d entries, want 1: a seal crosses no record below it", got)
	}
	for name, was := range before {
		if got := r.fileBytes(name); !bytes.Equal(got, was) {
			t.Errorf("the reopen changed %s, want the cut-and-sealed directory as it stood", name)
		}
	}
}

// TestCutNeverPassesAVerifyingRecord holds the cut's floors over three planted
// layouts, asserting the answer on disk: the cut is never below the last
// verifying record (the bytes below it are exactly the planted ones, so every
// acknowledging survivor stays), never below the highest synced-through a
// verifying record names, and never below the mark's own Through — the floor
// the marks raise folded into the fate and the phase that truncates must
// keep, for a sealed-segment end below a surviving mark's Offset wedges every
// later open on the lost-tail refusal.
func TestCutNeverPassesAVerifyingRecord(t *testing.T) {
	t.Run("the last verifying record", func(t *testing.T) {
		endA := endAfter(tailRec(200, 0x4100))
		seg := buildSeg(2, tailRec(200, 0x4100))
		seg = append(seg, bytes.Repeat([]byte{0xff}, 16)...) // the torn tail past A
		r := newRig(t)
		r.writeFormat()
		r.plantSeg(2, seg)

		st := runTails(t, r)

		wantFate(t, st, 2, fateTornTail, endA)
		wantSealed(t, r, 2, seg, endA, 1)
		if st.Tails.TornTails != 1 {
			t.Errorf("TornTails = %d, want 1", st.Tails.TornTails)
		}
	})
	t.Run("the mark's Through", func(t *testing.T) {
		id := core.SegmentID(2)
		seg, _ := appendClone(raw.SegmentHeader{Identity: journalID, ID: uint64(id)}.Bytes(),
			id, func(int64) int64 { return 0 })
		droppedEnd := int64(len(seg))
		seg = append(seg, make([]byte, testCap-droppedEnd)...)
		r := newRig(t)
		r.writeFormat()
		r.plantSeg(id, seg)

		st := runWithMarks(t, r, []core.SyncMark{
			{Stream: 1, Segment: uint64(id), Through: droppedEnd, Offset: droppedEnd, Sequence: 200},
		})

		// The fold floored the fate at the mark's Through, the end of the
		// record the sync proved, and the phase truncates exactly there: the
		// rotted bytes of the corrupt drop stay below the cut, on disk, and
		// the seal's marker follows the end the mark named — a cursor below
		// them would answer for the sync with a seal over its own floor.
		wantFate(t, st, id, fateTornTail, droppedEnd)
		wantSealed(t, r, id, seg, droppedEnd, 1)
		if m := st.Tails.TornTails; m != 1 {
			t.Errorf("TornTails = %d, want 1", m)
		}
	})
	t.Run("the synced-through a record names", func(t *testing.T) {
		endA := endAfter(tailRec(200, 0x4100))
		a := syncedRec(200, 0x4100, endA)
		c := cloneRec()
		cOff := format.SegmentHeaderSize + int64(len(a.Bytes()))
		// C's own synced-through covers the whole of its bytes: the address
		// its rotted body's header still names, past the last record that
		// verified whole (A). The cut floors there, never at A's end.
		c.SyncedThrough = uint64(cOff + int64(len(c.Bytes())))
		p, _ := buildSealed(1, a, c)
		p[cOff+format.RecordHeaderSize] ^= 0xff // C's body rotted under its synced header
		markerOff := len(p) - raw.TrailerSize - raw.CatalogEntrySize*2 - format.SealMarkerSize
		p[markerOff+31] ^= 0xff // the marker rotted: the segment could have been open at a crash
		cEnd := cOff + int64(len(c.Bytes()))
		r := newRig(t)
		r.writeFormat()
		r.plantSeg(1, p)

		st := runTails(t, r)

		wantFate(t, st, 1, fateTornTail, cEnd)
		wantSealed(t, r, 1, p, cEnd, 2)
		if len(st.Tails.Corrupt) != 1 || !st.Tails.Corrupt[0].Drop.Synced || st.Tails.Corrupt[0].Drop.Header.Offset != cOff {
			t.Errorf("Corrupt got %+v, want the one synced drop at the rotted clone target's %d: the cut leaves the fold's report alone",
				st.Tails.Corrupt, cOff)
		}
		if !slices.Equal(st.Tails.Damaged, []core.SegmentID{1}) {
			t.Errorf("Damaged got %v, want [1]: the fold's synced corruption stays reported", st.Tails.Damaged)
		}
	})
}

// TestRecycledOldBytesCutUnreported plants a recycled segment's shape: a fresh
// header and a record of the new life at the front, and the old life's bytes
// past them — records that name the old segment so no header of them verifies
// where they sit, its seal marker and footer and trailer among them. The open
// reads the segment by its record headers alone (an unnamed segment is never
// offered a catalog, so the old one is never read), cuts the old bytes as the
// torn tail they are, and reports nothing: no damage, no corrupt drop, one
// torn tail. The conformance row keeps the emptying half waiting; this is the
// cut half.
func TestRecycledOldBytesCutUnreported(t *testing.T) {
	id := core.SegmentID(5)
	n := tailRec(300, 0x500)
	seg := raw.SegmentHeader{Identity: journalID, ID: uint64(id)}.Bytes()
	n.Identity, n.Segment, n.RecordOffset = journalID, uint64(id), uint32(len(seg))
	seg = append(seg, n.Bytes()...)
	end := int64(len(seg))
	old, _ := buildSealed(4, tailRec(100, 0x4100), tailRec(101, 0x8100))
	seg = append(seg, old[format.SegmentHeaderSize:]...)
	seg = append(seg, make([]byte, testCap-int64(len(seg)))...)
	r := newRig(t)
	r.writeFormat()
	r.plantSeg(id, seg)

	st := runTails(t, r)

	wantFate(t, st, id, fateTornTail, end)
	wantSealed(t, r, id, seg, end, 1)
	if st.Tails.TornTails != 1 {
		t.Errorf("TornTails = %d, want 1: the old bytes cut as one torn tail", st.Tails.TornTails)
	}
	if len(st.Tails.Damaged) != 0 || len(st.Tails.Corrupt) != 0 {
		t.Errorf("the recycled life reported damage or corruption: Damaged %v Corrupt %+v, want none",
			st.Tails.Damaged, st.Tails.Corrupt)
	}
}

// TestSealSyncFailureLeavesFailedSegment holds R52's arms at the phase that
// writes: a truncation whose sync fails here leaves its segment failed and
// unsealed for the open's continuation — the row the assembly seeded Active
// is ended, the open succeeds, and the not-durable cut shows nothing on disk
// — and a segment whose sync failed earlier in this open, at the tail's
// proof, is never truncated at all: it is left standing as the failed window
// it already is.
func TestSealSyncFailureLeavesFailedSegment(t *testing.T) {
	t.Run("the truncation's own sync", func(t *testing.T) {
		full := buildSeg(2, tailRec(200, 0x4100), tailRec(201, 0x8100))
		planted := full[:len(full)-5]
		r := newRig(t)
		r.writeFormat()
		r.plantSeg(2, planted)
		// The tail proof's one sync succeeds; the truncation's is the second.
		if err := r.seam.Schedule(kit.FailingFrom(kit.OpSync, format.SegmentName(2), 2, 1, syscall.EIO)); err != nil {
			t.Fatalf("schedule: %v", err)
		}

		st := runTails(t, r)

		if st.Tails.TornTails != 0 {
			t.Errorf("TornTails = %d, want 0: a cut its sync never made durable is not a tail counted", st.Tails.TornTails)
		}
		wantFate(t, st, 2, fateTornTail, endAfter(tailRec(200, 0x4100)))
		wantRow(t, st, 2, segment.Ended, false)
		if got := r.fileBytes(format.SegmentName(2)); !bytes.Equal(got, planted) {
			t.Errorf("the failed cut showed on disk, got %d bytes, want the file as it began", len(got))
		}
	})
	t.Run("a failed window is never truncated", func(t *testing.T) {
		seg, _ := buildSealed(3) // a record-less unnamed seal: the marker its proof must re-read
		r := newRig(t)
		r.writeFormat()
		r.plantSeg(3, seg)
		if err := r.seam.Schedule(kit.Failing(kit.OpSync, format.SegmentName(3), 1, syscall.EIO)); err != nil {
			t.Fatalf("schedule: %v", err)
		}

		st := runTails(t, r)

		wantFate(t, st, 3, fateEnded, format.SegmentHeaderSize)
		if !slices.Contains(st.Proof.FailedWindows, 3) {
			t.Errorf("FailedWindows got %v, want segment 3's window in it", st.Proof.FailedWindows)
		}
		wantRow(t, st, 3, segment.Ended, false)
		if got := r.fileBytes(format.SegmentName(3)); !bytes.Equal(got, seg) {
			t.Errorf("the failed window's segment was truncated on disk, got %d bytes, want the file whole as it began", len(got))
		}
	})
	t.Run("the seal writer's own sync", func(t *testing.T) {
		// The durable truncation is the phase's own second sync; the seal
		// writer's records-durable sync is the third, and the fault lands
		// inside the writer: the writer ends the row itself in the set, and
		// the open must go on — the row ended once, never a second end the
		// set refuses, and the segment left failed and unsealed for the
		// continuation.
		endA := endAfter(tailRec(200, 0x4100))
		full := buildSeg(2, tailRec(200, 0x4100), tailRec(201, 0x8100))
		planted := full[:len(full)-5]
		r := newRig(t)
		r.writeFormat()
		r.plantSeg(2, planted)
		if err := r.seam.Schedule(kit.FailingFrom(kit.OpSync, format.SegmentName(2), 3, 1, syscall.EIO)); err != nil {
			t.Fatalf("schedule: %v", err)
		}

		st := runTails(t, r)

		wantFate(t, st, 2, fateTornTail, endA)
		if st.Tails.TornTails != 0 {
			t.Errorf("TornTails = %d, want 0: the tail was cut but never sealed whole", st.Tails.TornTails)
		}
		wantRow(t, st, 2, segment.Ended, false)
		f := r.fileBytes(format.SegmentName(2))
		if int64(len(f)) != endA {
			t.Fatalf("segment 2's file is %d bytes, want the durable cut at %d alone: the writer wrote no marker", len(f), endA)
		}
		if _, err := format.ReadFooter(bytes.NewReader(f), endA, journalID, 2); err == nil {
			t.Errorf("the failed writer left a readable footer, want none: the segment stays unsealed")
		}
	})
}

// TestEndedSegmentTakesNoMarker holds R51: a segment the open ends — here over
// a torn slot a later record survives, the tail past it torn — is truncated at
// the end of its last verifying record and takes no seal marker, no footer and
// no trailer, so the next open cannot read the tear the ended fate answered
// for as corruption behind a seal. The zero slot and the record past it stay
// below the cut, and the row keeps the ending the assembly seeded.
func TestEndedSegmentTakesNoMarker(t *testing.T) {
	endA := endAfter(syncedRec(200, 0x4100, 0))
	seg := buildSeg(1, syncedRec(200, 0x4100, endA))
	seg = append(seg, make([]byte, 16)...) // the torn slot
	rr := tailRec(201, 0x8100)
	rr.Identity, rr.Segment, rr.RecordOffset = journalID, 1, uint32(len(seg))
	seg = append(seg, rr.Bytes()...)
	endR := int64(len(seg))
	tt := tailRec(202, 0xc100)
	tt.Identity, tt.Segment, tt.RecordOffset = journalID, 1, uint32(len(seg))
	seg = append(seg, tt.Bytes()...)
	seg[endR+format.RecordHeaderSize+8+2] ^= 0xff // T's payload torn
	r := newRig(t)
	r.writeFormat()
	r.plantSeg(1, seg)

	st := runTails(t, r)

	wantFate(t, st, 1, fateEnded, endR)
	f := r.fileBytes(format.SegmentName(1))
	if int64(len(f)) != endR {
		t.Fatalf("the ended segment is %d bytes, want %d: the cut runs to the last verifying record and adds nothing",
			len(f), endR)
	}
	if !bytes.Equal(f, seg[:endR]) {
		t.Fatalf("the cut changed bytes below the last verifying record, got %x, want the planted %x", f, seg[:endR])
	}
	if _, err := format.ReadFooter(bytes.NewReader(f), endR, journalID, 1); !errors.Is(err, format.ErrNoFooter) {
		t.Errorf("the ended segment holds a readable footer (%v), want none: an ended segment is never sealed", err)
	}
	if st.Tails.TornTails != 0 {
		t.Errorf("TornTails = %d, want 0: an ended cut is not a torn tail", st.Tails.TornTails)
	}
	wantRow(t, st, 1, segment.Ended, false)
	if got := len(st.Rebuilt.Index.FileEntries(tailFile())); got != 2 {
		t.Errorf("the index holds %d entries, want 2: the cut crosses no record", got)
	}
}

// wantCutLeft holds what the cut-and-seal phase left of a planted segment:
// every byte below the fate's cut exactly as planted — the cut crossed no
// record that verifies — and past the cut what the fate names: the early
// seal of a torn tail, with a verifying footer whose entries end at the
// marker, or the bare truncation of an ended one, with nothing on the file
// past the cut at all.
func wantCutLeft(t *testing.T, r *rig, st State, id core.SegmentID, planted []byte) {
	t.Helper()
	f := fateOf(t, st, id)
	if f.Offset == 0 {
		// Nothing to cut: an intact or ended-whole segment whose bytes the
		// open leaves exactly as planted, whatever segments the emptying
		// takes later.
		if file := r.fileBytes(format.SegmentName(id)); !bytes.Equal(file, planted) {
			t.Fatalf("segment %d's bytes changed under a %v fate with no cut, want the planted ones", id, f.Kind)
		}
		return
	}
	file := r.fileBytes(format.SegmentName(id))
	if !bytes.Equal(file[:f.Offset], planted[:f.Offset]) {
		t.Fatalf("segment %d's bytes below the cut at %d changed, got %x, want the planted %x",
			id, f.Offset, file[:f.Offset], planted[:f.Offset])
	}
	switch f.Kind {
	case fateTornTail:
		footer, err := format.ReadFooter(bytes.NewReader(file), int64(len(file)), journalID, id)
		if err != nil {
			t.Fatalf("segment %d's cut left no verifying catalog: %v", id, err)
		}
		want := footer.Offset + int64(len(footer.Entries))*format.CatalogEntrySize + format.TrailerSize
		if int64(len(file)) != want {
			t.Fatalf("segment %d's file is %d bytes, want the %d-byte early seal its footer names", id, len(file), want)
		}
		// The marker never sits below the fate's cut — the floor — and it
		// follows the last record exactly.
		if footer.Offset-format.SealMarkerSize < f.Offset {
			t.Errorf("segment %d's marker sits at %d, below the cut %d", id, footer.Offset-format.SealMarkerSize, f.Offset)
		}
		if len(footer.Entries) > 0 {
			last := footer.Entries[len(footer.Entries)-1]
			end := last.RecordOffset + format.RecordHeader{Kind: last.Kind, Extent: core.Extent{Len: last.Extent.Len}}.Size()
			if end+format.SealMarkerSize != footer.Offset {
				t.Errorf("segment %d's last entry ends at %d, want it at the marker %d: the seal follows it exactly", id, end, footer.Offset-format.SealMarkerSize)
			}
		}
	case fateEnded:
		if int64(len(file)) != f.Offset {
			t.Fatalf("segment %d's file is %d bytes, want the bare ended cut at %d", id, len(file), f.Offset)
		}
		if _, err := format.ReadFooter(bytes.NewReader(file), f.Offset, journalID, id); err == nil {
			t.Errorf("segment %d's ended cut holds a readable footer, want none: an ended segment takes no seal", id)
		}
	default:
		if !bytes.Equal(file, planted) {
			t.Fatalf("segment %d's bytes changed under a %v fate, want the planted ones", id, f.Kind)
		}
	}
}

// footerOf reads the segment's footer out of its bytes.
func footerOf(t *testing.T, f []byte, id core.SegmentID) format.Footer {
	t.Helper()
	footer, err := format.ReadFooter(bytes.NewReader(f), int64(len(f)), journalID, id)
	if err != nil {
		t.Fatalf("segment %d's footer does not verify: %v", id, err)
	}
	return footer
}
