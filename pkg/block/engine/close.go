package engine

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

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

// join waits for done and reports true, or reports false when the bound ends
// first. It fires the joining seam once it has to wait.
func (b *stopBound) join(done <-chan struct{}) bool {
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
		(*f)()
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
	e.closed.Store(true)
	e.mu.Unlock()

	r.err = e.stop(e.newBound(ctx), r)
	close(r.done)

	return r.err
}

// awaitClose is the answer of a Close that arrives while another runs.
func (e *Engine) awaitClose(ctx context.Context, r *closeRun) error {
	if !e.newBound(ctx).join(r.done) {
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

	r.final = true

	return e.closeAll()
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

		if !b.join(ended) {
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
// §2.5 step 4). A handle or journal already closed by an earlier Close is
// skipped, so a Close that follows a failed one closes nothing twice.
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
		if !b.closed.CompareAndSwap(false, true) {
			continue
		}

		if err := b.h.Close(); err != nil {
			errs = append(errs, fmt.Errorf("engine: close: handle of share %v: %w",
				b.share.ID, e.journalErr(err)))
		}
	}

	for _, vl := range slices.Backward(journals) {
		if !vl.closed.CompareAndSwap(false, true) {
			continue
		}

		if err := vl.j.Close(); err != nil {
			errs = append(errs, fmt.Errorf("engine: close: journal: %w", e.journalErr(err)))
		}
	}

	return errors.Join(errs...)
}
