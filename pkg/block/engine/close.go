package engine

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"time"

	"github.com/cubbit/dittofs/pkg/block/journal"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/vfs/vfserr"
)

// stopDefault is the stop bound of a Close whose context has no deadline.
//
// decision: RFC 8 §2.5 names "the stop bound" without a value. It is the
// deadline of the context Close is given and, only when that context has
// none, 30 s from the call on the engine's clock, so a Close over a stuck
// loop still returns and a caller who gave a longer deadline keeps it. 30 s
// matches the server's shutdown timeout default, which the node's stop has to
// fit in. Withdrawn if RFC 8 or RFC 23 sets the bound.
const stopDefault = 30 * time.Second

// closeRun is one Close: the call that runs the steps, and the result every
// call that arrives meanwhile waits for.
type closeRun struct {
	done  chan struct{}
	err   error // written before done is closed
	final bool  // the handles and journals were closed: err is the answer of every later call
}

// stopBound is how long Close waits for one thing to end: until its context
// ends, and, when the context has no deadline, until the default timer fires.
// A nil bound waits without end, for the test helpers that stop one part.
type stopBound struct {
	e     *Engine
	ctx   context.Context
	timer <-chan struct{}
}

// newBound starts the stop bound of a Close called with ctx.
func (e *Engine) newBound(ctx context.Context) *stopBound {
	b := &stopBound{e: e, ctx: ctx}

	if _, ok := ctx.Deadline(); !ok {
		b.timer = e.d.Clock.After(stopDefault)
	}

	return b
}

// over reports whether the bound has ended, without waiting.
func (b *stopBound) over() bool {
	if b == nil {
		return false
	}

	select {
	case <-b.ctx.Done():
		return true
	case <-b.timer:
		return true
	default:
		return false
	}
}

// join waits for done, which ends what, and reports true, or reports false
// when the bound ends first. It fires the joining seam with what once it has
// to wait.
func (b *stopBound) join(done <-chan struct{}, what string) bool {
	if b == nil {
		<-done

		return true
	}

	select {
	case <-done:
		return true
	default:
	}

	if f := b.e.seams.joining.Load(); f != nil {
		(*f)(what)
	}

	select {
	case <-done:
		return true
	case <-b.ctx.Done():
		return false
	case <-b.timer:
		return false
	}
}

// Close stops the engine and closes what it opened, in reverse (RFC 8 §2.5
// Stop). It refuses every call at once, waits for the lifecycle calls in
// progress, and closes the handles and then the journals. A join that does
// not end within the stop bound leaves the journal and every handle on it
// open and is reported in the error, so no component is closed under work
// that uses it (RFC 1 §8.7).
//
// Two Close calls at the same time are one stop: the second waits for the
// first and answers its result, or its own error when its context ends
// first. A Close after one that reported an unfinished join runs the steps
// again and joins in its own bound; after one that closed the handles and the
// journals, it answers that one's result, an error of a close included.
//
// Close runs after the filesystem service has stopped (RFC 8 §2.5), so no
// content call is in flight when it begins; one that draws a version or
// admits a clone at that moment is outside its contract.
//
// decision: Close calls no Done for a write it leaves uncommitted (a file a
// removal guards, the pending existence of a read-only journal): the
// filesystem service, whose quota tracker holds the reservation a Done
// releases, has stopped before Close runs, and the next start re-applies the
// write from the journal and charges it, with nothing reserved (RFC 8 §5.1,
// "After a restart nothing is reserved"). RFC 8 §5.1 lists the paths that
// call Done and Close is not one; the stop-serving sweep it does list is the
// node ceasing to serve a shard while the process lives. Overturned if the
// RFC has Close call Done for the writes it leaves.
func (e *Engine) Close(ctx context.Context) error {
	e.mu.Lock()

	if r := e.closing; r != nil {
		select {
		case <-r.done:
			if r.err == nil || r.final {
				e.mu.Unlock()

				return r.err
			}
		default:
			e.mu.Unlock()

			return e.awaitClose(ctx, r)
		}
	}

	r := &closeRun{done: make(chan struct{})}
	e.closing = r

	if !e.closed.Swap(true) {
		close(e.stopping)
	}
	e.mu.Unlock()

	r.err = e.stop(e.newBound(ctx), r)
	close(r.done)

	return r.err
}

// awaitClose is the answer of a Close that arrives while another runs.
func (e *Engine) awaitClose(ctx context.Context, r *closeRun) error {
	if !e.newBound(ctx).join(r.done, "the other Close") {
		return fmt.Errorf("engine: close: context ended while waiting for the other Close: %w",
			vfserr.ErrUnavailable)
	}

	return r.err
}

// stop runs the steps after the closed state is set.
func (e *Engine) stop(b *stopBound, r *closeRun) error {
	if err := e.drainLifecycle(b); err != nil {
		return err
	}

	if err := e.stopWork(b); err != nil {
		return err
	}

	if err := e.stopGroupCommits(b); err != nil {
		return err
	}

	if err := e.stopRaise(b); err != nil {
		return err
	}

	if err := e.stopRetries(b); err != nil {
		return err
	}

	r.final = true

	return e.closeAll()
}

// stuck is the error of a join that did not end within the stop bound.
func stuck(what string) error {
	return fmt.Errorf(
		"engine: close: %s still running at the end of the stop bound, "+
			"journals and handles left open: %w",
		what, vfserr.ErrUnavailable,
	)
}

// stopGroupCommits commits every file's pending existence in a last group
// commit per journal, so a clean stop leaves no append or hole fill for the
// next start to re-apply, and then stops and joins each group commit (RFC 8
// §2.5 step 2). None starts again.
func (e *Engine) stopGroupCommits(b *stopBound) error {
	e.gcMu.Lock()
	e.gcsClosed = true
	gcs := slices.Collect(maps.Values(e.gcs))
	e.gcMu.Unlock()

	for _, g := range gcs {
		// decision: a journal that is read-only is not synced at Close, whether
		// it was so when Close began or turned so during the last group
		// commit (commitAll checks each round). RFC 8 §2.5 has the engine
		// refuse every sync of a read-only journal ("once the journal reports
		// itself read-only, the engine refuses every sync"), so a last group
		// commit over it commits nothing and would wait for the stop bound,
		// while the pending existence is in the journal, where the next
		// Start's recovery re-applies it (Start step 3). The handles and the
		// journal still close, in reverse order, and the group commits are
		// still stopped and joined: RFC 1 §8.7 has the journal's Close close
		// every segment descriptor without a final sync, so it has nothing to
		// refuse on a read-only device. RFC 1 §5.4 says nothing about the
		// Close of a read-only journal. Overturned if the journal's Close
		// syncs, or if the RFC asks Close to report the existence it left.
		//
		// decision: a last group commit that does not land within the stop
		// bound leaves the journal and every handle open and says so, as every
		// other join does (RFC 8 §2.5 step 5), and does not cancel the batch.
		// Cancelling it and closing would turn a slow store into work for the
		// next start's recovery; left running, a Close the caller retries
		// finds the store answered and ends clean. RFC 8 §2.5 step 2 only
		// bounds the commit. Overturned if the RFC has a commit that misses
		// the bound close the journal and leave its existence to recovery.
		if !g.commitAll(b) {
			return stuck("the last group commit")
		}
	}

	for _, g := range gcs {
		if !g.halt(b) {
			return stuck("a group commit")
		}
	}

	return nil
}

// stopRaise cancels the version limit raise and joins it (RFC 8 §2.5 step 3).
// It follows the work queue and the last group commit: work still running
// before it can draw a version and wake the raise, and a raise cancelled
// earlier would then start after its join.
func (e *Engine) stopRaise(b *stopBound) error {
	e.raising.cancel()

	raised := make(chan struct{})

	go func() {
		e.raising.wg.Wait()
		close(raised)
	}()

	if !b.join(raised, "a version limit raise") {
		return stuck("a version limit raise")
	}

	return nil
}

// stopRetries cancels the retried open of a journal refused at Start and
// joins it (RFC 8 §2.5 step 3). A retry that is inside Open when it is
// cancelled sees the closed state when Open returns and closes what it
// opened, so nothing it opened is left for the step that closes the journals.
func (e *Engine) stopRetries(b *stopBound) error {
	e.retrying.cancel()

	retried := make(chan struct{})

	go func() {
		e.retrying.wg.Wait()
		close(retried)
	}()

	if !b.join(retried, "a retried open") {
		return stuck("a retried open")
	}

	return nil
}

// stopWork stops the background work that takes journal versions: each
// journal's removal loop, which refuses new dispatch, cancels the batch in
// flight and ends the items left, and the goroutines of the clones admitted
// before Close and of the rebuilt clones still waiting for their source's
// shard (RFC 8 §2.5 step 2). A clone whose phase 1 was cut short stays
// in its file's entry for the next start to rebuild.
func (e *Engine) stopWork(b *stopBound) error {
	e.gcMu.Lock()
	gcs := slices.Collect(maps.Values(e.gcs))
	e.gcMu.Unlock()

	for _, g := range gcs {
		if !g.removals.halt(b) {
			return stuck("a removal loop")
		}
	}

	cloned := make(chan struct{})

	go func() {
		e.cloning.Wait()
		close(cloned)
	}()

	if !b.join(cloned, "a clone goroutine") {
		return stuck("a clone goroutine")
	}

	return nil
}

// drainLifecycle waits for the lifecycle calls in progress, a Bind, Unbind or Drop
// naming a share and a Start, before anything is stopped: one that is past
// its checks could otherwise start a raise or a group commit after the join
// that was meant to end them (RFC 8 §2.3, §2.5).
func (e *Engine) drainLifecycle(b *stopBound) error {
	waited := false

	for {
		e.mu.Lock()
		busy := slices.Collect(func(yield func(entity.ShareID) bool) {
			for id := range e.busy {
				if !yield(id) {
					return
				}
			}
		})
		starting := e.starting
		ended := e.ended
		e.mu.Unlock()

		if len(busy) == 0 && !starting {
			return nil
		}

		if f := e.seams.draining.Load(); f != nil && !waited {
			(*f)()
		}

		waited = true

		if !b.join(ended, "a lifecycle call") {
			return fmt.Errorf(
				"engine: close: lifecycle calls still in progress at the end of the stop bound "+
					"(shares %v, start %t), journals and handles left open: %w",
				busy, starting, vfserr.ErrUnavailable,
			)
		}
	}
}

// lifecycleEnded wakes the Close that waits for the lifecycle calls. The
// caller holds mu.
func (e *Engine) lifecycleEnded() {
	close(e.ended)
	e.ended = make(chan struct{})
}

// closeAll closes every handle, newest binding first, and then every
// journal, newest first: the reverse of the order they were opened in (RFC 8
// §2.5 step 4). It runs once: stop sets final before it, and a later Close
// answers that result without running the steps again.
func (e *Engine) closeAll() error {
	e.mu.Lock()
	journals := slices.Clone(e.journals)
	e.mu.Unlock()

	var bs []*binding

	for _, b := range e.table() {
		if b.h != nil {
			bs = append(bs, b)
		}
	}

	slices.SortFunc(bs, func(x, y *binding) int {
		switch {
		case x.seq > y.seq:
			return -1
		case x.seq < y.seq:
			return 1
		}

		return 0
	})

	var errs []error

	for _, b := range bs {
		// A handle a failed Drop closed answers ErrClosed: it is closed.
		if err := b.h.Close(); err != nil && !errors.Is(err, journal.ErrClosed) {
			errs = append(errs, fmt.Errorf("engine: close: handle of share %v: %w",
				b.share.ID, e.journalErr(err)))
		}
	}

	for _, vl := range slices.Backward(journals) {
		if err := vl.j.Close(); err != nil {
			errs = append(errs, fmt.Errorf("engine: close: journal: %w", e.journalErr(err)))
		}
	}

	return errors.Join(errs...)
}
