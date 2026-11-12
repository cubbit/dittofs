package recovery

import (
	"bytes"
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/format"
	"github.com/cubbit/dittofs/pkg/block/journal/journaltest/raw"
)

// cloneRec is a clone target record whose body verifies until a test rots it.
func cloneRec() raw.Record {
	return rec(7, func(r *raw.Record) {
		r.Clone = &raw.CloneBody{Share: srcShare(), File: srcFile(), Off: 0x101, AsOf: raw.Version{Counter: 9}}
	})
}

// cloneFile is the file cloneRec writes.
func cloneFile() core.FileID { return core.NewFileID(cloneRec().FileID) }

// wantNoCorrupt checks that no drop is reported.
func wantNoCorrupt(t *testing.T, st State) {
	t.Helper()
	if len(st.Tails.Corrupt) != 0 {
		t.Errorf("Corrupt got %+v, want none: the record was never proven synced", st.Tails.Corrupt)
	}
}

func TestATornCloneTargetAtTheNewestTailIsATornTail(t *testing.T) {
	endA := endAfter(tailRec(200, 0x4100))
	full := buildSeg(1, tailRec(200, 0x4100), cloneRec())
	rotted := slices.Clone(full)
	rotted[endA+format.RecordHeaderSize] ^= 0xff // the body's bytes are there but never verified
	for _, row := range []struct {
		name string
		seg  []byte
	}{
		// The walk cannot read a record past the file's end: a tail range, no drop.
		{"the crash cut the body", full[:len(full)-10]},
		// The header verifies and the whole body is there: a scanned drop.
		{"the body's bytes are garbage", rotted},
	} {
		t.Run(row.name, func(t *testing.T) {
			r := newRig(t)
			r.writeFormat()
			r.plantSeg(1, row.seg)
			st := runUnchanged(t, r, 1)

			if got := len(st.Rebuilt.Index.FileEntries(cloneFile())); got != 0 {
				t.Errorf("the index holds %d clone target entries, want 0: the body never verified", got)
			}
			if got := planted(st); got != 1 {
				t.Errorf("the index holds %d write entries, want 1: A attaches", got)
			}
			wantFate(t, st, 1, fateTornTail, endA)
			wantDamaged(t, st)
			wantNoCorrupt(t, st)
		})
	}
}

func TestASyncedCloneTargetWhoseBodyRotsIsACorruptDrop(t *testing.T) {
	endA := endAfter(tailRec(200, 0x4100))
	endB := endAfter(tailRec(200, 0x4100), cloneRec())
	seg := buildSeg(1, tailRec(200, 0x4100), cloneRec(), syncedRec(201, 0x8100, endB))
	seg[endA+format.RecordHeaderSize] ^= 0xff // B's spec
	r := newRig(t)
	r.writeFormat()
	r.plantSeg(1, seg)
	st := runUnchanged(t, r, 1)

	if got := len(st.Rebuilt.Index.FileEntries(cloneFile())); got != 0 {
		t.Errorf("the index holds %d clone target entries, want 0", got)
	}
	if got := planted(st); got != 2 {
		t.Errorf("the index holds %d write entries, want 2: A and C attach", got)
	}
	if len(st.Tails.Corrupt) != 1 {
		t.Fatalf("Corrupt got %+v, want exactly the clone target", st.Tails.Corrupt)
	}
	c := st.Tails.Corrupt[0]
	if c.Segment != 1 || !c.Drop.Synced || c.Drop.Header.Offset != endA || !errors.Is(c.Drop.Err, core.ErrCorrupt) {
		t.Errorf("Corrupt[0] got segment %d synced %v offset %d err %v, want segment 1, synced, offset %d, ErrCorrupt",
			c.Segment, c.Drop.Synced, c.Drop.Header.Offset, c.Drop.Err, endA)
	}
	if !slices.Equal(st.Tails.Damaged, []core.SegmentID{1}) {
		t.Errorf("Damaged got %v, want [1]", st.Tails.Damaged)
	}
	wantFate(t, st, 1, fateIntact, 0)
}

func TestAnUnprovenTornCloneTargetBeforeARecordEndsTheSegment(t *testing.T) {
	endA := endAfter(tailRec(200, 0x4100))
	seg := buildSeg(1, tailRec(200, 0x4100), cloneRec(), tailRec(201, 0x8100))
	seg[endA+format.RecordHeaderSize] ^= 0xff // B's spec; C's synced-through names nothing
	r := newRig(t)
	r.writeFormat()
	r.plantSeg(1, seg)
	st := runUnchanged(t, r, 1)

	if got := planted(st); got != 2 {
		t.Errorf("the index holds %d write entries, want 2: A and C attach", got)
	}
	wantFate(t, st, 1, fateEnded, 0)
	wantDamaged(t, st)
	wantNoCorrupt(t, st)
}

// TestACloneTargetBodyIsClassedTheSameWhereverItIsRead holds that the open's
// two ways of finding a failing clone target, the sealed segment's catalogue
// and the header walk, report the same drop: synced, in the same segment, the
// same record, the same error class.
func TestACloneTargetBodyIsClassedTheSameWhereverItIsRead(t *testing.T) {
	b, _ := buildSealed(1, cloneRec(), tailRec(201, 0x8100))
	b[format.SegmentHeaderSize+format.RecordHeaderSize] ^= 0xff // the clone target's spec

	var got [2]corruptRecord
	for i, forceScan := range []bool{false, true} {
		r := newRig(t)
		r.writeFormat()
		r.plantSeg(1, b)
		r.plantSeg(2, namingSeg(2, 1))
		deps := r.runDeps()
		deps.rebuildForceScan = forceScan
		st, err := Run(context.Background(), deps, core.Version{}, journalID, nil)
		if err != nil {
			t.Fatalf("Run (forced scan %v): %v", forceScan, err)
		}
		if len(st.Tails.Corrupt) != 1 || !slices.Equal(st.Tails.Damaged, []core.SegmentID{1}) {
			t.Fatalf("Run (forced scan %v): Corrupt %+v Damaged %v, want one drop and segment 1 damaged", forceScan, st.Tails.Corrupt, st.Tails.Damaged)
		}
		got[i] = st.Tails.Corrupt[0]
	}
	a, s := got[0], got[1]
	if a.Segment != s.Segment || a.Drop.Synced != s.Drop.Synced || !a.Drop.Synced ||
		a.Drop.Header.Offset != s.Drop.Header.Offset || a.Drop.Header.Seq != s.Drop.Header.Seq ||
		errors.Is(a.Drop.Err, core.ErrCorrupt) != errors.Is(s.Drop.Err, core.ErrCorrupt) {
		t.Errorf("the catalogued drop %+v and the scanned drop %+v differ, want one corruptRecord shape", a, s)
	}
}

// TestClassifyTailsVisitsTheScansByAscendingID hands the classing two
// corrupt ranges in descending order and holds that the refusal names the
// lowest segment: a directory listing never comes back descending, so the
// scans are built here.
func TestClassifyTailsVisitsTheScansByAscendingID(t *testing.T) {
	scan := func(id core.SegmentID) segmentScan {
		return segmentScan{ID: id, Sealed: true, RecordsEnd: 200, Size: 300,
			Ranges: []format.ScanRange{{Off: 120, End: 140}}}
	}
	// Each segment is named by a successor, so no footer is read.
	var named Attached
	for i, pred := range []core.SegmentID{9, 4, 7} {
		named.Own = append(named.Own, SegmentInfo{ID: core.SegmentID(20 + i),
			Header: format.SegmentHeader{Predecessor: pred}})
	}
	_, err := classifyTails(nil, journalID, named, []segmentScan{scan(9), scan(4), scan(7)})
	if !errors.Is(err, core.ErrNotImplemented) {
		t.Fatalf("classifyTails got err %v, want one wrapping core.ErrNotImplemented", err)
	}
	if want := "segment 4:"; !contains(err.Error(), want) {
		t.Errorf("classifyTails refused with %q, want it to name the lowest segment (%q)", err.Error(), want)
	}
}

// TestTwoCorruptCloneTargetDropsComeInAscendingRecordOffset holds the order of
// Tails.Corrupt for scanned drops, whose entry is zero: the record's own
// header offset orders them. The Run leg plants two rotted bodies a later
// record's synced-through covers; the direct leg hands the classing the same
// drops in descending order, since a walk only ever yields them ascending.
func TestTwoCorruptCloneTargetDropsComeInAscendingRecordOffset(t *testing.T) {
	endA := endAfter(tailRec(200, 0x4100))
	endB := endAfter(tailRec(200, 0x4100), cloneRec())
	endC := endAfter(tailRec(200, 0x4100), cloneRec(), cloneRec())
	seg := buildSeg(1, tailRec(200, 0x4100), cloneRec(), cloneRec(), syncedRec(201, 0x8100, endC))
	seg[endA+format.RecordHeaderSize] ^= 0xff // B's spec
	seg[endB+format.RecordHeaderSize] ^= 0xff // C's spec
	r := newRig(t)
	r.writeFormat()
	r.plantSeg(1, seg)
	st := runUnchanged(t, r, 1)

	offsets := func(cs []corruptRecord) []int64 {
		var o []int64
		for _, c := range cs {
			if !c.Drop.Synced {
				t.Errorf("a drop at %d is not synced, want both covered by the later record's proof", c.Drop.Header.Offset)
			}
			o = append(o, c.Drop.Header.Offset)
		}
		return o
	}
	want := []int64{endA, endB}
	if got := offsets(st.Tails.Corrupt); !slices.Equal(got, want) {
		t.Errorf("Run's Corrupt offsets got %v, want %v", got, want)
	}
	if !slices.Equal(st.Tails.Damaged, []core.SegmentID{1}) {
		t.Errorf("Damaged got %v, want [1] once", st.Tails.Damaged)
	}

	sc, ok := scanOf(st, 1)
	if !ok || len(sc.Dropped) != 2 {
		t.Fatalf("the scan holds %d drops, want 2", len(sc.Dropped))
	}
	slices.Reverse(sc.Dropped)
	got, err := classifyTails(nil, journalID, Attached{}, []segmentScan{sc})
	if err != nil {
		t.Fatalf("classifyTails: %v", err)
	}
	if o := offsets(got.Corrupt); !slices.Equal(o, want) {
		t.Errorf("classifyTails over descending drops got Corrupt offsets %v, want %v", o, want)
	}
}

// TestATornSlotDropDoesNotMoveTheTailStart holds that the tail's start is the
// end of the last verifying record when a torn slot, a dropped clone target
// below that record, precedes it: the slot's own offset never lowers the cut.
func TestATornSlotDropDoesNotMoveTheTailStart(t *testing.T) {
	endA := endAfter(tailRec(200, 0x4100))
	seg := buildSeg(1, tailRec(200, 0x4100), cloneRec(), tailRec(201, 0x8100))
	seg[endA+format.RecordHeaderSize] ^= 0xff // B's spec; nothing proves B
	end := int64(len(seg))
	seg = append(seg, bytes.Repeat([]byte{0xff}, 16)...)
	r := newRig(t)
	r.writeFormat()
	r.plantSeg(1, seg)
	st := runUnchanged(t, r, 1)

	wantFate(t, st, 1, fateEnded, end)
	wantDamaged(t, st)
	wantNoCorrupt(t, st)
}

// TestAZeroGapThenACloneTargetWhoseBodyFailedIsOneTornTailFromTheLastVerifyingRecord
// plants A, a zero gap, then a clone target whose header verifies and body
// does not, to the file's end: the target is not a verifying record, so the
// tail runs from the end of A.
func TestAZeroGapThenACloneTargetWhoseBodyFailedIsOneTornTailFromTheLastVerifyingRecord(t *testing.T) {
	endA := endAfter(tailRec(200, 0x4100))
	seg := buildSeg(1, tailRec(200, 0x4100))
	seg = append(seg, make([]byte, 16)...)
	c := cloneRec()
	c.Identity, c.Segment, c.RecordOffset = journalID, 1, uint32(len(seg))
	at := len(seg)
	seg = append(seg, c.Bytes()...)
	seg[at+format.RecordHeaderSize] ^= 0xff // the body's bytes never verify
	r := newRig(t)
	r.writeFormat()
	r.plantSeg(1, seg)
	st := runUnchanged(t, r, 1)
	wantFate(t, st, 1, fateTornTail, endA)
	wantDamaged(t, st)
	wantNoCorrupt(t, st)
}
