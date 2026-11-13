package recovery

import (
	"fmt"
	"slices"
	"testing"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/format"
	"github.com/cubbit/dittofs/pkg/block/journal/journaltest/raw"
)

// zeroHeader wipes the segment header slot of b, as a slot nobody wrote reads.
func zeroHeader(b []byte) []byte {
	clear(b[:format.SegmentHeaderSize])
	return b
}

// provenTwo is segment id's two records, the second's synced-through covering
// both, and where they end.
func provenTwo(id core.SegmentID) ([]byte, int64) {
	endA := endAfter(tailRec(200, 0x4100))
	endB := endAfter(tailRec(200, 0x4100), tailRec(201, 0x8100))
	return buildSeg(id, syncedRec(200, 0x4100, endA), syncedRec(201, 0x8100, endB)), endB
}

func TestAZeroHeaderSegmentsRecordsAreScannedAndItIsClassed(t *testing.T) {
	t.Run("unnamed", func(t *testing.T) {
		seg, endB := provenTwo(2)
		r := newRig(t)
		r.writeFormat()
		r.plantSeg(2, zeroHeader(seg))
		st := runUnchanged(t, r, 2)
		if len(st.Attached.ZeroHeader) != 1 {
			t.Fatalf("Attached.ZeroHeader got %v, want the planted segment", st.Attached.ZeroHeader)
		}
		if got := planted(st); got != 2 {
			t.Errorf("the index holds %d entries, want 2: the zero-header segment's records attach", got)
		}
		wantFate(t, st, 2, fateIntact, 0)
		wantDamaged(t, st, 2) // a record's synced-through proves the slot was written
		if sc, _ := scanOf(st, 2); sc.RecordsEnd != endB {
			t.Errorf("the scan's RecordsEnd got %d, want %d", sc.RecordsEnd, endB)
		}
	})
	t.Run("named by a successor", func(t *testing.T) {
		sealed, _ := buildSealed(1, tailRec(200, 0x4100), tailRec(201, 0x8100))
		r := newRig(t)
		r.writeFormat()
		r.plantSeg(1, zeroHeader(sealed))
		r.plantSeg(2, namingSeg(2, 1))
		st := runTails(t, r)
		if got := planted(st); got != 2 {
			t.Errorf("the index holds %d entries, want 2", got)
		}
		wantFate(t, st, 1, fateSealed, 0)
		wantDamaged(t, st, 1)
	})
	t.Run("named and cut to nothing refuses", func(t *testing.T) {
		r := newRig(t)
		r.writeFormat()
		r.plantSeg(1, nil)
		r.plantSeg(2, namingSeg(2, 1))
		wantRefused(t, r, 1, 0, 0)
	})
}

// A spare nobody took, an all-zero file, scans empty and the open succeeds.
func TestAnAllZeroSegmentScansEmpty(t *testing.T) {
	r := newRig(t)
	r.writeFormat()
	r.plantSeg(1, make([]byte, testCap))
	st := runUnchanged(t, r, 1)
	if got := planted(st); got != 0 {
		t.Errorf("the index holds %d entries, want 0", got)
	}
	if sc, ok := scanOf(st, 1); !ok || len(sc.Records) != 0 {
		t.Errorf("the all-zero segment's scan got %d records (found %v), want an empty one", len(sc.Records), ok)
	}
	wantDamaged(t, st)
}

func TestANamedPredecessorCutBelowTheFirstRecordSlotRefuses(t *testing.T) {
	full := buildSeg(1, tailRec(200, 0x4100))
	for _, size := range []int{0, 20, 47, 48} {
		t.Run(fmt.Sprintf("%d bytes", size), func(t *testing.T) {
			r := newRig(t)
			r.writeFormat()
			r.plantSeg(1, slices.Clone(full[:size]))
			r.plantSeg(2, namingSeg(2, 1))
			wantRefused(t, r, 1, int64(size), int64(size))
		})
	}
}

// A seal marker rotted alone leaves its named segment sealed; a clone target
// whose body rotted above every synced-through is still corrupt there, the
// marker never having proven it synced.
func TestANamedSegmentWithARottedMarkerDropsARottedCloneTargetAsCorrupt(t *testing.T) {
	b, _ := buildSealed(1, tailRec(200, 0x4100), cloneRec())
	endA := endAfter(tailRec(200, 0x4100))
	b[endA+format.RecordHeaderSize] ^= 0xff // the clone target's spec
	markerOff := int64(len(b)) - raw.TrailerSize - 2*raw.CatalogEntrySize - format.SealMarkerSize
	b[markerOff+format.SealMarkerSize-1] ^= 0xff
	r := newRig(t)
	r.writeFormat()
	r.plantSeg(1, b)
	r.plantSeg(2, namingSeg(2, 1))
	st := runTails(t, r)
	if len(st.Tails.Corrupt) != 1 || st.Tails.Corrupt[0].Segment != 1 || !st.Tails.Corrupt[0].Drop.Synced ||
		st.Tails.Corrupt[0].Drop.Header.Offset != endA {
		t.Fatalf("Corrupt got %+v, want the clone target at %d of segment 1, synced", st.Tails.Corrupt, endA)
	}
	wantFate(t, st, 1, fateSealed, 0)
	if !slices.Equal(st.Tails.Damaged, []core.SegmentID{1}) {
		t.Errorf("Damaged got %v, want [1]", st.Tails.Damaged)
	}
}

func TestAnUnverifiedHeaderIsDamagedFromTheFirstRecordSlotOn(t *testing.T) {
	for _, row := range []struct {
		name    string
		synced  int64
		damaged bool
	}{
		{"a sync that reached the first record slot", format.SegmentHeaderSize, true},
		{"a sync one byte short of it", format.SegmentHeaderSize - 1, false},
	} {
		t.Run(row.name, func(t *testing.T) {
			r := newRig(t)
			r.writeFormat()
			r.plantSeg(1, damageHeader(buildSeg(1, syncedRec(200, 0x4100, row.synced))))
			st := runUnchanged(t, r, 1)
			if row.damaged {
				wantFate(t, st, 1, fateIntact, 0)
				wantDamaged(t, st, 1)
			} else {
				wantFate(t, st, 1, fateEnded, 0)
				wantDamaged(t, st)
			}
		})
	}
}

// A verifying record may name a synced-through past the file's end. It proves
// every byte the file holds, so a record cut short in it is refused as
// corruption, never cut as a torn tail at, or past, the file's size.
func TestASyncedThroughPastTheFileEndCoversAnyCutRecord(t *testing.T) {
	endA := endAfter(tailRec(200, 0x4100))
	full := buildSeg(1, syncedRec(200, 0x4100, endA+1000), tailRec(201, 0x8100))
	cut := full[:len(full)-5]
	r := newRig(t)
	r.writeFormat()
	r.plantSeg(1, cut)
	wantRefused(t, r, 1, endA, int64(len(cut)))
}
