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
	stepNone   step = iota // nothing ran: the next refusal repacks by the scope (journal-wide limits only)
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

// repack runs one repack pass over scope at one repack reserve of bytes,
// which the journal sizes to at least a segment's live payload (RFC 8
// section 10.3). The result of the pass is not read: what is left to free is
// read from Stats afterwards.
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

// relieve runs the next step of b's repack path for a refusal by one of the
// journal's own limits and reports whether the call is retried. A step that
// fails with anything but the caller's context leaves the refusal as the
// input, as if it freed nothing, and the error is logged, not answered (RFC 8
// §10.2). A closed journal is returned as it is. A share's own limit runs
// sharePass instead.
//
// ponytail: a journal-wide limit runs its repack path in the caller's
// goroutine, one pass per refusal and none kept in flight per journal; the
// eviction loop's own repack comes with offload. Its syncScope runs per
// refusal too and grows with the files on the journal, so a burst of N
// refusals is 2N repacks and N full-journal syncs. Upgrade to the share
// limit's one pass in flight that later refusals join when repack bytes copied
// per byte freed, or the time a refusal spends in syncScope, rises under a
// refusal storm.
func (e *Engine) relieve(ctx context.Context, b *binding, r refusal, at *step) error {
	scope, bound := e.scopeOf(b, r)

	var err error

	switch *at {
	case stepNone:
		err = e.repack(ctx, b, scope)
		*at = stepRepack
	default:
		if err = e.syncScope(ctx, b, bound); err == nil || ctx.Err() == nil {
			serr := e.repack(ctx, b, scope)
			if err == nil {
				err = serr
			}
		}

		*at = stepLast
	}

	return e.stepOutcome(ctx, b, err)
}

// sharePass is one pass of step 4 over a share's own scope (RFC 8 §10.2):
// the share's sync, then its repack. A refusal of the share's limit joins it
// while its sync has not begun; one that comes later waits for the next pass.
type sharePass struct {
	done    chan struct{}
	err     error // set before done closes: nil to retry, else what the refusal maps
	syncing bool  // under the passes' mu: the sync began, so a refusal now waits for the next
}

// passSlot is a share's pass in flight and the one queued behind it.
type passSlot struct{ cur, next *sharePass }

// sharePasses are the share passes, at most one in flight and one queued per
// share, run under the engine's lifetime rather than any caller's: ctx is
// what Close cancels and wg what it joins, and closed, set under mu when
// Close stops them, keeps any from starting after the join. Each pass is
// also work of its share's gate, so Unbind waits for it. started, nil in
// production, counts every pass started once a test sets it.
type sharePasses struct {
	mu      sync.Mutex
	closed  bool
	slots   map[*binding]*passSlot
	started *atomic.Int64
	ctx     context.Context
	cancel  context.CancelFunc
	wg      sync.WaitGroup
}

// endedPass is a pass that ran nothing and ends with err.
func endedPass(err error) *sharePass {
	p := &sharePass{done: make(chan struct{}), err: err}
	close(p.done)

	return p
}

// passFor is the pass a refusal of b's share limit waits for: the pass in
// flight while its sync has not begun, else the one queued behind it, queued
// now if none is, else a new one, started now. Once Close has stopped the
// passes, or Unbind has shut the share's gate, it is a pass already ended
// with the journal closed.
//
// decision: a refusal that arrives once the pass's sync has begun does not
// join it: a write served after that sync, such as a small overwrite, holds a
// superseded record the sync did not reach, so the figures after the pass
// would count it as nothing and answer no space. It waits for the next pass,
// which syncs it. Overturned if the journal's sync reports which records it
// covered, so a joiner could tell whether its share's writes were among them.
func (e *Engine) passFor(b *binding) *sharePass {
	ps := &e.passing
	ps.mu.Lock()
	defer ps.mu.Unlock()

	sl := ps.slots[b]
	switch {
	case sl == nil:
		if ps.closed || !b.gate.enter() {
			return endedPass(journal.ErrClosed)
		}

		if ps.slots == nil {
			ps.slots = map[*binding]*passSlot{}
		}

		sl = &passSlot{cur: &sharePass{done: make(chan struct{})}}
		ps.slots[b] = sl
		e.startPass(b, sl.cur)

		return sl.cur
	case !sl.cur.syncing:
		return sl.cur
	case sl.next == nil:
		sl.next = &sharePass{done: make(chan struct{})}
	}

	return sl.next
}

// startPass runs p over b's scope in the background, as one unit of b's gate
// that the caller has entered and the pass withdraws when it ends. The shut
// of the gate cancels it, so Unbind's wait for the gate's work ends it, and a
// pass that Close cancelled ends with the journal closed. ps.mu is held.
func (e *Engine) startPass(b *binding, p *sharePass) {
	ps := &e.passing

	if ps.started != nil {
		ps.started.Add(1)
	}
	ps.wg.Add(1)

	go func() {
		defer ps.wg.Done()
		defer b.gate.leave()

		ctx, cancel := context.WithCancel(ps.ctx)
		ended := make(chan struct{})

		go func() {
			select {
			case <-b.gate.shutdown():
				cancel()
			case <-ended:
			}
		}()

		if h := e.seams.passing.Load(); h != nil {
			(*h)()
		}

		ps.mu.Lock()
		p.syncing = true
		ps.mu.Unlock()

		err := e.sharePass(ctx, b)
		close(ended)
		cancel()

		if ps.ctx.Err() != nil {
			err = journal.ErrClosed
		}

		ps.mu.Lock()
		sl := ps.slots[b]
		next := sl.next
		sl.next = nil

		switch {
		case next == nil:
			delete(ps.slots, b)
		case ps.closed || !b.gate.enter():
			delete(ps.slots, b)
			next.err = journal.ErrClosed
			close(next.done)
		default:
			sl.cur = next
			e.startPass(b, next)
		}
		ps.mu.Unlock()

		p.err = err
		close(p.done)
	}()
}

// sharePass syncs every file of b's share, so what an unsynced overwrite
// supersedes becomes unreclaimed, and repacks the share's scope, which seals
// and syncs the segments it closes, only when the share's unreclaimed bytes
// after the sync, active segments included, are above zero: a share of live,
// synced bytes is left untouched, and superseded bytes that sit only in an
// active segment are still sealed and freed.
func (e *Engine) sharePass(ctx context.Context, b *binding) error {
	err := e.syncScope(ctx, b, []*binding{b})
	if !errors.Is(err, journal.ErrClosed) && ctx.Err() == nil &&
		b.j.Stats().Shares[b.share.Tag].UnreclaimedBytes > 0 {
		if serr := e.repack(ctx, b, journal.RepackScope{Share: b.share.Tag}); err == nil {
			err = serr
		}
	}

	return e.stepOutcome(ctx, b, err)
}

// passWaitNoDeadline is the most a refusal whose context has no deadline
// waits for its share's pass: a tenth of the 30 s default request deadline.
//
// decision: a context with no deadline is given the 30 s default request
// deadline, so it waits 3 s, then answers ErrDelay (DelayCapacity), rather
// than without limit: a held or slow sync would otherwise park the caller for
// as long as it lasts, while a pass that nothing holds ends well inside 3 s
// and the write is still served in one call. The bound has a ceiling: every
// call and retry goes through passFor and awaitPass again and reads the
// figures only after a pass it joined ended inside its wait, so a share whose
// sync takes longer than the wait answers ErrDelay (DelayCapacity) to every
// write and every retry while its passes run back to back. Overturned if RFC
// 8 or RFC 23 sets another default request deadline, or if a measured share
// pass p95 comes above the bound.
const passWaitNoDeadline = 3 * time.Second

// awaitPass waits for p to end and reports whether it did: at most a tenth of
// the time left to ctx's deadline by the engine's clock, or
// passWaitNoDeadline with no deadline, and never past ctx's end or the shut
// of b's gate. A pass that has not ended is left running.
func (e *Engine) awaitPass(ctx context.Context, b *binding, p *sharePass) bool {
	select {
	case <-p.done:
		return true
	default:
	}

	wait := passWaitNoDeadline
	if deadline, ok := ctx.Deadline(); ok {
		wait = deadline.Sub(e.d.Clock.Now()) / 10
	}

	select {
	case <-p.done:
		return true
	case <-e.d.Clock.After(wait):
	case <-ctx.Done():
	case <-b.gate.shutdown():
	}

	return false
}

// stopPasses cancels the share passes in flight and joins them; none starts
// after it.
func (e *Engine) stopPasses(b *stopBound) error {
	ps := &e.passing
	ps.mu.Lock()
	ps.closed = true
	ps.mu.Unlock()

	ps.cancel()

	passed := make(chan struct{})

	go func() {
		ps.wg.Wait()
		close(passed)
	}()

	if !b.join(passed, "a share pass") {
		return stuck("a share pass")
	}

	return nil
}

// stepOutcome is what a step of b's repack path that ended with err leaves the
// refusal: nil to retry, as if the step freed nothing when it failed with
// anything but ctx's end, a closed journal or an unimplemented repack, which
// are returned as they are.
func (e *Engine) stepOutcome(ctx context.Context, b *binding, err error) error {
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

	// decision: a removed maximum counts only while the journal reports
	// segments, because a closed journal answers the zero Stats, which reads
	// as unbounded too. No path judges a closed journal today, as the group
	// commits stop before the journals close, so no test reaches this; the
	// guard keeps a refusal standing if a judge ever outlives its journal.
	if st.MaxBytes <= 0 && st.Segments > 0 {
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
