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

// A zeroed header slot is a header that does not verify, and a segment is a
// spare only where no record header verifies anywhere in it: the records
// behind the slot attach on their own checksums, named by a successor or not,
// and a verifying record's synced-through past the slot makes it damage.
func TestAnUnnamedZeroHeaderSegmentsRecordsAreScanned(t *testing.T) {
	seg, endB := provenTwo(2)
	r := newRig(t)
	r.writeFormat()
	r.plantSeg(2, zeroHeader(seg))
	st := runUnchanged(t, r, 2)
	if len(st.Attached.ZeroHeader) != 1 || st.Attached.ZeroHeader[0] != 2 {
		t.Fatalf("Attached.ZeroHeader got %v, want [2]", st.Attached.ZeroHeader)
	}
	if got := planted(st); got != 2 {
		t.Errorf("the index holds %d entries, want 2: the zero-header segment's records attach", got)
	}
	if sc, ok := scanOf(st, 2); !ok || sc.RecordsEnd != endB {
		t.Errorf("the scan of segment 2 got RecordsEnd %d (found %v), want %d", sc.RecordsEnd, ok, endB)
	}
	// Unsealed and whole: the tail's proof owns it, and its slot was proven
	// written by the synced-through that passes it.
	wantFate(t, st, 2, fateIntact, 0)
	wantDamaged(t, st, 2)
}

// A zero-header segment whose records verify but whose header slot no proof
// covers is no spare: the records attach, nothing is damaged, and the header is
// torn, so the segment ends with no tail to cut.
func TestAnUnnamedZeroHeaderSegmentWithNoProofIsEndedNotSkipped(t *testing.T) {
	r := newRig(t)
	r.writeFormat()
	r.plantSeg(2, zeroHeader(buildSeg(2, tailRec(200, 0x4100), tailRec(201, 0x8100))))
	st := runUnchanged(t, r, 2)
	if got := planted(st); got != 2 {
		t.Errorf("the index holds %d entries, want 2: the records attach", got)
	}
	wantFate(t, st, 2, fateEnded, 0)
	wantDamaged(t, st)
}

// TestAZeroHeaderSegmentsTailIsProvenLikeAnyOthers holds that a zero-header
// segment whose records the rebuild walked reaches the tail's proof: its
// records were on the journal's books, so a tail record the device lost is
// re-appended and ends the segment, and one whose bytes never verified drops
// as never written, keeping the fate the classing gave a segment no proof
// covers its header slot — ended at the last record that held, never a
// torn tail's seal written over the slot nothing proves.
func TestAZeroHeaderSegmentsTailIsProvenLikeAnyOthers(t *testing.T) {
	const overwrite = 0x4100
	t.Run("a lost tail record is re-appended and the segment ends", func(t *testing.T) {
		endA := endAfter(tailRec(200, overwrite))
		endB := endAfter(tailRec(200, overwrite), tailRec(201, 0x8100))
		full := buildSeg(2, syncedRec(200, overwrite, endA), syncedRec(201, 0x8100, endA))
		name := format.SegmentName(2)
		goodB := full[endA:endB]

		r := newRig(t)
		r.writeFormat()
		// The header slot reads zero past a synced-through that covers it, so
		// the classing marks the slot damaged, the segment intact.
		r.plantSeg(2, zeroHeader(full[:endA]))
		r.writeLiveOnly(name, endA, goodB) // B, whole in the cache alone
		r.dropSyncs(name)

		st := runTails(t, r)

		wantReappends(t, st, reappendOf(t, 2, endA, goodB))
		if got := planted(st); got != 2 {
			t.Errorf("the index holds %d entries, want 2: A attached and B re-appended from the cache", got)
		}
		wantFate(t, st, 2, fateEnded, endB)
		wantDamaged(t, st, 2)
	})
	t.Run("a torn tail record drops and the ended fate stands", func(t *testing.T) {
		endA := endAfter(tailRec(200, overwrite))
		seg := buildSeg(2, tailRec(200, overwrite), tailRec(201, 0x8100))
		seg[endA+format.RecordHeaderSize+8+2] ^= 0xff // B's payload, torn everywhere

		r := newRig(t)
		r.writeFormat()
		r.plantSeg(2, zeroHeader(seg))
		st := runUnchanged(t, r, 2)

		if got := planted(st); got != 1 {
			t.Errorf("the index holds %d entries, want 1: A attaches and the torn B drops as never written", got)
		}
		noTornReport(t, st)
		wantFate(t, st, 2, fateEnded, endA)
		wantDamaged(t, st)
	})
}

// An all-zero spare at the segment cap scans empty: it holds no record, so it
// takes no fate and no damage, and the open changes nothing. The spares phase
// re-adopts each where it stands, so a fixture planted cap-sized keeps them
// on disk as this row reads them; the phase unlinks one short or over the cap.
func TestAnAllZeroSpareScansEmptyAndTakesNoFate(t *testing.T) {
	const spares = 6
	r := newRig(t)
	r.writeFormat()
	r.plantSeg(1, buildSeg(1, tailRec(200, 0x4100)))
	for id := core.SegmentID(2); id < 2+spares; id++ {
		r.plantSeg(id, make([]byte, testCap))
	}
	before := r.snapshot()
	st := runTails(t, r)
	assertOnlySpareUnlinks(t, before, r)
	if len(st.Attached.ZeroHeader) != spares {
		t.Fatalf("Attached.ZeroHeader got %v, want %d spares", st.Attached.ZeroHeader, spares)
	}
	for id := core.SegmentID(2); id < 2+spares; id++ {
		if _, ok := scanOf(st, id); !ok {
			t.Errorf("Rebuilt.Scans holds no scan of the all-zero spare %d, want an empty one", id)
		}
	}
	if got := planted(st); got != 1 {
		t.Errorf("the index holds %d entries, want segment 1's 1", got)
	}
	wantFate(t, st, 1, fateIntact, 0)
	for _, f := range st.Tails.Fates {
		if f.ID != 1 {
			t.Errorf("Tails.Fates holds %+v for a spare, want none: the spare phase owns it", f)
		}
	}
	wantDamaged(t, st)
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
	// A long named file no record survives in has the whole file as its
	// missing end too — the range starts at 0, never at the first record
	// slot, for no surviving record names an end to start from.
	t.Run("4096 bytes of zeros", func(t *testing.T) {
		r := newRig(t)
		r.writeFormat()
		r.plantSeg(1, make([]byte, 4096))
		r.plantSeg(2, namingSeg(2, 1))
		wantRefused(t, r, 1, 0, 4096)
	})
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
