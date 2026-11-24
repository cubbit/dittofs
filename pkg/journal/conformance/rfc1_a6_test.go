//go:build journal_conformance

package conformance

import (
	"fmt"
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/journal"
	"github.com/cubbit/dittofs/pkg/journal/journaltest"
	"github.com/cubbit/dittofs/pkg/journal/journaltest/kit"
)

// TestRFC1A_SettledRemovalOutlivesItsUnlinks is the settled repeat of
// "removal outlives its segment": a delete settled before reclamation is
// kept while the unlink of the segment holding the bytes it removed is not
// durable. The old bytes are written in a segment of their own, sealed, and
// killed by an overwrite that lands beside the delete in another segment.
// The old bytes' segment is unlinked with the one directory sync after its
// unlink failing, so the unlink is never durable and a crash brings the
// segment back, though it has left the journal's set. A pass over the
// delete's segment then runs with every sync succeeding, so whatever it
// retires stays gone. The file must still read as gone after a crash: a
// journal that drops the settled delete once the old segment left its set,
// before its unlink was durable, retires the delete's segment and serves
// the old bytes again. A journal that keeps the delete where it is, or
// carries it forward, passes.
//
// The seam has no tear that keeps a later directory change and loses an
// earlier one under a reported success; a failed sync is what tells a
// correct journal the unlink is not durable, and the seam keeps a change a
// failed sync lost lost, whatever later syncs succeed.
func TestRFC1A_SettledRemovalOutlivesItsUnlinks(t *testing.T) {
	f := journaltest.NewWithSegmentCap(t, recordCap, nil)
	const size = recordCap / 2

	for k := range 2 {
		h, id := f.Share(k), journaltest.File(k)
		write(t, h, id, 0, distinct(50+k, size))
		oldSeg := heldEntry(t, f, id).Location.Segment
		sealNow(t, f, oldSeg)
		// The unlink of the old bytes' segment, whenever the journal makes
		// it, is stopped at the seam until the directory sync after it is
		// armed to fail.
		unlink := f.Seam.Hold(t, kit.OpRemove, segmentName(oldSeg))
		write(t, h, id, 0, distinct(60+k, size))
		deleteSeg := heldEntry(t, f, id).Location.Segment
		if deleteSeg == oldSeg {
			t.Fatalf("share %d: the overwrite landed in the sealed segment %d of the bytes it kills", k, oldSeg)
		}
		dv, err := h.Delete(t.Context(), id)
		stub(t, "Delete", err)
		if err != nil {
			t.Fatalf("share %d: delete: %v", k, err)
		}
		syncFile(t, h, id)
		h.Settle(id, dv)

		// The old bytes' segment is unlinked, by a pass or by the journal's
		// own retirement of a segment nothing holds, and the one directory
		// sync after the unlink fails. Then a pass over the delete's segment
		// runs with every sync succeeding.
		f.Advance(2 * journaltest.IdleSealAge)
		passed := make(chan error, 1)
		go func() {
			_, err := f.J.Repack(t.Context(), journaltest.FixtureMaxBytes, journal.RepackScope{Segments: []journal.SegmentID{oldSeg}})
			passed <- err
		}()
		select {
		case <-unlink.Reached():
		case <-time.After(backgroundWait):
			t.Fatalf("share %d: segment %d holding only dead bytes was never unlinked", k, oldSeg)
		}
		f.FailNext(kit.OpSyncDir, "*", 1)
		unlink.Release()
		eventually(t, fmt.Sprintf("segment %d was not retired", oldSeg), func() bool { return retired(f, oldSeg) })
		if err := <-passed; err != nil {
			stub(t, "Repack", err)
			t.Fatalf("share %d: the pass over segment %d: %v", k, oldSeg, err)
		}
		f.Seam.SetFault(nil)
		eventually(t, fmt.Sprintf("segment %d was not sealed", deleteSeg), func() bool {
			info, ok := segmentInfo(f, deleteSeg)
			return ok && info.Sealed
		})
		err = within(t, f, "the pass over the delete's segment", func() error {
			_, err := f.J.Repack(t.Context(), journaltest.FixtureMaxBytes, journal.RepackScope{Segments: []journal.SegmentID{deleteSeg}})
			return err
		})
		stub(t, "Repack", err)
		if err != nil {
			t.Fatalf("share %d: the pass over segment %d: %v", k, deleteSeg, err)
		}

		// A crash now brings back the old bytes' segment, whose unlink no
		// sync made durable. Whether the pass retired the delete's segment
		// is the journal's to decide: it may keep the delete there, or carry
		// it forward, while what it removed may still come back.
		if _, err := f.Seam.Durable(segmentName(oldSeg)); err != nil {
			t.Fatalf("share %d: a crash would leave no segment %d (%v): its unlink was durable, so the leg checks nothing", k, oldSeg, err)
		}
		f.Crash()

		r := read(t, f.Share(k), id, 0, size)
		wantMissing(t, r, 0, []journal.Extent{ext(0, size)}, "the settled deleted file after a crash undid its old segment's unlink")
	}
}
