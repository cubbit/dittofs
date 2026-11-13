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

// A segment's header is written before its first record, so every proof a
// record can carry needs the header durable first: a zeroed header hides no
// synced record and the rebuild scans none. A spare nobody took, all zeros at
// the segment cap, is the usual one, and reading it whole at every open would
// cost the pool's whole size.
func TestAZeroHeaderSegmentIsNeverScanned(t *testing.T) {
	t.Run("an all-zero spare", func(t *testing.T) {
		r := newRig(t)
		r.writeFormat()
		r.plantSeg(1, buildSeg(1, tailRec(200, 0x4100)))
		r.plantSeg(2, make([]byte, 1<<16))
		st := runUnchanged(t, r, 2)
		if len(st.Attached.ZeroHeader) != 1 || st.Attached.ZeroHeader[0] != 2 {
			t.Fatalf("Attached.ZeroHeader got %v, want [2]", st.Attached.ZeroHeader)
		}
		if sc, ok := scanOf(st, 2); ok {
			t.Errorf("Rebuilt.Scans holds a scan of the all-zero spare (%+v), want none", sc)
		}
		if got := planted(st); got != 1 {
			t.Errorf("the index holds %d entries, want segment 1's 1", got)
		}
		wantFate(t, st, 1, fateIntact, 0)
		for _, f := range st.Tails.Fates {
			if f.ID == 2 {
				t.Errorf("Tails.Fates holds %+v for the spare, want none: the spare phase owns it", f)
			}
		}
		wantDamaged(t, st)
	})
	t.Run("spares add no scans", func(t *testing.T) {
		const spares = 6
		r := newRig(t)
		r.writeFormat()
		r.plantSeg(1, buildSeg(1, tailRec(200, 0x4100)))
		for id := core.SegmentID(2); id < 2+spares; id++ {
			r.plantSeg(id, make([]byte, 1<<16))
		}
		st := runTails(t, r)
		if len(st.Attached.ZeroHeader) != spares {
			t.Fatalf("Attached.ZeroHeader got %v, want %d spares", st.Attached.ZeroHeader, spares)
		}
		if got := len(st.Rebuilt.Scans); got != 1 {
			t.Errorf("Rebuilt.Scans holds %d scans, want 1: the %d spares are never scanned", got, spares)
		}
	})
}

// A named predecessor whose header slot reads zero is scanned: the successor
// proves it was sealed, so a cut one refuses as a missing end and a whole one
// attaches its records on their own checksums, damaged for the lost header.
func TestANamedZeroHeaderPredecessorIsScanned(t *testing.T) {
	sealed, _ := buildSealed(1, tailRec(200, 0x4100), tailRec(201, 0x8100))
	for _, row := range []struct {
		name string
		seg  []byte
	}{
		{"cut to nothing", nil},
		{"cut to 20 bytes", make([]byte, 20)},
	} {
		t.Run(row.name+" refuses", func(t *testing.T) {
			r := newRig(t)
			r.writeFormat()
			r.plantSeg(1, row.seg)
			r.plantSeg(2, namingSeg(2, 1))
			wantRefused(t, r, 1, 0, int64(len(row.seg)))
		})
	}
	t.Run("a whole sealed life behind the zeroed slot", func(t *testing.T) {
		r := newRig(t)
		r.writeFormat()
		r.plantSeg(1, zeroHeader(slices.Clone(sealed)))
		r.plantSeg(2, namingSeg(2, 1))
		st := runTails(t, r)
		if got := planted(st); got != 2 {
			t.Errorf("the index holds %d entries, want 2: the records attach on their own checksums", got)
		}
		wantFate(t, st, 1, fateSealed, 0)
		wantDamaged(t, st, 1)
	})
}

// A named predecessor with a header that does not verify, cut at or below its
// first record slot, refuses with the whole file as its missing end: no record
// can have survived, so the range starts at 0, never an empty one at the cut.
func TestANamedPredecessorCutBelowTheFirstRecordSlotRefuses(t *testing.T) {
	full := buildSeg(1, tailRec(200, 0x4100))
	for _, size := range []int{20, 47, 48} {
		t.Run(fmt.Sprintf("%d bytes", size), func(t *testing.T) {
			r := newRig(t)
			r.writeFormat()
			r.plantSeg(1, slices.Clone(full[:size]))
			r.plantSeg(2, namingSeg(2, 1))
			wantRefused(t, r, 1, 0, int64(size))
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
