package engine

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/cubbit/dittofs/pkg/block/journal"
	"github.com/cubbit/dittofs/pkg/vfs/vfserr"
)

// conditionJournalFull is RFC 8 §11.1's condition for a journal that refused
// a write for its own maximum, its headroom or its index bound, or because its
// device is full.
const conditionJournalFull = "journal_full"

// step is how far a refusal's repack path has gone.
type step uint8

const (
	stepNone   step = iota // nothing ran: the next refusal repacks by the scope
	stepRepack             // the scoped repack ran: the next refusal runs the last step
	stepLast               // the last step ran: the next refusal is answered
)

// capState is what a journal keeps for its refusals: the passes running on
// it, whether its repack path is failing and whether the latest refusal named
// the headroom. It lives on the journal's group commit.
type capState struct {
	// passes counts the Repack calls in flight on this journal.
	//
	// decision: the index row answers retry-later only while another pass is
	// running, since its coalescing may still lower the count, and no space at
	// once otherwise: a refusal in its sync, or between steps, is no pass, so
	// under steady writes the row cannot delay for ever and hang a hard mount.
	// Overturned if a refusal's sync is shown to lower the count after another
	// refusal has read it.
	passes   atomic.Int64
	failing  atomic.Bool
	headroom atomic.Bool // the latest refusal that raised journal_full named the headroom
	lines    refusalLimiter
}

// refusalNote is what a journal_full refusal lacked and the journal's figures
// then, for the condition's detail.
type refusalNote struct {
	missing  int64
	unit     journal.Unit
	freed    uint64
	maxBytes int64
	used     int64
	headroom int64
	extents  int64
	extentAt int64
}

// relieves reports whether a refusal by r is answered by a repack path: a
// limit that repack and the last step can drain. A full device is cleared
// only by an operator, the version limit by its raise and the spare pool by
// itself, so none of them repacks.
func relieves(r refusal) bool {
	if r.device {
		return false
	}

	switch r.limit {
	case journal.LimitShare, journal.LimitJournal, journal.LimitIndex, journal.LimitHeadroom:
		return true
	}

	return false
}

// settled is the answer to a refusal by r once the repack path ran: retry-later
// while the scope has bytes left to free, and no space at once when it has
// none. The index bound answers by whether another pass is running, since that
// pass's coalescing may still lower the count (RFC 8 §10.2).
//
// decision: bytes that are all dirty count as nothing that can be freed, since
// no offload is composed here: a scope with nothing left to free answers no
// space at once rather than an endless delay, which would hang an NFS hard
// mount. Withdrawn when offload is composed and the dirty bytes can be freed.
func settled(r refusal, left int64, others bool) row {
	free := left > 0
	if r.limit == journal.LimitIndex {
		free = others
	}

	if free {
		return row{delay: vfserr.DelayCapacity}
	}

	return row{noSpace: true, why: fmt.Sprintf("the %s limit has nothing left to free", r)}
}

// expired reports whether ctx has ended or its deadline has passed on the
// engine's clock, which a test clock moves without ending the context.
func (e *Engine) expired(ctx context.Context) bool {
	if ctx.Err() != nil {
		return true
	}

	deadline, ok := ctx.Deadline()

	return ok && !e.d.Clock.Now().Before(deadline)
}

// scopeOf is the repack scope of a refusal by r of b's journal, and the
// bindings whose files the last step syncs: the share alone for its own limit,
// every bound share of the journal for the journal's own limits (RFC 8 §10.2).
func (e *Engine) scopeOf(b *binding, r refusal) (journal.RepackScope, []*binding) {
	if r.limit == journal.LimitShare {
		return journal.RepackScope{Share: b.share.Tag}, []*binding{b}
	}

	var bound []*binding

	for _, sb := range e.table() {
		if !sb.removed && sb.j == b.j {
			bound = append(bound, sb)
		}
	}

	return journal.RepackScope{}, bound
}

// repack runs one repack pass over scope, spending one repack reserve of
// bytes: the journal sizes it to at least a segment's live payload (RFC 8
// §10.3). The result of the pass is not read: what is left to free is read
// from Stats afterwards.
func (e *Engine) repack(ctx context.Context, b *binding, scope journal.RepackScope) error {
	b.gc.cap.passes.Add(1)
	defer b.gc.cap.passes.Add(-1)

	_, err := b.j.Repack(ctx, b.j.Stats().RepackReserveBytes, scope)

	return err
}

// syncScope syncs every file of the bound shares, each through its own handle
// so a failed window reaches the file's next Commit (RFC 8 §5.1). It takes
// no file entry, guard or registration, only the group commit's lock for a
// moment, so a Delete or Truncate refused while it holds its removal guard
// cannot deadlock on it.
func (e *Engine) syncScope(ctx context.Context, bound []*binding) error {
	for _, sb := range bound {
		var ids []journal.FileID

		for id := range sb.h.Files() {
			ids = append(ids, id)
		}

		if len(ids) == 0 {
			continue
		}

		if err := e.sync(ctx, sb, ids...); err != nil {
			return err
		}
	}

	return nil
}

// relieve runs the next step of b's repack path for a refusal by r and reports
// whether the call is retried. A step that fails with anything but the
// caller's context leaves the refusal as the input, as if it freed nothing,
// and the error is logged, not answered (RFC 8 §10.2). A closed journal is
// returned as it is.
//
// ponytail: repack runs only at a refusal, one scoped pass per refusal and none
// queued or kept in flight per scope; the eviction loop's own repack comes with
// offload. Upgrade to one repack in flight per journal and scope that later
// refusals wait on when repack bytes copied per byte freed rises under a
// refusal storm.
func (e *Engine) relieve(ctx context.Context, b *binding, r refusal, at *step) error {
	scope, bound := e.scopeOf(b, r)

	var err error

	switch *at {
	case stepNone:
		err = e.repack(ctx, b, scope)
		*at = stepRepack
	default:
		if err = e.syncScope(ctx, bound); err == nil || ctx.Err() == nil {
			serr := e.repack(ctx, b, scope)
			if err == nil {
				err = serr
			}
		}

		*at = stepLast
	}

	switch {
	case err == nil:
		e.stepOK(b)
	case errors.Is(err, journal.ErrClosed), ctx.Err() != nil:
		return err
	case errors.Is(err, journal.ErrNotImplemented):
		// ponytail: the journal's Repack is not built and it reports no
		// UnreclaimedEndedBytes yet (RFC 1 §10 questions 5 and 6), so the refusal
		// keeps the interim retry-later answer instead of ending no space by a
		// figure of 0. Withdraw once Repack seals and reports the figure.
		return err
	default:
		e.stepFailed(b, err)
	}

	return nil
}

// stepFailed logs a failing step of b's repack path: at Error when it starts
// failing, then once a minute as a summary while it lasts (RFC 25 §5.2).
func (e *Engine) stepFailed(b *binding, err error) {
	st := &b.gc.cap
	now := e.d.Clock.Elapsed()

	if st.failing.CompareAndSwap(false, true) {
		st.lines.take(now)
		e.d.Logger.Error("capacity step failed", "err", err)

		return
	}

	if logged, suppressed := st.lines.take(now); logged {
		e.d.Logger.Warn("capacity step still failing", "err", err, "suppressed", suppressed)
	}
}

// stepOK logs the end of a failing stretch of b's repack path.
func (e *Engine) stepOK(b *binding) {
	if b.gc.cap.failing.CompareAndSwap(true, false) {
		e.d.Logger.Info("capacity step recovered")
	}
}

// left is the bytes the scope of a refusal by r still has to free, read from
// the journal's Stats after the last retry: the unreclaimed bytes of segments
// that take no further append, of the share's tag for its own limit and of the
// journal otherwise. A failed segment counts whether or not its window has
// resolved, and one a running pass selected still counts (RFC 8 §10.2).
func (e *Engine) left(b *binding, r refusal) int64 {
	st := b.j.Stats()
	if r.limit == journal.LimitShare {
		return st.Shares[b.share.Tag].UnreclaimedEndedBytes
	}

	return st.UnreclaimedEndedBytes
}

// raiseFull raises journal_full for a refusal by r of b's journal, with what
// the refusal lacked and the journal's figures then. A share's own limit, a
// roll with no spare and the version limit raise nothing (RFC 8 §11.1).
func (e *Engine) raiseFull(b *binding, r refusal, err error) {
	if !r.device && r.limit != journal.LimitJournal &&
		r.limit != journal.LimitHeadroom && r.limit != journal.LimitIndex {
		return
	}

	st := b.j.Stats()
	n := &refusalNote{
		freed:    st.Counters.FreedBytes,
		maxBytes: st.MaxBytes,
		used:     st.UsedBytes,
		headroom: st.HeadroomBytes,
		extents:  st.ExtentCount,
		extentAt: st.ExtentLimit,
	}

	var ns *journal.NoSpaceError
	if errors.As(err, &ns) {
		n.missing, n.unit = ns.Missing, ns.Unit
	}

	b.gc.cap.headroom.Store(r.limit == journal.LimitHeadroom && !r.device)

	e.conds.raise(condKey{conditionJournalFull, b.gc}, Condition{
		Name:    conditionJournalFull,
		Subject: Subject{Scope: ScopeJournal},
		Cause:   err,
		Detail: fmt.Sprintf(
			"the journal refused a write by %s, lacking %d %s; used %d of %d bytes, "+
				"freed %d, headroom %d, index %d of %d",
			r, n.missing, n.unit, n.used, n.maxBytes, n.freed, n.headroom, n.extents, n.extentAt,
		),
	}, time.Time{})
}

// acceptedWrite clears journal_full when the latest refusal named the
// headroom: below half the headroom every unreserved content write is refused,
// so one accepted outside a reservation shows the headroom has room again. No
// other accepted write clears it (RFC 8 §11.1).
func (e *Engine) acceptedWrite(b *binding) {
	if b.gc.cap.headroom.CompareAndSwap(true, false) {
		e.conds.clear(condKey{conditionJournalFull, b.gc})
	}
}
