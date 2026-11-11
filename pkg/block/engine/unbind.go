package engine

import (
	"context"
	"errors"
	"fmt"
	"maps"

	"github.com/cubbit/dittofs/pkg/block/journal"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/vfs/vfserr"
)

// Unbind refuses everything new for the share and waits for what already
// entered it (RFC 8 §2.3): its calls, any work registered in its gate and the
// removal batch the loop is running for it. Content calls then answer
// ErrStale. It waits within ctx, never holding the lifecycle mutex, and a
// context that ends returns its own error with the share still refusing: a
// second Unbind joins what is left. keepHolds is refused with ErrInvalid, as
// no offload exists to keep a removal's holds. A share with no binding, or one
// already removed, answers nil. While Start runs it answers ErrDelay, and
// after Close began ErrUnavailable.
func (e *Engine) Unbind(ctx context.Context, id entity.ShareID, keepHolds bool) error {
	if keepHolds {
		return fmt.Errorf("engine: unbind share %v: keeping the holds is not built: %w",
			id, vfserr.ErrInvalid)
	}

	b, err := e.beginLifecycle(id, func(b *binding) bool {
		return !b.removed
	})
	if err != nil || b == nil {
		return err
	}

	defer e.endCall(id)

	b.gate.shut()

	if err := e.joinWork(ctx, &b.gate); err != nil {
		return err
	}

	return b.gc.removals.joinRunning(ctx, id)
}

// beginLifecycle takes the lifecycle entry of share id and answers its
// binding, nil when the share has none or wants rejects its binding, in which
// case no entry is taken.
func (e *Engine) beginLifecycle(id entity.ShareID, wants func(*binding) bool) (*binding, error) {
	e.mu.Lock()

	defer e.mu.Unlock()

	switch {
	case e.closed.Load():
		return nil, errClosed
	case e.starting:
		return nil, &vfserr.Delay{Cause: vfserr.DelayFrozen}
	}

	if _, busy := e.busy[id]; busy {
		return nil, &vfserr.Delay{Cause: vfserr.DelayConflict}
	}

	b := e.table()[id]
	if b == nil || !wants(b) {
		return nil, nil
	}

	e.busy[id] = struct{}{}

	return b, nil
}

// Drop ends the share's journal tag (RFC 8 §2.3): the removal items it still
// has on the loop, its files in the group commit, its handle, then the
// whole-tag Forget, and the writes that Forget dropped get their Done once,
// charging nothing. The share must be unbound: its calls and batches have
// ended, or Drop answers ErrInvalid. The binding stays, as a removed one that
// holds the tag and no handle, so the share answers ErrStale and a repeated
// Drop only forgets again. A step that fails leaves the rest to the retry. A
// share with no binding answers nil.
//
// ponytail: every dropped share keeps a removed binding in the share table,
// one entry per share removed since the engine started. Keep a set of the
// removed share IDs instead when that count shows in a profile.
func (e *Engine) Drop(ctx context.Context, id entity.ShareID) error {
	b, err := e.beginLifecycle(id, func(*binding) bool {
		return true
	})
	if err != nil || b == nil {
		return err
	}

	defer e.endCall(id)

	if !b.removed {
		if !b.gate.drained() {
			return fmt.Errorf("engine: drop share %v: its unbind has not completed: %w",
				id, vfserr.ErrInvalid)
		}

		if err := e.endShareWork(ctx, b); err != nil {
			return err
		}

		// A close that failed may have left the handle open, as the
		// journal's does, or closed it: a retry's close then answers
		// ErrClosed, which is a closed handle.
		if err := b.h.Close(); err != nil && !errors.Is(err, journal.ErrClosed) {
			return fmt.Errorf("engine: drop share %v: close handle: %w", id, e.journalErr(err))
		}

		b = e.swapRemoved(b)
	}

	if err := e.forget(ctx, b); err != nil {
		return err
	}

	runAll(e.discard(b))

	return nil
}

// endShareWork ends what the share still has in the engine's loops: its
// removal items, and its files in the group commit, whose batch in flight it
// waits for. It records the files on b, so a retry finds them.
func (e *Engine) endShareWork(ctx context.Context, b *binding) error {
	l := b.gc.removals

	// A retry after a failed step appends the same files again: discard
	// settles a file once, as its entry is gone after the first, so a repeat
	// is harmless.
	b.files = append(b.files, l.dropShare(b.share.ID)...)

	if err := l.joinRunning(ctx, b.share.ID); err != nil {
		return err
	}

	files, err := b.gc.takeDue(ctx, b.share.ID)
	b.files = append(b.files, files...)

	return err
}

// swapRemoved replaces b in the share table by a removed binding that holds
// the share's ID and tag, the files its Drop has still to settle, and no
// handle, so Close never closes the handle again.
func (e *Engine) swapRemoved(b *binding) *binding {
	rb := &binding{
		share:   Share{ID: b.share.ID, Tag: b.share.Tag},
		removed: true,
		seq:     b.seq,
		files:   b.files,
	}

	e.mu.Lock()

	defer e.mu.Unlock()

	t := make(bindings, len(e.table()))
	maps.Copy(t, e.table())
	t[rb.share.ID] = rb
	e.shares.Store(&t)

	return rb
}

// forget runs the whole-tag Forget of removed binding b in the journal.
func (e *Engine) forget(ctx context.Context, b *binding) error {
	e.mu.Lock()
	vl, err := e.onlyJournal()
	e.mu.Unlock()

	if err != nil {
		return err
	}

	if err := vl.j.Forget(ctx, b.share.Tag); err != nil {
		return fmt.Errorf(
			"engine: drop share %v: forget the tag: %w",
			b.share.ID,
			e.journalErr(err),
		)
	}

	return nil
}

// discard takes every pending operation of b's files out of their entries,
// releases a guard a cut-short release left held, and answers the Done of each
// write it dropped, to run once no lock is held.
func (e *Engine) discard(b *binding) (dones []func()) {
	for _, f := range b.files {
		fe := e.files.lockOf(f)
		if fe == nil {
			continue
		}

		for i := range fe.ops {
			if d := fe.ops[i].done; d != nil {
				dones = append(dones, d)
				fe.ops[i].done = nil
			}
		}

		fe.ops = nil

		if fe.guardDone != nil {
			fe.unguard()
		}

		e.files.unlock(f, fe)
	}

	b.files = nil

	return dones
}
