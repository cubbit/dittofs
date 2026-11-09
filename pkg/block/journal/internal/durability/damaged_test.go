package durability

import (
	"context"
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

// wantBlocks fails unless seg's row is counted and blocks reclamation as
// want says, through both ReclaimBlocked and the Stats row.
func wantBlocks(t *testing.T, cr *corruptRig, seg core.SegmentID, want bool) {
	t.Helper()
	if got := cr.d.ReclaimBlocked(seg); got != want {
		t.Fatalf("ReclaimBlocked(%d) = %v, want %v", seg, got, want)
	}
	p := cr.d.Stats()
	r, ok := rowOf(p, seg)
	if !ok || r.BlocksReclaim != want || p.DamagedSegments != 1 {
		t.Fatalf("count %d, row %+v, %v, want 1 and a row with BlocksReclaim %v", p.DamagedSegments, r, ok, want)
	}
}

// TestDamagedReadableNeighbourDoesNotBlockReclaim covers a damaged segment
// whose corrupt record was dropped and which still holds a readable neighbour:
// the neighbour is content the segment can produce, so nothing blocks
// reclamation, while the row and the count stay.
func TestDamagedReadableNeighbourDoesNotBlockReclaim(t *testing.T) {
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
	if _, ok := cr.ix.Lookup(f, 100); !ok {
		t.Fatal("the neighbour does not read")
	}
	wantBlocks(t, cr, seg, false)
}

// TestDamagedBlocksReclaimWhileAHeldRecordIsUnrepointed covers a synced
// record found corrupt and served from its copy while the copy's segment
// claim is owed: the index still names the corrupt record, so its segment
// blocks reclamation until the record leaves it, by the worker's repoint once
// the claim is taken, or until the segment is retired. Only the block ends,
// never the count or the row.
func TestDamagedBlocksReclaimWhileAHeldRecordIsUnrepointed(t *testing.T) {
	for _, end := range []string{"repointed", "retired"} {
		t.Run(end, func(t *testing.T) {
			prof.Watch(t, prof.Options{})
			l := newLanding(false)
			cr := newCorruptRig(t, l.install)
			f := file(1)
			r := cr.reappend(t, f, 0, "aaaa")
			if err := cr.d.Sync(context.Background(), f); err != nil {
				t.Fatalf("Sync of the re-append: %v", err)
			}
			cr.register(r)
			copySeg, damaged := r.orig.Segment, r.c.Record.Segment
			if !cr.set.Claim(copySeg) {
				t.Fatal("the repack could not claim the copy's segment")
			}

			cr.useCopy(t, r)
			if got := cr.placedAt(t, f, 0); got != r.c.Record {
				t.Fatalf("f is placed at %+v, want the corrupt record %+v unrepointed", got, r.c.Record)
			}
			wantBlocks(t, cr, damaged, true)

			switch end {
			case "repointed":
				cr.set.Release(copySeg)
				cr.tickUntil(t, "the hold to be let go", func() bool { return !cr.holdOf(copySeg).held })
				if got := cr.placedAt(t, f, 0); got == r.c.Record {
					t.Fatal("f is still placed at the corrupt record")
				}
			case "retired":
				cr.d.Retired(damaged)
			}
			wantBlocks(t, cr, damaged, false)
		})
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
