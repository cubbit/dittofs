package durability

import (
	"context"
	"slices"
	"syscall"
	"testing"

	"github.com/cubbit/dittofs/internal/prof"
	"github.com/cubbit/dittofs/pkg/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/journal/internal/format"
	"github.com/cubbit/dittofs/pkg/journal/journaltest/kit"
)

// TestANewIdentityAtAUsedPathOwnsItsLossState holds the empty-and-Init
// scenario: this process can make a new journal at a path it already used,
// and the new journal's identity re-reaches the sequence numbers the dead
// one lost, so every piece of per-process loss state — the failed list, the
// taken pieces, the generation — is scoped to the identity with the path,
// never the path alone. The new identity's FailedFor answers nothing, its
// Place publishes a piece the dead journal took, and its generation starts
// at zero.
func TestANewIdentityAtAUsedPathOwnsItsLossState(t *testing.T) {
	prof.Watch(t, prof.Options{})
	dir := t.TempDir()

	// Journal A: a read-only window fails the loss records of two appends,
	// so their sequence numbers join A's failed list, the pieces are taken
	// and the window's raise moves A's generation.
	rA, _ := newRORig(t, 1, dir)
	f := file(1)
	rA.publish(t, f, 0, "aaaa")
	seqW := core.Seq(rA.lastSeq.Load())
	_, seqH := rA.appendHeader(t, f, format.KindTruncate, format.RecordBody{})
	rA.d.Enter(syscall.EROFS)
	_ = mustReturn(t, "Sync", rA.syncAsync(context.Background(), f))

	got, err := FailedFor(dir, rigIdentity)
	if err != nil {
		t.Fatalf("FailedFor of A: %v", err)
	}
	has := func(seq core.Seq) bool {
		return slices.ContainsFunc(got, func(f Failed) bool { return f.Seq == seq })
	}
	if len(got) != 2 || !has(seqW) || !has(seqH) {
		t.Fatalf("the precondition failed: A's FailedFor = %v, want the two seqs %d and %d", got, seqW, seqH)
	}
	if g := rA.d.Stats().LossGeneration; g != 1 {
		t.Fatalf("the precondition failed: A's generation = %d, want 1: the failed window raises", g)
	}

	// Journal B: a new identity Init'd at the same directory, in this same
	// process, the way a re-created journal's Open builds its layer.
	otherID := core.JournalID{0x1, 0x2, 0x3}
	c := testConfig(t, kit.NewStopwatchClock(), &testDomains{})
	c.Dir = dir
	c.Identity = otherID
	dB, err := New(c)
	if err != nil {
		t.Fatalf("New of the new identity: %v", err)
	}
	t.Cleanup(dB.Stop)

	// The new identity's failed list answers nothing: A's numbers belong to
	// A's identity, and B's records at the same sequence numbers are B's
	// acknowledged data.
	if got, err := FailedFor(dir, otherID); err != nil || len(got) != 0 {
		t.Errorf("the new identity's FailedFor = %v, %v, want none: the failed list belongs to the dead journal's identity", got, err)
	}

	// A piece the dead journal took: B's Place publishes it, for the dedup
	// that answered it taken was A's, and nothing B does asks A.
	evB := core.LossEvent{File: f, Reason: core.LossCorrupt, Extent: core.Extent{Off: 0, Len: 4}}
	dB.Place(evB, seqW)
	if ring := dB.Stats().Losses; len(ring) != 1 {
		t.Errorf("the new identity's ring after placing A's taken piece = %+v, want the one event: the piece dedup of the dead journal suppresses nothing here", ring)
	}

	// The new identity's generation starts at zero: the dead journal's
	// raise answers to its identity, not to the path.
	if g := dB.Generation(); g != 0 {
		t.Errorf("the new identity's generation = %d, want 0: A's raise does not inherit across identities", g)
	}
}
