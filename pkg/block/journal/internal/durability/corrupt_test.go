package durability

import (
	"context"
	"errors"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/format"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/index"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/segment"
	"github.com/cubbit/dittofs/pkg/block/journal/journaltest/kit"
	"github.com/cubbit/dittofs/pkg/block/journal/journaltest/prof"
)

// corruptRig is a one-stream failRig whose MarkLoss calls are recorded.
type corruptRig struct {
	*failRig
	mu     sync.Mutex
	marked []marked
}

// marked is one MarkLoss call.
type marked struct {
	file    core.FileID
	extent  core.Extent
	version core.Version
}

// newCorruptRig builds the rig, runs pre on it before the layer starts, and
// starts it.
func newCorruptRig(t *testing.T, pre func(*corruptRig)) *corruptRig {
	t.Helper()
	cr := &corruptRig{failRig: &failRig{resolved: make(chan core.SegmentID, 64)}}
	cr.rig = newRigConfigured(t, 1, func(c *Config) {
		c.Resolved = func(_ uint32, seg core.SegmentID) { cr.resolved <- seg }
		c.MarkLoss = func(f core.FileID, x core.Extent, v core.Version) (index.HeldDelta, error) {
			cr.mu.Lock()
			defer cr.mu.Unlock()
			cr.marked = append(cr.marked, marked{f, x, v})
			return index.HeldDelta{}, nil
		}
	})
	if pre != nil {
		pre(cr)
	}
	cr.d.Start(cr.streams)
	return cr
}

// marks returns the MarkLoss calls so far.
func (cr *corruptRig) marks() []marked {
	cr.mu.Lock()
	defer cr.mu.Unlock()
	return slices.Clone(cr.marked)
}

// written is a published write and the Corruption a read of it would report.
type written struct {
	loc segment.Location
	c   Corruption
}

// write publishes a write of f and returns it with its Corruption.
func (cr *corruptRig) write(t *testing.T, f core.FileID, off int64, payload string) written {
	t.Helper()
	loc, v := cr.publish(t, f, off, payload)
	return written{loc: loc, c: Corruption{
		Share:   1,
		File:    f,
		Record:  index.RecordAt{Segment: loc.Segment, Offset: uint32(loc.Off)},
		Seq:     core.Seq(cr.lastSeq.Load()),
		Version: v,
		Extent:  core.Extent{Off: off, Len: int64(len(payload))},
	}}
}

// corrupt calls Corrupt and fails the test unless it drops the record with
// no error.
func (cr *corruptRig) corrupt(t *testing.T, c Corruption) {
	t.Helper()
	out, err := cr.d.Corrupt(context.Background(), c)
	if err != nil || out.Kind != CorruptDropped {
		t.Fatalf("Corrupt = %+v, %v, want Dropped and nil", out, err)
	}
}

// lossesOf returns the events naming f.
func (cr *corruptRig) lossesOf(f core.FileID) []core.LossEvent {
	var out []core.LossEvent
	for _, ev := range cr.events() {
		if ev.File == f {
			out = append(out, ev)
		}
	}
	return out
}

// recordsNaming returns the durable loss records naming seq.
func (cr *corruptRig) recordsNaming(t *testing.T, seq core.Seq) []format.Loss {
	t.Helper()
	var out []format.Loss
	for _, l := range cr.lossRecordsOnDisk(t) {
		if l.Dropped == seq {
			out = append(out, l)
		}
	}
	return out
}

// awaitRecord waits until one durable loss record names seq and returns it.
func (cr *corruptRig) awaitRecord(t *testing.T, seq core.Seq) format.Loss {
	t.Helper()
	var got []format.Loss
	eventually(t, "the loss record", func() bool {
		got = cr.recordsNaming(t, seq)
		return len(got) > 0
	})
	if len(got) != 1 {
		t.Fatalf("%d loss records name seq %d, want 1: %+v", len(got), seq, got)
	}
	return got[0]
}

// inLedger reports whether the stream's ledger holds the record at loc.
func (cr *corruptRig) inLedger(loc segment.Location) bool {
	st := cr.d.made[0]
	st.mu.Lock()
	defer st.mu.Unlock()
	return slices.ContainsFunc(st.ledger, func(e entry) bool { return e.w.Loc == loc })
}

// marksOf returns how many marks the stream holds for f.
func (cr *corruptRig) marksOf(f core.FileID) int {
	st := cr.d.made[0]
	st.mu.Lock()
	defer st.mu.Unlock()
	return len(st.marks[f])
}

// gate returns a hook that signals reached the first time it runs and then
// waits for release, and the function that releases it. The caller registers
// the release as a cleanup once the rig is built, so it runs before the
// layer's Stop, which would otherwise wait on a parked worker.
func gate(t *testing.T) (hook func(), reached <-chan struct{}, release func()) {
	t.Helper()
	at := make(chan struct{})
	ch, open := releaser(t)
	var once sync.Once
	return func() {
		first := false
		once.Do(func() { first = true; close(at) })
		if first {
			<-ch
		}
	}, at, open
}

// TestCorruptDropsExtentsAndServesNothing covers a corrupt record: its extent
// leaves the index and Corrupt reports it dropped with no error.
func TestCorruptDropsExtentsAndServesNothing(t *testing.T) {
	prof.Watch(t, prof.Options{})
	cr := newCorruptRig(t, nil)
	f := file(1)
	w := cr.write(t, f, 0, "aaaa")

	cr.corrupt(t, w.c)
	for off := range int64(4) {
		if p, ok := cr.ix.Lookup(f, off); ok {
			t.Fatalf("offset %d still reads %+v after Corrupt", off, p)
		}
	}
	cr.awaitRecord(t, w.c.Seq)
}

// TestCorruptDirtyVsOffloaded covers the generation and the counters: a dirty
// corrupt extent is data loss and raises the generation once, an offloaded one
// is repairable and raises nothing.
func TestCorruptDirtyVsOffloaded(t *testing.T) {
	for _, offloaded := range []bool{false, true} {
		name := "dirty"
		if offloaded {
			name = "offloaded"
		}
		t.Run(name, func(t *testing.T) {
			prof.Watch(t, prof.Options{})
			cr := newCorruptRig(t, func(cr *corruptRig) {
				if offloaded {
					cr.d.afterCorruptDrop = func(fw *index.FailedWindow) {
						for i := range fw.Dropped {
							fw.Dropped[i].Offloaded = true
						}
					}
				}
			})
			f := file(1)
			w := cr.write(t, f, 0, "aaaa")
			before := cr.d.Generation()

			cr.corrupt(t, w.c)
			wantGen := before + 1
			if offloaded {
				wantGen = before
			}
			if got := cr.d.Generation(); got != wantGen {
				t.Fatalf("the generation is %d when Corrupt returned, want %d", got, wantGen)
			}
			eventually(t, "the loss event", func() bool { return len(cr.lossesOf(f)) > 0 })
			evs := cr.lossesOf(f)
			if len(evs) != 1 || evs[0].Reason != core.LossCorrupt || evs[0].Offloaded != offloaded || evs[0].Version != w.c.Version {
				t.Fatalf("events = %+v, want one corrupt loss, offloaded %v", evs, offloaded)
			}
			if got := cr.d.Generation(); got != wantGen {
				t.Fatalf("the generation is %d after the event, want %d: Lose raised again", got, wantGen)
			}
			p := cr.d.Stats()
			if offloaded && (p.CorruptRepairable != 1 || p.CorruptDataLoss != 0) {
				t.Fatalf("counters = %d data loss, %d repairable, want 0 and 1", p.CorruptDataLoss, p.CorruptRepairable)
			}
			if !offloaded && (p.CorruptDataLoss != 1 || p.CorruptRepairable != 0) {
				t.Fatalf("counters = %d data loss, %d repairable, want 1 and 0", p.CorruptDataLoss, p.CorruptRepairable)
			}
			if rec := cr.awaitRecord(t, w.c.Seq); rec.Offloaded != offloaded || rec.Reason != format.LossCorrupt {
				t.Fatalf("loss record = %+v, want corrupt, offloaded %v", rec, offloaded)
			}
		})
	}
}

// TestCorruptNeighbourStillReads covers confinement: another record of the
// same segment is untouched.
func TestCorruptNeighbourStillReads(t *testing.T) {
	prof.Watch(t, prof.Options{})
	cr := newCorruptRig(t, nil)
	f := file(1)
	a := cr.write(t, f, 0, "aaaa")
	b := cr.write(t, f, 8, "bbbb")
	if a.loc.Segment != b.loc.Segment {
		t.Fatal("the two records landed in different segments")
	}

	cr.corrupt(t, a.c)
	if p, ok := cr.ix.Lookup(f, 8); !ok || p.Version != b.c.Version {
		t.Fatalf("the neighbour reads %+v, %v, want its own version", p, ok)
	}
	if _, ok := cr.ix.Lookup(f, 0); ok {
		t.Fatal("the corrupt record still reads")
	}
}

// TestCorruptUnsyncedGetsUnsyncedLossRecordSyncedGetsSynced covers the synced
// flag of the loss record, decided from the ledger.
func TestCorruptUnsyncedGetsUnsyncedLossRecordSyncedGetsSynced(t *testing.T) {
	prof.Watch(t, prof.Options{})
	cr := newCorruptRig(t, nil)
	f, g := file(1), file(2)
	u := cr.write(t, f, 0, "aaaa")
	cr.corrupt(t, u.c)
	if rec := cr.awaitRecord(t, u.c.Seq); rec.Synced {
		t.Fatalf("the unsynced record's loss record = %+v, want unsynced", rec)
	}
	if m := cr.marks(); len(m) != 0 {
		t.Fatalf("an unsynced loss left markers %+v", m)
	}

	s := cr.write(t, g, 0, "bbbb")
	if err := cr.d.Sync(context.Background(), g); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	cr.corrupt(t, s.c)
	if rec := cr.awaitRecord(t, s.c.Seq); !rec.Synced {
		t.Fatalf("the synced record's loss record = %+v, want synced", rec)
	}
	eventually(t, "the synced loss's marker", func() bool { return len(cr.marks()) == 1 })
	if m := cr.marks()[0]; m.file != g || m.version != s.c.Version || m.extent != s.c.Extent {
		t.Fatalf("marker = %+v, want g's extent at its version", m)
	}
}

// TestCorruptSyncedLossMasksWhatItSuperseded covers a synced record found
// corrupt over an older synced one: the extent reads missing, never the older.
func TestCorruptSyncedLossMasksWhatItSuperseded(t *testing.T) {
	prof.Watch(t, prof.Options{})
	cr := newCorruptRig(t, nil)
	f := file(1)
	cr.write(t, f, 0, "aaaa")
	if err := cr.d.Sync(context.Background(), f); err != nil {
		t.Fatalf("Sync of v5: %v", err)
	}
	v10 := cr.write(t, f, 0, "bbbb")
	if err := cr.d.Sync(context.Background(), f); err != nil {
		t.Fatalf("Sync of v10: %v", err)
	}

	cr.corrupt(t, v10.c)
	if p, ok := cr.ix.Lookup(f, 0); ok {
		t.Fatalf("the extent reads %+v after the synced v10 was found corrupt, want missing", p)
	}
	if rec := cr.awaitRecord(t, v10.c.Seq); !rec.Synced {
		t.Fatalf("loss record = %+v, want synced", rec)
	}
	eventually(t, "the marker", func() bool { return len(cr.marks()) == 1 })
}

// TestCorruptUnsyncedReholdsAndLeavesLedger covers an unsynced record over a
// synced one: the older is served again, the entry leaves the ledger, a later
// sync lifts nothing.
func TestCorruptUnsyncedReholdsAndLeavesLedger(t *testing.T) {
	prof.Watch(t, prof.Options{})
	cr := newCorruptRig(t, nil)
	f, g := file(1), file(2)
	v5 := cr.write(t, f, 0, "aaaa")
	if err := cr.d.Sync(context.Background(), f); err != nil {
		t.Fatalf("Sync of v5: %v", err)
	}
	v10 := cr.write(t, f, 0, "bbbb")
	other := cr.write(t, g, 0, "cccc")
	if other.loc.Segment != v10.loc.Segment {
		t.Fatal("g's record landed in another segment")
	}

	cr.corrupt(t, v10.c)
	if p, ok := cr.ix.Lookup(f, 0); !ok || p.Version != v5.c.Version {
		t.Fatalf("the extent reads %+v, %v, want v5 held again", p, ok)
	}
	if cr.inLedger(v10.loc) {
		t.Fatal("v10's entry is still in the ledger")
	}
	if rec := cr.awaitRecord(t, v10.c.Seq); rec.Synced {
		t.Fatalf("loss record = %+v, want unsynced", rec)
	}

	cr.syncStream(t, f)
	eventually(t, "the completion pass", func() bool { return !cr.inLedger(other.loc) })
	if p, ok := cr.ix.Lookup(f, 0); !ok || p.Version != v5.c.Version {
		t.Fatalf("after a sync the extent reads %+v, %v, want v5 still held", p, ok)
	}
}

// TestCorruptUnsyncedThenItsSegmentFails covers an unsynced record found
// corrupt whose own segment's sync then fails, with no sync in between: the
// window is taken without it, so it is neither failed nor reported again, and
// what it superseded stays held.
func TestCorruptUnsyncedThenItsSegmentFails(t *testing.T) {
	prof.Watch(t, prof.Options{})
	cr := newCorruptRig(t, nil)
	f, g := file(1), file(2)
	v5 := cr.write(t, f, 0, "aaaa")
	if err := cr.d.Sync(context.Background(), f); err != nil {
		t.Fatalf("Sync of v5: %v", err)
	}
	v10 := cr.write(t, f, 0, "bbbb")

	cr.corrupt(t, v10.c)
	// The loss record lands, synced, before the segment's sync is made to
	// fail, so the failure takes g's later record and not the loss record's
	// own sync, which would also have covered g's.
	cr.awaitRecord(t, v10.c.Seq)
	other := cr.write(t, g, 0, "cccc")
	if other.loc.Segment != v10.loc.Segment {
		t.Fatal("g's record landed in another segment than v10")
	}
	cr.failSegmentOf(t, v10.loc.Segment, g)
	if got := cr.awaitResolved(t); got != v10.loc.Segment {
		t.Fatalf("resolved segment %d, want v10's %d", got, v10.loc.Segment)
	}
	cr.d.Stop()
	if p, ok := cr.ix.Lookup(f, 0); !ok || p.Version != v5.c.Version {
		t.Fatalf("after the window failed the extent reads %+v, %v, want v5", p, ok)
	}
	evs := cr.lossesOf(f)
	if len(evs) != 1 || evs[0].Reason != core.LossCorrupt {
		t.Fatalf("f's events = %+v, want only the corrupt loss", evs)
	}
	if recs := cr.recordsNaming(t, v10.c.Seq); len(recs) != 1 || recs[0].Reason != format.LossCorrupt {
		t.Fatalf("loss records naming v10 = %+v, want only the corrupt one", recs)
	}
}

// TestCorruptUnsyncedLedgerRecordIsNeverDurable covers an unsynced record
// found corrupt: it never reads durable, a durability wait on it keeps
// waiting, and a second read of it adds nothing.
func TestCorruptUnsyncedLedgerRecordIsNeverDurable(t *testing.T) {
	prof.Watch(t, prof.Options{})
	cr := newCorruptRig(t, nil)
	f := file(1)
	w := cr.write(t, f, 0, "aaaa")

	cr.corrupt(t, w.c)
	if cr.d.Durable(w.loc) {
		t.Fatal("the corrupt unsynced record reads durable")
	}
	wait := make(chan error, 1)
	go func() { wait <- cr.d.WaitDurable(w.loc) }()
	mustBlock(t, "WaitDurable", wait)
	cr.awaitRecord(t, w.c.Seq)

	before := cr.d.Generation()
	cr.corrupt(t, w.c)
	cr.d.Stop()
	if err := mustReturn(t, "WaitDurable", wait); !errors.Is(err, core.ErrClosed) {
		t.Fatalf("WaitDurable = %v, want ErrClosed at Stop", err)
	}
	if got := cr.d.Generation(); got != before {
		t.Fatalf("a second read moved the generation from %d to %d", before, got)
	}
	if evs := cr.lossesOf(f); len(evs) != 1 {
		t.Fatalf("events = %+v, want 1", evs)
	}
	if recs := cr.recordsNaming(t, w.c.Seq); len(recs) != 1 {
		t.Fatalf("loss records = %+v, want 1", recs)
	}
}

// TestCorruptPendingWindowRecordIsNeverDurable covers a record of a pending
// window found corrupt: it never reads durable, before or after the window
// resolves, and a durability wait on it keeps waiting.
func TestCorruptPendingWindowRecordIsNeverDurable(t *testing.T) {
	prof.Watch(t, prof.Options{})
	cr, fw, _, release := pendingWindow(t)

	cr.corrupt(t, fw.c)
	if cr.d.Durable(fw.loc) {
		t.Fatal("the corrupt record of a pending window reads durable")
	}
	wait := make(chan error, 1)
	go func() { wait <- cr.d.WaitDurable(fw.loc) }()
	mustBlock(t, "WaitDurable", wait)
	release()
	cr.awaitResolved(t)
	if cr.d.Durable(fw.loc) {
		t.Fatal("the corrupt record reads durable once its window resolved")
	}
	mustBlock(t, "WaitDurable", wait)
	cr.d.Stop()
	if err := mustReturn(t, "WaitDurable", wait); !errors.Is(err, core.ErrClosed) {
		t.Fatalf("WaitDurable = %v, want ErrClosed at Stop", err)
	}
}

// TestCorruptSyncedRecordStaysDurable covers a synced record found corrupt:
// it was synced, so it still reads durable.
func TestCorruptSyncedRecordStaysDurable(t *testing.T) {
	prof.Watch(t, prof.Options{})
	cr := newCorruptRig(t, nil)
	f := file(1)
	w := cr.write(t, f, 0, "aaaa")
	if err := cr.d.Sync(context.Background(), f); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	cr.corrupt(t, w.c)
	if !cr.d.Durable(w.loc) {
		t.Fatal("the synced corrupt record no longer reads durable")
	}
}

// TestCorruptCoveredBeforeWorkerAppliesCompletion covers a record a sync
// covered whose completion the worker has not applied: it is synced, its
// entry stays for the worker, and nothing it superseded stays retained.
func TestCorruptCoveredBeforeWorkerAppliesCompletion(t *testing.T) {
	prof.Watch(t, prof.Options{})
	hook, reached, release := gate(t)
	var parked atomic.Bool
	cr := newCorruptRig(t, func(cr *corruptRig) {
		cr.d.afterSync = func() {
			if parked.Load() {
				hook()
			}
		}
	})
	t.Cleanup(release)
	f := file(1)
	cr.write(t, f, 0, "aaaa")
	if err := cr.d.Sync(context.Background(), f); err != nil {
		t.Fatalf("Sync of v5: %v", err)
	}

	st := cr.d.made[0]
	v10 := cr.write(t, f, 0, "bbbb")
	// Park the worker after its next sync: the sync covers v10 and the pass
	// that would complete it has not run.
	parked.Store(true)
	syncing := cr.syncAsync(context.Background(), f)
	<-reached
	st.mu.Lock()
	covered := st.covered(v10.loc)
	st.mu.Unlock()
	if !covered {
		t.Fatal("the sync did not cover v10")
	}
	if len(cr.retainedBy(f, v10.c.Version)) == 0 {
		t.Fatal("v5 is not retained by v10 before the completion")
	}

	cr.corrupt(t, v10.c)
	if !cr.inLedger(v10.loc) {
		t.Fatal("Corrupt took the covered entry out of the ledger; the worker completes it")
	}
	if p, ok := cr.ix.Lookup(f, 0); ok {
		t.Fatalf("the extent reads %+v, want missing: v5 must not be held again", p)
	}
	release()
	if err := mustReturn(t, "Sync of v10", syncing); err != nil {
		t.Fatalf("Sync of v10: %v", err)
	}
	if rec := cr.awaitRecord(t, v10.c.Seq); !rec.Synced {
		t.Fatalf("loss record = %+v, want synced", rec)
	}
	eventually(t, "the completion", func() bool { return !cr.inLedger(v10.loc) })
	if got := cr.ix.Retained(f); len(got) != 0 {
		t.Fatalf("retained %+v after the completion, want none", got)
	}
	if p, ok := cr.ix.Lookup(f, 0); ok {
		t.Fatalf("the extent reads %+v after the completion, want missing", p)
	}
}

// TestCorruptSyncBetweenReadAndDecision covers a sync that completes after a
// read found the record unsynced and before Corrupt decides: the decision is
// Corrupt's own, so the record is synced and nothing is held again.
func TestCorruptSyncBetweenReadAndDecision(t *testing.T) {
	prof.Watch(t, prof.Options{})
	cr := newCorruptRig(t, nil)
	f := file(1)
	cr.write(t, f, 0, "aaaa")
	if err := cr.d.Sync(context.Background(), f); err != nil {
		t.Fatalf("Sync of v5: %v", err)
	}
	v10 := cr.write(t, f, 0, "bbbb")
	if cr.d.Durable(v10.loc) {
		t.Fatal("v10 reads durable before its sync")
	}
	// The read has found v10 unsynced; its sync completes before Corrupt.
	if err := cr.d.Sync(context.Background(), f); err != nil {
		t.Fatalf("Sync of v10: %v", err)
	}

	cr.corrupt(t, v10.c)
	if p, ok := cr.ix.Lookup(f, 0); ok {
		t.Fatalf("the extent reads %+v, want missing: the synced v10 masks v5", p)
	}
	if got := cr.ix.Retained(f); len(got) != 0 {
		t.Fatalf("retained %+v, want none", got)
	}
	if rec := cr.awaitRecord(t, v10.c.Seq); !rec.Synced {
		t.Fatalf("loss record = %+v, want synced", rec)
	}
}

// pendingWindow publishes f's only record of a segment and g's, fails the
// segment's sync through the timer and parks the worker before it fails the
// window's records. It returns f's write and the release of the worker.
func pendingWindow(t *testing.T) (cr *corruptRig, fw, gw written, release func()) {
	t.Helper()
	hook, reached, open := gate(t)
	cr = newCorruptRig(t, func(cr *corruptRig) { cr.d.beforeFail = func(*window) { hook() } })
	t.Cleanup(open)
	f, g := file(1), file(2)
	fw = cr.write(t, f, 0, "aaaa")
	gw = cr.write(t, g, 0, "bbbb")
	if fw.loc.Segment != gw.loc.Segment {
		t.Fatal("the two records landed in different segments")
	}
	cr.failByTimer(t, fw.loc.Segment)
	<-reached
	return cr, fw, gw, open
}

// TestCorruptRecordInPendingWindowMarksFile covers a record of a pending
// window, its file's only write there, found corrupt before the worker fails
// it: the file's next Sync reports the window once, the worker neither fails
// it nor reports it again, and one unsynced corrupt loss names it.
func TestCorruptRecordInPendingWindowMarksFile(t *testing.T) {
	prof.Watch(t, prof.Options{})
	cr, fw, gw, release := pendingWindow(t)
	f, g := fw.c.File, gw.c.File

	cr.corrupt(t, fw.c)
	// The worker is still parked: the report comes from Corrupt's mark.
	wantSyncError(t, mustReturn(t, "first Sync", cr.syncAsync(context.Background(), f)), fw.loc.Segment, f)
	cr.wantNilSync(t, f)

	release()
	cr.awaitResolved(t)
	cr.wantNilSync(t, f)
	wantSyncError(t, mustReturn(t, "Sync of g", cr.syncAsync(context.Background(), g)), gw.loc.Segment, g)

	if _, ok := cr.ix.Lookup(f, 0); ok {
		t.Fatal("f's corrupt record still reads")
	}
	if rec := cr.awaitRecord(t, fw.c.Seq); rec.Synced || rec.Reason != format.LossCorrupt {
		t.Fatalf("f's loss record = %+v, want one unsynced corrupt loss", rec)
	}
	cr.d.Stop()
	evs := cr.lossesOf(f)
	if len(evs) != 1 || evs[0].Reason != core.LossCorrupt {
		t.Fatalf("f's events = %+v, want one corrupt loss and no failed-window one", evs)
	}
	if recs := cr.recordsNaming(t, fw.c.Seq); len(recs) != 1 {
		t.Fatalf("loss records naming f's record = %+v, want 1", recs)
	}
}

// TestCorruptRecordOfFailedSegmentBeforeItsWindowMarksFile covers a record of
// a segment whose sync already failed, found corrupt before the worker took
// the segment's window: it is still in the ledger, and its file, whose only
// write in the window it was, is told of the failure all the same, once, also
// by a Sync that was already waiting on it.
func TestCorruptRecordOfFailedSegmentBeforeItsWindowMarksFile(t *testing.T) {
	prof.Watch(t, prof.Options{})
	hook, reached, release := gate(t)
	var parked atomic.Bool
	var waits atomic.Int64
	f, g := file(1), file(2)
	cr := newCorruptRig(t, func(cr *corruptRig) {
		cr.d.afterSync = func() {
			if parked.Load() {
				hook()
			}
		}
		// Counts the times a Sync naming f is left waiting.
		cr.d.beforeWait = func(ws []work) {
			if slices.ContainsFunc(ws, func(w work) bool { return slices.Contains(w.ids, f) }) {
				waits.Add(1)
			}
		}
	})
	t.Cleanup(release)
	gw := cr.write(t, g, 0, "gggg")
	// Park the worker after a sync that covers g's record, before its next
	// pass takes any window.
	parked.Store(true)
	gSync := cr.syncAsync(context.Background(), g)
	<-reached

	fw := cr.write(t, f, 0, "aaaa")
	if fw.loc.Segment != gw.loc.Segment {
		t.Fatal("f's record landed in another segment")
	}
	cr.failSyncOf(t, fw.loc.Segment, errors.New("sync boom"))
	if _, err := cr.streams.For(f).Seal(false); err == nil {
		t.Fatal("the seal's sync did not fail")
	}
	st := cr.d.made[0]
	eventually(t, "the end event to be queued", func() bool {
		st.mu.Lock()
		defer st.mu.Unlock()
		return len(st.ends) > 0
	})
	waiting := cr.syncAsync(context.Background(), f)
	eventually(t, "the Sync of f to wait", func() bool { return waits.Load() > 0 })
	mustBlock(t, "the waiting Sync", waiting)

	cr.corrupt(t, fw.c)
	release()
	if err := mustReturn(t, "Sync of g", gSync); err != nil {
		t.Fatalf("Sync of g: %v", err)
	}
	wantSyncError(t, mustReturn(t, "the waiting Sync", waiting), fw.loc.Segment, f)
	cr.wantNilSync(t, f)
	cr.awaitRecord(t, fw.c.Seq)
}

// TestCorruptRecordWhileItsEndEventIsTakenMarksFile covers a record found
// corrupt after the worker took its segment's end event off the queue and
// before it made the event's window: the file, whose only write in the window
// it was, is told of the failure once.
func TestCorruptRecordWhileItsEndEventIsTakenMarksFile(t *testing.T) {
	prof.Watch(t, prof.Options{})
	hook, reached, release := gate(t)
	cr := newCorruptRig(t, func(cr *corruptRig) { cr.d.beforeTakeWindow = hook })
	t.Cleanup(release)
	f := file(1)
	w := cr.write(t, f, 0, "aaaa")
	// A seal whose sync fails ends the segment with no failure the worker
	// counted, so the queued end event is all that names the failure.
	cr.failSyncOf(t, w.loc.Segment, errors.New("sync boom"))
	if _, err := cr.streams.For(f).Seal(false); err == nil {
		t.Fatal("the seal's sync did not fail")
	}
	<-reached

	cr.corrupt(t, w.c)
	release()
	wantSyncError(t, mustReturn(t, "first Sync", cr.syncAsync(context.Background(), f)), w.loc.Segment, f)
	cr.wantNilSync(t, f)
	cr.awaitRecord(t, w.c.Seq)
}

// TestCorruptEmptiesPendingWindowWhichResolves covers a pending window whose
// only record a read found corrupt: the window is resolved with nothing to
// fail, and the file is told once.
func TestCorruptEmptiesPendingWindowWhichResolves(t *testing.T) {
	prof.Watch(t, prof.Options{})
	hook, reached, release := gate(t)
	cr := newCorruptRig(t, func(cr *corruptRig) { cr.d.beforeFail = func(*window) { hook() } })
	t.Cleanup(release)
	f := file(1)
	w := cr.write(t, f, 0, "aaaa")
	cr.failByTimer(t, w.loc.Segment)
	<-reached

	cr.corrupt(t, w.c)
	release()
	if got := cr.awaitResolved(t); got != w.loc.Segment {
		t.Fatalf("resolved segment %d, want %d", got, w.loc.Segment)
	}
	wantSyncError(t, mustReturn(t, "first Sync", cr.syncAsync(context.Background(), f)), w.loc.Segment, f)
	cr.wantNilSync(t, f)
	cr.d.Stop()
	evs := cr.lossesOf(f)
	if len(evs) != 1 || evs[0].Reason != core.LossCorrupt {
		t.Fatalf("events = %+v, want only the corrupt loss", evs)
	}
	if recs := cr.recordsNaming(t, w.c.Seq); len(recs) != 1 || recs[0].Reason != format.LossCorrupt {
		t.Fatalf("loss records = %+v, want only the corrupt one", recs)
	}
}

// TestReadOnlyPassResolvesAWindowCorruptEmptied covers a window a read took
// the last record out of before the read-only pass reached it: the pass, with
// no record left to fail, resolves the window rather than leaving it pending.
func TestReadOnlyPassResolvesAWindowCorruptEmptied(t *testing.T) {
	prof.Watch(t, prof.Options{})
	hook, reached, release := gate(t)
	var park atomic.Bool
	cr := newCorruptRig(t, func(cr *corruptRig) {
		cr.d.afterPass = func() {
			if park.Load() {
				hook()
			}
		}
	})
	t.Cleanup(release)
	f := file(1)
	w := cr.write(t, f, 0, "aaaa")
	st := cr.d.made[0]
	// Park the worker after a pass, so the test makes the window and runs the
	// read-only pass in its place.
	park.Store(true)
	st.wake()
	<-reached

	win := &window{id: cr.d.windowSeq.Add(1), seg: w.loc.Segment, err: errors.New("sync boom")}
	st.mu.Lock()
	st.removeLocked(func(e *entry) bool { return e.w.Loc == w.loc }, func(e entry) { win.entries = append(win.entries, e) })
	st.windows = append(st.windows, win)
	st.mu.Unlock()
	if len(win.entries) != 1 {
		t.Fatalf("the window holds %d records, want the one written", len(win.entries))
	}
	cr.corrupt(t, w.c)

	cr.d.Enter(errors.New("read-only"))
	cr.d.failReadOnly(st)
	if got := cr.awaitResolved(t); got != w.loc.Segment {
		t.Fatalf("resolved segment %d, want %d", got, w.loc.Segment)
	}
	st.mu.Lock()
	pending := st.windowPendingLocked(win.id)
	st.mu.Unlock()
	if pending {
		t.Fatal("the emptied window is still pending")
	}
}

// TestCorruptInReadOnlyWindowReportsTheRefusal covers a record a read takes
// out of a failed segment's window once the journal is read-only, before the
// read-only pass reached the window: its file is told the window's failure
// with the read-only refusal, as the pass tells the file of a record it fails
// in that window, and the record is lost with the refusal too.
func TestCorruptInReadOnlyWindowReportsTheRefusal(t *testing.T) {
	prof.Watch(t, prof.Options{})
	hook, reached, release := gate(t)
	var park atomic.Bool
	cr := newCorruptRig(t, func(cr *corruptRig) {
		cr.d.afterPass = func() {
			if park.Load() {
				hook()
			}
		}
	})
	t.Cleanup(release)
	f, g := file(1), file(2)
	fw := cr.write(t, f, 0, "aaaa")
	gw := cr.write(t, g, 0, "bbbb")
	if fw.loc.Segment != gw.loc.Segment {
		t.Fatal("the two records landed in different segments")
	}
	st := cr.d.made[0]
	park.Store(true)
	st.wake()
	<-reached

	win := &window{id: cr.d.windowSeq.Add(1), seg: fw.loc.Segment, err: errors.New("sync boom")}
	st.mu.Lock()
	st.removeLocked(func(e *entry) bool { return e.w.Loc.Segment == fw.loc.Segment }, func(e entry) { win.entries = append(win.entries, e) })
	st.windows = append(st.windows, win)
	st.mu.Unlock()
	cr.d.Enter(errors.New("read-only"))

	cr.corrupt(t, fw.c)
	st.mu.Lock()
	l, lost := st.lost[fw.loc]
	st.mu.Unlock()
	if !lost || !errors.Is(l.err, core.ErrJournalReadOnly) {
		t.Fatalf("f's record lost = %v with %v, want lost with the read-only refusal", lost, l.err)
	}
	cr.d.failReadOnly(st)
	for _, w := range []written{fw, gw} {
		err := mustReturn(t, "Sync", cr.syncAsync(context.Background(), w.c.File))
		wantSyncError(t, err, w.loc.Segment, w.c.File)
		if !errors.Is(err, core.ErrJournalReadOnly) {
			t.Fatalf("Sync(%v) = %v, want the read-only refusal", w.c.File, err)
		}
	}
}

// TestCorruptAfterWorkerFailedRecordDoesNothing covers a record the worker
// already failed: Corrupt reports it dropped and adds nothing.
func TestCorruptAfterWorkerFailedRecordDoesNothing(t *testing.T) {
	prof.Watch(t, prof.Options{})
	cr := newCorruptRig(t, nil)
	f := file(1)
	w := cr.write(t, f, 0, "aaaa")
	cr.failSegmentOf(t, w.loc.Segment, f)
	cr.awaitResolved(t)
	cr.awaitRecord(t, w.c.Seq)
	before := cr.d.Generation()

	cr.corrupt(t, w.c)
	cr.d.Stop()
	if got := len(cr.lossesOf(f)); got != 1 {
		t.Fatalf("%d events name f, want the failed window's one", got)
	}
	if recs := cr.recordsNaming(t, w.c.Seq); len(recs) != 1 || recs[0].Reason != format.LossFailedSync {
		t.Fatalf("loss records = %+v, want the failed window's one", recs)
	}
	if got := cr.d.Generation(); got != before {
		t.Fatalf("the generation moved from %d to %d", before, got)
	}
}

// TestCorruptAppendsNothingAndReturnsBeforeSeal covers the read's side: with
// every write held at the seam, Corrupt returns, the generation is up, and a
// Sync of the file returns, while the loss record waits for the worker.
func TestCorruptAppendsNothingAndReturnsBeforeSeal(t *testing.T) {
	prof.Watch(t, prof.Options{})
	cr := newCorruptRig(t, nil)
	f := file(1)
	w := cr.write(t, f, 0, "aaaa")
	before := cr.d.Generation()
	writes := cr.seam.Calls(kit.OpWrite)
	hold := cr.seam.Hold(t, kit.OpWrite, "*")

	done := make(chan error, 1)
	go func() {
		_, err := cr.d.Corrupt(context.Background(), w.c)
		done <- err
	}()
	if err := mustReturn(t, "Corrupt", done); err != nil {
		t.Fatalf("Corrupt: %v", err)
	}
	if got := cr.d.Generation(); got != before+1 {
		t.Fatalf("the generation is %d when Corrupt returned, want %d", got, before+1)
	}
	if err := mustReturn(t, "Sync", cr.syncAsync(context.Background(), f)); err != nil {
		t.Fatalf("Sync = %v while the worker is held", err)
	}
	if got := cr.seam.Calls(kit.OpWrite); got > writes+1 {
		t.Fatalf("%d writes reached the seam, want at most the worker's one", got-writes)
	}
	hold.Release()
	cr.awaitRecord(t, w.c.Seq)
}

// TestCorruptThenStopWritesLossRecord covers a loss queued just before Stop
// that the worker never drained: Stop records it, so its record is on disk
// when Stop returns.
func TestCorruptThenStopWritesLossRecord(t *testing.T) {
	prof.Watch(t, prof.Options{})
	hook, reached, release := gate(t)
	var parked atomic.Bool
	cr := newCorruptRig(t, func(cr *corruptRig) {
		cr.d.afterSync = func() {
			if parked.Load() {
				hook()
			}
		}
	})
	t.Cleanup(release)
	f := file(1)
	w := cr.write(t, f, 0, "aaaa")
	// Park the worker inside a pass, past the step that drains the queue.
	parked.Store(true)
	syncing := cr.syncAsync(context.Background(), f)
	<-reached

	st := cr.d.made[0]
	// No wake is left for the worker to take another pass on.
	select {
	case <-st.kick:
	default:
	}
	job := corruptJob{record: w.c.Record, losses: []Loss{{
		Share: w.c.Share, File: f, Extent: w.c.Extent, Version: w.c.Version,
		Seq: w.c.Seq, Reason: core.LossCorrupt, Raised: true,
	}}}
	if !st.pushCorrupt(job) {
		t.Fatal("the stream refused the job before Stop")
	}

	stopped := make(chan struct{})
	go func() { cr.d.Stop(); close(stopped) }()
	eventually(t, "Stop to close the layer", func() bool {
		select {
		case <-cr.d.done:
			return true
		default:
			return false
		}
	})
	st.mu.Lock()
	queued := len(st.corrupt)
	st.mu.Unlock()
	if queued != 1 {
		t.Fatalf("%d jobs queued when Stop was called, want the one the worker never drained", queued)
	}
	release()
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("Stop did not return")
	}
	_ = mustReturn(t, "Sync", syncing)
	if recs := cr.recordsNaming(t, w.c.Seq); len(recs) != 1 {
		t.Fatalf("loss records naming the record after Stop = %+v, want 1", recs)
	}
}

// TestCorruptAfterStopStillRecordsTheLoss covers a Corrupt after Stop: no
// worker runs, and the loss is still recorded.
func TestCorruptAfterStopStillRecordsTheLoss(t *testing.T) {
	prof.Watch(t, prof.Options{})
	cr := newCorruptRig(t, nil)
	f := file(1)
	w := cr.write(t, f, 0, "aaaa")
	cr.d.Stop()

	cr.corrupt(t, w.c)
	if recs := cr.recordsNaming(t, w.c.Seq); len(recs) != 1 {
		t.Fatalf("loss records naming the record = %+v, want 1", recs)
	}
}

// TestCorruptHeadroomExhausted covers a loss record that cannot be appended:
// the drop stands, Corrupt reports it with no error, and the seq is listed.
func TestCorruptHeadroomExhausted(t *testing.T) {
	prof.Watch(t, prof.Options{})
	cr := newCorruptRig(t, func(cr *corruptRig) {
		cr.d.refuse = func() error { return core.ErrNoSpace }
	})
	f := file(1)
	w := cr.write(t, f, 0, "aaaa")

	cr.corrupt(t, w.c)
	if _, ok := cr.ix.Lookup(f, 0); ok {
		t.Fatal("the corrupt record still reads")
	}
	eventually(t, "the seq on the failed list", func() bool { return slices.Contains(cr.d.Failed(), w.c.Seq) })
	if recs := cr.recordsNaming(t, w.c.Seq); len(recs) != 0 {
		t.Fatalf("a refused loss record is on disk: %+v", recs)
	}
}

// corruptLocks is a Domains that counts the Locks Corrupt itself takes, told
// from the worker's by the caller's stack, and passes every Lock on.
type corruptLocks struct {
	inner Domains
	n     atomic.Int64
}

func (l *corruptLocks) Lock(id core.FileID) func() {
	pcs := make([]uintptr, 32)
	frames := runtime.CallersFrames(pcs[:runtime.Callers(2, pcs)])
	for {
		fr, more := frames.Next()
		if strings.HasSuffix(fr.Function, ".(*Durability).Corrupt") {
			l.n.Add(1)
		}
		if !more {
			break
		}
	}
	return l.inner.Lock(id)
}

// once returns f made safe to call more than once.
func once(f func()) func() {
	var o sync.Once
	return func() { o.Do(f) }
}

// TestCorruptDomainHeld covers a caller that holds the file's append domain:
// Corrupt takes it no second time and leaves it held.
func TestCorruptDomainHeld(t *testing.T) {
	prof.Watch(t, prof.Options{})
	byCorrupt := &corruptLocks{}
	cr := &corruptRig{failRig: &failRig{resolved: make(chan core.SegmentID, 64)}}
	cr.rig = newRigConfigured(t, 1, func(c *Config) {
		byCorrupt.inner = c.Domains
		c.Domains = byCorrupt
	})
	cr.d.Start(cr.streams)
	f := file(1)
	w := cr.write(t, f, 0, "aaaa")

	unlock := once(cr.domains.Lock(f))
	t.Cleanup(unlock)
	c := w.c
	c.DomainHeld = true
	done := make(chan error, 1)
	go func() {
		_, err := cr.d.Corrupt(context.Background(), c)
		done <- err
	}()
	if err := mustReturn(t, "Corrupt", done); err != nil {
		t.Fatalf("Corrupt: %v", err)
	}
	if n := byCorrupt.n.Load(); n != 0 {
		t.Fatalf("Corrupt took %d domains with DomainHeld", n)
	}
	if got := cr.domains.heldNow(); got != 1 {
		t.Fatalf("%d domains held on return, want the caller's one", got)
	}
	unlock()
	if _, ok := cr.ix.Lookup(f, 0); ok {
		t.Fatal("the corrupt record still reads")
	}
}

// TestCorruptOverwrittenBeforeDecisionIsNoLoss covers a record overwritten
// before Corrupt ran: the index no longer names it, and nothing is lost.
func TestCorruptOverwrittenBeforeDecisionIsNoLoss(t *testing.T) {
	prof.Watch(t, prof.Options{})
	cr := newCorruptRig(t, nil)
	f := file(1)
	old := cr.write(t, f, 0, "aaaa")
	newer := cr.write(t, f, 0, "bbbb")
	before := cr.d.Generation()

	cr.corrupt(t, old.c)
	cr.d.Stop()
	if p, ok := cr.ix.Lookup(f, 0); !ok || p.Version != newer.c.Version {
		t.Fatalf("the extent reads %+v, %v, want the newer write", p, ok)
	}
	if evs := cr.lossesOf(f); len(evs) != 0 {
		t.Fatalf("events = %+v, want none", evs)
	}
	if recs := cr.lossRecordsOnDisk(t); len(recs) != 0 {
		t.Fatalf("loss records = %+v, want none", recs)
	}
	if n := cr.marksOf(f); n != 0 {
		t.Fatalf("%d marks for f, want none", n)
	}
	if got := cr.d.Generation(); got != before {
		t.Fatalf("the generation moved from %d to %d", before, got)
	}
}

// TestCorruptRepeatedReadsOneLoss covers many reads finding one record of a
// pending window corrupt at once: one event, one loss record, one raise and
// one mark.
func TestCorruptRepeatedReadsOneLoss(t *testing.T) {
	prof.Watch(t, prof.Options{})
	cr, fw, _, release := pendingWindow(t)
	f := fw.c.File
	before := cr.d.Generation()

	const n = 16
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for range n {
		wg.Go(func() {
			out, err := cr.d.Corrupt(context.Background(), fw.c)
			if err == nil && out.Kind != CorruptDropped {
				err = errors.New("not dropped")
			}
			errs <- err
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("Corrupt: %v", err)
		}
	}
	if got := cr.d.Generation(); got != before+1 {
		t.Fatalf("the generation rose by %d for one record, want 1", got-before)
	}
	if got := cr.marksOf(f); got != 1 {
		t.Fatalf("%d marks for f, want 1", got)
	}
	release()
	cr.awaitResolved(t)
	cr.d.Stop()
	if evs := cr.lossesOf(f); len(evs) != 1 {
		t.Fatalf("events = %+v, want 1", evs)
	}
	if recs := cr.recordsNaming(t, fw.c.Seq); len(recs) != 1 {
		t.Fatalf("loss records = %+v, want 1", recs)
	}
}
