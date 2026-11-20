package journal_test

import (
	"errors"
	"fmt"
	"testing"

	"github.com/cubbit/dittofs/pkg/block/journal"
	"github.com/cubbit/dittofs/pkg/block/journal/journaltest"
)

// headroomRecordCharge is what one header-only record draws on the headroom:
// a 96-byte header, at most 56 bytes of payload and a 72-byte footer entry.
const headroomRecordCharge = 96 + 56 + 72

// fillToMaximum writes file id through h until the journal refuses a write
// naming its maximum.
func fillToMaximum(t *testing.T, h *journal.Handle, id journal.FileID) {
	t.Helper()
	chunk := make([]byte, 256<<10)
	for off := int64(0); ; off += int64(len(chunk)) {
		if off > 2*journaltest.FixtureMaxBytes {
			t.Fatalf("%d bytes written into a journal of %d were never refused", off, journaltest.FixtureMaxBytes)
		}
		err := journaltest.RetrySpare(func() error {
			_, err := h.WriteAt(t.Context(), id, off, chunk, 0)
			return err
		})
		var ns *journal.NoSpaceError
		if errors.As(err, &ns) && ns.Limit == journal.LimitJournal {
			return
		}
		if err != nil {
			t.Fatalf("WriteAt at %d before the maximum: %v", off, err)
		}
	}
}

// TestOpenWiresTheHeadroom checks, through Stats, that a journal the
// production open built draws a truncate at its maximum on the headroom its
// settings reserve: the draw is counted and HeadroomBytes, the headroom's
// unused reservation, falls by one record's charge. It runs once on the
// journal Init made, once on the same directory reopened through the
// recovery path, the two places the open builds the core.
func TestOpenWiresTheHeadroom(t *testing.T) {
	for _, reopen := range []bool{false, true} {
		t.Run(fmt.Sprintf("reopen=%v", reopen), func(t *testing.T) {
			f := journaltest.New(t)
			h, id := f.Share(0), journaltest.File(0)
			fillToMaximum(t, h, id)
			if reopen {
				f.Reopen()
				h = f.Share(0)
			}

			st := f.J.Stats()
			before, unused := st.Counters.HeadroomDraws, st.HeadroomBytes
			if _, err := h.Truncate(t.Context(), id, 4096); err != nil {
				t.Fatalf("Truncate at the journal's maximum: %v", err)
			}
			st = f.J.Stats()
			if got := st.Counters.HeadroomDraws; got != before+1 {
				t.Fatalf("HeadroomDraws = %d after a truncate at the maximum, want %d", got, before+1)
			}
			if want := unused - headroomRecordCharge; st.HeadroomBytes != want {
				t.Fatalf("HeadroomBytes = %d after a truncate drew on it, want %d: one record's charge below %d", st.HeadroomBytes, want, unused)
			}
		})
	}
}

// TestRestartResetsTheHeadroomPin pins the headroom ledger's restart
// decision rather than guarding a change: the ledger starts empty at every
// open, so HeadroomBytes after a restart reads what a journal that never
// drew reads, whatever the process before it drew at its maximum. Nothing
// seeds the ledger at open, so it cannot fail as the code stands; it fails
// once the drawing side is made to persist across an open, which is when it
// must be rewritten with that decision.
func TestRestartResetsTheHeadroomPin(t *testing.T) {
	f := journaltest.New(t)
	whole := f.J.Stats().HeadroomBytes
	h, id := f.Share(0), journaltest.File(0)
	fillToMaximum(t, h, id)
	for i := range int64(8) {
		if _, err := h.Deallocate(t.Context(), id, journal.Extent{Off: i * 8192, Len: 4096}); err != nil {
			t.Fatalf("Deallocate %d at the journal's maximum: %v", i, err)
		}
	}
	if got, want := f.J.Stats().HeadroomBytes, whole-8*headroomRecordCharge; got != want {
		t.Fatalf("HeadroomBytes = %d after eight draws, want %d", got, want)
	}
	f.Reopen()
	if got := f.J.Stats().HeadroomBytes; got != whole {
		t.Fatalf("HeadroomBytes = %d after a restart, want %d, the headroom whole", got, whole)
	}
}
