package durability_test

import (
	"context"
	"testing"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
)

// TestTheLossRingAndItsDedupLivePerDirectoryAcrossReopens catches a reopen
// in one process re-reporting a loss found on disk: the ring, the event count
// and the (dropped seq, extent) pieces a Lose or a Place has taken live per
// directory, so a Durability built later on the directory shares them rather
// than starting a ring of its own.
func TestTheLossRingAndItsDedupLivePerDirectoryAcrossReopens(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()

	// A loss record found at open, placed through the first open's layer.
	first := core.LossEvent{File: file(2), Reason: core.LossCorrupt, Extent: core.Extent{Off: 0, Len: 3}}
	e.d.Place(first, 9)
	if got := e.d.Stats().Losses; len(got) != 1 || got[0].SeqNum != 1 {
		t.Fatalf("the first instance's ring after one Place = %+v, want the one event numbered 1", got)
	}

	// A reopen: the second Durability on the same directory, bound to the same
	// streams, the way a close and open in one process builds one.
	reopen := e.newDurability()

	// The same piece placed through the reopen publishes nothing, and a new
	// piece placed through it continues the first instance's numbering.
	reopen.Place(first, 9)
	next := core.LossEvent{File: file(2), Reason: core.LossCorrupt, Extent: core.Extent{Off: 8, Len: 3}}
	reopen.Place(next, 10)
	if got := e.d.Stats().Losses; len(got) != 2 || got[1].SeqNum != 2 || got[1].Extent != next.Extent {
		t.Fatalf("the first instance's ring = %+v, want its own placed event and then the reopen's new piece, numbered 2", got)
	}
	if got := reopen.Stats().Losses; len(got) != 2 || got[1].SeqNum != 2 || got[1].Extent != next.Extent {
		t.Fatalf("the reopen's ring = %+v, want the same two events a later reader could poll from either: the second one is the new piece, numbered 2", got)
	}

	// A piece Lost through the first instance publishes nothing through the
	// reopen: one loss record, one event beyond the two placed, one raise.
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
	if got := e.d.Stats().Losses; len(got) != 3 || got[2].SeqNum != 3 {
		t.Errorf("the ring after the reopen's refused duplicate = %+v, want 3 events numbered up to 3", got)
	}
	if got := reopen.Stats().Losses; len(got) != 3 {
		t.Errorf("the reopen's ring holds %d events, want 3", len(got))
	}

	// Place appended nothing, raised nothing and counted nothing, and the
	// generation the Lose raised is the directory's, through either instance.
	if got := e.d.Generation(); got != 1 {
		t.Errorf("the first instance's generation = %d, want 1: raised once by the one Lose, never by a Place", got)
	}
	if got := reopen.Generation(); got != 1 {
		t.Errorf("the reopen's generation = %d, want the directory's 1", got)
	}

	// Another directory shares none of it: the same piece placed there is new,
	// numbered from 1, and raises nothing.
	other := buildEnv(t)
	other.d.Place(first, 9)
	p := other.d.Stats()
	if len(p.Losses) != 1 || p.Losses[0].SeqNum != 1 {
		t.Errorf("the other directory's ring = %+v, want the piece published anew and numbered 1", p.Losses)
	}
	if p.LossGeneration != 0 {
		t.Errorf("the other directory's generation = %d, want 0", p.LossGeneration)
	}
	if got := e.d.Stats().Losses; len(got) != 3 {
		t.Errorf("the first directory's ring changed to %d events after a Place at another directory, want 3", len(got))
	}
}
