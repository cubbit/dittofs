package datapath

import (
	"context"
	"errors"
	"testing"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/durability"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/index"
)

func TestNew_WiresDurabilityOverTheSameIndex(t *testing.T) {
	c, _ := newTestCore(t)
	f := testFile(1)
	publish(t, c, f, 0, "aaaa")
	syncFiles(t, c, f)
	v2 := publish(t, c, f, 0, "bbbb")
	if got := c.Index().Retained(f); len(got) != 1 || got[0].By != v2 {
		t.Fatalf("before the sync the retained pieces are %+v, want one superseded by v2", got)
	}
	syncFiles(t, c, f)
	if got := c.Index().Retained(f); len(got) != 0 {
		t.Fatalf("the sync did not lift the superseded piece from the core's index: %+v", got)
	}
}

func TestAccount_AppliesHeldDelta(t *testing.T) {
	c, _ := newTestCore(t)
	c.Account(index.HeldDelta{HeldBytes: 10, DirtyBytes: 10})
	if h, d, r := c.heldBytes(); h != 10 || d != 10 || r != 0 {
		t.Fatalf("counters = %d/%d/%d, want 10/10/0", h, d, r)
	}
	c.Account(index.HeldDelta{HeldBytes: -4, DirtyBytes: -4, RetainedBytes: 3})
	if h, d, r := c.heldBytes(); h != 6 || d != 6 || r != 3 {
		t.Fatalf("counters = %d/%d/%d, want 6/6/3", h, d, r)
	}
	c.Account(index.HeldDelta{})
	if h, d, r := c.heldBytes(); h != 6 || d != 6 || r != 3 {
		t.Fatalf("a zero delta moved the counters: %d/%d/%d", h, d, r)
	}
}

func TestAccount_ReachedFromDurabilityLift(t *testing.T) {
	c, _ := newTestCore(t)
	f := testFile(2)
	publish(t, c, f, 0, "aaaa")
	syncFiles(t, c, f)
	publish(t, c, f, 0, "bbbb")
	if _, _, r := c.heldBytes(); r != 4 {
		t.Fatalf("retained = %d after the overwrite, want 4", r)
	}
	syncFiles(t, c, f)
	if h, _, r := c.heldBytes(); r != 0 || h != 4 {
		t.Fatalf("after the lift held=%d retained=%d, want 4 and 0", h, r)
	}
}

func TestAddShare_ReturnsItsTag(t *testing.T) {
	c, _ := newTestCore(t)
	if s := c.AddShare(7); s.Tag != 7 {
		t.Fatalf("Tag = %d, want 7", s.Tag)
	}
}

// A synced loss through the durability layer drops the held bytes from the
// core's counters once: the layer accounts the delta its marker returns, and
// the core adds nothing of its own. A marker the index refuses surfaces to the
// caller of Lose.
func TestLose_AccountsTheMarkersDeltaOnceAndSurfacesItsError(t *testing.T) {
	c, _ := newTestCore(t)
	f := testFile(3)
	publish(t, c, f, 0, "aaaa")
	h0, d0, r0 := c.heldBytes()
	if h0 != 4 || d0 != 4 {
		t.Fatalf("setup counters = %d/%d/%d, want 4/4/_", h0, d0, r0)
	}
	if _, err := c.Durability().Lose(context.Background(), durability.Loss{
		Share: 1, File: f, Extent: core.Extent{Off: 0, Len: 4}, Version: core.Version{Counter: 1 << 20},
		Seq: 1000, Synced: true, Deferred: true, Reason: core.LossStale,
	}); err != nil {
		t.Fatalf("Lose: %v", err)
	}
	if h, d, r := c.heldBytes(); h != 0 || d != 0 || r != r0 {
		t.Fatalf("after the loss counters = %d/%d/%d, want 0/0/%d", h, d, r, r0)
	}
	if _, err := c.Durability().Lose(context.Background(), durability.Loss{
		Share: 1, File: f, Extent: core.Extent{Off: 0, Len: 0}, Version: core.Version{Counter: 1 << 21},
		Seq: 1001, Synced: true, Deferred: true, Reason: core.LossStale,
	}); !errors.Is(err, core.ErrZeroLength) {
		t.Fatalf("Lose of a zero extent = %v, want ErrZeroLength", err)
	}
}

// TestNew_UsesTheStatesCounters fails if the layers were built over a second
// Counters: a loss through the durability layer draws its record's sequence
// number from the counters it was configured with, so the state's own must
// have moved.
func TestNew_UsesTheStatesCounters(t *testing.T) {
	counters := &index.Counters{}
	if err := counters.SetVersionLimit(core.Version{Counter: 1 << 40}); err != nil {
		t.Fatalf("SetVersionLimit: %v", err)
	}
	c, _ := newTestCore(t, func(o *coreOptions) { o.state = State{Counters: counters} })
	if c.counters != counters {
		t.Fatal("the core did not adopt the state's counters")
	}
	f := testFile(4)
	publish(t, c, f, 0, "aaaa") // draws seq 1 from the same counters
	if _, err := c.Durability().Lose(context.Background(), durability.Loss{
		Share: 1, File: f, Extent: core.Extent{Off: 0, Len: 4}, Version: core.Version{Counter: 1}, Seq: 1, Synced: true, Reason: core.LossStale,
	}); err != nil {
		t.Fatalf("Lose: %v", err)
	}
	if seq, err := counters.NextSeq(); err != nil || seq < 3 {
		t.Fatalf("NextSeq = %d, %v; want at least 3: the loss record drew its own from the state's counters", seq, err)
	}
}
