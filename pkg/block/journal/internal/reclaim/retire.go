package reclaim

import (
	"sync"
	"time"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/segment"
)

// Gate is what a candidate asks of the durability service: whether the
// causality record of each location a mover recorded alongside its offer is
// published — the offer stands once every record it names is durable — and
// whether an ended segment's window is open or its reclamation is blocked,
// the two facts an ended candidate re-reads under its claim. It is the
// interface this package declares and the durability service implements.
type Gate interface {
	Durable(segment.Location) bool
	WindowOpen(core.SegmentID) bool
	ReclaimBlocked(core.SegmentID) bool
}

// Executor is the retirement executor the worker hands a segment to: one id,
// the reason the segment's state names. The segment executor's own queue and
// worker take it from there.
type Executor interface {
	Enqueue(core.SegmentID, segment.Reason)
}

// Worker turns reclamation candidates into retirements. Two kinds of offer
// reach it: Emptied, a mover's word that a segment was emptied, carrying the
// causality record's location; Prompt, the tracker's candidate callback and
// the tick scan's find, carrying nothing. One queue entry exists per id —
// a later offer of a queued segment only appends what it carries — and a
// single goroutine drains the queue, running one pass over it per wake.
//
// Per candidate, under the claim it takes from the segment set: the tracker's
// Held is re-checked, the gate is asked for every recorded location, the
// reason is read from the ledger's state — Sealed hands over as NotHeld,
// Ended as ReasonEnded, anything else drops — an ended candidate re-reads
// the window and the reclaim block, a prompt-only candidate waits out one
// tick since it was first offered, and the claim is released before the
// hand-over to the executor. A candidate a check sends back keeps its entry
// and is re-offered by the tick.
type Worker struct {
	segs *segment.Set
	tr   *Tracker
	gate Gate
	exec Executor

	// mu guards the queue below. It is the one lock Emptied and Prompt
	// take, and it is never held across a Set, Tracker, Gate or Executor
	// call.
	mu      sync.Mutex
	entries map[core.SegmentID]*offer
	order   []core.SegmentID

	kick      chan struct{}
	done      chan struct{}
	stop      sync.Once
	wg        sync.WaitGroup
	started   bool
	stopped   bool
	tickEvery time.Duration // the re-offer and scan rhythm; a test shortens it

	// afterClaim runs right after the worker's claim of a candidate,
	// whatever the claim answered; set only from tests, before Start.
	afterClaim func(core.SegmentID)
}

// offer is one queued candidate: the segment, the locations its movers
// recorded — each one a causality record the gate must answer published
// before the retirement may run — and when the segment was first offered,
// the timestamp a prompt-only candidate's one-tick wait measures from.
type offer struct {
	id    core.SegmentID
	locs  []segment.Location
	first time.Time
}

// NewWorker returns a worker draining tr's candidates into exec. A nil gate
// disables the location, window and block checks and exists for tests only;
// production passes one. A nil set, tracker or executor is a caller bug and
// panics.
func NewWorker(segs *segment.Set, tr *Tracker, gate Gate, exec Executor) *Worker {
	if segs == nil {
		panic("reclaim: a worker needs its segment set")
	}
	if tr == nil {
		panic("reclaim: a worker needs its tracker")
	}
	if exec == nil {
		panic("reclaim: a worker needs its executor")
	}
	return &Worker{
		segs:      segs,
		tr:        tr,
		gate:      gate,
		exec:      exec,
		entries:   make(map[core.SegmentID]*offer),
		kick:      make(chan struct{}, 1),
		done:      make(chan struct{}),
		tickEvery: time.Second,
	}
}

// Emptied is the mover's offer: its last thing left seg, and at is the
// location of the causality record naming the crossing — the one fact the
// gate is asked about before the segment may be retired. The location is
// appended to the segment's entry's list, founding the entry when none
// existed. It may be called from any context, including the set's observer
// delivery: it takes the queue lock alone and touches nothing else.
func (w *Worker) Emptied(seg core.SegmentID, at segment.Location) {
	w.mu.Lock()
	if w.stopped {
		w.mu.Unlock()
		return
	}
	e := w.entries[seg]
	if e == nil {
		e = &offer{id: seg, first: time.Now()}
		w.entries[seg] = e
		w.order = append(w.order, seg)
	}
	e.locs = append(e.locs, at)
	w.mu.Unlock()
	w.kickWorker()
}

// Prompt is the candidate callback and the scan's find: a segment that may
// now hold nothing, offered with no location. A segment already queued gains
// nothing but the wake. It runs wherever its caller does — the tracker fires
// the candidate from inside its own release paths, which the set's seal
// observer's delivery can reach while that delivery holds its lock — so it
// takes the queue lock alone and touches nothing else.
func (w *Worker) Prompt(seg core.SegmentID) {
	w.mu.Lock()
	if w.stopped {
		w.mu.Unlock()
		return
	}
	if _, ok := w.entries[seg]; !ok {
		w.entries[seg] = &offer{id: seg, first: time.Now()}
		w.order = append(w.order, seg)
	}
	w.mu.Unlock()
	w.kickWorker()
}

// Start launches the worker, idempotent; after Stop it is a no-op. Offers
// collected before Start are drained by the first pass.
func (w *Worker) Start() {
	w.mu.Lock()
	if w.started || w.stopped {
		w.mu.Unlock()
		return
	}
	w.started = true
	w.wg.Add(1)
	go w.worker()
	w.mu.Unlock()
	w.kickWorker()
}

// Stop stops the worker and joins it. Offers that reach the worker after
// Stop drop, and the queue it held is dropped with it rather than drained
// — the same ceiling the executor's own Stop decision names: a not-held
// segment is re-handed at the next open's scan, so draining it can only
// spend storage time at close.
func (w *Worker) Stop() {
	w.stop.Do(func() {
		w.mu.Lock()
		defer w.mu.Unlock()
		w.stopped = true
		close(w.done)
	})
	w.wg.Wait()
}

// worker drains the queue: one goroutine, woken by a coalesced kick or by
// the tick, running one pass per wake and, on each tick, the scan before
// the pass.
func (w *Worker) worker() {
	defer w.wg.Done()

	t := time.NewTicker(w.tickEvery)
	defer t.Stop()
	for {
		select {
		case <-w.done:
			return
		case <-w.kick:
			w.pass()
		case <-t.C:
			w.scan()
			w.pass()
		}
	}
}

// scan is the ticker's backstop: every segment the ledger holds Sealed or
// Ended and nothing holds, left alone by a pass if an entry for it already
// exists. The ledger's claim cannot be read without taking it, so a segment
// another holder claims is refused by the pass below rather than here.
//
// ponytail: the tick scan walks the ledger's whole snapshot each tick,
// O(sealed segments) per tick. Upgrade to something narrower when the walk
// shows in a profile.
func (w *Worker) scan() {
	for _, r := range w.segs.Snapshot() {
		if r.State != segment.Sealed && r.State != segment.Ended {
			continue
		}
		if w.tr.Held(r.ID) {
			continue
		}
		w.mu.Lock()
		if w.stopped {
			w.mu.Unlock()
			return
		}
		if _, ok := w.entries[r.ID]; !ok {
			w.entries[r.ID] = &offer{id: r.ID, first: time.Now()}
			w.order = append(w.order, r.ID)
		}
		w.mu.Unlock()
	}
	w.kickWorker()
}

// pass runs one drain: every queued candidate once. A Stop that lands
// mid-pass leaves the rest of the queue with the worker, like the executor's
// own pass.
func (w *Worker) pass() {
	w.mu.Lock()
	if w.stopped {
		w.mu.Unlock()
		return
	}
	ids := w.order
	w.order = nil
	w.mu.Unlock()

	for _, id := range ids {
		select {
		case <-w.done:
			return
		default:
		}
		// The entry is snapshot under the queue lock — a mover's location
		// can append while the pass runs — and nothing else decides on its
		// behalf: what the claim below holds is what the passes read.
		w.mu.Lock()
		e := w.entries[id]
		var locs []segment.Location
		var first time.Time
		if e != nil {
			locs = append(locs, e.locs...)
			first = e.first
		}
		w.mu.Unlock()
		if e == nil {
			continue
		}
		w.handle(id, locs, first)
	}
}

// handle is one candidate's pass, on locs and first as the pass read them.
// Its refusals in order: the claim (someone else won the segment), the
// tracker's Held re-checked under the claim, the gate's answer for each
// recorded location, the segment's state, the ended window and block
// re-read, and the prompt-only tick wait. A refusal drops the entry or sends
// it back; the hand-over releases the claim and hands the segment to the
// executor.
func (w *Worker) handle(id core.SegmentID, locs []segment.Location, first time.Time) {
	claimed := w.segs.Claim(id)
	if w.afterClaim != nil {
		w.afterClaim(id)
	}
	if !claimed {
		w.drop(id)
		return
	}
	// Every path below holds the claim and releases it before returning.

	// The tracker's own contract: a candidacy is not a fact, the crossing
	// can race back, so Held is re-checked under the claim and a segment
	// held again is let go until the next crossing.
	if w.tr.Held(id) {
		w.segs.Release(id)
		w.drop(id)
		return
	}

	if w.gate != nil {
		for _, at := range locs {
			if !w.gate.Durable(at) {
				w.segs.Release(id)
				w.requeue(id)
				return
			}
		}
	}

	var reason segment.Reason
	switch st, err := w.segs.State(id); {
	case err != nil:
		// The segment left the set since the offer: someone else retired
		// it, and there is nothing to hand over.
		w.segs.Release(id)
		w.drop(id)
		return
	case st == segment.Sealed:
		reason = segment.NotHeld
	case st == segment.Ended:
		reason = segment.ReasonEnded
		if w.gate != nil && (w.gate.WindowOpen(id) || w.gate.ReclaimBlocked(id)) {
			// The claim held across the re-read is what serialises this
			// against the copy hold that flips the window.
			w.segs.Release(id)
			w.requeue(id)
			return
		}
	default:
		// Neither of the two states a retirement serves: the offer named
		// a segment a stream still appends to, and the entry is wrong to
		// carry it further.
		w.segs.Release(id)
		w.drop(id)
		return
	}

	if len(locs) == 0 && time.Since(first) < w.tickEvery {
		// decision: a prompt-only candidate waits until a full tick has
		// passed since it was first offered, because a causality record of
		// a bytes-move publishes before the crossing it names, and the
		// sync bound covers every published record within one tick — and
		// the wait exists because a mover's Emptied offer can race the
		// tick scan that found the segment. Overturned by any path that
		// empties a sealed segment without publishing a record the sync
		// bound covers within one tick.
		w.segs.Release(id)
		w.requeue(id)
		return
	}

	w.drop(id)
	// ponytail: storage is freed in whole segments only — neither retirement
	// nor repack frees any byte range inside a segment while it remains —
	// so every reclaimed byte costs a copy of the live records sharing its
	// segment. The upgrade is freeing storage at release for whole records
	// only, never part of one.
	//
	// decision: the claim is released before the hand-over because no claim
	// takeover exists — the executor claims through the ledger's own flag,
	// and a segment handed over still claimed would come back refused-claimed
	// for ever. The executor's queue treats that refusal as retryable, so
	// the release race is covered on its side.
	w.segs.Release(id)
	w.exec.Enqueue(id, reason)
}

// drop forgets a queued candidate: handed over, or wrong to carry further.
func (w *Worker) drop(id core.SegmentID) {
	w.mu.Lock()
	delete(w.entries, id)
	w.mu.Unlock()
}

// requeue puts a refused candidate back for the next pass, keeping its
// recorded locations and its first offer; the entry above it stays until
// something hands it over or drops it.
func (w *Worker) requeue(id core.SegmentID) {
	w.mu.Lock()
	if _, ok := w.entries[id]; ok {
		w.order = append(w.order, id)
	}
	w.mu.Unlock()
}

// kickWorker wakes the worker, coalescing.
func (w *Worker) kickWorker() {
	select {
	case w.kick <- struct{}{}:
	default:
	}
}
