package durability_test

import (
	"context"
	"testing"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
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
		// A reader reaches the stopped instance's ring — its Stats still
		// stands in this process — and carries the report away: the reach,
		// not the moment of it, is what the claim asks.
		if got := e.d.Stats().Losses; len(got) != 1 {
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
