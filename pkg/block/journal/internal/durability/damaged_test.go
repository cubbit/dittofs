package durability

import (
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/block/journal/journaltest/prof"
)

// rowOf returns the damaged row of seg in the report, and whether it has one.
func rowOf(p Part, seg core.SegmentID) (DamagedRow, bool) {
	for _, r := range p.Segments {
		if r.Segment == seg {
			return r, true
		}
	}
	return DamagedRow{}, false
}

// TestDamagedCountsFromTheCall covers the count: it reads 1 as soon as Corrupt
// returns, with the record's extents already dropped and the worker not waited
// on.
func TestDamagedCountsFromTheCall(t *testing.T) {
	prof.Watch(t, prof.Options{})
	cr := newCorruptRig(t, nil)
	w := cr.write(t, file(1), 0, "aaaa")
	if got := cr.d.Stats().DamagedSegments; got != 0 {
		t.Fatalf("DamagedSegments = %d before any corrupt record, want 0", got)
	}
	cr.corrupt(t, w.c)
	p := cr.d.Stats()
	if p.DamagedSegments != 1 {
		t.Fatalf("DamagedSegments = %d right after Corrupt, want 1", p.DamagedSegments)
	}
	if _, ok := rowOf(p, w.loc.Segment); !ok || len(p.Segments) != 1 {
		t.Fatalf("Segments = %+v, want one row for segment %d", p.Segments, w.loc.Segment)
	}
}

// TestDamagedStaysAfterReclaimBlockEnds covers the block: a damaged segment
// that still backs a held extent blocks reclamation, and once that extent is
// dropped only the block ends, never the count or the row.
func TestDamagedStaysAfterReclaimBlockEnds(t *testing.T) {
	prof.Watch(t, prof.Options{})
	cr := newCorruptRig(t, nil)
	f := file(1)
	a := cr.write(t, f, 0, "aaaa")
	b := cr.write(t, f, 100, "bbbb")
	if a.loc.Segment != b.loc.Segment {
		t.Fatal("the two records are in different segments")
	}
	seg := a.loc.Segment
	if cr.d.ReclaimBlocked(seg) {
		t.Fatal("ReclaimBlocked is true for a segment with no unreadable record")
	}
	cr.corrupt(t, a.c)
	if !cr.d.ReclaimBlocked(seg) {
		t.Fatal("ReclaimBlocked is false while the segment still backs a held extent")
	}
	if r, ok := rowOf(cr.d.Stats(), seg); !ok || !r.BlocksReclaim {
		t.Fatalf("row = %+v, %v, want one that blocks reclamation", r, ok)
	}
	cr.corrupt(t, b.c)
	if cr.d.ReclaimBlocked(seg) {
		t.Fatal("ReclaimBlocked is still true with no held extent")
	}
	p := cr.d.Stats()
	r, ok := rowOf(p, seg)
	if p.DamagedSegments != 1 || !ok || r.BlocksReclaim {
		t.Fatalf("after the block ended: count %d, row %+v, %v, want 1 and a row that no longer blocks", p.DamagedSegments, r, ok)
	}
}

// TestDamagedRowRetiredAfterRetired covers the retirement hook: the row is
// marked retired and kept, and still counted.
func TestDamagedRowRetiredAfterRetired(t *testing.T) {
	prof.Watch(t, prof.Options{})
	cr := newCorruptRig(t, nil)
	w := cr.write(t, file(1), 0, "aaaa")
	cr.corrupt(t, w.c)
	cr.d.Retired(w.loc.Segment)
	cr.d.Retired(core.SegmentID(9999)) // a segment with no row changes nothing
	p := cr.d.Stats()
	r, ok := rowOf(p, w.loc.Segment)
	if p.DamagedSegments != 1 || !ok || !r.Retired || len(p.Segments) != 1 {
		t.Fatalf("count %d, rows %+v, want 1 and one retired row", p.DamagedSegments, p.Segments)
	}
}

// TestDamagedIsIdempotentPerSegment covers two corrupt records of one
// segment: one row, one count.
func TestDamagedIsIdempotentPerSegment(t *testing.T) {
	prof.Watch(t, prof.Options{})
	cr := newCorruptRig(t, nil)
	f := file(1)
	a := cr.write(t, f, 0, "aaaa")
	b := cr.write(t, f, 100, "bbbb")
	if a.loc.Segment != b.loc.Segment {
		t.Fatal("the two records are in different segments")
	}
	cr.corrupt(t, a.c)
	cr.corrupt(t, b.c)
	cr.corrupt(t, b.c)
	if p := cr.d.Stats(); p.DamagedSegments != 1 || len(p.Segments) != 1 {
		t.Fatalf("count %d, rows %+v, want 1 and one row", p.DamagedSegments, p.Segments)
	}
}

// TestStatsDamagedServedWithoutLocks covers the report's cost: Stats reads the
// damaged count while a stream's mutex and a file's append domain are held, so
// it neither walks a stream nor waits on a lock a write needs.
func TestStatsDamagedServedWithoutLocks(t *testing.T) {
	prof.Watch(t, prof.Options{})
	cr := newCorruptRig(t, nil)
	f := file(1)
	w := cr.write(t, f, 0, "aaaa")
	cr.corrupt(t, w.c)

	st := cr.d.made[0]
	st.mu.Lock()
	unlock := cr.d.domains.Lock(f)
	got := make(chan uint64, 1)
	go func() { got <- cr.d.Stats().DamagedSegments }()
	select {
	case n := <-got:
		if n != 1 {
			t.Errorf("DamagedSegments = %d, want 1", n)
		}
		unlock()
		st.mu.Unlock()
	case <-time.After(5 * time.Second):
		t.Error("Stats waited on a stream or domain lock")
		unlock()
		st.mu.Unlock()
		<-got
	}
}

// TestDamagedCountsWhenWorkerAlreadyFailedRecord covers a record the worker
// already failed: the read still proved it unreadable, so its segment counts.
func TestDamagedCountsWhenWorkerAlreadyFailedRecord(t *testing.T) {
	prof.Watch(t, prof.Options{})
	cr := newCorruptRig(t, nil)
	f := file(1)
	w := cr.write(t, f, 0, "aaaa")
	cr.failSegmentOf(t, w.loc.Segment, f)
	cr.awaitResolved(t)
	cr.awaitRecord(t, w.c.Seq)

	cr.corrupt(t, w.c)
	p := cr.d.Stats()
	if _, ok := rowOf(p, w.loc.Segment); p.DamagedSegments != 1 || !ok {
		t.Fatalf("count %d, rows %+v, want 1 and a row for segment %d", p.DamagedSegments, p.Segments, w.loc.Segment)
	}
}

// TestDamagedCountsWhenIndexNoLongerNamesRecord covers a record an overwrite
// already took out of the index: it was still read as unreadable, so its
// segment counts although nothing is lost.
func TestDamagedCountsWhenIndexNoLongerNamesRecord(t *testing.T) {
	prof.Watch(t, prof.Options{})
	cr := newCorruptRig(t, nil)
	f := file(1)
	old := cr.write(t, f, 0, "aaaa")
	cr.write(t, f, 0, "bbbb")

	cr.corrupt(t, old.c)
	p := cr.d.Stats()
	if _, ok := rowOf(p, old.loc.Segment); p.DamagedSegments != 1 || !ok {
		t.Fatalf("count %d, rows %+v, want 1 and a row for segment %d", p.DamagedSegments, p.Segments, old.loc.Segment)
	}
}
