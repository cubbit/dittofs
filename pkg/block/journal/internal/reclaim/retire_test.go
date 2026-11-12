package reclaim

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/format"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/index"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/segment"
	"github.com/cubbit/dittofs/pkg/block/journal/journaltest/kit"
)

// retireFixture is one journal's retirement periphery over a recorded storage
// seam: a real ledger, a real maker, a real retirer started on its own
// worker, an index and a tracker of it, and the channel the retirer's
// retired-segment hook reports each completed retirement on. The worker under
// test is built per test, over these facts.
type retireFixture struct {
	ix      *index.Index
	set     *segment.Set
	seam    *kit.Seam
	tr      *Tracker
	retirer *segment.Retirer
	retired chan core.SegmentID
	// last is the largest segment id a Maker drew over the seam, so a later
	// spare draws above it instead of over its name.
	last core.SegmentID
}

// newRetireFixture builds the periphery and starts the retirer's worker. The
// retirer stops with the test.
func newRetireFixture(t *testing.T) *retireFixture {
	t.Helper()
	seam, err := kit.NewSeam(t.TempDir())
	if err != nil {
		t.Fatalf("the seam could not be made: %v", err)
	}
	set := segment.NewSet(segCap, nil)
	maker := segment.NewMaker(seam.FS(), set, 1<<62, 0, nil)
	cache := segment.NewDescriptors(seam.FS(), format.SegmentName, core.NewDescriptorBudget(64))
	retired := make(chan core.SegmentID, 256)
	r := segment.NewRetirer(segment.RetirerConfig{
		Set:     set,
		Cache:   cache,
		Maker:   maker,
		FS:      seam.FS(),
		Retired: func(id core.SegmentID) { retired <- id },
	})
	r.Start()
	t.Cleanup(r.Stop)
	ix := index.New(1 << 20)
	return &retireFixture{
		ix:      ix,
		set:     set,
		seam:    seam,
		tr:      New(ix, nil),
		retirer: r,
		retired: retired,
	}
}

// takeIn makes one more spare over the fixture's seam and takes it into the
// fixture's set: a real further segment, Active and holding nothing.
func (f *retireFixture) takeIn(t *testing.T) core.SegmentID {
	t.Helper()
	maker := segment.NewMaker(f.seam.FS(), f.set, 1<<62, f.last, nil)
	seg, err := maker.Make()
	if err != nil {
		t.Fatalf("the maker could not make a spare: %v", err)
	}
	if err := f.set.Take(seg); err != nil {
		t.Fatalf("Take: %v", err)
	}
	f.last = seg
	return seg
}

// sealed takes one fresh segment in and seals it early: Sealed, at its cap,
// holding nothing.
func (f *retireFixture) sealed(t *testing.T) core.SegmentID {
	t.Helper()
	seg := f.takeIn(t)
	if err := f.set.Seal(seg, false); err != nil {
		t.Fatalf("Seal: %v", err)
	}
	return seg
}

// ended takes one fresh segment in and ends it: Ended, holding nothing.
func (f *retireFixture) ended(t *testing.T) core.SegmentID {
	t.Helper()
	seg := f.takeIn(t)
	if err := f.set.End(seg); err != nil {
		t.Fatalf("End: %v", err)
	}
	return seg
}

// worker builds the worker under test over the fixture with a fast tick
// (the retirer's own rhythm is one second; a test shortens it, as the
// executor's comment allows) and starts it. gate may be nil, which the
// worker's contract keeps for mechanics tests.
func (f *retireFixture) worker(t *testing.T, gate Gate, exec Executor) *Worker {
	t.Helper()
	w := NewWorker(f.set, f.tr, gate, exec)
	w.tickEvery = 10 * time.Millisecond
	t.Cleanup(w.Stop)
	w.Start()
	return w
}

// at64 is one causality record's location inside a segment: offset 64, the
// first record slot, some width. The values are the gate's to read, nothing
// more.
func at64(seg core.SegmentID) segment.Location {
	return segment.Location{Segment: seg, Off: 64, Len: 16}
}

// testGate is the tests' hand on the gate the worker asks: the knobs an
// atomic setter flips while the worker runs, and a refusal channel the
// methods report each "not yet" answer on. The knob is an input to the code
// under test, set per test like an injected clock.
type testGate struct {
	// refuseAll answers not-yet-durable for every recorded location.
	refuseAll atomic.Bool
	// refuseOff answers not-yet-durable for exactly the location recorded
	// at this offset, zero disabled.
	refuseOff atomic.Int64
	// window answers WindowOpen true while set.
	window atomic.Bool
	// blocked answers ReclaimBlocked true while set.
	blocked atomic.Bool
	// refusals receives one report per refused check, from the worker
	// goroutine; the test drains what its assertions count.
	refusals chan string
}

func (g *testGate) Durable(loc segment.Location) bool {
	if g.refuseAll.Load() || (g.refuseOff.Load() != 0 && g.refuseOff.Load() == loc.Off) {
		g.refuse("durable")
		return false
	}
	return true
}

func (g *testGate) WindowOpen(core.SegmentID) bool {
	if g.window.Load() {
		g.refuse("window")
		return true
	}
	return false
}

func (g *testGate) ReclaimBlocked(core.SegmentID) bool {
	if g.blocked.Load() {
		g.refuse("blocked")
		return true
	}
	return false
}

func (g *testGate) refuse(what string) {
	select {
	case g.refusals <- what:
	default:
	}
}

// recordedExec hands each call to the executor it wraps and counts them: the
// requirement these tests assert on is which hand-overs reach the executor,
// and a wrapper that records the call and passes it through, never answering
// in its place, keeps that record.
type recordedExec struct {
	next Executor
	mu   sync.Mutex
	n    int
}

func (e *recordedExec) Enqueue(id core.SegmentID, reason segment.Reason) {
	e.mu.Lock()
	e.n++
	e.mu.Unlock()
	e.next.Enqueue(id, reason)
}

func (e *recordedExec) count() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.n
}

// oneRec is one header-only write record registered live of seg, so the
// tracker holds the segment: the registration the tests place between an
// offer and the worker's pass.
func oneRec(seg core.SegmentID) Rec {
	return Rec{
		Kind: format.KindWrite, File: fid(3), Seg: seg,
		Ext:  core.Extent{Off: 0, Len: 4096},
		Pair: index.Numbers{Version: ver(2), Seq: 1}, Seq: 1,
	}
}

// TestEmptiedRetiresANotHeldSealedSegment: the mover's offer of a fresh,
// not-held, sealed segment is claimed, re-checked, gated and handed over as
// a NotHeld retirement: the retirer reports one retirement, the freed
// counter rises by the segment's accounted size, the ledger's snapshot drops
// the segment, and nothing counts a double retire.
func TestEmptiedRetiresANotHeldSealedSegment(t *testing.T) {
	f := newRetireFixture(t)
	w := f.worker(t, nil, f.retirer)
	seg := f.sealed(t)

	w.Emptied(seg, at64(seg))
	select {
	case id := <-f.retired:
		if id != seg {
			t.Fatalf("Emptied retired segment %v, want %v", id, seg)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("Emptied of a not-held sealed segment: no retirement arrived, want segment %v", seg)
	}

	for _, row := range f.set.Snapshot() {
		if row.ID == seg {
			t.Fatalf("the retired segment %v is still in the ledger's snapshot, want it gone", seg)
		}
	}
	if got, want := f.retirer.Counters().FreedBytes, uint64(segCap); got != want {
		t.Fatalf("FreedBytes after one retirement = %d, want %d", got, want)
	}
	if got := f.set.Counters().DoubleRetires; got != 0 {
		t.Fatalf("DoubleRetires = %d, want 0: one offer, one retirement", got)
	}
}

// TestANotYetDurableLocationRequeuesUntilTheGateOpens: a location the gate
// answers not-yet-durable keeps its entry alive — the same location is asked
// twice, which only a requeued entry can do (a requeue keeps the entry, and
// a scan's prompt carries no location to ask about) — and no hand-over
// reaches the executor while the gate refuses. Once the gate opens, the
// re-offer retires the segment.
func TestANotYetDurableLocationRequeuesUntilTheGateOpens(t *testing.T) {
	f := newRetireFixture(t)
	gate := &testGate{refusals: make(chan string, 256)}
	gate.refuseAll.Store(true)
	exec := &recordedExec{next: f.retirer}
	w := NewWorker(f.set, f.tr, gate, exec)
	w.tickEvery = 10 * time.Millisecond
	t.Cleanup(w.Stop)
	w.Start()
	seg := f.sealed(t)

	w.Emptied(seg, at64(seg))
	for i := 1; i <= 2; i++ {
		select {
		case what := <-gate.refusals:
			if what != "durable" {
				t.Fatalf("the gate reported refusal %q, want a durable refusal", what)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("the %d. durable refusal never arrived, want the not-yet-durable offer to requeue and be asked again", i)
		}
	}
	if got := exec.count(); got != 0 {
		t.Fatalf("Enqueue called %d times while the gate refused, want 0", got)
	}

	gate.refuseAll.Store(false)
	w.Emptied(seg, at64(seg))
	select {
	case id := <-f.retired:
		if id != seg {
			t.Fatalf("after the gate opened, retired segment %v, want %v", id, seg)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("after the gate opened, no retirement arrived, want segment %v", seg)
	}
	if got, want := f.retirer.Counters().FreedBytes, uint64(segCap); got != want {
		t.Fatalf("FreedBytes after the re-offer's retirement = %d, want %d", got, want)
	}
}

// TestHeldRecheckUnderTheClaimDropsTheCandidate: a live-record registration
// landing between the offer and the worker's pass is caught by the Held
// re-check under the claim — the candidate is dropped, nothing reaches the
// executor — and the record's release is what makes the segment offerable
// again, by the tick scan's fresh prompt rather than the dropped entry.
func TestHeldRecheckUnderTheClaimDropsTheCandidate(t *testing.T) {
	f := newRetireFixture(t)
	exec := &recordedExec{next: f.retirer}
	// Collected before Start, so the record is registered between the offer
	// and the first pass, exactly as a registration that raced the offer.
	w := NewWorker(f.set, f.tr, nil, exec)
	w.tickEvery = 10 * time.Millisecond
	t.Cleanup(w.Stop)
	seg := f.sealed(t)

	rec := oneRec(seg)
	f.tr.Register(rec)
	w.Emptied(seg, at64(seg))
	w.Start()

	// Bounding window, not a truth: no hand-over may reach the executor
	// while the record is registered, and the worker ticks fast enough
	// that several passes ran inside it.
	select {
	case id := <-f.retired:
		t.Fatalf("retired segment %v while its record was registered, want no retirement", id)
	case <-time.After(300 * time.Millisecond):
	}
	if got := exec.count(); got != 0 {
		t.Fatalf("Enqueue called %d times for a segment held again, want 0", got)
	}
	if got, want := f.retirer.Counters().FreedBytes, uint64(0); got != want {
		t.Fatalf("FreedBytes while the segment was held = %d, want %d", got, want)
	}

	// The release leaves the segment not held; the scan offers it and the
	// retirement lands, proving the entry's drop left nothing stalled.
	f.tr.Unregister(rec)
	select {
	case id := <-f.retired:
		if id != seg {
			t.Fatalf("retired segment %v after the record's release, want %v", id, seg)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("no retirement after the record's release, want segment %v", seg)
	}
}

// TestAPromptOnlyCandidateWaitsOneTickALocatedOneDoesNot: a prompt-only
// candidate waits until a full tick has passed since it was first offered —
// with a tick far longer than any test wait, it never retires — while a
// candidate that gains a location meanwhile is handed over on the next pass,
// tick or not, because the location's gate answer replaces the wait.
func TestAPromptOnlyCandidateWaitsOneTickALocatedOneDoesNot(t *testing.T) {
	f := newRetireFixture(t)
	w := NewWorker(f.set, f.tr, nil, f.retirer)
	w.tickEvery = time.Hour
	t.Cleanup(w.Stop)
	w.Start()
	seg := f.sealed(t)

	w.Prompt(seg)
	// Bounding window, not a truth: no tick has passed, so no prompt-only
	// hand-over may happen.
	select {
	case id := <-f.retired:
		t.Fatalf("retired segment %v before one tick, want the backstop to hold the prompt-only candidate", id)
	case <-time.After(300 * time.Millisecond):
	}

	w.Emptied(seg, at64(seg))
	select {
	case id := <-f.retired:
		if id != seg {
			t.Fatalf("retired segment %v, want %v", id, seg)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("a candidate with a recorded location was not handed over without a tick, want segment %v retired", seg)
	}
}

// TestTheTickScanOffersWhatNothingHolds: the tick scan offers a sealed
// segment nothing offered by hand, and its retirement lands after the
// backstop's tick; a sealed segment whose record is registered is skipped by
// the same scan and never handed over.
func TestTheTickScanOffersWhatNothingHolds(t *testing.T) {
	f := newRetireFixture(t)
	f.worker(t, nil, f.retirer)
	seg := f.sealed(t)
	held := f.sealed(t)
	heldRec := oneRec(held)
	f.tr.Register(heldRec)

	select {
	case id := <-f.retired:
		if id != seg {
			t.Fatalf("the scan retired segment %v, want %v", id, seg)
		}
	case <-time.After(3 * time.Second):
		t.Fatalf("the tick scan never offered the untouched sealed segment %v, want its retirement", seg)
	}

	// Bounding window, not a truth: several scans ran inside it, and each
	// must skip the segment whose record is registered.
	select {
	case id := <-f.retired:
		t.Fatalf("retired segment %v whose record is registered, want no retirement", id)
	case <-time.After(300 * time.Millisecond):
	}
	if got, want := f.retirer.Counters().FreedBytes, uint64(segCap); got != want {
		t.Fatalf("FreedBytes with one segment freed = %d, want %d: the held segment must not be freed", got, want)
	}
}

// TestAnEndedCandidateWaitsForTheWindowToClose: an ended candidate re-reads
// the window and the reclaim block under the claim, and neither hand-over
// nor the executor is reached while the window answers open; once it closes,
// the segment is handed over as a ReasonEnded retirement — the executor
// refuses a wrong reason, so the retirement arriving at all proves the
// reason matched the Ended state.
func TestAnEndedCandidateWaitsForTheWindowToClose(t *testing.T) {
	f := newRetireFixture(t)
	gate := &testGate{refusals: make(chan string, 256)}
	gate.window.Store(true)
	exec := &recordedExec{next: f.retirer}
	w := f.worker(t, gate, exec)
	seg := f.ended(t)

	w.Prompt(seg)
	select {
	case what := <-gate.refusals:
		if what != "window" {
			t.Fatalf("the gate reported refusal %q, want a window refusal", what)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("the window was never asked, want the ended candidate to re-read it under the claim")
	}
	// Bounding window, not a truth: the window answers open and the worker
	// ticks fast enough that several re-reads ran inside it.
	select {
	case id := <-f.retired:
		t.Fatalf("retired segment %v while its window answered open, want no retirement", id)
	case <-time.After(300 * time.Millisecond):
	}
	if got := exec.count(); got != 0 {
		t.Fatalf("Enqueue called %d times while the window answered open, want 0", got)
	}

	gate.window.Store(false)
	select {
	case id := <-f.retired:
		if id != seg {
			t.Fatalf("retired segment %v once the window closed, want %v", id, seg)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("no retirement once the window closed, want segment %v", seg)
	}
	if got, want := f.retirer.Counters().FreedBytes, uint64(segCap); got != want {
		t.Fatalf("FreedBytes after the ended segment's retirement = %d, want %d", got, want)
	}
}

// TestTwoWorkersRetireOneSegmentOnce: two workers over one set, tracker and
// executor, both offered the same segment, retire it exactly once: the
// claim a hand-over takes refuses the other's, the executor's queue keeps
// one entry per id, and the ledger counts no double retire.
func TestTwoWorkersRetireOneSegmentOnce(t *testing.T) {
	f := newRetireFixture(t)
	w1 := f.worker(t, nil, f.retirer)
	w2 := f.worker(t, nil, f.retirer)
	seg := f.sealed(t)

	w1.Emptied(seg, at64(seg))
	w2.Emptied(seg, at64(seg))
	select {
	case id := <-f.retired:
		if id != seg {
			t.Fatalf("retired segment %v, want %v", id, seg)
		}
	case <-time.After(3 * time.Second):
		t.Fatalf("two workers on one segment: no retirement arrived, want segment %v", seg)
	}
	w1.Stop()
	w2.Stop()

	// The join has happened, so any second retirement is already in the
	// channel; a full receive here would be a second handing-over.
	select {
	case id := <-f.retired:
		t.Fatalf("a second retirement of segment %v arrived, want exactly one", id)
	default:
	}
	if got := f.set.Counters().DoubleRetires; got != 0 {
		t.Fatalf("DoubleRetires = %d, want 0: the claim must keep one hand-over per segment", got)
	}
	if got, want := f.retirer.Counters().FreedBytes, uint64(segCap); got != want {
		t.Fatalf("FreedBytes after two offers = %d, want %d: the segment is freed once", got, want)
	}
	for _, row := range f.set.Snapshot() {
		if row.ID == seg {
			t.Fatalf("the segment %v is still in the ledger's snapshot, want it gone", seg)
		}
	}
}

// TestARefusedClaimDropsTheEntryALaterPromptReoffers: a candidate whose
// claim another holder refuses is dropped, its recorded location with it —
// the gate keeps refusing that exact location, so a requeued entry would
// never retire — and a later Prompt offers the segment again fresh, with no
// location, and the retirement lands.
func TestARefusedClaimDropsTheEntryALaterPromptReoffers(t *testing.T) {
	f := newRetireFixture(t)
	gate := &testGate{refusals: make(chan string, 256)}
	gate.refuseOff.Store(64) // the recorded location stays not-yet-durable forever
	w := NewWorker(f.set, f.tr, gate, f.retirer)
	w.tickEvery = 10 * time.Millisecond
	t.Cleanup(w.Stop)
	seg := f.sealed(t)

	if !f.set.Claim(seg) {
		t.Fatalf("Claim: the external claim could not take the segment")
	}
	claimed := make(chan struct{})
	var once sync.Once
	w.afterClaim = func(core.SegmentID) { once.Do(func() { close(claimed) }) }
	w.Start()

	w.Emptied(seg, at64(seg))
	select {
	case <-claimed:
	case <-time.After(2 * time.Second):
		t.Fatalf("the worker never ran its pass over the refused claim, want the pass to have claimed and been refused")
	}

	// The external claim gone, the later prompt re-offers the segment with
	// no location of the still-refused kind; had the first entry been
	// requeued instead of dropped, its location would keep the gate
	// refusing for ever and no retirement would arrive.
	f.set.Release(seg)
	w.Prompt(seg)
	select {
	case id := <-f.retired:
		if id != seg {
			t.Fatalf("retired segment %v, want %v", id, seg)
		}
	case <-time.After(3 * time.Second):
		t.Fatalf("the later Prompt did not re-offer the dropped candidate, want segment %v's retirement", seg)
	}
	if got := f.set.Counters().DoubleRetires; got != 0 {
		t.Fatalf("DoubleRetires = %d, want 0", got)
	}
}

// TestStopJoinsAndLaterOffersDrop: Stop joins the worker's goroutine, offers
// after it drop, and the executor is never reached — the freed counter stays
// zero, the segment stays in the ledger, and no retirement is completed.
func TestStopJoinsAndLaterOffersDrop(t *testing.T) {
	f := newRetireFixture(t)
	exec := &recordedExec{next: f.retirer}
	w := NewWorker(f.set, f.tr, nil, exec)
	w.tickEvery = 10 * time.Millisecond
	w.Start()
	w.Stop() // returning at all is Stop joining, within the test's own timeout

	seg := f.sealed(t)
	w.Emptied(seg, at64(seg))
	w.Prompt(seg)
	// Bounding window, not a truth: the offers dropped synchronously inside
	// Stop, so this only guards against a pass that escaped the join.
	select {
	case id := <-f.retired:
		t.Fatalf("retired segment %v after Stop, want the offers dropped", id)
	case <-time.After(200 * time.Millisecond):
	}
	if got := exec.count(); got != 0 {
		t.Fatalf("Enqueue called %d times after Stop, want 0: the executor was reached past a joined worker", got)
	}
	if got, want := f.retirer.Counters().FreedBytes, uint64(0); got != want {
		t.Fatalf("FreedBytes after Stop = %d, want %d", got, want)
	}
	found := false
	for _, row := range f.set.Snapshot() {
		if row.ID == seg {
			found = true
		}
	}
	if !found {
		t.Fatalf("the segment %v left the ledger without a retirement, want it still in the set", seg)
	}
	if got := f.set.Counters().DoubleRetires; got != 0 {
		t.Fatalf("DoubleRetires = %d, want 0", got)
	}
}

// TestPromptFiredFromTheSealObserverWhileARetirementCompletes: the candidate
// fires from inside the set's seal observer — whose delivery the set holds a
// lock across the whole callback — while the worker completes another
// segment's retirement concurrently, whose unlink needs the very lock the
// delivery holds. The queue-only candidate and the claim's release order
// keep this a wait, never a deadlock: every round's two segments retire, and
// the ledger counts no double retire at the end.
func TestPromptFiredFromTheSealObserverWhileARetirementCompletes(t *testing.T) {
	const rounds = 15
	f := newRetireFixture(t)
	var w *Worker
	tr := New(f.ix, func(seg core.SegmentID) { w.Prompt(seg) })
	tr.SetMember(func(seg core.SegmentID) bool {
		_, err := f.set.State(seg)
		return err == nil
	})
	w = NewWorker(f.set, tr, nil, f.retirer)
	w.tickEvery = 10 * time.Millisecond
	t.Cleanup(w.Stop)
	f.set.OnSeal(tr.SealObserved)
	w.Start()

	for round := 0; round < rounds; round++ {
		ended := f.takeIn(t)
		if err := f.set.End(ended); err != nil {
			t.Fatalf("End: %v", err)
		}
		copies := f.takeIn(t)
		tr.HoldEnded(ended, copies, 10)

		// The mover's offer racing the seal: the worker retires this
		// segment while the observer below fires the candidate.
		other := f.sealed(t)
		w.Emptied(other, at64(other))

		if err := f.set.Seal(copies, true); err != nil {
			t.Fatalf("Seal of the copies' segment: %v", err)
		}

		// Collect until both of the round's segments retired. Segments of
		// earlier rounds (the copies, sealed holding nothing) are re-handed
		// by later ticks' scans and tolerated here.
		want := map[core.SegmentID]struct{}{ended: {}, other: {}}
		for len(want) > 0 {
			select {
			case id := <-f.retired:
				delete(want, id)
			case <-time.After(2 * time.Second):
				t.Fatalf("retirements stalled inside the seal observer's round; still waiting for %v", want)
			}
		}
	}
	if got := f.set.Counters().DoubleRetires; got != 0 {
		t.Fatalf("DoubleRetires after %d rounds = %d, want 0", rounds, got)
	}
}
