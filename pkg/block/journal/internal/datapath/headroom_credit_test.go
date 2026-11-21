package datapath

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/capacity"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/durability"
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

// loseOn raises a corrupt loss of file f through the core's durability layer,
// at its own extent so repeated losses never share a piece, and returns the
// segment the record landed in, read as the stream's open segment right
// after the append.
func loseOn(t *testing.T, c *Core, s *Share, f core.FileID, off int64, atOpen bool) core.SegmentID {
	t.Helper()
	_, err := c.Durability().Lose(context.Background(), durability.Loss{
		Share:   s.Tag,
		File:    f,
		Extent:  core.Extent{Off: off, Len: 4096},
		Version: core.Version{Counter: 7},
		Seq:     core.Seq(uint64(off/4096) + 1),
		Reason:  core.LossCorrupt,
		AtOpen:  atOpen,
	})
	if err != nil {
		t.Fatalf("Lose(atOpen=%v) at the limit: %v", atOpen, err)
	}
	seg, ok := c.Streams().For(f).OpenSegment()
	if !ok {
		t.Fatal("the loss record's stream holds no open segment after the append")
	}
	return seg
}

// sealAndRetire seals f's stream early and retires seg through the core's own
// executor, the path a repack's retirement takes.
func sealAndRetire(t *testing.T, c *Core, f core.FileID, seg core.SegmentID) {
	t.Helper()
	if _, err := c.Streams().For(f).Seal(true); err != nil {
		t.Fatalf("seal the loss record's stream: %v", err)
	}
	if _, err := c.Retire(seg, segment.NotHeld); err != nil {
		t.Fatalf("Retire(%d): %v", seg, err)
	}
}

// TestRetirementCreditsALossRecordsDraw checks the loss path's share of the
// headroom credit: a loss record a running journal appends at its limit draws
// on the headroom, and retiring the segment it landed in gives that draw
// back, so HeadroomBytes is whole again. A loss record an open appends on the
// open reserve, once the drawing side is spent, is credited back when its
// segment retires, to the open reserve first (RFC 1 section 6).
func TestRetirementCreditsALossRecordsDraw(t *testing.T) {
	t.Run("drawing side", func(t *testing.T) {
		c, _, s, _ := atTheLimit(t)
		full := c.Stats().HeadroomBytes
		lost := testFile(5)
		seg := loseOn(t, c, s, lost, 0, false)
		if got := c.Stats().HeadroomBytes; got != full-capacity.RecordCharge {
			t.Fatalf("HeadroomBytes = %d after a runtime loss at the limit, want %d: the loss record drew one charge", got, full-capacity.RecordCharge)
		}
		sealAndRetire(t, c, lost, seg)
		if got := c.Stats().HeadroomBytes; got != full {
			t.Fatalf("HeadroomBytes = %d once the loss record's segment %d retired, want %d, the headroom whole", got, seg, full)
		}
	})
	t.Run("open reserve", func(t *testing.T) {
		// Synthetic: an AtOpen loss on a running core whose headroom was
		// drained; recovery runs on an empty ledger, so production reaches
		// the reserve only when an open's loss meets a ledger already spent.
		c, _, s, _ := atTheLimit(t)
		drainHeadroom(t, c, s)
		lost := testFile(5)
		// The loss lands in a segment of its own: the stream is sealed first,
		// so the retirement below takes no removal record with it.
		if _, err := c.Streams().For(lost).Seal(true); err != nil {
			t.Fatalf("seal the loss file's stream: %v", err)
		}
		spent := c.headroom.Spent()
		seg := loseOn(t, c, s, lost, 0, true)
		if got := c.headroom.Spent(); got != spent+capacity.RecordCharge {
			t.Fatalf("Spent = %d after an open's loss on the reserve, want %d", got, spent+capacity.RecordCharge)
		}
		sealAndRetire(t, c, lost, seg)
		if got := c.headroom.Spent(); got != spent {
			t.Fatalf("Spent = %d once the reserve loss's segment %d retired, want %d: the reserve draw credited back to the open reserve", got, seg, spent)
		}
		// The credit refilled the open reserve, not the drawing side, which
		// the drain left spent.
		if got := c.Stats().HeadroomBytes; got != 0 {
			t.Fatalf("HeadroomBytes = %d once the reserve draw was credited, want 0: the open reserve refills first", got)
		}
	})
}
