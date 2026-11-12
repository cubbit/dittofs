package datapath

import (
	"context"
	"errors"
	"runtime"
	"testing"
	"time"

	"github.com/cubbit/dittofs/internal/prof"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/format"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/index"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/reclaim"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/segment"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/unread"
	"github.com/cubbit/dittofs/pkg/block/journal/journaltest/kit"
)

// These rows drive the retirement wiring over the assembled core on the real
// seam. Nothing below reaches a repack pass — the core's surface offers none
// — so every retirement in this file happens through the liveness wiring
// alone: the record registration, the emptied offer, the worker's gates and
// the retirer's unlink or recycle.

// inSet reports whether the set's snapshot still names seg.
func inSet(c *Core, seg core.SegmentID) bool {
	for _, r := range c.set.Snapshot() {
		if r.ID == seg {
			return true
		}
	}
	return false
}

// setSize returns the accounted size the set holds seg at, failing the test
// when the segment is not in the set.
func setSize(t *testing.T, c *Core, seg core.SegmentID) int64 {
	t.Helper()
	for _, r := range c.set.Snapshot() {
		if r.ID == seg {
			return r.Size
		}
	}
	t.Fatalf("segment %v is not in the set", seg)
	return 0
}

// takeInEnded takes a content spare from the pool into the set, the way a
// stream's roll does, and ends it: a real segment of the core's own ledger,
// held by nothing, whose window never opened. The rows that plant holds on an
// ended segment need one, and no data-path call ends a segment on demand.
func takeInEnded(t *testing.T, c *Core) core.SegmentID {
	t.Helper()
	seg, err := c.pool.Take(segment.Content)
	if err != nil {
		t.Fatalf("the pool refused a content spare: %v", err)
	}
	if err := c.set.End(seg); err != nil {
		t.Fatalf("End(%d): %v", seg, err)
	}
	return seg
}

// gone waits until seg leaves the set, so a row can assert a retirement's
// effect beside it.
func gone(t *testing.T, c *Core, what string, seg core.SegmentID) {
	t.Helper()
	waitIn(t, 8*time.Second, what, func() bool { return !inSet(c, seg) })
}

// notWithin is the other side of waitIn: it polls cond for the whole of a
// bounded window and fails the test the moment cond holds. Where no clock can
// be injected, the bounded wait is itself the assertion, so these windows
// hold several of the worker's ticks and only guard what must never happen
// inside them.
func notWithin(t *testing.T, d time.Duration, what string, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(d); ; time.Sleep(time.Millisecond) {
		if cond() {
			t.Fatalf("%s happened inside the bounded window, want none", what)
		}
		if time.Now().After(deadline) {
			return
		}
	}
}

// TestOverwriteRetiresItsEmptiedSegmentOnlyAfterTheSync pins the causality
// order a retirement owes an overwrite: a segment a superseding record
// emptied leaves the journal only once the record that emptied it is
// durable. The seam holds the sync that would make the overwrite durable —
// no retirement may reach the executor while it stands, and exactly one
// follows the release. The overwrite goes through the lift path: the
// superseding record's publish retains what it displaced, and the lift that
// reports the move runs only when the record's sync has covered it.
func TestOverwriteRetiresItsEmptiedSegmentOnlyAfterTheSync(t *testing.T) {
	ctx := context.Background()
	c, seam := newTestCore(t)
	s := mustShare(t, c, 1)
	f := testFile(1)

	// A small write into the stream's active segment, made durable, then an
	// early seal: the segment is closed to appends, and every retirement of
	// it is an unlink — an early seal is never a recycle candidate, whatever
	// the pool holds.
	const payload = 4 << 10
	mustWrite(t, c, s, f, 0, make([]byte, payload))
	syncFiles(t, c, f)
	if _, err := c.Streams().For(f).Seal(true); err != nil {
		t.Fatalf("the early seal: %v", err)
	}
	seg := recordOf(t, c, f, 0).Record.Segment
	size := setSize(t, c, seg)

	// The overwrite of the same extent supersedes the sealed segment's one
	// record. The sync that would make it durable is held at the seam — the
	// writer's Sync is what draws it and the caller waits for it — so the
	// lift that drops the segment's retained bytes, the move the retirement
	// waits on, cannot have happened while the hold stands.
	hold := seam.Hold(t, kit.OpSync, "*.seg")
	mustWrite(t, c, s, f, 0, make([]byte, payload))
	syncDone := make(chan error, 1)
	go func() { syncDone <- c.Durability().Sync(ctx, f) }()
	select {
	case <-hold.Reached():
	case <-time.After(5 * time.Second):
		t.Fatal("the held sync was never issued: the overwrite's record drew no sync")
	}

	notWithin(t, 2*time.Second, "a retirement while the overwrite's sync was held", func() bool {
		return !inSet(c, seg) || c.Retirer().Counters().FreedBytes != 0
	})

	// The release lets the sync cover the overwrite's record, the lift
	// report the emptied segment and the worker hand it over: one
	// retirement, sized by the accounted size the early seal shrank to.
	// The offer the lift hands the worker carries the causality record, so
	// the hand-over asks no tick's wait — a candidate the worker only found
	// on its own tick waits a full tick first, so the bound below sits under
	// that floor and pins the lift's own offer as what carried the segment
	// here.
	hold.Release()
	select {
	case err := <-syncDone:
		if err != nil {
			t.Fatalf("Sync after its parked call was released: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Sync never returned after its parked call was released")
	}
	waitIn(t, 600*time.Millisecond, "the overwritten segment to leave the set through the lift's own offer", func() bool {
		return !inSet(c, seg)
	})
	if got, want := c.Retirer().Counters().FreedBytes, uint64(size); got != want {
		t.Fatalf("FreedBytes after the overwrite's retirement = %d, want %d (the segment's accounted size %d)", got, want, size)
	}
	if got := c.set.Counters().DoubleRetires; got != 0 {
		t.Fatalf("DoubleRetires = %d, want 0: one offer, one retirement", got)
	}
}

// TestARecycledRetirementKeepsTheOldID: a recycle answers every hook with
// the retired segment's old id — the fresh spare's id is the pool's own word,
// and a durable-retirement signal that named it would leave the registry
// holding a segment the journal no longer has. The row drives a full seal's
// retirement with the pool below its target, so the retirer recycles rather
// than unlinks, and plants a forget record whose snapshot names the retired
// segment: the registry entry is released exactly when the tracker hears of
// the retirement, so its release proves which id reached it.
//
// The pool is held below its target by parking the spare maker's creates at
// the seam, the shape a full device would give: every roll below the target
// takes from the pool and nothing refills it while the hold stands.
func TestARecycledRetirementKeepsTheOldID(t *testing.T) {
	ctx := context.Background()
	c, seam := newTestCore(t)
	hold := seam.Hold(t, kit.OpCreate, "*.spare.tmp")
	s := mustShare(t, c, 1)
	f := testFile(1)

	// A write past the segment cap rolls and seals its first segments full;
	// the pool, parked at the seam, cannot refill, so it sits below its
	// target from here on.
	mustWrite(t, c, s, f, 0, make([]byte, 2*testSegmentCap))
	syncFiles(t, c, f)
	var sealed []core.SegmentID
	for _, r := range c.set.Snapshot() {
		if r.State == segment.Sealed {
			sealed = append(sealed, r.ID)
		}
	}
	if len(sealed) == 0 {
		t.Fatalf("a write of %d bytes sealed no segment", 2*testSegmentCap)
	}
	sparesBefore := c.pool.Counters().Spares
	if sparesBefore >= int64(c.contentTarget) {
		t.Fatalf("the pool holds %d spares, want it below its target %d", sparesBefore, c.contentTarget)
	}

	// The witness: a forget record whose snapshot names the sealed segments
	// answers undroppable while the snapshot names a segment of the set, and
	// the retirement hearing their old ids prunes it.
	rec := reclaim.Rec{
		Kind: format.KindForget,
		File: testFile(9),
		Seg:  recordOf(t, c, f, 2*testSegmentCap-1).Record.Segment, // the active segment carrying the write's tail
		Pair: index.Numbers{Version: core.Version{Counter: 7}, Seq: 1},
		Seq:  1,
	}
	c.liveness.RegisterForget(rec, sealed)
	if c.liveness.Droppable(rec) {
		t.Fatal("the forget's snapshot names segments of the set, want the record undroppable before the retirement")
	}

	// The truncation empties the sealed segments and offers them, gated on
	// the removal record, and the retirement below the target recycles: the
	// footprint stands — the cap moves to the spare pool — and the old ids
	// leave the set.
	used := c.set.Counters().UsedBytes
	if _, err := c.Truncate(ctx, s, f, 0); err != nil {
		t.Fatalf("Truncate(%d, 0): %v", f, err)
	}
	for _, seg := range sealed {
		gone(t, c, "the recycled segment to leave the set", seg)
	}

	// A recycle's hooks: the footprint does not fall (the cap moved into the
	// spare pool, it did not return to the filesystem), the pool is one
	// spare richer per recycle, the retired ids are gone from the set, and
	// the witness released — the tracker heard the retirement under the old
	// ids.
	if got, want := c.set.Counters().UsedBytes, used; got != want {
		t.Fatalf("UsedBytes across the recycle = %d, want %d: a recycle moves the cap into the spare pool, it frees no byte", got, want)
	}
	if got, want := c.pool.Counters().Spares, sparesBefore+int64(len(sealed)); got != want {
		t.Fatalf("the pool holds %d spares after the recycles, want %d (before %d, recycled %d)", got, want, sparesBefore, len(sealed))
	}
	if got, want := c.Retirer().Counters().FreedBytes, uint64(len(sealed))*uint64(testSegmentCap); got != want {
		t.Fatalf("FreedBytes after %d recycles = %d, want %d (each at the cap %d)", len(sealed), got, want, testSegmentCap)
	}
	if !c.liveness.Droppable(rec) {
		t.Fatal("the forget's snapshot survived the retirement: the registry was not pruned of the old ids, want the record released when the tracker heard the retirement")
	}
	if got := c.set.Counters().DoubleRetires; got != 0 {
		t.Fatalf("DoubleRetires = %d, want 0", got)
	}
	hold.Release()
}

// TestTruncateRetiresItsEmptiedSegmentOnlyAfterTheRecordSynces pins the same
// causal order for the removal path: the truncation's record is offered to
// the retirement worker at its publish, before it is durable, and the gate
// the worker asks — not the caller's own wait — is what holds the retirement
// back: the candidate stays queued and re-asked while the record cannot
// sync, and retires once it can.
func TestTruncateRetiresItsEmptiedSegmentOnlyAfterTheRecordSynces(t *testing.T) {
	ctx := context.Background()
	c, seam := newTestCore(t)
	s := mustShare(t, c, 1)
	f := testFile(1)

	// A write of one and a half segments: the first seals full, the tail
	// stays in the stream's active segment, which the truncation's own
	// record lands in — and holds.
	mustWrite(t, c, s, f, 0, make([]byte, 3*testSegmentCap/2))
	syncFiles(t, c, f)
	waitIn(t, 5*time.Second, "the pool to refill to its target", func() bool {
		return c.pool.Counters().Spares >= int64(c.contentTarget)
	})
	seg := recordOf(t, c, f, 0).Record.Segment
	active := recordOf(t, c, f, 3*testSegmentCap/2-1).Record.Segment
	if seg == active {
		t.Fatal("the write kept every record in one segment, want a sealed segment beside the active one")
	}

	// Every sync of the active segment parks at the seam, so the
	// truncation's record cannot become durable: the call blocks waiting for
	// that, after its removal has been applied and its emptied segment
	// offered.
	hold := seam.Hold(t, kit.OpSync, format.SegmentName(active))
	done := make(chan error, 1)
	go func() {
		_, err := c.Truncate(ctx, s, f, 0)
		done <- err
	}()
	select {
	case <-hold.Reached():
	case <-time.After(5 * time.Second):
		t.Fatal("the truncation's record drew no sync: the held call was never issued")
	}

	// Bounded window, not a truth: while the removal record is not durable
	// no hand-over may reach the executor, and the worker's ticks keep
	// re-asking the gate inside the window.
	select {
	case err := <-done:
		t.Fatalf("Truncate returned (%v) while its record could not sync, want it to wait for durability", err)
	case <-time.After(2 * time.Second):
	}
	if !inSet(c, seg) {
		t.Fatal("the emptied segment was retired while its removal record was not durable, want it held back")
	}
	if got, want := c.Retirer().Counters().FreedBytes, uint64(0); got != want {
		t.Fatalf("FreedBytes while the removal record was not durable = %d, want %d", got, want)
	}

	// The release lets the sync cover the record, the wait return and the
	// gate open: the queued candidate retires, and no second one follows.
	hold.Release()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Truncate after the release: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Truncate never returned after its record's sync was released")
	}
	gone(t, c, "the emptied segment to leave the set", seg)
	if got, want := c.Retirer().Counters().FreedBytes, uint64(testSegmentCap); got != want {
		t.Fatalf("FreedBytes after the truncation's retirement = %d, want %d", got, want)
	}
	if !inSet(c, active) {
		t.Fatal("the active segment left the set, want only the sealed one retired")
	}
	if got := c.set.Counters().DoubleRetires; got != 0 {
		t.Fatalf("DoubleRetires = %d, want 0", got)
	}
}

// TestAnEndedSegmentRetiresOnlyAfterItsFailedWindowResolves: a failed sync
// at the seam ends its segment with an open window, and the journal never
// hands the segment over while that window stands — the retirement reaches
// the executor only after the window's record has re-appended elsewhere and
// the window closed. The re-append is parked at the seam, the shape a device
// that keeps failing the segment's writes would give. The failed sync is a
// seal's first step, which makes the segment's records durable: the fault on
// it ends the segment from the caller's own goroutine, and the one record
// the failed seal left uncovered is the window, whose re-appended copy is
// what leaves the ended segment holding nothing.
func TestAnEndedSegmentRetiresOnlyAfterItsFailedWindowResolves(t *testing.T) {
	c, seam := newTestCore(t)
	s := mustShare(t, c, 1)
	f := testFile(1)
	boom := errors.New("the injected sync failure")

	const payload = 4 << 10
	mustWrite(t, c, s, f, 0, make([]byte, payload))
	seg := recordOf(t, c, f, 0).Record.Segment

	// The fault on the segment's own sync, and the hold on the first write
	// to a segment file below: the failed seal ends the segment, and the
	// window's re-append parks at the seam, so the end leaves the window
	// open for as long as the hold stands.
	if err := seam.Schedule(kit.Failing(kit.OpSync, format.SegmentName(seg), 1, boom)); err != nil {
		t.Fatalf("the seam refused the sync fault: %v", err)
	}
	writeHold := seam.HoldFirst(t, kit.OpWrite, "*.seg")
	if _, err := c.Streams().For(f).Seal(true); err == nil {
		t.Fatal("the seal succeeded over a segment whose sync the seam fails, want the failure to end it")
	}
	st, err := c.set.State(seg)
	if err != nil || st != segment.Ended {
		t.Fatalf("the state of the segment whose seal failed = %v, %v, want Ended", st, err)
	}
	waitIn(t, 5*time.Second, "the failed segment's window to be taken", func() bool {
		return c.Durability().WindowOpen(seg)
	})

	// Bounded window, not a truth: the worker's ticks pass inside it, and
	// the ended segment with an open window must stay.
	notWithin(t, 2*time.Second, "a retirement of the ended segment while its window stood open", func() bool {
		return !inSet(c, seg) || c.Retirer().Counters().FreedBytes != 0
	})

	// The release lets the window's re-append run, the index leave the
	// failed segment's places and the window close: the retirement follows.
	writeHold.Release()
	gone(t, c, "the ended segment to leave the set once its window closed", seg)
	if c.Durability().WindowOpen(seg) {
		t.Fatal("the failed segment's window still answers open after its retirement, want it closed")
	}
	if got, want := c.Retirer().Counters().FreedBytes, uint64(testSegmentCap); got != want {
		t.Fatalf("FreedBytes after the ended segment's retirement = %d, want %d", got, want)
	}
	if got := c.set.Counters().DoubleRetires; got != 0 {
		t.Fatalf("DoubleRetires = %d, want 0", got)
	}
}

// TestAClaimedSegmentStaysUntilTheClaimReleases: a copy in use takes the
// claim of the segment it sits in before a read serves from it, and a
// retirement of that segment must not slip past the claim — the worker's own
// claim is refused, and the segment is offered again and retired only once
// the hold gives the claim back. The read path that runs holds of this kind
// through real copies does not exist yet; the row takes the claim the way
// that path does, which is the same serialized window the worker meets.
func TestAClaimedSegmentStaysUntilTheClaimReleases(t *testing.T) {
	c, _ := newTestCore(t)
	seg := takeInEnded(t, c)

	// The claim a held copy would carry, taken before the segment is ever a
	// candidate.
	if !c.set.Claim(seg) {
		t.Fatalf("Claim(%d) was refused on an unheld segment, want it to win", seg)
	}

	// Bounded window, not a truth: several of the worker's ticks pass inside
	// it, each finding the segment offered by the scan and its claim
	// refused.
	notWithin(t, 2*time.Second, "a retirement of the claimed segment", func() bool {
		return !inSet(c, seg) || c.Retirer().Counters().FreedBytes != 0
	})

	// The claim back, and the scan's next offer retires the segment: the
	// refusal dropped the candidate, and the worker re-offers what the scan
	// finds rather than wedging on the segment it lost.
	c.set.Release(seg)
	gone(t, c, "the segment to retire once the claim was released", seg)
	if got, want := c.Retirer().Counters().FreedBytes, uint64(testSegmentCap); got != want {
		t.Fatalf("FreedBytes after the claim's release = %d, want %d (the segment's accounted cap %d)", got, want, testSegmentCap)
	}
	if got := c.set.Counters().DoubleRetires; got != 0 {
		t.Fatalf("DoubleRetires = %d, want 0", got)
	}
}

// TestARacingReclaimPassCannotRetireTwice: a reclamation pass selecting over
// the same set and index as the worker, claiming the very segment the
// worker retires, cannot make the journal retire it twice — the pass's claim
// and the worker's serialize, the retirer unlinks the segment once, and the
// footprint falls by the segment once. The pass empties no segment of its
// own: it selects, and gives every claim back.
func TestARacingReclaimPassCannotRetireTwice(t *testing.T) {
	ctx := context.Background()
	c, _ := newTestCore(t)
	s := mustShare(t, c, 1)
	f := testFile(1)

	mustWrite(t, c, s, f, 0, make([]byte, 3*testSegmentCap/2))
	syncFiles(t, c, f)
	waitIn(t, 5*time.Second, "the pool to refill to its target", func() bool {
		return c.pool.Counters().Spares >= int64(c.contentTarget)
	})
	seg := recordOf(t, c, f, 0).Record.Segment
	used := c.set.Counters().UsedBytes

	// The racing pass: a real selection of the core's own set and index,
	// claiming and giving back the very segment the truncation below
	// empties.
	sel := reclaim.NewSelector(c.set, c.index, unread.New(c.clock), c.dur, nil)
	stop, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			default:
			}
			claimed := sel.Select(reclaim.Scope{Segments: []core.SegmentID{seg}}, 1)
			for _, cand := range claimed {
				sel.Done(cand.ID)
			}
			runtime.Gosched()
		}
	}()

	// The truncation offers the emptied segment while the pass races it, and
	// the worker retires it somewhere between the pass's claims.
	if _, err := c.Truncate(ctx, s, f, 0); err != nil {
		t.Fatalf("Truncate(%d, 0): %v", f, err)
	}
	gone(t, c, "the racing pass and the worker to settle on one retirement", seg)
	close(stop)
	<-done

	if got, want := c.Retirer().Counters().FreedBytes, uint64(testSegmentCap); got != want {
		t.Fatalf("FreedBytes after the race = %d, want %d: the segment is freed once", got, want)
	}
	if got, want := c.set.Counters().UsedBytes, used-testSegmentCap; got != want {
		t.Fatalf("UsedBytes after the race = %d, want %d (before the truncation %d, the freed cap %d)", got, want, used, testSegmentCap)
	}
	if got := c.set.Counters().DoubleRetires; got != 0 {
		t.Fatalf("DoubleRetires after the race = %d, want 0", got)
	}
}

// TestRetirementDecidesWithoutReadingTheSegment: the decision that a segment
// holds nothing answers from the tracker's and the index's counters alone,
// never from the segment's own bytes — a retirement whose path had to read
// the segment to decide would cost the reclaim an I/O the counters owe it.
func TestRetirementDecidesWithoutReadingTheSegment(t *testing.T) {
	ctx := context.Background()
	c, seam := newTestCore(t)
	s := mustShare(t, c, 1)
	f := testFile(1)

	mustWrite(t, c, s, f, 0, make([]byte, 3*testSegmentCap/2))
	syncFiles(t, c, f)
	waitIn(t, 5*time.Second, "the pool to refill to its target", func() bool {
		return c.pool.Counters().Spares >= int64(c.contentTarget)
	})
	seg := recordOf(t, c, f, 0).Record.Segment

	reads := seam.Calls(kit.OpRead) + seam.Calls(kit.OpReadDirect)
	if _, err := c.Truncate(ctx, s, f, 0); err != nil {
		t.Fatalf("Truncate(%d, 0): %v", f, err)
	}
	syncFiles(t, c, f)
	gone(t, c, "the emptied segment to leave the set", seg)

	if got, want := seam.Calls(kit.OpRead)+seam.Calls(kit.OpReadDirect)-reads, 0; got != want {
		t.Fatalf("the retirement path read the segment %d times, want %d: liveness answers from counters, never from storage", got, want)
	}
	if got, want := c.Retirer().Counters().FreedBytes, uint64(testSegmentCap); got != want {
		t.Fatalf("FreedBytes after the retirement = %d, want %d", got, want)
	}
}

// TestCloseDropsAQueuedCandidateAndJoinsTheWorker: a candidate that sits in
// the retirement worker's queue when the journal closes retires nothing —
// the close joins the worker, the queue dies with it, and no goroutine of
// the journal outlives the call. The watch fails the test on any goroutine
// or descriptor left behind.
func TestCloseDropsAQueuedCandidateAndJoinsTheWorker(t *testing.T) {
	prof.Watch(t, prof.Options{})

	c, _ := newTestCore(t)
	seg := takeInEnded(t, c)

	// A candidate in the worker's queue, the way the tracker's crossing or
	// the tick's scan would put one there: a prompt with no location, whose
	// hand-over waits out a full tick — a wait the close below runs past.
	c.retiring.Prompt(seg)
	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if !inSet(c, seg) {
		t.Fatal("the queued candidate retired across the journal's close, want the queue dropped with the worker")
	}
	if got, want := c.Retirer().Counters().FreedBytes, uint64(0); got != want {
		t.Fatalf("FreedBytes after Close = %d, want %d", got, want)
	}
	if got := c.set.Counters().DoubleRetires; got != 0 {
		t.Fatalf("DoubleRetires after Close = %d, want 0", got)
	}
}

// TestAnEndedSegmentOutlivesItsCopiesProof: records re-appended into a copy
// segment keep their failed segment held until something proves the copies
// are recorded — a recorded mark past the last copy, or the copies' segment
// sealing, which the wiring hears through the set's seal observer. While the
// hold stands the segment does not retire; the proof that ends it releases
// the hold, and the retirement follows. The re-append path that would
// register the hold does not exist yet, so the rows plant the hold the way
// that path will: the same tracker call, on the core's own ended segment.
func TestAnEndedSegmentOutlivesItsCopiesProof(t *testing.T) {
	c, _ := newTestCore(t)

	// Round one: the proof is a recorded mark past the last copy.
	seg := takeInEnded(t, c)
	copies, err := c.pool.Take(segment.Content)
	if err != nil {
		t.Fatalf("the pool refused a content spare for the copies' segment: %v", err)
	}
	const last = int64(10)
	c.liveness.HoldEnded(seg, copies, last)
	if !c.liveness.Held(seg) {
		t.Fatal("the ended segment with a registered hold answers not held, want the hold to keep it")
	}

	// Bounded window, not a truth: the worker's ticks pass inside it, and
	// the held segment is not even offered.
	notWithin(t, 1500*time.Millisecond, "a retirement of the ended segment while its copies' proof was still owed", func() bool {
		return !inSet(c, seg) || c.Retirer().Counters().FreedBytes != 0
	})

	// A mark short of the last copy proves nothing — the segment stays.
	c.liveness.MarksRecorded(copies, last)
	if !inSet(c, seg) {
		t.Fatal("the ended segment retired on a mark that did not pass its last copy, want the hold to stand")
	}

	// A mark past the last copy proves the copies: the hold releases, the
	// crossing offers the segment and the retirement follows.
	c.liveness.MarksRecorded(copies, last+1)
	waitIn(t, 5*time.Second, "the held-ended segment's crossing to fire", func() bool { return !c.liveness.Held(seg) })
	gone(t, c, "the ended segment to leave the set once its copies were proven", seg)
	if got, want := c.Retirer().Counters().FreedBytes, uint64(testSegmentCap); got != want {
		t.Fatalf("FreedBytes after the ended hold's release = %d, want %d", got, want)
	}
	if got := c.set.Counters().DoubleRetires; got != 0 {
		t.Fatalf("DoubleRetires after round one = %d, want 0", got)
	}

	// Round two: the proof is the copies' segment sealing, which the wiring
	// hears through the seal observer the core registered. The copies'
	// segment holds the copies themselves — live records — so no
	// retirement of it can close the hold either: the seal is the only
	// proof this round's hold can end by.
	seg2 := takeInEnded(t, c)
	copies2, err := c.pool.Take(segment.Content)
	if err != nil {
		t.Fatalf("the pool refused a content spare for the second copies' segment: %v", err)
	}
	c.liveness.Register(reclaim.Rec{
		Kind: format.KindWrite, File: testFile(8), Seg: copies2,
		Ext:  core.Extent{Off: 0, Len: 4096},
		Pair: index.Numbers{Version: core.Version{Counter: 3}, Seq: 2}, Seq: 2,
	})
	c.liveness.HoldEnded(seg2, copies2, 100)
	if !c.liveness.Held(seg2) {
		t.Fatal("the second ended segment with a registered hold answers not held, want the hold to keep it")
	}

	notWithin(t, 1500*time.Millisecond, "a retirement of the second ended segment while its copies' segment was unsealed", func() bool {
		return !inSet(c, seg2) || c.Retirer().Counters().FreedBytes != uint64(testSegmentCap)
	})

	if err := c.set.Seal(copies2, true); err != nil {
		t.Fatalf("Seal(%d, full) of the copies' segment: %v", copies2, err)
	}
	waitIn(t, 5*time.Second, "the hold to hear the copies' seal", func() bool { return !c.liveness.Held(seg2) })
	gone(t, c, "the second ended segment to leave the set once its copies' segment sealed", seg2)
	if got := c.set.Counters().DoubleRetires; got != 0 {
		t.Fatalf("DoubleRetires after round two = %d, want 0", got)
	}
}

// TestAnUndurableCandidateDoesNotWedgeTheWorker: a candidate whose causality
// record cannot sync is sent back — the gate refuses it, the worker's queue
// takes it again on the next tick — and a second segment emptied meanwhile
// must still retire, on the same worker, behind the first one's back. The
// row parks one file's sync at the seam while another file's truncation
// flows.
func TestAnUndurableCandidateDoesNotWedgeTheWorker(t *testing.T) {
	ctx := context.Background()
	c, seam := newTestCore(t)
	s := mustShare(t, c, 1)
	f := testFile(1)

	mustWrite(t, c, s, f, 0, make([]byte, 3*testSegmentCap/2))
	syncFiles(t, c, f)
	waitIn(t, 5*time.Second, "the pool to refill to its target", func() bool {
		return c.pool.Counters().Spares >= int64(c.contentTarget)
	})
	seg := recordOf(t, c, f, 0).Record.Segment // the candidate that cannot retire yet
	active := recordOf(t, c, f, 3*testSegmentCap/2-1).Record.Segment

	// Every sync of f's active segment parks, so f's truncation offers a
	// candidate the gate must keep refusing.
	hold := seam.Hold(t, kit.OpSync, format.SegmentName(active))
	done := make(chan error, 1)
	go func() {
		_, err := c.Truncate(ctx, s, f, 0)
		done <- err
	}()
	select {
	case <-hold.Reached():
	case <-time.After(5 * time.Second):
		t.Fatal("f's truncation record drew no sync: the held call was never issued")
	}

	// An unrelated file on another stream empties its own segment, whose
	// causality record syncs fine: its retirement must go through the same
	// worker that keeps refusing the held-back candidate.
	g := otherStreamFile(c, f)
	mustWrite(t, c, s, g, 0, make([]byte, 3*testSegmentCap/2))
	syncFiles(t, c, g)
	waitIn(t, 5*time.Second, "the pool to refill to its target again", func() bool {
		return c.pool.Counters().Spares >= int64(c.contentTarget)
	})
	other := recordOf(t, c, g, 0).Record.Segment
	if _, err := c.Truncate(ctx, s, g, 0); err != nil {
		t.Fatalf("Truncate(%d, 0) of the unrelated file: %v", g, err)
	}
	gone(t, c, "the unrelated segment to retire behind the undurable candidate", other)

	// The held-back candidate is in the worker's queue, not in its way: the
	// unrelated segment retired, the refused one stayed.
	if !inSet(c, seg) {
		t.Fatalf("segment %d was retired while its causality record's sync was held, want no retirement", seg)
	}
	if got, want := c.Retirer().Counters().FreedBytes, uint64(testSegmentCap); got != want {
		t.Fatalf("FreedBytes while the candidate was held back = %d, want %d (only the unrelated segment freed)", got, want)
	}

	// The release opens the gate and the refused candidate retires on its
	// queued offer.
	hold.Release()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("the held file's Truncate after the release: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the held file's Truncate never returned after its record's sync was released")
	}
	gone(t, c, "the refused candidate to retire once its record went durable", seg)
	if got, want := c.Retirer().Counters().FreedBytes, uint64(2*testSegmentCap); got != want {
		t.Fatalf("FreedBytes after both retirements = %d, want %d", got, want)
	}
	if got := c.set.Counters().DoubleRetires; got != 0 {
		t.Fatalf("DoubleRetires after both retirements = %d, want 0", got)
	}
}
