package engine

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/cubbit/dittofs/pkg/block/journal"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/metadata/kvstore"
	"github.com/cubbit/dittofs/pkg/vfs/vfserr"
)

// removal is one removal of a file between its guard's taking and its
// release: its share, its file, the extent it removes and, once the journal
// has removed it, the version the journal gave it.
type removal struct {
	b    *binding
	f    entity.FileID
	x    entity.Extent
	kind entity.RemovalKind
	v    journal.Version
	at   time.Time
	// through, above v, is the newest version phase 1 committed with it, which
	// release settles through: a clone's copies and the writes above it.
	through journal.Version
}

// guard takes file f's guard for a removal of x and marks x in flight,
// waiting, within ctx, for a removal of f already holding it. A removal into
// a clone's registration is answered at once, with nothing taken: a truncate
// or deallocate overlapping a source extent, or of a file holding a
// destination extent, is delayClone(); a release of a file holding any is
// errRegistered. A clone's own guard, on its destination, passes its own
// registration. The guard keeps the entry alive.
//
// decision: the registration check and the in-flight mark sit in one
// section under the entry's lock. A removal that checked before a clone
// registered could otherwise drop the bytes the clone is to read, and a clone
// registering while the mark is set waits until the removal's phase 1 ends.
// Withdraw it only with a clone that reads the source through the journal's
// own snapshot.
func (e *Engine) guard(ctx context.Context, b *binding, f entity.FileID, kind entity.RemovalKind, x entity.Extent) (*removal, error) {
	fe := e.files.lockFor(f)
	for {
		// The registrations are checked before any wait, so a removal of a
		// file a clone holds, whose guard the clone may hold too, is answered
		// at once rather than behind that guard.
		if err := fe.refuseRemoval(kind, x); err != nil {
			e.files.unlock(f, fe)
			return nil, err
		}
		if fe.guardDone == nil {
			break
		}
		var err error
		if fe, err = e.awaitGuard(ctx, f, fe); err != nil {
			return nil, err
		}
	}
	fe.guardDone = make(chan struct{})
	fe.guarded.Store(true)
	fe.removing = &x
	e.files.unlock(f, fe)
	return &removal{b: b, f: f, x: x, kind: kind, at: e.d.Clock.Now()}, nil
}

// refuseRemoval is a removal of kind over x refused by the clones
// registered on the file: errRegistered for a release of a file any holds,
// delayClone() for a truncate or deallocate overlapping a source extent or
// of a file holding a destination extent; nil for a clone's own guard. The
// caller holds the entry's lock.
func (fe *fileEntry) refuseRemoval(kind entity.RemovalKind, x entity.Extent) error {
	switch kind {
	case entity.RemoveClone:
	case entity.RemoveRelease:
		if len(fe.regs) > 0 {
			return errRegistered
		}
	default:
		return fe.refuseChange(x)
	}
	return nil
}

// awaitGuard releases fe, f's entry, and waits for its guard's release,
// answering the entry locked again, or, at ctx's end, its error.
func (e *Engine) awaitGuard(ctx context.Context, f entity.FileID, fe *fileEntry) (*fileEntry, error) {
	ch := fe.guardDone
	e.files.unlock(f, fe)
	select {
	case <-ch:
		return e.files.lockFor(f), nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// begin takes f's guard for a removal of kind over x, as guard does, then
// waits out a batch in flight on f, so no commit that starts after it
// overtakes that batch; the guard is released again if the wait ends first.
func (e *Engine) begin(ctx context.Context, b *binding, f entity.FileID, kind entity.RemovalKind, x entity.Extent) (*removal, error) {
	r, err := e.guard(ctx, b, f, kind, x)
	if err != nil {
		return nil, err
	}
	if err := e.waitBatch(ctx, f); err != nil {
		e.release(r, false)
		return nil, err
	}
	return r, nil
}

// guardOf is the channel closed when f's guard is released, nil when no
// removal holds it.
func (e *Engine) guardOf(f entity.FileID) <-chan struct{} {
	fe := e.files.lockOf(f)
	if fe == nil {
		return nil
	}
	defer e.files.unlock(f, fe)
	if fe.guardDone == nil {
		return nil
	}
	return fe.guardDone
}

// release ends r's hold on its file: the in-flight mark is cleared, the guard
// released and its waiters woken. Then, once its phase 1 committed, the
// journal is settled through it, and the file, if it still holds operations,
// is noted for the group commit, in that order. One whose phase 1 did not
// commit settles only when unstage asks it to, for a terminal failure.
func (e *Engine) release(r *removal, settle bool) {
	fe := e.files.lockFor(r.f)
	fe.removing = nil
	fe.guarded.Store(false)
	close(fe.guardDone)
	fe.guardDone = nil
	var oldest time.Time
	if len(fe.ops) > 0 {
		oldest = fe.ops[0].at
	}
	e.files.unlock(r.f, fe)
	if settle {
		r.b.h.Settle(fileID(r.f), newer(r.v, r.through))
	}
	if !oldest.IsZero() && !r.b.gc.removals.isDropped(r.b.share.ID) {
		_ = r.b.gc.note(r.b, r.f, oldest) // refused only once closed or lost, when nothing commits
	}
}

// Truncate sets file f's size. At or above the size its operations leave, it
// is a grow: the journal's Truncate gives it its version and the group commit
// commits it, with no phase 1. Below, it is a removal: the guard is held
// across the journal's Truncate and phase 1, which commits the file's pending
// existence below the truncate with it, and it answers once phase 1 commits.
//
// decision: a grow needs no phase 1, since it masks nothing: it is one more
// pending operation, the store's OpGrow, committed like a write. Revisit if
// a grow ever has to drop records.
func (e *Engine) Truncate(ctx context.Context, s entity.ShareID, f entity.FileID, size int64) error {
	if size < 0 {
		return vfserr.ErrInvalid
	}
	b, err := e.bindingOf(s)
	if err != nil {
		return err
	}
	r, err := e.begin(ctx, b, f, entity.RemoveTruncate, entity.ExtentOf(size, entity.EndOfFile))
	if err != nil {
		return err
	}
	committed, err := e.d.Existence.Size(ctx, s, f)
	if err != nil {
		e.release(r, false)
		return e.storeErr(err)
	}
	grow, err := e.stage(ctx, r, func(fe *fileEntry) (journal.Version, error) {
		return b.h.Truncate(ctx, fileID(f), size)
	}, opTruncate, journal.Extent{Off: size}, func(fe *fileEntry) bool {
		return size >= replaySize(committed, fe.ops)
	})
	if err != nil || grow {
		e.release(r, false)
		return err
	}
	e.staged()
	return e.phase1(ctx, r, kvstore.RemoveOp{Kind: entity.RemoveTruncate, Extent: r.x})
}

// Deallocate makes [off, off+n) of file f a hole: the journal stops holding
// it and phase 1 records the extent's merged hole at once, staging no zeros.
// A zero length removes nothing.
func (e *Engine) Deallocate(ctx context.Context, s entity.ShareID, f entity.FileID, off, n int64) error {
	if off < 0 || n < 0 || n > entity.EndOfFile-off {
		return vfserr.ErrInvalid
	}
	b, err := e.bindingOf(s)
	if err != nil || n == 0 {
		return err // a share not bound refuses even an empty extent
	}
	r, err := e.begin(ctx, b, f, entity.RemoveDeallocate, entity.Extent{Offset: off, Length: n})
	if err != nil {
		return err
	}
	x := journal.Extent{Off: off, Len: n}
	if _, err := e.stage(ctx, r, func(*fileEntry) (journal.Version, error) {
		return b.h.Deallocate(ctx, fileID(f), x)
	}, opDeallocate, x, nil); err != nil {
		e.release(r, false)
		return err
	}
	e.staged()
	return e.phase1(ctx, r, kvstore.RemoveOp{Kind: entity.RemoveDeallocate, Extent: r.x})
}

// Delete is file f's release, never the journal's eviction: the guard, the
// journal's Delete, then phase 1, which re-checks the file's holders. It
// reads and writes no open state: the filesystem service marked the file
// releasing before calling it. A phase 1 that commits within the call
// answers a nil channel and nil. One a holder aborts answers
// vfserr.ErrFileOpen. One the call cannot commit, the journal's Delete done,
// is pending: it answers a channel and nil, the loop retries it keeping the
// guard, and the channel yields its outcome once: nil once it commits,
// vfserr.ErrFileOpen if a holder aborts it, vfserr.ErrUnavailable if the loop
// ends first. A release of a file a running clone holds is pending before
// the journal's Delete, which it never reaches: see behindClone.
//
// decision: a release holds no lock across the journal's Delete and stages
// no pending operation, unlike a truncate: the filesystem service marks the
// file releasing through open state before it calls Delete, and no write,
// read or open reaches a file so marked. Give it a staged operation if a
// call can ever reach the engine past that mark.
//
// decision: a release is pending, not failed, from the moment the journal's
// Delete returns, as the journal no longer holds the file's bytes and only
// its marker brings them back; before that, the context's end is a refusal
// that changed nothing. Withdraw it only with a journal Delete that can be
// undone.
func (e *Engine) Delete(ctx context.Context, s entity.ShareID, f entity.FileID) (retry <-chan error, err error) {
	b, err := e.bindingOf(s)
	if err != nil {
		return nil, err
	}
	r, err := e.begin(ctx, b, f, entity.RemoveRelease, entity.ExtentOf(0, entity.EndOfFile))
	if errors.Is(err, errRegistered) {
		return e.behindClone(b, f), nil
	}
	if err != nil {
		return nil, err
	}
	v, err := b.h.Delete(ctx, fileID(f))
	if err != nil {
		e.release(r, false)
		return nil, e.journalErr(err)
	}
	r.v = v
	e.issued(b.vl, v)
	e.staged()
	op := kvstore.RemoveOp{Kind: entity.RemoveRelease, Extent: r.x, Version: entity.JournalVersion(v.Bytes()), Time: r.at}
	switch err := e.tryPhase1(ctx, r, op); {
	case err == nil:
		return nil, nil
	case errors.Is(err, kvstore.ErrHeld):
		return nil, e.abort(r)
	case errors.Is(err, entity.ErrStale):
		e.unstage(r, err)
		return nil, nil // a file already gone is released already, its writes pending or not
	case terminal(err):
		e.unstage(r, err)
		return nil, terminalErr(err)
	}
	ch := make(chan error, 1) // buffered: the loop never blocks on a caller gone
	e.retryLater(r, op, func(err error) { ch <- err })
	return ch, nil
}

// errRegistered is guard's answer to a release of a file a clone holds.
var errRegistered = errors.New("engine: a clone holds the file")

// behindClone answers a release of file f that a running clone holds,
// before the journal's Delete: a pending channel, which yields
// ErrDelay(DelayClone) once no clone holds f, for the filesystem service to
// run the release again, or vfserr.ErrUnavailable if f's journal's loop ends
// first. The loop looks again at its backoff and counts no stall: the clone
// holds the file, not a failure.
//
// ponytail: the loop polls the registrations at its backoff rather than the
// clone's end waking the release, so a release behind a clone answers up to
// one backoff late. Wake it from the clone's end if that delay shows.
func (e *Engine) behindClone(b *binding, f entity.FileID) <-chan error {
	ch := make(chan error, 1) // buffered: the loop never blocks on a caller gone
	b.gc.removals.enqueue(&loopItem{b: b, f: f, kind: entity.RemoveRelease,
		run: func(context.Context) answer {
			if e.registered(f) {
				return itemRetryUncounted
			}
			ch <- delayClone()
			return itemDone
		},
		end: func() { ch <- errLoopEnded },
	})
	return ch
}

// registered reports whether a clone holds file f.
func (e *Engine) registered(f entity.FileID) bool {
	fe := e.files.lockOf(f)
	if fe == nil {
		return false
	}
	defer e.files.unlock(f, fe)
	return len(fe.regs) > 0
}

// abort ends release r that a holder refused: the file's pending writes
// below it are dropped, their Dones run, the journal is settled without the removal, the
// loss is raised on the file's sequence and logged naming it, and the guard
// is released. It answers vfserr.ErrFileOpen, which the service tells from
// ErrUnavailable: it clears the file's releasing mark for one and keeps it
// for the other.
//
// decision: the store's ErrHeld crosses as vfserr.ErrFileOpen, which only
// the engine and the filesystem service exchange, so the service's release
// clears its mark for an abort and keeps it for an outage. Withdraw it if a
// release's outcomes ever reach an adapter.
func (e *Engine) abort(r *removal) error {
	fe := e.files.lockFor(r.f)
	var dones []func()
	kept := fe.ops[:0]
	for _, op := range fe.ops {
		switch {
		case op.version.Compare(r.v) >= 0:
			kept = append(kept, op)
		case !op.dropped && op.done != nil:
			dones = append(dones, op.done)
		}
	}
	clear(fe.ops[len(kept):])
	fe.ops = kept
	e.files.unlock(r.f, fe)
	l := &r.b.gc.losses
	l.mu.Lock()
	l.raise(r.f)
	l.mu.Unlock()
	e.d.Logger.Error("a release was aborted by a holder: the file's content is lost", "file", r.f)
	e.release(r, true)
	runAll(dones)
	return vfserr.ErrFileOpen
}

// stage runs the journal's removal j under f's entry lock and inserts its
// pending operation before the lock is released, answering whether grow,
// asked under the same lock, made it a grow rather than a removal.
//
// decision: the entry's lock is held across the journal's Truncate or
// Deallocate and the insert of its operation, unlike a write's, as the
// journal stops holding the extent at once and a read between the two would
// find it missing with no pending removal and read it lost. Reads of the
// file wait out one journal removal. Withdraw it if the journal hands back a
// removal's version before it stops holding the extent.
func (e *Engine) stage(ctx context.Context, r *removal, j func(*fileEntry) (journal.Version, error), kind opKind, x journal.Extent, grow func(*fileEntry) bool) (bool, error) {
	fe := e.files.lockFor(r.f)
	defer e.files.unlock(r.f, fe)
	// A write the journal staged before the removal is in the entry before
	// the removal decides or is staged: a write admitted and not yet inserted
	// is waited out, as a clone's registration waits it out. The wait
	// releases the lock, so no other write is admitted until the removal is
	// staged: one admitted meanwhile would reach the journal first, below the
	// removal's version, and a stream of them would hold the removal, and its
	// guard, to its deadline.
	defer fe.endStaging()
	if err := fe.stageAfterWrites(ctx); err != nil {
		return false, err
	}
	g := grow != nil && grow(fe)
	v, err := j(fe)
	if err != nil {
		return false, e.journalErr(err)
	}
	r.v = v
	fe.insert(pendingOp{version: v, kind: kind, extent: x, at: r.at, grow: g, setsMtime: g}) // a size change moves mtime, up as down
	e.issued(r.b.vl, v)
	return g, nil
}

// replaySize is the size the file's operations leave over committed: a
// truncate sets it, a write raises it to its end.
func replaySize(committed int64, ops []pendingOp) int64 {
	size := committed
	for _, op := range ops {
		switch {
		case op.dropped:
		case op.kind == opTruncate:
			size = op.extent.Off
		case op.kind == opWrite, op.kind == opClone:
			size = max(size, opEnd(op.extent))
		}
	}
	return size
}

// phase1 is the removal r's first metadata transaction, op at r's version,
// run once the caller waited out a batch in flight on the file: it holds the file's pending
// operations below the removal, syncs the file, and hands them to the
// store's Remove with the removal. Once that commits, the held operations
// and the removal's own leave the entry and their Dones run, and the guard
// is released; a failure releases the hold and answers the store's error,
// a conflict at ctx's end DelayConflict.
//
// decision: phase 1 holds only the operations below the removal's version.
// One staged between the journal's removal and phase 1 is above it and stays
// pending: the store's Remove skips a removal at or below the version the
// file has applied, so committing an operation above it first would lose the
// removal. Withdraw it if the store ever applies a removal below applied.
func (e *Engine) phase1(ctx context.Context, r *removal, op kvstore.RemoveOp) error {
	op.Version, op.Time = entity.JournalVersion(r.v.Bytes()), r.at
	err := e.tryPhase1(ctx, r, op)
	switch {
	case err == nil:
		return nil
	case terminal(err):
		e.unstage(r, err)
		return terminalErr(err)
	}
	// decision: once the journal has removed the extent, a phase 1 the call
	// cannot commit is not failed: the journal's loop retries it, keeping the
	// guard and the in-flight mark, and the call answers retry-later. A file
	// found gone, or a removal the store refuses as invalid, ends it, its
	// marker settled without the removal (RFC 8 §8.1).
	e.retryLater(r, op, nil)
	if errors.Is(err, kvstore.ErrConflict) {
		return &vfserr.Delay{Cause: vfserr.DelayConflict}
	}
	return &vfserr.Delay{Cause: vfserr.DelayCommit}
}

// tryPhase1 runs r's phase 1 once: the hold, the sync, the store's Remove,
// then, once it commits, the held operations and the removal's own out
// of the entry, r finished and the Dones run. A failure releases the hold
// and keeps the guard.
func (e *Engine) tryPhase1(ctx context.Context, r *removal, op kvstore.RemoveOp) error {
	e.holdBelow(r)
	if err := e.sync(ctx, r.b, fileID(r.f)); err != nil {
		e.unhold(r)
		return err
	}
	if err := e.d.Existence.Remove(ctx, r.b.share.ID, r.f, 0, op, e.held(r)); err != nil {
		e.unhold(r)
		return err
	}
	dones := e.committed(r)
	e.finish(r)
	runAll(dones)
	return nil
}

// terminal reports a phase 1 failure that retrying cannot change: the file
// gone, or the removal refused as invalid.
func terminal(err error) bool {
	return errors.Is(err, entity.ErrStale) || kvstore.Refused(err)
}

// terminalErr answers a call whose phase 1 ended on err, terminal: the file
// gone is stale, and a refusal, which unstage logged, is invalid rather than
// unavailable, as retrying it fails the same way.
func terminalErr(err error) error {
	if errors.Is(err, entity.ErrStale) {
		return fmt.Errorf("engine: removal: %v: %w", err, vfserr.ErrStale)
	}
	return fmt.Errorf("engine: removal refused: %v: %w", err, vfserr.ErrInvalid)
}

// unstage ends removal r whose phase 1 will not commit: its own pending
// operation leaves the entry, so no Commit waits on it and the file's other
// operations commit. A phase 1 that ended on a terminal failure, err, settles
// the journal's marker without the removal (RFC 8 §8.1), so no later start
// re-applies it. For a file still there, the settle waits for the existence
// of the file's operations below the removal, a lost write's included,
// committed by commitBelow, and the loss is logged; a commit that fails is
// retried by the journal's loop, the guard held and the marker unsettled
// until it commits. A file found gone is discarded at once.
func (e *Engine) unstage(r *removal, err error) {
	fe := e.files.lockFor(r.f)
	fe.ops = slices.DeleteFunc(fe.ops, func(op pendingOp) bool { return op.version == r.v })
	e.files.unlock(r.f, fe)
	if errors.Is(err, entity.ErrStale) {
		e.release(r, true)
		return
	}
	e.d.Logger.Error("removal ended unapplied, its journal marker settled", "file", r.f, "kind", r.kind.String(), "err", err)
	if e.commitBelow(r.b.gc.removals.ctx, r) {
		return
	}
	r.b.gc.removals.enqueue(&loopItem{b: r.b, f: r.f, kind: r.kind,
		run: func(ctx context.Context) answer {
			if e.commitBelow(ctx, r) {
				return itemDone
			}
			return itemRetry
		},
		// The loop's end leaves the file and its marker to the next start,
		// as it leaves a phase 1 it cut short.
	})
}

// commitBelow commits, in a transaction of its own, the existence of r's
// file's pending operations below r, a lost write's from its loss record,
// once r ended unapplied, then releases the guard and settles the journal
// through r: the settle then drops no write's or loss's record that metadata
// never recorded (RFC 1 §3.10). A file found gone settles with nothing
// committed, and operations the store refuses are dropped as lost. It
// answers false, the hold released and the guard held, when the commit
// failed otherwise.
func (e *Engine) commitBelow(ctx context.Context, r *removal) bool {
	ctx, cancel := e.bounded(ctx)
	defer cancel()
	e.holdBelow(r)
	err := e.sync(ctx, r.b, fileID(r.f))
	if ops := e.held(r); err == nil && len(ops) > 0 {
		var failed map[entity.FileID]error
		failed, err = e.d.Existence.Commit(ctx, []kvstore.PendingExistence{{Share: r.b.share.ID, File: r.f, Ops: ops}}, nil)
		if err == nil {
			err = failed[r.f]
		}
	}
	switch {
	case err == nil:
		dones := e.committed(r)
		e.release(r, true)
		runAll(dones)
		return true
	case errors.Is(err, entity.ErrStale):
		e.unhold(r)
		e.release(r, true)
		return true
	case kvstore.Refused(err):
		// The store refuses these operations themselves, so committing them
		// again fails the same way: they leave the entry, logged as lost, and
		// the journal settles through r without them.
		e.d.Logger.Error("pending operations below a removal refused by the store, dropped as lost", "file", r.f, "kind", r.kind.String(), "err", err)
		dones := e.dropHeld(r)
		e.release(r, true)
		runAll(dones)
		return true
	}
	e.unhold(r)
	return false
}

// dropHeld removes from r's file's entry the operations r held, which
// the store refused, and answers their Dones, charging nothing.
func (e *Engine) dropHeld(r *removal) (dones []func()) {
	fe := e.files.lockFor(r.f)
	defer e.files.unlock(r.f, fe)
	fe.ops = slices.DeleteFunc(fe.ops, func(op pendingOp) bool {
		if op.held && !op.dropped && op.done != nil {
			dones = append(dones, op.done)
		}
		return op.held
	})
	return dones
}

// holdBelow holds r's file's live operations below r's version, writes and
// grows, for phase 1. The hold is phase 1's own, apart from a batch's claim:
// a batch that captures a held operation, and fails, releases its claim and
// leaves the hold, so phase 1 still commits it; one that commits it first
// leaves phase 1's commit to skip it as applied.
func (e *Engine) holdBelow(r *removal) {
	fe := e.files.lockFor(r.f)
	defer e.files.unlock(r.f, fe)
	for i := range fe.ops {
		op := &fe.ops[i]
		if op.version.Compare(r.v) >= 0 {
			break
		}
		if !op.dropped {
			op.held = true
		}
	}
}

// held is r's held operations as the store commits them, read after
// the sync, so a loss its drain marked commits lost.
func (e *Engine) held(r *removal) []kvstore.PendingOp {
	fe := e.files.lockFor(r.f)
	defer e.files.unlock(r.f, fe)
	var out []kvstore.PendingOp
	for i, op := range fe.ops {
		if !op.held || op.dropped {
			continue
		}
		var mtime time.Time
		if op.setsMtime {
			mtime = op.at
		}
		ps := storeOp(op, mtime)
		ps.Over = superseded(fe.ops, i)
		out = append(out, ps)
	}
	return out
}

// unhold releases r's hold after a failed phase 1: the operations stay
// pending, and a batch's claim on any is left to that batch.
func (e *Engine) unhold(r *removal) {
	fe := e.files.lockFor(r.f)
	defer e.files.unlock(r.f, fe)
	for i := range fe.ops {
		fe.ops[i].held = false
	}
}

// committed removes from r's file's entry, after its phase 1 committed, the
// operations it held and its own, and a clone's copies, which its phase 1
// recorded through Copied, before the guard is released, so no batch
// captures any: it answers the held writes' Dones. A held
// write a failed window dropped leaves too, its Done having run then.
func (e *Engine) committed(r *removal) (dones []func()) {
	fe := e.files.lockFor(r.f)
	defer e.files.unlock(r.f, fe)
	kept := fe.ops[:0]
	for _, op := range fe.ops {
		switch {
		case op.held:
			if !op.dropped {
				if op.done != nil {
					dones = append(dones, op.done)
				}
				e.pendingApplied.Add(1)
			}
		case op.version == r.v, op.copy:
		default:
			kept = append(kept, op)
		}
	}
	clear(fe.ops[len(kept):])
	fe.ops = kept
	e.files.removals.Add(1) // committed operations left the entry: see Allocation
	return dones
}

// staged runs a test's seam between a removal's journal step and its phase 1.
func (e *Engine) staged() {
	if h := e.hooks.staged.Load(); h != nil {
		(*h)()
	}
}
