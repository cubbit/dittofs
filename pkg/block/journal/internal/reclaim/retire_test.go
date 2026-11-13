package reclaim

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/durability"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/format"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/index"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/segment"
	"github.com/cubbit/dittofs/pkg/block/journal/journaltest/kit"
)

// retireFixture is one journal's retirement periphery over a recorded storage
// seam: a real ledger, a real maker, a real retirer started on its own
// worker, an index and a tracker of it, and the channel the retirer's
// retired-segment hook reports each completed retirement on. The worker under
// test is built per test, over these facts.
type retireFixture struct {
	ix      *index.Index
	set     *segment.Set
	seam    *kit.Seam
	tr      *Tracker
	retirer *segment.Retirer
	retired chan core.SegmentID
	// last is the largest segment id a Maker drew over the seam, so a later
	// spare draws above it instead of over its name.
	last core.SegmentID

	// dur, streams and counters hold the real gate the rows that need a
	// refusing gate build once through gate; nil until then.
	dur      *durability.Durability
	streams  *segment.Streams
	counters *index.Counters
}

// newRetireFixture builds the periphery and starts the retirer's worker. The
// retirer stops with the test.
func newRetireFixture(t *testing.T) *retireFixture {
	t.Helper()
	seam, err := kit.NewSeam(t.TempDir())
	if err != nil {
		t.Fatalf("the seam could not be made: %v", err)
	}
	set := segment.NewSet(segCap, nil)
	maker := segment.NewMaker(seam.FS(), set, 1<<62, 0, nil)
	cache := segment.NewDescriptors(seam.FS(), format.SegmentName, core.NewDescriptorBudget(64))
	retired := make(chan core.SegmentID, 256)
	r := segment.NewRetirer(segment.RetirerConfig{
		Set:     set,
		Cache:   cache,
		Maker:   maker,
		FS:      seam.FS(),
		Retired: func(id core.SegmentID) { retired <- id },
	})
	r.Start()
	t.Cleanup(r.Stop)
	ix := index.New(1 << 20)
	return &retireFixture{
		ix:      ix,
		set:     set,
		seam:    seam,
		tr:      New(ix, nil),
		retirer: r,
		retired: retired,
	}
}

// takeIn makes one more spare over the fixture's seam and takes it into the
// fixture's set: a real further segment, Active and holding nothing.
func (f *retireFixture) takeIn(t *testing.T) core.SegmentID {
	t.Helper()
	maker := segment.NewMaker(f.seam.FS(), f.set, 1<<62, f.last, nil)
	seg, err := maker.Make()
	if err != nil {
		t.Fatalf("the maker could not make a spare: %v", err)
	}
	if err := f.set.Take(seg); err != nil {
		t.Fatalf("Take: %v", err)
	}
	f.last = seg
	return seg
}

// sealed takes one fresh segment in and seals it early: Sealed, at its cap,
// holding nothing.
func (f *retireFixture) sealed(t *testing.T) core.SegmentID {
	t.Helper()
	seg := f.takeIn(t)
	if err := f.set.Seal(seg, false); err != nil {
		t.Fatalf("Seal: %v", err)
	}
	return seg
}

// worker builds the worker under test over the fixture with a fast tick
// (the retirer's own rhythm is one second; a test shortens it, as the
// executor's comment allows) and starts it. gate may be nil, which the
// worker's contract keeps for mechanics tests.
func (f *retireFixture) worker(t *testing.T, gate Gate, exec Executor) *Worker {
	t.Helper()
	w := NewWorker(f.set, f.tr, gate, exec)
	w.tickEvery = 10 * time.Millisecond
	t.Cleanup(w.Stop)
	w.Start()
	return w
}

// at64 is one causality record's location inside a segment: offset 64, the
// first record slot, some width. The values are the gate's to read, nothing
// more.
func at64(seg core.SegmentID) segment.Location {
	return segment.Location{Segment: seg, Off: 64, Len: 16}
}

// rigDomains is a file append domain table that never blocks: the gate rig's
// rows take no domain, nothing contending.
type rigDomains struct{}

func (rigDomains) Lock(core.FileID) func() { return func() {} }

// gate builds the real retirement gate over the fixture: a durability layer
// with one append stream of the fixture's own set, whose ledger answers the
// worker's asks — a record not yet covered by a sync answers not durable,
// and the record the row appends is made durable or not by the stream's real
// sync, parked at the seam's hold like any call. Take every fabricated
// segment the row needs before calling this: the rig's pool maker draws its
// ids above the fixture's last only, so a segment taken later could share a
// name with the spares the pool made.
func (f *retireFixture) gate(t *testing.T) *durability.Durability {
	t.Helper()
	if f.dur != nil {
		return f.dur
	}
	counters := &index.Counters{}
	d, err := durability.New(durability.Config{
		Clock:    kit.NewStopwatchClock(),
		Domains:  rigDomains{},
		Index:    f.ix,
		Dir:      t.TempDir(),
		Counters: counters,
		MarkLoss: f.ix.Lose,
	})
	if err != nil {
		t.Fatalf("durability.New: %v", err)
	}
	maker := segment.NewMaker(f.seam.FS(), f.set, 1<<62, f.last, nil)
	pool := segment.NewPool(maker, segment.Targets{Content: 1}, d.Admit, nil)
	cache := segment.NewDescriptors(f.seam.FS(), format.SegmentName, core.NewDescriptorBudget(64))
	// The cleanups run last in, first out: the worker under test stops
	// before them, then the layer joins, the streams close, the pool stops
	// and the descriptors shut down — the order the durability harness
	// itself closes in.
	t.Cleanup(func() {
		if err := cache.Shutdown(); err != nil {
			t.Errorf("the rig's cache Shutdown: %v", err)
		}
	})
	t.Cleanup(pool.Stop)
	streams := segment.NewStreams(1, core.JournalID{1}, f.set, pool, cache, d.Sink)
	t.Cleanup(func() { _ = streams.Close() })
	t.Cleanup(d.Stop)
	pool.Start()
	for deadline := time.Now().Add(10 * time.Second); pool.Counters().Spares < 1; time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("the rig's pool never readied a spare: %+v", pool.Counters())
		}
	}
	f.dur, f.streams, f.counters = d, streams, counters
	d.Start(streams)
	return d
}

// appendRecord appends one content record of id through the rig's real
// stream and returns where it landed: a true ledger entry whose durability
// the gate answers by — not durable until a sync has covered it, which the
// row leads and the seam's hold may park.
func (f *retireFixture) appendRecord(t *testing.T, id core.FileID, v core.Version) segment.Location {
	t.Helper()
	seq, err := f.counters.NextSeq()
	if err != nil {
		t.Fatalf("NextSeq: %v", err)
	}
	pl, err := f.streams.Stream(0).Append(format.RecordHeader{
		Kind: format.KindWrite, File: id, Extent: core.Extent{Off: 0, Len: 16},
		Seq: seq, Version: v, Tag: 1,
	}, format.RecordBody{Payload: make([]byte, 16), MTime: 1})
	if err != nil {
		t.Fatalf("the rig's append: %v", err)
	}
	return pl.Loc
}

// queuedEntry reports whether the worker's queue still holds seg's entry,
// and whether that entry still carries recorded locations. The rows assert
// requeue and drop with it: a requeue keeps the entry and its locations, a
// drop empties the queue, and a tick scan's prompt-only re-offer fills it
// again with none.
func queuedEntry(w *Worker, seg core.SegmentID) (queued, located bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	e, ok := w.entries[seg]
	return ok, ok && len(e.locs) != 0
}

// waitCond polls cond, with a short pause between polls, until it holds
// inside the bounded window, failing the test when it never does.
func waitCond(t *testing.T, d time.Duration, what string, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(d); ; time.Sleep(time.Millisecond) {
		if cond() {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s never happened inside %v", what, d)
			return
		}
	}
}

// recordedExec hands each call to the executor it wraps and counts them: the
// requirement these tests assert on is which hand-overs reach the executor,
// and a wrapper that records the call and passes it through, never answering
// in its place, keeps that record.
type recordedExec struct {
	next Executor
	mu   sync.Mutex
	n    int
}

func (e *recordedExec) Enqueue(id core.SegmentID, reason segment.Reason) {
	e.mu.Lock()
	e.n++
	e.mu.Unlock()
	e.next.Enqueue(id, reason)
}

func (e *recordedExec) count() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.n
}

// oneRec is one header-only write record registered live of seg, so the
// tracker holds the segment: the registration the tests place between an
// offer and the worker's pass.
func oneRec(seg core.SegmentID) Rec {
	return Rec{
		Kind: format.KindWrite, File: fid(3), Seg: seg,
		Ext:  core.Extent{Off: 0, Len: 4096},
		Pair: index.Numbers{Version: ver(2), Seq: 1}, Seq: 1,
	}
}

// TestEmptiedRetiresANotHeldSealedSegment: the mover's offer of a fresh,
// not-held, sealed segment is claimed, re-checked, gated and handed over as
// a NotHeld retirement: the retirer reports one retirement, the freed
// counter rises by the segment's accounted size, the ledger's snapshot drops
// the segment, and nothing counts a double retire.
func TestEmptiedRetiresANotHeldSealedSegment(t *testing.T) {
	f := newRetireFixture(t)
	w := f.worker(t, nil, f.retirer)
	seg := f.sealed(t)

	w.Emptied(seg, at64(seg))
	select {
	case id := <-f.retired:
		if id != seg {
			t.Fatalf("Emptied retired segment %v, want %v", id, seg)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("Emptied of a not-held sealed segment: no retirement arrived, want segment %v", seg)
	}

	for _, row := range f.set.Snapshot() {
		if row.ID == seg {
			t.Fatalf("the retired segment %v is still in the ledger's snapshot, want it gone", seg)
		}
	}
	if got, want := f.retirer.Counters().FreedBytes, uint64(segCap); got != want {
		t.Fatalf("FreedBytes after one retirement = %d, want %d", got, want)
	}
	if got := f.set.Counters().DoubleRetires; got != 0 {
		t.Fatalf("DoubleRetires = %d, want 0: one offer, one retirement", got)
	}
}

// TestANotYetDurableRecordRequeuesUntilItsSyncCoversIt: the mover's offer
// names a record whose sync cannot complete — a real record of the rig's
// real stream, its fsync parked at the seam — and the real ledger answers it
// not durable, so the entry keeps requeueing and no hand-over reaches the
// executor while the fsync stands. The requeue itself is read in process:
// two passes ran over the candidate and the queue entry kept its recorded
// location, which only a requeue does — a drop empties the entry, and the
// tick scan's prompt-only re-offer fills it with none. The released fsync
// covers the record, and the requeued offer retires the segment.
func TestANotYetDurableRecordRequeuesUntilItsSyncCoversIt(t *testing.T) {
	f := newRetireFixture(t)
	seg := f.sealed(t)
	d := f.gate(t)
	exec := &recordedExec{next: f.retirer}
	w := NewWorker(f.set, f.tr, d, exec)
	w.tickEvery = 10 * time.Millisecond
	t.Cleanup(w.Stop)

	var passes atomic.Int32
	w.afterClaim = func(core.SegmentID) { passes.Add(1) }

	loc := f.appendRecord(t, fid(3), ver(1))
	hold := f.seam.Hold(t, kit.OpSync, format.SegmentName(loc.Segment))
	syncDone := make(chan error, 1)
	go func() { _, err := d.Sync(context.Background(), fid(3)); syncDone <- err }()
	select {
	case <-hold.Reached():
	case <-time.After(5 * time.Second):
		t.Fatal("the recorded record's sync was never issued: no fsync reached the seam's hold")
	}
	if d.Durable(loc) {
		t.Fatal("the ledger answered the record durable while its fsync stood parked at the seam, want not durable")
	}

	// The worker starts only now: the segment is sealed and empty from the
	// start, so a tick scan running earlier would offer it prompt-only,
	// freeze a pending tail without the parked record and retire it.
	w.Start()
	w.Emptied(seg, loc)
	waitCond(t, 5*time.Second, "two passes over the refused candidate", func() bool {
		return passes.Load() >= 2
	})
	if queued, located := queuedEntry(w, seg); !queued || !located {
		t.Fatalf("the refused candidate's queue entry is queued=%v located=%v, want the requeue to keep it with its recorded location", queued, located)
	}
	if got := exec.count(); got != 0 {
		t.Fatalf("Enqueue called %d times while the offer's record could not sync, want 0", got)
	}
	// Bounding window, not a truth: the fast ticks ran further passes inside
	// it, and each must have been refused by the still-parked record.
	select {
	case id := <-f.retired:
		t.Fatalf("retired segment %v while its offer's record could not sync, want none", id)
	case <-time.After(300 * time.Millisecond):
	}

	hold.Release()
	select {
	case err := <-syncDone:
		if err != nil {
			t.Fatalf("Sync after its parked fsync was released: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Sync never returned once its parked fsync was released")
	}
	select {
	case id := <-f.retired:
		if id != seg {
			t.Fatalf("after the record went durable, retired segment %v, want %v", id, seg)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("after the record went durable, no retirement arrived, want segment %v", seg)
	}
	if got, want := f.retirer.Counters().FreedBytes, uint64(segCap); got != want {
		t.Fatalf("FreedBytes after the requeued offer's retirement = %d, want %d", got, want)
	}
	if got := f.set.Counters().DoubleRetires; got != 0 {
		t.Fatalf("DoubleRetires = %d, want 0", got)
	}
}

// TestTheHandOverWaitsOnARecordAnotherMoverLeftUnsynced: an offer may name a
// record that is already durable — one mover's crossing — while another
// record of the same stream, appended after that sync was issued, still
// cannot sync: the shape of a truncation whose cut emptied a segment behind
// a record the offer names nowhere. The offer's own gate answers durable, so
// only the hand-over's pending-record barrier stands between the segment and
// an unlink that would lose the unsynced record; the release covers it and
// exactly one retirement follows.
func TestTheHandOverWaitsOnARecordAnotherMoverLeftUnsynced(t *testing.T) {
	f := newRetireFixture(t)
	seg := f.sealed(t)
	d := f.gate(t)
	exec := &recordedExec{next: f.retirer}
	w := NewWorker(f.set, f.tr, d, exec)
	w.tickEvery = 10 * time.Millisecond
	t.Cleanup(w.Stop)

	// The first mover's record, appended and made durable by its own sync:
	// the record its offer is gated on.
	first := f.appendRecord(t, fid(3), ver(1))
	if _, err := d.Sync(context.Background(), fid(3)); err != nil {
		t.Fatalf("the first record's sync: %v", err)
	}
	if !d.Durable(first) {
		t.Fatal("the first record answered not durable after its own sync completed, want durable")
	}

	// The second mover's record, appended after that sync returned — beyond
	// everything it captured — and parked at the seam: not durable.
	second := f.appendRecord(t, fid(4), ver(1))
	if second.Segment != first.Segment {
		t.Fatalf("the two records landed in segments %v and %v, want the stream's one shared active segment", first.Segment, second.Segment)
	}
	hold := f.seam.Hold(t, kit.OpSync, format.SegmentName(second.Segment))
	syncDone := make(chan error, 1)
	go func() { _, err := d.Sync(context.Background(), fid(4)); syncDone <- err }()
	select {
	case <-hold.Reached():
	case <-time.After(5 * time.Second):
		t.Fatal("the second record's sync was never issued: no fsync reached the seam's hold")
	}
	if d.Durable(second) {
		t.Fatal("the second record answered durable while its fsync stood parked at the seam, want not durable")
	}

	// The worker starts only now: the segment is sealed and empty from the
	// start, so a tick scan running earlier would offer it prompt-only,
	// freeze a pending tail without the parked record and retire it.
	w.Start()

	// The offer names the durable record alone. Bounding window, not a
	// truth: full scan passes run inside it, and each must be held back by
	// the pending record the gate adds.
	w.Emptied(seg, first)
	select {
	case id := <-f.retired:
		t.Fatalf("retired segment %v while the second record stood unsynced, want the hand-over held back", id)
	case <-time.After(300 * time.Millisecond):
	}
	if got := exec.count(); got != 0 {
		t.Fatalf("Enqueue called %d times while the second record stood unsynced, want 0", got)
	}

	// The release covers the pending record, the barrier lifts and the
	// handed-back candidate retires — once.
	hold.Release()
	select {
	case err := <-syncDone:
		if err != nil {
			t.Fatalf("the second record's Sync after its parked fsync was released: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the second record's Sync never returned once its parked fsync was released")
	}
	select {
	case id := <-f.retired:
		if id != seg {
			t.Fatalf("after the pending record went durable, retired segment %v, want %v", id, seg)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("after the pending record went durable, no retirement arrived, want segment %v", seg)
	}
	if got, want := f.retirer.Counters().FreedBytes, uint64(segCap); got != want {
		t.Fatalf("FreedBytes after the held-back candidate's retirement = %d, want %d", got, want)
	}
	if got := f.set.Counters().DoubleRetires; got != 0 {
		t.Fatalf("DoubleRetires = %d, want 0", got)
	}
}

// TestHeldRecheckUnderTheClaimDropsTheCandidate: a live-record registration
// landing between the offer and the worker's pass is caught by the Held
// re-check under the claim — the candidate is dropped, nothing reaches the
// executor — and the record's release is what makes the segment offerable
// again, by the tick scan's fresh prompt rather than the dropped entry.
func TestHeldRecheckUnderTheClaimDropsTheCandidate(t *testing.T) {
	f := newRetireFixture(t)
	exec := &recordedExec{next: f.retirer}
	// Collected before Start, so the record is registered between the offer
	// and the first pass, exactly as a registration that raced the offer.
	w := NewWorker(f.set, f.tr, nil, exec)
	w.tickEvery = 10 * time.Millisecond
	t.Cleanup(w.Stop)
	seg := f.sealed(t)

	rec := oneRec(seg)
	f.tr.Register(rec)
	w.Emptied(seg, at64(seg))
	w.Start()

	// Bounding window, not a truth: no hand-over may reach the executor
	// while the record is registered, and the worker ticks fast enough
	// that several passes ran inside it.
	select {
	case id := <-f.retired:
		t.Fatalf("retired segment %v while its record was registered, want no retirement", id)
	case <-time.After(300 * time.Millisecond):
	}
	if got := exec.count(); got != 0 {
		t.Fatalf("Enqueue called %d times for a segment held again, want 0", got)
	}
	if got, want := f.retirer.Counters().FreedBytes, uint64(0); got != want {
		t.Fatalf("FreedBytes while the segment was held = %d, want %d", got, want)
	}

	// The release leaves the segment not held; the scan offers it and the
	// retirement lands, proving the entry's drop left nothing stalled.
	f.tr.Unregister(rec)
	select {
	case id := <-f.retired:
		if id != seg {
			t.Fatalf("retired segment %v after the record's release, want %v", id, seg)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("no retirement after the record's release, want segment %v", seg)
	}
}

// TestAPromptOnlyCandidateWaitsOneTickALocatedOneDoesNot: a prompt-only
// candidate waits until a full tick has passed since it was first offered —
// with a tick far longer than any test wait, it never retires — while a
// candidate that gains a location meanwhile is handed over on the next pass,
// tick or not, because the location's gate answer replaces the wait.
func TestAPromptOnlyCandidateWaitsOneTickALocatedOneDoesNot(t *testing.T) {
	f := newRetireFixture(t)
	w := NewWorker(f.set, f.tr, nil, f.retirer)
	w.tickEvery = time.Hour
	t.Cleanup(w.Stop)
	w.Start()
	seg := f.sealed(t)

	w.Prompt(seg)
	// Bounding window, not a truth: no tick has passed, so no prompt-only
	// hand-over may happen.
	select {
	case id := <-f.retired:
		t.Fatalf("retired segment %v before one tick, want the backstop to hold the prompt-only candidate", id)
	case <-time.After(300 * time.Millisecond):
	}

	w.Emptied(seg, at64(seg))
	select {
	case id := <-f.retired:
		if id != seg {
			t.Fatalf("retired segment %v, want %v", id, seg)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("a candidate with a recorded location was not handed over without a tick, want segment %v retired", seg)
	}
}

// TestTheTickScanOffersWhatNothingHolds: the tick scan offers a sealed
// segment nothing offered by hand, and its retirement lands after the
// backstop's tick; a sealed segment whose record is registered is skipped by
// the same scan and never handed over.
func TestTheTickScanOffersWhatNothingHolds(t *testing.T) {
	f := newRetireFixture(t)
	f.worker(t, nil, f.retirer)
	seg := f.sealed(t)
	held := f.sealed(t)
	heldRec := oneRec(held)
	f.tr.Register(heldRec)

	select {
	case id := <-f.retired:
		if id != seg {
			t.Fatalf("the scan retired segment %v, want %v", id, seg)
		}
	case <-time.After(3 * time.Second):
		t.Fatalf("the tick scan never offered the untouched sealed segment %v, want its retirement", seg)
	}

	// Bounding window, not a truth: several scans ran inside it, and each
	// must skip the segment whose record is registered.
	select {
	case id := <-f.retired:
		t.Fatalf("retired segment %v whose record is registered, want no retirement", id)
	case <-time.After(300 * time.Millisecond):
	}
	if got, want := f.retirer.Counters().FreedBytes, uint64(segCap); got != want {
		t.Fatalf("FreedBytes with one segment freed = %d, want %d: the held segment must not be freed", got, want)
	}
}

// TestTwoWorkersRetireOneSegmentOnce: two workers over one set, tracker and
// executor, both offered the same segment, retire it exactly once: the
// claim a hand-over takes refuses the other's, the executor's queue keeps
// one entry per id, and the ledger counts no double retire.
func TestTwoWorkersRetireOneSegmentOnce(t *testing.T) {
	f := newRetireFixture(t)
	w1 := f.worker(t, nil, f.retirer)
	w2 := f.worker(t, nil, f.retirer)
	seg := f.sealed(t)

	w1.Emptied(seg, at64(seg))
	w2.Emptied(seg, at64(seg))
	select {
	case id := <-f.retired:
		if id != seg {
			t.Fatalf("retired segment %v, want %v", id, seg)
		}
	case <-time.After(3 * time.Second):
		t.Fatalf("two workers on one segment: no retirement arrived, want segment %v", seg)
	}
	w1.Stop()
	w2.Stop()

	// The join has happened, so any second retirement is already in the
	// channel; a full receive here would be a second handing-over.
	select {
	case id := <-f.retired:
		t.Fatalf("a second retirement of segment %v arrived, want exactly one", id)
	default:
	}
	if got := f.set.Counters().DoubleRetires; got != 0 {
		t.Fatalf("DoubleRetires = %d, want 0: the claim must keep one hand-over per segment", got)
	}
	if got, want := f.retirer.Counters().FreedBytes, uint64(segCap); got != want {
		t.Fatalf("FreedBytes after two offers = %d, want %d: the segment is freed once", got, want)
	}
	for _, row := range f.set.Snapshot() {
		if row.ID == seg {
			t.Fatalf("the segment %v is still in the ledger's snapshot, want it gone", seg)
		}
	}
}

// TestARefusedClaimDropsTheEntryALaterPromptReoffers: a candidate whose
// claim another holder refuses is dropped, its recorded location with it.
// The drop is read in process: a pass claimed, was refused, and the queue
// entry is gone — where a requeue would have kept it, location and all, and
// every later pass would have found the claim still taken and re-refused.
// A later Prompt offers the segment again fresh, with no location, and the
// retirement lands.
func TestARefusedClaimDropsTheEntryALaterPromptReoffers(t *testing.T) {
	f := newRetireFixture(t)
	w := NewWorker(f.set, f.tr, nil, f.retirer)
	w.tickEvery = 10 * time.Millisecond
	t.Cleanup(w.Stop)
	seg := f.sealed(t)

	if !f.set.Claim(seg) {
		t.Fatalf("Claim: the external claim could not take the segment")
	}
	claimed := make(chan struct{})
	var once sync.Once
	w.afterClaim = func(core.SegmentID) { once.Do(func() { close(claimed) }) }
	w.Start()

	w.Emptied(seg, at64(seg))
	select {
	case <-claimed:
	case <-time.After(2 * time.Second):
		t.Fatalf("the worker never ran its pass over the refused claim, want the pass to have claimed and been refused")
	}

	// The pass refused the claim and dropped the entry; the bounded wait
	// covers the drop landing on the worker's goroutine behind the claim
	// the hook reports. A requeue would have kept the entry here for the
	// external claim stands, so every later pass finds nothing new.
	waitCond(t, 2*time.Second, "the refused claim to drop the candidate's entry", func() bool {
		queued, _ := queuedEntry(w, seg)
		return !queued
	})

	// The external claim gone, the later prompt re-offers the segment fresh
	// and the retirement lands.
	f.set.Release(seg)
	w.Prompt(seg)
	select {
	case id := <-f.retired:
		if id != seg {
			t.Fatalf("retired segment %v, want %v", id, seg)
		}
	case <-time.After(3 * time.Second):
		t.Fatalf("the later Prompt did not re-offer the dropped candidate, want segment %v's retirement", seg)
	}
	if got := f.set.Counters().DoubleRetires; got != 0 {
		t.Fatalf("DoubleRetires = %d, want 0", got)
	}
	if got, want := f.retirer.Counters().FreedBytes, uint64(segCap); got != want {
		t.Fatalf("FreedBytes after the re-offer's retirement = %d, want %d", got, want)
	}
}

// TestStopJoinsAndLaterOffersDrop: Stop joins the worker's goroutine, offers
// after it drop, and the executor is never reached — the freed counter stays
// zero, the segment stays in the ledger, and no retirement is completed.
func TestStopJoinsAndLaterOffersDrop(t *testing.T) {
	f := newRetireFixture(t)
	exec := &recordedExec{next: f.retirer}
	w := NewWorker(f.set, f.tr, nil, exec)
	w.tickEvery = 10 * time.Millisecond
	w.Start()
	w.Stop() // returning at all is Stop joining, within the test's own timeout

	seg := f.sealed(t)
	w.Emptied(seg, at64(seg))
	w.Prompt(seg)
	// Bounding window, not a truth: the offers dropped synchronously inside
	// Stop, so this only guards against a pass that escaped the join.
	select {
	case id := <-f.retired:
		t.Fatalf("retired segment %v after Stop, want the offers dropped", id)
	case <-time.After(200 * time.Millisecond):
	}
	if got := exec.count(); got != 0 {
		t.Fatalf("Enqueue called %d times after Stop, want 0: the executor was reached past a joined worker", got)
	}
	if got, want := f.retirer.Counters().FreedBytes, uint64(0); got != want {
		t.Fatalf("FreedBytes after Stop = %d, want %d", got, want)
	}
	found := false
	for _, row := range f.set.Snapshot() {
		if row.ID == seg {
			found = true
		}
	}
	if !found {
		t.Fatalf("the segment %v left the ledger without a retirement, want it still in the set", seg)
	}
	if got := f.set.Counters().DoubleRetires; got != 0 {
		t.Fatalf("DoubleRetires = %d, want 0", got)
	}
}

// TestPromptFiredFromTheSealObserverWhileARetirementCompletes: the candidate
// fires from inside the set's seal observer — whose delivery the set holds a
// lock across the whole callback — while the worker completes another
// segment's retirement concurrently, whose unlink needs the very lock the
// delivery holds. The queue-only candidate and the claim's release order
// keep this a wait, never a deadlock: every round's two segments retire, and
// the ledger counts no double retire at the end.
func TestPromptFiredFromTheSealObserverWhileARetirementCompletes(t *testing.T) {
	const rounds = 15
	f := newRetireFixture(t)
	var w *Worker
	tr := New(f.ix, func(seg core.SegmentID) { w.Prompt(seg) })
	tr.SetMember(func(seg core.SegmentID) bool {
		_, err := f.set.State(seg)
		return err == nil
	})
	w = NewWorker(f.set, tr, nil, f.retirer)
	w.tickEvery = 10 * time.Millisecond
	t.Cleanup(w.Stop)
	f.set.OnSeal(tr.SealObserved)
	w.Start()

	for round := 0; round < rounds; round++ {
		ended := f.takeIn(t)
		if err := f.set.End(ended); err != nil {
			t.Fatalf("End: %v", err)
		}
		copies := f.takeIn(t)
		tr.HoldEnded(ended, copies, 10)

		// The mover's offer racing the seal: the worker retires this
		// segment while the observer below fires the candidate.
		other := f.sealed(t)
		w.Emptied(other, at64(other))

		if err := f.set.Seal(copies, true); err != nil {
			t.Fatalf("Seal of the copies' segment: %v", err)
		}

		// Collect until both of the round's segments retired. Segments of
		// earlier rounds (the copies, sealed holding nothing) are re-handed
		// by later ticks' scans and tolerated here.
		want := map[core.SegmentID]struct{}{ended: {}, other: {}}
		for len(want) > 0 {
			select {
			case id := <-f.retired:
				delete(want, id)
			case <-time.After(2 * time.Second):
				t.Fatalf("retirements stalled inside the seal observer's round; still waiting for %v", want)
			}
		}
	}
	if got := f.set.Counters().DoubleRetires; got != 0 {
		t.Fatalf("DoubleRetires after %d rounds = %d, want 0", rounds, got)
	}
}
