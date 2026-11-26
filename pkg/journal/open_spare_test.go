package journal_test

import (
	"fmt"
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/journal"
	"github.com/cubbit/dittofs/pkg/journal/journaltest"
	"github.com/cubbit/dittofs/pkg/journal/journaltest/kit"
)

// TestFirstRollTakesTheTargetSpare opens a journal of one stream and a spare
// target of one, lets its pool settle, and then writes the stream's first
// segment full and on into the second while every spare make is parked at the
// seam, the way a cache-speed first fill outruns a make: the first roll is
// served only by a spare already in stock. Without the opening spare the
// stream's first segment takes the one target spare, and the first roll is
// refused `spare`. Once the makes run again the pool settles back at its
// target: the opening spare is kept for the first segment only, never for
// every roll. It runs on a fresh journal and on one reopened after writing,
// whose stream starts again with no segment.
func TestFirstRollTakesTheTargetSpare(t *testing.T) {
	for _, reopen := range []bool{false, true} {
		t.Run(fmt.Sprintf("reopen=%v", reopen), func(t *testing.T) {
			f := journaltest.NewWith(t, func(s *journal.Settings) {
				s.Streams = 1
				s.SpareTarget = 1
			})
			h, id := f.Share(0), journaltest.File(0)
			chunk := make([]byte, 64<<10)
			if reopen {
				if _, err := h.WriteAt(t.Context(), id, 0, chunk, 0); err != nil {
					t.Fatalf("WriteAt before the reopen: %v", err)
				}
				f.Reopen()
				f.WaitSpares()
				h = f.Share(0)
			}
			if got, want := f.J.Stats().Spares, 2; got != want {
				t.Fatalf("Spares = %d once the pool settled at open, want %d: the target's one and the stream's opening spare", got, want)
			}

			makes := f.Seam.Hold(t, kit.OpCreate, "*.spare.tmp")
			for off := int64(0); off < journaltest.FixtureSegmentCap+int64(2*len(chunk)); off += int64(len(chunk)) {
				if _, err := h.WriteAt(t.Context(), id, off, chunk, 0); err != nil {
					t.Fatalf("WriteAt at %d with every make parked: %v; want the first roll served from the pool's stock", off, err)
				}
			}
			// The roll spent the target spare, so the pool went to replace it:
			// the make it started is the one parked here.
			select {
			case <-makes.Reached():
			case <-time.After(30 * time.Second):
				t.Fatalf("no spare make reached the seam after the first roll; want the roll to have taken the target spare")
			}

			makes.Release()
			f.WaitSpares()
			if got, want := f.J.Stats().Spares, 1; got != want {
				t.Fatalf("Spares = %d once the pool settled after the first roll, want %d: the target alone", got, want)
			}
		})
	}
}

// TestAJournalAtItsMaximumOpensWithoutTheOpeningSpare opens a journal whose
// maximum holds its spares' targets and no more: the opening spare does not
// fit, so the pool settles at its target and the fixture's wait for the
// spares at open is answered, not refused one short.
func TestAJournalAtItsMaximumOpensWithoutTheOpeningSpare(t *testing.T) {
	f := journaltest.NewWith(t, func(s *journal.Settings) {
		s.Streams = 1
		s.SpareTarget = 1
		// The content target's one, the headroom's one and the reserve's one.
		s.MaxBytes = 3 * journaltest.FixtureSegmentCap
	})
	if got, want := f.J.Stats().Spares, 1; got != want {
		t.Fatalf("Spares = %d at open at the maximum, want %d: the target alone", got, want)
	}
}
