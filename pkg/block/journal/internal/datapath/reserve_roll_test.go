package datapath

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/capacity"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/format"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/segment"
	"github.com/cubbit/dittofs/pkg/block/journal/journaltest/kit"
)

// TestReserveAhead_InterleavedRollTakesItsReplacementSpare checks R85's first
// half: a reserved write whose own payload fits any segment rolls anyway,
// because another writer filled the segment it held, and the roll takes a
// replacement spare back from the pool while the reservation's remaining bytes
// still need one. The pool's held figure is read from its state, not counted at
// the call: after the write the hold is back at the reservation's remaining
// need, the spare the interleaved roll spent replaced.
func TestReserveAhead_InterleavedRollTakesItsReplacementSpare(t *testing.T) {
	const payload = 4096
	cost := capacity.WriteCharge(payload, testSegmentCap)
	// Two writes: the second leaves bytes undrawn, so the reservation still
	// needs a spare after the first and the interleaved roll is replaced.
	_, spares := capacity.PiecesFor(2*cost, 2, testSegmentCap)
	c, _ := newTestCore(t, func(o *coreOptions) { o.contentSpares = spares + 1 })
	s := mustShare(t, c, 1)

	// Two files on one stream: the other writer's, and the reservation's.
	other := testFile(9)
	reservedFile := sameStreamAs(t, c, other, testFile(1))

	// The other writer fills the segment almost whole, leaving a room no
	// record of the reserved write's payload can take even one byte of: the
	// write's own bytes fit any segment, so the roll it meets is the fill's.
	warm := roomAfter(0) - 8
	if _, err := c.WriteAt(context.Background(), s, other, 0, make([]byte, warm), 1); err != nil {
		t.Fatalf("the other writer's fill: %v", err)
	}
	if room := c.Streams().For(other).Room(); room >= 16 {
		t.Fatalf("the other writer leaves a room of %d, want less than a record's overhead", room)
	}

	r := reserve(t, c, s, 2*cost, 2)
	defer r.Release()
	if _, err := c.WriteReserved(context.Background(), s, r, reservedFile, 0, make([]byte, payload), 1); err != nil {
		t.Fatalf("the reserved write beside the fill: %v", err)
	}
	// The hold owes exactly what the reservation still needs: the interleaved
	// roll's spare was replaced.
	if held, need := c.pool.Counters().Held, int64(r.NeedsSpares()); held != need {
		t.Fatalf("the pool owes %d spares after the interleaved roll, want the reservation's remaining need of %d: the roll took no replacement", held, need)
	}
}

// TestReserveAhead_OwnRollTakesNoReplacement checks the counterweight: a
// reserved write whose own payload rolls a stream the reservation alone
// touches takes no replacement, because both rolls are the write's own. The
// pool's held figure falls by the spares those rolls spent and stays there.
func TestReserveAhead_OwnRollTakesNoReplacement(t *testing.T) {
	// Two empty segments' room: the split rolls once past the first piece, and
	// the first piece rolls onto the stream's first segment.
	payload := make([]byte, 2*emptyRoomOf(testSegmentCap))
	cost := capacity.WriteCharge(int64(len(payload)), testSegmentCap)
	_, spares := capacity.PiecesFor(cost, 1, testSegmentCap)
	c, _ := newTestCore(t, func(o *coreOptions) { o.contentSpares = spares + 1 })
	s := mustShare(t, c, 1)

	r := reserve(t, c, s, cost, 1)
	defer r.Release()
	heldBefore := c.pool.Counters().Held
	if heldBefore < 2 {
		t.Fatalf("the reservation owes the pool %d spares, want at least its two rolls", heldBefore)
	}

	if _, err := c.WriteReserved(context.Background(), s, r, testFile(1), 0, payload, 1); err != nil {
		t.Fatalf("the reserved two-piece write: %v", err)
	}
	// The write rolled twice, both its own: the pool owes fewer than before,
	// and a replacement on either would have left the figure where it was.
	if got := c.pool.Counters().Held; got >= heldBefore {
		t.Fatalf("the pool owes %d spares after the write's own rolls, want fewer than the %d it owed before: a roll of its own was replaced", got, heldBefore)
	}
}

// TestReserveAhead_UnreservedRollRefusedWhileASpareIsHeld checks R85's second
// half: an unreserved roll that would leave an open reservation short of its
// spares is refused `spare` even though a spare IS ready — it is the
// reservation's, and no unreserved take reaches it. The plain write is
// admitted while the stream still has the room its payload fits, the room is
// spent meanwhile, and its append then rolls with nothing to roll onto.
func TestReserveAhead_UnreservedRollRefusedWhileASpareIsHeld(t *testing.T) {
	cost := capacity.WriteCharge(512, testSegmentCap)
	_, spares := capacity.PiecesFor(cost, 1, testSegmentCap)
	// The pool readies the reservation's spares beside the one the warm
	// write's first roll takes.
	c, seam := newTestCore(t, func(o *coreOptions) { o.contentSpares = spares + 1 })
	s := mustShare(t, c, 1)

	// One stream, three files: the warm record, the room-spender and the
	// plain write's. The warm record leaves the plain write's payload room,
	// so its admission grants it the room and no spare.
	anchor := testFile(9)
	plain := sameStreamAs(t, c, anchor, testFile(2))
	filler := sameStreamAs(t, c, anchor, plain)
	// The maker is gated before the warm write, so the one spare that write's
	// first roll takes is never replaced: the pool settles at the
	// reservation's own spares, kept ready for the hold and unreachable
	// unreserved.
	if err := seam.Schedule(kit.Failing(kit.OpAllocate, "*", 0, errMakeSpares)); err != nil {
		t.Fatalf("Schedule: %v", err)
	}
	warm := roomAfter(0) - 8192
	if _, err := c.WriteAt(context.Background(), s, anchor, 0, make([]byte, warm), 1); err != nil {
		t.Fatalf("the warm write: %v", err)
	}
	room := c.Streams().For(anchor).Room()
	if room < 512 {
		t.Fatalf("the warm write leaves a room of %d, want the plain write's 512 inside it", room)
	}

	waitIn(t, 5*time.Second, "the pool to settle at the reservation's spares", func() bool {
		return c.pool.Counters().Spares == int64(spares)
	})
	r := reserve(t, c, s, cost, 1)
	defer r.Release()
	if held, ready := c.pool.Counters().Held, c.pool.Counters().Spares; held != int64(spares) || ready != held {
		t.Fatalf("the pool holds %d of %d ready spares, want every ready spare held by the reservation", held, ready)
	}

	// The plain write is parked after its admission, before its append: its
	// grant is the room above, no spare.
	parked, release := make(chan struct{}), make(chan struct{})
	var parkOnce, openOnce sync.Once
	open := func() { openOnce.Do(func() { close(release) }) }
	// A failure below must not leave the parked write holding the file's
	// append domain, or the core's close never returns.
	t.Cleanup(open)
	c.beforeAppend = func() {
		parkOnce.Do(func() {
			close(parked)
			<-release
		})
	}
	errs := make(chan error, 1)
	go func() {
		_, err := c.WriteAt(context.Background(), s, plain, 0, make([]byte, 512), 1)
		errs <- err
	}()
	<-parked

	// The filler spends the room the plain write's grant claimed, with no
	// admission of its own: its record is the room left less its own header,
	// so it rolls nowhere and needs no spare.
	unlock := c.domains.Lock(filler)
	seq, _ := c.counters.NextSeq()
	if _, err := c.Streams().For(filler).Append(format.RecordHeader{
		Kind: format.KindWrite, File: filler, Extent: core.Extent{Len: room},
		Seq: seq, Tag: 1,
	}, format.RecordBody{Payload: make([]byte, room), MTime: 1}); err != nil {
		unlock()
		open()
		t.Fatalf("the filler's record of %d bytes: %v", room, err)
	}
	unlock()
	if got := c.Streams().For(anchor).Room(); got >= 512 {
		t.Fatalf("the filler leaves a room of %d, want less than the plain write's 512", got)
	}
	open()

	ns := refused(t, <-errs, core.LimitSpare)
	if ns.Missing < 1 || ns.Unit != core.UnitSegments {
		t.Fatalf("the refusal = %d %s, want a segment missing", ns.Missing, ns.Unit)
	}
	// The refusal was the hold's rule, not a shortage: the spare is still
	// ready, and it is the reservation's.
	if held, ready := c.pool.Counters().Held, c.pool.Counters().Spares; ready < 1 || ready != held {
		t.Fatalf("after the refused plain roll the pool has %d ready and %d held, want every ready spare still the reservation's", ready, held)
	}
	// The reservation's write still has its spare and lands.
	if _, err := c.WriteReserved(context.Background(), s, r, testFile(4), 0, make([]byte, 512), 1); err != nil {
		t.Fatalf("the reserved write after the refused plain roll: %v", err)
	}
}

// TestReserveAhead_CopyBesideARollingWriter checks R85's third criterion on a
// scaled fixture: a reserved copy of many writes lands whole while other
// writers keep rolling the same stream, with the pool at its target. No
// reserved write is refused; every entry and footer byte the copy drew is
// inside the reservation its caller took; and any `spare` refusal falls on an
// unreserved write.
//
// The fixture is the epic's 1 GiB copy in 1 MiB writes beside 10 GiB of other
// appends, scaled to keep the same number of rolls at a test's size: a 128 KiB
// copy in sixteen writes of one segment's room, beside 512 KiB of other
// appends, over 8 KiB segments. The copy crosses sixteen caps where the epic's
// crosses 1024, so the fixture rolls a segment every few writes rather than
// every few megabytes.
func TestReserveAhead_CopyBesideARollingWriter(t *testing.T) {
	const segCap = int64(8 << 10)
	// The reserved copy: sixteen writes of one segment's payload, which rolls
	// the stream once per write on an empty segment and once more per cap.
	const copyWrites = 16
	each := segment.EmptyRoom(segCap)
	cost := int64(copyWrites) * capacity.WriteCharge(each, segCap)
	_, spares := capacity.PiecesFor(cost, copyWrites, segCap)
	// The pool readies the reservation's spares, which it holds: the target
	// the pool is at, and the holds ride above it. The refill makes the spares
	// the other writers' rolls need beside them.
	c, _ := newTestCore(t, func(o *coreOptions) {
		o.segmentCap = segCap
		o.contentSpares = spares
		o.extentLimit = int64(spares)*4 + 1<<6
	})
	s := mustShare(t, c, 1)

	// The copy's files and the other writers' files all hash to one stream, so
	// every append of the fixture rolls that one stream.
	anchor := testFile(9)
	stream := c.Streams().IndexFor(anchor)
	copyFiles := make([]core.FileID, copyWrites)
	otherFiles := make([]core.FileID, copyWrites)
	for i := range copyFiles {
		copyFiles[i] = onStream(t, c, stream, core.NewFileID([16]byte{byte(i), 0xa1}))
		otherFiles[i] = onStream(t, c, stream, core.NewFileID([16]byte{byte(i), 0xb2}))
	}

	r := reserve(t, c, s, cost, copyWrites)
	defer r.Release()
	if heldBefore := c.pool.Counters().Held; heldBefore < 1 {
		t.Fatalf("the reservation owes the pool %d spares, want its copies' rolls held", heldBefore)
	}

	var copyRefused, spareOnOther atomic.Int64
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < copyWrites; i++ {
			if _, err := c.WriteReserved(context.Background(), s, r, copyFiles[i], int64(i)*each, make([]byte, each), 1); err != nil {
				copyRefused.Add(1)
				t.Errorf("reserved copy write %d of %d was refused: %v", i+1, copyWrites, err)
				return
			}
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < copyWrites; i++ {
			_, err := c.WriteAt(context.Background(), s, otherFiles[i], int64(i)*each, make([]byte, each), 1)
			if err == nil {
				continue
			}
			var ns *core.NoSpaceError
			if errors.As(err, &ns) && ns.Limit == core.LimitSpare {
				spareOnOther.Add(1)
				continue
			}
			t.Errorf("an unreserved append was refused for a reason other than a spare: %v", err)
		}
	}()
	wg.Wait()

	if got := copyRefused.Load(); got != 0 {
		t.Fatalf("reserved writes refused = %d of %d, want none", got, copyWrites)
	}
	// The copy drew every byte and entry of its ask and no more: what is left
	// is what its writes' worst-case charges over-covered, never below zero.
	if left := r.Remaining(); left < 0 {
		t.Fatalf("the reservation is over-drawn by %d record bytes", -left)
	}
	// The copy's entries stayed inside the reservation: every write published
	// its pieces from what the reservation held, so the held figure fell by
	// the pieces published and never went below zero.
	if held := c.HeldEntries(); held < 0 {
		t.Fatalf("the reservation's held entries are over-drawn by %d: a write drew an entry it held", -held)
	}
	// Any spare refusal the fixture's other writers met fell on them, never on
	// a reserved write: the copy's writes above are checked to have landed, and
	// the other writer's loop fails the test for any refusal that is not a
	// spare's. How often they interleave is the scheduler's, so no assertion
	// counts the refusals — a run where the copy and the other writer seldom
	// meet is still valid, and one where they meet must refuse the unreserved
	// write alone.
	// The hold is back at the reservation's remaining need: every spare the
	// copy's interleaved rolls spent was replaced, and no roll of its own took
	// a replacement it did not need.
	if held, need := c.pool.Counters().Held, int64(r.NeedsSpares()); held != need {
		t.Fatalf("the pool owes %d spares after the copy, want its remaining need of %d: a roll was replaced wrongly or not at all", held, need)
	}
	// Every piece the copy published is inside the reservation's draw: the
	// index holds one entry a piece and no piece of it was refused.
	for i, f := range copyFiles {
		if _, ok := c.Index().Lookup(f, int64(i)*each); !ok {
			t.Fatalf("the copy's write %d published no piece", i+1)
		}
	}
}

// onStream returns f when f hashes to stream, or the first id it finds that
// does, so a test can put a family of files on one append stream.
func onStream(t *testing.T, c *Core, stream int, f core.FileID) core.FileID {
	t.Helper()
	if c.Streams().IndexFor(f) == stream {
		return f
	}
	for i := 0; i < 512; i++ {
		g := core.NewFileID([16]byte{byte(i), 0x5c, byte(stream)})
		if c.Streams().IndexFor(g) == stream {
			return g
		}
	}
	t.Fatalf("no file id on stream %d", stream)
	return core.FileID{}
}

// TestReserveAhead_OwnStraddlesStayWithinTheRemainingNeed checks what the
// remaining-need rule guarantees for a copy of small writes alone on one stream:
// each write straddles the tail its predecessor left, so each rolls across a cap
// the reservation already sized, and the hold never exceeds what the copy's
// undrawn bytes still need. The rule may replace one of those rolls — the stream
// cannot tell them from another writer's, and the hold is then one over that
// need until the copy releases — but never more, and the hold never rises above
// the spares Reserve took.
func TestReserveAhead_OwnStraddlesStayWithinTheRemainingNeed(t *testing.T) {
	const writes = 6
	// Each write is half an empty segment's room, so a second write on the
	// same stream straddles the tail the first left and rolls.
	each := emptyRoomOf(testSegmentCap) / 2
	cost := int64(writes) * capacity.WriteCharge(each, testSegmentCap)
	_, spares := capacity.PiecesFor(cost, writes, testSegmentCap)
	c, _ := newTestCore(t, func(o *coreOptions) { o.contentSpares = spares })
	s := mustShare(t, c, 1)

	r := reserve(t, c, s, cost, writes)
	defer r.Release()
	if held := c.pool.Counters().Held; held != int64(spares) {
		t.Fatalf("the reservation takes %d spares, want its ask's %d", held, spares)
	}

	// Every write lands on one file of one stream, so write k+1 meets the tail
	// write k left and rolls across it. The hold is checked after every write:
	// it never exceeds the reservation's remaining need by more than the one
	// roll the rule may replace, and never exceeds what Reserve took.
	f := testFile(1)
	for i := 0; i < writes; i++ {
		if _, err := c.WriteReserved(context.Background(), s, r, f, int64(i)*each, make([]byte, each), 1); err != nil {
			t.Fatalf("reserved write %d of %d: %v", i+1, writes, err)
		}
		held, need := c.pool.Counters().Held, int64(r.NeedsSpares())
		if held > need+1 {
			t.Fatalf("after write %d the pool owes %d spares, want at most the remaining need's %d plus one", i+1, held, need)
		}
		if held > int64(spares) {
			t.Fatalf("after write %d the pool owes %d spares, want at most the %d Reserve took", i+1, held, spares)
		}
	}
	// Once the copy is spent, the hold is back at what its undrawn bytes need —
	// nothing left, so nothing held beyond the ask.
	if held, need := c.pool.Counters().Held, int64(r.NeedsSpares()); held != need {
		t.Fatalf("the pool owes %d spares after the copy, want its remaining need of %d", held, need)
	}
}

// TestReserveAhead_ReplacementBeatsACompetingUnreservedTake checks R85 end to
// end, with the refusal depending on the reservation: a reserved copy whose
// every write rolls a segment another writer filled lands whole although the
// interleaved rolls spend its hold, while an unreserved writer competing for the
// pool's last ready spare is refused — and the same writer is admitted once the
// reservation releases. The pool readies the reservation's spares and exactly
// one more, and its maker is gated, so only a replacement restores what an
// interleaved roll spends, and the spare the competitor needs is one the
// reservation holds.
func TestReserveAhead_ReplacementBeatsACompetingUnreservedTake(t *testing.T) {
	const writes = 2
	payload := int64(4096)
	cost := capacity.WriteCharge(payload, testSegmentCap)
	_, spares := capacity.PiecesFor(int64(writes)*cost, writes, testSegmentCap)
	// The pool readies the reservation's spares and two beyond them: the one the
	// warm write below rolls onto, and then, after it, exactly one more — the one
	// spare an unreserved take could reach and the one a replacement takes for an
	// interleaved roll. The maker is gated before the warm write, so that spare is
	// never replaced: the pool settles at the figure the scenario needs, without a
	// refill the test would have to race.
	c, seam := newTestCore(t, func(o *coreOptions) { o.contentSpares = spares + 2 })
	s := mustShare(t, c, 1)
	// newTestCore awaited the pool at its target; the gate makes that figure the
	// last one, so the warm write's roll takes exactly one and no make follows.
	if err := seam.Schedule(kit.Failing(kit.OpAllocate, "*", 0, errMakeSpares)); err != nil {
		t.Fatalf("Schedule: %v", err)
	}

	// The other writer's warm record leaves the stream a room too small for any
	// record, so every reserved write rolls a segment another writer filled.
	anchor := testFile(9)
	reservedFile := sameStreamAs(t, c, anchor, testFile(1))
	warm := roomAfter(0) - 8
	if _, err := c.WriteAt(context.Background(), s, anchor, 0, make([]byte, warm), 1); err != nil {
		t.Fatalf("the other writer's warm write: %v", err)
	}
	// The warm write spent its spare and nothing is made after the gate: the pool
	// holds the reservation's spares and exactly one beyond them, and only a
	// replacement restores a spare an interleaved roll spends.
	if got := c.pool.Counters().Spares; got != int64(spares+1) {
		t.Fatalf("the pool holds %d spares after the warm write, want the reservation's %d and one", got, spares)
	}

	r := reserve(t, c, s, int64(writes)*cost, writes)

	// Each write rolls a segment the other writer filled: its record takes the
	// remaining room with no admission, so it spends no spare, and the copy's
	// next record rolls a segment nothing of its own had filled.
	for i := 0; i < writes; i++ {
		room := c.Streams().For(anchor).Room()
		unlock := c.domains.Lock(anchor)
		seq, _ := c.counters.NextSeq()
		_, err := c.Streams().For(anchor).Append(format.RecordHeader{
			Kind: format.KindWrite, File: anchor, Extent: core.Extent{Len: room},
			Seq: seq, Tag: 2,
		}, format.RecordBody{Payload: make([]byte, room), MTime: 1})
		unlock()
		if err != nil {
			t.Fatalf("the other writer's fill %d: %v", i+1, err)
		}
		if _, err := c.WriteReserved(context.Background(), s, r, reservedFile, int64(i)*payload, make([]byte, payload), 1); err != nil {
			t.Fatalf("reserved write %d of %d beside a competing writer: %v", i+1, writes, err)
		}
	}
	// Both writes landed: the replacements kept the hold at the reservation's
	// remaining need, so neither roll met the pool's refusal. The hold is back at
	// that need, and nearer than without a replacement — every interleaved roll
	// after the first would have drawn it down by one and left the pool owing
	// less than its undrawn bytes need.
	if left := r.Remaining(); left < 0 {
		t.Fatalf("the reservation is over-drawn by %d record bytes", -left)
	}
	if held, need := c.pool.Counters().Held, int64(r.NeedsSpares()); held < need {
		t.Fatalf("the pool owes %d spares after the copy, want its remaining need of %d: an interleaved roll took no replacement", held, need)
	}

	// The pool has no spare an unreserved take can reach — the reservation holds
	// them — so the competing write on a roomless stream is refused `spare`.
	competing := core.FileID{}
	for i := 0; i < 256; i++ {
		f := core.NewFileID([16]byte{byte(i), 0xd4})
		if c.Streams().IndexFor(f) != c.Streams().IndexFor(anchor) {
			competing = f
			break
		}
	}
	if ready := c.pool.ReadyContent(); ready != 0 {
		t.Fatalf("the pool offers %d spares unreserved, want none while the reservation holds them", ready)
	}
	_, err := c.WriteAt(context.Background(), s, competing, 1<<20, make([]byte, payload), 1)
	_ = refused(t, err, core.LimitSpare)

	// The control: once the reservation releases, the same write is admitted —
	// the refusal was the reservation's hold, not a shortage of spares.
	r.Release()
	if _, err := c.WriteAt(context.Background(), s, competing, 2<<20, make([]byte, payload), 1); err != nil {
		t.Fatalf("the competing write after the reservation released: %v", err)
	}
}
