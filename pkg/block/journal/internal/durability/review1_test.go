package durability

import (
	"context"
	"errors"
	"slices"
	"sync/atomic"
	"testing"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/format"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/index"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/segment"
	"github.com/cubbit/dittofs/pkg/block/journal/journaltest/kit"
	"github.com/cubbit/dittofs/pkg/block/journal/journaltest/prof"
)

// failOnTwoStreams publishes a record of a file on each of two streams, fails
// the first sync of each, and returns the files, with the layer's windows
// recorded: every returned window has been failed by the worker, which is what
// Config.Resolved announces. gate, when set, is called by the worker with the
// segment of each window before the window is failed, and held, when set, is
// called with the two records' locations before any sync fails.
func failOnTwoStreams(t *testing.T, gate func(core.SegmentID), held func(lf, lg segment.Location)) (r *rig, f, g core.FileID, resolved *recorder[resolvedAt]) {
	t.Helper()
	resolved = &recorder[resolvedAt]{}
	r = newRigConfigured(t, 2, func(c *Config) {
		c.Resolved = func(stream uint32, seg core.SegmentID) { resolved.add(resolvedAt{stream, seg}) }
	})
	if gate != nil {
		r.d.beforeFail = func(w *window) { gate(w.seg) }
	}
	r.d.Start(r.streams)
	f, g = r.fileOn(t, 0), r.fileOn(t, 1)
	lf, _ := r.publish(t, f, 0, "aaaa")
	lg, _ := r.publish(t, g, 0, "gggg")
	if held != nil {
		held(lf, lg)
	}
	if err := r.seam.Schedule(
		kit.Failing(kit.OpSync, format.SegmentName(lf.Segment), 1, errors.New("boom f")),
		kit.Failing(kit.OpSync, format.SegmentName(lg.Segment), 1, errors.New("boom g")),
	); err != nil {
		t.Fatal(err)
	}
	r.stamped(t, 0)
	r.stamped(t, 1)
	eventually(t, "both syncs to fail", func() bool {
		r.clock.Advance(2 * core.SyncBound)
		return r.d.Stats().SyncFailures >= 2
	})
	return r, f, g, resolved
}

// TestASyncNamingFilesOnTwoStreamsReportsBothFailures pins that across streams
// one Sync names every file the failures of both streams took, and
// clears the mark of each, so no later Sync reports an old failure again.
func TestASyncNamingFilesOnTwoStreamsReportsBothFailures(t *testing.T) {
	prof.Watch(t, prof.Options{})
	r, f, g, resolved := failOnTwoStreams(t, nil, nil)
	eventually(t, "both windows to be failed", func() bool { return len(resolved.all()) == 2 })

	err := mustReturn(t, "Sync of both", r.syncAsync(context.Background(), f, g))
	var se *core.SyncError
	if !errors.As(err, &se) {
		t.Fatalf("Sync = %v, want a *core.SyncError", err)
	}
	if !slices.Contains(se.Files, f) || !slices.Contains(se.Files, g) {
		t.Errorf("SyncError.Files = %v, want both files", se.Files)
	}
	for _, id := range []core.FileID{f, g} {
		if err := mustReturn(t, "Sync after the report", r.syncAsync(context.Background(), id)); err != nil {
			t.Errorf("Sync(%v) after the report = %v, want nil: its mark was left set", id, err)
		}
	}
}

// TestASyncWaitsForAFailureInFlightOnAnotherStream pins that a Sync naming
// files of two streams, whose first stream has failed its window, does not
// return that failure alone while the other stream's window is taken but not
// yet failed: the call names both files at once, and the second file is not
// left to be told by its next Sync.
func TestASyncWaitsForAFailureInFlightOnAnotherStream(t *testing.T) {
	prof.Watch(t, prof.Options{})
	var heldSeg atomic.Uint64
	gate, open := releaser(t)
	// The window of g's stream waits at the gate before it is failed; f's does
	// not.
	r, f, g, resolved := failOnTwoStreams(t, func(seg core.SegmentID) {
		if heldSeg.Load() == uint64(seg) {
			<-gate
		}
	}, func(_, lg segment.Location) { heldSeg.Store(uint64(lg.Segment)) })
	// Registered after the rig's own cleanups, so it runs before the layer
	// stops: a worker parked at the gate would stop the stop.
	t.Cleanup(open)
	eventually(t, "f's window to be failed", func() bool { return len(resolved.all()) == 1 })

	done := r.syncAsync(context.Background(), f, g)
	mustBlock(t, "Sync of both files with g's failure in flight", done)
	open()
	err := mustReturn(t, "Sync of both", done)
	var se *core.SyncError
	if !errors.As(err, &se) {
		t.Fatalf("Sync = %v, want a *core.SyncError", err)
	}
	if !slices.Contains(se.Files, f) || !slices.Contains(se.Files, g) {
		t.Errorf("SyncError.Files = %v, want both files", se.Files)
	}
	if err := mustReturn(t, "Sync of g after the report", r.syncAsync(context.Background(), g)); err != nil {
		t.Errorf("Sync(g) after the report = %v, want nil: its mark was left set", err)
	}
}

// TestASyncWaitsForAFailureInFlightOnItsOwnStream pins the same wait when both
// files live on one stream: f's failure is already waiting as a mark, h's
// window is taken but not yet failed, and a Sync naming both returns only once
// it names h as well, as it does when the files are on two streams.
func TestASyncWaitsForAFailureInFlightOnItsOwnStream(t *testing.T) {
	prof.Watch(t, prof.Options{})
	var heldSeg atomic.Uint64
	gate, open := releaser(t)
	resolved := &recorder[resolvedAt]{}
	r := newRigConfigured(t, 1, func(c *Config) {
		c.Resolved = func(stream uint32, seg core.SegmentID) { resolved.add(resolvedAt{stream, seg}) }
	})
	r.d.beforeFail = func(w *window) {
		if heldSeg.Load() == uint64(w.seg) {
			<-gate
		}
	}
	r.d.Start(r.streams)
	// Registered after the rig's own cleanups, so it runs before the layer
	// stops: a worker parked at the gate would stop the stop.
	t.Cleanup(open)

	failed := func(f core.FileID, n int) segment.Location {
		loc, _ := r.publish(t, f, int64(n)*4, "aaaa")
		r.failSyncOf(t, loc.Segment, errors.New("boom"))
		r.stamped(t, 0)
		eventually(t, "the sync to fail", func() bool {
			r.clock.Advance(2 * core.SyncBound)
			return r.d.Stats().SyncFailures >= uint64(n+1)
		})
		return loc
	}
	f, h := file(1), file(2)
	failed(f, 0)
	eventually(t, "f's window to be failed", func() bool { return len(resolved.all()) == 1 })
	// h's segment is the one the stream rolled to after f's failure.
	locH, _ := r.publish(t, h, 4, "hhhh")
	heldSeg.Store(uint64(locH.Segment))
	r.failSyncOf(t, locH.Segment, errors.New("boom h"))
	r.stamped(t, 0)
	eventually(t, "h's window to be taken", func() bool {
		r.clock.Advance(2 * core.SyncBound)
		return len(r.windowsOf(0)) > 0
	})

	done := r.syncAsync(context.Background(), f, h)
	mustBlock(t, "Sync of both files with h's failure in flight", done)
	open()
	err := mustReturn(t, "Sync of both", done)
	var se *core.SyncError
	if !errors.As(err, &se) {
		t.Fatalf("Sync = %v, want a *core.SyncError", err)
	}
	if !slices.Contains(se.Files, f) || !slices.Contains(se.Files, h) {
		t.Errorf("SyncError.Files = %v, want both files", se.Files)
	}
	if err := mustReturn(t, "Sync of h after the report", r.syncAsync(context.Background(), h)); err != nil {
		t.Errorf("Sync(h) after the report = %v, want nil: its mark was left set", err)
	}
}

// TestASyncThatEndsEarlyStillReportsTheMarksItTook pins that when a later
// stream answers core.ErrClosed after an earlier one gave up its marks, the
// call returns the failure it took, not the closed error: nothing else would
// report it, its mark being cleared.
func TestASyncThatEndsEarlyStillReportsTheMarksItTook(t *testing.T) {
	prof.Watch(t, prof.Options{})
	r, f, g, resolved := failOnTwoStreams(t, nil, nil)
	eventually(t, "both windows to be failed", func() bool { return len(resolved.all()) == 2 })
	// g's stream closes under a record of another file, h, that no sync
	// reached and that owes no failure.
	var h core.FileID
	for b := 1; b < 255; b++ {
		if c := file(byte(b)); c != g && r.streams.For(c) == r.streams.Stream(1) {
			h = c
			break
		}
	}
	r.publish(t, h, 4, "hhhh")
	st1 := r.d.made[1]
	st1.mu.Lock()
	st1.closed = true
	st1.mu.Unlock()

	// The streams are consulted in the order given, f's first, so f's mark is
	// taken before g's stream answers.
	ids := []core.FileID{f, h}
	ws := []work{
		{st: r.d.made[0], ids: ids, targets: r.d.made[0].targetsOf(ids)},
		{st: st1, ids: ids, targets: st1.targetsOf(ids)},
	}
	err := r.d.await(context.Background(), ws, true)
	var se *core.SyncError
	if !errors.As(err, &se) || !slices.Contains(se.Files, f) {
		t.Fatalf("await = %v, want the SyncError naming f, whose mark it took", err)
	}
}

// TestMergedFailuresNameTheOldestWindow pins that the failure a Sync across
// streams names is the one of the oldest window, whichever stream it came
// from: window ids are global, the streams are consulted in no fixed order.
func TestMergedFailuresNameTheOldestWindow(t *testing.T) {
	older := func() *owedFailure {
		return &owedFailure{err: &core.SyncError{Segment: 2, Err: errors.New("older"), Files: []core.FileID{file(1)}}, window: 3}
	}
	newer := func() *owedFailure {
		return &owedFailure{err: &core.SyncError{Segment: 5, Err: errors.New("newer"), Files: []core.FileID{file(2)}}, window: 9}
	}
	for name, got := range map[string]*owedFailure{
		"older first": mergeOwed(older(), newer()),
		"newer first": mergeOwed(newer(), older()),
	} {
		if got.err.Segment != 2 || got.err.Err.Error() != "older" || got.window != 3 {
			t.Errorf("%s: merged failure names segment %d (%v) of window %d, want the older window's", name, got.err.Segment, got.err.Err, got.window)
		}
		if len(got.err.Files) != 2 {
			t.Errorf("%s: merged files = %v, want both", name, got.err.Files)
		}
	}
}

// TestTwoWaitersOnAFailedRecordReturnAndOneIsTold pins that a Sync whose target
// was failed, and whose file's mark another Sync took, returns instead of
// waiting for a record that is gone.
func TestTwoWaitersOnAFailedRecordReturnAndOneIsTold(t *testing.T) {
	prof.Watch(t, prof.Options{})
	r := newRigUnstarted(t, 1)
	gate, open := releaser(t)
	r.d.beforeFail = func(*window) { <-gate }
	t.Cleanup(open)
	r.d.Start(r.streams)
	f := file(1)
	loc, _ := r.publish(t, f, 0, "aaaa")
	r.failSyncOf(t, loc.Segment, errors.New("sync boom"))
	first, second := r.syncAsync(context.Background(), f), r.syncAsync(context.Background(), f)
	// Both waiters hold the failed record as their target before the window
	// is failed.
	eventually(t, "the window to be taken", func() bool { return len(r.windowsOf(0)) > 0 })
	open()

	told := 0
	for _, ch := range []<-chan error{first, second} {
		err := mustReturn(t, "a waiter", ch)
		if err == nil {
			continue
		}
		wantSyncError(t, err, loc.Segment, f)
		told++
	}
	if told != 1 {
		t.Errorf("%d waiters were told of the failure, want exactly one", told)
	}
}

// TestALossRecordWhoseSyncFailsEndsItsSegment covers the stream not wedging
// when the sync of a loss record fails: the next record is refused or its
// Sync is answered, and never left waiting.
func TestALossRecordWhoseSyncFailsEndsItsSegment(t *testing.T) {
	prof.Watch(t, prof.Options{})
	fr := newFailRig(t, nil)
	f, g := file(1), file(2)
	a, _ := fr.publish(t, f, 0, "aaaa")
	if err := fr.seam.Schedule(
		kit.Failing(kit.OpSync, format.SegmentName(a.Segment), 1, errors.New("boom 1")),
		kit.Failing(kit.OpSync, format.SegmentName(a.Segment+1), 1, errors.New("boom 2")),
	); err != nil {
		t.Fatal(err)
	}
	_ = mustReturn(t, "Sync of f", fr.syncAsync(context.Background(), f))

	// The loss record went to the spare, whose sync failed. A write after it
	// either is refused by the stopped stream or gets its Sync answered.
	v, err := fr.d.counters.Draw()
	if err != nil {
		t.Fatal(err)
	}
	_, err = fr.streams.For(g).Append(format.RecordHeader{
		Kind: format.KindWrite, File: g, Extent: core.Extent{Len: 4}, Version: v, Tag: 1,
	}, format.RecordBody{Payload: []byte("gggg"), MTime: 1})
	if err != nil {
		if !errors.Is(err, segment.ErrStreamStopped) {
			t.Fatalf("Append after the loss record = %v", err)
		}
		return
	}
	_ = mustReturn(t, "Sync of g", fr.syncAsync(context.Background(), g))
}

// TestACoverStillInALaterWindowIsNotLiftedWhenAnEarlierOneFails pins that the
// cover of a failed record, a newer record that took over what it had
// superseded, is lifted only when no window or ledger entry of the stream
// still holds it: here the cover's own window fails next, and the extent the
// first failure restored has to be held again for a reader.
func TestACoverStillInALaterWindowIsNotLiftedWhenAnEarlierOneFails(t *testing.T) {
	prof.Watch(t, prof.Options{})
	// No worker runs, so the test takes the two ends and fails the windows
	// itself, with both windows pending together.
	r := newRigUnstarted(t, 1)
	f := file(1)
	st := r.d.made[0]
	r.streams.OnEnd(r.d.onEnd)

	_, v1 := r.publish(t, f, 0, "aaaa")
	r.syncStream(t, f)
	a, _ := r.publish(t, f, 0, "bbbb")
	r.failNextWrite(t, a.Segment, errors.New("write boom a"))
	r.appendFailing(t, f, 100)
	r.settlePool(t)
	b, _ := r.publish(t, f, 0, "cccc")
	if b.Segment == a.Segment {
		t.Fatal("the append after the failed write stayed in the failed segment")
	}
	r.failNextWrite(t, b.Segment, errors.New("write boom b"))
	r.appendFailing(t, f, 100)

	eventually(t, "both ends to be queued", func() bool {
		st.mu.Lock()
		defer st.mu.Unlock()
		return len(st.ends) == 2
	})
	r.d.takeWindows(st)
	if n := len(r.windowsOf(0)); n != 2 {
		t.Fatalf("%d windows pending, want both", n)
	}
	r.d.resolveWindows(st)

	if p, ok := r.ix.Lookup(f, 0); !ok || p.Version != v1 {
		t.Fatalf("after both windows failed Lookup serves %+v, %v, want v1's piece held again", p, ok)
	}
}

// TestASyncOfARecordASyncAlreadyCoveredWaitsForTheLift pins that a Sync issued
// after a sync covered the record, but before the worker lifted what the
// record superseded, does not return until the lift: a Sync that returned has
// seen the superseded extent released.
func TestASyncOfARecordASyncAlreadyCoveredWaitsForTheLift(t *testing.T) {
	prof.Watch(t, prof.Options{})
	r := newRig(t, 1)
	f := file(1)
	r.publish(t, f, 0, "aaaa")
	if err := r.d.Sync(context.Background(), f); err != nil {
		t.Fatalf("Sync of v1: %v", err)
	}
	r.publish(t, f, 0, "bbbb")

	// The test holds the file's domain, so the worker, which lifts under it,
	// stops there once a sync has covered v2.
	r.domains.mu.Lock()
	before := r.domains.taken
	r.domains.mu.Unlock()
	unlock := r.domains.Lock(f)
	released := false
	release := func() {
		if !released {
			released = true
			unlock()
		}
	}
	t.Cleanup(release)
	r.syncStream(t, f)
	eventually(t, "the worker to ask for the file's domain", func() bool {
		r.domains.mu.Lock()
		defer r.domains.mu.Unlock()
		return r.domains.taken > before+1
	})

	done := r.syncAsync(context.Background(), f)
	mustBlock(t, "Sync of a covered record whose lift is pending", done)
	release()
	if err := mustReturn(t, "Sync", done); err != nil {
		t.Fatalf("Sync = %v", err)
	}
	if got := r.ix.Retained(f); len(got) != 0 {
		t.Fatalf("after the Sync %+v is still retained", got)
	}
}

// TestACoverIsHeldOnlyByTheRecordOfItsOwnExtent pins that a sibling record of
// the cover's version, a piece of the same split write that still sits in the
// ledger, does not stop the lift of a part another piece covers: the ledger
// holds the cover only when an entry's extent holds the part.
func TestACoverIsHeldOnlyByTheRecordOfItsOwnExtent(t *testing.T) {
	r := newRigUnstarted(t, 1)
	st := r.d.made[0]
	f := file(1)
	v := core.Version{Counter: 3}
	st.mu.Lock()
	st.ledger = append(st.ledger, entry{w: segment.Written{
		Kind: format.KindWrite, File: f, Version: v, Extent: core.Extent{Off: 4, Len: 4},
	}})
	st.mu.Unlock()

	if r.d.hasEntry(st, f, index.Cover{Version: v, Extent: core.Extent{Off: 0, Len: 4}}) {
		t.Error("a sibling record's entry was taken for the record that covers [0,4)")
	}
	if !r.d.hasEntry(st, f, index.Cover{Version: v, Extent: core.Extent{Off: 4, Len: 2}}) {
		t.Error("the record whose extent holds the cover was not found")
	}
}

// TestASyncWaitsForAFailureInFlightWhileAHeaderOnlyRecordIsCarriedForward pins
// that a Sync naming a file whose header-only record was re-appended still
// waits while another file's failed window on the same stream is being
// handled: f's delete is re-appended, g's write in the next segment is failed
// and its window held, and a Sync naming both returns only after g's window is
// failed, with the generation raised, and names g alone: f's record was
// carried forward, not lost.
func TestASyncWaitsForAFailureInFlightWhileAHeaderOnlyRecordIsCarriedForward(t *testing.T) {
	prof.Watch(t, prof.Options{})
	var heldSeg atomic.Uint64
	gate, open := releaser(t)
	resolved := &recorder[resolvedAt]{}
	r := newRigConfigured(t, 1, func(c *Config) {
		c.Resolved = func(stream uint32, seg core.SegmentID) { resolved.add(resolvedAt{stream, seg}) }
	})
	r.d.beforeFail = func(w *window) {
		if heldSeg.Load() == uint64(w.seg) {
			<-gate
		}
	}
	r.d.Start(r.streams)
	t.Cleanup(open)

	f, g := file(1), file(2)
	delLoc := r.appendKind(t, f, format.KindDelete, format.RecordBody{})
	r.failSyncOf(t, delLoc.Segment, errors.New("boom f"))
	r.stamped(t, 0)
	eventually(t, "f's sync to fail", func() bool {
		r.clock.Advance(2 * core.SyncBound)
		return r.d.Stats().SyncFailures >= 1
	})
	// The window resolves: its header-only record is re-appended, not failed.
	eventually(t, "f's window to be resolved", func() bool {
		for _, got := range resolved.all() {
			if got.seg == delLoc.Segment {
				return true
			}
		}
		return false
	})

	// A sync succeeds in the spare first, so g's failure is not the second
	// segment in a row the device refused, which would turn the journal
	// read-only (RFC 1 section 5.4) and fail every record in one pass.
	h := file(3)
	locH, _ := r.publish(t, h, 0, "hhhh")
	if err := r.d.Sync(context.Background(), h); err != nil {
		t.Fatalf("Sync of h in the spare: %v", err)
	}
	locG, _ := r.publish(t, g, 4, "gggg")
	if locG.Segment != locH.Segment {
		t.Fatalf("g's write in segment %d, want h's %d", locG.Segment, locH.Segment)
	}
	heldSeg.Store(uint64(locG.Segment))
	r.failSyncOf(t, locG.Segment, errors.New("boom g"))
	r.stamped(t, 0)
	eventually(t, "g's window to be taken", func() bool {
		r.clock.Advance(2 * core.SyncBound)
		return r.d.Stats().SyncFailures >= 2 && len(r.windowsOf(0)) > 0
	})

	before := r.d.Generation()
	done := r.syncAsync(context.Background(), f, g)
	mustBlock(t, "Sync of both files with g's failure in flight", done)
	open()
	err := mustReturn(t, "Sync of both", done)
	gen := r.d.Generation()
	var se *core.SyncError
	if !errors.As(err, &se) {
		t.Fatalf("Sync = %v, want a *core.SyncError", err)
	}
	if !slices.Contains(se.Files, g) || slices.Contains(se.Files, f) {
		t.Errorf("SyncError.Files = %v, want g alone: f's record was re-appended", se.Files)
	}
	if gen <= before {
		t.Errorf("generation %d after the Sync, was %d before g's window was failed: the Sync returned before the raise", gen, before)
	}
}

// TestAFailedWindowLiftsACoverOverItsOwnExtentOnly pins the lift the worker
// makes for the cover of a failed record, here one record of a write split in
// two: the first record has synced, so its part is lifted, and the second is
// still in the ledger, so the part it was handed stays retained until its own
// sync. The lift names the cover's extent, and not the whole file, which would
// drop the second record's part.
func TestAFailedWindowLiftsACoverOverItsOwnExtentOnly(t *testing.T) {
	prof.Watch(t, prof.Options{})
	// No worker runs, so the test takes the window, completes the first
	// record's sync and fails the window itself, in that order.
	r := newRigUnstarted(t, 1)
	st := r.d.made[0]
	r.streams.OnEnd(r.d.onEnd)
	f := file(1)

	r.publish(t, f, 0, "aaaaaaaa")
	r.syncStream(t, f)
	r.d.complete(st)
	// The failing record: it supersedes v1 and fails with its segment.
	a, _ := r.publish(t, f, 0, "bbbbbbbb")
	r.failNextWrite(t, a.Segment, errors.New("write boom"))
	r.appendFailing(t, f, 100)
	eventually(t, "the end to be queued", func() bool {
		st.mu.Lock()
		defer st.mu.Unlock()
		return len(st.ends) == 1
	})
	r.d.takeWindows(st)
	if n := len(r.windowsOf(0)); n != 1 {
		t.Fatalf("%d windows pending, want one", n)
	}

	// A write split in two records of one version, over the failing record's
	// extent: the first syncs and completes, the second stays in the ledger.
	v3, err := r.d.counters.Draw()
	if err != nil {
		t.Fatalf("Draw: %v", err)
	}
	r.publishAs(t, f, 0, "cccc", v3, nil)
	r.syncStream(t, f)
	r.d.complete(st)
	r.publishAs(t, f, 4, "dddd", v3, nil)
	if n := r.ledgerLen(0); n != 1 {
		t.Fatalf("ledger holds %d entries, want the second record alone", n)
	}

	r.d.resolveWindows(st)
	got := r.retainedBy(f, v3)
	if len(got) == 0 {
		t.Fatalf("nothing is retained by the second record, whose sync has not completed: its part was lifted with the first record's")
	}
	for _, p := range got {
		if p.Extent.Off < 4 {
			t.Errorf("the first record's part %+v is still retained: it synced", p)
		}
	}
}
