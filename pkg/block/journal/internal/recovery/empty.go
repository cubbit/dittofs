package recovery

import (
	"context"
	"errors"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/durability"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/segment"
)

// Emptying is the open's emptying phase, the one the phases list covers with
// a span-only entry its loop runs as a no-op: the phase's work waits for the
// open's loss records, which the loss pass appends and syncs only after the
// assembled journal exists — no phase of the recovery itself can run that
// pass — so the work runs here, where the open stands past it, before the
// journal serves and before the retirement worker starts (RFC 28 §3.3: the
// loss records first, then every attached segment that is not held is
// retired, then the ended segments are emptied).
//
// The span the phases list opens covers the recovery's own end; the work of
// the phase opens it once more here, so the trace of an open shows both: the
// named no-op where the phase sits in the list, and the real span its work
// ran under. The two-step shape of the list's one entry — retiring first,
// emptying the ended segments after, each retirement refilling the reserve
// before the next ended segment's work draws a spare from it — lives in this
// file: the retire step below runs first, and the emptying step the list's
// span also names joins it here.
func Emptying(ctx context.Context, deps Deps, st *State, expect core.JournalID, floor core.Version, marks []core.SyncMark) (err error) {
	if err := ctx.Err(); err != nil {
		return err
	}
	ctx, end := startSpan(ctx, deps, core.SpanOpenEmptying)
	defer func() { end(err) }()
	rd := newReader(ctx, deps.FS, deps.Clock, deps.Log)
	return runRetire(ctx, deps, rd, st, expect, floor, marks)
}

// runRetire is the emptying's first step: every attached segment the
// liveness tracker answers not held is retired, through the retirement path
// the assembled journal runs — recycle or unlink, the pool's refill after
// each included — before any ended segment is emptied.
//
// The order this step runs in — past the loss pass, whose records are
// durable before the journal it assembled reaches here — is what lets a
// retiree go: a segment may hold the only copy of a record the crash
// corrupted, and the mask its loss record makes is on disk before the
// segment's unlink takes the record with it. Nothing is asserted of that
// order here: the caller hands the state the loss pass already gated on.
func runRetire(ctx context.Context, deps Deps, rd *reader, st *State, expect core.JournalID, floor core.Version, marks []core.SyncMark) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if st.Assembled.Set == nil {
		return errors.New("recovery: the emptying's state carries no assembled ledger, so nothing is a retirement's to take")
	}
	if deps.Liveness == nil || deps.Retire == nil {
		return errors.New("recovery: the emptying's deps carry no liveness tracker or retirement path: the open hands the ones the journal it assembled runs")
	}
	// The deferred-loss registry, read the way the open reads the failed
	// list: a segment the registry answers as holding a deferred loss's
	// dropped record is never retired, for its copy has not been made, and
	// retiring it would take the record the next open's re-detection would
	// need (RFC 28 §3.3).
	deferred, err := durability.DeferredFor(deps.Dir, expect)
	if err != nil {
		return err
	}
	for _, row := range st.Assembled.Set.Snapshot() {
		var reason segment.Reason
		switch row.State {
		case segment.Sealed:
			reason = segment.NotHeld
		case segment.Ended:
			// decision: the ended window and the damaged-segment block a
			// running journal's retirement re-reads are not consulted here,
			// for neither can answer true before this step runs: a window
			// opens only when a stream appends, and the journal the open
			// holds has served no operation; a segment turns damaged only on
			// a serving read's corrupt find. The first re-reads a segment
			// facing either are the worker's own, past the open.
			reason = segment.ReasonEnded
		default:
			// decision: an Active row is never a retirement's candidate,
			// whatever its bytes hold — the retirement path itself refuses
			// the pair, and this step widens it by nothing: a re-adopted
			// spare the spares phase took stands in the headroom until the
			// pool wiring reads it; a take withheld over a predecessor the
			// open ends stands until that predecessor is unlinked with a
			// synced unlink; a live row holds the records a stream still
			// appends beside. The pool's own spares are not among the
			// ledger's rows at all, so a spare the retirer recycled during
			// this very step — the reserve's refill — is not taken either.
			continue
		}
		if deps.Liveness.Held(row.ID) {
			continue // the tracker's answer is the whole candidate test: no superset beside it
		}
		if len(deferred[row.ID]) > 0 {
			// decision: the registry is structurally empty at an open whose
			// loss records are already durable — the loss pass refuses the
			// past-the-reserve drops it would once have deferred before
			// anything appends, so no entry exists by the time this step
			// reads the registry. The guard reads the real registry anyway:
			// the emptying that writes each deferred record into its copy
			// lifts the holds by ending its deferrals, and its withdrawal
			// needs no new wiring here.
			continue
		}
		// The doorstep of the retirement's writes: a cancellation that
		// already landed leaves the segment standing and the next open
		// re-reads and re-decides, but no unlink begins past it.
		if err := ctx.Err(); err != nil {
			return err
		}
		if _, err := deps.Retire(row.ID, reason); err != nil {
			return err
		}
	}
	return nil
}
