package durability_test

import (
	"context"
	"testing"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/durability"
)

// TestDeferredLossSurvivesInProcessReopen pins the registry-level home of the
// deferred losses: a loss one instance of the journal deferred survives the
// close-and-open the same process makes of the directory, reaching the copy
// that retires its segment through the fresh instance — and, before any
// instance is built, through the per-directory reader that answers the same
// registry the failed list reads. The piece the deferral took is the
// directory's own, so a reopen defers it no second time. The leg the registry
// cannot cover — the copy writing the deferred record into the segment it
// copies — rides the emptying, which is not built yet.
func TestDeferredLossSurvivesInProcessReopen(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()

	l := loss(5)
	l.Synced = true
	l.Deferred = true
	l.Seg = 2
	if _, err := e.d.Lose(ctx, l); err != nil {
		t.Fatalf("the deferred Lose: %v", err)
	}
	if got := e.d.DeferredAgainst(2); len(got) != 1 || got[0] != l {
		t.Fatalf("the first instance's DeferredAgainst(2) = %+v, want the one deferred loss", got)
	}

	// The per-directory reader answers before any instance is built, keyed
	// like FailedFor on the directory and the journal identity — the zero one
	// here, the identity the harness builds its layers under — and hands back
	// the same entry the instance's DeferredAgainst does.
	before, err := durability.DeferredFor(e.dir, core.JournalID{})
	if err != nil {
		t.Fatalf("DeferredFor: %v", err)
	}
	if got := before[2]; len(got) != 1 || got[0] != l {
		t.Fatalf("DeferredFor answered segment 2 = %+v, want the one deferred loss of the first instance", got)
	}

	// The reopen: a fresh Durability on the same directory, the way a close
	// and open in one process builds one. The deferred losses are the
	// registry's, not the instance's, so the copy that retires segment 2 asks
	// the reopen and finds the entry the dead instance left.
	reopen := e.newDurability()
	if got := reopen.DeferredAgainst(2); len(got) != 1 || got[0] != l {
		t.Fatalf("the reopen's DeferredAgainst(2) = %+v, want the deferred loss of the earlier instance: the registry must keep it across the reopen", got)
	}

	// The piece the deferral took is the directory's: the reopen defers the
	// same loss no second time, for the dedup still remembers it.
	if ev, err := reopen.Lose(ctx, l); err != nil || ev != (core.LossEvent{}) {
		t.Fatalf("Lose of an already-deferred piece through the reopen = %+v, %v, want the zero event and nil: the piece is the directory's own", ev, err)
	}
	if got := reopen.DeferredAgainst(2); len(got) != 1 {
		t.Fatalf("the reopen's DeferredAgainst(2) = %+v, want the one entry: a taken piece defers no second entry", got)
	}

	// The end of the deferral is the copy's own word, heard through the
	// reopen; the entry leaves the registry for whichever instance asks.
	if !reopen.EndDeferral(2, l) {
		t.Fatalf("EndDeferral through the reopen answered false, want the entry the first instance deferred to leave")
	}
	if got := reopen.DeferredAgainst(2); len(got) != 0 {
		t.Fatalf("the reopened DeferredAgainst(2) after EndDeferral = %+v, want none", got)
	}
	if got := e.d.DeferredAgainst(2); len(got) != 0 {
		t.Fatalf("the first instance's DeferredAgainst(2) after EndDeferral = %+v, want none: the registry is one", got)
	}
}
