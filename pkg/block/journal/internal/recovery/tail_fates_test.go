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

// A's payload rots under a header that verifies. The open reads no content
// payload and verifies only past the synced-through, so no open phase
// reports A: the read that would serve it catches it, through the corrupt
// path that raises the loss event and drops the extent. That split between
// the open's scan and the serving read is the format's own, so A stays
// indexed and the segment intact.
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

// slotSeg is segment id holding A, a 16-byte zero slot and R whose
// synced-through is rSync(slot), the slot's offset.
func slotSeg(id core.SegmentID, rSync func(slot int64) int64) []byte {
	endA := endAfter(tailRec(200, 0x4100))
	seg := buildSeg(id, syncedRec(200, 0x4100, endA))
	seg = append(seg, make([]byte, 16)...)
	rr := syncedRec(201, 0x8100, rSync(endA))
	rr.Identity, rr.Segment, rr.RecordOffset = journalID, uint64(id), uint32(len(seg))
	return append(seg, rr.Bytes()...)
}

// slotRig plants slotSeg as segment 2 and runs, checking Run cut nothing.
func slotRig(t *testing.T, rSync func(slot int64) int64) State {
	t.Helper()
	r := newRig(t)
	r.writeFormat()
	r.plantSeg(2, slotSeg(2, rSync))
	return runUnchanged(t, r, 2)
}

// runUnchanged runs and checks the segment's bytes are as planted.
func runUnchanged(t *testing.T, r *rig, id core.SegmentID) State {
	t.Helper()
	before := bytes.Clone(r.fileBytes(format.SegmentName(id)))
	st := runTails(t, r)
	if !bytes.Equal(before, r.fileBytes(format.SegmentName(id))) {
		t.Errorf("Run changed segment %d, want the directory unchanged", id)
	}
	return st
}

func wantDamaged(t *testing.T, st State, want ...core.SegmentID) {
	t.Helper()
	if !slices.Equal(st.Tails.Damaged, want) {
		t.Errorf("Damaged got %v, want %v", st.Tails.Damaged, want)
	}
	if len(st.Tails.Corrupt) != 0 {
		t.Errorf("Corrupt got %+v, want none: a refused range is not a dropped record", st.Tails.Corrupt)
	}
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
	t.Run("the header alone", func(t *testing.T) {
		seg := buildSeg(1, tailRec(200, 0x4100), tailRec(201, 0x8100))
		r := newRig(t)
		r.writeFormat()
		r.plantSeg(1, damageHeader(seg))
		st := runTails(t, r)

		if got := planted(st); got != 2 {
			t.Errorf("the index holds %d entries, want 2", got)
		}
		wantFate(t, st, 1, fateEnded, 0)
		wantDamaged(t, st)
	})
	t.Run("before a torn slot", func(t *testing.T) {
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
		wantDamaged(t, st)
	})
}

func TestTailsClassifyEverySegmentFate(t *testing.T) {
	r := newRig(t)
	r.writeFormat()
	named, _ := buildSealed(1, tailRec(200, 0x4100))
	unnamed, _ := buildSealed(3, tailRec(201, 0x8100))
	r.plantSeg(1, named)
	plantTake(r, 2, 1)
	r.plantSeg(3, unnamed)
	st := runTails(t, r)

	wantFate(t, st, 1, fateSealed, 0)
	wantFate(t, st, 3, fateSealToProve, 0)
	if sc, _ := scanOf(st, 2); sc.RecordsEnd != format.SegmentHeaderSize {
		t.Errorf("the empty segment's RecordsEnd got %d, want %d", sc.RecordsEnd, format.SegmentHeaderSize)
	}
	// Segment 2 is a record-less take, so the spares phase re-adopted it:
	// its fate is nobody's to hold, and only the two sealed lives keep one.
	if got, want := st.Spares.Adopted, []AdoptedSpare{{ID: 2}}; !slices.Equal(got, want) {
		t.Errorf("the re-adopted spares got %v, want %v", got, want)
	}
	if len(st.Tails.Fates) != 2 || len(st.Tails.Damaged) != 0 {
		t.Errorf("Tails got %+v, want two fates and nothing damaged", st.Tails)
	}
}

func TestTheTopologyIgnoresNamesOfSegmentsNotOnDisk(t *testing.T) {
	r := newRig(t)
	r.writeFormat()
	r.plantSeg(5, buildSeg(5, tailRec(200, 0x4100)))
	plantTake(r, 6, 99)
	st := runTails(t, r)

	wantFate(t, st, 5, fateIntact, 0)
	// Segment 6's claim of a predecessor no directory holds seals nothing,
	// and its record-less take is the spares phase's: re-adopted, so it
	// keeps no fate.
	if got, want := st.Spares.Adopted, []AdoptedSpare{{ID: 6}}; !slices.Equal(got, want) {
		t.Errorf("the re-adopted spares got %v, want %v", got, want)
	}
}

func TestTwoSuccessorsMayNameOnePredecessor(t *testing.T) {
	r := newRig(t)
	r.writeFormat()
	sealed, _ := buildSealed(1, tailRec(200, 0x4100))
	r.plantSeg(1, sealed)
	plantTake(r, 2, 1)
	plantTake(r, 3, 1)
	st := runTails(t, r)

	wantFate(t, st, 1, fateSealed, 0)
	// Both successors are record-less takes, so the spares phase re-adopted
	// both: the predecessor's fate stays the one their headers named.
	if got, want := st.Spares.Adopted, []AdoptedSpare{{ID: 2}, {ID: 3}}; !slices.Equal(got, want) {
		t.Errorf("the re-adopted spares got %v, want %v", got, want)
	}
}

func TestAZeroSlotAProofCoversRefusesTheOpen(t *testing.T) {
	endA := endAfter(tailRec(200, 0x4100))
	r := newRig(t)
	r.writeFormat()
	r.plantSeg(2, slotSeg(2, func(slot int64) int64 { return slot + 1 }))
	wantRefused(t, r, 2, endA, endA+16)
}

func TestARefusedRangeInANamedSegmentRefusesTheOpen(t *testing.T) {
	endA := endAfter(tailRec(200, 0x4100))
	r := newRig(t)
	r.writeFormat()
	// R's synced-through is below the slot: unnamed, this slot is torn.
	r.plantSeg(1, slotSeg(1, func(slot int64) int64 { return slot - 1 }))
	r.plantSeg(2, raw.SegmentHeader{Identity: journalID, ID: 2, Predecessor: 1}.Bytes())
	wantRefused(t, r, 1, endA, endA+16)
}

func TestARefusedRangeInASealedSegmentRefusesTheOpen(t *testing.T) {
	endA := endAfter(tailRec(200, 0x4100))
	seg := slotSeg(1, func(slot int64) int64 { return slot - 1 })
	seg = append(seg, raw.SealMarker{Identity: journalID, Segment: 1, Offset: uint64(len(seg))}.Bytes()...)
	r := newRig(t)
	r.writeFormat()
	r.plantSeg(1, seg)
	wantRefused(t, r, 1, endA, endA+16)
}

func TestATornSlotAndATailRangeEndAtTheLastRecord(t *testing.T) {
	seg := slotSeg(1, func(slot int64) int64 { return slot - 1 })
	end := int64(len(seg))
	seg = append(seg, bytes.Repeat([]byte{0xff}, 16)...)
	r := newRig(t)
	r.writeFormat()
	r.plantSeg(1, seg)
	st := runUnchanged(t, r, 1)
	wantDamaged(t, st)
	wantFate(t, st, 1, fateEnded, end)
}

// TestACataloguedDropIsCorruptAndItsSegmentDamaged confines one record by a
// clone-target entry whose seq its header contradicts: the drop carries into
// Tails.Corrupt as synced and the segment is damaged, though the open succeeds.
func TestACataloguedDropIsCorruptAndItsSegmentDamaged(t *testing.T) {
	clone := rec(7, func(r *raw.Record) {
		r.Clone = &raw.CloneBody{Share: srcShare(), File: srcFile(), Off: 0x101, AsOf: raw.Version{Counter: 9}}
	})
	b, ents := buildSealed(1, clone, tailRec(201, 0x8100))
	es := slices.Clone(ents)
	es[0].Seq++
	markerOff := len(b) - raw.TrailerSize - raw.CatalogEntrySize*len(es) - format.SealMarkerSize
	b = appendSealed(b[:markerOff], 1, es)

	r := newRig(t)
	r.writeFormat()
	r.plantSeg(1, b)
	r.plantSeg(2, raw.SegmentHeader{Identity: journalID, ID: 2, Predecessor: 1}.Bytes())
	st := runTails(t, r)

	if len(st.Tails.Corrupt) != 1 {
		t.Fatalf("Corrupt got %+v, want the one confined record", st.Tails.Corrupt)
	}
	c := st.Tails.Corrupt[0]
	if c.Segment != 1 || !c.Drop.Synced || c.Drop.Entry != es[0] {
		t.Errorf("Corrupt[0] got segment %d synced %v entry %+v, want segment 1 synced true entry %+v", c.Segment, c.Drop.Synced, c.Drop.Entry, es[0])
	}
	if !slices.Equal(st.Tails.Damaged, []core.SegmentID{1}) {
		t.Errorf("Damaged got %v, want [1]", st.Tails.Damaged)
	}
}
