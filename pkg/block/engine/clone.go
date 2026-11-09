package engine

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/cubbit/dittofs/pkg/block/journal"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/metadata/kvstore"
	"github.com/cubbit/dittofs/pkg/vfs/vfserr"
)

// reg is one running clone's registration on a file: the extent it holds,
// and whether the file is the clone's destination or its source.
type reg struct {
	entity.Extent
	dst bool
}

// delayClone is the answer to a call behind a running clone.
//
// decision: a call behind a clone answers at once and the engine never waits
// inside it, since the filesystem service holds the file's admission and
// accept locks across the call and holds it behind the clone itself (RFC 17
// §5.9), and a call held behind a clone holds no entry mutex (RFC 14 §11).
// Withdraw it if RFC 17 moves that hold into the engine.
func delayClone() error { return &vfserr.Delay{Cause: vfserr.DelayClone} }

// refuseWrite answers delayClone() if x of the file overlaps an extent a
// clone registered on it, source or destination; nil otherwise. The caller
// holds the entry's lock.
func (fe *fileEntry) refuseWrite(x entity.Extent) error {
	for _, r := range fe.regs {
		if r.Overlaps(x) {
			return delayClone()
		}
	}
	return nil
}

// refuseRead answers delayClone() if x of the file overlaps an extent a
// clone registered on it as its destination, whose content the clone is
// changing; a source extent reads as it is. The caller holds the entry's lock.
func (fe *fileEntry) refuseRead(x entity.Extent) error {
	for _, r := range fe.regs {
		if r.dst && r.Overlaps(x) {
			return delayClone()
		}
	}
	return nil
}

// refuseChange answers delayClone() if a change to x of the file, a clone
// into it or a truncate or deallocate of it, meets a clone registered on it:
// one overlapping x, source or destination, or any clone into the file,
// which holds one clone into it at a time and must not change under it. The
// caller holds the entry's lock.
func (fe *fileEntry) refuseChange(x entity.Extent) error {
	for _, r := range fe.regs {
		if r.dst || r.Overlaps(x) {
			return delayClone()
		}
	}
	return nil
}

// leave removes x, one admitted write, from those writing, and wakes a
// registration waiting for it. The caller holds the entry's lock.
func (fe *fileEntry) leave(x entity.Extent) {
	if i := slices.Index(fe.writing, x); i >= 0 {
		fe.writing = slices.Delete(fe.writing, i, i+1)
	}
	fe.staged.Broadcast()
}

// register installs r on f's entry and waits until every write admitted
// before it into r's extent has its operation in the entry, so the clone
// reads the file as those writes leave it. A destination extent is refused
// delayClone() where it overlaps any registration on f, as a write there
// would be, and on a file another clone already writes into, which holds
// one clone into it at a time; a source extent only where it overlaps a
// destination extent, so
// several clones may share a source, none of them changing it. The
// registration keeps the entry alive. It answers ctx's error, registering
// nothing, if ctx ends first.
func (e *Engine) register(ctx context.Context, f entity.FileID, r *reg) error {
	fe := e.files.lockFor(f)
	for {
		// The registrations are checked before any wait, so an extent a
		// running clone holds, whose guard that clone may hold, is answered
		// at once rather than behind the guard.
		var err error
		if r.dst {
			err = fe.refuseChange(r.Extent)
		} else {
			err = fe.refuseRead(r.Extent)
		}
		if err != nil {
			e.files.unlock(f, fe)
			return err
		}
		// A removal in flight over the extent is waited out: it checked the
		// file's registrations before this one, so its phase 1 must end
		// before the clone reads the file.
		if fe.removing == nil || !fe.removing.Overlaps(r.Extent) {
			break
		}
		if fe, err = e.awaitGuard(ctx, f, fe); err != nil {
			return err
		}
	}
	fe.regs = append(fe.regs, r)
	err := fe.awaitWrites(ctx, func() bool { return slices.ContainsFunc(fe.writing, r.Overlaps) })
	if err != nil {
		fe.drop(r)
	}
	e.files.unlock(f, fe)
	return err
}

// unregister removes r from f's entry, which it kept alive.
func (e *Engine) unregister(f entity.FileID, r *reg) {
	fe := e.files.lockOf(f)
	if fe == nil {
		return
	}
	fe.drop(r)
	e.files.unlock(f, fe)
}

// drop removes r from the entry's registrations. The caller holds its lock.
func (fe *fileEntry) drop(r *reg) {
	if i := slices.Index(fe.regs, r); i >= 0 {
		fe.regs = slices.Delete(fe.regs, i, i+1)
	}
}

// Clone makes x of dst the content of x of src, atomically, answered when
// done. Each file is named with its own share: on one node a clone between
// two shares of one namespace reaches the engine.
//
// Admission refuses, before any journal or store call, a share with no
// binding on either side (DelayFrozen), a zero or negative length or offset,
// overlapping extents of one file and a length over clone_max_len
// (ErrInvalid), two write namespaces (ErrCross with CrossNamespace), and an
// extent a running clone holds (DelayClone). It then registers the source
// extent and the destination extent. Nothing joins a running clone at
// Unbind or Close yet, so it then undoes both and answers errUnbuilt; with a
// test's stand-in it runs on: step 1 on the call (prepare), whose refusal
// undoes both registrations and changes nothing, then from the journal step
// on its own goroutine (runClone) and the removal loop (clonePhase2), where
// it fails only if the loop ends first, answering ErrUnavailable, until its
// phase 2 is done and its hold on the source released; the call waits for it
// within its own context, whose end, a deadline or a cancel, answers
// DelayClone and leaves the clone running, as the clone may already have
// changed the destination and only a call that changed nothing answers
// refused.
//
// decision: a clone between two shares on one node is admitted, as both
// shards are this node's and no failover or handover can split them while
// the clone runs; RFC 8 §9.1 refuses a clone across shards only in a cluster.
// Withdraw it when a node can lose one of the two shards mid-clone.
//
// decision: an incomplete source is refused by the filesystem service (RFC
// 17 §5.9 step 2), which reads the File record: the engine reads none, and
// the store keeps no incomplete flag for it to check.
func (e *Engine) Clone(ctx context.Context, srcShare entity.ShareID, src entity.FileID, dstShare entity.ShareID, dst entity.FileID, x vfserr.Extent) error {
	sb, err := e.bindingOf(srcShare)
	if err != nil {
		return err
	}
	db, err := e.bindingOf(dstShare)
	if err != nil {
		return err
	}
	st := e.settings.Load()
	switch {
	case st == nil:
		return fmt.Errorf("engine: no settings applied: %w", vfserr.ErrUnavailable)
	case x.Len <= 0 || x.SrcOff < 0 || x.DstOff < 0 || x.Len > entity.EndOfFile-max(x.SrcOff, x.DstOff):
		return vfserr.ErrInvalid
	case src == dst && entity.Extent{Offset: x.SrcOff, Length: x.Len}.Overlaps(entity.Extent{Offset: x.DstOff, Length: x.Len}):
		return vfserr.ErrInvalid
	case x.Len > st.CloneMaxLen:
		return vfserr.ErrInvalid
	}
	// decision: the namespace check compares the two shares' write
	// namespaces, the one each Share names. A share listing two during a
	// re-home (RFC 27) needs the whole list, which the store's phase 1 also
	// compares, as a backstop; widen it here once re-homes are built.
	//
	// decision: the check holds only once Bind fills Share.Namespace. Until
	// then every bound share carries the zero namespace, so two shares of
	// different namespaces compare equal here and are refused only by the
	// store's phase 1, after the journal step. Bind's test must then clone
	// between two shares Bind gave different namespaces and see ErrCross
	// with no journal call.
	if sb.share.Namespace != db.share.Namespace {
		return &vfserr.CrossError{Kind: vfserr.CrossNamespace}
	}
	rs := &reg{Extent: entity.Extent{Offset: x.SrcOff, Length: x.Len}}
	rd := &reg{Extent: entity.Extent{Offset: x.DstOff, Length: x.Len}, dst: true}
	if err := e.register(ctx, src, rs); err != nil {
		return err
	}
	if err := e.register(ctx, dst, rd); err != nil {
		e.unregister(src, rs)
		return err
	}
	if !e.fakeCloneDone.Load() {
		// A clone stops once registered while nothing joins a running one:
		// a test of admission alone runs its hook first, and a test's
		// stand-in lets it run on.
		//
		// decision: everything past this gate, step 1, the journal step, the
		// copies, phase 1, the hold on the source, phase 2, the release and
		// the done signal, runs only under the test's stand-in, never in a
		// served engine: the clone's goroutine and its loop items are joined
		// by nothing, so Close and Unbind would leave one running, and a
		// retry with the same spec starts a second clone rather than
		// joining the first. Withdraw it when the running clones are kept
		// where a retry, Unbind and Close join them.
		if h := e.afterRegister; h != nil {
			h()
		}
		e.unregister(dst, rd)
		e.unregister(src, rs)
		return errUnbuilt
	}
	c := &cloneRun{sb: sb, db: db, src: src, dst: dst, rs: rs, rd: rd, done: make(chan struct{})}
	if err := e.prepare(ctx, c, x); err != nil {
		e.endClone(c, err)
		return err
	}
	go e.runClone(c)
	select {
	case <-c.done:
		return c.err
	case <-ctx.Done():
		return delayClone() // it may have changed the destination: never answered as refused
	}
}

// cloneRun is one clone past its admission: its two files and bindings,
// their registrations, its spec, the source's held runs it copies, one copy
// write each, the reservation taken for them, and once its copies are
// synced, the newest copy's version.
type cloneRun struct {
	sb, db   *binding
	src, dst entity.FileID
	rs, rd   *reg
	spec     journal.CloneSpec
	runs     []journal.Held
	res      Reservation // nil when none is held
	copied   bool        // the copies are written and synced
	newest   journal.Version
	done     chan struct{}
	err      error
}

// copyBuf is the clone's copy buffer: the source is resolved and copied in
// windows of it.
const copyBuf = 1 << 20

// prepare is the clone's step 1, on the call: the source's pending existence
// committed, which syncs it; a source extent past the source's committed end
// refused ErrInvalid; a clone not done into the destination or over the
// source extent, by the store's own read, DelayClone; then the sizing pass
// and the reservation (size).
// Nothing it does changes the destination.
func (e *Engine) prepare(ctx context.Context, c *cloneRun, x vfserr.Extent) error {
	if err := e.commitNow(ctx, c.sb, c.src); err != nil {
		return e.waitErr(c.sb, c.src, err)
	}
	size, err := e.d.Existence.Size(ctx, c.sb.share.ID, c.src)
	if err != nil {
		return e.storeErr(err)
	}
	if x.SrcOff+x.Len > size {
		return vfserr.ErrInvalid
	}
	// The store's phase 1 delays a clone while one not done still writes
	// into the destination or over the source extent; read here, it delays
	// the call instead, as nothing may refuse the clone past its journal step.
	if err := e.d.Existence.CloneFree(ctx, kvstore.CloneOp{Share: c.db.share.ID, File: c.dst,
		Spec: entity.CloneSpec{SrcShare: c.sb.share.ID, Src: c.src, SrcOff: x.SrcOff, DstOff: x.DstOff, Len: x.Len}}); err != nil {
		return e.storeErr(err)
	}
	c.spec = journal.CloneSpec{SrcShare: journal.ShareID(c.sb.share.ID), Src: fileID(c.src),
		SrcOff: x.SrcOff, DstOff: x.DstOff, Len: x.Len}
	asOf, err := e.size(ctx, c)
	if err != nil {
		return e.journalErr(err)
	}
	c.spec.AsOf = asOf
	return nil
}

// size resolves the source extent in windows of copyBuf, keeping its held
// runs, and reserves on the destination's handle every byte and write the
// copies need, so no copy is refused part-way. It answers the newest
// version the source was read at.
//
// ponytail: a clone reads its source's held bytes twice, here to size them
// and in its copies. Give the handle RFC 1 §3.7's Extents to size without
// reading when clone time on journal-held sources shows in a profile.
func (e *Engine) size(ctx context.Context, c *cloneRun) (journal.Version, error) {
	end := c.spec.SrcOff + c.spec.Len
	buf := make([]byte, min(copyBuf, c.spec.Len))
	var asOf journal.Version
	var n int64
	c.runs = c.runs[:0]
	for off := c.spec.SrcOff; off < end; off += int64(len(buf)) {
		w := buf[:min(int64(len(buf)), end-off)]
		_, _, held, at, err := c.sb.h.ReadAt(ctx, fileID(c.src), off, w)
		if err != nil {
			return journal.Version{}, err
		}
		for _, h := range held {
			n += h.Extent.Len
		}
		c.runs = append(c.runs, held...)
		asOf = newer(asOf, at)
	}
	if len(c.runs) > 0 {
		res, err := c.db.h.Reserve(n, len(c.runs))
		if err != nil {
			return journal.Version{}, err
		}
		c.res = res
	}
	return asOf, nil
}

// runClone is the clone from its journal step on, on its own goroutine under
// the destination journal's loop context: the destination's guard, a batch in
// flight on it waited out, CloneTarget with the clone's operation in the
// entry, then what attempt runs. A failure before CloneTarget changes nothing
// and fails the clone; from CloneTarget on it never fails: what attempt
// cannot finish, the loop retries, the guard held.
//
// ponytail: the goroutine is not tracked, so nothing joins it at Unbind or
// Close but the loop context's end, and it may call the destination's handle
// or the store after Close; the engine's map of running clones, which
// retries, Unbind and Close join, replaces it.
func (e *Engine) runClone(c *cloneRun) {
	ctx := c.db.gc.removals.ctx
	r, err := e.begin(ctx, c.db, c.dst, entity.RemoveClone, c.rd.Extent)
	if err == nil {
		dx := journal.Extent{Off: c.spec.DstOff, Len: c.spec.Len}
		if _, err = e.stage(ctx, r, func(*fileEntry) (journal.Version, error) {
			return c.db.h.CloneTarget(ctx, fileID(c.dst), c.spec)
		}, opClone, dx, nil); err != nil {
			e.release(r, false)
		}
	}
	if err != nil { // before the journal step: nothing changed
		if errors.Is(err, context.Canceled) {
			err = errLoopEnded
		}
		e.endClone(c, err)
		return
	}
	if e.attempt(ctx, c, r) == itemRetry {
		e.retryClone(c, r)
	}
}

// attempt runs what the clone has left past its journal step: its copies
// and the destination's sync, unless done, then its phase 1, bounded by the
// conflict bound. It answers itemDone once its phase 1 committed, its phase
// 2 posted to the loop, or once the clone ended; itemRetry for the loop to
// run it again.
//
// decision: once the journal step dropped the destination extent no undo
// exists, so a copy, a sync or a phase 1 that fails is retried by the loop
// at its backoff, the guard held and both files registered, and five
// failures in a row raise removal_stalled. A copy refused because a loss
// split a run past what was reserved is retried after sizing and reserving
// again. A failure that never clears pins the destination's guard and both
// registrations until Close, which leaves the clone to the next start's
// rebuild from the journal's marker. Withdraw it if a clone can ever be
// undone after its journal step.
func (e *Engine) attempt(ctx context.Context, c *cloneRun, r *removal) answer {
	if !c.copied {
		if err := e.copyAll(ctx, c); err != nil {
			if ctx.Err() == nil {
				e.d.Logger.Error("a clone's copies failed after its journal step: retried", "src", c.src, "dst", c.dst, "err", err)
			}
			return itemRetry
		}
		c.copied = true
		e.staged()
	}
	pctx, cancel := e.bounded(ctx)
	defer cancel()
	err := e.tryClonePhase1(pctx, c, r)
	switch {
	case err == nil:
		c.db.gc.removals.enqueue(e.clonePhase2(c, r))
		return itemDone
	case errors.Is(err, entity.ErrStale):
		// The store answers stale for either file gone; only the
		// destination's end ends the clone, its record gone with it.
		if _, serr := e.d.Existence.Size(ctx, c.db.share.ID, c.dst); errors.Is(serr, entity.ErrStale) {
			e.cloneEnded(c, r, err)
			return itemDone
		}
		// decision: a source gone after the journal step is retried, the
		// guard held, as nothing may fail past it; until phase 1 writes the
		// source's durable open, the source's registration answers its
		// release pending, so only a path that bypasses the engine reaches
		// it. Withdraw it if such a path is built.
		e.d.Logger.Error("a clone's source went after its journal step: retried", "src", c.src, "dst", c.dst, "err", err)
	case errors.Is(err, entity.ErrDelay):
		e.d.Logger.Warn("a clone's phase 1 was delayed by a clone not done: retried", "src", c.src, "dst", c.dst, "err", err)
	case kvstore.Refused(err):
		e.d.Logger.Error("a clone's phase 1 was refused after its journal step: retried", "src", c.src, "dst", c.dst, "err", err)
	}
	return itemRetry
}

// copyAll writes the clone's copies through a reservation, sized again
// unless step 1's is still held, syncs the destination and closes the
// reservation.
func (e *Engine) copyAll(ctx context.Context, c *cloneRun) error {
	if c.res == nil {
		if _, err := e.size(ctx, c); err != nil {
			return err
		}
	}
	newest, err := e.copyRuns(ctx, c)
	if err == nil {
		err = e.sync(ctx, c.db, fileID(c.dst))
	}
	if c.res != nil {
		if cerr := c.res.Close(); err == nil && cerr != nil {
			err = cerr
		}
		c.res = nil
	}
	c.newest = newer(c.newest, newest) // a failed attempt's copies are committed too, by the one that succeeds
	return err
}

// copyRuns copies every source run size found held into the destination,
// through the reservation, each a pending write of the destination's above
// the clone's version, with no Done, as no client write is charged; it
// answers the newest copy's version. A run the journal no longer holds is
// copied as far as it still holds it: the rest is the source's uncarved
// content, which the destination reads lost, never zeros.
//
// decision: a clone copies the source's unoffloaded bytes through the
// journal rather than offloading them first, so a clone never waits on the
// remote tier; the price is journal capacity, a second upload of those
// bytes, and source writes held for one clone's length, capped by
// clone_max_len for CLONE, by one chunk for a copy. Offload first instead,
// as one path, if clones of large dirty extents are shown to push the
// journal into pacing.
func (e *Engine) copyRuns(ctx context.Context, c *cloneRun) (journal.Version, error) {
	var newest journal.Version
	if len(c.runs) == 0 {
		return newest, nil
	}
	now := e.d.Clock.Now()
	buf := make([]byte, copyBuf)
	for _, run := range c.runs {
		p := buf[:run.Extent.Len]
		_, _, held, _, err := c.sb.h.ReadAt(ctx, fileID(c.src), run.Extent.Off, p)
		if err != nil {
			return newest, err
		}
		for _, h := range held {
			at := h.Extent.Off - run.Extent.Off
			off := c.spec.DstOff + h.Extent.Off - c.spec.SrcOff
			v, err := c.res.WriteAt(ctx, fileID(c.dst), off, p[at:at+h.Extent.Len], now.UnixNano())
			if err != nil {
				return newest, err
			}
			fe := e.files.lockFor(c.dst)
			fe.insert(pendingOp{version: v, kind: opWrite, extent: journal.Extent{Off: off, Len: h.Extent.Len},
				at: now, setsMtime: true, copy: true})
			e.files.unlock(c.dst, fe)
			newest = newer(newest, v)
		}
	}
	return newest, nil
}

// tryClonePhase1 runs the clone's phase 1 once: the destination's pending
// operations held, the copies and the clone's own aside, the destination
// synced, and the store's Clone handed them; once it commits, they leave the
// entry with the clone's operation and its copies, the guard is released and
// the journal settled through the newest version it committed, and the
// held writes' Dones run. A failure releases the hold and keeps the guard.
func (e *Engine) tryClonePhase1(ctx context.Context, c *cloneRun, r *removal) error {
	if err := e.holdAll(ctx, r); err != nil {
		return err
	}
	if err := e.sync(ctx, c.db, fileID(c.dst)); err != nil {
		e.unhold(r)
		return err
	}
	op := kvstore.CloneOp{Share: c.db.share.ID, File: c.dst, Version: entity.JournalVersion(r.v.Bytes()),
		Copied: entity.JournalVersion(c.newest.Bytes()), Time: r.at,
		Spec: entity.CloneSpec{SrcShare: c.sb.share.ID, Src: c.src, SrcOff: c.spec.SrcOff, DstOff: c.spec.DstOff,
			Len: c.spec.Len, AsOf: entity.JournalVersion(c.spec.AsOf.Bytes())}}
	if err := e.d.Existence.Clone(ctx, op, e.held(r)); err != nil {
		e.unhold(r)
		return err
	}
	dones := e.committed(r)
	r.through = newer(r.through, c.newest)
	e.release(r, true)
	runAll(dones)
	return nil
}

// holdAll holds r's file's live operations but r's own and a clone's
// copies, for a clone's phase 1, once no write admitted to the file is
// still in the journal. A write staged between the clone's version and its
// copies' newest that phase 1 did not hold would be skipped as applied by
// every later commit once the store's Clone sets applied past it; the hold
// is what has phase 1 commit it. No new write is admitted while it waits,
// as stage holds them, so a stream of writes cannot hold phase 1 off.
func (e *Engine) holdAll(ctx context.Context, r *removal) error {
	fe := e.files.lockFor(r.f)
	defer e.files.unlock(r.f, fe)
	defer fe.endStaging()
	if err := fe.stageAfterWrites(ctx); err != nil {
		return err
	}
	for i := range fe.ops {
		op := &fe.ops[i]
		if !op.dropped && !op.copy && op.version != r.v {
			op.held = true
			r.through = newer(r.through, op.version)
		}
	}
	return nil
}

// retryClone hands the rest of the clone, which its goroutine could not
// finish, to the destination journal's loop, which runs attempt again at its
// backoff, the guard held, so no group commit raises applied above the
// clone. A destination found gone ends it; the loop's end leaves it to the
// next start.
//
// decision: the store's phase 1 refuses an overlap, a source past its end
// and two namespaces, and delays a clone not done into the destination or
// over the source extent, after the journal step RFC 8 §9.1 says no refusal
// may follow. Admission refuses the first three and step 1 reads the last
// two (CloneFree), so one is met here only when the store changed under a
// registered extent by a path this part does not have, the rebuild at start
// among them; met anyway, it is logged, counted a failed batch toward
// removal_stalled, retried and never fails or undoes. One that never clears
// pins the destination's guard, which a group commit's capture skips.
// Withdraw it once phase 1 runs no check admission does not run first.
func (e *Engine) retryClone(c *cloneRun, r *removal) {
	c.db.gc.removals.enqueue(&loopItem{b: c.db, f: c.dst, kind: entity.RemoveClone,
		run: func(ctx context.Context) answer { return e.attempt(ctx, c, r) },
		end: func() { e.endClone(c, errLoopEnded) },
	})
}

// cloneEnded ends a clone whose phase 1 will not commit as its destination
// is gone, err its store error. The destination loses the clone's copies
// with its own operation: committed as writes, they would raise the
// destination's applied above the clone, and the next start's rebuild of it
// would then be skipped as applied. Both registrations go, and the call
// answers why.
//
// A loop's end, as Close's stop of the work queue, ends the clone with
// endClone alone: the destination is left as it is, its operations, the
// clone's and its copies included, staying in the entry and its guard held,
// so no group commit after it commits any, and the journal's marker leaves
// the clone to the next start, as a removal's cut short phase 1 is left
// (RFC 8 §2.5 step 2).
func (e *Engine) cloneEnded(c *cloneRun, r *removal, err error) {
	fe := e.files.lockFor(c.dst)
	fe.ops = slices.DeleteFunc(fe.ops, func(op pendingOp) bool { return op.copy })
	e.files.unlock(c.dst, fe)
	e.unstage(r, err)
	e.endClone(c, e.storeErr(err))
}

// endClone ends c, answering err: its reservation, when one is held, is
// given back, both registrations go, which a release answered pending
// behind them sees at its next look, and its done signal is raised. A
// clone ends with a nil err once its phase 2 is done and its hold on the
// source released (clonePhase2).
func (e *Engine) endClone(c *cloneRun, err error) {
	if c.res != nil {
		_ = c.res.Close()
		c.res = nil
	}
	e.unregister(c.dst, c.rd)
	e.unregister(c.src, c.rs)
	c.err = err
	close(c.done)
}
