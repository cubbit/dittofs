//go:build journal_conformance

package conformance

import (
	"errors"
	"fmt"
	"io/fs"
	"math/rand/v2"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/cubbit/dittofs/internal/prof"
	"github.com/cubbit/dittofs/pkg/journal"
	segset "github.com/cubbit/dittofs/pkg/journal/internal/segment"
	"github.com/cubbit/dittofs/pkg/journal/journaltest"
)

// The soak's journal: one fixture, small in every gauge it watches, so hours
// of pressure fit a temporary directory. Segments keep the fixture's own
// 1 MiB cap; the maximum drops from the fixture's 64 MiB to 16 MiB, whole
// segments, so the run's writes meet the journal's limit long before the
// disk does, and the space this run reclaims it makes with overwrites — the
// superseded halves of a churned live set — and the repack that frees them.
//
// decision: no operation of the mix runs an offload or a release, though the
// soak's load names both beside the writes: neither call is wired into the
// journal yet, so no mix could run them. The rule holds until the offload
// and the release are wired into the journal — the one change that restores
// them to the mix, alongside the space they alone can reclaim, and with it
// this marker goes; until then the writes' superseded live set and the
// removals' settled segments are the only sources of reclaimable space the
// journal answers for.
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
	// the main run: a fourth of a minute, a two-hour band reading 1,800
	// samples and a five-minute calibration reading twenty. A duration shorter
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
	// loss and the reopen that follows: one crash every eight windows, a
	// slow period beside the writes that carries the run — short enough that
	// even a sanity-length run crosses one and has its recovery checked,
	// which the reach assertions below require.
	soakCrashWindows = 8

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

	// soakOrphanBytes is the planted footprint leak: eight segments' worth,
	// over the churn the directory's own spare-making and crashing swings
	// (two to three segments a window), so what the plant adds is a rise the
	// byte total's floor sees above the noise. That noise is the gauge's
	// honest sensitivity - a single orphaned segment rides inside it, and
	// only a drift of two or three accumulates past it - which is why the
	// plant stands for a leak's accumulation, not its first segment.
	soakOrphanBytes = 8 << 20

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
// calibration: three clean runs of the five-minute default under the mixed
// mix measured the bands below — the two-hour cell re-measures them on its
// first nightly under it; each threshold = the measured width plus slack,
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
	// Mixed-mix band 28..64 against the overwrite mix's 68..93, over the
	// three measured runs: the removals keep no steady entry count — a
	// delete empties a file the settle spends and the next write re-holds —
	// so the width, not the level, is what moved; the halves' medians fall
	// (49/41, 47/46, 40/50) with the churn's own cuts, so the floor of 8
	// held clean, and a leak keeps its rise against that floor either way.
	{"ExtentCount", func(s *soakSample) int64 { return s.ExtentCount }, 40, 8},
	// Mixed-mix band 4..13 against the overwrite mix's 6..10, over the three
	// measured runs: the removals hand the retire worker segments a tick at
	// a time, so the count dips while it drains a window's residue and
	// climbs while the next window piles one. Halves' medians 10/6, 6/6 and
	// 6/7 — a fall, never a rise — so the floor of 2 held clean.
	{"Segments", func(s *soakSample) int64 { return int64(s.Segments) }, 10, 2},
	{"Spares", func(s *soakSample) int64 { return int64(s.Spares) }, 6, 1},                              // band 0..2, halves 2/2: held
	{"OpenDescriptors", func(s *soakSample) int64 { return int64(s.OpenDescriptors) }, 12, 2},           // rides the segment set: band 4..13, halves 10/6
	{"Descriptors", func(s *soakSample) int64 { return int64(s.ProcessFds - s.OpenDescriptors) }, 8, 0}, // neutralised: band 4..4, halves 4/4: held
	{"HeapBytes", func(s *soakSample) int64 { return int64(s.Heap) }, 24 << 20, -1},                     // band 3.3..5.2 MB, converged
	// Mixed-mix band 9..16.7 MiB against the overwrite mix's 10.5..13.6,
	// over the three measured runs: the churn rides the journal's maximum
	// and a crash's reopen drops what the loss removed, so the width takes
	// the crash's own quantum — one segment — over the measured 7.3; the
	// halves' medians stayed flat or fell in every run, so the 2 MiB floor
	// held, and its two segments of slack are the accounting's own quantum,
	// so a rise of more than that is a drift the books never gave back
	{"UsedBytes", func(s *soakSample) int64 { return s.UsedBytes }, 9 << 20, 2 << 20},
	{"DirFiles", func(s *soakSample) int64 { return int64(s.DirFiles) }, 8, -1}, // band 10..17 through the churn: held
	// No floor on the count: a reopen's spare-making swings it wider than
	// one leaked file, so the count's flatness is a width-and-drift bound
	// alone, and the byte total below is what leaked storage answers to.
	// Mixed-mix band 9.4..16.8 MiB, held: a crash's reopen drops the
	// directory under either half's median and never lifts one, so the
	// floor still sees a leak from the planted five segments up, while the
	// width bound stays loose against the crash churn on purpose: the
	// floor, not the width, is the criterion that sees a leak
	{"DirBytes", func(s *soakSample) int64 { return s.DirBytes }, 16 << 20, 2 << 20},
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

	// UsedBytes is the journal's accounted local storage, and DirFiles and
	// DirBytes the files and bytes its directory holds on the real
	// filesystem - the footprint the disk answers for, as distinct from the
	// heap above: a retire path that drops a segment from the journal's
	// books and never unlinks its file answers to these two, not to the
	// heap and not, alone, to the accounting.
	UsedBytes int64
	DirFiles  int
	DirBytes  int64
}

// soakLog is what one run did: the operation counts, for the report a green
// run and a finding both want, and the samples the flat checks read.
type soakLog struct {
	ops, writes, syncs, settles, repacks, crashes  int
	refused, reclaimed, repackSpares, repackClaims int
	written, freed                                 int64
	repackWall                                     time.Duration
	samples                                        []soakSample

	// truncates and deletes are the removals the mix drew, and wedged the
	// operations the churn could not land over its bounded reclaim attempts —
	// a journal whose writes answer the limits for good, which ends the loop
	// so the caller's checks judge it: the removals leg holds its drain up
	// to the refusal, and the other legs fail on the wedge itself. Zero on a
	// healthy journal.
	truncates, deletes, wedged int

	// maxV is the highest version the run issued, and fixture the journal
	// it issued them on, still open: a caller's after-the-run checks — the
	// removals leg's drain — settle and judge the same journal the loop
	// churned. The zero values on a run that never wrote.
	maxV    journal.Version
	fixture *journaltest.Fixture

	// planted is the descriptor the run opened at its plant edge and never
	// closed; nil on a run without one. It is the caller's to close, so the
	// fixture's own watch never sees the fault it was made to catch.
	planted *os.File

	// orphan is the file the run wrote into the journal's directory at its
	// plant edge and never removed, as a footprint leak to be caught; empty
	// on a run without one. The temp directory's own cleanup removes it.
	// The name is plain prose, not the journal's .seg or .spare.tmp shapes,
	// so a stray in a debug session or a directory diff reads as the
	// harness's own, never as the journal's: the open tolerates a stray
	// whatever it is named — probed with three segment-shaped files planted
	// beside a live journal, the reopen after a crash ran on unattached
	// (RejectedSegments unmoved) and writes continued — so nothing about
	// the plant's survival across the leg's crashes hangs on the name, and
	// the leak this plant stands for — storage the journal stopped
	// accounting and never gave back — is the same leak whatever the
	// stray's name is.
	orphan string
}

// soakCfg bounds one run. A duration, with zero maxOps, runs for the wall
// time; a nonzero maxOps runs that many operations whatever the clock says,
// so a leg can stand a fixed size beside the soak's own.
type soakCfg struct {
	duration time.Duration
	maxOps   int

	// crashOp, on a run that names one, crashes the journal after that many
	// operations — the probe, the power loss and the reopen soakCrash runs
	// — on the window edge the plant below sits on, beside the crash
	// cadence the standing soak runs on. Zero names none.
	crashOp int

	// plantOp, on a run that names one, is after which operation the run
	// plants its leak, on a window edge so the plant sits whole between two
	// samples. The leak is one descriptor, opened and never closed, unless
	// plantDisk is set, when it is one unaccounted file instead.
	plantOp   int
	plantDisk bool
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
	log := &soakLog{fixture: f}

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
	wr := 0
	// The mix carves the removals' fourteen out of the walk first — eight
	// truncates and six deletes, the operations whose space the retire
	// worker and the drain must see come back — then the walker's writes,
	// syncs and settles over the remaining eighty-six: writes below 71 of
	// a hundred, syncs below 86, settles over the rest.
	writeCut, syncCut := 71, 86
	soakOp := func(op int) (landed bool) {
		file := wr % soakFileCount
		slot := (wr / soakFileCount) % soakSlotsOf
		switch n := rng.IntN(100); {
		case n < 8: // truncate, ~8%: the walker's file cut at
			// its next slot boundary — the one the walker's next write of
			// the file lands at — so the cut always takes a tail the walk
			// refills from, keeping the live set's shape.
			cut := int64(slot) * soakSlot
			if !soakRemove(t, f, log, &maxV, fmt.Sprintf("truncate at %d", cut), file, func() (journal.Version, error) {
				return f.Share(file%2).Truncate(t.Context(), journaltest.File(file), cut)
			}) {
				return false
			}
			log.truncates++

		case n < 14: // delete, ~6%: the walker's file whole.
			// Delete stops holding every extent of the id and leaves a
			// removal marker until the settle path settles it; the id
			// itself stays exactly as usable as it was — the walker's next
			// write of the file re-holds its slot, the marker's settle is
			// the mix's own settle branch — so the walker keeps to the same
			// eight files and no id rotation or re-creation is needed.
			if !soakRemove(t, f, log, &maxV, "delete", file, func() (journal.Version, error) {
				return f.Share(file%2).Delete(t.Context(), journaltest.File(file))
			}) {
				return false
			}
			log.deletes++

		case n < writeCut: // write, over the walker's slot
			off := int64(slot) * soakSlot
			p := pattern(fmt.Sprintf("op-%d", op), soakSlot)
			if _, ok := soakWrite(t, f, log, &maxV, file, off, p); !ok {
				return false
			}
			wr++
			log.writes++

		case n < syncCut: // sync the share's files, forcing the window's steps durable
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
		return true
	}

	for op := 1; cfg.maxOps == 0 || op <= cfg.maxOps; op++ {
		// The run's duration is a loop edge, never a sleep: the deadline
		// lands between two operations, the current one completing first.
		if cfg.maxOps == 0 && !time.Now().Before(deadline) {
			break
		}
		log.ops++

		// ponytail: the mix is a fixed uniform table over the walker's
		// slot-aligned whole-slot writes, with the truncates and the
		// deletes cut in ahead of them and the syncs and the settles
		// behind it, so the live set keeps a fixed shape and the index's
		// entry count converges instead of fragmenting. The flat bands
		// below carry this mix's own calibration, measured once the
		// removals joined it; upgrade to a weighted, unaligned mix only
		// for a load-shaped question, and recalibrate the bands with it.
		if !soakOp(op) {
			// A wedged operation: the journal refused it over every
			// bounded reclaim, so the churn it can no longer land ends
			// here — the loop stops, never the run: the removals leg's
			// drain judges the refusal, and the other legs fail on the
			// wedge itself.
			break
		}
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
			if cfg.plantDisk {
				name := filepath.Join(f.Dir, "orphan-soak")
				// Written whole and closed, as a remnant the journal never
				// owned again: the leak is the file's presence, not any
				// handle held on it, so the bytes count the moment the
				// write returns.
				if err := os.WriteFile(name, pattern("orphan", soakOrphanBytes), 0o644); err != nil {
					t.Fatalf("soak: plant an orphaned file: %v", err)
				}
				log.orphan = name
			} else {
				leaked, err := os.Open(f.Dir)
				if err != nil {
					t.Fatalf("soak: plant a leaked descriptor: %v", err)
				}
				log.planted = leaked
			}
		}

		if op == cfg.crashOp {
			// The run's named crash, planted on a window edge the same
			// way the leak plants above: the removals leg sites it at the
			// middle of the window its freeze formed in, so every green
			// run carries a reopen at that depth before the drain reads.
			soakCrash(t, f, log, &maxV)
			continue
		}
		if len(log.samples)%soakCrashWindows == 0 {
			soakCrash(t, f, log, &maxV)
			continue
		}
		if len(log.samples)%soakRepackWindows == 0 {
			soakRepack(t, f, log)
		}
	}

	log.maxV = maxV
	return log
}

// soakWrite writes p at off of file i and returns its version, refusing no
// capacity: a write the journal refuses at capacity — for the journal's own
// maximum, or a share's or headroom's bound the pressure reaches — is
// retried beside a repack, the reclaimer this journal has, until the write
// lands or the bounded attempts run out. There the write counts wedged and
// returns unlanded, so the churn it can no longer land ends and the caller
// judges the journal that refused it — the removals leg by its drain, the
// other legs by failing on the wedge itself.
func soakWrite(t *testing.T, f *journaltest.Fixture, log *soakLog, maxV *journal.Version, i int, off int64, p []byte) (journal.Version, bool) {
	t.Helper()

	id := journaltest.File(i)
	for attempt := 1; ; attempt++ {
		if attempt > soakReclaimAttempts {
			log.wedged++
			return journal.Version{}, false
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
			return v, true
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

// soakRemove runs one removal of the walker's file — remove calls it, on a
// truncate or a delete — with the reclaim-and-retry the writes take under
// capacity, because a removal's record draws on the reserved headroom and is
// refused once that headroom is exhausted, the ledger never credited back
// before a repack. A removal no bounded reclaim relieves counts wedged and
// returns false, as a write's refusal does: the churn ends and the caller
// judges the journal that refused it.
func soakRemove(t *testing.T, f *journaltest.Fixture, log *soakLog, maxV *journal.Version, what string, i int, remove func() (journal.Version, error)) bool {
	t.Helper()

	for attempt := 1; ; attempt++ {
		if attempt > soakReclaimAttempts {
			log.wedged++
			return false
		}

		var v journal.Version
		err := f.RetrySpare(func() error {
			var e error
			v, e = remove()
			return e
		})
		if err == nil {
			trackVersion(t, maxV, v, fmt.Sprintf("%s of file %d", what, i))
			return true
		}

		var ns *journal.NoSpaceError
		if !errors.As(err, &ns) {
			t.Fatalf("soak: %s of file %d: %v", what, i, err)
		}
		switch ns.Limit {
		case journal.LimitSpare:
			// As a write's: a spare refusal the fixture's own wait gave up
			// on is a capacity event, taking the same reclaim-and-retry.
			fallthrough
		case journal.LimitHeadroom, journal.LimitJournal, journal.LimitShare:
			// Capacity, the pressure the removals churn the same way the
			// writes do. Reclaim and retry beside the repack the headroom's
			// ledger answers to, and the sync settling the removals already
			// landed on the later attempts.
			log.refused++
			soakRepack(t, f, log)
			f.Advance(soakClockStep)
			if attempt >= 2 {
				soakSyncAll(t, f)
				soakWaitReclaim(f, 2*time.Second)
			}
			log.reclaimed++
		default:
			t.Fatalf("soak: the %s of file %d was refused by a limit the soak does not press (%v): %v", what, i, ns.Limit, err)
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
	// spent and lets its caller retry.
	var ns *journal.NoSpaceError
	if errors.As(err, &ns) && ns.Limit == journal.LimitSpare {
		log.repackSpares++
		return
	}
	// decision: a claim refusal spends the attempt rather than the run: the
	// retire worker runs on real time the wall-clock runs outlive, and its
	// pass holds a segment's claim while it retires the very unit this pass
	// selected. The ledger's claim table refuses a second taker by design
	// and calls its own answer uncounted and retryable — the claim's holder
	// releases it without retiring when refused — which is how the retire
	// queue answers it (a requeued id) and how the engine answers any failed
	// repack step (logged, the refusal kept for retry). The overlap needs a
	// run the worker's real-time ticks reach inside; a fixed-size leg ends
	// before its first tick, so the overwrite-only calibration never met it,
	// while the removals hand the worker segments to retire at every tick of
	// a long run. Withdraw for a claim the journal never gives back — which
	// the removals leg's drain and the standing runs' flat bands read in
	// their own assertions anyway — never for one the worker holds and
	// releases.
	if errors.Is(err, segset.ErrClaimed) {
		log.repackClaims++
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

// soakStuckSegments reads the set snapshot and returns the sealed segments
// the churn's removals stranded: sealed, undamaged, unretired segments the
// index holds no bytes for — the freeze's own shape, one reputation the
// drain asserts on and the removals crash path gates its pool wait on. A
// retired damaged row and a segment still holding live index bytes are
// answers to other questions, so neither counts.
func soakStuckSegments(f *journaltest.Fixture) []journal.SegmentInfo {
	var stuck []journal.SegmentInfo
	for _, s := range f.J.Inspect().Segments() {
		if s.Sealed && !s.Damaged && !s.Retired && s.HeldBytes == 0 && s.RetainedBytes == 0 {
			stuck = append(stuck, s)
		}
	}
	return stuck
}

// soakCrash simulates a power loss with the probe synced first, so the
// reopen verifies the recovery: the synced bytes survive the loss, the loss
// generation never turns back, and — through the floor the fixture raises at
// each open, which the next write's own version check carries — every
// version the reopened journal issues sits above every one the crash left.
//
// The probe write is a write of the mix's own, so it can land wedged; a
// probe that never landed is no probe, and the crash is spent — skipped,
// counted among the wedged ops, never a crash of content it could not
// write — while the caller's checks judge the refusal that spent it.
func soakCrash(t *testing.T, f *journaltest.Fixture, log *soakLog, maxV *journal.Version) {
	t.Helper()

	file := log.crashes % soakFileCount
	probe := pattern(fmt.Sprintf("probe-crash-%d", log.crashes), soakSlot)
	if _, ok := soakWrite(t, f, log, maxV, file, 0, probe); !ok {
		// decision: a wedged probe spends the crash rather than the run:
		// a write the journal refused over its whole bounded reclaim made
		// no content to lose, so the crash over it would verify a
		// recovery of nothing. The wedge is counted among the run's wedged
		// ops, which the caller judges — the removals leg's drain by its
		// assertion, the other legs by failing on the wedge itself.
		return
	}
	if _, err := f.Share(file%2).Sync(t.Context(), journaltest.File(file)); err != nil {
		t.Fatalf("soak: sync the probe before the crash: %v", err)
	}

	before := f.J.Stats().LossGeneration
	f.Crash()
	// decision: the pool's settle waits after the crash only while no
	// sealed, index-empty segment stands in the set. A wedged journal —
	// the pool one spare short for good — answers the wait with the pool's
	// refusal, which reads as the fixture's own fatal and ends the run
	// before the caller that judges the wedge could name the state it
	// saw: the removals leg's drain by its assertion, the other legs by
	// their wedge refusal. A journal without one — every drained run —
	// waits at the soak's full strength.
	if len(soakStuckSegments(f)) == 0 {
		f.WaitSpares()
	}
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
// no wider than that (the drift read in absolute value - a fall is as
// much drift as a rise, and the accounting that drops a segment its
// directory still holds is the down direction of the same leak), and —
// where floor is not negative — the band's second half resting no more
// than floor above its first by the two halves' medians. That median criterion is what a permanent rise fails: a leak
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
	if drift := max(last-first, first-last); drift > threshold {
		return fmt.Errorf("%s got %v..%v, want flat within %d: the end of the run sits %d from where its band began, over the calibrated threshold", name, first, last, threshold, drift)
	}
	if floor >= 0 {
		half := len(band) / 2
		if rise := soakMedian(band[half:]) - soakMedian(band[:half]); rise > floor {
			return fmt.Errorf("%s got %v..%v, want flat within %d: the second half's median %v sits %v above the first half's %v, over the calibrated floor", name, first, last, threshold, soakMedian(band[half:]), rise, soakMedian(band[:half]))
		}
	}
	return nil
}

// soakSampleOf reads one window's counters: from the journal's Stats, which
// walks nothing, and from the profiler, whose Descriptors reads the process.
// The heap's sample runs a collection first, so garbage a window made does
// not read as held.
func soakSampleOf(t *testing.T, f *journaltest.Fixture) soakSample {
	t.Helper()

	st := f.J.Stats()
	fds, _ := prof.Descriptors()

	// The directory's real files and bytes, the footprint the disk answers
	// for: read off the real filesystem beside the journal's own accounting,
	// so a divergence between the two books is the sample's to hold. A name
	// that vanished between the listing and its stat is churn the window
	// caught mid-move - a spare renamed into the set, a retire's unlink - and
	// counts for nothing: what is not on the disk any more is not footprint.
	files, bytes := int64(0), int64(0)
	entries, err := os.ReadDir(f.Dir)
	if err != nil {
		t.Fatalf("soak: read the journal's directory: %v", err)
	}
	for _, e := range entries {
		info, err := e.Info()
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			t.Fatalf("soak: stat %s: %v", e.Name(), err)
		}
		files++
		bytes += info.Size()
	}

	return soakSample{
		ExtentCount:     st.ExtentCount,
		Segments:        st.Segments,
		Spares:          st.Spares,
		OpenDescriptors: st.OpenDescriptors,
		ProcessFds:      fds,
		Heap:            prof.LiveHeap(),
		UsedBytes:       st.UsedBytes,
		DirFiles:        int(files),
		DirBytes:        bytes,
	}
}
