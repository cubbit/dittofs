package engine

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/metadata/kvstore"
	"github.com/cubbit/dittofs/pkg/vfs/vfserr"
)

// answer is what one run of a loop item answers.
type answer uint8

const (
	itemDone           answer = iota + 1 // the item is finished
	itemRetry                            // it failed: retried after the backoff, counted toward a stall
	itemRetryUncounted                   // it is held by something outside it: retried, never counted
	itemAdvanced                         // it advanced and has more to do: run again at once, its failures reset
)

// runResult is a phase-2 run's outcome, one bounded Resume and the prune
// after it, as engine_removal_runs_total labels it.
type runResult uint8

const (
	runDone runResult = iota
	runAdvanced
	runConflict
	runError
	runResults
)

var runResultNames = [runResults]string{"done", "advanced", "conflict", "error"}

// stallAfter is the count of consecutive failures after which an item is
// removal_stalled.
const stallAfter = 5

// loopBackoff is the wait before an item that did not finish runs again.
const loopBackoff = time.Second

// loopItem is one piece of removal work: a removal's phase 2 and prune, or a
// phase 1 the call that began it could not finish.
type loopItem struct {
	b    *binding
	f    entity.FileID
	kind entity.RemovalKind
	run  func(ctx context.Context) answer
	// end is called once if the loop ends, or its share is dropped, before
	// the item finished; nil when there is nothing to end.
	end func()

	queued    time.Time // when it was first enqueued
	notBefore time.Time
	fails     int
	stalled   bool
}

// removalLoop runs one journal's removal work.
//
// ponytail: removal phase 2 and its prune run from one loop per journal, a
// FIFO with a fixed backoff, not the shared work scheduler. Replace it with
// the scheduler's removal source when offload adds the first work that needs
// fairness between shares.
type removalLoop struct {
	e      *Engine
	mu     sync.Mutex
	items  []*loopItem
	closed bool
	// decision: only a test helper sets dropped, until Drop is built, so the
	// branches that read it end a dropped share's items for tests alone.
	// They are the shape Drop needs: Drop sets it, and the helper goes,
	// once built. Delete the field and its branches if Drop ends a share's
	// items some other way.
	dropped map[entity.ShareID]bool  // shares dropped: an item of one, running or enqueued, is ended
	steps   atomic.Int64             // items run, for a test to wait on a run
	runs    [runResults]atomic.Int64 // engine_removal_runs_total by result
	wake    chan struct{}
	stop    chan struct{}
	done    chan struct{}
	ctx     context.Context
	cancel  context.CancelFunc
}

func newRemovalLoop(e *Engine) *removalLoop {
	l := &removalLoop{e: e, wake: make(chan struct{}, 1), stop: make(chan struct{}), done: make(chan struct{})}
	l.ctx, l.cancel = context.WithCancel(withRetryingOwner(context.Background()))
	return l
}

// enqueue adds it at the loop's back. On a closed loop it is ended at once.
func (l *removalLoop) enqueue(it *loopItem) {
	now := l.e.d.Clock.Now()
	l.mu.Lock()
	if l.closed || l.dropped[it.b.share.ID] {
		l.mu.Unlock()
		if it.end != nil {
			it.end()
		}
		return
	}
	it.queued = now
	l.items = append(l.items, it)
	l.mu.Unlock()
	select {
	case l.wake <- struct{}{}:
	default:
	}
}

func (l *removalLoop) run() {
	defer close(l.done)
	for {
		it, wait := l.next()
		if it != nil {
			l.step(it)
			continue
		}
		var timer <-chan struct{}
		if wait >= 0 {
			timer = l.e.d.Clock.After(wait)
		}
		select {
		case <-l.wake:
		case <-timer:
		case <-l.stop:
			return
		}
	}
}

// next is the first item whose backoff has passed, taken off the loop, or
// how long until the first one's has, -1 when the loop is empty.
func (l *removalLoop) next() (*loopItem, time.Duration) {
	now := l.e.d.Clock.Now()
	l.mu.Lock()
	defer l.mu.Unlock()
	wait := time.Duration(-1)
	for i, it := range l.items {
		if !it.notBefore.After(now) {
			l.items = slices.Delete(l.items, i, i+1)
			return it, 0
		}
		if w := it.notBefore.Sub(now); wait < 0 || w < wait {
			wait = w
		}
	}
	return nil, wait
}

// step runs it once, under a root span, and puts it back at the loop's end
// unless it finished, raising removal_stalled after five failures in a row
// and clearing it once it finishes or advances.
func (l *removalLoop) step(it *loopItem) {
	ctx, end := l.e.d.Tracer(l.ctx, "engine.removal_batch", slog.String("kind", it.kind.String()))
	a := it.run(ctx)
	end(nil)
	l.steps.Add(1)
	log := l.e.d.Logger
	backoff := loopBackoff
	switch a {
	case itemDone:
		if it.stalled {
			log.Warn("removal_stalled cleared", "file", it.f, "kind", it.kind)
		}
		return
	case itemAdvanced:
		if it.stalled {
			log.Warn("removal_stalled cleared", "file", it.f, "kind", it.kind)
		}
		it.fails, it.stalled, backoff = 0, false, 0
	case itemRetry:
		it.fails++
		if it.fails >= stallAfter && !it.stalled {
			it.stalled = true
			log.Warn("removal_stalled", "file", it.f, "kind", it.kind)
		}
	}
	it.notBefore = l.e.d.Clock.Now().Add(backoff)
	l.mu.Lock()
	ended := l.closed || l.dropped[it.b.share.ID]
	if !ended {
		l.items = append(l.items, it)
	}
	l.mu.Unlock()
	if ended && it.end != nil {
		it.end()
	}
}

// close stops the loop, joins its goroutine and ends every item left. A
// second close does nothing.
func (l *removalLoop) close() {
	l.mu.Lock()
	if l.closed {
		l.mu.Unlock()
		return
	}
	l.closed = true
	items := l.items
	l.items = nil
	l.mu.Unlock()
	l.cancel()
	close(l.stop)
	<-l.done
	for _, it := range items {
		if it.end != nil {
			it.end()
		}
	}
}

// isDropped reports whether share s was dropped: nothing of it is noted for
// a commit again.
func (l *removalLoop) isDropped(s entity.ShareID) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.dropped[s]
}

// pending is the count of items on the loop and the oldest one's age:
// engine_removals_pending and engine_removals_oldest_seconds.
func (l *removalLoop) pending() (n int, oldest time.Duration) {
	now := l.e.d.Clock.Now()
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, it := range l.items {
		oldest = max(oldest, now.Sub(it.queued))
	}
	return len(l.items), oldest
}

// stalledOn reports whether f has an item removal_stalled.
func (l *removalLoop) stalledOn(f entity.FileID) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, it := range l.items {
		if it.f == f && it.stalled {
			return true
		}
	}
	return false
}

// phase2 is the item that drops r's masked records and prunes its removal
// record: the store's Resume runs every batch of the file's removals not
// done, and the prune, with no pass in flight, is at r's own version. One
// bound covers a run; a run the bound cuts after a batch committed advanced,
// so a removal larger than one bound's worth of batches is never counted
// toward removal_stalled while each run moves its cursor.
func (e *Engine) phase2(r *removal) *loopItem {
	share, v := r.b.share.ID, entity.JournalVersion(r.v.Bytes())
	return &loopItem{b: r.b, f: r.f, kind: r.kind, run: func(ctx context.Context) answer {
		ctx, cancel := e.bounded(ctx)
		defer cancel()
		advanced, err := e.d.Content.Resume(ctx, share, r.f)
		if err == nil {
			err = e.d.Content.PruneRemovals(ctx, share, r.f, v)
		}
		res, a := runError, itemRetry
		switch {
		case err == nil:
			res, a = runDone, itemDone
		case advanced:
			res, a = runAdvanced, itemAdvanced
		case errors.Is(err, kvstore.ErrConflict):
			res = runConflict
		}
		r.b.gc.removals.runs[res].Add(1)
		return a
	}}
}

// clonePhase2 is clone c's item once its phase 1 committed, r the clone's
// removal of the destination extent: phase2's item, its run preceded by the
// hold on the source and followed by the hold's release and the clone's end,
// its registrations dropped and its done signal raised. Hold and Release are
// each bounded by the conflict bound, and a failed one is retried counted, as
// a phase 2 run is. The loop runs it with no entry lock or guard held.
func (e *Engine) clonePhase2(c *cloneRun, r *removal) *loopItem {
	it := e.phase2(r)
	it.kind = entity.RemoveClone
	p2 := it.run
	var (
		asked, held, p2done bool
		hold                entity.OpenID
	)
	// decision: a clone's source open is keyed in its share's own shard, the
	// one Bind named; nothing the engine receives names a finer shard. A share
	// split into per-child shards needs Existence.Clone to return the hold's key.
	//
	// decision: the hold names the clone's version v, not asOf, as the store's
	// phase 1 records the source's open with v and CloneOpen matches on it,
	// while CloneSrc.AsOf says asOf. Pass asOf once the store records it, or
	// rename the field if v is kept.
	src := CloneSrc{Share: c.sb.share.ID, Shard: c.sb.share.Shard, File: c.src, Dst: c.dst,
		AsOf: entity.JournalVersion(r.v.Bytes())}
	release := func(ctx context.Context) error {
		ctx, cancel := e.bounded(ctx)
		defer cancel()
		return e.d.Holds.Release(ctx, hold)
	}
	it.run = func(ctx context.Context) answer {
		if !asked {
			hctx, cancel := e.bounded(ctx)
			id, err := e.d.Holds.Hold(hctx, src)
			cancel()
			var d *vfserr.Delay
			switch {
			case errors.As(err, &d) && d.Cause == vfserr.DelayFrozen:
				// decision: a shard not yet served is retried uncounted with no
				// ceiling, as serving it is open state's to raise, not a fault
				// of this removal. A shard never served keeps both
				// registrations, so writes to either extent keep answering
				// DelayClone, and raises no removal_stalled. Count it toward
				// the stall if a shard is seen staying unserved with nothing
				// of open state's own reporting it.
				return itemRetryUncounted
			case errors.Is(err, entity.ErrStale):
				// decision: a Hold refused for good, the source holding no
				// open of the clone's, holds nothing and phase 2 runs on, with
				// no Release after it; any other error is retried below. The
				// clone's registration on the source keeps a release of it
				// pending until the clone ends, so its bytes stay; opens of
				// the source are refused meanwhile. Hold it back instead if a
				// release is ever seen passing a source whose Hold failed.
				e.d.Logger.Error("a clone's hold on its source was refused: phase 2 runs without it",
					"src", c.src, "dst", c.dst, "err", err)
			case err != nil:
				e.d.Logger.Warn("a clone's hold on its source failed: retried", "src", c.src, "dst", c.dst, "err", err)
				return itemRetry
			default:
				hold, held = id, true
			}
			asked = true
		}
		if !p2done {
			if a := p2(ctx); a != itemDone {
				return a
			}
			p2done = true
		}
		if held {
			// decision: a Release that fails is retried alone, with the
			// registrations kept, so nothing reaches the source while open
			// state may still hold it. Drop the registrations at once instead
			// if a Release failing for good is seen pinning a source.
			if err := release(ctx); err != nil {
				e.d.Logger.Warn("a clone's release of its source failed: retried", "src", c.src, "dst", c.dst, "err", err)
				return itemRetry
			}
			held = false
		}
		e.endClone(c, nil)
		return itemDone
	}
	it.end = func() {
		// The loop ended, its context with it: the release runs under its own.
		if held {
			if err := release(context.Background()); err != nil {
				e.d.Logger.Warn("a clone's release of its source at its loop's end failed", "src", c.src, "dst", c.dst, "err", err)
			}
		}
		e.endClone(c, errLoopEnded)
	}
	return it
}

// finish ends r after its phase 1 committed: the guard released, the
// journal settled through it, and its phase 2 posted to its journal's loop.
func (e *Engine) finish(r *removal) {
	e.release(r, true)
	r.b.gc.removals.enqueue(e.phase2(r))
}

// retryLater hands r's phase 1, which its call could not commit, to its
// journal's loop, which retries it keeping the guard and the in-flight mark
// until it commits, or until the file is found gone. outcome, when not nil,
// is told once how it ended: nil once it commits.
//
// decision: a phase 1 its loop's end cuts short, as Close's stop of the work
// queue does, leaves its file as it is: every pending operation, the
// removal's own included, stays in the entry and the guard stays held, so no
// group commit after it, Close's last included, commits any of them, and the
// journal's unsettled marker leaves the removal to the next start's recovery
// (RFC 8 §2.5 step 2). A caller waiting on the guard waits to its own
// deadline. Withdraw it if the loop can ever end while the engine still
// serves the file.
func (e *Engine) retryLater(r *removal, op kvstore.RemoveOp, outcome func(error)) {
	tell := func(err error) {
		if outcome != nil {
			outcome(err)
		}
	}
	r.b.gc.removals.enqueue(&loopItem{b: r.b, f: r.f, kind: r.kind,
		run: func(ctx context.Context) answer {
			ctx, cancel := e.bounded(ctx)
			defer cancel()
			err := e.tryPhase1(ctx, r, op)
			switch {
			case err == nil:
				tell(nil)
				return itemDone
			case errors.Is(err, kvstore.ErrHeld):
				tell(e.abort(r))
				return itemDone
			case terminal(err):
				e.unstage(r, err)
				tell(e.storeErr(err))
				return itemDone
			}
			return itemRetry
		},
		end: func() { tell(errLoopEnded) },
	})
}

// errLoopEnded is a retried phase 1's outcome when its loop ended first:
// vfserr.ErrUnavailable.
var errLoopEnded = errClosed
