package durability

import (
	"context"
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/index"
	"github.com/cubbit/dittofs/pkg/block/journal/journaltest/prof"
)

// syncAtTake makes Corrupt run a Sync of the file at the point its drop of an
// unsynced record becomes visible to a Sync, and returns the generation that
// Sync found and its result once Corrupt has returned.
func syncAtTake(t *testing.T, cr *corruptRig, w written) (gen uint64, err error) {
	t.Helper()
	type seen struct {
		gen uint64
		err error
	}
	ch := make(chan seen, 1)
	cr.d.afterCorruptTake = func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		// The generation is read after the Sync returns, so it is at least the
		// one the Sync's caller would sample with its answer.
		err := cr.d.Sync(ctx, w.c.File)
		ch <- seen{gen: cr.d.Generation(), err: err}
	}
	cr.corrupt(t, w.c)
	select {
	case s := <-ch:
		return s.gen, s.err
	default:
		t.Fatal("Corrupt never made the drop visible to a Sync")
		return 0, nil
	}
}

// TestCorruptRaisesBeforeASyncSeesAnUnsyncedDrop covers an unsynced record of
// a healthy segment: a Sync that sees it dropped returns with the generation
// already raised, so a caller that samples the generation with that answer
// never reports the dropped write stable under the verifier it was written
// with.
func TestCorruptRaisesBeforeASyncSeesAnUnsyncedDrop(t *testing.T) {
	prof.Watch(t, prof.Options{})
	cr := newCorruptRig(t, nil)
	w := cr.write(t, file(1), 0, "aaaa")
	before := cr.d.Generation()

	gen, err := syncAtTake(t, cr, w)
	if err != nil {
		t.Fatalf("the Sync that saw the drop = %v, want nil", err)
	}
	if gen <= before {
		t.Fatalf("the Sync that saw the drop returned with the generation at %d, want above %d", gen, before)
	}
	if got := cr.d.Generation(); got != before+1 {
		t.Fatalf("the generation is %d after Corrupt, want %d: one raise", got, before+1)
	}
}

// TestCorruptRaisesBeforeASyncSeesAPendingWindowDrop covers a record of a
// pending window the worker has not failed yet: the Sync Corrupt's mark
// answers returns the window's failure with the generation already raised.
func TestCorruptRaisesBeforeASyncSeesAPendingWindowDrop(t *testing.T) {
	prof.Watch(t, prof.Options{})
	cr, fw, _, release := pendingWindow(t)
	before := cr.d.Generation()

	gen, err := syncAtTake(t, cr, fw)
	wantSyncError(t, err, fw.loc.Segment, fw.c.File)
	if gen <= before {
		t.Fatalf("the Sync that took the window's failure returned with the generation at %d, want above %d", gen, before)
	}
	release()
	cr.awaitResolved(t)
}

// TestCorruptOfAnOffloadedRecordRaisesNothing covers an unsynced record whose
// pieces are all offloaded: its drop loses nothing the remote tier lacks, so
// neither a Sync that sees it nor Corrupt's return finds the generation moved.
func TestCorruptOfAnOffloadedRecordRaisesNothing(t *testing.T) {
	prof.Watch(t, prof.Options{})
	cr := newCorruptRig(t, func(cr *corruptRig) {
		cr.d.onCorruptHeld = func(held []index.Piece) {
			for i := range held {
				held[i].Offloaded = true
			}
		}
	})
	w := cr.write(t, file(1), 0, "aaaa")
	before := cr.d.Generation()

	gen, err := syncAtTake(t, cr, w)
	if err != nil || gen != before {
		t.Fatalf("the Sync that saw the drop = %v with the generation at %d, want nil and %d", err, gen, before)
	}
	if got := cr.d.Generation(); got != before {
		t.Fatalf("the generation is %d after Corrupt, want %d", got, before)
	}
}

// TestCorruptOfAnOverwrittenRecordRaisesNothing covers an unsynced record a
// later write overwrote before the read found it corrupt: the index names
// nothing of it, so nothing is lost, the generation stays and no loss is
// reported.
func TestCorruptOfAnOverwrittenRecordRaisesNothing(t *testing.T) {
	prof.Watch(t, prof.Options{})
	cr := newCorruptRig(t, nil)
	f := file(1)
	old := cr.write(t, f, 0, "aaaa")
	cr.write(t, f, 0, "bbbb")
	before := cr.d.Generation()

	cr.corrupt(t, old.c)
	// The overwrite is unsynced, so this Sync waits for its sync, which the
	// worker completes under the file's domain; it runs once Corrupt let the
	// domain go.
	cr.wantNilSync(t, f)
	cr.d.Stop()
	if got := cr.d.Generation(); got != before {
		t.Fatalf("the generation is %d after Corrupt, want %d", got, before)
	}
	if evs := cr.lossesOf(f); len(evs) != 0 {
		t.Fatalf("events = %+v, want none", evs)
	}
	if recs := cr.recordsNaming(t, old.c.Seq); len(recs) != 0 {
		t.Fatalf("loss records = %+v, want none", recs)
	}
}

// TestCorruptRacingItsWindowsFailureRaisesOnce covers a record of a failed
// segment, its window's only one, found corrupt at each point of the worker's
// failure of that window: before the window's raise, between the raise and
// the drop, and after the drop. The window is raised once whichever comes
// first, its file is told once, and the generation ends one above where it
// was.
func TestCorruptRacingItsWindowsFailureRaisesOnce(t *testing.T) {
	for _, at := range []string{"before the raise", "between the raise and the drop", "after the drop"} {
		t.Run(at, func(t *testing.T) {
			prof.Watch(t, prof.Options{})
			hook, reached, release := gate(t)
			cr := newCorruptRig(t, func(cr *corruptRig) { cr.d.beforeFail = func(*window) { hook() } })
			t.Cleanup(release)
			f := file(1)
			w := cr.write(t, f, 0, "aaaa")
			before := cr.d.Generation()
			cr.failByTimer(t, w.loc.Segment)
			<-reached

			switch at {
			case "before the raise":
				cr.corrupt(t, w.c)
				release()
				cr.awaitResolved(t)
			case "between the raise and the drop":
				// The worker raises and then waits on the file's domain to
				// drop the record, which this read holds.
				unlock := cr.d.domains.Lock(f)
				release()
				eventually(t, "the window's raise", func() bool { return cr.d.Generation() == before+1 })
				c := w.c
				c.DomainHeld = true
				cr.corrupt(t, c)
				unlock()
				cr.awaitResolved(t)
			case "after the drop":
				release()
				cr.awaitResolved(t)
				cr.corrupt(t, w.c)
			}
			if got := cr.d.Generation(); got != before+1 {
				t.Fatalf("the generation is %d, want %d: one raise for the window and its record", got, before+1)
			}
			wantSyncError(t, mustReturn(t, "first Sync", cr.syncAsync(context.Background(), f)), w.loc.Segment, f)
			cr.wantNilSync(t, f)
			cr.d.Stop()
			if got := cr.d.Generation(); got != before+1 {
				t.Fatalf("the generation is %d once the worker stopped, want %d", got, before+1)
			}
			if evs := cr.lossesOf(f); len(evs) != 1 {
				t.Fatalf("events = %+v, want one loss for the record", evs)
			}
		})
	}
}
