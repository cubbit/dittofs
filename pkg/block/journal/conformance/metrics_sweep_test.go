//go:build journal_conformance

package conformance

import (
	"errors"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"github.com/cubbit/dittofs/pkg/block/journal"
	"github.com/cubbit/dittofs/pkg/block/journal/journaltest"
	"github.com/cubbit/dittofs/pkg/block/journal/journaltest/kit"
)

// metricNames maps each Stats field this sweep drives to the metric name the
// engine's exporter gives it. The journal exports nothing itself (RFC 1
// section 3.9: its dependency set excludes any metrics library); the engine
// exports every field of section 3.7, each counter as journal_<field>_total
// and each gauge as journal_<field>, and section 9.4 reads each "through
// Stats". So a metric is reachable exactly when its Stats field moves for
// the condition it names, and the name an operator would scrape lives in
// this table, one row per field.
//
// decision: superseded_extents has no Counters field on this branch: the
// sweep would drive its loss event's Reason and report the missing counter
// rather than inventing one, but no public call produces that reason either,
// so it has no row here. The cost is one follow-up task adding the counter
// and its subtest beside the others; a field the journal does not keep
// cannot be asserted to move.
var metricNames = map[string]string{
	"Counters.Syncs":                    "journal_syncs_total",
	"Counters.SyncFailures":             "journal_sync_failures_total",
	"Counters.SyncReappendedBytes":      "journal_sync_reappended_bytes_total",
	"Counters.CorruptExtentsDataLoss":   `journal_corrupt_extents_total{outcome="data_loss"}`,
	"Counters.CorruptExtentsRepairable": `journal_corrupt_extents_total{outcome="repairable"}`,
	"Counters.StaleExtents":             "journal_stale_extents_total",
	"Stats.LossGeneration":              "journal_loss_generation",
	"Stats.Losses":                      "journal_losses",
	"Stats.ReadOnly":                    "journal_read_only",
}

// exporterName derives the metric name the engine's exporter gives the Stats
// field path: journal_ plus the field's name as the exporter's snake case,
// _total for a counter, and the two corrupt-extent counters share one name,
// journal_corrupt_extents_total, labelled with their outcome rather than
// split into two fields.
func exporterName(field, snake string) string {
	name := "journal_" + snake
	switch field {
	case "Counters.CorruptExtentsDataLoss", "Counters.CorruptExtentsRepairable":
		outcome := "repairable"
		if field == "Counters.CorruptExtentsDataLoss" {
			outcome = "data_loss"
		}
		return "journal_corrupt_extents_total{outcome=" + strconv.Quote(outcome) + "}"
	}
	if strings.HasPrefix(field, "Counters.") {
		return name + "_total"
	}
	return name
}

// TestMetrics_Epic6ExporterNames checks that every row of metricNames is the
// name the exporter's rule gives its field: each counter journal_<field>_total,
// each gauge journal_<field>, the corrupt extents labelled with their
// outcome. A mistyped or stale row fails here rather than handing the
// operator a metric name nothing serves.
func TestMetrics_Epic6ExporterNames(t *testing.T) {
	for field, name := range metricNames {
		_, snake, _ := strings.Cut(field, ".")
		var b strings.Builder
		for i := range snake {
			if c := snake[i]; c >= 'A' && c <= 'Z' {
				if i > 0 {
					b.WriteByte('_')
				}
				b.WriteByte(c + 'a' - 'A')
			} else {
				b.WriteByte(c)
			}
		}
		if want := exporterName(field, b.String()); name != want {
			t.Errorf("%s maps to %q, want the exporter's name %q", field, name, want)
		}
	}
}

// wantNamed asserts, for the field the subtest drives, that the metric name
// the exporter gives it exists in the table: each subtest names its row, so
// the table is checked row by row and a field with no name fails the sweep.
func wantNamed(t *testing.T, field string) {
	t.Helper()

	if metricNames[field] == "" {
		t.Fatalf("%s has no metric name in the sweep's table", field)
	}
}

// TestMetrics_Epic6ConditionsMoveTheirMetric drives the condition each
// metric of this area names and reads the metric through Stats: a sync, a
// failed sync, a re-append, a loss of each reason, a corrupt extent of each
// outcome, and a read-only entry. Each subtest starts from a fresh fixture,
// asserts its metric moved and the sibling another condition could raise by
// mistake stayed flat, and names its row of metricNames.
//
// decision: the superseded and acknowledged loss reasons have no subtest
// here: no public call produces them (DropOlder and DropSuperseded land with
// a later epic's surface), and an acknowledged loss needs an operator call
// that does not exist. Rather than stub a drive, the sweep asserts none of
// them and the missing rows are reported. Overturned when the reconcile
// surface lands, when both subtests go in beside the others.
func TestMetrics_Epic6ConditionsMoveTheirMetric(t *testing.T) {
	// A clean sync raises journal_syncs_total and leaves the failure
	// counter flat.
	t.Run("sync", func(t *testing.T) {
		wantNamed(t, "Counters.Syncs")
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

	// A sync whose storage fails raises journal_sync_failures_total, with
	// LastSyncError carrying the failure.
	t.Run("failed_sync", func(t *testing.T) {
		wantNamed(t, "Counters.SyncFailures")
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

	// A failed window re-appended by the journal raises
	// journal_sync_reappended_bytes_total and holds the loss generation:
	// re-appending loses nothing. failWindow fails the segment's reads too,
	// so the window can resolve only from the copy the journal kept at write
	// time, and a journal that kept one re-appends rather than fails. Content
	// re-appended from a copy holds the generation and moves only the
	// re-appended-bytes counter; a window failed whole is loss_failed_window's
	// leg, and one whose sync merely failed is failed_sync's.
	t.Run("reappend", func(t *testing.T) {
		wantNamed(t, "Counters.SyncReappendedBytes")
		f := journaltest.New(t)
		h := f.Share(0)
		id := journaltest.File(20)
		write(t, h, id, 0, pattern("reappend", 4096))
		seg := segmentOf(t, f, id, 0)
		before, gen := f.J.Stats().Counters, f.J.Stats().LossGeneration
		failWindow(t, f, seg)
		err := h.Sync(t.Context(), id)
		f.Seam.SetFault(nil)
		stub(t, "Sync", err)
		if errors.Is(err, journal.ErrSyncFailed) {
			t.Fatalf("the window failed rather than re-appending, so Counters.SyncReappendedBytes cannot rise: a journal that keeps a copy re-appends, and one that re-reads the segment cannot, with its reads failed")
		}
		if err != nil {
			t.Fatal(err)
		}
		after := f.J.Stats().Counters
		if after.SyncReappendedBytes <= before.SyncReappendedBytes {
			t.Fatalf("a re-appended window moved Counters.SyncReappendedBytes from %d to %d, want it risen", before.SyncReappendedBytes, after.SyncReappendedBytes)
		}
		if now := f.J.Stats().LossGeneration; now != gen {
			t.Fatalf("a re-append moved the loss generation from %d to %d, want it held: nothing was lost", gen, now)
		}
	})

	// A window the journal fails — its segment's reads failing beside its
	// sync, so it can resolve only from a copy the journal does not keep —
	// raises the loss generation, publishes a LossFailedWindow event for
	// the write it lost, and raises SyncFailures. The re-appended
	// resolution is reappend's leg.
	t.Run("loss_failed_window", func(t *testing.T) {
		wantNamed(t, "Stats.LossGeneration")
		f := journaltest.New(t)
		h := f.Share(0)
		id := journaltest.File(30)
		p := pattern("window", 4096)
		write(t, h, id, 0, p)
		seg := segmentOf(t, f, id, 0)
		before, losses, gen := f.J.Stats().Counters, f.J.Stats().Losses, f.J.Stats().LossGeneration
		failWindow(t, f, seg)
		err := h.Sync(t.Context(), id)
		f.Seam.SetFault(nil)
		stub(t, "Sync", err)
		if !errors.Is(err, journal.ErrSyncFailed) {
			t.Fatalf("a window the journal fails returned %v, want ErrSyncFailed; the re-appended resolution is the reappend subtest's", err)
		}
		after := f.J.Stats().Counters
		if after.SyncFailures <= before.SyncFailures {
			t.Fatalf("a failed window moved Counters.SyncFailures from %d to %d, want it risen", before.SyncFailures, after.SyncFailures)
		}
		ev := lastLoss(t, f, journal.Stats{Losses: losses})
		if ev.Reason != journal.LossFailedWindow || ev.File != id || ev.Extent.Off != 0 {
			t.Fatalf("the failed window published %+v, want a LossFailedWindow event of file %v at offset 0", ev, id)
		}
		if now := f.J.Stats().LossGeneration; now <= gen {
			t.Fatalf("a failed window left the loss generation at %d, want it above %d", gen, now)
		}
	})

	// A stale extent raises journal_stale_extents_total, publishes its
	// LossStale event, and holds the loss generation: what was dropped is
	// older than content already offloaded.
	t.Run("loss_stale", func(t *testing.T) {
		wantNamed(t, "Counters.StaleExtents")
		f := journaltest.New(t)
		h := f.Share(0)
		id, e := journaltest.File(2), ext(0, 4096)
		write(t, h, id, 0, pattern("stale", int(e.Len)))
		newer, err := f.J.Draw()
		stub(t, "Draw", err)
		if err != nil {
			t.Fatal(err)
		}
		mark(t, h, id, e, newer)
		before, losses, gen := f.J.Stats().Counters, f.J.Stats().Losses, f.J.Stats().LossGeneration
		read(t, h, id, 0, e.Len)
		after := f.J.Stats().Counters
		if after.StaleExtents <= before.StaleExtents {
			t.Fatalf("a stale extent moved Counters.StaleExtents from %d to %d, want it risen", before.StaleExtents, after.StaleExtents)
		}
		if after.CorruptExtentsDataLoss != before.CorruptExtentsDataLoss || after.CorruptExtentsRepairable != before.CorruptExtentsRepairable {
			t.Fatalf("a stale extent moved the corrupt counters from %d/%d to %d/%d, want them flat", before.CorruptExtentsDataLoss, before.CorruptExtentsRepairable, after.CorruptExtentsDataLoss, after.CorruptExtentsRepairable)
		}
		ev := lastLoss(t, f, journal.Stats{Losses: losses})
		if ev.Reason != journal.LossStale || ev.File != id || ev.Extent != e {
			t.Fatalf("the stale drop published %+v, want a LossStale event of file %v at %+v", ev, id, e)
		}
		if now := f.J.Stats().LossGeneration; now != gen {
			t.Fatalf("a stale extent moved the loss generation from %d to %d, want it held: nothing not yet offloaded was lost", gen, now)
		}
	})

	// A corrupt extent whose offloaded bit is unset raises
	// journal_corrupt_extents_total{outcome=data_loss} only.
	t.Run("corrupt_data_loss", func(t *testing.T) {
		wantNamed(t, "Counters.CorruptExtentsDataLoss")
		before, after := corruptOutcome(t, false)
		if after.CorruptExtentsDataLoss <= before.CorruptExtentsDataLoss {
			t.Fatalf("a corrupt dirty extent moved CorruptExtentsDataLoss from %d to %d, want it risen", before.CorruptExtentsDataLoss, after.CorruptExtentsDataLoss)
		}
		if after.CorruptExtentsRepairable != before.CorruptExtentsRepairable {
			t.Fatalf("a corrupt dirty extent moved CorruptExtentsRepairable from %d to %d, want it flat: the offloaded bit was unset", before.CorruptExtentsRepairable, after.CorruptExtentsRepairable)
		}
	})

	// The same corruption on an extent whose offloaded bit is set raises
	// journal_corrupt_extents_total{outcome=repairable} only.
	t.Run("corrupt_repairable", func(t *testing.T) {
		wantNamed(t, "Counters.CorruptExtentsRepairable")
		before, after := corruptOutcome(t, true)
		if after.CorruptExtentsRepairable <= before.CorruptExtentsRepairable {
			t.Fatalf("a corrupt offloaded extent moved CorruptExtentsRepairable from %d to %d, want it risen", before.CorruptExtentsRepairable, after.CorruptExtentsRepairable)
		}
		if after.CorruptExtentsDataLoss != before.CorruptExtentsDataLoss {
			t.Fatalf("a corrupt offloaded extent moved CorruptExtentsDataLoss from %d to %d, want it flat: the offloaded bit was set", before.CorruptExtentsDataLoss, after.CorruptExtentsDataLoss)
		}
	})

	// A corrupt extent whose offloaded bit is unset publishes a LossCorrupt
	// event, the loss reason the data-loss outcome names, and raises the
	// loss generation: content not yet offloaded was lost.
	t.Run("loss_corrupt", func(t *testing.T) {
		wantNamed(t, "Stats.Losses")
		f := journaltest.New(t)
		before := f.J.Stats().Losses
		loseDirty(t, f, 3)
		ev := lastLoss(t, f, journal.Stats{Losses: before})
		if ev.Reason != journal.LossCorrupt || ev.File != journaltest.File(3) || ev.Extent != ext(0, 4096) {
			t.Fatalf("the corrupt drop published %+v, want a LossCorrupt event of file %v at %+v", ev, journaltest.File(3), ext(0, 4096))
		}
		if after := f.J.Stats().Counters.CorruptExtentsDataLoss; after == 0 {
			t.Fatalf("CorruptExtentsDataLoss = %d after a corrupt dirty extent, want it risen", after)
		}
	})

	// A journal whose storage turns read-only names the state in
	// journal_read_only: ReadOnly set, the error carried, no spare yet. The
	// sync that EROFS failed is a failed sync too, so its counter rises
	// beside the state — a journal that counts the failure but never names
	// the read-only state, or names it while counting nothing, fails one of
	// the two.
	t.Run("read_only", func(t *testing.T) {
		wantNamed(t, "Stats.ReadOnly")
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
		eventually(t, "journal_read_only to name the read-only state", func() bool { return f.J.Stats().ReadOnly.ReadOnly })
		ro := f.J.Stats().ReadOnly
		if !errors.Is(ro.Err, syscall.EROFS) {
			t.Fatalf("journal_read_only names %v, want the EROFS that turned the journal read-only", ro.Err)
		}
		if ro.SpareMade {
			t.Fatalf("journal_read_only reports a spare made over the very failure that turned the journal read-only, want it false")
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
