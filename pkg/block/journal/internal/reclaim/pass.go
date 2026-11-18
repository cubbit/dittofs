package reclaim

import (
	"context"
	"fmt"
	"slices"
	"sync"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/index"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/segment"
)

// Durable waits for one record's place to become durable: the durability
// layer's own WaitDurable, which starts a sync for a record that has none and
// follows a re-appended record to its new place.
type Durable interface {
	WaitDurable(at segment.Location) error
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
	// pins holds back, until the run ends, every source the pass could not
	// hand over, so the next unit of the same run cannot select it again.
	// A nil *Pins pins nothing, which is every test wiring's choice.
	pins *Pins

	// decision: mu is a leaf. It guards the stop flag and the run count's
	// admission alone — spent once to refuse a Run begun after Stop and to
	// keep Stop's join from racing a late wg.Add — and nothing is called
	// while it is held: the interrupt and the join it orders both run once
	// it is released, so no lock of the journal's is ever taken under it.
	mu      sync.Mutex
	stopped bool
	wg      sync.WaitGroup
}

// NewPass builds the pass over the pieces its caller owns: the selector claims
// the candidates the units run over, the copier copies and carries under the
// same tracker the retirement worker serves, dur is the durability service the
// ended segments re-read, wait the durability waiter the copies publish
// through, ret the retirements' executor, end the deferral-end wiring and acct
// the held-delta consumer. dur, end and acct may be nil — a pass over a journal
// that carries no deferral needs none of them, and the tests that drive the
// pieces alone pass nil. The rest are a caller bug and panic here.
func NewPass(sel *Selector, cop *Copier, tr *Tracker, ix *index.Index, set *segment.Set, dur Durability, wait Durable, ret Retirer, end func(core.SegmentID, DeferredLoss) bool, acct func(index.HeldDelta), pins *Pins) *Pass {
	if sel == nil || cop == nil || tr == nil || ix == nil || set == nil || wait == nil || ret == nil {
		panic("reclaim: NewPass needs its selector, copier, tracker, index, segment set, durability waiter and retirer")
	}
	return &Pass{sel: sel, cop: cop, tr: tr, ix: ix, set: set, dur: dur, wait: wait, ret: ret, end: end, acct: acct, pins: pins}
}

// Stop interrupts every running pass between its units and joins it: a unit in
// flight completes — its copies, repoints, deferral ends and retirement run to
// their ends — and the loop returns at its next turn. A Run begun after Stop
// returns core.ErrClosed. Stop is safe to call twice and safe from any
// goroutine; it must not be called from inside one of its own passes, which
// would wait for themselves.
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

// Run runs one pass of units inside scope, copying at most budget bytes: the
// budget clamps the pass's selection, never a unit in flight — the check runs
// before the next unit starts, and a unit whose copies exceed the budget
// started completes (RFC 1 §3), so a caller's budget smaller than one segment
// gets one unit's worth of copies and one retirement, never half a unit. A
// budget of zero or less runs no unit. The context is checked before any
// state change — a refused pass selects nothing, appends nothing, releases
// nothing — and between units: a pass cancelled there returns after the unit
// in flight. The pass stops when no candidate remains, when the budget is
// reached, on its context's error, or on the error of a unit that cannot
// complete — a permanently failing unit ends the pass, its claim released,
// never retried within the same call. Every source the pass cannot hand over
// is pinned off its own selection until Run returns.
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
	var stayed []core.SegmentID
	defer func() {
		for _, seg := range stayed {
			p.pins.Unpin(seg)
		}
	}()
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
		bytes, freed, stayedNow, err := p.unit(ctx, seg)
		out.Bytes += bytes
		out.Freed += freed
		out.Segments++
		if stayedNow {
			stayed = append(stayed, seg)
			p.pins.Pin(seg)
		}
		if err != nil {
			return out, err
		}
	}
}

// unit runs one unit of work over seg, whose claim the selector holds. Every
// return releases the claim: on the retirement path before the hand-over —
// the retirer takes the ledger's own claim itself, the order the retirement
// worker's own hand-over keeps — and on every refusal. stayed reports a
// source the unit completed without handing over, whose next selection in
// the same pass would only repeat it.
func (p *Pass) unit(ctx context.Context, seg core.SegmentID) (bytes, freed int64, stayed bool, err error) {
	res, err := p.cop.Copy(ctx, seg)
	if err != nil {
		p.sel.Done(seg)
		return 0, 0, false, fmt.Errorf("reclaim: the unit over segment %d: %w", seg, err)
	}
	carry, err := p.cop.Carry(ctx, seg, res)
	if err != nil {
		p.sel.Done(seg)
		return 0, 0, false, fmt.Errorf("reclaim: the unit over segment %d: %w", seg, err)
	}
	// Durability before any publish: every place this unit wrote — its
	// copies, its carried records and the deferred loss records its carry
	// appended — is waited for before one index entry moves.
	for _, cp := range res.Copies {
		if werr := p.wait.WaitDurable(cp.Loc); werr != nil {
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
			p.sel.Done(seg)
			return 0, 0, false, fmt.Errorf("reclaim: the unit over segment %d: repointing the copies of file %v: %w", seg, file, rerr)
		}
	}

	// The deferral ends: the waits above have covered each deferred loss
	// record's place, so the registry gives its entry up and the tracker's
	// deferred count lowers with it — the source may be handed over only
	// after both (RFC 1 §6).
	for _, dc := range carry.Deferred {
		if p.end != nil {
			p.end(seg, dc.Loss)
		}
		p.tr.SetDeferred(seg, false)
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
		p.sel.Done(seg)
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
		if p.dur != nil && (p.dur.WindowOpen(seg) || p.dur.ReclaimBlocked(seg)) {
			p.sel.Done(seg)
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
		return bytes, 0, false, fmt.Errorf("reclaim: the unit over segment %d: %w", seg, terr)
	}
	// A retirement whose directory sync failed counts its freed bytes all
	// the same: the retirement is completed, and no later sync ever makes
	// it durable in this process (Retirement.Durable's own contract); the
	// next open's scan settles what the failed sync left.
	return bytes, ret.Freed, false, nil
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
	p.sel.Done(seg)
	return fmt.Errorf("reclaim: the unit over segment %d: %w", seg, err)
}
