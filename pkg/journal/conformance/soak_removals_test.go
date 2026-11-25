//go:build journal_conformance

package conformance

import (
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/journal"
	"github.com/cubbit/dittofs/pkg/journal/journaltest"
)

// soakRemovalOps is the removals leg's size, fixed so every run presses the
// same window of pressure: 4_096 operations, of which 14 in a hundred draw a
// removal, so a run draws ~570 of them. That is far short of the issue's
// hundreds of thousands of removals, but this rig packs the same record
// overlap into a far smaller live set — eight files against a 16 MiB journal
// — and the freeze formed within 800..2_350 operations in every observed
// run, so the count needs no more than room past the slowest of those: it
// keeps the wedge's own variance inside the run while a green journal pays
// the whole count in seconds, and a wedged one ends early — a wedged
// operation ends the churn, so a red run pays mostly the drain that
// judges it.
const soakRemovalOps = 4_096

// soakRemovalCrashOp is the leg's own crash site, on a window edge as the
// leak plants sit: window twelve, the operations finding the middle of the
// 800..2_350 window the freeze formed in, so the churn crosses a power loss
// and a reopen at the depth the wedge used to sit at — beside the cadence
// crashes the leg's windows already carry, the last of which the drain
// reads past.
const soakRemovalCrashOp = soakWindowOps * 12

// soakDrainBound bounds the wait for the drain's retirements. The retire
// worker's ticker runs on real time, one second a tick, which no test clock
// reaches, so the bound covers at least two of its ticks however late the
// drain's settle kicked the worker's pass; a journal that drains exits the
// wait at its own event long before it.
const soakDrainBound = 20 * time.Second

// The drain repack's pacing: a grace window the settle's own retirements
// run in — the retire worker's ticks, one segment a tick — before the leg's
// first pass, and the pass period after it, one bounded repack a second,
// the engine's own retry cadence under a capacity refusal (RFC 1 §6: a
// refusal by the journal's maximum is transient while repack can drain it,
// and the engine answers retry-later for as long as that lasts).
const (
	soakDrainRepackGrace = 6 * time.Second
	soakDrainRepackEvery = time.Second
)

// TestSoakRemovalChurnDrains runs the soak's walker under the removal churn
// — truncates and deletes cut into the overwrite walk, the mix of the
// issue whose wedge this leg holds up — and asserts the issue's done-when:
// once every file is settled through the highest issued version, the retire
// worker has had its ticks, and the drain's own bounded repacks — the
// machinery the engine drives under a capacity refusal — have run at the
// engine's own paced retry, the journal drains. No sealed segment
// in the set snapshot holds on with no index bytes, the walker's write
// lands instead of answering LimitJournal, and the ended segments' unreclaimed
// bytes fall below one segment's cap. A journal that wedges — every write
// answered at capacity for good — ends its churn wedged, and fails here on
// the drain: the sealed, index-empty residue its removals leave is what
// this fingerprint names.
func TestSoakRemovalChurnDrains(t *testing.T) {
	run := soakCfg{maxOps: soakRemovalOps, crashOp: soakRemovalCrashOp}
	log := soakRun(t, run)

	t.Logf("removal churn: %d ops: %d writes, %d truncates, %d deletes, %d wedged operations (of %d capacity refusals), %d syncs, %d settles, %d repacks (%d bytes freed), %d crashes",
		log.ops, log.writes, log.truncates, log.deletes, log.wedged, log.refused, log.syncs, log.settles, log.repacks, log.freed, log.crashes)

	f := log.fixture

	// The drain: every file settled through the highest version the churn
	// issued — the caller's release of every removal marker the walks left,
	// settled nothing else holds drop on — and every file synced, so the
	// retire worker's pass finds the churn's segments all ended.
	for i := range soakFileCount {
		f.Share(i%2).Settle(journaltest.File(i), log.maxV)
	}
	soakSyncAll(t, f)

	repacksBefore := log.repacks
	stuck, unreclaimed := soakDrainWait(t, f, log)
	t.Logf("removal drain: %d repacks spent of the run's %d total",
		log.repacks-repacksBefore, log.repacks)

	if len(stuck) > 0 {
		st := f.J.Stats()
		t.Fatalf("removal churn wedges at capacity: %d sealed segments hold no index bytes after the drain settled every file through version %v beside %d of the worker's ticks: the first, segment %v, sealed, undamaged and unretired, still counted at %d bytes, with %d of %d journal bytes used and %d unreclaimed in ended segments",
			len(stuck), log.maxV, int(soakDrainBound/time.Second), stuck[0].ID, stuck[0].Allocated, st.UsedBytes, st.MaxBytes, unreclaimed)
	}

	// A write lands: the walker's own slot write, straight through the
	// drain the journal just proved — the issue's every-write-answers-
	// LimitJournal is the wedge, and a journal that drained has no answer
	// but a version.
	maxV := log.maxV
	p := pattern("drain-write", soakSlot)
	if _, ok := soakWrite(t, f, log, &maxV, 0, 0, p); !ok {
		st := f.J.Stats()
		t.Fatalf("removal churn wedges at capacity: the drain's settlement write never landed over %d reclaim attempts after every file was settled through version %v: the journal stays at %d of %d bytes used, %d unreclaimed in ended segments", soakReclaimAttempts, log.maxV, st.UsedBytes, st.MaxBytes, st.UnreclaimedEndedBytes)
	}

	if unreclaimed >= journaltest.SegmentCap {
		st := f.J.Stats()
		t.Fatalf("removal churn wedges at capacity: the ended segments keep %d bytes unreclaimed after the drain, want the figure below one segment's %d: the journal sits at %d of %d bytes used", unreclaimed, journaltest.SegmentCap, st.UsedBytes, st.MaxBytes)
	}
}

// soakDrainWait polls the drain's own condition — every sealed segment the
// churn's removals left holding nothing (soakStuckSegments, the same
// predicate the removals crash path gates on), and the ended segments'
// unreclaimed bytes under one segment cap — until it holds, returning the
// last reading either way. Each poll reads the real condition: the retire
// worker runs on a one-second ticker of real time, which the fixture's
// clock cannot fire, and no event of its reaches the conformance package,
// so the poll is the wait and the bound is its backstop — never a sleep a
// deadline waits out. In the wedged case — the only one that pays the whole
// bound — the 20s span at least two of the worker's ticks before the
// assertions read, closing the issue's done-when as it asks; a journal
// that drains exits at the first poll's own event, never at the bound.
//
// decision: the drain runs a bounded repack of its own, one per second
// after a grace window of the settle's own retirements, because the churn
// can leave residue that settle and ticks alone cannot unwind: a mutual
// MayOutrank pair of index-empty segments, each holding removals that stay
// undroppable while the other's noted ranges remain, each's ranges noted
// by records the other holds — a cycle only a carried copy breaks, for the
// class rule stops a source holding once its copy stands, and no settle
// order unwinds it. RFC 1 §6 names the machinery that drains it: the
// capacity refusal is transient while repack drains it, and the engine
// drives a pass under continuing pressure — which is draining at capacity,
// the journal kept going by exactly the machinery the engine drives; a leg
// that demanded a drain with zero repacks would assert a
// stronger property than the design promises. The first pass waits out the
// grace window, so the settle's own one-segment-a-tick routine clears what
// it clears before any pass draws one: the repacks serve the residue the
// ticks could not clear inside the grace, a mutual pair among it — a
// residue larger than the grace's room may draw a pass it could have done
// without, harmless beside the bound. The passes are the run's own bounded
// budget (soakRepack, one megabyte
// a pass, the same budget the churn's paced repacks spend), the pace
// mirrors the engine's retry cadence, and the assertions stay on the end
// state alone: the drain, the tolerances, never a pass count. Making the
// journal repack itself is a design change reserved to the RFC owner and
// is recorded as such, not taken here.
func soakDrainWait(t *testing.T, f *journaltest.Fixture, log *soakLog) (stuck []journal.SegmentInfo, unreclaimed int64) {
	t.Helper()

	next := time.Now().Add(soakDrainRepackGrace)
	for start := time.Now(); ; time.Sleep(20 * time.Millisecond) {
		got := soakStuckSegments(f)
		st := f.J.Stats()
		if len(got) == 0 && st.UnreclaimedEndedBytes < journaltest.SegmentCap {
			return nil, st.UnreclaimedEndedBytes
		}
		if now := time.Now(); now.Sub(start) >= soakDrainBound {
			return got, st.UnreclaimedEndedBytes
		} else if now.After(next) {
			soakRepack(t, f, log)
			next = now.Add(soakDrainRepackEvery)
		}
	}
}
