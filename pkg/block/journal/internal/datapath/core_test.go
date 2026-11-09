package datapath

import (
	"testing"

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
