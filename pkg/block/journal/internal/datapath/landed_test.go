package datapath

import (
	"context"
	"errors"
	"testing"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/durability"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/format"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/segment"
	"github.com/cubbit/dittofs/pkg/block/journal/journaltest/kit"
)

// landedRow is one writer of a tagged record outside a write. do performs the
// operation on a core whose tag holds a sealed base segment and a live write
// at 8192 of f, not yet synced; it returns the segment whose retirement the row then runs.
type landedRow struct {
	name string
	tag  core.ShareTag
	do   func(t *testing.T, c *Core, seam *kit.Seam, s *Share, f core.FileID) (retire core.SegmentID, reason segment.Reason)
}

// shareFigureIsTheSets checks the share's tally against the set's settled
// bytes for the tag less the repack copies held out, the figure a retirement's
// subtraction is exact against.
func shareFigureIsTheSets(t *testing.T, c *Core, when string, tag core.ShareTag) {
	t.Helper()
	if got, want := c.ShareFigure(tag), c.set.ShareBytes(tag)-c.CopyHeldBytes(); got != want {
		t.Fatalf("%s: share figure = %d, want the set's settled bytes less the held copies, %d", when, got, want)
	}
}

// TestRecordsLandedOutsideAWriteReachTheShareFigure runs each writer of a
// tagged record that holds no grant — a failed window's re-append, a loss
// record, a removal — on a real core, and checks the share's figure
// equals the set's own settled bytes for the tag both while the record's
// segment stands and after it retires through the core's retirement hook. A
// share whose figure never took the record's bytes drops below the set's when
// the segment retires, so its limit admits past it.
func TestRecordsLandedOutsideAWriteReachTheShareFigure(t *testing.T) {
	boom := errors.New("sync boom")
	const tag core.ShareTag = 7
	rows := []landedRow{
		{"re-append", tag, func(t *testing.T, c *Core, seam *kit.Seam, s *Share, f core.FileID) (core.SegmentID, segment.Reason) {
			seg := recordOf(t, c, f, 8192).Record.Segment
			if err := seam.Schedule(kit.Failing(kit.OpSync, format.SegmentName(seg), 1, boom)); err != nil {
				t.Fatalf("the seam refused the sync fault: %v", err)
			}
			t.Cleanup(func() { _ = seam.Schedule() })
			before := c.set.ShareBytes(tag)
			syncFiles(t, c, f)
			if got := c.set.ShareBytes(tag); got <= before {
				t.Fatalf("the failed window re-appended nothing: the set holds %d bytes of the tag, was %d", got, before)
			}
			return seg, segment.ReasonEnded
		}},
		{"loss record", tag, func(t *testing.T, c *Core, _ *kit.Seam, _ *Share, f core.FileID) (core.SegmentID, segment.Reason) {
			seg := recordOf(t, c, f, 8192).Record.Segment
			if _, err := c.Durability().Lose(context.Background(), durability.Loss{
				Share: tag, File: f, Extent: core.Extent{Off: 8192, Len: 32},
				Version: core.Version{Counter: 1}, Seq: 1 << 30, Reason: core.LossStale,
			}); err != nil {
				t.Fatalf("Lose: %v", err)
			}
			return seg, segment.NotHeld
		}},
		{"removal", tag, func(t *testing.T, c *Core, _ *kit.Seam, s *Share, f core.FileID) (core.SegmentID, segment.Reason) {
			seg := recordOf(t, c, f, 8192).Record.Segment
			// Nothing is held at this offset: a header-only record that frees nothing.
			if _, err := c.Deallocate(context.Background(), s, f, core.Extent{Off: 1 << 20, Len: 1}); err != nil {
				t.Fatalf("Deallocate: %v", err)
			}
			return seg, segment.NotHeld
		}},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			c, seam := newTestCore(t)
			s := mustShare(t, c, row.tag)
			f := testFile(1)
			// The base: a sealed segment of the tag that stays, so no retirement
			// below can clamp the figure at zero and hide an undercount.
			mustWrite(t, c, s, f, 0, make([]byte, 4096))
			syncFiles(t, c, f)
			if _, err := c.Streams().For(f).Seal(true); err != nil {
				t.Fatalf("Seal: %v", err)
			}
			mustWrite(t, c, s, f, 8192, make([]byte, 4096))
			shareFigureIsTheSets(t, c, "after the writes", row.tag)

			seg, reason := row.do(t, c, seam, s, f)
			shareFigureIsTheSets(t, c, "with the record's segment standing", row.tag)

			// The segment holding the record retires through the hook the
			// core's own executor runs.
			if _, err := c.Streams().For(f).Seal(true); err != nil && !errors.Is(err, segment.ErrSegmentEnded) {
				t.Logf("Seal: %v", err)
			}
			re := segment.NewRetirer(segment.RetirerConfig{
				Set: c.set, Cache: c.cache,
				Maker:   segment.NewMaker(seam.FS(), c.set, unboundedBytes, 0, nil),
				FS:      seam.FS(),
				Retired: c.retiredSegment,
			})
			if _, err := re.Retire(seg, reason); err != nil {
				t.Fatalf("Retire(%d): %v", seg, err)
			}
			shareFigureIsTheSets(t, c, "after the record's segment retired", row.tag)
		})
	}
}
