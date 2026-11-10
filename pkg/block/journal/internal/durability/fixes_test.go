package durability

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/format"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/segment"
	"github.com/cubbit/dittofs/pkg/block/journal/journaltest/kit"
	"github.com/cubbit/dittofs/pkg/block/journal/journaltest/prof"
)

// strand appends a record of f to a segment, fails that segment's seal so it
// ends, and appends a second record of g, which rolls to a new segment. It
// returns the two locations.
func (r *rig) strand(t *testing.T, f, g core.FileID) (older, newer segment.Location) {
	t.Helper()
	a := r.appendWrite(t, f, 0, "a")
	if err := r.seam.Schedule(kit.Failing(kit.OpSync, format.SegmentName(a.Segment), 1, errors.New("seal boom"))); err != nil {
		t.Fatal(err)
	}
	if _, err := r.streams.For(f).Seal(false); err == nil {
		t.Fatal("the seal was expected to fail")
	}
	b := r.appendWrite(t, g, 0, "b")
	if b.Segment == a.Segment {
		t.Fatal("the append did not roll to a new segment")
	}
	return a, b
}

// TestEndedOlderSegmentDoesNotSpinTheTimer pins that a record of an ended
// older segment, which no sync of the active segment can cover, does not make
// the timer fire back to back: with the clock frozen, the active segment's
// sync count must stay flat.
func TestEndedOlderSegmentDoesNotSpinTheTimer(t *testing.T) {
	prof.Watch(t, prof.Options{})
	r := newRig(t, 1)
	_, b := r.strand(t, file(1), file(2))
	r.clock.Advance(core.SyncBound)
	time.Sleep(50 * time.Millisecond)
	c1 := r.syncsOf(b.Segment)
	time.Sleep(50 * time.Millisecond)
	if c2 := r.syncsOf(b.Segment); c2 != c1 || c2 > 3 {
		t.Fatalf("syncs of the active segment went %d then %d with the clock frozen", c1, c2)
	}
}

// TestSyncOverARecordStrandedInAnOlderSegmentIsASyncError pins that Sync names
// every segment a file has an uncovered record in: the later record's sync
// does not make the earlier, stranded one durable, and a waiter on it fails
// with the segment's SyncError instead of leading a sync of the active one.
func TestSyncOverARecordStrandedInAnOlderSegmentIsASyncError(t *testing.T) {
	prof.Watch(t, prof.Options{})
	r := newRig(t, 1)
	f := file(1)
	a, b := r.strand(t, f, f)
	err := mustReturn(t, "Sync", r.syncAsync(context.Background(), f))
	wantSyncError(t, err, a.Segment, f)
	if r.d.Durable(a) {
		t.Fatal("the stranded record reads durable")
	}
	if got := r.syncsOf(b.Segment); got > 2 {
		t.Errorf("a waiter on the older segment issued %d syncs of the active one", got)
	}
}

// TestFailureIsRecordedAgainstTheSegmentSynced pins that a failed sync lands
// on the segment it was a sync of even when a seal and a roll run before the
// worker takes its lock: the waiter on the failed segment gets the SyncError
// and the new segment is not poisoned.
func TestFailureIsRecordedAgainstTheSegmentSynced(t *testing.T) {
	prof.Watch(t, prof.Options{})
	r := newRigUnstarted(t, 1)
	f, g := file(1), file(2)
	var once sync.Once
	r.d.afterSync = func() {
		once.Do(func() {
			_, _ = r.streams.For(f).Seal(false)
			r.appendWrite(t, g, 0, "b")
		})
	}
	// Started before the first append: the end events are registered there.
	r.d.Start(r.streams)
	a := r.appendWrite(t, f, 0, "a")
	boom := errors.New("sync boom")
	if err := r.seam.Schedule(kit.Failing(kit.OpSync, format.SegmentName(a.Segment), 1, boom)); err != nil {
		t.Fatal(err)
	}

	err := mustReturn(t, "Sync of the failed segment", r.syncAsync(context.Background(), f))
	var se *core.SyncError
	if !errors.As(err, &se) || se.Segment != a.Segment {
		t.Fatalf("Sync = %v, want a SyncError of segment %d", err, a.Segment)
	}
	if err := mustReturn(t, "Sync of the new segment", r.syncAsync(context.Background(), g)); err != nil {
		t.Fatalf("Sync of a record in a segment that never failed = %v", err)
	}
}

// TestClosedStreamIsNotAFailedSync pins that a Sync over a closed stream
// answers core.ErrClosed alone, with no failure counted or recorded.
func TestClosedStreamIsNotAFailedSync(t *testing.T) {
	prof.Watch(t, prof.Options{})
	r := newRig(t, 1)
	f := file(1)
	r.appendWrite(t, f, 0, "a")
	if err := r.streams.Close(); err != nil {
		t.Logf("Close: %v", err)
	}
	err := mustReturn(t, "Sync", r.syncAsync(context.Background(), f))
	if !errors.Is(err, core.ErrClosed) || errors.Is(err, core.ErrSyncFailed) {
		t.Fatalf("Sync over a closed stream = %v, want ErrClosed and not ErrSyncFailed", err)
	}
	if got := r.d.Stats().SyncFailures; got != 0 {
		t.Errorf("SyncFailures = %d, want 0", got)
	}
}

// TestRollToAnUntrackedSegmentDoesNotSpin pins that a stream which rolled to a
// segment holding no tracked record does not make the timer or a Sync waiter
// sync that segment over and over: a record of the older segment whose seal
// failed is not covered by any sync of the new one, so what the worker counts
// as the active segment must not move back to it. The sync calls made on the
// stream, counted where the worker makes them, stay bounded while the clock
// runs past the sync bound: a refused call moves neither Stats counter, so the
// counters alone cannot show the spin.
func TestRollToAnUntrackedSegmentDoesNotSpin(t *testing.T) {
	prof.Watch(t, prof.Options{})
	r := newRigUnstarted(t, 1)
	var calls atomic.Int64
	r.d.afterSync = func() { calls.Add(1) }
	// The layer registers its end consumer on Start, which must precede the
	// stream's first append.
	r.d.Start(r.streams)
	f := file(1)
	// r1 is a header-only record whose kept bytes are damaged, so no pass can
	// re-append it: it stays pending in its window, and nothing is appended
	// into the new segment ahead of the test's own append.
	a := r.appendKind(t, f, format.KindDelete, format.RecordBody{})
	r.d.made[0].mu.Lock()
	r.d.made[0].ledger[0].w.Bytes = make([]byte, format.RecordHeaderSize)
	r.d.made[0].mu.Unlock()
	if err := r.seam.Schedule(kit.Failing(kit.OpSync, format.SegmentName(a.Segment), 1, errors.New("seal boom"))); err != nil {
		t.Fatal(err)
	}
	if _, err := r.streams.For(f).Seal(false); err == nil {
		t.Fatal("the seal was expected to fail")
	}
	r.settlePool(t)
	// The roll's header write is the first write after this point and passes;
	// the record's is the second and fails: the roll lands and nothing is
	// tracked in the new segment.
	if err := r.seam.Schedule(kit.FailingFrom(kit.OpWrite, "*", 2, 1, errors.New("record boom"))); err != nil {
		t.Fatal(err)
	}
	h := format.RecordHeader{Kind: format.KindWrite, File: f, Extent: core.Extent{Off: 8, Len: 2}, Version: core.Version{Counter: 9}, Tag: 1}
	if _, err := r.streams.For(f).Append(h, format.RecordBody{Payload: []byte("r2"), MTime: 1}); err == nil {
		t.Fatal("the append into the new segment was expected to fail")
	}
	// The failed seal ended the first segment, so r1 leaves the ledger for
	// that segment's window, which the worker takes after the seal returns,
	// and the new segment tracks nothing.
	eventually(t, "the ledger to hold no entry", func() bool { return r.ledgerLen(0) == 0 })

	r.clock.Advance(core.SyncBound)
	// A Sync waiter on r1 leads the same way the timer does; it ends at the
	// deadline, r1 never being durable.
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	_ = r.d.Sync(ctx, f)
	r.clock.Advance(core.SyncBound)
	time.Sleep(50 * time.Millisecond)
	c1 := calls.Load()
	time.Sleep(100 * time.Millisecond)
	if c2 := calls.Load(); c2 != c1 || c2 > 5 {
		t.Fatalf("the stream was asked to sync %d then %d times with the clock frozen", c1, c2)
	}
}

// errorCounter is a slog handler that counts the records logged at Error.
type errorCounter struct{ n atomic.Int64 }

func (c *errorCounter) Enabled(context.Context, slog.Level) bool { return true }
func (c *errorCounter) Handle(_ context.Context, r slog.Record) error {
	if r.Level >= slog.LevelError {
		c.n.Add(1)
	}
	return nil
}
func (c *errorCounter) WithAttrs([]slog.Attr) slog.Handler { return c }
func (c *errorCounter) WithGroup(string) slog.Handler      { return c }

// TestSyncOnAnEndedSegmentIssuesNoFsync pins that a sync the worker issues for
// a segment that has ended, before it has taken the segment's end event, is
// refused without an fsync: no sync is counted as failed and nothing is logged
// at Error for it. The waiter then returns nil once the record, re-appended
// when the end event is taken, is synced in the next segment.
func TestSyncOnAnEndedSegmentIssuesNoFsync(t *testing.T) {
	prof.Watch(t, prof.Options{})
	logs := &errorCounter{}
	r := newRigLogging(t, 1, nil, slog.New(logs))
	// The worker is stopped after its window step, once armed, so the seal
	// below ends the segment with its event still untaken when the sync runs.
	var armed atomic.Bool
	reached := make(chan struct{}, 16)
	gate, open := releaser(t)
	t.Cleanup(open)
	r.d.afterHolds = func() {
		if armed.Load() {
			reached <- struct{}{}
			<-gate
		}
	}
	r.d.Start(r.streams)
	f := file(1)
	a := r.appendKind(t, f, format.KindDelete, format.RecordBody{})
	armed.Store(true)
	r.d.made[0].wake()
	select {
	case <-reached:
	case <-time.After(5 * time.Second):
		t.Fatal("the worker never reached its hold step")
	}
	if err := r.seam.Schedule(kit.Failing(kit.OpSync, format.SegmentName(a.Segment), 1, errors.New("seal boom"))); err != nil {
		t.Fatal(err)
	}
	if _, err := r.streams.For(f).Seal(false); err == nil {
		t.Fatal("the seal was expected to fail")
	}
	before := r.syncsOf(a.Segment)
	done := r.syncAsync(context.Background(), f)
	mustBlock(t, "Sync", done)
	armed.Store(false)
	open()

	if err := mustReturn(t, "Sync on the ended segment", done); err != nil {
		t.Fatalf("Sync = %v, want nil: the record is re-appended", err)
	}
	// The failed seal is itself reported once, as a failure and an Error log,
	// when its end event is taken; the refused sync adds nothing to either.
	// The worker counts the failure under the stream's lock and logs it after
	// releasing it, so both are waited for before they are read as the seal's.
	eventually(t, "the seal's end event to be counted and logged", func() bool {
		return r.d.Stats().SyncFailures == 1 && logs.n.Load() == 1
	})
	if got := r.syncsOf(a.Segment); got != before {
		t.Errorf("the ended segment saw %d fsyncs after the Sync, want %d", got, before)
	}
	// The one fsync issued is the new segment's, for the re-appended record.
	if st := r.d.Stats(); st.Syncs != 1 || st.SyncFailures != 1 {
		t.Errorf("Stats = %d syncs, %d failures, want 1 and the seal's 1", st.Syncs, st.SyncFailures)
	}
}
