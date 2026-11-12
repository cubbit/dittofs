package recovery

import (
	"bytes"
	"context"
	"slices"
	"testing"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/format"
	"github.com/cubbit/dittofs/pkg/block/journal/journaltest/raw"
)

// syncedRec is a tailRec that names synced as the offset a sync had completed.
func syncedRec(counter, off uint64, synced int64) raw.Record {
	r := tailRec(counter, off)
	r.SyncedThrough = uint64(synced)
	return r
}

// endAfter is the offset where the records of a segment built from recs end.
func endAfter(recs ...raw.Record) int64 { return int64(len(buildSeg(1, recs...))) }

// fateOf returns the fate the classing gave segment id.
func fateOf(t *testing.T, st State, id core.SegmentID) segmentFate {
	t.Helper()
	for _, f := range st.Tails.Fates {
		if f.ID == id {
			return f
		}
	}
	t.Fatalf("Tails.Fates %+v holds no fate of segment %d", st.Tails.Fates, id)
	return segmentFate{}
}

// wantFate checks segment id's fate kind and offset.
func wantFate(t *testing.T, st State, id core.SegmentID, kind fateKind, off int64) {
	t.Helper()
	if f := fateOf(t, st, id); f.Kind != kind || f.Offset != off {
		t.Errorf("segment %d's fate got %v at %d, want %v at %d", id, f.Kind, f.Offset, kind, off)
	}
}

func runTails(t *testing.T, r *rig) State {
	t.Helper()
	st, err := Run(context.Background(), r.runDeps(), core.Version{}, journalID, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	return st
}

func planted(st State) int { return len(st.Rebuilt.Index.FileEntries(tailFile())) }

func TestSyncedDamageIsNotATornTailPayloadLeg(t *testing.T) {
	endA := endAfter(tailRec(200, 0x4100))
	endB := endAfter(tailRec(200, 0x4100), tailRec(201, 0x8100))
	seg := buildSeg(2, syncedRec(200, 0x4100, endA), syncedRec(201, 0x8100, endB), tailRec(202, 0xc100))
	seg[format.SegmentHeaderSize+format.RecordHeaderSize+8+2] ^= 0xff // A's payload

	r := newRig(t)
	r.writeFormat()
	r.plantSeg(2, seg)
	st := runTails(t, r)

	if got := planted(st); got != 3 {
		t.Errorf("the index holds %d entries, want 3: A, B and C attach", got)
	}
	wantFate(t, st, 2, fateIntact, 0)
	if len(st.Tails.Damaged) != 0 {
		t.Errorf("Damaged got %v, want none: a payload the read catches is not the tail's", st.Tails.Damaged)
	}
}

// slotRig plants A, a 16-byte zero slot and R whose synced-through is rSync
// (a function of the slot's offset).
func slotRig(t *testing.T, rSync func(slot int64) int64) State {
	t.Helper()
	endA := endAfter(tailRec(200, 0x4100))
	slot := endA
	seg := buildSeg(2, syncedRec(200, 0x4100, endA))
	seg = append(seg, make([]byte, 16)...)
	rr := syncedRec(201, 0x8100, rSync(slot))
	rr.Identity, rr.Segment, rr.RecordOffset = journalID, 2, uint32(len(seg))
	seg = append(seg, rr.Bytes()...)

	r := newRig(t)
	r.writeFormat()
	r.plantSeg(2, seg)
	before := bytes.Clone(r.fileBytes(format.SegmentName(2)))
	st := runTails(t, r)
	if !bytes.Equal(before, r.fileBytes(format.SegmentName(2))) {
		t.Errorf("Run changed the segment, want the directory unchanged")
	}
	return st
}

func TestARecordAfterATornSlotIsAttached(t *testing.T) {
	st := slotRig(t, func(slot int64) int64 { return slot })
	if got := planted(st); got != 2 {
		t.Errorf("the index holds %d entries, want 2: A and R attach", got)
	}
	wantFate(t, st, 2, fateEnded, 0)
	if len(st.Tails.Damaged) != 0 {
		t.Errorf("Damaged got %v, want none", st.Tails.Damaged)
	}
}

func TestAZeroSlotTheSegmentLacksProofForIsNeverWritten(t *testing.T) {
	st := slotRig(t, func(slot int64) int64 { return slot - 1 })
	wantFate(t, st, 2, fateEnded, 0)
	if len(st.Tails.Damaged) != 0 {
		t.Errorf("Damaged got %v, want none: the slot was never proven", st.Tails.Damaged)
	}
}

func TestTheNewestSegmentTruncatedMidRecordIsATornTail(t *testing.T) {
	endA := endAfter(tailRec(200, 0x4100))
	full := buildSeg(2, tailRec(200, 0x4100), tailRec(201, 0x8100))
	r := newRig(t)
	r.writeFormat()
	r.plantSeg(2, full[:len(full)-5])
	st := runTails(t, r)

	if got := planted(st); got != 1 {
		t.Errorf("the index holds %d entries, want 1: only A verifies", got)
	}
	wantFate(t, st, 2, fateTornTail, endA)
	if len(st.Tails.Damaged) != 0 {
		t.Errorf("Damaged got %v, want none", st.Tails.Damaged)
	}
}

func TestARottenSealMarkerNothingNamesIsATornTail(t *testing.T) {
	seg := buildSeg(2, tailRec(200, 0x4100), tailRec(201, 0x8100))
	end := int64(len(seg))
	m := raw.SealMarker{Identity: journalID, Segment: 2, Offset: uint64(end)}.Bytes()
	m[31] ^= 0xff
	seg = append(append(seg, m...), make([]byte, 128)...)

	r := newRig(t)
	r.writeFormat()
	r.plantSeg(2, seg)
	st := runTails(t, r)

	wantFate(t, st, 2, fateTornTail, end)
	if len(st.Tails.Damaged) != 0 {
		t.Errorf("Damaged got %v, want none: the marker is the tail's first bytes", st.Tails.Damaged)
	}
}

func TestATornSegmentHeaderIsDamageWhenASealMarkerFollowsIt(t *testing.T) {
	sealed, _ := buildSealed(1, tailRec(200, 0x4100), tailRec(201, 0x8100))

	t.Run("unnamed", func(t *testing.T) {
		r := newRig(t)
		r.writeFormat()
		r.plantSeg(1, damageHeader(bytes.Clone(sealed)))
		st := runTails(t, r)
		if got := planted(st); got != 2 {
			t.Errorf("the index holds %d entries, want 2", got)
		}
		wantFate(t, st, 1, fateSealToProve, 0)
		if !slices.Equal(st.Tails.Damaged, []core.SegmentID{1}) {
			t.Errorf("Damaged got %v, want [1]", st.Tails.Damaged)
		}
	})
	t.Run("named", func(t *testing.T) {
		r := newRig(t)
		r.writeFormat()
		r.plantSeg(1, damageHeader(bytes.Clone(sealed)))
		r.plantSeg(2, raw.SegmentHeader{Identity: journalID, ID: 2, Predecessor: 1}.Bytes())
		st := runTails(t, r)
		if got := planted(st); got != 2 {
			t.Errorf("the index holds %d entries, want 2", got)
		}
		wantFate(t, st, 1, fateSealed, 0)
		if !slices.Equal(st.Tails.Damaged, []core.SegmentID{1}) {
			t.Errorf("Damaged got %v, want [1]: a named segment is damaged, never torn-open", st.Tails.Damaged)
		}
	})
}

func TestATornSegmentHeaderEndsItsSegment(t *testing.T) {
	seg := buildSeg(1, tailRec(200, 0x4100))
	seg = append(seg, make([]byte, 16)...)
	rr := tailRec(201, 0x8100)
	rr.Identity, rr.Segment, rr.RecordOffset = journalID, 1, uint32(len(seg))
	seg = append(seg, rr.Bytes()...)
	r := newRig(t)
	r.writeFormat()
	r.plantSeg(1, damageHeader(seg))
	st := runTails(t, r)

	if got := planted(st); got != 2 {
		t.Errorf("the index holds %d entries, want 2: the record after the slot attaches", got)
	}
	wantFate(t, st, 1, fateEnded, 0)
	if len(st.Tails.Damaged) != 0 {
		t.Errorf("Damaged got %v, want none: nothing proves the header slot", st.Tails.Damaged)
	}
}

func TestTailsClassifyEverySegmentFate(t *testing.T) {
	r := newRig(t)
	r.writeFormat()
	named, _ := buildSealed(1, tailRec(200, 0x4100))
	unnamed, _ := buildSealed(3, tailRec(201, 0x8100))
	r.plantSeg(1, named)
	r.plantSeg(2, raw.SegmentHeader{Identity: journalID, ID: 2, Predecessor: 1}.Bytes())
	r.plantSeg(3, unnamed)
	st := runTails(t, r)

	wantFate(t, st, 1, fateSealed, 0)
	wantFate(t, st, 2, fateIntact, 0)
	wantFate(t, st, 3, fateSealToProve, 0)
	if sc, _ := scanOf(st, 2); sc.RecordsEnd != format.SegmentHeaderSize {
		t.Errorf("the empty segment's RecordsEnd got %d, want %d", sc.RecordsEnd, format.SegmentHeaderSize)
	}
	if len(st.Tails.Fates) != 3 || len(st.Tails.Damaged) != 0 {
		t.Errorf("Tails got %+v, want three fates and nothing damaged", st.Tails)
	}
}

func TestTheTopologyIgnoresNamesOfSegmentsNotOnDisk(t *testing.T) {
	r := newRig(t)
	r.writeFormat()
	r.plantSeg(5, buildSeg(5, tailRec(200, 0x4100)))
	r.plantSeg(6, raw.SegmentHeader{Identity: journalID, ID: 6, Predecessor: 99}.Bytes())
	st := runTails(t, r)

	wantFate(t, st, 5, fateIntact, 0)
	wantFate(t, st, 6, fateIntact, 0)
}

func TestTwoSuccessorsMayNameOnePredecessor(t *testing.T) {
	r := newRig(t)
	r.writeFormat()
	sealed, _ := buildSealed(1, tailRec(200, 0x4100))
	r.plantSeg(1, sealed)
	r.plantSeg(2, raw.SegmentHeader{Identity: journalID, ID: 2, Predecessor: 1}.Bytes())
	r.plantSeg(3, raw.SegmentHeader{Identity: journalID, ID: 3, Predecessor: 1}.Bytes())
	st := runTails(t, r)

	wantFate(t, st, 1, fateSealed, 0)
	wantFate(t, st, 2, fateIntact, 0)
	wantFate(t, st, 3, fateIntact, 0)
}
