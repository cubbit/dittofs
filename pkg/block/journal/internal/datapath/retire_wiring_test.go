package datapath

import (
	"context"
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/segment"
)

// TestRetireWiringRetiresEmptiedSegments drives the assembled core's liveness
// wiring on the real seam: a file's records fill a segment past the cap so the
// stream rolls and seals it, the file is then truncated to nothing and synced,
// and the sealed segment — which the removal now holds nothing of — must leave
// the segment set and the journal's footprint on its own. Nothing repacks: the
// core's surface holds no repack pass, and the retirement comes from the
// liveness wiring alone — the removal record's registration, the emptied offer
// it hands the worker, the worker's gate checks and the retirer's unlink.
func TestRetireWiringRetiresEmptiedSegments(t *testing.T) {
	ctx := context.Background()
	c, _ := newTestCore(t)
	s := mustShare(t, c, 1)
	f := testFile(1)

	// A write past the segment cap rolls the stream, so the first segment
	// seals full and holds the write's first records.
	mustWrite(t, c, s, f, 0, make([]byte, 2*testSegmentCap))
	var sealedIDs []core.SegmentID
	var sealedBytes int64
	for _, r := range c.set.Snapshot() {
		if r.State == segment.Sealed {
			sealedIDs = append(sealedIDs, r.ID)
			sealedBytes += r.Size
		}
	}
	if len(sealedIDs) == 0 {
		t.Fatalf("a write of %d bytes sealed no segment", 2*testSegmentCap)
	}

	// The pool back at its target before the truncation, so the retirement
	// unlinks rather than recycles: the pool takes a full sealed segment as
	// a spare while it is below its target, and a recycled segment keeps its
	// id gone but its footprint standing.
	waitIn(t, 5*time.Second, "the pool to refill to its target", func() bool {
		return c.pool.Counters().Spares >= int64(c.contentTarget)
	})
	used := c.set.Counters().UsedBytes

	// Truncate to nothing: the removal's marker cuts the sealed segments'
	// held bytes to zero, and the call returns once its record is durable, so
	// the causality record the worker's gate asks about is on the device.
	if _, err := c.Truncate(ctx, s, f, 0); err != nil {
		t.Fatalf("Truncate(%d, 0): %v", f, err)
	}
	syncFiles(t, c, f)

	gone := func() bool {
		for _, r := range c.set.Snapshot() {
			for _, id := range sealedIDs {
				if r.ID == id {
					return false
				}
			}
		}
		return true
	}
	waitIn(t, 5*time.Second, "the emptied segments to leave the set", gone)
	if got, want := c.set.Counters().UsedBytes, used-sealedBytes; got != want {
		t.Fatalf("UsedBytes after the retirements = %d, want %d (before the truncation %d, sealed bytes freed %d)",
			got, want, used, sealedBytes)
	}
}
