package durability

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/format"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/index"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/segment"
	"github.com/cubbit/dittofs/pkg/block/journal/journaltest"
	"github.com/cubbit/dittofs/pkg/block/journal/journaltest/prof"
)

// undoLog records every record Config.Undo was handed, with how many file
// domains were held while it ran.
type undoLog struct {
	mu    sync.Mutex
	calls []segment.Written
	held  []int
}

func (u *undoLog) all() []segment.Written {
	u.mu.Lock()
	defer u.mu.Unlock()
	return slices.Clone(u.calls)
}

// undoDelta is the delta the test Undo returns for a record of file f, so the
// account log shows it came from the undo.
func undoDelta(f core.FileID) index.HeldDelta {
	return index.HeldDelta{Tag: 1, File: f, HeldBytes: 7}
}

// newRORig builds a started rig over n streams on dir (a fresh one when
// empty) whose Undo is recorded.
func newRORig(t *testing.T, n int, dir string) (*rig, *undoLog) {
	t.Helper()
	u := &undoLog{}
	var r *rig
	r = newRigConfigured(t, n, func(c *Config) {
		if dir != "" {
			c.Dir = dir
		}
		c.Undo = func(w segment.Written) index.HeldDelta {
			u.mu.Lock()
			defer u.mu.Unlock()
			u.calls = append(u.calls, w)
			u.held = append(u.held, r.domains.heldNow())
			return undoDelta(w.File)
		}
	})
	r.d.Start(r.streams)
	return r, u
}

// appendHeader appends a header-only record of kind k to f under f's append
// domain, as a removal does, and returns its place and sequence number.
func (r *rig) appendHeader(t *testing.T, f core.FileID, k format.Kind, body format.RecordBody) (segment.Location, core.Seq) {
	t.Helper()
	v, err := r.d.counters.Draw()
	if err != nil {
		t.Fatalf("Draw: %v", err)
	}
	seq, err := r.d.counters.NextSeq()
	if err != nil {
		t.Fatalf("NextSeq: %v", err)
	}
	var x core.Extent
	if k == format.KindDeallocate {
		x = core.Extent{Off: 0, Len: 4} // a punch names a range
	}
	unlock := r.domains.Lock(f)
	defer unlock()
	p, err := r.streams.For(f).Append(format.RecordHeader{Kind: k, File: f, Extent: x, Seq: seq, Version: v, Tag: 1}, body)
	if err != nil {
		t.Fatalf("Append of %v: %v", k, err)
	}
	return p.Loc, seq
}

// waitDurableAsync runs WaitDurable in a goroutine and returns the channel its
// error lands on.
func (r *rig) waitDurableAsync(at segment.Location) <-chan error {
	ch := make(chan error, 1)
	go func() { ch <- r.d.WaitDurable(at) }()
	return ch
}

// wantReadOnly fails the test unless err is the read-only failure, carrying
// the cause the journal entered with.
func wantReadOnly(t *testing.T, what string, err error) {
	t.Helper()
	if !errors.Is(err, core.ErrJournalReadOnly) {
		t.Fatalf("%s = %v, want core.ErrJournalReadOnly", what, err)
	}
	if !errors.Is(err, syscall.EROFS) {
		t.Errorf("%s = %v, want the read-only cause reachable", what, err)
	}
}

// wantReadOnlySyncError fails the test unless err is the read-only failure of
// a window that names f.
func wantReadOnlySyncError(t *testing.T, what string, err error, f core.FileID) {
	t.Helper()
	wantReadOnly(t, what, err)
	var se *core.SyncError
	if !errors.As(err, &se) || !slices.Contains(se.Files, f) {
		t.Errorf("%s = %v, want a *core.SyncError naming the file", what, err)
	}
}

// accounted reports whether the account log holds hd.
func (r *rig) accounted(hd index.HeldDelta) bool {
	return slices.Contains(r.acct.all(), hd)
}

// TestEnteringReadOnlyFailsTheUnsyncedContentOfEveryStream covers RFC 1 §5.4:
// every stream's unsynced content is failed, each file is
// told through its Sync, the generation rises once for all of them, the
// events are published and no loss record is appended.
func TestEnteringReadOnlyFailsTheUnsyncedContentOfEveryStream(t *testing.T) {
	prof.Watch(t, prof.Options{})
	r, _ := newRORig(t, 2, "")
	f0, f1 := r.fileOn(t, 0), r.fileOn(t, 1)
	loc0, v0 := r.publish(t, f0, 0, "aaaa")
	loc1, v1 := r.publish(t, f1, 0, "bbbb")
	gen := r.d.Generation()
	writes := func() int {
		return r.seam.CallsFor(journaltest.OpWrite, format.SegmentName(loc0.Segment)) +
			r.seam.CallsFor(journaltest.OpWrite, format.SegmentName(loc1.Segment))
	}
	before := writes()

	r.d.Enter(syscall.EROFS)
	wantReadOnlySyncError(t, "Sync of stream 0's file", mustReturn(t, "Sync", r.syncAsync(context.Background(), f0)), f0)
	wantReadOnlySyncError(t, "Sync of stream 1's file", mustReturn(t, "Sync", r.syncAsync(context.Background(), f1)), f1)

	if got := r.d.Generation(); got != gen+1 {
		t.Errorf("Generation = %d, want %d: once for every stream's failed records", got, gen+1)
	}
	evs := r.events()
	if len(evs) != 2 {
		t.Fatalf("%d loss events, want 2: %+v", len(evs), evs)
	}
	for _, want := range []struct {
		f core.FileID
		v core.Version
	}{{f0, v0}, {f1, v1}} {
		if !slices.ContainsFunc(evs, func(ev core.LossEvent) bool {
			return ev.File == want.f && ev.Version == want.v && ev.Reason == core.LossFailedWindow
		}) {
			t.Errorf("no failed-window event for %v at %v: %+v", want.f, want.v, evs)
		}
	}
	if got := writes() - before; got != 0 {
		t.Errorf("%d writes reached the segments after entry, want none", got)
	}
	if recs := r.lossRecordsOnDisk(t); len(recs) != 0 {
		t.Errorf("durable loss records %+v, want none", recs)
	}
	for _, loc := range []segment.Location{loc0, loc1} {
		if r.d.Durable(loc) {
			t.Errorf("failed record %+v reads durable", loc)
		}
	}
	if p, ok := r.ix.Lookup(f0, 0); ok {
		t.Errorf("the index still serves the failed write: %+v", p)
	}
}

// TestEnteringReadOnlyUndoesAHeaderOnlyRecord covers the in-memory undo of a
// header-only record and its accounting: a truncate is undone once, under its file's append domain, its delta reaches Account,
// its Sync fails with ErrJournalReadOnly, and its sequence number is listed.
func TestEnteringReadOnlyUndoesAHeaderOnlyRecord(t *testing.T) {
	prof.Watch(t, prof.Options{})
	r, undo := newRORig(t, 1, "")
	f, g := file(1), file(2)
	loc, seq := r.appendHeader(t, f, format.KindTruncate, format.RecordBody{})
	gen := r.d.Generation()

	r.d.Enter(syscall.EROFS)
	// The Sync is issued once the pass has undone the truncate, so it waits on
	// nothing and must still hear of it.
	eventually(t, "the truncate to be undone", func() bool { return len(undo.all()) > 0 })
	wantReadOnly(t, "Sync of the truncated file", mustReturn(t, "Sync", r.syncAsync(context.Background(), f)))

	calls := undo.all()
	if len(calls) != 1 || calls[0].Loc != loc || calls[0].Kind != format.KindTruncate {
		t.Fatalf("Undo saw %+v, want the truncate once", calls)
	}
	if undo.held[0] < 1 {
		t.Error("Undo ran with no file append domain held")
	}
	if !r.accounted(undoDelta(f)) {
		t.Errorf("the undo's delta never reached Account: %+v", r.acct.all())
	}
	if !slices.Contains(r.d.Failed(), seq) {
		t.Errorf("Failed() = %v, want the truncate's seq %d", r.d.Failed(), seq)
	}
	if r.d.Durable(loc) {
		t.Error("the undone truncate reads durable")
	}
	if got := r.d.Generation(); got != gen+1 {
		t.Errorf("Generation = %d, want %d", got, gen+1)
	}

	// A later pass, run for a record of another file, undoes nothing again.
	r.publish(t, g, 0, "gggg")
	_ = mustReturn(t, "Sync of a later file", r.syncAsync(context.Background(), g))
	if got := len(undo.all()); got != 1 {
		t.Errorf("Undo called %d times, want once", got)
	}
}

// TestHeaderOnlyRecordsAreUndoneNewestFirst pins the order: two removals of
// one file are undone in the reverse of the order they were appended, so each
// undo finds the index as its record left it.
func TestHeaderOnlyRecordsAreUndoneNewestFirst(t *testing.T) {
	prof.Watch(t, prof.Options{})
	r, undo := newRORig(t, 1, "")
	f := file(1)
	first, _ := r.appendHeader(t, f, format.KindTruncate, format.RecordBody{})
	second, _ := r.appendHeader(t, f, format.KindDeallocate, format.RecordBody{})

	r.d.Enter(syscall.EROFS)
	wantReadOnly(t, "WaitDurable", mustReturn(t, "WaitDurable", r.waitDurableAsync(first)))
	calls := undo.all()
	if len(calls) != 2 || calls[0].Loc != second || calls[1].Loc != first {
		t.Fatalf("Undo saw %+v, want the deallocate then the truncate", calls)
	}
}

// TestEverySyncWaitingOnAnUndoneRecordFails covers RFC 1 §5.4's "every Sync
// that waits on one" of an undone record: two Syncs waiting on one truncate when the journal turns
// read-only both fail, though the failure is reported to the file once.
func TestEverySyncWaitingOnAnUndoneRecordFails(t *testing.T) {
	prof.Watch(t, prof.Options{})
	r, _ := newRORig(t, 1, "")
	f := file(1)
	loc, _ := r.appendHeader(t, f, format.KindTruncate, format.RecordBody{})
	hold := r.seam.HoldFirst(t, journaltest.OpSync, format.SegmentName(loc.Segment))
	a := r.syncAsync(context.Background(), f)
	select {
	case <-hold.Reached():
	case <-time.After(5 * time.Second):
		t.Fatal("the truncate's sync never reached storage")
	}
	b := r.syncAsync(context.Background(), f)
	mustBlock(t, "the second Sync", b)

	r.d.Enter(syscall.EROFS)
	hold.Release()
	wantReadOnly(t, "the first Sync", mustReturn(t, "the first Sync", a))
	wantReadOnly(t, "the second Sync", mustReturn(t, "the second Sync", b))
}

// TestEnteringReadOnlyLeavesALossRecordsDropStanding covers RFC 1 §5.4's "only
// a loss record's drop stands": an unsynced loss record is not undone, not
// listed, raises nothing and fails no Sync.
func TestEnteringReadOnlyLeavesALossRecordsDropStanding(t *testing.T) {
	prof.Watch(t, prof.Options{})
	r, undo := newRORig(t, 1, "")
	f := file(1)
	_, seq := r.appendHeader(t, f, format.KindLoss, format.RecordBody{Loss: format.Loss{Dropped: 5, Reason: format.LossFailedSync}})
	gen := r.d.Generation()

	r.d.Enter(syscall.EROFS)
	eventually(t, "the loss record to leave the ledger", func() bool { return r.ledgerLen(0) == 0 })
	if err := mustReturn(t, "Sync", r.syncAsync(context.Background(), f)); err != nil {
		t.Errorf("Sync of the loss record's file = %v, want nil", err)
	}
	if got := undo.all(); len(got) != 0 {
		t.Errorf("Undo saw %+v, want no loss record undone", got)
	}
	if slices.Contains(r.d.Failed(), seq) {
		t.Errorf("Failed() = %v lists the loss record's own seq %d", r.d.Failed(), seq)
	}
	if got := r.d.Generation(); got != gen {
		t.Errorf("Generation = %d, want %d: a failed loss record loses nothing", got, gen)
	}
}

// TestARecordTrackedAfterTheFailurePassIsFailedByTheNext covers an append
// whose Admit passed just before entry and that lands after the pass: the
// next pass fails it, and the generation is not raised again.
func TestARecordTrackedAfterTheFailurePassIsFailedByTheNext(t *testing.T) {
	prof.Watch(t, prof.Options{})
	r, undo := newRORig(t, 1, "")
	f, g := file(1), file(2)
	r.publish(t, f, 0, "aaaa")
	gen := r.d.Generation()
	r.d.Enter(syscall.EROFS)
	wantReadOnlySyncError(t, "Sync of the first file", mustReturn(t, "Sync", r.syncAsync(context.Background(), f)), f)

	locG, vG := r.publish(t, g, 0, "gggg")
	hloc, _ := r.appendHeader(t, g, format.KindDeallocate, format.RecordBody{})
	// The write's own Track may run a pass before the deallocate lands, and
	// the Sync then answers the write's report alone: the deallocate's wait is
	// what follows it.
	wantReadOnlySyncError(t, "Sync of the late file", mustReturn(t, "Sync", r.syncAsync(context.Background(), g)), g)
	wantReadOnly(t, "WaitDurable of the late deallocate", mustReturn(t, "WaitDurable", r.waitDurableAsync(hloc)))

	if r.d.Durable(locG) {
		t.Error("the late write reads durable")
	}
	if calls := undo.all(); len(calls) != 1 || calls[0].Loc != hloc {
		t.Errorf("Undo saw %+v, want the late deallocate", calls)
	}
	if !slices.ContainsFunc(r.events(), func(ev core.LossEvent) bool { return ev.File == g && ev.Version == vG }) {
		t.Errorf("no event names the late write: %+v", r.events())
	}
	if got := r.d.Generation(); got != gen+1 {
		t.Errorf("Generation = %d, want %d: one raise for the whole entry", got, gen+1)
	}
}

// TestASyncThatSucceedsAfterEntryMovesNothing covers RFC 1 §5.4's sync in
// flight at the transition: it succeeds, yet satisfies no failed record's
// Sync and leaves the record failed.
func TestASyncThatSucceedsAfterEntryMovesNothing(t *testing.T) {
	prof.Watch(t, prof.Options{})
	r, _ := newRORig(t, 1, "")
	f := file(1)
	loc, _ := r.publish(t, f, 0, "aaaa")
	hold := r.seam.HoldFirst(t, journaltest.OpSync, format.SegmentName(loc.Segment))
	ch := r.syncAsync(context.Background(), f)
	select {
	case <-hold.Reached():
	case <-time.After(5 * time.Second):
		t.Fatal("the worker's sync never reached storage")
	}

	r.d.Enter(syscall.EROFS)
	mustBlock(t, "Sync while its sync is held", ch)
	hold.Release()
	wantReadOnlySyncError(t, "Sync over the late sync", mustReturn(t, "Sync", ch), f)
	if r.d.Durable(loc) {
		t.Error("the record the late sync covered reads durable")
	}
	if p, ok := r.ix.Lookup(f, 0); ok {
		t.Errorf("the index still serves the failed write: %+v", p)
	}
}

// TestFailedSeqsAreSeenByALaterDurabilityOnTheDirectory covers the
// per-directory failed list: every
// failed record's sequence number is listed for the directory, a second
// Durability on it in the process sees them, one on another directory does
// not.
func TestFailedSeqsAreSeenByALaterDurabilityOnTheDirectory(t *testing.T) {
	prof.Watch(t, prof.Options{})
	dir := t.TempDir()
	r, _ := newRORig(t, 1, dir)
	f := file(1)
	r.publish(t, f, 0, "aaaa")
	seqW := core.Seq(r.lastSeq.Load())
	_, seqH := r.appendHeader(t, f, format.KindTruncate, format.RecordBody{})

	r.d.Enter(syscall.EROFS)
	_ = mustReturn(t, "Sync", r.syncAsync(context.Background(), f))
	for _, seq := range []core.Seq{seqW, seqH} {
		if !slices.Contains(r.d.Failed(), seq) {
			t.Errorf("Failed() = %v, missing seq %d", r.d.Failed(), seq)
		}
	}

	later := func(dir string) *Durability {
		c := testConfig(t, journaltest.NewStopwatchClock(), &testDomains{})
		c.Dir = dir
		d, err := New(c)
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		return d
	}
	got := later(dir).Failed()
	for _, seq := range []core.Seq{seqW, seqH} {
		if !slices.Contains(got, seq) {
			t.Errorf("a later Durability's Failed() = %v, missing seq %d", got, seq)
		}
	}
	if other := later(t.TempDir()).Failed(); len(other) != 0 {
		t.Errorf("a Durability on another directory lists %v", other)
	}
}

// TestARemovalsWaitDurableReturnsErrJournalReadOnly covers a removal's
// durability wait: a removal
// waiting for its record's sync is woken by the entry with
// ErrJournalReadOnly and never a *core.SyncError, whether its record was
// unsynced or kept by a failed window, and a Stop before entry still gives
// ErrClosed.
func TestARemovalsWaitDurableReturnsErrJournalReadOnly(t *testing.T) {
	notSyncError := func(t *testing.T, err error) {
		t.Helper()
		var se *core.SyncError
		if errors.As(err, &se) {
			t.Errorf("WaitDurable = %v, a *core.SyncError", err)
		}
	}

	t.Run("unsynced", func(t *testing.T) {
		prof.Watch(t, prof.Options{})
		r, _ := newRORig(t, 1, "")
		f := file(1)
		loc, _ := r.appendHeader(t, f, format.KindDelete, format.RecordBody{})
		hold := r.seam.HoldFirst(t, journaltest.OpSync, format.SegmentName(loc.Segment))
		ch := r.waitDurableAsync(loc)
		select {
		case <-hold.Reached():
		case <-time.After(5 * time.Second):
			t.Fatal("the removal's sync never reached storage")
		}
		r.d.Enter(syscall.EROFS)
		hold.Release()
		err := mustReturn(t, "WaitDurable", ch)
		wantReadOnly(t, "WaitDurable", err)
		notSyncError(t, err)
	})

	t.Run("keptByAFailedWindow", func(t *testing.T) {
		prof.Watch(t, prof.Options{})
		r, _ := newRORig(t, 1, "")
		f := file(1)
		loc, _ := r.appendHeader(t, f, format.KindDelete, format.RecordBody{})
		r.failSyncOf(t, loc.Segment, errors.New("sync boom"))
		wantSyncError(t, mustReturn(t, "Sync", r.syncAsync(context.Background(), f)), loc.Segment, f)
		ch := r.waitDurableAsync(loc)
		mustBlock(t, "WaitDurable on a record a failed window keeps", ch)
		r.d.Enter(syscall.EROFS)
		err := mustReturn(t, "WaitDurable", ch)
		wantReadOnly(t, "WaitDurable", err)
		notSyncError(t, err)
	})

	t.Run("lostToAFailedWindow", func(t *testing.T) {
		// The failed window dropped the record and was resolved before the
		// entry, so the entry has nothing left to fail on the stream, and
		// the waiter must still be woken.
		prof.Watch(t, prof.Options{})
		r, _ := newRORig(t, 1, "")
		f := file(1)
		loc, _ := r.publish(t, f, 0, "aaaa")
		r.failSyncOf(t, loc.Segment, errors.New("sync boom"))
		wantSyncError(t, mustReturn(t, "Sync", r.syncAsync(context.Background(), f)), loc.Segment, f)
		eventually(t, "the window to be resolved", func() bool { return len(r.windowsOf(0)) == 0 })
		ch := r.waitDurableAsync(loc)
		mustBlock(t, "WaitDurable on a record a failed window dropped", ch)
		r.d.Enter(syscall.EROFS)
		err := mustReturn(t, "WaitDurable", ch)
		wantReadOnly(t, "WaitDurable", err)
		notSyncError(t, err)
	})

	t.Run("stopFirst", func(t *testing.T) {
		prof.Watch(t, prof.Options{})
		r, _ := newRORig(t, 1, "")
		f := file(1)
		loc, _ := r.appendHeader(t, f, format.KindDelete, format.RecordBody{})
		r.failSyncOf(t, loc.Segment, errors.New("sync boom"))
		wantSyncError(t, mustReturn(t, "Sync", r.syncAsync(context.Background(), f)), loc.Segment, f)
		ch := r.waitDurableAsync(loc)
		mustBlock(t, "WaitDurable on a record a failed window keeps", ch)
		r.d.Stop()
		r.d.Enter(syscall.EROFS)
		if err := mustReturn(t, "WaitDurable", ch); !errors.Is(err, core.ErrClosed) {
			t.Errorf("WaitDurable = %v, want core.ErrClosed", err)
		}
	})
}

// TestConcurrentEntriesRaiseTheGenerationOnce enters from Observe's EROFS and
// from a stopped stream's notification at once, over two streams with
// unsynced content: the generation rises by exactly one.
func TestConcurrentEntriesRaiseTheGenerationOnce(t *testing.T) {
	prof.Watch(t, prof.Options{})
	r, _ := newRORig(t, 2, "")
	f0, f1 := r.fileOn(t, 0), r.fileOn(t, 1)
	r.publish(t, f0, 0, "aaaa")
	r.publish(t, f1, 0, "bbbb")
	gen := r.d.Generation()

	var wg sync.WaitGroup
	for i := range 16 {
		wg.Add(2)
		go func() { defer wg.Done(); _ = r.d.observe(fmt.Errorf("write %d: %w", i, syscall.EROFS)) }()
		go func() { defer wg.Done(); r.d.readOnly(fmt.Errorf("stream stopped %d: %w", i, syscall.EROFS)) }()
	}
	wg.Wait()

	wantReadOnlySyncError(t, "Sync of stream 0's file", mustReturn(t, "Sync", r.syncAsync(context.Background(), f0)), f0)
	wantReadOnlySyncError(t, "Sync of stream 1's file", mustReturn(t, "Sync", r.syncAsync(context.Background(), f1)), f1)
	if got := r.d.Generation(); got != gen+1 {
		t.Errorf("Generation = %d, want %d", got, gen+1)
	}
}

// TestAnEarlierRemovalIsUndoneAfterALaterWriteInAnotherSegment pins the order
// across segments: a truncate still in the ledger, its segment's seal not
// reported, and a later write of the same file in another segment's failed
// window are failed newest first, so the write is dropped before the truncate
// is undone, though the window was taken before the ledger's records were.
// The write's segment is numbered above the truncate's in one case and below
// it in the other, since a segment's number says when its spare was made, not
// when the stream took it.
func TestAnEarlierRemovalIsUndoneAfterALaterWriteInAnotherSegment(t *testing.T) {
	for _, tc := range []struct {
		name               string
		truncSeg, writeSeg core.SegmentID
	}{
		{"writeInAHigherSegment", synthSeg + 1, synthSeg + 2},
		{"writeInALowerSegment", synthSeg + 2, synthSeg + 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			prof.Watch(t, prof.Options{})
			var mu sync.Mutex
			var order []string
			note := func(s string) {
				mu.Lock()
				defer mu.Unlock()
				order = append(order, s)
			}
			r := newRigConfigured(t, 1, func(c *Config) {
				c.Account = func(hd index.HeldDelta) { note(fmt.Sprintf("drop %d", hd.HeldBytes)) }
				c.Undo = func(segment.Written) index.HeldDelta { note("undo"); return index.HeldDelta{} }
			})
			f := file(1)

			// The truncate, then the write over it, published into the index
			// as a writer does.
			r.sinks[0].Track(segment.Written{File: f, Kind: format.KindTruncate, Seq: 1, Version: core.Version{Counter: 1},
				Loc: segment.Location{Segment: tc.truncSeg, Off: 64, Len: 64}})
			write := segment.Written{File: f, Kind: format.KindWrite, Seq: 2, Version: core.Version{Counter: 2}, Tag: 1,
				Extent: core.Extent{Off: 0, Len: 4}, Loc: segment.Location{Segment: tc.writeSeg, Off: 64, Len: 64}}
			unlock := r.domains.Lock(f)
			if _, err := r.ix.WriteRetain(1, f, index.Placed{Extent: write.Extent, Record: recordAt(write), Version: write.Version}, write.Seq); err != nil {
				t.Fatalf("WriteRetain: %v", err)
			}
			unlock()
			r.sinks[0].Track(write)

			// The write's segment fails and the journal turns read-only before
			// the worker runs, so its first pass takes the write's window and
			// then moves the truncate out of the ledger.
			r.d.onEnd(segment.EndEvent{Stream: 0, Segment: tc.writeSeg, Cause: segment.EndSeal, Err: errors.New("seal boom")})
			r.d.Enter(syscall.EROFS)
			r.d.Start(r.streams)

			eventually(t, "both records to be failed", func() bool {
				mu.Lock()
				defer mu.Unlock()
				return len(order) == 2
			})
			mu.Lock()
			defer mu.Unlock()
			if order[0] != "drop -4" || order[1] != "undo" {
				t.Fatalf("the layer did %v, want the later write dropped before the earlier truncate is undone", order)
			}
		})
	}
}
