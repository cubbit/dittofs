//go:build journal_conformance

package conformance

import (
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/block/journal/journaltest"
	"github.com/cubbit/dittofs/pkg/block/journal/journaltest/kit"
)

// TestStatsAfterOpenWalksNoLock re-runs the Stats cost check over the fields
// a reopened journal serves — Recovery, the open's counters and SyncMarks: a
// journal reopened over a large recovered state allocates per Stats call no
// more than the same journal reopened over a small one, so the recovered
// figures cost what the streams and segments they name cost and never a walk.
// Like the flat-cost check it counts allocations, which the race detector
// changes, so it skips under it and runs in this suite's own leg.
func TestStatsAfterOpenWalksNoLock(t *testing.T) {
	if raceEnabled {
		t.Skip("the race detector changes allocation counts")
	}

	f := journaltest.New(t)
	h := f.Share(0)

	// One fixture across both sizes, its watches and cleanups alive once:
	// the profiler's watch is process-wide and two of its fixtures overlap,
	// as StatsCostIsFlat's own shape keeps to one.
	allocsReopened := func(extents int) float64 {
		for i := f.J.Stats().ExtentCount; i < int64(extents); i++ {
			// One-byte writes a byte apart never merge into one extent.
			write(t, h, journaltest.File(1), 2*i, []byte{byte(i)})
		}
		f.Reopen()
		h = f.Share(0)

		// AllocsPerRun counts the whole process's allocations, so the
		// measure waits for the journal's background work, such as making
		// spares, to go quiet first and keeps the least of several samples.
		quiet(t, f)
		least := testing.AllocsPerRun(10, func() { f.J.Stats() })
		for range 4 {
			least = min(least, testing.AllocsPerRun(10, func() { f.J.Stats() }))
		}
		return least
	}
	small := allocsReopened(1_000)
	large := allocsReopened(100_000)

	// The figures the check measures over are the open's own: the recovery
	// read segments back, and the marks it recovered name the streams'
	// records — otherwise the measure would pin the cost of a zero figure
	// the open never filled. The counts are read from the reopened journal
	// at its larger size, which the smaller size already served the same
	// way.
	st := f.J.Stats()
	if got := st.Recovery.CatalogSegments + st.Recovery.ScannedSegments; got == 0 {
		t.Fatalf("the reopened journal's Recovery = %+v, want the open to have read segments back", st.Recovery)
	}
	if len(st.SyncMarks) == 0 {
		t.Fatalf("the reopened journal's SyncMarks = %+v, want an entry for a stream whose records the open recovered", st.SyncMarks)
	}

	// The rounded comparison, not the raw least-of-samples: the samples
	// average fractional allocations, so two equal-count answers can miss
	// each other by a rounding boundary's worth of noise, which is not a
	// growth.
	t.Logf("Stats after an open allocates %.0f times at 10^5 extents and %.0f at 10^3", large, small)
	if int64(large+0.5) > int64(small+0.5) {
		t.Fatalf("Stats after an open allocates %.2f times at 10^5 recovered extents and %.2f at 10^3: its cost grows with the recovered journal", large, small)
	}
}

// quiet waits for the journal's background work, such as making spares, to go
// quiet, so allocation samples measure no more of it.
func quiet(t *testing.T, f *journaltest.Fixture) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for prev := -1; ; time.Sleep(10 * time.Millisecond) {
		if n := f.Seam.Calls(kit.OpWrite); n == prev || time.Now().After(deadline) {
			return
		} else {
			prev = n
		}
	}
}
