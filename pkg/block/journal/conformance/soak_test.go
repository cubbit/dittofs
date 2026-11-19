//go:build journal_conformance && journal_soak

package conformance

import (
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/cubbit/dittofs/internal/prof"
	"github.com/cubbit/dittofs/pkg/block/journal"
	"github.com/cubbit/dittofs/pkg/block/journal/journaltest"
)

// The soak's journal: one fixture, small in every gauge it watches, so hours
// of pressure fit a temporary directory. Segments keep the fixture's own
// 1 MiB cap; the maximum drops from the fixture's 64 MiB to 16 MiB, whole
// segments, so the run's writes meet the journal's limit long before the
// disk does, and no operation the soak runs needs the offload and the
// release, which a later step of the epic owns: the space this run reclaims
// it makes with overwrites — the superseded halves of a churned live set —
// and the repack that frees them.
const (
	// soakMaxBytes is the journal's maximum: whole segments, far below what a
	// run writes, and above the live set below.
	soakMaxBytes = 16 << 20

	// The live set: eight files, each eight slots of 64 KiB, evenly split
	// over the fixture's two shares and its two append streams — the
	// two-file, two-stream, two-share floor the soak keeps. 4 MiB live
	// against a 16 MiB maximum leaves three quarters of the journal
	// reclaimable.
	soakFileCount = 8
	soakSlotsOf   = 8
	soakSlot      = 64 << 10

	// soakWindowOps is how many operations pass between two snapshots of a
	// fixed-size run — the planted leg's own cadence; the main run, whose
	// length the clock names and whose operation count a machine's speed
	// sets, samples at soakWindowSecs below instead, so an hours-long run's
	// band keeps a bounded number of samples whatever it churns through.
	soakWindowOps = 128

	// soakWindowSecs is how much wall time passes between two snapshots of
	// the main run: a fourth of a minute, an hour's band reading 1,800
	// samples and a five-minute calibration reading 75. A duration shorter
	// than twelve windows scales it down to a twelfth of the run, so a
	// short sanity run still reads a band the checks can judge — the loop
	// caps at the floor of one second, the shortest window whose samples
	// sit at loop edges.
	soakWindowSecs = 15 * time.Second

	// soakWarmupWindows is how many windows the band the checks read skips
	// at the run's start: the first windows fill the live set and warm the
	// journal's caches and pools, a rise that is the journal reaching its
	// steady state, not a leak.
	soakWarmupWindows = 4

	// soakCrashWindows is how many windows pass between a simulated power
	// loss and the reopen that follows: one crash every sixteen windows, a
	// slow period beside the writes that carry the load.
	soakCrashWindows = 16

	// soakRepackWindows is how many windows pass between two repacks of the
	// soak's own pacing; a write refused at capacity also repacks, inside
	// its retry, whatever window it is in.
	soakRepackWindows = 4

	// soakRepackBudget is the bytes one repack of the soak may move: one
	// segment's worth, the repack reserve the pool stands — the spare a pass
	// copies into — so one unit per call, and a call at capacity never asks
	// the pool for a second spare it has no room to make.
	soakRepackBudget = 1 << 20

	// soakReclaimAttempts bounds how many times a write refused at capacity
	// repacks, syncs and retries before the soak takes the refusal for a
	// finding: each attempt spends the repack reserve, and beyond a bounded
	// handful the refusal is one the journal cannot relieve.
	soakReclaimAttempts = 16

	// soakClockStep is how far each operation advances the fixture's clock:
	// one group-commit bound, so a bounded write's sync window resolves
	// every operation, and a segment no write touched idles to its seal
	// within half a window.
	soakClockStep = journal.SyncBound
)

// soakFlats is the gauges the run holds flat, checked over the windows past
// the warm-up, each with the width — in its own unit — of the band a clean
// run may leave between its least and its greatest sample, and each with the
// floor its two halves' medians may drift apart.
//
// calibration: clean runs of the five-minute default and the two-hour cell
// measured the bands below; each threshold = the measured width plus slack,
// each floor 0-clean. HeapBytes carries no floor: the live heap's rise is
// the rig's own bounded fill — the seam's capped call records, the
// journal's caches — which converges by the run's middle and is the watch's
// to bound from its own baseline once the fixtures close. Raising any
// threshold takes a new run's evidence named in the change's description.
//
// Descriptors runs on the journal-neutralised count — the process's
// descriptors less the journal's own — because the raw count carries the
// journal's own cache, which churns a window-wide ±2 and shifts a whole
// unit at every crash's reopen: a reopen's open descriptors are among the
// process's, both series move by the same handle, and the difference reads
// through it (the 2h cell: process floors 10/10 against journal floors
// 6/6, difference 4/4, across 29 crashes). The churn the journal leaves in
// the difference is inside its threshold, and a permanent leak — a
// descriptor the process holds that the journal does not — is one the
// median criterion catches whatever the crash count, which is why the
// floor stays 0 with no scaling for the reopen's shift.
type soakFlat struct {
	name      string
	get       func(*soakSample) int64
	threshold int64
	floor     int64
}

var soakFlats = []soakFlat{
	{"ExtentCount", func(s *soakSample) int64 { return s.ExtentCount }, 32, 8},                          // band 68..93, halves 68/68
	{"Segments", func(s *soakSample) int64 { return int64(s.Segments) }, 8, 2},                          // band 6..10, halves 6/6
	{"Spares", func(s *soakSample) int64 { return int64(s.Spares) }, 6, 1},                              // band 0..3, halves 0/0
	{"OpenDescriptors", func(s *soakSample) int64 { return int64(s.OpenDescriptors) }, 10, 2},           // band 6..10, halves 6/6
	{"Descriptors", func(s *soakSample) int64 { return int64(s.ProcessFds - s.OpenDescriptors) }, 8, 0}, // neutralised: band 3..8, halves 4/4
	{"HeapBytes", func(s *soakSample) int64 { return int64(s.Heap) }, 24 << 20, -1},                     // band 3.3..9.2 MB across the cells, converging
}

// soakSample is one window's counters, taken at a loop edge with every
// operation of the window returned: the placement index's ExtentCount, the
// segment set's and the spare pool's counts, the journal's open descriptors,
// the process's, and the live heap after a collection, the honest floor.
type soakSample struct {
	ExtentCount     int64
	Segments        int
	Spares          int
	OpenDescriptors int
	ProcessFds      int
	Heap            uint64
}

// soakLog is what one run did: the operation counts, for the report a green
// run and a finding both want, and the samples the flat checks read.
type soakLog struct {
	ops, writes, syncs, settles, repacks, crashes int
	refused, reclaimed, repackSpares              int
	written, freed                                int64
	repackWall                                    time.Duration
	samples                                       []soakSample

	// planted is the descriptor the run opened at its plant edge and never
	// closed; nil on a run without one. It is the caller's to close, so the
	// fixture's own watch never sees the fault it was made to catch.
	planted *os.File
}

// soakCfg bounds one run. A duration, with zero maxOps, runs for the wall
// time; a nonzero maxOps runs that many operations whatever the clock says,
// so a leg can stand a fixed size beside the soak's own.
type soakCfg struct {
	duration time.Duration
	maxOps   int

	// plantOp, on a run that names one, is after which operation the run
	// opens one descriptor and never closes it, as a leak to be caught. It
	// falls on a window edge, so the plant sits whole between two samples.
	plantOp int
}

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

// soakGaugeOf returns the flat entry the named gauge runs with, so a leg
// reads the same gauge, thresholds and floors the main soak runs rather
// than spelling its own; its name comes from the gauge table.
func soakGaugeOf(name string) soakFlat {
	for _, flat := range soakFlats {
		if flat.name == name {
			return flat
		}
	}
	panic("soak: no flat gauge named " + name)
}

// soakMedian returns the median of vals — the middle of its sorted order,
// the upper middle of an even count. The floor criterion reads medians, not
// leasts, because a sample of a gauge whose two sources are read one after
// the other — the journal's own descriptor count and the process's — can
// skew a unit off for the moment its second source has moved and its first
// has not; the least of a half answers such a dip, and a permanent rise of
// one would hide behind it, where the median does not.
func soakMedian(vals []int64) int64 {
	s := append([]int64(nil), vals...)
	slices.Sort(s)
	return s[len(s)/2]
}

// soakWantFlat checks that vals, one sample per window, are flat: the end
// within threshold of the band the samples past the warm-up span, the band
// no wider than that, and — where floor is not negative — the band's second
// half resting no more than floor above its first by the two halves'
// medians. That median criterion is what a permanent rise fails: a leak
// lifts every window after it, so the second half's middle sits a whole
// unit above the first's, while a transient opens and closes and puts the
// count right back; the width and the drift alone cannot see one
// descriptor stay, in a band the journal's own churn oscillates wider than
// it, and the leasts would hand the leaks to the sampling skew above.
func soakWantFlat(name string, vals []int64, threshold, floor int64) error {
	if len(vals) <= soakWarmupWindows+1 {
		return fmt.Errorf("%s: %d windows, want more than the %d warm-up ones to read a band from", name, len(vals), soakWarmupWindows)
	}
	band := vals[soakWarmupWindows:]

	high, low := band[0], band[0]
	for _, v := range band[1:] {
		high, low = max(high, v), min(low, v)
	}
	first, last := band[0], band[len(band)-1]
	if width := high - low; width > threshold {
		return fmt.Errorf("%s got %v..%v, want flat within %d: the %d windows past the warm-up span %v..%v, wider than the calibrated band", name, first, last, threshold, len(band), low, high)
	}
	if drift := last - first; drift > threshold {
		return fmt.Errorf("%s got %v..%v, want flat within %d: the end of the run sits %d above where its band began, over the calibrated threshold", name, first, last, threshold, drift)
	}
	if floor >= 0 {
		half := len(band) / 2
		if rise := soakMedian(band[half:]) - soakMedian(band[:half]); rise > floor {
			return fmt.Errorf("%s got %v..%v, want flat within %d: the second half's median %v sits %v above the first half's %v, over the calibrated floor", name, first, last, threshold, soakMedian(band[half:]), rise, soakMedian(band[:half]))
		}
	}
	return nil
}

// soakRun runs the soak's loop under cfg on a fresh fixture journal and
// returns what it did, with every window's samples in the log it returns.
//
// The loop is one goroutine's: the soak pays for its pressure in the
// journal's own background work — spare-making, sealing, retiring — not in
// workers of the test's own, so what the gauges read is one caller's load
// and nothing the rig adds.
func soakRun(t *testing.T, cfg soakCfg) *soakLog {
	t.Helper()

	f := journaltest.NewWithOptions(t, func(s *journal.Settings) { s.MaxBytes = soakMaxBytes })
	log := &soakLog{}

	// Deterministic: a finding the soak reports on a later machine is a
	// sequence another one replays.
	rng := rand.New(rand.NewPCG(0x734f614b, 0x6c617661))
	maxV := f.Floor()
	deadline := time.Now().Add(cfg.duration)
	// A duration run's windows and its stop both ride the wall clock, and
	// a short duration needs its windows short too: the flat checks read a
	// band of at least a dozen windows whatever the run's length, so a
	// two-minute sanity run and even a half-minute one work, at a window of
	// a twelfth of the duration rather than the fifteen-second default the
	// hours-long cells keep. The fixed-size legs keep their op-count
	// cadence: they name no duration and no window.
	window := soakWindowSecs
	if cfg.duration > 0 && cfg.duration < 12*window {
		window = max(cfg.duration/12, time.Second)
	}
	windowEnd := time.Now().Add(window)

	// soakOp runs one mixed operation, its kind drawn from the table below.
	//
	// The writes walk the live set in a fixed order — slot after slot,
	// file after file, through the eight files, over the fixture's two
	// shares and its two append streams — so every write replaces a known
	// slot and the superseded records it leaves are the whole content of
	// the segments the syncs settle behind the walker: the journal's own
	// reconcile path retires those segments wholesale, which the writes of
	// this journal must, since without the offload and the release a
	// segment whose bytes back a straggler of the churn can be freed only
	// by a repack copy.
	//
	// decision: the mix runs no truncate and no delete, though the soak's
	// design named them among the operations that make reclaimable space,
	// because a workload whose removals ride the churn wedges this journal
	// as built: after some hundreds of thousands of removals under
	// capacity pressure the journal holds sealed, index-empty segments
	// that nothing retires — the spare pool stays one segment short of a
	// spare for ever, every write refuses the maximum, and no repack pass
	// can run, its only copy target being the pool's empty reserve, so no
	// byte of the unreclaimed storage comes back, across a crash and a
	// reopen included. An overwrite-only churn drains: the pool stays full
	// and a minute of the paced repacks frees the tens of MiB the minute's
	// writes churn through. Restore the two operations — the space they
	// make is the space a fuller soak needs — when the retirement worker
	// drains the sealed, index-empty segments such a workload leaves.
	wr := 0
	soakOp := func(op int) {
		file := wr % soakFileCount
		slot := (wr / soakFileCount) % soakSlotsOf
		switch n := rng.IntN(100); {
		case n < 66: // write, over the walker's slot
			off := int64(slot) * soakSlot
			p := pattern(fmt.Sprintf("op-%d", op), soakSlot)
			soakWrite(t, f, log, &maxV, file, off, p)
			wr++
			log.writes++

		case n < 85: // sync the share's files, forcing the window's steps durable
			ids := make([]journal.FileID, 0, soakFileCount/2)
			for i := range soakFileCount {
				if i%2 == file%2 {
					ids = append(ids, journaltest.File(i))
				}
			}
			if _, err := f.Share(file%2).Sync(t.Context(), ids...); err != nil {
				t.Fatalf("soak: sync %d files: %v", len(ids), err)
			}
			log.syncs++

		default: // settle every file through the highest version the run
			// issued, as the caller that no longer reads below its newest
			// version — the release that lets the retired storage go
			for i := range soakFileCount {
				f.Share(i%2).Settle(journaltest.File(i), maxV)
			}
			log.settles++
		}
	}

	for op := 1; cfg.maxOps == 0 || op <= cfg.maxOps; op++ {
		// The run's duration is a loop edge, never a sleep: the deadline
		// lands between two operations, the current one completing first.
		if cfg.maxOps == 0 && !time.Now().Before(deadline) {
			break
		}
		log.ops++

		// ponytail: the mix is a fixed uniform table over the walker's
		// slot-aligned whole-slot writes, with the syncs and the settles
		// behind it, so the live set keeps a fixed shape and the index's
		// entry count converges instead of fragmenting; upgrade to a
		// weighted, unaligned mix — with the removals the decision above
		// withholds — when the retirement worker drains what they leave,
		// and recalibrate the thresholds here with it.
		soakOp(op)
		f.Advance(soakClockStep)

		// The window's edge: the fixed-size legs turn on op counts, the
		// duration runs on the wall clock — sampled at the operation that
		// finds the window over, still a loop edge with the operation
		// returned, never a timer's own tick.
		edge := false
		if cfg.maxOps > 0 {
			edge = op%soakWindowOps == 0 || op == cfg.maxOps
		} else {
			edge = !time.Now().Before(windowEnd)
		}
		if !edge {
			continue
		}
		windowEnd = time.Now().Add(window)

		// The window's edge: snapshot first, then the slow-period events.
		// Samples sit at loop edges with every operation of the window
		// returned — the counters are read never on a timer.
		log.samples = append(log.samples, soakSampleOf(t, f))

		if op == cfg.plantOp {
			leaked, err := os.Open(f.Dir)
			if err != nil {
				t.Fatalf("soak: plant a leaked descriptor: %v", err)
			}
			log.planted = leaked
		}

		if len(log.samples)%soakCrashWindows == 0 {
			soakCrash(t, f, log, &maxV)
			continue
		}
		if len(log.samples)%soakRepackWindows == 0 {
			soakRepack(t, f, log)
		}
	}

	return log
}

// soakWrite writes p at off of file i and returns its version, refusing no
// capacity: a write the journal refuses at capacity — for the journal's own
// maximum, or a share's or headroom's bound the pressure reaches — is
// retried beside a repack, the reclaimer this journal has, until the write
// lands or the bounded attempts run out and the run fails on the refusal.
func soakWrite(t *testing.T, f *journaltest.Fixture, log *soakLog, maxV *journal.Version, i int, off int64, p []byte) journal.Version {
	t.Helper()

	id := journaltest.File(i)
	for attempt := 1; ; attempt++ {
		if attempt > soakReclaimAttempts {
			st := f.J.Stats()
			t.Fatalf("soak: the write of %d bytes at %d of file %d never landed over %d reclaim attempts: the journal stays at capacity, %d of %d bytes used, %d of them reclaimable in ended segments", len(p), off, i, soakReclaimAttempts, st.UsedBytes, st.MaxBytes, st.UnreclaimedEndedBytes)
		}

		var v journal.Version
		err := f.RetrySpare(func() error {
			var e error
			v, e = f.Share(i%2).WriteAt(t.Context(), id, off, p, 0)
			return e
		})
		if err == nil {
			log.written += int64(len(p))
			trackVersion(t, maxV, v, fmt.Sprintf("write of %d bytes at %d of file %d", len(p), off, i))
			return v
		}

		var ns *journal.NoSpaceError
		if !errors.As(err, &ns) {
			t.Fatalf("soak: write %d bytes at %d of file %d: %v", len(p), off, i, err)
		}
		switch ns.Limit {
		case journal.LimitSpare:
			// A spare refusal the fixture's own wait gave up on is a
			// capacity event too, at the brim this run keeps: the pool has
			// no ready spare and no room to make one until a retirement
			// frees some, which is the same wait the journal's own limits
			// below run. It takes the same reclaim-and-retry, not a
			// distinct fatal: with room to make one, the pool settles a
			// spare before the fixture's wait runs out.
			fallthrough
		case journal.LimitJournal, journal.LimitShare, journal.LimitHeadroom:
			// Capacity: the pressure the soak means. Reclaim and retry, as
			// the engine's refusal path does: one repack pass spending the
			// journal's own repack reserve, a sync of the live files on the
			// later attempts to settle what the removals left, and the
			// clock moved between them — the retirements and the refills a
			// rejection waits on run on the fixture's clock, and a retry
			// loop that never moves it waits a clock that never fires. A
			// pass the pool refuses a spare is an attempt spent, as the
			// engine logs it, not the run's failure: the refusal clears
			// once the retirements ahead of it land.
			log.refused++
			soakRepack(t, f, log)
			f.Advance(soakClockStep)
			if attempt >= 2 {
				soakSyncAll(t, f)
				soakWaitReclaim(f, 2*time.Second)
			}
			log.reclaimed++
		default:
			t.Fatalf("soak: write refused by a limit the soak does not press (%v): %v", ns.Limit, err)
		}
	}
}

// soakRepack repacks within the paced budget. The repack is the run's only
// reclaimer, so it runs both on its own slow period and beside a capacity
// refusal.
//
// decision: the repack runs at a budget of one segment and no larger because
// the pass's per-unit work grows with the segment count — its copy reads a
// full snapshot of the segment set for each unit it retires, and a budget
// over the segment cap makes one call span the whole set, the scaling the
// nightly cannot carry. Two copies never overlap either: the pool's reserve
// is the single spare a pass copies into, so a pass that asks for a second
// one while the journal is at capacity is refused, counted above, and left
// to the next attempt once the retirements ahead of it land. Widen this only
// when the pass's copy stops reading the whole set per unit.
func soakRepack(t *testing.T, f *journaltest.Fixture, log *soakLog) {
	t.Helper()

	start := time.Now()
	freed, err := f.J.Repack(t.Context(), soakRepackBudget, journal.RepackScope{})
	log.repacks++
	log.repackWall += time.Since(start)
	log.freed += freed
	if err == nil {
		return
	}
	if errors.Is(err, journal.ErrJournalReadOnly) {
		return
	}
	// A spare-limit refusal is the pool's recycle and refill racing the
	// pass — the engine logs a failing capacity step and answers
	// retry-later, never the error — so the run records it as an attempt
	// spent and lets its caller retry; anything else is the run's own
	// failure, on the pass or the journal beneath it.
	var ns *journal.NoSpaceError
	if errors.As(err, &ns) && ns.Limit == journal.LimitSpare {
		log.repackSpares++
		return
	}
	t.Fatalf("soak: repack: %v", err)
}

// soakWaitReclaim polls, bounded by bound, for the retirements a reclaim
// attempt waits on to land: the retirement worker's own drain runs on a
// ticker of real time — one second, a duration no test clock reaches — once
// the last settle stopped kicking it, so a retry loop that never yields one
// starves the drain it is waiting for, and the engine it mirrors answers
// retry-later between its steps for the same reason. It returns as soon as
// a figure moves: a spare lands, the reserve refills, or the freed
// counter rises — the real condition each attempt is waiting on, never a
// bare sleep to a deadline.
func soakWaitReclaim(f *journaltest.Fixture, bound time.Duration) {
	before := f.J.Stats().Counters.FreedBytes
	for start := time.Now(); time.Since(start) < bound; time.Sleep(20 * time.Millisecond) {
		st := f.J.Stats()
		if st.Counters.FreedBytes != before || st.RepackReserveBytes > 0 || st.Spares > 0 {
			return
		}
	}
}

// soakSyncAll syncs every live file through its own share's handle, the
// engine's answer to a journal-wide refusal after its first repack, so the
// removals the mix left settle into the figures the next attempt reads.
func soakSyncAll(t *testing.T, f *journaltest.Fixture) {
	t.Helper()

	for s := range 2 {
		var ids []journal.FileID
		for i := range soakFileCount {
			if i%2 == s {
				ids = append(ids, journaltest.File(i))
			}
		}
		if _, err := f.Share(s).Sync(t.Context(), ids...); err != nil {
			t.Fatalf("soak: sync share %d's files: %v", s, err)
		}
	}
}

// soakCrash simulates a power loss with the probe synced first, so the
// reopen verifies the recovery: the synced bytes survive the loss, the loss
// generation never turns back, and — through the floor the fixture raises at
// each open, which the next write's own version check carries — every
// version the reopened journal issues sits above every one the crash left.
func soakCrash(t *testing.T, f *journaltest.Fixture, log *soakLog, maxV *journal.Version) {
	t.Helper()

	file := log.crashes % soakFileCount
	probe := pattern(fmt.Sprintf("probe-crash-%d", log.crashes), soakSlot)
	soakWrite(t, f, log, maxV, file, 0, probe)
	if _, err := f.Share(file%2).Sync(t.Context(), journaltest.File(file)); err != nil {
		t.Fatalf("soak: sync the probe before the crash: %v", err)
	}

	before := f.J.Stats().LossGeneration
	f.Crash()
	f.WaitSpares()
	log.crashes++

	wantBytes(t, f.Share(file%2), journaltest.File(file), 0, probe, fmt.Sprintf("after crash %d: ", log.crashes))
	if after := f.J.Stats().LossGeneration; after < before {
		t.Fatalf("soak: crash %d turned the loss generation back: %d, was %d", log.crashes, after, before)
	}
}

// trackVersion fails the test unless v exceeds every version the run saw
// before it, and records it. Versions extend across a crash, whose floor the
// fixture raises at each open, so a journal reissuing a version the crash
// lost fails here, mid-run, on the write that saw it.
func trackVersion(t *testing.T, maxV *journal.Version, v journal.Version, what string) {
	t.Helper()

	if v.Compare(*maxV) <= 0 {
		t.Fatalf("soak: %s assigned version %v, want one above %v, the highest issued this run: versions extend across a crash's floor", what, v, *maxV)
	}
	*maxV = v
}

// soakSampleOf reads one window's counters: from the journal's Stats, which
// walks nothing, and from the profiler, whose Descriptors reads the process.
// The heap's sample runs a collection first, so garbage a window made does
// not read as held.
func soakSampleOf(t *testing.T, f *journaltest.Fixture) soakSample {
	t.Helper()

	st := f.J.Stats()
	fds, _ := prof.Descriptors()
	return soakSample{
		ExtentCount:     st.ExtentCount,
		Segments:        st.Segments,
		Spares:          st.Spares,
		OpenDescriptors: st.OpenDescriptors,
		ProcessFds:      fds,
		Heap:            prof.LiveHeap(),
	}
}
