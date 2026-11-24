package reclaim

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"

	"github.com/cubbit/dittofs/pkg/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/journal/internal/index"
	"github.com/cubbit/dittofs/pkg/journal/internal/segment"
)

// Durable waits for one record's place to become durable: the durability
// layer's own waiters. WaitDurable starts a sync for a record that has none
// and follows a re-appended record to its new place, so it serves the carry's
// header-only records, whose failed window re-appends them. WaitCopy serves
// a copy instead: a failed window re-appends a content record to a place
// the pass cannot name, for its move names the copy's own place, or fails
// its re-read and drops it, and either way the copy the pass holds is dead,
// so the wait answers the failure the window settled with rather than
// publish the copy at a place a failed window abandoned.
type Durable interface {
	WaitDurable(at segment.Location) error
	WaitCopy(at segment.Location) error
}

// Retirer is the retirement executor a pass hands its sources to, one at a
// time under the claim the pass has just released: the segment executor's own
// queue is the retirement worker's path, never a pass's — Retire runs a
// segment's retirement in the pass's own goroutine.
type Retirer interface {
	Retire(id core.SegmentID, reason segment.Reason) (segment.Retirement, error)
}

// Outcome is what one pass did: how many units it ran, the payload bytes its
// copies moved and the storage its retirements returned. The units a pass ran
// count once each, whatever each ended in: freed stays zero for a unit whose
// source it did not retire.
type Outcome struct {
	Segments int
	Bytes    int64
	Freed    int64
}

// Pass is the repack pass: units of one source segment each — select, copy,
// carry, wait for the copies to be durable, repoint each file's pieces in one
// index step, end the deferrals the unit's carry owes, and hand the source to
// the retirer — bounded by a budget of copied bytes and driven by the caller
// (RFC 1 §7.2). A Pass is safe for concurrent use: two Run calls claim their
// sources through the selector, which never hands one segment to two.
type Pass struct {
	sel  *Selector
	cop  *Copier
	tr   *Tracker
	ix   *index.Index
	set  *segment.Set
	dur  Durability
	wait Durable
	ret  Retirer
	// end ends one deferred loss of a segment — the registry entry as the
	// carry's wiring mapped it out — reporting whether an entry left; acct
	// hands a repoint's held delta to the counter that consumes it. Either
	// may be nil: the pass ends no deferral, accounts no delta.
	end  func(core.SegmentID, DeferredLoss) bool
	acct func(index.HeldDelta)
	// res bounds the pass's copying by the repack reserve and counts each
	// copy to it while its source remains.
	res Reserve
	// seal closes a share's active segments before a share-scoped pass
	// selects; nil seals nothing. Set once at the construction.
	seal *Sealer

	// decision: mu is a leaf. It guards the stop flag and the run count's
	// admission alone — spent once to refuse a Run begun after Stop and to
	// keep Stop's join from racing a late wg.Add — and nothing is called
	// while it is held: the interrupt and the join it orders both run once
	// it is released, so no lock of the journal's is ever taken under it.
	mu      sync.Mutex
	stopped bool
	wg      sync.WaitGroup
}

// PassConfig carries every piece a Pass is built over, so the constructor
// takes one argument whatever the wiring grows: the selector claims the
// candidates the units run over, the copier copies and carries under the same
// tracker the retirement worker serves, Durability is the service the ended
// segments re-read, Durable the waiter the copies publish through, Retirer
// the retirements' executor, EndDeferral the deferral-end wiring and Account
// the held-delta consumer, Reserve the repack reserve's bound and accounting,
// and Sealer the sealer a share-scoped run closes the share's active segments
// with. Durability, EndDeferral and Account may be nil — a pass over a journal
// that carries no deferral needs none of them, and the tests that drive the
// pieces alone pass nil — and so may Sealer, which leaves every scope sealing
// nothing. The rest are a caller bug and panic in the constructor.
//
// decision: the sealer is a field of the one construction rather than a second
// call the caller must order — the sealer depends on nothing the pass holds,
// so the old "before the pass is shared" rule guarded a race that could not
// arise unless a caller raced its own construction. Overturned by a sealer
// that ever needs a piece the config does not already carry.
type PassConfig struct {
	Selector    *Selector
	Copier      *Copier
	Tracker     *Tracker
	Index       *index.Index
	Set         *segment.Set
	Durability  Durability
	Durable     Durable
	Retirer     Retirer
	EndDeferral func(core.SegmentID, DeferredLoss) bool
	Account     func(index.HeldDelta)
	Reserve     Reserve
	Sealer      *Sealer
}

// NewPass builds the pass over cfg.
func NewPass(cfg PassConfig) *Pass {
	if cfg.Selector == nil || cfg.Copier == nil || cfg.Tracker == nil || cfg.Index == nil || cfg.Set == nil || cfg.Durable == nil || cfg.Retirer == nil {
		panic("reclaim: NewPass needs its selector, copier, tracker, index, segment set, durability waiter and retirer")
	}
	return &Pass{sel: cfg.Selector, cop: cfg.Copier, tr: cfg.Tracker, ix: cfg.Index, set: cfg.Set, dur: cfg.Durability, wait: cfg.Durable, ret: cfg.Retirer, end: cfg.EndDeferral, acct: cfg.Account, res: cfg.Reserve, seal: cfg.Sealer}
}

// Stop interrupts every running pass between its units and joins it. What the
// in-flight unit does depends on where it stands: a segment it is already
// retiring completes — the retirer runs until after the pass's stop — while a
// unit waiting for a record to become durable is woken by the durability
// layer's own stop, which runs first, and abandons safely: nothing was
// published at that point, so it repoints nothing, retires nothing, and the
// claim goes back. A Run begun after Stop returns core.ErrClosed. Stop is
// safe to call twice and safe from any goroutine; it must not be called from
// inside one of its own passes, which would wait for themselves.
//
// decision: a never-blocking close outranks the interrupted unit's completion.
// The close's own reverse order stops the durability layer — whose stop wakes
// every durability waiter — and the pool, whose refill a roll that landed its
// first placement would be waiting on, before the pass's stop, so a close
// joins an abandoned unit rather than a finished one whenever they meet in a
// wait. That order is the core's deliberate close sequence and is not the
// pass's to invert. Overturned only by a ruled requirement that a close join
// a pass's completed retirement, which would need the durability layer to
// outlive the pass's stop — a close-order change, not a pass change.
func (p *Pass) Stop() {
	p.mu.Lock()
	p.stopped = true
	p.mu.Unlock()
	p.wg.Wait()
}

// stoppedNow reports whether Stop has run.
func (p *Pass) stoppedNow() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.stopped
}

// Run runs one pass of units inside scope, copying at most budget bytes and
// no more than the repack reserve held when the pass began, whichever is
// smaller: the budget and the reserve clamp the pass's selection, never a unit in flight — the check runs
// before the next unit starts, and a unit whose copies exceed the budget
// started completes (RFC 1 §3), so a caller's budget smaller than one segment
// gets one unit's worth of copies and one retirement, never half a unit. A
// unit whose expected copies do not fit what is left of the reserve is not
// started: the pass returns what it did, and the next call, once the
// retirements have returned the reserve whole, runs it. The pass never waits
// for the reserve itself. A
// budget of zero or less runs no unit. The context is checked before any
// state change — a refused pass selects nothing, appends nothing, releases
// nothing — and between units: a pass cancelled there returns after the unit
// in flight. The pass stops when no candidate remains, when the budget is
// reached, on its context's error, or on the error of a unit that cannot
// complete — a permanently failing unit ends the pass, its claim released,
// never retried within the same call. Every source a unit of this run chose
// not to hand over stays excluded from the run's own selection: the unit
// hands its claim to the run, which holds it open — no later Select offers
// the segment again — and releases it when the run ends.
//
// decision: the exclusion is the run's own claim, never the selector's pin
// set — the pins are the offer protocol's, and a run's unpin would delete an
// offer's pin on the same segment, so the pass touches no pin. The expense is
// one claim held per stayed candidate, to the run's end, so the selector
// leaves it out of every later Select of the run. Overturned by a pin
// protocol that counts its holders instead of naming them.
func (p *Pass) Run(ctx context.Context, budget int64, scope Scope) (Outcome, error) {
	if err := ctx.Err(); err != nil {
		return Outcome{}, err
	}
	p.mu.Lock()
	if p.stopped {
		p.mu.Unlock()
		return Outcome{}, core.ErrClosed
	}
	p.wg.Add(1)
	p.mu.Unlock()
	defer p.wg.Done()
	var out Outcome
	// decision: the reserve is read once, here, and every unit is counted
	// against that reading. A reserve not yet refilled reads zero, so the
	// pass returns freeing nothing and a caller looping until a pass frees
	// nothing can stop early. Overturned by a wait or poll for the refill, or
	// by a caller contract that retries after the retirements report.
	allowed := p.res.held()
	// The share seal: only a pass aimed at one share by tag alone closes
	// its active segments, which no selection offers while they are open. A
	// zero scope and a segment-set scope name segments or none, and seal
	// nothing. A budget that runs no unit seals nothing either. The context
	// is checked before it: the seal writes the marker, and a dead caller's
	// deadline must not be held by I/O its refusal will never live to use.
	if err := ctx.Err(); err != nil {
		return Outcome{}, err
	}
	if p.seal != nil && budget > 0 && scope.Share != 0 && len(scope.Segments) == 0 {
		p.seal.SealShare(scope.Share)
	}
	// reheld are the sources whose unit stayed: their claims — the run's,
	// not the unit's, from the moment they stay — keep every later Select
	// of the run from offering them again, and release when the run ends.
	// The release is a closure because a deferred call binds its arguments
	// where it is registered, and reheld is empty here.
	var reheld []core.SegmentID
	defer func() { p.sel.Done(reheld...) }()
	for {
		if err := ctx.Err(); err != nil {
			return out, err
		}
		if p.stoppedNow() {
			return out, core.ErrClosed
		}
		// decision: the budget clamps the selection, never a unit in
		// flight. The check takes the bytes already copied against the
		// caller's budget and stops before the next unit, so one unit
		// always runs for a positive budget and completes whatever it
		// copied — the retirement's one-per-reserve test leans on a
		// started unit never being cut in half. Overturned by a ruled
		// need to cap the copies mid-unit.
		if out.Bytes >= budget {
			return out, nil
		}
		cands := p.sel.Select(scope, 1)
		if len(cands) == 0 {
			return out, nil
		}
		seg := cands[0].ID
		if !admits(allowed, out.Bytes, cands[0].Live) {
			p.sel.Done(seg)
			return out, nil
		}
		bytes, freed, stayedNow, err := p.unit(ctx, seg)
		out.Bytes += bytes
		out.Freed += freed
		out.Segments++
		if stayedNow {
			reheld = append(reheld, seg)
		}
		if err != nil {
			return out, err
		}
	}
}

// unit runs one unit of work over seg, whose claim the selector holds. Every
// return releases the claim except a stay: on the retirement path the release
// comes first — the retirer takes the ledger's own claim itself, the order
// the retirement worker's own hand-over keeps — and every refusal releases
// too, while a stay hands the claim to the run it answers, which holds it
// open and releases it when the run ends. stayed reports a source the unit
// completed without handing over, whose next selection in the same run would
// only repeat it.
func (p *Pass) unit(ctx context.Context, seg core.SegmentID) (bytes, freed int64, stayed bool, err error) {
	res, err := p.cop.Copy(ctx, seg)
	if err != nil {
		// A copy that failed midway leaves the copies it already appended
		// in the set under their shares' tags, so they are counted and
		// handed over like any abandoned unit's.
		p.count(seg, res, CarryResult{})
		return 0, 0, false, p.abandon(seg, err)
	}
	carry, err := p.cop.Carry(ctx, seg, res)
	if err != nil {
		// The copies Copy landed and the records Carry placed before it
		// failed stay on disk as their shares' records, so they are
		// counted and handed over as the abandon comment promises: this
		// unit releases its claim and the source stays, and whatever the
		// source's later retirement would hand over is no less owed now.
		p.count(seg, res, carry)
		return 0, 0, false, p.abandon(seg, err)
	}
	// The copies are held out of their shares' tallies until the source goes: held here,
	// handed to their shares when it is retired.
	p.count(seg, res, carry)
	// Durability before any publish: every place this unit wrote — its
	// copies, its carried records and the deferred loss records its carry
	// appended — is waited for before one index entry moves.
	for _, cp := range res.Copies {
		if werr := p.wait.WaitCopy(cp.Loc); werr != nil {
			return 0, 0, false, p.abandon(seg, werr)
		}
	}
	for _, cr := range carry.Carried {
		if werr := p.wait.WaitDurable(cr.Rec.Loc); werr != nil {
			return 0, 0, false, p.abandon(seg, werr)
		}
	}
	for _, dc := range carry.Deferred {
		if werr := p.wait.WaitDurable(dc.Loc); werr != nil {
			return 0, 0, false, p.abandon(seg, werr)
		}
	}

	// The repoint: one index step per file, in the order the facts name
	// their files, so a reader of one file sees its pieces move together
	// (RFC 1 §8.4). Each Copied fact moves the pieces of every source
	// record its placement covered, inside the fact's extent — the facts,
	// not the runs: a split placement is several facts of one run, each
	// covering the sub-extent it placed, and a piece straddling the split
	// moves its two halves to the two facts' targets, each with its own
	// skip in the record it read from. A carried record moves by its own
	// original place, its copy registered live at its landing; a forget
	// record names no file, so the index holds nothing of it to move.
	moves := make(map[core.FileID][]index.RepointMove)
	var files []core.FileID
	addMove := func(file core.FileID, m index.RepointMove) {
		if _, known := moves[file]; !known {
			files = append(files, file)
		}
		moves[file] = append(moves[file], m)
	}
	for _, cp := range res.Copies {
		for _, sc := range cp.Sources {
			if slices.ContainsFunc(moves[cp.File], func(m index.RepointMove) bool { return m.From == sc.Record && m.Window == cp.Extent }) {
				continue
			}
			// The copy carries a number of its own, which the moved pieces
			// answer at its place — a read's header check names the record
			// the index entry sits on, so a piece keeping its source's
			// number would read the new record as another record's.
			addMove(cp.File, index.RepointMove{Window: cp.Extent, From: sc.Record, To: cp.To, Seq: cp.Seq})
		}
		bytes += cp.Extent.Len
	}
	for _, cr := range carry.Carried {
		if cr.Rec.File == (core.FileID{}) {
			continue
		}
		addMove(cr.Rec.File, index.RepointMove{Window: covered(cr.Rec), From: cr.From, To: cr.To})
	}
	for _, file := range files {
		_, hd, rerr := p.ix.RepointFile(file, moves[file])
		if p.acct != nil {
			p.acct(hd)
		}
		if rerr != nil {
			// An unpublished move strands the copies the unit counted against
			// the source beside the ones it repointed whole: both are the
			// shares' records now, for the source stays with its claim
			// released, so the abandon hands its tallies over with the rest.
			return 0, 0, false, p.abandon(seg, fmt.Errorf("repointing the copies of file %v: %w", file, rerr))
		}
	}

	// The deferral ends: the waits above have covered each deferred loss
	// record's place, so the registry gives its entry up and the tracker's
	// deferred count — lowered only for an entry the registry actually
	// gave up, never for one that already left or for a raise another
	// deferral owns — the source may be handed over only after both
	// (RFC 1 §6).
	for _, dc := range carry.Deferred {
		if p.end == nil || p.end(seg, dc.Loss) {
			p.tr.SetDeferred(seg, false)
		}
	}
	// The deferred snapshot's re-check: the carry read the registry once,
	// and a deferral raised since must not have its loss record left behind
	// by a retirement that never saw it — the source stays, its claim
	// released, for a later pass to copy under the standing deferral.
	//
	// decision: the re-check asks the tracker's whole Held answer, not the
	// deferred count alone, so its ceiling is a segment still holding
	// anything the tracker counts — a live header-only record that no
	// settlement has ended — which this pass keeps exactly as the
	// retirement worker's own hand-over does; the next pass after its
	// marker settles retires it. Overturned by a ruled need to retire a
	// segment whose liveness the tracker still counts.
	stayed = p.tr.Held(seg)
	if stayed {
		// The claim is kept, not released: the run must not re-select the
		// segment for a unit that will only repeat this one, and it
		// releases the claim when the run ends.
		return bytes, 0, true, nil
	}
	st, serr := p.set.State(seg)
	if serr != nil {
		p.sel.Done(seg)
		return bytes, 0, false, fmt.Errorf("reclaim: the unit over segment %d: the segment left the set before its retirement: %w", seg, serr)
	}
	var reason segment.Reason
	switch st {
	case segment.Sealed:
		reason = segment.NotHeld
	case segment.Ended:
		// An ended segment's window and reclaim block are re-read under
		// the claim the pass holds, as the retirement worker does: a
		// window that opened since the selection keeps its source for a
		// pass that runs once its records are carried and resolved.
		//
		// An ended segment's window is owed from the end itself — the set
		// carries the end not yet taken, and WindowOpen reads it — so no
		// gap exists for a pass to reach the segment through, however the
		// durability layer's take of the end event races: a newly failed
		// segment is declined here as long as its window stands.
		if p.dur != nil && (p.dur.WindowOpen(seg) || p.dur.ReclaimBlocked(seg)) {
			// The claim stays with the run, as the hold does above: the
			// ended segment whose window reopened keeps its place out of
			// this run's selections until the run releases it.
			return bytes, 0, true, nil
		}
		reason = segment.ReasonEnded
	default:
		p.sel.Done(seg)
		return bytes, 0, false, fmt.Errorf("reclaim: the unit over segment %d: the segment is in state %d, neither sealed nor ended", seg, st)
	}
	// The claim is released before the hand-over: the retirer takes the
	// ledger's own claim itself, and a segment handed over still claimed
	// would come back refused for ever (Done's own decision).
	p.sel.Done(seg)
	ret, terr := p.ret.Retire(seg, reason)
	if terr != nil {
		// decision: a claim on the segment that another holder took in the
		// gap between the claim's release above and Retire's own claim — the
		// retirement worker, to which a forget's drop offers a segment the
		// tag's records left empty, or a durability copy hold, which never
		// retires anything — or a retirement that already finished and
		// removed the segment, refuses the hand-over. RFC 1 §7.1 retires a
		// not-held segment by the journal on its own, without waiting for
		// the repack that selected it: the refusal answers a visit that
		// freed nothing, and the storage belongs to whichever retirement
		// eventually runs. ErrNotInSet was already counted by the ledger as
		// a double retire at Warn, the same cost the retirement queue
		// accepts for the same race. The ceiling: a holder that parks without
		// retiring stalls this segment's reclamation until it releases — the
		// worker's tick scan then offers the segment for retirement — as the
		// queue's own ErrClaimed retry does.
		if errors.Is(terr, segment.ErrClaimed) || errors.Is(terr, segment.ErrNotInSet) {
			return bytes, 0, false, nil
		}
		return bytes, 0, false, fmt.Errorf("reclaim: the unit over segment %d: %w", seg, terr)
	}
	// A retirement whose directory sync failed contributes no freed bytes
	// to the answer, which is the sum of the retirements the executor
	// reports durable: the removal may not have reached the directory, no
	// later sync of this process makes it durable (Retirement.Durable's own
	// contract), and the next open's scan settles what the failed sync
	// left, so the caller that waits on freed to retry waits for bytes it
	// can trust.
	if !ret.Durable {
		return bytes, 0, false, nil
	}
	return bytes, ret.Freed, false, nil
}

// count holds the bytes of every copy, carried record and deferred loss
// record the unit appended out of its shares' tallies against seg.
func (p *Pass) count(seg core.SegmentID, res Result, carry CarryResult) {
	if p.res.Count == nil {
		return
	}
	for _, cp := range res.Copies {
		p.res.Count(seg, cp.Tag, cp.Loc.Len)
	}
	for _, cr := range carry.Carried {
		p.res.Count(seg, cr.Rec.Tag, cr.Rec.Loc.Len)
	}
	for _, dc := range carry.Deferred {
		p.res.Count(seg, dc.Loss.Share, dc.Loc.Len)
	}
}

// abandon ends a unit whose copy, carried record or deferred loss record
// could not be made durable: the copies stay unpublished — repointed
// nowhere, retired nowhere — and the source's claim goes back, so a later
// pass finds the segment as it was.
//
// decision: a copy whose sync fails is abandoned, never repointed. The
// copies were appended but published nowhere before their wait, so a failed
// window that drops them removes nothing a reader sees; the unit is
// abandoned whole, the source is not retried within the same pass, and the
// error ends it. The ceiling: an abandoned unit costs its copies' bytes,
// held on the journal until the failed window resolves, and a failed sync
// loses that pass's progress. It is overturned once the durability layer's
// ledger can tell a never-published copy from a published record: then the
// copy is re-appended, or dropped silently, and the unit continues.
func (p *Pass) abandon(seg core.SegmentID, err error) error {
	// The unpublished copies stay on disk as records of their shares, whose
	// segments' retirement lowers the shares' figures by them, so they are
	// handed over now rather than at a source that stays.
	if p.res.Transfer != nil {
		p.res.Transfer(seg)
	}
	p.sel.Done(seg)
	return fmt.Errorf("reclaim: the unit over segment %d: %w", seg, err)
}
