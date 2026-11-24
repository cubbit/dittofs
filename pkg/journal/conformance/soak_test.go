//go:build journal_conformance && journal_soak

package conformance

import (
	"os"
	"strings"
	"testing"
	"time"
)

// soakDuration returns how long the soak runs: DITTOFS_SOAK_DURATION, parsed
// as a duration, set by the nightly suite's cell to the hours it wants.
// Unset, or anything that parses to zero or below — "0" and "0s" alike —
// means the 5-minute calibration default, so the test also runs by hand
// at a size its evidence can be read from. A text that parses as no
// duration is the knob misused, and fails the run.
func soakDuration(t *testing.T) time.Duration {
	t.Helper()

	s, ok := os.LookupEnv("DITTOFS_SOAK_DURATION")
	if !ok || s == "" {
		return 5 * time.Minute
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		t.Fatalf("DITTOFS_SOAK_DURATION=%q: %v", s, err)
	}
	if d <= 0 {
		return 5 * time.Minute
	}
	return d
}

// TestJournalSoak drives one journal at capacity pressure for hours —
// overwrites and repacks against a maximum a fraction of what the run
// writes, with a simulated power loss and a reopen every slow period — and
// holds the gauges flat over the whole run: no counter, no descriptor count
// and no heap may end above the band a clean journal keeps.
//
// The suite this test runs in has no known failures and no retries: a red
// here is a leak to fix.
//
// The main run is driven by wall-clock deadlines: DITTOFS_SOAK_DURATION
// names its stop, its windows scale with it, and the loop ends at the
// operation that finds the deadline past - never at a timer, a sleep or a
// fixed operation count, which the planted leg alone takes.
func TestJournalSoak(t *testing.T) {
	log := soakRun(t, soakCfg{duration: soakDuration(t)})

	t.Logf("soak: %d ops over %d windows: %d writes (%d refused at capacity, %d reclaimed), %d syncs, %d settles, %d repacks (%v, %d bytes freed), %d crashes",
		log.ops, len(log.samples), log.writes, log.refused, log.reclaimed, log.syncs, log.settles, log.repacks, log.repackWall, log.freed, log.crashes)

	// What the run reached, as the model's sweeps reach-assert: a run that
	// wrote no more than the journal's maximum held no pressure, one whose
	// repacks freed nothing never reclaimed, and one that crossed no crash
	// checked no recovery — a degenerate pass of any of the three is a rig
	// fault, not a green soak.
	if log.written <= soakMaxBytes {
		t.Errorf("soak: reached: %d bytes written, want over the %d-byte maximum: a run that never met the journal's limit holds no pressure", log.written, soakMaxBytes)
	}
	if log.refused == 0 && log.freed == 0 {
		t.Errorf("soak: reached: %d capacity refusals and %d bytes freed, want one of them above zero: a run that never reclaimed checked no reclamation", log.refused, log.freed)
	}
	if log.crashes == 0 {
		t.Errorf("soak: reached: %d crashes, want the run to have crossed and recovered a simulated power loss", log.crashes)
	}

	// The flat checks run after the last window, over the whole run's band:
	// every gauge the run snapshots stays flat — the end within its
	// calibrated threshold of the band its samples span, and the band no
	// wider — so a rise that stays there is told from the oscillation a
	// journal at capacity makes by outliving it.
	for _, flat := range soakFlats {
		vals := make([]int64, len(log.samples))
		for i := range log.samples {
			vals[i] = flat.get(&log.samples[i])
		}
		t.Logf("soak: %s samples: first=%v last=%v, band=%v..%v, halves' medians=%v/%v",
			flat.name, vals[0], vals[len(vals)-1], soakLeast(vals), soakMost(vals), soakMedian(vals[:len(vals)/2]), soakMedian(vals[len(vals)/2:]))
		if err := soakWantFlat(flat.name, vals, flat.threshold, flat.floor); err != nil {
			t.Errorf("soak: %v", err)
		}
	}
}

// soakLeast and soakMost return the least and the greatest of vals.
func soakLeast(vals []int64) int64 {
	l := vals[0]
	for _, v := range vals[1:] {
		l = min(l, v)
	}
	return l
}

func soakMost(vals []int64) int64 {
	m := vals[0]
	for _, v := range vals[1:] {
		m = max(m, v)
	}
	return m
}

// TestJournalSoakFindsPlantedDescriptor proves the flat checks catch what
// they claim to: exactly one descriptor, opened by the test itself mid-run
// and never closed, must fail the descriptor check *on it*. The plant is
// fault injection on the measurement — the soak's own rig, its own windows,
// one handle nobody closes, the journal underneath untouched — not a stub
// standing in for it.
//
// The check it faces is the production one, unchanged: the Descriptors
// gauge the main soak runs — the journal-neutralised count, whose crash
// shifts cancel (see the gauge table) — read through soakGaugeOf, which
// hands the leg the gauge's own series, threshold and floor rather than
// any it might spell itself, so a leak this leg plants is, by
// construction, one the main soak's own check would catch. The plant lands
// before the run's crash window, so the series it fails the check on
// carries a crash and a reopen inside its band too: the reopen shifts the
// journal's own descriptors and the process's by the same handle, and the
// difference reads through it. What proves the claim is the halves' median
// criterion alone — a permanent +1 in the process series lifts the second
// half's middle above the first's, where the sampling skew that moves a
// single window cannot — so the leg pins that criterion in its failure
// and its log.
func TestJournalSoakFindsPlantedDescriptor(t *testing.T) {
	// Enough windows that the plant, placed after the warm-up so the band
	// reads the clean floor first, is a rise the end of the run still
	// carries whole halves around — and one crash window past it, whose
	// reopen the neutralised series must read through.
	plant := soakCfg{maxOps: soakWindowOps * 24, plantOp: soakWindowOps * 13}
	log := soakRun(t, plant)
	if log.planted == nil {
		t.Fatalf("the run planted no descriptor, so this leg checks nothing")
	}
	// The plant is this leg's own fault, so it closes the handle here,
	// before the fixture's watch looks: what that watch bounds is what the
	// test leaks of itself, and this leg leaves nothing.
	defer log.planted.Close()

	descriptors := soakGaugeOf("Descriptors")
	past := make([]int64, len(log.samples))
	for i := range log.samples {
		past[i] = descriptors.get(&log.samples[i])
	}
	err := soakWantFlat("Descriptors", past, descriptors.threshold, descriptors.floor)
	if err == nil {
		t.Fatalf("the descriptor check passed with one descriptor planted at op %d and never closed: descriptors past the journal's own over the windows %v, want the check to fail on the plant", plant.plantOp, past)
	}
	t.Logf("the planted descriptor was caught by the production gauge's check:\n%v", err)
	if !strings.Contains(err.Error(), "the second half's median") {
		t.Fatalf("the descriptor check failed with:\n%v\nwant it to fail on the median criterion exactly, the one criterion a never-returns rise trips", err)
	}
	// The plant is the whole rise: the journal-neutralised count's median
	// moves across the band by the one planted descriptor and nothing else —
	// single windows can skew a unit on the sampling order, so the leg
	// asserts the move on the same medians the check reads.
	med := len(past) / 2
	if rise := soakMedian(past[med:]) - soakMedian(past[soakWarmupWindows:med]); rise != 1 {
		t.Fatalf("the descriptors past the journal's own moved by %d between the halves' medians, want exactly the one planted descriptor: %v", rise, past)
	}
}

// TestJournalSoakFindsPlantedFootprint proves the footprint gauges catch
// what they claim to: eight segments' worth of bytes, written into the
// journal's directory mid-run by the test itself and never removed — a
// remnant the journal's own accounting does not carry, as a retire path
// that drops a segment from its books and never unlinks its file leaves —
// must fail the directory-bytes check *on it*. The plant is fault
// injection on the same measurement the soak reads, like the descriptor
// leg beside it, not a stub in front of the journal; the count gauge
// carries no floor - the reopen's spare-making swings the count wider
// than one file - so the byte total is the gauge this leg proves.
//
// The check it faces is the production one, through soakGaugeOf: the
// DirBytes gauge the main soak runs, whose floor the orphan's one-segment
// step must trip through the medians. The run's crashes land inside the
// band too — they remove spares no sync covered and so drop the count,
// which the median of either half sits under, and which never lifts a
// half — so what the leg catches, a never-removed remnant does, at every
// duration the main run allows.
func TestJournalSoakFindsPlantedFootprint(t *testing.T) {
	plant := soakCfg{maxOps: soakWindowOps * 24, plantOp: soakWindowOps * 13, plantDisk: true}
	log := soakRun(t, plant)
	if log.orphan == "" {
		t.Fatalf("the run planted no orphaned file, so this leg checks nothing")
	}

	bytes := soakGaugeOf("DirBytes")
	bseries := make([]int64, len(log.samples))
	for i := range log.samples {
		bseries[i] = bytes.get(&log.samples[i])
	}
	err := soakWantFlat("DirBytes", bseries, bytes.threshold, bytes.floor)
	if err == nil {
		t.Fatalf("the byte-total check passed with an orphaned file planted at op %d and never removed: the directory's bytes over the windows %v, want the check to fail on the plant", plant.plantOp, bseries)
	}
	t.Logf("the planted footprint was caught by the production gauge's check:\n%v", err)

	// The orphan is the whole never-returns rise on the byte series, so the
	// failure the check reports is the plant's own arithmetic; the count
	// gauge beside it is a width-and-drift bound alone, a reopen's
	// spare-making swinging the count wider than one file, so the byte
	// total is the gauge the plant answers to.
	if !strings.Contains(err.Error(), "the second half's median") {
		t.Fatalf("the byte-total check failed with:\n%v\nwant it to fail on the median criterion exactly, the one criterion a never-returns rise trips", err)
	}
	// The medians move in the churn's own quantum - whole segments, a MiB
	// at a time - so the plant's exact arithmetic reads through it as the
	// orphan's eight segments within the three the churn swings.
	med := len(bseries) / 2
	rise := soakMedian(bseries[med:]) - soakMedian(bseries[soakWarmupWindows:med])
	if d := rise - soakOrphanBytes; d < -3<<20 || d > 3<<20 {
		t.Fatalf("the directory's bytes moved by %d between the halves' medians, want the %d-byte orphan within the churn's three segments: %v", rise, soakOrphanBytes, bseries)
	}
}
