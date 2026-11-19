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
func (e *Engine) repack(ctx context.Context, b *binding, r refusal, scope journal.RepackScope) error {
	b.gc.cap.passes.Add(1)
	defer b.gc.cap.passes.Add(-1)

	// The share's own limit charges its first pass with a budget of no
	// bytes when the pre-sync figures read empty: a scope the figures name
	// empty is one no pass can relieve, so the pass runs for its own
	// contract — its error is the ladder's to answer — and seals and copies
	// for nothing, paying nothing of the caller's deadline. A pass with a
	// real candidate runs at the repack reserve as before.
	budget := b.j.Stats().RepackReserveBytes
	if r.limit == journal.LimitShare && e.figuresEmpty(b, r) {
		budget = 0
	}

	_, err := b.j.Repack(ctx, budget, scope)

	return err
}

// syncScope syncs every file of the bound shares, each through its own handle
// so a failed window reaches the file's next Commit (RFC 8 §5.1). It takes
// no file entry, guard or registration, only the group commit's lock for a
// moment, so a Delete or Truncate refused while it holds its removal guard
// cannot deadlock on it. A share other than own whose handle is closed is
// being dropped, since Drop closes the handle before it marks the binding
// removed: its files are skipped, as they are not this refusal's to free and
// its closed handle would fail the refused share's write.
//
// The journal's Files is a stub that yields no file until the index lands, so
// over the real journal this syncs nothing yet; the figures it is meant to
// move are read from Stats afterwards either way.
func (e *Engine) syncScope(ctx context.Context, own *binding, bound []*binding) error {
	for _, sb := range bound {
		var ids []journal.FileID

		for id := range sb.h.Files() {
			ids = append(ids, id)
		}

		if len(ids) == 0 {
			continue
		}

		if err := e.sync(ctx, sb, ids...); err != nil {
			if sb != own && errors.Is(err, journal.ErrClosed) {
				continue // being dropped: its files are not this refusal's to free
			}

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
// offload. A journal-wide limit also runs its own syncScope per refusal, which
// grows with the files on the journal, so a burst of N refusals is 2N repacks
// and N full-journal syncs. Upgrade to one repack and one scope sync in flight
// per journal and scope that later refusals wait on when repack bytes copied
// per byte freed, or the time a refusal spends in syncScope, rises under a
// refusal storm.
func (e *Engine) relieve(ctx context.Context, b *binding, r refusal, at *step, synced bool, s0 error) error {
	scope, bound := e.scopeOf(b, r)

	var err error

	switch *at {
	case stepNone:
		err = e.repack(ctx, b, r, scope)
		*at = stepRepack
	default:
		// The step's own sync, unless the figures half above already ran
		// it and handed its outcome in: the sync's error seeds the repack's
		// guard either way, so a dead sync still lets no repack run and a
		// live one that failed lets the repack try, as before.
		err = s0
		if !synced {
			err = e.syncScope(ctx, b, bound)
		}
		if err == nil || ctx.Err() == nil {
			serr := e.repack(ctx, b, r, scope)
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
		// decision: a journal that answers ErrNotImplemented from Repack —
		// a zero Journal, or the fake's unmodelled segment free — serves no
		// UnreclaimedEndedBytes a settled answer could read, so the refusal
		// keeps the interim retry-later answer instead of ending no space by
		// a figure of 0. Withdrawn when every journal that can refuse a
		// write also reports the figure.
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

// figuresRead answers whether the share-limit figures of b's journal
// name nothing the relief could act on: no unreclaimed byte in any segment
// — dead weight, released extents, what a superseded record the sync just
// lifted — no reservation still in flight, whose room a caller may get by
// landing or rolling it back without a free, no record byte charged in a
// sealed or ended segment, which is the live a pass would cycle, and the
// repack reserve holding the share's live payload twice over, the condition
// under which a pass converges rather than stalls with its copies charged
// in sealed segments — the stall a share looping a half-open batch against
// its own limit rides. The prefigures half reports the state before the
// scope's sync: its positive read says the ladder needs no help finding
// what to do, and it also says nothing a sync would expose — the share
// whose governed figures read zero pre-sync is the one whose room may
// still change, so the first relief step runs its pass on a budget of no
// bytes and the answer waits for the sync the last step runs. All four
// satisfied after that sync — the pass had its chance to act and its
// error is the ladder's to answer — settle the refusal at once from the
// figures, without the second pass the last step would run: a scope the
// figures name empty is one no repack can relieve, so the pass buys the
// writing nothing and the earlier a no-space is answered the less of the
// caller's deadline the relief spends (RFC 8 section 10.2's second row).
//
// decision: the check answers the share's own limit alone. A journal-wide
// refusal with its unreclaimed bytes all in active segments is served by
// the sync-then-repack ladder, so a zero read of the journal's own figure
// does not predict that verdict; extending the check there needs the
// journal-side post-sync figure that the active-segments row's serving
// path itself is the evidence for. Withdrawn when the journal-wide limits
// share the prediction.
func (e *Engine) figuresEmpty(b *binding, r refusal) bool {
	st := b.j.Stats()
	figs, booked := st.Shares[b.share.Tag]
	if !booked || figs.UnreclaimedBytes > 0 || figs.ReservedBytes > 0 || e.left(b, r) > 0 ||
		st.RepackReserveBytes < 2*figs.HeldBytes {
		return false
	}
	return true
}

// settledFromFigures answers a share-limit refusal the post-sync figures
// settle: the scope's own sync is issued here — the one the ladder's last
// step runs, so the step issues it exactly once — and the figures it
// leaves empty mean no repack can relieve the refusal, which answers no
// space at once rather than spend the caller's deadline on a pass that
// would seal, copy and retire for nothing. A sync that fails leaves the
// verdict unread: done false with the sync's error, for the step's own
// guard, whose rules the caller keeps.
func (e *Engine) settledFromFigures(ctx context.Context, b *binding, r refusal, err error) (answer error, done bool, serr error) {
	serr = e.syncScope(ctx, b, []*binding{b})
	if serr != nil {
		return nil, false, serr
	}
	if !e.figuresEmpty(b, r) {
		return nil, false, nil
	}
	return e.answeredAs(b, err, r, settled(r, 0, false)), true, nil
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

	// The flag follows the raise, so a write accepted between the two finds
	// the flag false and leaves the condition up; the next accepted write,
	// which finds it true, clears it. The other order let that write clear a
	// condition not raised yet, which then stayed up.
	b.gc.cap.headroom.Store(r.limit == journal.LimitHeadroom && !r.device)
}

// acceptedWrite clears journal_full when the latest refusal named the
// headroom: below half the headroom every unreserved content write is refused,
// so one accepted outside a reservation shows the headroom has room again. No
// other accepted write clears it (RFC 8 §11.1).
func (e *Engine) acceptedWrite(b *binding) {
	// A load first: a write on every accepted call must not take the cache
	// line exclusively while the flag is false, which it nearly always is.
	if b.gc.cap.headroom.Load() && b.gc.cap.headroom.CompareAndSwap(true, false) {
		e.conds.clear(condKey{conditionJournalFull, b.gc})
	}
}
