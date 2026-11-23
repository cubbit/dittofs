package engine

import (
	"context"
	"errors"
	"fmt"
	"sync"
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
// it, whether its repack path is failing, whether the latest refusal named
// the headroom, and what the latest one that named the device or the maximum
// lacked. It lives on the journal's group commit.
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

	// full is the latest refusal that raised journal_full when it named the
	// device or the maximum, nil otherwise or once room cleared it, and
	// checkAt when the group commit's loop next judges its room: zero, at
	// once. mu orders a raise against a clear on room, so a clear judged on an
	// older refusal never drops the condition a newer one raised.
	mu      sync.Mutex
	full    *refusalNote
	checkAt time.Time
}

// refusalNote is what a journal_full refusal lacked and the journal's figures
// then, for the condition's detail.
type refusalNote struct {
	device   bool
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

// repack runs one repack pass over scope. A pass the share-limit figures
// name empty runs at a budget of no bytes — the journal runs no unit, seals
// nothing and touches no segment, paying nothing of the caller's deadline,
// which is why a call that answers nothing can still be worth making — and
// every other pass runs at one repack reserve of bytes, which the journal
// sizes to at least a segment's live payload (RFC 8 section 10.3). The result
// of the pass is not read: what is left to free is read from Stats
// afterwards.
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
func (e *Engine) relieve(ctx context.Context, b *binding, r refusal, at *step) error {
	scope, bound := e.scopeOf(b, r)

	var err error

	switch *at {
	case stepNone:
		err = e.repack(ctx, b, r, scope)
		*at = stepRepack
	default:
		if err = e.syncScope(ctx, b, bound); err == nil || ctx.Err() == nil {
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

// figuresEmpty answers whether the share-limit figures of b's journal name
// nothing the relief could act on, read before the scope's own sync runs
// and used only to charge the relief's pass: an empty read sends the pass
// to the journal at a budget of no bytes, a call that keeps the ladder's
// failing-step and dead-sync contracts without their cost. The figures and
// what each guards against:
//
//   - the share's unreclaimed bytes, beyond zero, name dead weight in any
//     segment — released extents, superseded records — that a pass could
//     retire;
//   - the share's reservations still in flight name bytes whose room the
//     caller may get by landing or rolling one back without a free, so the
//     figures cannot yet rule anything freable;
//   - the share's ended candidacy, the record bytes charged in segments no
//     append can reach, names the live bytes a pass would cycle;
//   - the repack reserve below twice the share's live payload means a pass
//     cannot converge: its copies would stall charged in sealed segments,
//     the stall a share looping a half-open batch against its own limit
//     rides.
//
// decision: the check answers the share's own limit alone. A journal-wide
// refusal with its unreclaimed bytes all in active segments is served by the
// sync-then-repack ladder, so a zero read of the journal's own figure does
// not predict that verdict; extending the check there needs the journal-side
// post-sync figure that the active-segments row's serving path itself is the
// evidence for. Withdrawn when the journal-wide limits share the prediction.
func (e *Engine) figuresEmpty(b *binding, r refusal) bool {
	st := b.j.Stats()
	figs, booked := st.Shares[b.share.Tag]
	if !booked || figs.UnreclaimedBytes > 0 || figs.ReservedBytes > 0 || e.left(b, r) > 0 ||
		st.RepackReserveBytes < 2*figs.HeldBytes {
		return false
	}
	return true
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
		device:   r.device,
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

	c := &b.gc.cap
	c.mu.Lock()
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
	c.full, c.checkAt = nil, time.Time{}
	if r.device || r.limit == journal.LimitJournal {
		c.full = n
	}
	judge := c.full != nil
	c.mu.Unlock()

	if judge {
		b.gc.poke() // the loop judges the room at this wake, then every fullRecheck
	}

	// The flag follows the raise, so a write accepted between the two finds
	// the flag false and leaves the condition up; the next accepted write,
	// which finds it true, clears it. The other order let that write clear a
	// condition not raised yet, which then stayed up.
	b.gc.cap.headroom.Store(r.limit == journal.LimitHeadroom && !r.device)
}

// acceptedWrite clears journal_full when the latest refusal named the
// headroom: below half the headroom every unreserved content write is refused,
// so one accepted outside a reservation shows the headroom has room again. No
// other accepted write clears it (RFC 8 §11.1): a refusal of the device or the
// maximum clears when the group commit's loop finds its limit has room again
// (checkFull).
func (e *Engine) acceptedWrite(b *binding) {
	// A load first: a write on every accepted call must not take the cache
	// line exclusively while the flag is false, which it nearly always is.
	if b.gc.cap.headroom.Load() && b.gc.cap.headroom.CompareAndSwap(true, false) {
		e.conds.clear(condKey{conditionJournalFull, b.gc})
	}
}

// deviceRoomBytes is the free space a full device must regain before
// journal_full clears: a segment's cap, from which a spare can be made again
// (RFC 8 §11.1, RFC 28 §2.2).
//
// ponytail: the journal reports no segment cap, so this copies the format's,
// 256 MiB. A journal opened with a smaller cap, which only a test harness can
// set, clears later than it could, never earlier. Upgrade to the cap the
// journal's Stats reports once it reports one.
const deviceRoomBytes = 256 << 20

// roomAgain reports whether the limit refusal n named has room again since
// it, by at least what n lacked (RFC 8 §11.1), from st, the journal's figures
// now, and free, the free space of the journal directory's filesystem, which
// it reads only for a full device. Under the maximum the room is the storage
// retired segments returned since n, by an unlink or a recycle, and any
// change of the maximum, a lowered one counted against them, or the maximum
// removed; never UsedBytes, which a recycle does not lower. On a full
// device it is a segment's cap, recycled or free.
//
// decision: a free-space read that fails counts as no room, so journal_full
// stays up rather than clearing over a device nobody could read. Overturned
// if a read can fail for a cause unrelated to the device, keeping the
// condition up with the device healthy.
func (n *refusalNote) roomAgain(st journal.Stats, free func() (int64, error)) bool {
	gained := int64(0)
	if st.Counters.FreedBytes > n.freed {
		gained = int64(st.Counters.FreedBytes - n.freed)
	}

	if n.device {
		if gained >= deviceRoomBytes {
			return true
		}

		f, err := free()

		return err == nil && f >= deviceRoomBytes
	}

	if st.MaxBytes <= 0 {
		return true // the maximum was removed: the journal is unbounded
	}

	return gained+(st.MaxBytes-n.maxBytes) >= max(n.missing, 1)
}

// fullRecheck is how often the group commit's loop judges the room of a
// standing refusal that named the device or the maximum.
const fullRecheck = time.Second

// fullDue is how long from now until the loop judges the room of the latest
// refusal that named the device or the maximum: zero when it is due, -1 when
// none stands.
func (g *groupCommit) fullDue(now time.Time) time.Duration {
	c := &g.cap
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.full == nil {
		return -1
	}

	return max(c.checkAt.Sub(now), 0)
}

// checkFull judges whether the limit the latest refusal named, the device or
// the maximum, has room again, and clears journal_full if so; otherwise it
// judges again fullRecheck later. It runs on the group commit's loop, so
// Stats reads only the condition and makes no I/O for it. Its journal and
// free-space reads run outside the lock, so a raise meanwhile replaces the
// note and the verdict, judged on the older one, is dropped.
//
// ponytail: the check rides on the group commit's loop, which wakes for it
// at the refusal and then every fullRecheck while a refusal of the device or
// the maximum stands, idle or not, and runs it before each take, so a batch
// on every pass does not starve it; a store stall holds the loop inside a
// commit, so the clear can lag by a stall. Upgrade to the work queue's age
// tick (RFC 8 §2.5 step 7) when the second part builds it.
func (g *groupCommit) checkFull() {
	e, c := g.e, &g.cap
	c.mu.Lock()
	n := c.full
	c.mu.Unlock()

	if n == nil {
		return
	}

	room := n.roomAgain(g.j.Stats(), func() (int64, error) { return e.d.FreeSpace(e.d.Devices[0]) })

	if h := e.fullJudging.Load(); h != nil {
		(*h)()
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	if c.full == n {
		if room {
			c.full = nil
			e.conds.clear(condKey{conditionJournalFull, g})
		} else {
			c.checkAt = e.d.Clock.Now().Add(fullRecheck)
		}
	}

	// Under the lock, so a test that hears it reads the state this judge left.
	if h := e.fullChecked.Load(); h != nil {
		(*h)()
	}
}
