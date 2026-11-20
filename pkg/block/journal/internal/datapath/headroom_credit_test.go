package datapath

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/capacity"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/segment"
)

// drawnAndRetired exhausts the headroom of a core at its limit with removals
// of a file it does not hold, seals the drain's stream early and retires,
// through the core's own executor, every sealed segment the index names the
// drained file in — the segments the draws landed in. It returns the
// headroom's unused bytes before the drain and the segments retired.
func drawnAndRetired(t *testing.T, c *Core, s *Share) (full int64, retired []core.SegmentID) {
	t.Helper()
	full = c.Stats().HeadroomBytes
	if full <= 0 {
		t.Fatalf("HeadroomBytes = %d at the limit before any draw, want the drawing side", full)
	}
	drainHeadroom(t, c, s)
	drain := testFile(9)
	_, err := c.Deallocate(context.Background(), s, drain, core.Extent{Off: 1 << 30, Len: 1})
	_ = refused(t, err, core.LimitHeadroom)
	if got := c.Stats().HeadroomBytes; got != 0 {
		t.Fatalf("HeadroomBytes = %d at the exhausted headroom, want 0", got)
	}

	if _, err := c.Streams().For(drain).Seal(true); err != nil {
		t.Fatalf("seal the drain's stream: %v", err)
	}
	for _, row := range c.set.Snapshot() {
		if row.State != segment.Sealed || !slices.Contains(c.index.FilesOfSegment(row.ID), drain) {
			continue
		}
		if _, err := c.Retire(row.ID, segment.NotHeld); err != nil {
			t.Fatalf("Retire(%d): %v", row.ID, err)
		}
		retired = append(retired, row.ID)
	}
	if len(retired) == 0 {
		t.Fatal("no sealed segment held the drain's records")
	}
	return full, retired
}

// refill writes content through s until the journal refuses it at its limit,
// naming the journal or, below half the headroom, the headroom.
func refill(t *testing.T, c *Core, s *Share) {
	t.Helper()
	p := make([]byte, 4096)
	for off := int64(0); ; off += int64(len(p)) {
		if off > 64*testSegmentCap {
			t.Fatalf("%d bytes written after the retirements were never refused", off)
		}
		err := retryRefused(func() error {
			_, err := c.WriteAt(context.Background(), s, testFile(3), off, p, 1)
			return err
		}, core.LimitSpare)
		var ns *core.NoSpaceError
		if errors.As(err, &ns) && (ns.Limit == core.LimitJournal || ns.Limit == core.LimitHeadroom) {
			return
		}
		if err != nil {
			t.Fatalf("refill at %d: %v", off, err)
		}
	}
}

// TestRetirementCreditsTheHeadroom checks, through Stats and the public
// removals, that a journal at its limit whose headroom its removals
// exhausted is not wedged once the segments holding those records are
// retired: HeadroomBytes is whole again, and both a removal and a clone
// target, which the exhausted and the below-half headroom refused, are
// admitted.
func TestRetirementCreditsTheHeadroom(t *testing.T) {
	c, _, s, _ := atTheLimit(t)
	full, _ := drawnAndRetired(t, c, s)

	if got := c.Stats().HeadroomBytes; got != full {
		t.Fatalf("HeadroomBytes = %d once the drawn records' segments were retired, want %d, the headroom whole", got, full)
	}
	// The retirements freed storage, so the journal is filled to its limit
	// again: there an uncredited headroom refuses both calls below.
	refill(t, c, s)
	if _, err := c.CloneTarget(context.Background(), s, testFile(2), core.CloneSpec{DstOff: 0, Len: 8}); err != nil {
		t.Fatalf("a clone target once the headroom was credited = %v, want it admitted", err)
	}
	if _, err := c.Deallocate(context.Background(), s, testFile(9), core.Extent{Off: 1 << 31, Len: 1}); err != nil {
		t.Fatalf("a removal once the headroom was credited = %v, want it admitted", err)
	}
}

// TestRetirementCreditsTheHeadroomOnce checks a replayed retirement credits
// nothing more: a second Retire of a retired segment is refused and the
// retired hook delivered again moves nothing, so HeadroomBytes stays at what
// the first retirement returned, the headroom's size and no more.
func TestRetirementCreditsTheHeadroomOnce(t *testing.T) {
	c, _, s, _ := atTheLimit(t)
	full, retired := drawnAndRetired(t, c, s)

	// Draw one record again at the limit, so a second credit of the first
	// segment would show as the figure rising past what the draw left.
	refill(t, c, s)
	if _, err := c.Deallocate(context.Background(), s, testFile(9), core.Extent{Off: 1 << 31, Len: 1}); err != nil {
		t.Fatalf("a removal once the headroom was credited: %v", err)
	}
	want := full - capacity.RecordCharge
	for _, seg := range retired {
		if _, err := c.Retire(seg, segment.NotHeld); !errors.Is(err, segment.ErrNotInSet) {
			t.Fatalf("a second Retire(%d) = %v, want ErrNotInSet", seg, err)
		}
		c.retiredSegment(seg)
	}
	if got := c.Stats().HeadroomBytes; got != want {
		t.Fatalf("HeadroomBytes = %d after the retirements were replayed, want %d: one draw since the credit, and no second credit", got, want)
	}
}
