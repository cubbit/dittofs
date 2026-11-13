package engine

import (
	"container/list"
	"context"
	"errors"
	"fmt"
	"iter"
	"slices"
	"sync/atomic"

	"github.com/cubbit/dittofs/pkg/block/journal"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/metadata/kvstore"
	"github.com/cubbit/dittofs/pkg/vfs/vfserr"
)

// reg is one running clone's registration on a file: the extent it holds,
// whether the file is the clone's destination or its source, and whether
// the clone waits for journal space. A destination registration names its
// clone, which a retry of it finds there (joinable) for as long as the
// registration is installed.
type reg struct {
	entity.Extent
	dst   bool
	run   *cloneRun   // the clone, on a destination registration
	space atomic.Bool // the clone waits for journal space to copy: a call behind it carries DelayCapacity
}

// delay is the answer to a call behind r: DelayCapacity while its clone
// waits for journal space, DelayClone otherwise.
func (r *reg) delay() error {
	if r.space.Load() {
		return &vfserr.Delay{Cause: vfserr.DelayCapacity}
	}
	return delayClone()
}

// delayClone is the answer to a call behind a running clone.
//
// decision: a call behind a clone answers at once and the engine never waits
// inside it: RFC 17 §5.9 puts the held wait, bounded by the call's WithHold,
// in the filesystem service, which holds none of the file's locks across a
// clone past its admission (WithAdmitted), and a call held behind a clone
// holds no entry mutex (RFC 14 §11). Withdraw it if RFC 17 moves that hold
// into the engine.
func delayClone() error { return &vfserr.Delay{Cause: vfserr.DelayClone} }

// refuseWrite answers delayClone() if x of the file overlaps an extent a
// clone registered on it, source or destination; nil otherwise. The caller
// holds the entry's lock.
func (fe *fileEntry) refuseWrite(x entity.Extent) error {
	for _, r := range fe.regs {
		if r.Overlaps(x) {
			return r.delay()
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
			return r.delay()
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
			return r.delay()
		}
	}
	return nil
}

// running is the clone registered into the file, nil when none is: a file
// takes one clone into it at a time (refuseChange). The caller holds the
// entry's lock.
func (fe *fileEntry) running() *cloneRun {
	for _, r := range fe.regs {
		if r.dst {
			return r.run
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
		// decision: a call carrying an admitted hook answers a removal in
		// flight DelayConflict at once rather than waiting it out. Its caller,
		// the filesystem, holds both files' admission locks until the hook
		// runs, and a removal under the filesystem holds the file's admission
		// lock through its own phase 1, so one met here is one the removal
		// loop retries, keeping its guard for as long as the store refuses it:
		// waiting would hold an open, a close or a lock of either file that
		// long. A copy's later chunk, whose locks are gone, ends the copy short
		// the same way. Withdraw it if the filesystem stops holding the locks
		// to the hook.
		if hasAdmitted(ctx) {
			e.files.unlock(f, fe)
			return &vfserr.Delay{Cause: vfserr.DelayConflict}
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
// extent a running clone holds (DelayClone). A call naming the source,
// destination, offsets and length of a clone running into the destination
// joins it (RFC 8 §9.1), starting no second clone, once ctx's admitted hook
// (WithAdmitted) has run; one naming another spec meets the running clone's
// registrations, answering DelayClone. One naming a done clone's spec,
// neither extent changed since, answers nil at once (doneBefore). Otherwise
// it registers the source extent and the destination extent, runs ctx's
// admitted hook, whose error undoes both registrations, then runs step 1
// on the call (prepare), whose refusal undoes both registrations and changes
// nothing, then from the journal step on its own goroutine (runClone) and the
// removal loop (clonePhase2), where it fails only if the loop ends first,
// answering ErrUnavailable, until its phase 2 is done. The call, and every
// one that joins the clone, waits for it within its own context, whose end, a
// deadline or a cancel, answers DelayClone and leaves the clone running, as
// the clone may already have changed the destination and only a call that
// changed nothing answers refused.
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
	sb, err := e.enter(srcShare)
	if err != nil {
		return err
	}
	defer sb.gate.leave()

	db, err := e.enter(dstShare)
	if err != nil {
		return err
	}
	defer db.gate.leave()
	st := e.settings.Load()
	switch {
	case st == nil:
		return fmt.Errorf("engine: no settings applied: %w", vfserr.ErrUnavailable)
	case invalid(x, src == dst):
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
	// decision: a clone is joined only once its step 1 is done (preparing): a
	// retry meeting it in step 1, where the first call's own context still
	// governs and its end refuses the clone, meets its registrations and
	// answers DelayClone, and the client retries again. The window grows with
	// the clone: step 1 commits the source, reads CloneFree and reserves over
	// the whole extent, up to clone_max_len, so a long clone's retries answer
	// DelayClone for longer. Join it there too if retries are seen answered
	// DelayClone behind clones that were not slow.
	//
	// decision: the filesystem service does not reach the join today: its
	// sizing reads the destination's Allocation before calling the engine,
	// and the running clone's registration refuses that DelayClone, so its
	// retry answers DelayClone until the clone is done and is then answered
	// by doneBefore. The join serves engine-direct calls, a copy continued
	// from a running chunk among them; its hook is pinned by an engine test.
	// Overturned by retries through the filesystem seen answered DelayClone
	// for long behind clones, which would let a joining retry past its sizing.
	req := cloneRequest{srcShare: srcShare, dstShare: dstShare, src: src, dst: dst, ext: x}
	if c := e.joinable(req); c != nil {
		if err := Admitted(ctx); err != nil {
			return err
		}
		return c.wait(ctx)
	}
	if e.doneBefore(req) {
		return nil
	}
	c := &cloneRun{sb: sb, db: db, src: src, dst: dst, req: req, done: make(chan struct{})}
	c.preparing.Store(true)
	rs := &reg{Extent: entity.Extent{Offset: x.SrcOff, Length: x.Len}}
	rd := &reg{Extent: entity.Extent{Offset: x.DstOff, Length: x.Len}, dst: true, run: c}
	c.rs, c.rd = rs, rd
	if err := e.register(ctx, src, rs); err != nil {
		return err
	}
	if err := e.register(ctx, dst, rd); err != nil {
		e.unregister(src, rs)
		return err
	}
	if h := e.afterRegister; h != nil {
		if err := h(); err != nil { // a test of admission alone stops the clone here
			e.unregister(dst, rd)
			e.unregister(src, rs)
			return err
		}
	}
	if !c.enterShares() { // an Unbind began since the call entered: nothing changed
		e.unregister(dst, rd)
		e.unregister(src, rs)
		return errRemoved
	}
	// decision: the admitted hook runs once the registrations are installed
	// and before step 1 (prepare), so step 1's sync of the source, its
	// CloneFree read and its reservation run with the filesystem's locks
	// already released, and only a refusal at admission leaves the
	// destination's mode alone. RFC 17 §5.9 still clears setuid and setgid in
	// its step 3, before the engine is called; this order stands until it
	// names the hook. Overturned by a refusal in step 1 that must leave the
	// mode alone, which would move the hook past prepare and step 1's sync
	// back under both locks.
	if err := Admitted(ctx); err != nil {
		e.endClone(c, err)
		return err
	}
	if err := e.prepare(ctx, c, x); err != nil {
		e.endClone(c, err)
		return err
	}

	c.preparing.Store(false) // a retry joins it from here
	e.cloning.Add(1)

	go func() {
		defer e.cloning.Done()

		e.runClone(c)
	}()

	return c.wait(ctx)
}

// admittedKey is the context key of a Clone call's admitted hook.
type admittedKey struct{}

// WithAdmitted returns ctx carrying admitted, which Clone, each of Copy's
// chunks included, calls on the calling goroutine with none of the engine's
// locks held: on a new clone once its admission's refusals have passed and
// it holds its registrations, before step 1 and before anything changes the
// destination; on a join once it has found the running clone, which may
// already have changed the destination. A retry of a done clone answers
// without calling it. Its error refuses the call, changing nothing; on a
// copy's later chunk it ends the copy short. The filesystem service clears
// setuid and setgid there and releases the files' locks, since from there
// the registrations order every call meeting the clone's extents (RFC 17
// §5.9). A clone the engine's admission refuses therefore leaves the mode
// alone; one refused in step 1 or at its journal step leaves the bits
// cleared, as a write the engine refuses does. admitted must not be nil.
func WithAdmitted(ctx context.Context, admitted func() error) context.Context {
	return context.WithValue(ctx, admittedKey{}, admitted)
}

// Admitted runs the admitted hook ctx carries, if any, and answers its
// error: what Clone calls, and what another Content calls at the same point.
func Admitted(ctx context.Context) error {
	if h, ok := ctx.Value(admittedKey{}).(func() error); ok {
		return h()
	}
	return nil
}

// hasAdmitted reports whether ctx carries an admitted hook: whether the
// caller holds its own locks until the hook runs.
func hasAdmitted(ctx context.Context) bool {
	_, ok := ctx.Value(admittedKey{}).(func() error)
	return ok
}

// wait answers c's outcome once it is done, or DelayClone at ctx's end,
// leaving it running: it may have changed the destination, so it is never
// answered as refused.
func (c *cloneRun) wait(ctx context.Context) error {
	select {
	case <-c.done:
		return c.err
	case <-ctx.Done():
		return delayClone()
	}
}

// cloneRequest is a Clone call's request as a retry of it repeats it (RFC 8
// §9.1): the shares and files on each side and the extent. A clone keeps the
// one it was made from (cloneRun.req), compared whole by joinable.
type cloneRequest struct {
	srcShare, dstShare entity.ShareID
	src, dst           entity.FileID
	ext                vfserr.Extent
}

// joinable is the clone running into req's destination when it was made
// from req, past its step 1; nil otherwise. The running clone is the one its
// destination registration names, so a clone is found from the moment that
// registration is installed until it is dropped, however it was made.
// A retry of a clone done is answered by doneBefore.
func (e *Engine) joinable(req cloneRequest) *cloneRun {
	fe := e.files.lockOf(req.dst)
	if fe == nil {
		return nil
	}
	c := fe.running()
	e.files.unlock(req.dst, fe)
	if c == nil || c.preparing.Load() || c.req != req {
		return nil
	}
	return c
}

// invalid reports whether x is refused ErrInvalid whatever its length: a
// zero or negative length or offset, an end past EndOfFile, or, oneFile when
// source and destination are one file, a source and destination that overlap.
func invalid(x vfserr.Extent, oneFile bool) bool {
	return x.Len <= 0 || x.SrcOff < 0 || x.DstOff < 0 || x.Len > entity.EndOfFile-max(x.SrcOff, x.DstOff) ||
		oneFile && entity.Extent{Offset: x.SrcOff, Length: x.Len}.Overlaps(entity.Extent{Offset: x.DstOff, Length: x.Len})
}

// copyChunk is the longest clone a copy runs: a writer of the source meeting
// a copy waits behind one chunk, not the whole copy.
//
// decision: the copy chunk is fixed, not a setting: it bounds how long a
// writer of the source is held, and a held writer resumes when the chunk ends
// rather than after a client's retry pause, so the wait a client sees is the
// chunk's own time. 64 MiB is a proposal, not a measurement; overturned if
// the source-write wait measured at the client during a COPY shows writers
// stalling for a visible fraction of their deadline.
//
// decision: a copy's edges. The chunk is the smaller of copyChunk and
// clone_max_len, so no chunk is one the length rule refuses. At the caller's
// deadline Copy answers the bytes of the chunks done and leaves the running
// chunk to finish, starting no other; with no chunk done it answers the first chunk's error, a
// Delay, never 0, which an NFSv4.2 client would read as nothing left to copy.
const copyChunk = 64 << 20

// chunks yields xs in order, each cut into clones of at most the smaller of
// copyChunk and maxLen, in offset order.
func chunks(xs []vfserr.Extent, maxLen int64) iter.Seq[vfserr.Extent] {
	n := min(copyChunk, maxLen)
	return func(yield func(vfserr.Extent) bool) {
		for _, x := range xs {
			for at := int64(0); at < x.Len; at += n {
				if !yield(vfserr.Extent{SrcOff: x.SrcOff + at, DstOff: x.DstOff + at, Len: min(n, x.Len-at)}) {
					return
				}
			}
		}
	}
}

// Copy copies xs of src into dst in order, as clones of at most a chunk
// each, and answers the bytes copied. Every extent is validated as Clone
// validates one before any chunk runs, so an invalid one, an overlap within
// one file included, answers ErrInvalid with nothing copied. A chunk's
// failure after earlier chunks are done answers the short count of those and
// nil, from which the client continues; only the first chunk's failure
// answers its error. A chunk that meets the caller's deadline is left
// running and is not counted, and a deadline met between two chunks starts
// no further one (copyChunk).
func (e *Engine) Copy(ctx context.Context, srcShare entity.ShareID, src entity.FileID, dstShare entity.ShareID, dst entity.FileID, xs []vfserr.Extent) (int64, error) {
	for _, x := range xs {
		if invalid(x, src == dst) {
			return 0, vfserr.ErrInvalid
		}
	}
	st := e.settings.Load()
	if st == nil {
		return 0, fmt.Errorf("engine: no settings applied: %w", vfserr.ErrUnavailable)
	}
	var done int64
	for c := range chunks(xs, st.CloneMaxLen) {
		if done > 0 && ctx.Err() != nil {
			return done, nil // past the deadline: start no chunk the count will not include
		}
		if err := e.Clone(ctx, srcShare, src, dstShare, dst, c); err != nil {
			if done > 0 {
				return done, nil
			}
			return 0, err
		}
		done += c.Len
	}
	return done, nil
}

// cloneRun is one clone past its admission: its two files and bindings,
// their registrations, its spec, the source's held runs it copies, one copy
// write each, the source extents a rebuilt clone's destination already
// holds copies of, the reservation taken for them, and once its copies are
// synced, the newest copy's version.
type cloneRun struct {
	sb, db    *binding
	src, dst  entity.FileID
	req       cloneRequest // the request it was made from, which a retry of it repeats (joinable)
	preparing atomic.Bool  // in its step 1, on its call: not joined until it is done
	rs, rd    *reg
	spec      journal.CloneSpec
	runs      []journal.Held
	kept      []journal.Extent // source offsets: copies are not made again for them
	res       Reservation      // nil when none is held
	copied    bool             // the copies are written and synced
	newest    journal.Version
	done      chan struct{}
	err       error
	leave     func() // withdraws the clone from both shares' gates (enterShares), once
}

// enterShares counts c as work of both its shares until it ends (endClone,
// or clonePhase2 once it released its source), so an Unbind of either waits for it (RFC 8 §2.3); it reports false, having
// entered neither, once either share is being unbound.
func (c *cloneRun) enterShares() bool {
	if !c.sb.gate.enter() {
		return false
	}
	if !c.db.gate.enter() {
		c.sb.gate.leave()
		return false
	}
	c.leave = leaveOnce(&c.sb.gate, &c.db.gate)
	return true
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

	asOf, err := e.size(ctx, c, true)
	if err != nil {
		if answeredRefusal(err) {
			return err
		}

		return e.journalErr(err)
	}
	c.spec.AsOf = asOf
	return nil
}

// size resolves the source extent in windows of copyBuf, keeping its held
// runs but where the destination already holds their copies (kept), and
// reserves on the destination's handle every byte and write the copies
// need, so no copy is refused part-way. It answers the newest version the
// source was read at.
//
// The reservation is answered by the refusal table. With client set, a clone
// of a caller waits for a version raise, and counts and logs each refusal;
// the removal loop's size, with client unset, takes the table's answer only,
// with no wait, count or warning, and the loop retries it.
//
// ponytail: a clone reads its source's held bytes twice, here to size them
// and in its copies. Give the handle RFC 1 §3.7's Extents to size without
// reading when clone time on journal-held sources shows in a profile.
func (e *Engine) size(ctx context.Context, c *cloneRun, client bool) (journal.Version, error) {
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
		c.runs = append(c.runs, held...)
		asOf = newer(asOf, at)
	}
	c.runs = uncovered(c.runs, c.kept)
	for _, h := range c.runs {
		n += h.Extent.Len
	}
	if len(c.runs) > 0 {
		reserve := func() (Reservation, error) {
			return c.db.h.Reserve(n, len(c.runs))
		}

		var (
			res Reservation
			err error
		)

		if client {
			res, err = answeringRefusals(ctx, e, c.db, true, reserve)
		} else {
			// The removal loop's reservation answers no caller, so it is
			// answered by the table without waiting, counting or warning;
			// the loop retries it as before.
			res, err = reserve()
			if answer, ok := tableAnswer(err); ok {
				err = answer
			}
		}

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
func (e *Engine) runClone(c *cloneRun) {
	ctx := c.db.gc.removals.ctx
	r, err := e.begin(ctx, c.db, c.dst, entity.RemoveClone, c.rd.Extent)
	if err == nil {
		dx := journal.Extent{Off: c.spec.DstOff, Len: c.spec.Len}
		// The removal loop's step answers no caller, so a capacity refusal is
		// answered by the table at once, with no repack, wait, count or warning,
		// as size does for the loop's reservation. stage returns the refusal
		// raw, its entry lock released.
		_, err = e.stage(ctx, r, func(*fileEntry) (journal.Version, error) {
			return c.db.h.CloneTarget(ctx, fileID(c.dst), c.spec)
		}, opClone, dx, nil)
		if answer, ok := tableAnswer(err); ok {
			err = answer
		}

		if err != nil {
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
	if e.attempt(ctx, c, r) != itemDone {
		e.retryClone(c, r)
	}
}

// attempt runs what the clone has left past its journal step: its copies
// and the destination's sync, unless done, then its phase 1, bounded by the
// conflict bound. It answers itemDone once its phase 1 committed, its phase
// 2 posted to the loop, or once the clone ended; itemRetry for the loop to
// run it again, itemRetryUncounted while it waits for journal space.
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
			switch {
			case ctx.Err() != nil:
			case errors.Is(err, journal.ErrJournalReadOnly):
				// Refused until the journal is reopened: noteReadOnly logs
				// the state once, not once per retry.
				e.noteReadOnly(true, err)
				e.d.Logger.Debug("a clone's copies refused by the read-only journal: retried", "src", c.src, "dst", c.dst, "err", err)
			case c.rd.space.Load():
				// decision: a clone waiting for journal space waits as any
				// capacity refusal does, uncounted toward removal_stalled,
				// as space is freed by offload and eviction, not by this
				// clone; the calls behind it answer DelayCapacity meanwhile.
				// Count it if a clone is seen waiting with nothing of the
				// journal's own reporting the shortage.
				e.d.Logger.Debug("a clone's copies wait for journal space: retried", "src", c.src, "dst", c.dst, "err", err)
				return itemRetryUncounted
			default:
				e.d.Logger.Error("a clone's copies failed after its journal step: retried", "src", c.src, "dst", c.dst, "err", err)
			}
			return itemRetry
		}
		c.copied = true
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
		_, err := e.size(ctx, c, false)

		var d *vfserr.Delay

		// size answers a refusal for space by the table, as DelayCapacity.
		space := errors.As(err, &d) && d.Cause == vfserr.DelayCapacity
		c.rs.space.Store(space)
		c.rd.space.Store(space)
		if err != nil {
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
			e.issued(c.db.vl, v)
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
	c.db.gc.removals.enqueue(e.cloneItem(c, r))
}

// cloneItem is the loop item running what clone c has left past its
// journal step, r its removal of the destination extent.
func (e *Engine) cloneItem(c *cloneRun, r *removal) *loopItem {
	return &loopItem{b: c.db, f: c.dst, kind: entity.RemoveClone,
		run: func(ctx context.Context) answer { return e.attempt(ctx, c, r) },
		end: func() { e.endClone(c, errLoopEnded) },
	}
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

// endClone ends c, answering err, and leaves both shares' gates: see
// settleClone.
func (e *Engine) endClone(c *cloneRun, err error) {
	e.settleClone(c, err)
	c.leave()
}

// settleClone is endClone short of leaving the gates: its reservation, when
// one is held, is given back, both registrations go, the destination's
// taking it out of the running clones, a done clone's spec taking their
// place (remember), which a release answered
// pending behind them sees at its next look, and its done signal is raised. A
// clone settles with a nil err once its phase 2 is done, before its hold on
// the source is released, and leaves the gates after that release
// (clonePhase2), so an Unbind of either share waits for it.
func (e *Engine) settleClone(c *cloneRun, err error) {
	if c.res != nil {
		_ = c.res.Close()
		c.res = nil
	}
	if err == nil {
		e.remember(c)
	} else {
		e.unregister(c.dst, c.rd)
		e.unregister(c.src, c.rs)
	}
	c.err = err
	close(c.done)
}

// doneClones is how many done clones the node remembers at most (D7's FIFO).
const doneClones = 1024

// doneClone is a done clone's spec, remembered in both its files' entries,
// which it keeps alive, and in the node's done list, until a write or
// removal overlapping either extent, an Unbind of either share, or the list's
// eviction of the oldest past doneCap drops it.
type doneClone struct {
	cloneRequest               // the request the clone was made from, which a retry of it repeats
	gone         atomic.Bool   // dropped: whoever set it removes it from the entries and the list
	el           *list.Element // its place in the done list, under doneMu; nil when not in it
}

// on reports whether the clone's extent on file f, as source or destination
// or both, overlaps x.
func (d *doneClone) on(f entity.FileID, x entity.Extent) bool {
	return d.src == f && x.Overlaps(entity.Extent{Offset: d.ext.SrcOff, Length: d.ext.Len}) ||
		d.dst == f && x.Overlaps(entity.Extent{Offset: d.ext.DstOff, Length: d.ext.Len})
}

// doneBefore reports whether a clone made from req is done and neither of
// its extents changed since: a retry of it (RFC 8 §9.1), answered success at
// once with no journal call. The destination's entry holds every remembered
// clone into it.
//
// ponytail: the memory is lost at restart and after doneClones later clones,
// so a retry then runs the clone again, which leaves the same content.
// Persist it in the clone's Removal record, kept done until pruned, if
// retries after a restart show in logs.
func (e *Engine) doneBefore(req cloneRequest) bool {
	fe := e.files.lockOf(req.dst)
	if fe == nil {
		return false
	}
	defer e.files.unlock(req.dst, fe)
	return slices.ContainsFunc(fe.done, func(d *doneClone) bool {
		return !d.gone.Load() && d.cloneRequest == req
	})
}

// remember puts c's spec in both its files' entries in place of its
// registrations, each swap under one lock so no write meets the extent
// between them, nor a retry the clone neither running (joinable) nor done
// (doneBefore), then at the back of the done list, dropping the oldest past
// doneCap. A clone within one file puts it in that file's entry once. A spec
// dropped before it reaches an entry or the list is not put there.
func (e *Engine) remember(c *cloneRun) {
	d := &doneClone{cloneRequest: c.req}
	for i, s := range []struct {
		f entity.FileID
		r *reg
	}{{c.dst, c.rd}, {c.src, c.rs}} {
		fe := e.files.lockFor(s.f)
		fe.drop(s.r)
		if !d.gone.Load() && (i == 0 || c.src != c.dst) {
			fe.done = append(fe.done, d)
		}
		e.files.unlock(s.f, fe)
	}
	var old *doneClone
	e.doneMu.Lock()
	if !d.gone.Load() {
		d.el = e.done.PushBack(d)
		if e.done.Len() > e.doneCap {
			old = e.done.Remove(e.done.Front()).(*doneClone)
			old.el = nil
		}
	}
	e.doneMu.Unlock()
	if old != nil && old.gone.CompareAndSwap(false, true) {
		e.unremember(old)
	}
}

// forgetOver drops, from f's entry, every remembered clone whose extent on f
// overlaps x, which a write or removal is about to change, and answers them
// for unremember once the entry's lock is released. The caller holds it.
func (fe *fileEntry) forgetOver(f entity.FileID, x entity.Extent) []*doneClone {
	if len(fe.done) == 0 {
		return nil
	}
	var gone []*doneClone
	fe.done = slices.DeleteFunc(fe.done, func(d *doneClone) bool {
		if !d.on(f, x) || !d.gone.CompareAndSwap(false, true) {
			return d.gone.Load()
		}
		gone = append(gone, d)
		return true
	})
	return gone
}

// unremember takes each of ds, already marked gone, out of both its files'
// entries, releasing what it kept alive, then out of the done list: each
// entry's lock is taken once, and doneMu once after them. The caller holds no
// entry's lock.
func (e *Engine) unremember(ds ...*doneClone) {
	if len(ds) == 0 {
		return
	}
	gone := make(map[*doneClone]bool, len(ds))
	files := make(map[entity.FileID]bool)
	for _, d := range ds {
		gone[d], files[d.src], files[d.dst] = true, true, true
	}
	for f := range files {
		if fe := e.files.lockOf(f); fe != nil {
			fe.done = slices.DeleteFunc(fe.done, func(o *doneClone) bool { return gone[o] })
			e.files.unlock(f, fe)
		}
	}
	e.doneMu.Lock()
	for _, d := range ds {
		if d.el != nil {
			e.done.Remove(d.el)
			d.el = nil
		}
	}
	e.doneMu.Unlock()
}

// forgetShare drops every remembered clone with share s on either side: once
// unbound, its files may change where this node does not see it.
func (e *Engine) forgetShare(s entity.ShareID) {
	var gone []*doneClone
	e.doneMu.Lock()
	for el := e.done.Front(); el != nil; el = el.Next() {
		if d := el.Value.(*doneClone); (d.srcShare == s || d.dstShare == s) && d.gone.CompareAndSwap(false, true) {
			gone = append(gone, d)
		}
	}
	e.doneMu.Unlock()
	e.unremember(gone...)
}
