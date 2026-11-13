package datapath

import (
	"context"
	"errors"
	"io/fs"
	"runtime"
	"slices"
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

// retiredTo waits until every named segment has left the set and the retirer
// has counted at least want freed bytes: the retirer counts its freed bytes
// a few steps after the segment leaves the set, on its own goroutine, so a
// wait on the set alone can observe the half-finished retirement and read
// the counter before it moves.
func retiredTo(t *testing.T, c *Core, what string, want uint64, segs ...core.SegmentID) {
	t.Helper()
	waitIn(t, 8*time.Second, what, func() bool {
		for _, s := range segs {
			if inSet(c, s) {
				return false
			}
		}
		return c.Retirer().Counters().FreedBytes >= want
	})
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
	waitIn(t, 900*time.Millisecond, "the overwritten segment to leave the set through the lift's own offer", func() bool {
		return !inSet(c, seg)
	})
	retiredTo(t, c, "the overwrite's retirement to count its freed bytes", uint64(size), seg)
	if got, want := c.Retirer().Counters().FreedBytes, uint64(size); got != want {
		t.Fatalf("FreedBytes after the overwrite's retirement = %d, want %d (the segment's accounted size %d)", got, want, size)
	}
	if got := c.set.Counters().DoubleRetires; got != 0 {
		t.Fatalf("DoubleRetires = %d, want 0: one offer, one retirement", got)
	}
}

// TestALiftsOfferWaitsOnARemovalLeftUnsyncedByAnotherFile: two files share
// one sealed segment; one file's overwrite supersedes its content — the old
// content stays retained until the overwrite's sync covers it — and the
// other file's truncation cuts the segment's held bytes to zero while the
// retained bytes still answer held, so its own report names nothing. The
// overwrite's sync then completes, and the lift the completion runs is the
// offer that reaches the worker, gated on the overwrite's record alone —
// already durable. Without the barrier that asks for every stream's pending
// records, the hand-over would follow that offer now and unlink the segment
// while the truncation's record, appended after the overwrite's sync, was
// still unsynced; with it, exactly one retirement follows the release of
// the truncation's own sync.
func TestALiftsOfferWaitsOnARemovalLeftUnsyncedByAnotherFile(t *testing.T) {
	ctx := context.Background()
	c, seam := newTestCore(t)
	s := mustShare(t, c, 1)
	f := testFile(1)
	g := sameStreamFile(c, f)

	// Two files' content in one segment, made durable, then sealed early:
	// every later write of the row lands past it.
	const payload = 4 << 10
	mustWrite(t, c, s, f, 0, make([]byte, payload))
	mustWrite(t, c, s, g, 0, make([]byte, payload))
	syncFiles(t, c, f, g)
	if _, err := c.Streams().For(f).Seal(true); err != nil {
		t.Fatalf("the early seal: %v", err)
	}
	seg := recordOf(t, c, f, 0).Record.Segment
	if recordOf(t, c, g, 0).Record.Segment != seg {
		t.Fatal("the two files' content landed in different segments, want one shared sealed segment")
	}
	size := setSize(t, c, seg)

	// The overwrite of f: its publish retains the old content in the sealed
	// segment, and the writer's own sync — the one that would make the
	// overwrite durable — parks at the seam, captured ahead of everything the
	// truncation below appends.
	mustWrite(t, c, s, f, 0, make([]byte, payload))
	active := recordOf(t, c, f, 0).Record.Segment
	if active == seg {
		t.Fatal("the overwrite landed in the sealed segment, want the stream's active segment past it")
	}
	leadSync := seam.Hold(t, kit.OpSync, format.SegmentName(active))
	syncDone := make(chan error, 1)
	go func() { syncDone <- c.Durability().Sync(ctx, f) }()
	select {
	case <-leadSync.Reached():
	case <-time.After(5 * time.Second):
		t.Fatal("the overwrite's sync was never issued: no fsync reached the seam's hold")
	}

	// The truncation of g, on the same stream: it appends its removal record
	// beyond the parked sync's captured prefix and cuts the sealed segment's
	// held bytes to zero — while the retained bytes of f's overwritten
	// content keep the segment answering held, so the removal's own report
	// offers nothing. The call then blocks behind the held sync of its
	// record.
	truncDone := make(chan error, 1)
	go func() {
		_, err := c.Truncate(ctx, s, g, 0)
		truncDone <- err
	}()
	waitIn(t, 5*time.Second, "the truncation to cut g's content out of the index", func() bool {
		ext, err := c.Extents(s, g)
		return err == nil && len(ext) == 0
	})
	if !c.liveness.Held(seg) {
		t.Fatal("the sealed segment answered not held while f's overwritten content kept it retained, want held")
	}
	select {
	case err := <-truncDone:
		t.Fatalf("Truncate returned (%v) while its record could not sync, want it to wait for durability", err)
	default:
	}

	// The barrier the row plants: every sync of the active segment past the
	// parked one parks here, so the truncation's record stays unsynced once
	// the overwrite's own sync goes through.
	tailSync := seam.Hold(t, kit.OpSync, format.SegmentName(active))

	// The overwrite's sync completes: the lift empties the sealed segment and
	// offers it gated on the overwrite's record — durable now — and the
	// worker's Sync returns only once that lift has run, so the offer has
	// reached its queue when it does.
	leadSync.Release()
	select {
	case err := <-syncDone:
		if err != nil {
			t.Fatalf("the overwrite's Sync after its parked fsync was released: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the overwrite's Sync never returned once its parked fsync was released")
	}

	// Bounded window, not a truth: the offer stood — the sync's return says
	// the lift finished — and the tick scans keep re-asking the gate inside
	// the window, but no hand-over may reach the executor while the
	// truncation's record is unsynced, whatever the offer's own location
	// says.
	notWithin(t, 2*time.Second, "a retirement of the segment while the truncation's record was still unsynced", func() bool {
		return !inSet(c, seg) || c.Retirer().Counters().FreedBytes != 0
	})

	// The release: the truncation's sync covers its record, the call
	// returns, and the candidate the barrier held back retires — exactly
	// once, sized by the early seal's accounted size.
	tailSync.Release()
	select {
	case err := <-truncDone:
		if err != nil {
			t.Fatalf("the truncation after its record's sync was released: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the truncation never returned once its record's sync was released")
	}
	retiredTo(t, c, "the segment to retire once the truncation's record was durable", uint64(size), seg)
	if got, want := c.Retirer().Counters().FreedBytes, uint64(size); got != want {
		t.Fatalf("FreedBytes after the retirement = %d, want %d (the segment's accounted size %d)", got, want, size)
	}
	if got := c.set.Counters().DoubleRetires; got != 0 {
		t.Fatalf("DoubleRetires = %d, want 0", got)
	}
	if !inSet(c, active) {
		t.Fatal("the active segment left the set, want only the sealed one retired")
	}
}

// TestALiftsOfferWaitsOnARemovalItsSegmentFailedToSync: the same crossing
// as the held-sync row, with the removal's own sync FAILED rather than
// parked: the failed seal of the removal's segment pulls the unsynced
// removal out of the stream's ledger into the failed segment's pending
// window, where the ledger's own name for it ends — the window's re-append
// is the only road left. The lift's offer names the completed overwrite
// alone there, so only the barrier that walks the pending windows holds the
// segment back while the removal sits unsynced; the re-append's sync
// resolves the window and exactly one retirement follows.
func TestALiftsOfferWaitsOnARemovalItsSegmentFailedToSync(t *testing.T) {
	ctx := context.Background()
	c, seam := newTestCore(t)
	s := mustShare(t, c, 1)
	f := testFile(1)
	g := sameStreamFile(c, f)
	boom := errors.New("the failed sync of the removal's segment")

	// Two files' content in one segment, made durable, then sealed early —
	// the shared sealed segment the row's emptiness rides on.
	const payload = 4 << 10
	mustWrite(t, c, s, f, 0, make([]byte, payload))
	mustWrite(t, c, s, g, 0, make([]byte, payload))
	syncFiles(t, c, f, g)
	if _, err := c.Streams().For(f).Seal(true); err != nil {
		t.Fatalf("the early seal: %v", err)
	}
	seg := recordOf(t, c, f, 0).Record.Segment
	if recordOf(t, c, g, 0).Record.Segment != seg {
		t.Fatal("the two files' content landed in different segments, want one shared sealed segment")
	}
	size := setSize(t, c, seg)

	// The overwrite and its parked sync, exactly the held row's shape: the
	// truncation's removal lands beyond the sync's captured prefix.
	mustWrite(t, c, s, f, 0, make([]byte, payload))
	home := recordOf(t, c, f, 0).Record.Segment
	if home == seg {
		t.Fatal("the overwrite landed in the sealed segment, want the stream's active segment past it")
	}
	leadSync := seam.Hold(t, kit.OpSync, format.SegmentName(home))
	syncDone := make(chan error, 1)
	go func() { syncDone <- c.Durability().Sync(ctx, f) }()
	select {
	case <-leadSync.Reached():
	case <-time.After(5 * time.Second):
		t.Fatal("the overwrite's sync was never issued: no fsync reached the seam's hold")
	}

	truncDone := make(chan error, 1)
	go func() {
		_, err := c.Truncate(ctx, s, g, 0)
		truncDone <- err
	}()
	waitIn(t, 5*time.Second, "the truncation to cut g's content out of the index", func() bool {
		ext, err := c.Extents(s, g)
		return err == nil && len(ext) == 0
	})
	if !c.liveness.Held(seg) {
		t.Fatal("the sealed segment answered not held while f's overwritten content kept it retained, want held")
	}

	// The fault: the one sync already counted for the removal's segment —
	// the overwrite's parked call — passes, and every later one fails, so
	// whichever the truncation's wait and the seal below issue, the segment
	// ends with the removal covered by nothing. The hold below parks the
	// first write to a segment file after it — the window's own re-append,
	// which rolls the stream's next segment first.
	if err := seam.Schedule(kit.FailingFrom(kit.OpSync, format.SegmentName(home), 2, 0, boom)); err != nil {
		t.Fatalf("the seam refused the sync fault: %v", err)
	}
	writeHold := seam.HoldFirst(t, kit.OpWrite, "*.seg")

	// The overwrite's sync completes and its lift offers the sealed
	// segment, gated on the overwrite alone. The failing seal then ends the
	// removal's segment, and its end event takes the unsynced removal out of
	// the ledger into the pending window.
	leadSync.Release()
	select {
	case err := <-syncDone:
		if err != nil {
			t.Fatalf("the overwrite's Sync after its parked fsync was released: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the overwrite's Sync never returned once its parked fsync was released")
	}
	if _, err := c.Streams().For(g).Seal(true); err == nil {
		t.Fatal("the seal succeeded over a segment whose syncs the seam fails, want the failure to end it")
	}
	waitIn(t, 5*time.Second, "the failed segment's window to take the unsynced removal", func() bool {
		return c.Durability().WindowOpen(home)
	})
	select {
	case err := <-truncDone:
		t.Fatalf("Truncate returned (%v) while its record sat unsynced in the pending window, want it to wait for durability", err)
	default:
	}

	// Bounded window, not a truth: full scan passes run inside it, and no
	// hand-over may pass while the removal sits unsynced in the failed
	// segment's window — the offer's own location, the completed overwrite,
	// answers durable already.
	notWithin(t, 2*time.Second, "a retirement of the segment while the removal sat unsynced in the failed segment's window", func() bool {
		return !inSet(c, seg) || c.Retirer().Counters().FreedBytes != 0
	})

	// The release lets the window re-append the removal, the new place sync
	// and the window resolve: the truncation follows its record there and
	// returns, and the held-back candidate retires once. The failed segment
	// stays — the live content of the overwrite keeps it held.
	writeHold.Release()
	select {
	case err := <-truncDone:
		if err != nil {
			t.Fatalf("the truncation after its record's re-append was released: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the truncation never returned once its record's window resolved")
	}
	retiredTo(t, c, "the segment to retire once the removal's window resolved", uint64(size), seg)
	if got, want := c.Retirer().Counters().FreedBytes, uint64(size); got != want {
		t.Fatalf("FreedBytes after the retirement = %d, want %d (the segment's accounted size %d)", got, want, size)
	}
	if got := c.set.Counters().DoubleRetires; got != 0 {
		t.Fatalf("DoubleRetires = %d, want 0", got)
	}
	if !inSet(c, home) {
		t.Fatal("the failed segment left the set, want the live overwrite's content to keep it held")
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
	retiredTo(t, c, "the recycled segments to leave the set", uint64(len(sealed))*uint64(testSegmentCap), sealed...)

	// A recycle's hooks: the footprint does not fall (the cap moved into the
	// spare pool, it did not return to the filesystem), the pool is one
	// spare richer per recycle, the retired ids are gone from the set, and
	// the witness released — the tracker heard the retirement under the old
	// ids. The two live gauges are waited to their settled values first:
	// the retirer's completion kicks the pool's counters mid-flight, so a
	// point-in-time read races the rebalancing rather than the recycle.
	waitIn(t, 5*time.Second, "the recycled cap to settle in the set and the pool", func() bool {
		return c.set.Counters().UsedBytes == used &&
			c.pool.Counters().Spares == sparesBefore+int64(len(sealed))
	})
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
	retiredTo(t, c, "the emptied segment to leave the set", uint64(testSegmentCap), seg)
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

	// Bounded window, not a truth: a full scan pass runs inside it, and
	// the ended segment with an open window must stay.
	notWithin(t, 2*time.Second, "a retirement of the ended segment while its window stood open", func() bool {
		return !inSet(c, seg) || c.Retirer().Counters().FreedBytes != 0
	})

	// The release lets the window's re-append run, the index leave the
	// failed segment's places and the window close: the retirement follows.
	writeHold.Release()
	retiredTo(t, c, "the ended segment to leave the set once its window closed", uint64(testSegmentCap), seg)
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

	// Bounded window, not a truth: at least a full scan pass runs inside it,
	// each finding the segment offered by the scan and its claim
	// refused.
	notWithin(t, 2*time.Second, "a retirement of the claimed segment", func() bool {
		return !inSet(c, seg) || c.Retirer().Counters().FreedBytes != 0
	})

	// The claim back, and the scan's next offer retires the segment: the
	// refusal dropped the candidate, and the worker re-offers what the scan
	// finds rather than wedging on the segment it lost.
	c.set.Release(seg)
	retiredTo(t, c, "the segment to retire once the claim was released", uint64(testSegmentCap), seg)
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
	// the worker retires it somewhere between the pass's claims. The race
	// runs for a bounded adversarial window: a real pass holds each claim
	// across its own work, while this one spins claim-and-release, and
	// demanding the worker WIN that spin inside a wall clock would test the
	// machine, not the journal. What the window does pin is the ceiling:
	// however the claims serialize, the journal never frees more than the
	// one segment.
	if _, err := c.Truncate(ctx, s, f, 0); err != nil {
		t.Fatalf("Truncate(%d, 0): %v", f, err)
	}
	notWithin(t, 3*time.Second, "a retirement beyond the one segment while the pass raced the worker", func() bool {
		return c.Retirer().Counters().FreedBytes > uint64(testSegmentCap)
	})

	// The race stops, the one retirement settles to completion, and the
	// footprint fell by the segment exactly once.
	close(stop)
	<-done
	retiredTo(t, c, "the racing pass and the worker to settle on one retirement", uint64(testSegmentCap), seg)

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
	retiredTo(t, c, "the emptied segment to leave the set", uint64(testSegmentCap), seg)

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
	// The wait is also what keeps the candidate queued: drop the prompt-only
	// tick decision and this candidate retires inside the tick the close
	// joins within, reddening this row on its own assertion.
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

	// A mark at the last copy, not past it, proves nothing — the segment
	// stays.
	c.liveness.MarksRecorded(copies, last)
	if !inSet(c, seg) {
		t.Fatal("the ended segment retired on a mark that did not pass its last copy, want the hold to stand")
	}

	// A mark past the last copy proves the copies: the hold releases, the
	// crossing offers the segment and the retirement follows.
	c.liveness.MarksRecorded(copies, last+1)
	waitIn(t, 5*time.Second, "the held-ended segment's crossing to fire", func() bool { return !c.liveness.Held(seg) })
	retiredTo(t, c, "the ended segment to leave the set once its copies were proven", uint64(testSegmentCap), seg)
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
// keeps its place in the same queue beside it: with the hand-over's
// pending-record barrier no retirement may pass the refused record on any
// stream, so the two wait together, and the release retires both, which is
// the proof the worker never wedged on the candidate it kept refusing.
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
	// causality record syncs fine: the barrier holds its retirement back too,
	// side by side with the refused candidate, for no hand-over may pass any
	// stream's unsynced record. Bounded window, not a truth: full scan passes
	// run inside it, and both candidates must stay put behind the held sync.
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
	notWithin(t, 2*time.Second, "a retirement of either segment while f's record stood unsynced", func() bool {
		return !inSet(c, seg) || !inSet(c, other) || c.Retirer().Counters().FreedBytes != 0
	})

	// The release opens the barrier for both candidates at once, and the
	// worker takes them rather than wedging: the unrelated segment and the
	// refused one both retire, one each, from the queue the refusals kept.
	hold.Release()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("the held file's Truncate after the release: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the held file's Truncate never returned after its record's sync was released")
	}
	retiredTo(t, c, "both retirements once the held record went durable", uint64(2*testSegmentCap), seg, other)
	if got, want := c.Retirer().Counters().FreedBytes, uint64(2*testSegmentCap); got != want {
		t.Fatalf("FreedBytes after both retirements = %d, want %d", got, want)
	}
	if got := c.set.Counters().DoubleRetires; got != 0 {
		t.Fatalf("DoubleRetires after both retirements = %d, want 0", got)
	}
}

// TestSteadyWritesOnAnotherStreamDoNotStarveRetirement: a retirement whose
// emptiness is owed to a durable record must not wait on records that have
// nothing to do with it. The writer below keeps another stream churning —
// appending one unsynced record after another, so the stream almost
// always holds an unsynced record at any given tick — while the row parks
// the truncation's own sync, so the candidate's passes spread over several
// ticks, sampling the writer's traffic again and again. Once that sync is
// released, the segment retires within a few ticks while the writer still
// runs: the worker froze the other stream's pending tail into the candidate
// at its first not-held pass and waits on that frozen sample, which the
// rolls drain, not on a fresh one whose every sample names the writer's
// newest record. Red is the re-sample: a barrier that asks the whole
// journal again on every pass waits for a moment no stream holds an
// unsynced record, which the writer never grants, and the segment starves
// while the writer runs.
func TestSteadyWritesOnAnotherStreamDoNotStarveRetirement(t *testing.T) {
	ctx := context.Background()
	c, seam := newTestCore(t)
	s := mustShare(t, c, 1)
	f := testFile(1)
	g := otherStreamFile(c, f)

	mustWrite(t, c, s, f, 0, make([]byte, 3*testSegmentCap/2))
	syncFiles(t, c, f)
	waitIn(t, 5*time.Second, "the pool to refill to its target", func() bool {
		return c.pool.Counters().Spares >= int64(c.contentTarget)
	})
	seg := recordOf(t, c, f, 0).Record.Segment
	active := recordOf(t, c, f, 3*testSegmentCap/2-1).Record.Segment

	// The writer: one unsynced record after another, each
	// at a fresh offset so it supersedes nothing, never calling a sync of its
	// own — exactly the shape the re-sample starves under: the stream's
	// records drain only at the rolls that seal its full segments, a pass
	// sampling between two rolls always finds unsynced records — and nothing
	// the writer does kicks the retire worker, so its passes stay on the
	// tick alone. The rolls are also what a frozen sample rides on: every
	// record the freeze caught goes durable at the next roll, so the frozen
	// tail clears without the writer's help.
	writeErr := make(chan error, 1)
	stop := make(chan struct{})
	stopped := make(chan struct{})
	// A roll may find no spare ready yet — the maker readies one in the
	// background, and the write the roll would carry is refused with the
	// named retryable spare limit until it does. The writer rides that
	// refusal within a bound and fails on any other error.
	spareDeadline := time.Now().Add(5 * time.Second)
	go func() {
		defer close(stopped)
		var off int64
		for {
			select {
			case <-stop:
				return
			default:
			}
			if _, err := c.WriteAt(ctx, s, g, off, make([]byte, 4<<10), 1); err != nil {
				var ns *core.NoSpaceError
				if errors.As(err, &ns) && ns.Limit == core.LimitSpare && time.Now().Before(spareDeadline) {
					time.Sleep(time.Millisecond)
					continue
				}
				select {
				case writeErr <- err:
				default:
				}
				return
			}
			off += 4 << 10
		}
	}()
	t.Cleanup(func() { close(stop); <-stopped })

	// The truncation's own sync parks at the seam, so the worker's passes
	// spread over several full ticks while the candidate waits on the
	// parking record — each pass sampling the writer's churning stream.
	hold := seam.Hold(t, kit.OpSync, format.SegmentName(active))
	truncDone := make(chan error, 1)
	go func() {
		_, err := c.Truncate(ctx, s, f, 0)
		truncDone <- err
	}()
	waitIn(t, 5*time.Second, "the truncation's sync to reach the parked call", func() bool {
		return hold.Waiting() != 0
	})
	// A bounded window the retirement cannot cross in either shape: the
	// offered record is unsynced while the park stands, so the candidate
	// stays queued whatever the barrier samples.
	notWithin(t, 2*time.Second, "a retirement of the truncated segment while its own record parked", func() bool {
		return !inSet(c, seg)
	})

	// The release: the truncation's record goes durable while the writer
	// still runs, and the segment retires within a few ticks.
	hold.Release()
	select {
	case err := <-truncDone:
		if err != nil {
			t.Fatalf("the truncation after its parked sync was released: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the truncation never returned once its parked sync was released")
	}
	gone(t, c, "the truncated segment to retire behind the steady writer", seg)
	select {
	case err := <-writeErr:
		t.Fatalf("the writer failed: %v", err)
	default:
	}
	if !inSet(c, recordOf(t, c, g, 0).Record.Segment) {
		t.Fatal("the writer's segment was retired, want the writer's own records to keep it")
	}
	if got := c.set.Counters().DoubleRetires; got != 0 {
		t.Fatalf("DoubleRetires = %d, want 0", got)
	}
}

// TestAFailedWindowsResolutionHoldsItsSegmentAndItsLossRecord: the window
// resolution's cut is the one whose causality record does not exist when
// the index cut runs — the failure pass appends the loss record after it
// cut the failed segment's records out of the index. The row parks the loss
// record's append at that exact crossing: the ended segment answers not
// held, its window holds the failed records, nothing has named the loss
// record anywhere, and no retirement may take the empty segment while any
// of that stands. The parked append released, the loss record lands, syncs
// and registers — and stays held exactly as long as the file's state keeps
// it: until the failed segment's own retirement — the first re-test event
// the file ever sees — drops the ranges the loss still outranked and
// re-tests it. Released, the segment carrying the loss record, sealed holding
// only that record, retires too: a loss record's liveness ends at the
// re-test events, never at the journal's close.
func TestAFailedWindowsResolutionHoldsItsSegmentAndItsLossRecord(t *testing.T) {
	c, seam := newTestCore(t)
	s := mustShare(t, c, 1)
	f := testFile(1)
	boom := errors.New("the injected sync failure")

	const payload = 4 << 10
	mustWrite(t, c, s, f, 0, make([]byte, payload))
	seg := recordOf(t, c, f, 0).Record.Segment

	// The fault on the segment's own sync and on its reads back: the failed
	// seal ends the segment, and its window, which cannot re-append the
	// record from pages that do not read back, fails it. The hold below
	// parks the first write to any segment after the seal — the loss record
	// the failure pass appends: an early seal that failed its sync writes
	// nothing more to the segment files.
	writeHold := seam.HoldFirst(t, kit.OpWrite, "*.seg")
	if err := seam.Schedule(
		kit.Failing(kit.OpSync, format.SegmentName(seg), 1, boom),
		kit.Failing(kit.OpRead, format.SegmentName(seg), 0, errors.New("the injected read failure")),
	); err != nil {
		t.Fatalf("the seam refused the window's faults: %v", err)
	}
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

	// Bounding window, not a truth: the failure pass has cut the segment's
	// records out of the index — the parked append is the loss record the
	// cut ran ahead of — and full scan passes run inside the window, each
	// of which must find the segment held back: by its pending window's
	// records, which the barrier's window walk names, and by the ended
	// candidate's window re-read, which refuses while the window answers
	// open.
	// The bound sits above the worker's whole one-tick grace envelope: a
	// prompt-only candidate the park leaves with nothing to wait on would
	// still take two ticks to leave — the first pass's one-tick wait, then
	// the hand-over — so the window must outlast both to prove anything.
	notWithin(t, 3500*time.Millisecond, "a retirement of the ended segment between its cut and its loss record", func() bool {
		return !inSet(c, seg) || c.Retirer().Counters().FreedBytes != 0
	})

	// The ended segment's unlink parks at the seam, so the resolution below
	// cannot run the retirement whose re-test ends the loss record's
	// liveness: the held-phase assertions read a state nothing can race.
	// The pattern names the segment alone, so the pool's spare files and
	// any other retirement pass untouched.
	unlinkHold := seam.Hold(t, kit.OpRemove, format.SegmentName(seg))
	t.Cleanup(unlinkHold.Release)

	// The release lets the loss record land: appended, synced, registered
	// with the tracker before its segment can seal, and the moved report the
	// durability layer makes names the segment gated on it.
	writeHold.Release()
	waitIn(t, 5*time.Second, "the failed segment's window to resolve once its loss record landed", func() bool {
		return !c.Durability().WindowOpen(seg)
	})
	lossSeg := lossRecordSegmentOf(t, seam)
	if lossSeg == 0 {
		t.Fatal("no loss record reached the disk once the window resolved, want the failure's crossing recorded")
	}
	if !c.liveness.Held(lossSeg) {
		t.Fatal("the segment carrying the loss record answered not held while the failed segment's retirement was parked, want the record's registration to keep it")
	}

	// The parked unlink released, the ended segment retires: its durable
	// retirement relay re-tests the file — the first in-process event that
	// ever names the loss record — and the records the failed segment still
	// outranked have left the drop ranges with the segment, so the loss's
	// registration releases with them.
	unlinkHold.Release()
	gone(t, c, "the ended segment to retire once its window resolved", seg)
	waitIn(t, 5*time.Second, "the loss record's registration to be released by the failed segment's own retirement", func() bool {
		return !c.liveness.Held(lossSeg)
	})

	// The loss record's own segment, sealed early holding only that record,
	// now answers held by nothing — a loss record's liveness ended at the
	// re-test event, its segment retires with it.
	if _, err := c.Streams().For(f).Seal(true); err != nil {
		t.Fatalf("the early seal of the loss record's segment: %v", err)
	}
	if st, err := c.set.State(lossSeg); err != nil || st != segment.Sealed {
		t.Fatalf("the loss record's segment's state = %v, %v, want Sealed", st, err)
	}
	lossSize := setSize(t, c, lossSeg)
	retiredTo(t, c, "the loss record's segment to retire once its registration went", uint64(testSegmentCap+lossSize), lossSeg)
	if got, want := c.Retirer().Counters().FreedBytes, uint64(testSegmentCap+lossSize); got != want {
		t.Fatalf("FreedBytes after both retirements = %d, want %d (the ended segment's cap %d and the loss record segment's %d)", got, want, testSegmentCap, lossSize)
	}
	if got := c.set.Counters().DoubleRetires; got != 0 {
		t.Fatalf("DoubleRetires = %d, want 0", got)
	}
}

// lossRecordSegmentOf scans the seam's segment files for a loss record and
// returns the segment it finds one in, zero over none. It reads the live
// directory, so a record whose sync has not completed is seen as soon as
// its append ran.
func lossRecordSegmentOf(t *testing.T, seam *kit.Seam) core.SegmentID {
	t.Helper()
	names, err := seam.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	for _, name := range names {
		kind, id := format.ClassifyName(name)
		if kind != format.NameSegment {
			continue
		}
		f, err := seam.Open(name)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			t.Fatalf("Open %s: %v", name, err)
		}
		size, err := f.Size()
		if err != nil {
			t.Fatalf("Size %s: %v", name, err)
		}
		sc, err := format.ScanSegment(f, size, testIdentity, id)
		if err != nil {
			// A segment the row faults the reads of shows nothing the
			// scan can name; the loss record lives in a healthy one.
			continue
		}
		for _, r := range sc.Records {
			if r.Header.Kind == format.KindLoss {
				return id
			}
		}
	}
	return 0
}

// TestASealedSegmentHoldingOnlyATruncateRecordStays: the removal path's
// registration is the only thing that keeps a segment whose held content a
// removal cut away while the removal record itself still rides in it. The
// row empties the file, seals the segment that now holds nothing but the
// truncate record, rolls the stream off it and keeps writing: the tick
// scan offers the not-held-looking segment every second — without the
// registration the removal path makes before the marker's cut, one of
// those offers retires it.
func TestASealedSegmentHoldingOnlyATruncateRecordStays(t *testing.T) {
	ctx := context.Background()
	c, _ := newTestCore(t)
	s := mustShare(t, c, 1)
	f := testFile(1)

	mustWrite(t, c, s, f, 0, make([]byte, 3*testSegmentCap/2))
	syncFiles(t, c, f)
	waitIn(t, 5*time.Second, "the pool to refill to its target", func() bool {
		return c.pool.Counters().Spares >= int64(c.contentTarget)
	})
	// The segment the truncate record will land in: the stream's active
	// one, holding the write's tail.
	holder := recordOf(t, c, f, 3*testSegmentCap/2-1).Record.Segment

	if _, err := c.Truncate(ctx, s, f, 0); err != nil {
		t.Fatalf("Truncate(%d, 0): %v", f, err)
	}
	syncFiles(t, c, f)
	if _, err := c.Streams().For(f).Seal(true); err != nil {
		t.Fatalf("the early seal of the segment holding the truncate record: %v", err)
	}
	if st, err := c.set.State(holder); err != nil || st != segment.Sealed {
		t.Fatalf("the truncate holder's state = %v, %v, want Sealed", st, err)
	}

	// Roll the stream off the sealed segment and write on: the sealed one
	// takes nothing more.
	mustWrite(t, c, s, f, 0, make([]byte, 4<<10))
	syncFiles(t, c, f)

	// Bounding window, not a truth: full scan passes run inside it, each
	// finding the segment offered unless the registration holds it.
	notWithin(t, 4*time.Second, "a retirement of the segment holding the truncate record", func() bool {
		return !inSet(c, holder)
	})
	if !c.liveness.Held(holder) {
		t.Fatal("the truncate record's answer to Held was released while its record remained, want the registration to hold its segment for the process's life")
	}
	if got := c.set.Counters().DoubleRetires; got != 0 {
		t.Fatalf("DoubleRetires = %d, want 0", got)
	}
}

// TestTheDurableRetirementSignalWaitsForTheDirectorySync: the tracker hears
// a retirement only through the durable-retirement signal, the one that
// fires once the unlink has been made durable by the executor's directory
// sync. With that sync failing at the seam, the unlink happens, the segment
// leaves the set — and the tracker still holds every registration the
// retired segment carried: a crash between the unlink and the sync has the
// next open rebuild the segment from its surviving file, so releasing the
// registrations on an unlink nothing made durable would treat a retirement
// the crash undoes as done. The green twin runs first, its directory
// sync intact: the identical retirement releases its witness. The fault
// then stays on through the failed twin, so no later sync can race it.
func TestTheDurableRetirementSignalWaitsForTheDirectorySync(t *testing.T) {
	c, seam := newTestCore(t)
	s := mustShare(t, c, 1)
	f := testFile(1)
	g := otherStreamFile(c, f)

	// One early-sealed segment per stream, each carrying a forget record's
	// witness whose snapshot names it: undroppable until the tracker hears
	// the segment's durable retirement.
	const payload = 4 << 10
	// Both seals first, then both witness homes: a home write that runs
	// between another file's seal and its own would land in the segment
	// that seal is about to make, keeping it held and breaking the row's
	// own shape.
	seg := earlySealedWithWitness(t, c, s, f, payload)
	segB := earlySealedWithWitness(t, c, s, g, payload)
	rec := witnessOn(t, c, s, testFile(10), seg)
	recB := witnessOn(t, c, s, testFile(11), segB)

	// The green twin first: g's retirement, its directory sync intact,
	// carries the signal and releases its own witness only.
	mustWrite(t, c, s, g, 0, make([]byte, payload))
	syncFiles(t, c, g)
	gone(t, c, "the segment whose retirement's directory sync succeeded", segB)
	// The signal the witness waits on rides on the retire executor's own
	// directory sync, which runs after the segment has already left the
	// set — so a read right after gone races it.
	waitIn(t, 8*time.Second, "the durably retired segment's witness to be released", func() bool {
		return c.liveness.Droppable(recB)
	})
	if c.liveness.Droppable(rec) {
		t.Fatal("the other segment's witness was released by the first retirement, want each retirement's signal to name only its own segment")
	}

	// Every directory sync fails from here to the end of the row, so no
	// later sync can race the failed one.
	if err := seam.Schedule(kit.Failing(kit.OpSyncDir, "*", 0, errors.New("the failed directory sync"))); err != nil {
		t.Fatalf("the seam refused the directory sync fault: %v", err)
	}

	// The failed-sync twin: f's overwrite empties its sealed segment, the
	// retirement unlinks, the directory sync fails, and the witness stays.
	mustWrite(t, c, s, f, 0, make([]byte, payload))
	syncFiles(t, c, f)
	gone(t, c, "the segment whose retirement's directory sync failed", seg)
	// Bounding window, not a truth: the sync runs after the segment leaves
	// the set, and a signal it wrongly sent would land inside it.
	notWithin(t, 2*time.Second, "the failed-sync witness's release", func() bool {
		return c.liveness.Droppable(rec)
	})
}

// earlySealedWithWitness writes payload at 0 of f through s, syncs it and
// seals the segment early, returning the sealed segment.
func earlySealedWithWitness(t *testing.T, c *Core, s *Share, f core.FileID, payload int) core.SegmentID {
	t.Helper()
	mustWrite(t, c, s, f, 0, make([]byte, payload))
	syncFiles(t, c, f)
	if _, err := c.Streams().For(f).Seal(true); err != nil {
		t.Fatalf("the early seal: %v", err)
	}
	return recordOf(t, c, f, 0).Record.Segment
}

// witnessOn registers a forget record whose snapshot names seg and returns
// it: undroppable until the tracker hears seg's durable retirement. The
// record's own registration lands on the segment a throwaway write of home
// took — never on seg itself, whose registration would hold the very
// segment the witness watches.
func witnessOn(t *testing.T, c *Core, s *Share, home core.FileID, seg core.SegmentID) reclaim.Rec {
	t.Helper()
	mustWrite(t, c, s, home, 0, make([]byte, 4<<10))
	syncFiles(t, c, home)
	rec := reclaim.Rec{
		Kind: format.KindForget,
		File: testFile(9),
		Seg:  recordOf(t, c, home, 0).Record.Segment,
		Pair: index.Numbers{Version: core.Version{Counter: 7}, Seq: 1},
		Seq:  1,
	}
	c.liveness.RegisterForget(rec, []core.SegmentID{seg})
	if c.liveness.Droppable(rec) {
		t.Fatal("the witness answered droppable over a segment of the set, want it undroppable until the durable retirement")
	}
	return rec
}

// TestACrashSparesASegmentWhoseEmptierWasUnsynced: a retirement that would
// run ahead of its emptier's sync loses acknowledged content at a crash —
// the unlink is durable, the record that emptied the segment is not, and
// the rebuild holds nothing over the file the retirement removed. The row
// parks the emptier's own sync at the seam, so two covers hold the
// retirement back, the superseded content's retention and the barrier, and
// it crashes inside that window: the
// durable image keeps the emptied segment's file and its synced content,
// because no unlink was allowed to reach the directory at all.
func TestACrashSparesASegmentWhoseEmptierWasUnsynced(t *testing.T) {
	c, seam := newTestCore(t)
	s := mustShare(t, c, 1)
	f := testFile(1)

	const payload = 4 << 10
	v0 := mustWrite(t, c, s, f, 0, make([]byte, payload))
	syncFiles(t, c, f)
	if _, err := c.Streams().For(f).Seal(true); err != nil {
		t.Fatalf("the early seal: %v", err)
	}
	seg := recordOf(t, c, f, 0).Record.Segment

	// The emptier's sync parks here: the overwrite publishes and reports
	// the emptied segment, but no sync of its own record can complete —
	// not a caller's, not the sync bound's, not a roll's.
	hold := seam.Hold(t, kit.OpSync, "*.seg")
	t.Cleanup(hold.Release)

	mustWrite(t, c, s, f, 0, make([]byte, payload))

	// The retirement cannot cross the window whatever the interleaving: a
	// hand-over would unlink the segment while the record that emptied it
	// is unsynced, and the crash below would find the file gone.
	notWithin(t, 2*time.Second, "a retirement of the segment whose emptier was unsynced", func() bool {
		return !segmentFilePresent(t, seam, seg)
	})

	if err := seam.Crash(); err != nil {
		t.Fatalf("the crash: %v", err)
	}
	crashedSeams.Store(seam, true)

	if !segmentFilePresent(t, seam, seg) {
		t.Fatal("the emptied segment's file was unlinked in the crash's durable image, want no retirement to have reached the directory while its emptier was unsynced")
	}
	var old bool
	for _, r := range readRecords(t, seam) {
		if r.Header.File == f && r.Header.Version == v0 {
			old = true
		}
	}
	if !old {
		t.Fatal("the synced content of the spared segment was lost in the crash's durable image, want the record the retirement would have orphaned")
	}
}

// segmentFilePresent reports whether seg's file name is in the seam's
// directory listing.
func segmentFilePresent(t *testing.T, seam *kit.Seam, seg core.SegmentID) bool {
	t.Helper()
	names, err := seam.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	return slices.Contains(names, format.SegmentName(seg))
}

// TestTheSettleReTestsThePinnedRemovalSegment: a removal record's liveness
// ends at its marker's settle, not at the journal's close. The row truncates
// a file whose emptied content retires from one segment while the removal
// record itself sits sealed in another, then settles at the removal's own
// version: the settle relays the re-test to the tracker, the record's
// registration is released and its segment retires in-process. Without the
// relay nothing in-process fires the re-test, and the record holds its
// segment for the journal's life.
func TestTheSettleReTestsThePinnedRemovalSegment(t *testing.T) {
	ctx := context.Background()
	c, _ := newTestCore(t)
	s := mustShare(t, c, 1)
	f := testFile(1)
	w := otherStreamFile(c, f)

	// The content in one early-sealed segment, and a witness on another
	// stream waiting for its durable retirement, whose home write holds a
	// segment the row leaves alone.
	const payload = 4 << 10
	mustWrite(t, c, s, f, 0, make([]byte, payload))
	syncFiles(t, c, f)
	if _, err := c.Streams().For(f).Seal(true); err != nil {
		t.Fatalf("the early seal: %v", err)
	}
	seg := recordOf(t, c, f, 0).Record.Segment
	size := setSize(t, c, seg)
	rec := witnessOn(t, c, s, w, seg)

	// The truncation empties the sealed segment — which retires on the
	// truncation's own offer — and lands its own record in the stream's next
	// active segment, registered live for as long as its marker stands.
	tr, err := c.Truncate(ctx, s, f, 0)
	if err != nil {
		t.Fatalf("Truncate(%d, 0): %v", f, err)
	}
	gone(t, c, "the emptied content segment", seg)
	// The witness releases only once the tracker has heard the retirement in
	// full, so the settle below re-tests the removal record only after the
	// file's drop ranges naming the emptied segment have left the index.
	waitIn(t, 8*time.Second, "the retired segment's witness to be released", func() bool {
		return c.liveness.Droppable(rec)
	})

	// The removal record's own segment, sealed early too and holding it
	// alone: of the snapshot's members only it and the witness's home
	// remain, and the witness's home stays for its live content.
	if _, err := c.Streams().For(f).Seal(true); err != nil {
		t.Fatalf("the early seal of the removal record's segment: %v", err)
	}
	var home core.SegmentID
	for _, r := range c.set.Snapshot() {
		if r.ID == rec.Seg {
			continue // the witness's home, live content the row leaves alone
		}
		home = r.ID
		if r.State != segment.Sealed {
			t.Fatalf("the removal record's segment state = %v, want Sealed", r.State)
		}
	}
	if home == 0 {
		t.Fatal("the snapshot named no sealed segment for the removal record")
	}
	homeSize := setSize(t, c, home)
	if !c.liveness.Held(home) {
		t.Fatal("the removal record's segment answered not held before the settle, want its registration to pin it")
	}

	// Bounded window, not a truth: while its marker is unsettled no re-test
	// answers the record droppable, whatever the worker's ticks ask.
	notWithin(t, 2*time.Second, "a retirement of the removal record's segment before its marker settled", func() bool {
		return !inSet(c, home)
	})

	// The settle at the removal's own version releases the record: the
	// marker the index held for it is gone, and the candidate crosses the
	// segment it pinned to retirement.
	c.Settle(s, f, tr)
	if c.liveness.Held(home) {
		t.Fatal("Held of the removal record's segment after the settle = true, want false: the settle's re-test releases the record's registration")
	}
	retiredTo(t, c, "the removal record's segment to retire once the settle released it", uint64(size+homeSize), home)
	if got, want := c.Retirer().Counters().FreedBytes, uint64(size+homeSize); got != want {
		t.Fatalf("FreedBytes after both retirements = %d, want %d (the content segment's %d and the removal record's %d)", got, want, size, homeSize)
	}
	if got := c.set.Counters().DoubleRetires; got != 0 {
		t.Fatalf("DoubleRetires = %d, want 0", got)
	}
}

// TestTheDurableRetirementCarriesTheSegmentsFiles: a retirement must tell
// the index each file the retired segment held, which the tracker's own
// registry cannot name for content-only files — a file whose every record
// in the segment is content, never registered live. The row retires a
// sealed segment holding one file's content alone and checks the file's
// drop ranges naming it leave with the segment: a range left behind keeps
// every later removal of the file answering the outranking guard true
// against a segment a crash can no longer resurrect.
func TestTheDurableRetirementCarriesTheSegmentsFiles(t *testing.T) {
	c, _ := newTestCore(t)
	s := mustShare(t, c, 1)
	f := testFile(1)

	// One file's content in one early-sealed segment: a content-only
	// segment, whose files the tracker's registry never hears of.
	const payload = 4 << 10
	mustWrite(t, c, s, f, 0, make([]byte, payload))
	syncFiles(t, c, f)
	if _, err := c.Streams().For(f).Seal(true); err != nil {
		t.Fatalf("the early seal: %v", err)
	}
	seg := recordOf(t, c, f, 0).Record.Segment
	if got, want := c.index.DropRanges(), 1; got != want {
		t.Fatalf("DropRanges after the first write = %d, want %d: one range of one record", got, want)
	}

	// The overwrite past the seal retires the emptied segment once its own
	// record is durable, and notes the file's second range of the active
	// segment.
	mustWrite(t, c, s, f, 0, make([]byte, payload))
	if got, want := c.index.DropRanges(), 2; got != want {
		t.Fatalf("DropRanges after the overwrite = %d, want %d (one range per segment)", got, want)
	}
	syncFiles(t, c, f)
	gone(t, c, "the emptied content segment", seg)

	// The durable retirement relays each file the segment held into the
	// index's SegmentGone, so the file's range of the retired segment drops
	// with it. Nothing else wrote f, so the surviving range is the
	// overwrite's.
	waitIn(t, 5*time.Second, "the retired segment's drop ranges to leave the index", func() bool {
		return c.index.DropRanges() == 1
	})
	if got, want := c.index.DropRanges(), 1; got != want {
		t.Fatalf("DropRanges after the retirement = %d, want %d (the overwrite's range alone)", got, want)
	}
}
