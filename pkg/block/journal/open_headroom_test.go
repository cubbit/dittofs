package journal_test

import (
	"errors"
	"fmt"
	"testing"

	"github.com/cubbit/dittofs/pkg/block/journal"
	"github.com/cubbit/dittofs/pkg/block/journal/journaltest"
)

// TestOpenWiresTheHeadroom checks, through Stats, that a journal the
// production open built draws a truncate at its maximum on the headroom its
// settings reserve: once on the journal Init made, once on the same
// directory reopened through the recovery path, the two places the open
// builds the core.
func TestOpenWiresTheHeadroom(t *testing.T) {
	for _, reopen := range []bool{false, true} {
		t.Run(fmt.Sprintf("reopen=%v", reopen), func(t *testing.T) {
			f := journaltest.New(t)
			h, id := f.Share(0), journaltest.File(0)
			chunk := make([]byte, 256<<10)
			var off int64
			for {
				err := journaltest.RetrySpare(func() error {
					_, err := h.WriteAt(t.Context(), id, off, chunk, 0)
					return err
				})
				var ns *journal.NoSpaceError
				if errors.As(err, &ns) && ns.Limit == journal.LimitJournal {
					break
				}
				if err != nil {
					t.Fatalf("WriteAt at %d before the maximum: %v", off, err)
				}
				if off += int64(len(chunk)); off > 2*journaltest.FixtureMaxBytes {
					t.Fatalf("%d bytes written into a journal of %d were never refused", off, journaltest.FixtureMaxBytes)
				}
			}
			if reopen {
				f.Reopen()
				h = f.Share(0)
			}

			before := f.J.Stats().Counters.HeadroomDraws
			if _, err := h.Truncate(t.Context(), id, 4096); err != nil {
				t.Fatalf("Truncate at the journal's maximum: %v", err)
			}
			if got := f.J.Stats().Counters.HeadroomDraws; got != before+1 {
				t.Fatalf("HeadroomDraws = %d after a truncate at the maximum, want %d", got, before+1)
			}
		})
	}
}
