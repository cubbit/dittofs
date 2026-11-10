//go:build journal_conformance

package conformance

import (
	"errors"
	"syscall"
	"testing"

	"github.com/cubbit/dittofs/pkg/block/journal"
	"github.com/cubbit/dittofs/pkg/block/journal/journaltest"
	"github.com/cubbit/dittofs/pkg/block/journal/journaltest/kit"
)

// TestMetrics_ConditionsMoveTheirMetric drives the condition each metric of
// this area names and reads the metric through Stats: a sync, a failed sync,
// a failed window in both its resolutions, a stale loss, a corrupt extent of
// each outcome, a corrupt loss event, and a read-only entry. Each subtest
// starts from a fresh fixture, asserts its metric moved and the sibling
// another condition could raise by mistake stayed flat.
//
// decision: the metrics are read through Stats, not through the exporter's
// names, because no exporter exists to be asserted against: the journal
// exports nothing itself (RFC 1 section 3.9 keeps any metrics library out
// of its dependency set) and the engine exporter that names these fields is
// not built. A check of the exporter's own output goes where an exporter can
// fail it, in the engine's tests, once one exists. Overturned when the
// engine exporter lands, when this sweep keeps its Stats assertions and the
// exporter's tests assert the names.
//
// decision: the superseded and acknowledged loss reasons have no subtest: no
// public call drops a superseded extent or acknowledges an unreadable one,
// and a superseded loss has no counter to raise. Both are left unasserted
// rather than stubbed with a drive that drives nothing. Overturned when
// those calls land, when their subtests go in beside the others'.
func TestMetrics_ConditionsMoveTheirMetric(t *testing.T) {
	// A clean sync raises the sync counter and leaves the failure counter
	// flat.
	t.Run("sync", func(t *testing.T) {
		f := journaltest.New(t)
		h := f.Share(1)
		before := f.J.Stats().Counters
		write(t, h, journaltest.File(2), 0, pattern("sync", 4096))
		syncNow(t, h, journaltest.File(2))
		after := f.J.Stats().Counters
		if after.Syncs <= before.Syncs {
			t.Fatalf("a sync moved Counters.Syncs from %d to %d, want it risen", before.Syncs, after.Syncs)
		}
		if after.SyncFailures != before.SyncFailures {
			t.Fatalf("a successful sync moved Counters.SyncFailures from %d to %d, want it flat", before.SyncFailures, after.SyncFailures)
		}
	})

	// A sync whose storage fails raises the failure counter, with
	// LastSyncError carrying the failure.
	t.Run("failed_sync", func(t *testing.T) {
		f := journaltest.New(t)
		h := f.Share(0)
		write(t, h, journaltest.File(0), 0, pattern("fail", 4096))
		before := f.J.Stats().Counters
		f.FailNext(kit.OpSync, "*.seg", 1)
		err := h.Sync(t.Context(), journaltest.File(0))
		f.Seam.SetFault(nil)
		stub(t, "Sync", err)
		after := f.J.Stats().Counters
		if after.SyncFailures <= before.SyncFailures {
			t.Fatalf("a failed sync moved Counters.SyncFailures from %d to %d, want it risen", before.SyncFailures, after.SyncFailures)
		}
		if bc := f.J.Stats().LastSyncError; bc == nil || bc.Err == nil {
			t.Fatalf("a failed sync left LastSyncError %+v, want it carrying the failure", bc)
		}
	})

	// A failed sync window resolves by re-appending its records or by
	// failing them, and only its Sync's own answer names which way the
	// journal chose: nil over a re-append, ErrSyncFailed over a failure.
	// failWindow fails the segment's reads beside its sync, so the window
	// resolves only from a copy the journal kept at write time, never from
	// the segment's pages. The window's sync failed whichever resolution it
	// got, so SyncFailures rises either way, and the half the answer named
	// must hold whole: a re-append moves SyncReappendedBytes, holds the
	// loss generation and publishes no loss event, a failure raises the
	// generation and publishes a LossFailedWindow event for the write it
	// lost.
	//
	// decision: the interface names no way to choose the resolution, so
	// the check requires the half its own Sync answer gave whole; a journal
	// that always re-appends from a kept copy leaves the failure half
	// unexercised, and one that never keeps a copy leaves the other.
	// Overturned if the resolution becomes settable.
	t.Run("failed_window", func(t *testing.T) {
		f := journaltest.New(t)
		h := f.Share(0)
		id := journaltest.File(20)
		write(t, h, id, 0, pattern("window", 4096))
		seg := segmentOf(t, f, id, 0)
		before, losses, gen := f.J.Stats().Counters, f.J.Stats().Losses, f.J.Stats().LossGeneration
		failWindow(t, f, seg)
		err := h.Sync(t.Context(), id)
		f.Seam.SetFault(nil)
		stub(t, "Sync", err)
		after := f.J.Stats().Counters
		if after.SyncFailures <= before.SyncFailures {
			t.Fatalf("the failed window moved Counters.SyncFailures from %d to %d, want it risen: its sync failed however it resolved", before.SyncFailures, after.SyncFailures)
		}
		switch {
		case errors.Is(err, journal.ErrSyncFailed):
			if now := f.J.Stats().LossGeneration; now <= gen {
				t.Fatalf("a failed window left the loss generation at %d, want it above %d", now, gen)
			}
			ev := lastLoss(t, f, journal.Stats{Losses: losses})
			if ev.Reason != journal.LossFailedWindow || ev.File != id || ev.Extent.Off != 0 {
				t.Fatalf("the failed window published %+v, want a LossFailedWindow event of file %v at offset 0", ev, id)
			}
		case err == nil:
			if after.SyncReappendedBytes <= before.SyncReappendedBytes {
				t.Fatalf("a re-appended window moved Counters.SyncReappendedBytes from %d to %d, want it risen", before.SyncReappendedBytes, after.SyncReappendedBytes)
			}
			if now, was := f.J.Stats().Losses, f.J.Stats().LossGeneration; len(now) != len(losses) || was != gen {
				t.Fatalf("a re-append published the losses %+v and moved the loss generation from %d to %d, want no event and it held: nothing was lost", now[len(losses):], gen, was)
			}
		default:
			t.Fatalf("the Sync over the failed window returned %v, want ErrSyncFailed or a re-append's nil", err)
		}
	})

	// A stale extent raises the stale counter, publishes its LossStale
	// event, and holds the loss generation: what was dropped is older than
	// content already offloaded. The drop happens inside the mark that
	// names the covering ref (RFC 28 section 3.2 drops what is older), so
	// the counters are snapshotted before it and the read afterwards
	// resolves the extent remote and moves nothing.
	t.Run("loss_stale", func(t *testing.T) {
		f := journaltest.New(t)
		h := f.Share(0)
		id, e := journaltest.File(2), ext(0, 4096)
		write(t, h, id, 0, pattern("stale", int(e.Len)))
		newer, err := f.J.Draw()
		stub(t, "Draw", err)
		if err != nil {
			t.Fatal(err)
		}
		before, losses, gen := f.J.Stats().Counters, f.J.Stats().Losses, f.J.Stats().LossGeneration
		mark(t, h, id, e, newer)
		read(t, h, id, 0, e.Len)
		after := f.J.Stats().Counters
		if after.StaleExtents <= before.StaleExtents {
			t.Fatalf("a stale extent moved Counters.StaleExtents from %d to %d, want it risen", before.StaleExtents, after.StaleExtents)
		}
		ev := lastLoss(t, f, journal.Stats{Losses: losses})
		if ev.Reason != journal.LossStale || ev.File != id || ev.Extent != e {
			t.Fatalf("the stale drop published %+v, want a LossStale event of file %v at %+v", ev, id, e)
		}
		if now := f.J.Stats().LossGeneration; now != gen {
			t.Fatalf("a stale extent moved the loss generation from %d to %d, want it held: nothing not yet offloaded was lost", gen, now)
		}
	})

	// A corrupt extent whose offloaded bit is unset raises the data-loss
	// counter only.
	t.Run("corrupt_data_loss", func(t *testing.T) {
		before, after := corruptOutcome(t, false)
		if after.CorruptExtentsDataLoss <= before.CorruptExtentsDataLoss {
			t.Fatalf("a corrupt dirty extent moved CorruptExtentsDataLoss from %d to %d, want it risen", before.CorruptExtentsDataLoss, after.CorruptExtentsDataLoss)
		}
		if after.CorruptExtentsRepairable != before.CorruptExtentsRepairable {
			t.Fatalf("a corrupt dirty extent moved CorruptExtentsRepairable from %d to %d, want it flat: the offloaded bit was unset", before.CorruptExtentsRepairable, after.CorruptExtentsRepairable)
		}
	})

	// The same corruption on an extent whose offloaded bit is set raises the
	// repairable counter only.
	t.Run("corrupt_repairable", func(t *testing.T) {
		before, after := corruptOutcome(t, true)
		if after.CorruptExtentsRepairable <= before.CorruptExtentsRepairable {
			t.Fatalf("a corrupt offloaded extent moved CorruptExtentsRepairable from %d to %d, want it risen", before.CorruptExtentsRepairable, after.CorruptExtentsRepairable)
		}
		if after.CorruptExtentsDataLoss != before.CorruptExtentsDataLoss {
			t.Fatalf("a corrupt offloaded extent moved CorruptExtentsDataLoss from %d to %d, want it flat: the offloaded bit was set", before.CorruptExtentsDataLoss, after.CorruptExtentsDataLoss)
		}
	})

	// A corrupt extent whose offloaded bit is unset also publishes a
	// LossCorrupt event, the loss reason the data-loss outcome names, and
	// raises the loss generation: content not yet offloaded was lost.
	t.Run("loss_corrupt", func(t *testing.T) {
		f := journaltest.New(t)
		before, losses, gen := f.J.Stats().Counters, f.J.Stats().Losses, f.J.Stats().LossGeneration
		loseDirty(t, f, 3)
		after := f.J.Stats().Counters
		if after.CorruptExtentsDataLoss <= before.CorruptExtentsDataLoss {
			t.Fatalf("a corrupt dirty extent moved CorruptExtentsDataLoss from %d to %d, want it risen", before.CorruptExtentsDataLoss, after.CorruptExtentsDataLoss)
		}
		if now := f.J.Stats().LossGeneration; now <= gen {
			t.Fatalf("a corrupt dirty extent left the loss generation at %d, want it above %d: content not yet offloaded was lost", now, gen)
		}
		ev := lastLoss(t, f, journal.Stats{Losses: losses})
		if ev.Reason != journal.LossCorrupt || ev.File != journaltest.File(3) || ev.Extent != ext(0, 4096) {
			t.Fatalf("the corrupt drop published %+v, want a LossCorrupt event of file %v at %+v", ev, journaltest.File(3), ext(0, 4096))
		}
	})

	// A journal whose storage turns read-only names the read-only state:
	// ReadOnly set, the error carried, no spare yet. The sync that EROFS
	// failed is a failed sync too, so its counter rises beside the state —
	// a journal that counts the failure but never names the read-only
	// state, or names it while counting nothing, fails one of the two.
	t.Run("read_only", func(t *testing.T) {
		f := journaltest.New(t)
		h, id := f.Share(0), journaltest.File(0)
		write(t, h, id, 0, pattern("a", 4096))
		syncNow(t, h, id)
		write(t, h, id, 4096, pattern("b", 4096))

		before := f.J.Stats().Counters
		schedule(t, f, readOnlyRules(syscall.EROFS)...)
		err := h.Sync(t.Context(), id)
		stub(t, "Sync", err)
		if !errors.Is(err, journal.ErrJournalReadOnly) {
			t.Fatalf("a Sync whose writes all failed EROFS returned %v, want ErrJournalReadOnly", err)
		}
		if after := f.J.Stats().Counters; after.SyncFailures <= before.SyncFailures {
			t.Fatalf("the sync that turned the journal read-only moved Counters.SyncFailures from %d to %d, want it risen: it failed", before.SyncFailures, after.SyncFailures)
		}
		eventually(t, "the read-only state to be named in Stats", func() bool { return f.J.Stats().ReadOnly.ReadOnly })
		ro := f.J.Stats().ReadOnly
		if !errors.Is(ro.Err, syscall.EROFS) {
			t.Fatalf("the read-only state names %v, want the EROFS that turned the journal read-only", ro.Err)
		}
		if ro.SpareMade {
			t.Fatalf("the read-only state reports a spare made over the very failure that turned the journal read-only, want it false")
		}
	})
}

// corruptOutcome drives the corrupt record of a dirty or offloaded extent
// through a read, and returns the counters before and after the drop.
func corruptOutcome(t *testing.T, offloaded bool) (before, after journal.Counters) {
	t.Helper()

	f := journaltest.New(t)
	h, id := f.Share(0), journaltest.File(0)
	e := ext(0, 4096)
	p := pattern(map[bool]string{false: "dirty", true: "off"}[offloaded], int(e.Len))
	v := write(t, h, id, 0, p)
	if offloaded {
		mark(t, h, id, e, v)
	}
	syncNow(t, h, id)
	before = f.J.Stats().Counters
	corrupt(t, f, id, p)
	_, _, _, _, err := h.ReadAt(t.Context(), id, 0, make([]byte, len(p)))
	stub(t, "ReadAt", err)
	return before, f.J.Stats().Counters
}
