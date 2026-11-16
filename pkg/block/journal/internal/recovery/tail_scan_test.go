package recovery

import (
	"context"
	"testing"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/block/journal/journaltest/raw"
)

// tailRec is a write record of its own version, so two of them in one
// segment are two index entries, not one.
func tailRec(counter uint64, off uint64) raw.Record {
	return rec(1, func(r *raw.Record) {
		r.Version = raw.Version{Counter: counter}
		r.Offset = off
		r.Write = &raw.WriteBody{MTime: 1, Payload: bytes16()}
	})
}

// tailFile is the file every tailRec writes.
func tailFile() core.FileID {
	return core.NewFileID(rec(1, nil).FileID)
}

// damageHeader flips a byte inside the first 8 bytes of a segment's header,
// so the header no longer verifies while every record behind it still does.
func damageHeader(b []byte) []byte {
	b[0] ^= 0xff
	return b
}

// TestTheRebuildScansAnUnverifiedHeaderSegment plants a segment whose header
// does not verify but whose records do: they verify by identity, segment and
// offset, whatever the header slot says, so the rebuild attaches them.
func TestTheRebuildScansAnUnverifiedHeaderSegment(t *testing.T) {
	r := newRig(t)
	r.writeFormat()
	r.plantSeg(2, damageHeader(buildSeg(2, tailRec(200, 0x4100), tailRec(201, 0x8100))))

	st, err := Run(context.Background(), r.runDeps(), core.Version{}, journalID, nil)
	if err != nil {
		t.Fatalf("Run over an unverified-header segment: %v", err)
	}
	if len(st.Attached.Unverified) != 1 || st.Attached.Unverified[0] != 2 {
		t.Fatalf("Attached.Unverified got %v, want [2]", st.Attached.Unverified)
	}
	if got := len(st.Rebuilt.Index.FileEntries(tailFile())); got != 2 {
		t.Errorf("the index holds %d entries of the planted file, want 2: the unverified segment's records attach", got)
	}
	sc, ok := scanOf(st, 2)
	if !ok {
		t.Fatalf("Rebuilt.Scans holds no scan of segment 2, want one")
	}
	endB := endAfter(tailRec(200, 0x4100), tailRec(201, 0x8100))
	if sc.RecordsEnd != endB || sc.Sealed {
		t.Errorf("the scan of segment 2 got RecordsEnd %d, Sealed %v, want %d (the end of the second planted record) and unsealed", sc.RecordsEnd, sc.Sealed, endB)
	}
}

// TestANamedUnverifiedSegmentIsScannedNeverCatalogued plants segment 1 with
// an unverified header and a footer that would read, named by segment 2's
// verified header: the catalogue is never offered to it, its records attach
// by the walk.
func TestANamedUnverifiedSegmentIsScannedNeverCatalogued(t *testing.T) {
	r := newRig(t)
	r.writeFormat()
	sealed, _ := buildSealed(1, tailRec(200, 0x4100), tailRec(201, 0x8100))
	r.plantSeg(1, damageHeader(sealed))
	r.plantSeg(2, raw.SegmentHeader{Identity: journalID, ID: 2, Predecessor: 1}.Bytes())

	st, err := Run(context.Background(), r.runDeps(), core.Version{}, journalID, nil)
	if err != nil {
		t.Fatalf("Run over a named unverified segment: %v", err)
	}
	if st.Rebuilt.ByCatalog != 0 {
		t.Errorf("Run rebuilt %d segments by catalog, want 0: an unverified segment is never catalogued", st.Rebuilt.ByCatalog)
	}
	if got := len(st.Rebuilt.Index.FileEntries(tailFile())); got != 2 {
		t.Errorf("the index holds %d entries of the planted file, want 2: segment 1's records attach", got)
	}
}
