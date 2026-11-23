package durability_test

import (
	"context"
	"testing"

	"github.com/cubbit/dittofs/pkg/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/journal/internal/durability"
)

// TestTheLossRingLivesPerInstanceAndItsDedupPerDirectory catches a reopen
// in one process re-reporting a loss found on disk: the (dropped seq, extent)
// pieces a Lose or a Place has taken live per directory, so a Durability
// built later on the directory does not publish a piece an earlier one took
// — while the ring that holds the published events is this instance's own:
// a reopen starts an empty one, and the previous instance's live events are
// not a fresh drain's concern. The numbering starts over with the ring; the
// generation the events' raises move is the directory's still.
func TestTheLossRingLivesPerInstanceAndItsDedupPerDirectory(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()

	// A loss record found at open, placed through the first open's layer:
	// its ring holds the one event, numbered from its own count.
	first := core.LossEvent{File: file(2), Reason: core.LossCorrupt, Extent: core.Extent{Off: 0, Len: 3}}
	e.d.Place(first, 9)
	if got := e.d.Stats().Losses; len(got) != 1 || got[0].SeqNum != 1 {
		t.Fatalf("the first instance's ring after one Place = %+v, want the one event numbered 1", got)
	}

	// A reopen: the second Durability on the same directory, bound to the same
	// streams, the way a close and open in one process builds one. Its ring
	// starts empty: the pieces are the directory's, the events this
	// instance's own.
	reopen := e.newDurability()
	if got := reopen.Stats().Losses; len(got) != 0 {
		t.Fatalf("the reopen's ring = %+v, want none: a fresh instance holds no published events, the earlier instance's ring is its own", got)
	}

	// The same piece placed through the reopen publishes nothing anywhere:
	// the directory took it, so no open reports the loss record as new. A
	// new piece placed through the reopen publishes into the reopen's own
	// ring, numbered from its own count.
	reopen.Place(first, 9)
	if got := reopen.Stats().Losses; len(got) != 0 {
		t.Fatalf("the reopen's ring after re-placing a taken piece = %+v, want none: the piece dedup is the directory's", got)
	}
	if got := e.d.Stats().Losses; len(got) != 1 {
		t.Errorf("the first instance's ring = %+v, want its own one event: a taken piece publishes through no instance", got)
	}
	next := core.LossEvent{File: file(2), Reason: core.LossCorrupt, Extent: core.Extent{Off: 8, Len: 3}}
	reopen.Place(next, 10)
	if got := reopen.Stats().Losses; len(got) != 1 || got[0].SeqNum != 1 || got[0].Extent != next.Extent {
		t.Fatalf("the reopen's ring = %+v, want the one new piece, numbered 1 of its own count", got)
	}
	if got := e.d.Stats().Losses; len(got) != 1 {
		t.Errorf("the first instance's ring = %+v, want its own one event: the reopen publishes into its own ring alone", got)
	}

	// A piece Lost through the first instance publishes nothing through the
	// reopen: one loss record, one event in the first instance's ring, one
	// raise of the directory's generation.
	if _, err := e.d.Lose(ctx, loss(11)); err != nil {
		t.Fatalf("Lose through the first instance: %v", err)
	}
	ev, err := reopen.Lose(ctx, loss(11))
	if err != nil || ev != (core.LossEvent{}) {
		t.Fatalf("Lose of an already-reported piece through the reopen = %+v, %v, want the zero event and nil", ev, err)
	}
	if lh, _ := e.lossRecords(); len(lh) != 1 {
		t.Errorf("%d loss records for one dropped piece, want 1", len(lh))
	}
	if got := e.d.Stats().Losses; len(got) != 2 {
		t.Errorf("the first instance's ring after the Lose = %+v, want 2 events: the Place's and the Lose's", got)
	}
	if got := reopen.Stats().Losses; len(got) != 1 {
		t.Errorf("the reopen's ring holds %d events, want 1: another instance's Lose publishes nothing here", len(got))
	}

	// Place appended nothing, raised nothing and counted nothing, and the
	// generation the Lose raised is the directory's, through either
	// instance.
	if got := e.d.Generation(); got != 1 {
		t.Errorf("the first instance's generation = %d, want 1: raised once by the one Lose, never by a Place", got)
	}
	if got := reopen.Generation(); got != 1 {
		t.Errorf("the reopen's generation = %d, want the directory's 1", got)
	}

	// Another directory shares none of it: the same piece placed there is new,
	// published and numbered from 1, and raises nothing.
	other := buildEnv(t)
	other.d.Place(first, 9)
	p := other.d.Stats()
	if len(p.Losses) != 1 || p.Losses[0].SeqNum != 1 {
		t.Errorf("the other directory's ring = %+v, want the piece published anew and numbered 1", p.Losses)
	}
	if p.LossGeneration != 0 {
		t.Errorf("the other directory's generation = %d, want 0", p.LossGeneration)
	}
	if got := e.d.Stats().Losses; len(got) != 2 {
		t.Errorf("the first directory's ring changed to %d events after a Place at another directory, want 2", len(got))
	}
}

// TestATakeoverOfADeadUnreadClaimRepublishes holds the takeover's word: a
// loss whose record never landed — the append refused against a storage the
// crash closed — published its event only into the ring of an instance that
// stopped before any reader reached it, so the only report the loss had
// answered nothing. The reopen's pass hands its Lose the piece marked
// reported, the credit that dead report still wrongly carries, and the take
// that overtakes the retired claim clears it: the reopen publishes the loss
// into its own ring, raising nothing, for the dead instance's raise already
// counted the generation — the Raise the take sets even when the caller
// brought neither, the re-detected deferral's shape, whose first take raised
// the piece. The kept failed claim names nothing — the pass plans the record
// as unnamed — while a piece whose report a reader reached keeps its
// suppression: the takeover does not clear a credit its reader already
// took, and the pass still appends the record the claim never owned.
func TestATakeoverOfADeadUnreadClaimRepublishes(t *testing.T) {
	ctx := context.Background()

	t.Run("the unread report is republished", func(t *testing.T) {
		e := newEnv(t)
		// The crash shape's own order: the storage closes under the journal
		// first, then the instance's Stop records the loss its drain owes —
		// an append that cannot place and an event no reader takes away.
		e.shutdown()
		e.d.Stop()
		l := loss(11)
		l.Raised = true
		if _, err := e.d.Lose(ctx, l); err == nil {
			t.Fatalf("the dead instance's Lose over closed streams: succeeded, want the refused append")
		}

		reopen := e.newDurability()
		if reopen.Named(l.Seq, l.Extent) {
			t.Fatalf("Named answers the dead instance's kept failed claim: the pass would plan no record for the piece its report never answered")
		}
		l.Reported = true
		ev, err := reopen.Lose(ctx, l)
		if err == nil {
			t.Fatalf("the reopen's append over closed streams: succeeded, want the refused append")
		}
		if ev == (core.LossEvent{}) {
			t.Fatalf("the takeover kept the dead report's suppression: the reopen published no event for a loss no reader was told of")
		}
		if got := reopen.Stats().Losses; len(got) != 1 || got[0].SeqNum != 1 {
			t.Fatalf("the reopen's ring = %+v, want the one republished loss, numbered 1 of its own count", got)
		}
		if got := reopen.Generation(); got != 0 {
			t.Fatalf("the republished loss raised the generation to %d, want 0: the takeover keeps the raise the dead instance already made", got)
		}
	})

	t.Run("a re-plan the caller raised nothing for raises nothing new", func(t *testing.T) {
		e := newEnv(t)
		e.shutdown()
		e.d.Stop()
		// Neither Reported nor Raised: the re-detected deferred loss's
		// shape, whose Lose arrives with no flag because its first take
		// already did the raising the rule would do again over the same
		// drop.
		l := loss(13)
		if _, err := e.d.Lose(ctx, l); err == nil {
			t.Fatalf("the dead instance's Lose over closed streams: succeeded, want the refused append")
		}
		if got := e.d.Generation(); got != 1 {
			t.Fatalf("the dead instance's own take raised the generation to %d, want 1", got)
		}

		reopen := e.newDurability()
		ev, err := reopen.Lose(ctx, l)
		if err == nil {
			t.Fatalf("the reopen's append over closed streams: succeeded, want the refused append")
		}
		if ev == (core.LossEvent{}) {
			t.Fatalf("the takeover published nothing for a loss no reader was told of")
		}
		if got := reopen.Generation(); got != 1 {
			t.Fatalf("the republished loss raised the generation to %d, want the 1 its piece's first take already counted", got)
		}
	})

	t.Run("a report a reader reached keeps its suppression", func(t *testing.T) {
		e := newEnv(t)
		e.shutdown()
		e.d.Stop()
		l := loss(12)
		l.Raised = true
		if _, err := e.d.Lose(ctx, l); err == nil {
			t.Fatalf("the dead instance's Lose over closed streams: succeeded, want the refused append")
		}
		// A reader reaches the stopped instance's ring — its genuine read
		// still stands in this process — and carries the report away. The
		// reach, not the moment of it, is what the claim asks.
		if got := e.d.Losses(); len(got) != 1 {
			t.Fatalf("the stopped instance's ring = %+v, want the one refused append's event", got)
		}

		reopen := e.newDurability()
		if reopen.Named(l.Seq, l.Extent) {
			t.Fatalf("Named answers a kept failed claim that owns no record: the pass must plan the record the claim still owes however far the ring was read")
		}
		l.Reported = true
		ev, err := reopen.Lose(ctx, l)
		if err == nil || ev != (core.LossEvent{}) {
			t.Fatalf("Lose of a served report = %+v, %v, want the zero event over the refused append: the takeover does not clear a credit its reader already took, and the take proceeds so the record the claim owes is still appended", ev, err)
		}
		if got := reopen.Stats().Losses; len(got) != 0 {
			t.Fatalf("the reopen's ring = %+v, want none: a served report keeps its suppression while the owed record is appended", got)
		}
	})

	t.Run("a stopped instance's second Lose of its own piece is dropped", func(t *testing.T) {
		e := newEnv(t)
		e.shutdown()
		e.d.Stop()
		l := loss(14)
		if _, err := e.d.Lose(ctx, l); err == nil {
			t.Fatalf("the dead instance's Lose over closed streams: succeeded, want the refused append")
		}
		// The drain of a stopped instance can hand it a second job of the
		// same piece: its own claim bars the retake — the ring held no
		// reader, so the dead-unserved rule alone would let it take its own
		// claim over — and the second Lose answers the dedup's zero event,
		// never a second event into the ring nobody reads.
		again, rerr := e.d.Lose(ctx, l)
		if rerr != nil || again != (core.LossEvent{}) {
			t.Fatalf("the stopped instance's second Lose of its own piece = %+v, %v, want the zero event and nil: its own claim bars it, never a takeover of itself", again, rerr)
		}
		if got := e.d.Stats().Losses; len(got) != 1 {
			t.Fatalf("the stopped instance's ring = %+v, want its one event: the second Lose published nothing", got)
		}
	})
}

// TestAFailureAReporterOnlyTheRingCouldCarryRepublishesOnceClosed holds the
// failed list's word: a failing loss whose report reached nobody but the
// ring — a corrupt record's loss record refused against a closed device —
// keeps the failing instance and its event, so the report is honoured while
// either lives and the loss re-published once neither does: the open that
// re-appends the record the failure owed publishes it itself, through the
// failed leg's failed-window reason the planner carries. A report a reader
// took away keeps its suppression however the instance ended.
func TestAFailureAReporterOnlyTheRingCouldCarryRepublishesOnceClosed(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	// The storage closed while the instance is still live, as a crash
	// beneath the journal: the Lose is refused back — not by the
	// read-only refusal, which tells its writer — and its event goes
	// into a ring nobody reads.
	e.shutdown()
	l := loss(31)
	if _, err := e.d.Lose(ctx, l); err == nil {
		t.Fatalf("the live instance's Lose over closed storage: succeeded, want the refused append")
	}
	e.d.Stop()

	// The word the reopen's planner reads: the report reached nothing.
	fs, err := durability.FailedFor(e.dir, core.JournalID{})
	if err != nil || len(fs) != 1 || fs[0].Seq != l.Seq {
		t.Fatalf("FailedFor = %+v, %v, want the one refused record's entry", fs, err)
	}
	if fs[0].Reported {
		t.Fatalf("the stopped instance's unread report still credited its failure: the closed ring answered for nothing")
	}
	reopen := e.newDurability()
	// The failed leg's plan: reported the word says not, so the Lose
	// publishes the loss instead of holding it back. The reason is the
	// planner's own row — the failed-window reason the failed leg carries,
	// not the corrupt reason the original drop took.
	failleg := loss(31)
	failleg.Reason = core.LossFailedWindow
	ev, err := reopen.Lose(ctx, failleg)
	if err == nil {
		t.Fatalf("the reopen's append over closed storage: succeeded, want the refused append")
	}
	if ev == (core.LossEvent{}) {
		t.Fatalf("the re-appended record's loss stayed suppressed: a failure only an unread ring carried is re-published")
	}
	if got := reopen.Stats().Losses; len(got) != 1 || got[0].Reason != core.LossFailedWindow {
		t.Fatalf("the reopen's ring = %+v, want the one re-published loss of the failed leg's own reason", got)
	}

	// A report a reader reached keeps its suppression: the same failure,
	// read out of the ring before the close, holds the re-append back.
	// buildEnv, not newEnv: one watch a test file, and this one already
	// started its own.
	e2 := buildEnv(t)
	e2.shutdown()
	l2 := loss(32)
	if _, err := e2.d.Lose(ctx, l2); err == nil {
		t.Fatalf("the live instance's Lose over closed storage: succeeded, want the refused append")
	}
	if got := e2.d.Losses(); len(got) != 1 {
		t.Fatalf("the instance's ring = %+v, want the one refused record's event", got)
	}
	e2.d.Stop()
	fs2, err := durability.FailedFor(e2.dir, core.JournalID{})
	if err != nil || len(fs2) != 1 || !fs2[0].Reported {
		t.Fatalf("FailedFor = %+v, %v, want the entry of a report a reader took away", fs2, err)
	}
	reopen2 := e2.newDurability()
	// The plan the word drives: reported, so the re-append holds the
	// event back.
	l2.Reported = true
	ev2, err2 := reopen2.Lose(ctx, l2)
	if err2 == nil || ev2 != (core.LossEvent{}) {
		t.Fatalf("Lose of a served report = %+v, %v, want the zero event over the refused append: the reached report keeps its event suppressed while the owed record is still re-appended", ev2, err2)
	}
}

// TestALossNobodyReadIsStillFoundAtOpenAtTheThirdReopen pins the placed
// claim's retirement at Reading A's third reopen: instance A loses a piece
// and no reader takes its event, reopen B places the record found at open
// and no reader takes B's event either, and the third reopen places the
// same record again — a Place's claim retires with its instance unread as a
// Lose's does, so a loss nobody had read never leaves the ring. A piece
// whose placement a reader did reach keeps its suppression at every later
// open, which is the one claim that never dies.
func TestALossNobodyReadIsStillFoundAtOpenAtTheThirdReopen(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()

	// A: two pieces lost unread, their events into A's own ring.
	unread := loss(11)
	evUnread, err := e.d.Lose(ctx, unread)
	if err != nil {
		t.Fatalf("the first instance's Lose of the unread piece: %v", err)
	}
	if evUnread == (core.LossEvent{}) {
		t.Fatalf("the unread piece's Lose published no event: %+v", evUnread)
	}
	read := loss(12)
	evRead, err := e.d.Lose(ctx, read)
	if err != nil {
		t.Fatalf("the first instance's Lose of the read piece: %v", err)
	}
	if evRead == (core.LossEvent{}) {
		t.Fatalf("the read piece's Lose published no event: %+v", evRead)
	}
	// A stops with no reader having reached either event.
	e.d.Stop()

	// B, the first reopen: each record the open finds on disk is placed
	// found at open, into B's own ring. The read placement's event is
	// carried away by a genuine reader — the drain this test stands in for
	// — and then the other piece places, its event landing past the read.
	B := e.newDurability()
	B.Place(evRead, read.Seq)
	if got := B.Losses(); len(got) != 1 || !got[0].AtOpen {
		t.Fatalf("the first reopen's ring after placing the read piece = %+v, want its one found-at-open event", got)
	}
	B.Place(evUnread, unread.Seq)
	if got := B.Stats().Losses; len(got) != 2 {
		t.Fatalf("the first reopen's ring = %+v, want its two found-at-open events", got)
	}
	// B stops: the read placement's event was answered, the unread one's
	// was not.
	B.Stop()

	// C, the third reopen: the unread piece's record is placed found at
	// open again — B's claim on it retired with B, for no reader reached
	// its event — while the piece whose placement was read stays
	// suppressed: its claim is the one that never dies.
	C := e.newDurability()
	C.Place(evUnread, unread.Seq)
	C.Place(evRead, read.Seq)
	got := C.Stats().Losses
	if len(got) != 1 || !got[0].AtOpen || got[0].Extent != unread.Extent {
		t.Fatalf("the third reopen's ring = %+v, want the unread piece's record alone, still found at open", got)
	}
	if lh, _ := e.lossRecords(); len(lh) != 2 {
		t.Fatalf("%d loss records on disk across three reopens, want the two", len(lh))
	}
}

// TestNamedAnswersThePieceOnlyThroughADeliveredReport pins named's ceiling:
// the answer the open's pass reads keeps a record the disk already names
// through a claim that still stands: its taking instance alive, a reader
// reached its event, or the claim placed over a record a Place found on
// disk, which answers however unread its placement went. A claim that is
// none of those — stopped before any reader took its event — retires, so
// named answers
// false and the pass plans the record again: the directory gains a second
// loss record for the piece, the report the dead claim held never having
// been delivered. Both records name one piece and the take dedup releases
// it once, so the later open reports the loss found at open exactly once —
// the residual of a reported loss nobody read, known and kept rather than
// hidden.
func TestNamedAnswersThePieceOnlyThroughADeliveredReport(t *testing.T) {
	ctx := context.Background()

	// A read-away claim: the loss record landed, a genuine reader carried
	// its event, and the answer the given name holds.
	e := newEnv(t)
	served := loss(15)
	if _, err := e.d.Lose(ctx, served); err != nil {
		t.Fatalf("the served piece's Lose: %v", err)
	}
	if got := e.d.Losses(); len(got) != 1 {
		t.Fatalf("the ring = %+v, want the one served piece's event", got)
	}
	e.d.Stop()
	if !e.d.Named(served.Seq, served.Extent) {
		t.Fatalf("Named of the served piece = false, want true: a reader carried its report")
	}

	// The unread ceiling: the same loss through another instance whose ring
	// no reader reaches, so its claim retires and the pass plans the record
	// the directory already names again.
	another := buildEnv(t)
	unread := loss(16)
	if _, err := another.d.Lose(ctx, unread); err != nil {
		t.Fatalf("the unread piece's Lose: %v", err)
	}
	another.d.Stop()
	if another.d.Named(unread.Seq, unread.Extent) {
		t.Fatalf("Named of the unread piece = true, want false: its claim retired with the instance whose ring answered nobody, so the pass plans the record the directory already names again")
	}
}

// TestAnOverwrittenEventAGenuineReadNeverCarriedStaysUnserved pins the
// served window's clamp: the ring keeps the newest core.LossRingSize events,
// and a genuine read answers for the events its copy carried alone. The
// events the ring had overwritten before the read — a poller that knows it
// missed them has still not read them — stay answered by nobody, so their
// claims survive their instance and the reopen places the losses they name
// found at open again, while every event the copy carried stays suppressed.
func TestAnOverwrittenEventAGenuineReadNeverCarriedStaysUnserved(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()

	// Four more losses than the ring holds, all unread, each its own piece.
	n := core.LossRingSize + 4
	seqs := make([]core.Seq, n)
	events := make([]core.LossEvent, n)
	for i := range events {
		seqs[i] = core.Seq(100 + i)
		ev, err := e.d.Lose(ctx, loss(seqs[i]))
		if err != nil {
			t.Fatalf("Lose of the piece at %d: %v", i, err)
		}
		if ev == (core.LossEvent{}) {
			t.Fatalf("the piece at %d published no event", i)
		}
		events[i] = ev
	}
	if got := e.d.Stats().Losses; len(got) != core.LossRingSize {
		t.Fatalf("the ring holds %d events, want its %d newest", len(got), core.LossRingSize)
	}
	// One genuine read, the drain's: its copy carries only the newest
	// LossRingSize events, never the first four.
	if got := e.d.Losses(); len(got) != core.LossRingSize {
		t.Fatalf("the genuine read carried %d events, want the %d newest", len(got), core.LossRingSize)
	}
	e.d.Stop()

	// The reopen reads what the disk holds and places each record the
	// claims let it. The four overwritten pieces' claims retired with their
	// unread events; the two hundred fifty-six the read carried stay held.
	reopen := e.newDurability()
	for i, ev := range events {
		reopen.Place(ev, seqs[i])
	}
	var placed []core.LossEvent
	for _, ev := range reopen.Stats().Losses {
		if !ev.AtOpen {
			t.Fatalf("the reopen published %+v, not marked found at open", ev)
		}
		placed = append(placed, ev)
	}
	if len(placed) != 4 {
		t.Fatalf("the reopen published %d found-at-open events, want the 4 overwritten ones: %+v", len(placed), placed)
	}
	for _, ev := range placed {
		if ev.Version.Counter < 200 || ev.Version.Counter >= 204 {
			t.Fatalf("the reopen published %+v, want only one of the four overwritten pieces", ev)
		}
	}
}
