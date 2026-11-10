package segment_test

import (
	"testing"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/segment"
)

// TestPoolReadyContent checks the pool's read of the content spares an
// unreserved take can still reach: the ready ones less the spares owed to open
// holds, zero when nothing is ready, and unmoved by a refused Hold, which
// holds nothing it was refused.
func TestPoolReadyContent(t *testing.T) {
	// The journal's maximum holds exactly the two content spares the target
	// asks, so the counts below stand on the holds alone, never on a race
	// with the worker's makes.
	_, _, _, p := poolOf(t, segment.Targets{Content: 2}, 2*segCap, 0, nil)
	p.Start()
	awaitCounters(t, "the two content spares ready", p,
		func(c segment.PoolCounters) bool { return c.Spares == 2 })
	defer p.Stop()

	if got := p.ReadyContent(); got != 2 {
		t.Fatalf("a pool with two ready spares reports ReadyContent = %d, want 2", got)
	}

	// A hold of one owes a spare: what an unreserved take can reach is the
	// one left.
	h, err := p.Hold(1)
	if err != nil {
		t.Fatalf("Hold(1) over two ready spares: %v", err)
	}
	if got := p.ReadyContent(); got != 1 {
		t.Fatalf("after Hold(1) over two ready spares, ReadyContent = %d, want 1", got)
	}
	h.Close()

	// A hold for both spares leaves nothing an unreserved take can reach,
	// while two still sit ready.
	h2, err := p.Hold(2)
	if err != nil {
		t.Fatalf("Hold(2) over two ready spares: %v", err)
	}
	if got := p.ReadyContent(); got != 0 {
		t.Fatalf("after Hold(2) over two ready spares, ReadyContent = %d, want 0", got)
	}
	h2.Close()

	// A refused Hold holds nothing: the pool still owes none, and the two
	// spares stand ready as before. The maximum holds exactly the two, so a
	// hold of one more than the class holds is refused by the pool, with the
	// spares it fell short by named.
	_, err = p.Hold(3)
	if err == nil {
		t.Fatal("a Hold of 3 over 2 ready spares was not refused")
	}
	assertSpareRefusal(t, "the hold of 3 over 2 ready spares", err, 1)
	if got := p.ReadyContent(); got != 2 {
		t.Fatalf("after the refused Hold, ReadyContent = %d, want the 2 it stood at", got)
	}
	if c := p.Counters(); c.Held != 0 {
		t.Fatalf("after the refused Hold the pool owes %d spares, want none: a refusal changes nothing", c.Held)
	}
}
