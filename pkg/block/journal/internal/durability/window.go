package durability

import (
	"slices"
	"time"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/segment"
)

// SyncFailure is the most recent sync failure, and when it happened (R64).
type SyncFailure struct {
	Err error
	// At is the clock's Now when the worker took the failure. A failure the
	// worker's own sync reported is stamped as it returns; a seal's failure is
	// stamped when the worker takes its end event, which the stream hands over
	// only after the segment's reservations and in-flight syncs drained, so At
	// can lag the failure by that drain.
	At time.Time
}

// failStamp is a stream's last sync failure, with the monotonic reading that
// orders it against the other streams' failures.
//
// decision: a failStamp is immutable once stored. The worker is its only
// writer, publishing a fresh one through st.lastFail's atomic pointer, and
// Stats reads that pointer without any lock, so a poll never waits on a lock
// an append takes. Withdrawn if a second goroutine ever stores one.
type failStamp struct {
	SyncFailure
	mono time.Duration
}

// window is a failed segment's records that no sync covered: taken from the
// ledger when the segment's end event reaches the worker (R13, R15, R85).
//
// decision: a window lives under the stream's leaf mutex, st.mu, like the
// ledger its entries came out of: it is read and listed in st.windows only
// with that mutex held, and only the stream's worker creates one (takeWindow,
// or failReadOnly once the journal is read-only) or drops one (resolve). Its
// entries are changed only under st.mu, by two writers: the worker, when
// failWindow or failReadOnly fails them, and Corrupt on a reader's goroutine,
// which takes one entry out holding that entry's file domain. failFile
// therefore re-checks, under the same domain, which of its entries are still
// in a window before it fails them. cause is kept for the code that fails a
// window's records, which reads why the segment ended.
type window struct {
	id      uint64
	seg     core.SegmentID
	cause   segment.EndCause
	err     error   // why the segment ended
	entries []entry // moved from the ledger, in Track order
	// failed is set once failWindow has run for the window: its content
	// records are dropped and the generation raised, or it holds none and
	// needed no raise. A header-only record answers a Sync's failure only
	// after that.
	failed bool
	// readOnly marks a window failReadOnly made of a segment's unsynced
	// records once the journal turned read-only: the segment did not fail,
	// so resolving the window announces nothing and keeps the segment's
	// failure state as it is.
	readOnly bool
}

// onEnd is the consumer of every stream's end events (D13). It runs on the
// stream's notifier with no stream lock held, so it only queues the event for
// the stream's worker and wakes it: no clock, no domain, no blocking.
func (d *Durability) onEnd(ev segment.EndEvent) {
	d.mu.Lock()
	st := d.made[ev.Stream]
	d.mu.Unlock()
	if st == nil {
		return
	}
	st.mu.Lock()
	st.ends = append(st.ends, ev)
	st.mu.Unlock()
	st.wake()
}

// takeWindows takes the window of every end event queued for the stream. It
// runs on the worker, before the completion pass, so a record a sync covered
// is completed there and only the uncovered ones are the window. It takes the
// events queued when it starts; one queued meanwhile waits for the next pass.
// Each event stays queued until takeWindow makes its window in the critical
// section that pops it, so Corrupt sees the segment failed throughout.
func (d *Durability) takeWindows(st *stream) {
	st.mu.Lock()
	n := len(st.ends)
	st.mu.Unlock()
	for range n {
		// Only the worker pops, so the head is the event peeked here.
		st.mu.Lock()
		ev := st.ends[0]
		st.mu.Unlock()
		if d.beforeTakeWindow != nil {
			d.beforeTakeWindow()
		}
		d.takeWindow(st, ev)
	}
}

// removeLocked drops the ledger entries drop selects, in Track order, and
// hands each to take, which may be nil. It is the one compaction of the
// ledger, shared by the completion pass and the window: it recounts stamped
// over what stays, so the stamped prefix stays the entries the worker has
// stamped, and clears the references the tail still holds. st.mu must be held.
func (st *stream) removeLocked(drop func(*entry) bool, take func(entry)) {
	kept := st.ledger[:0]
	stamped := 0
	for i := range st.ledger {
		e := &st.ledger[i]
		if drop(e) {
			if take != nil {
				take(*e)
			}
			continue
		}
		kept = append(kept, *e)
		if i < st.stamped {
			stamped++
		}
	}
	clear(st.ledger[len(kept):])
	st.ledger, st.stamped = kept, stamped
}

func (d *Durability) takeWindow(st *stream, ev segment.EndEvent) {
	// A seal's failed sync is a sync failure no worker sync reported, so it is
	// counted here, unless the worker already recorded a failure for the
	// segment: a seal that finds that failure ends with EndSeal too, and the
	// one fsync failure is counted once. The clock is read before the lock.
	// An append's end counts nothing, and a requested end is the failure the
	// worker counted when it asked.
	//
	// decision: every EndSeal the worker holds no failure for is counted as a
	// sync failure. A seal ends its segment mostly through a failed sync, and
	// the sync failure is the count's subject (R62, R64). It is known to
	// overcount: the stream also ends a segment with EndSeal when a write
	// fails while a seal drains, or when the seal-marker write fails, and
	// EndEvent cannot tell these from a failed sync, so sync_failures and
	// LastSyncError can then show a write error. Withdrawn once EndEvent says
	// whether the cause was a sync, which the stream would have to add, and
	// only that case is counted. The same ceiling covers ErrBadCatalog and
	// ledger refusals, which end with EndSeal too. So sync_failures counts
	// seal fsyncs that syncs does not: a sync of an ended segment issues no
	// fsync yet its seal counts one (Syncs=0, SyncFailures=1), and the two
	// counters count different sets.
	// sync_failures also differs from RFC 1's "syncs issued, and failed" row:
	// it counts a seal's failure the worker never issued. The RFC row should
	// say the same; that is a follow-up for the RFC, not for this code.
	var failedAt time.Time
	var failedMono time.Duration
	if ev.Cause == segment.EndSeal {
		failedAt, failedMono = d.clock.Now(), d.clock.Elapsed()
	}

	st.mu.Lock()
	// The event leaves the queue in the step that makes its window.
	st.ends = slices.Delete(st.ends, 0, 1)
	// decision: the window is the entries no sync has covered when the worker
	// takes the event, and no Synced report for the failed segment is refused
	// in the gap between the failure and this point, because none can arrive:
	// a failed sync freezes the segment's tracker under the append domain
	// before the failure is returned (syncFail), and a failed write freezes the
	// prefix at the record, so no later sync of that segment is reported
	// (syncDone is false) and no report can cover a record the failure took.
	// Every report made before the failure was ordered before it under the
	// same domain and covers records that are rightly not in the window. A
	// test that injects a report into the sink after the failure is making one
	// the stream never would. Withdrawn if the tracker stops freezing, or a
	// second source of Synced reports exists: the sink then refuses a report
	// for a segment in st.failed. A read-only journal's sink refuses every
	// report, but failReadOnly then fails every record no applied report
	// covered, this window's included, so a refused report leaves no record
	// unanswered.
	w := &window{id: d.windowSeq.Add(1), seg: ev.Segment, cause: ev.Cause, err: ev.Err}
	st.removeLocked(func(e *entry) bool {
		return e.w.Loc.Segment == ev.Segment && !st.covered(e.w.Loc)
	}, func(e entry) { w.entries = append(w.entries, e) })
	first, dup := st.failed[ev.Segment]
	if dup {
		// The sync's own error stands over the one a seal found after it,
		// which the segment's records are reported with.
		w.err = first
	} else {
		st.failed[ev.Segment] = ev.Err
	}
	_, wasCounted := st.counted[ev.Segment]
	delete(st.counted, ev.Segment)
	delete(st.ending, ev.Segment)
	st.windows = append(st.windows, w)
	// A file whose record of the segment Corrupt took out of the ledger before
	// this window was taken is told of the window as if it had failed it.
	for _, f := range st.owed[ev.Segment] {
		st.markLocked(w, f)
	}
	delete(st.owed, ev.Segment)
	if ev.Cause == segment.EndSeal && !dup && !wasCounted {
		st.failures.Add(1)
		st.lastFail.Store(&failStamp{SyncFailure: SyncFailure{Err: ev.Err, At: failedAt}, mono: failedMono})
	}
	st.d.notifyAll()
	empty := len(w.entries) == 0
	st.mu.Unlock()

	if ev.Cause == segment.EndRequested {
		d.log.Debug("journal segment ended", "stream", st.no, "segment", ev.Segment, "cause", ev.Cause, "err", ev.Err)
	} else {
		d.log.Error("journal segment ended", "stream", st.no, "segment", ev.Segment, "cause", ev.Cause, "err", ev.Err)
	}
	if empty {
		// Nothing is held from the segment, so there is nothing to fail.
		d.resolve(st, w)
	}
	// A stopped stream is a refused one, whatever stopped it, and takes no
	// further segment, so no later event reaches here.
	if ev.Stopped && d.readOnly != nil {
		d.readOnly(ev.Err)
	}
}

// resolve marks the window's records no longer pending: it drops the window
// and the failure kept for its segment once nothing else names it, then
// announces the segment through Config.Resolved (R19), unless copies in the
// segment are held, whose hold then announces it when it ends. It runs on the
// worker.
//
// decision: a window is resolved when it holds no record: at once when it is
// taken with none, or when failWindow has failed its last record. While the
// journal takes writes, a window holding a record that failWindow leaves, a
// header-only one, stays pending and is not announced, and every Sync naming a
// file with such a record answers a *core.SyncError, until the code that
// re-appends it lands, or until the journal turns read-only, which fails every
// record of every window and so resolves them all. A pending window never
// reads durable, so nothing is lost by waiting; what is withdrawn is only the
// window's memory. A window failReadOnly made is resolved without an
// announcement: its segment did not fail.
//
// A record failWindow dropped has left the window and the ledger, which would
// read it durable: st.lost keeps it out of that answer, and failWindow adds it
// there in the same critical section that removes it from the window.
func (d *Durability) resolve(st *stream, w *window) {
	st.mu.Lock()
	st.windows = slices.DeleteFunc(st.windows, func(o *window) bool { return o == w })
	delete(st.told, w.id)
	if w.readOnly {
		st.d.notifyAll()
		st.mu.Unlock()
		return
	}
	held := false
	for i := range st.ledger {
		held = held || st.ledger[i].w.Loc.Segment == w.seg
	}
	for _, o := range st.windows {
		held = held || o.seg == w.seg
	}
	if !held {
		delete(st.failed, w.seg)
	}
	// A segment copies are in use from stays unresolved: the hold's end
	// announces it.
	withheld := st.withholdLocked(w.seg)
	st.d.notifyAll()
	st.mu.Unlock()
	if !withheld && d.resolved != nil {
		d.resolved(st.no, w.seg)
	}
}
