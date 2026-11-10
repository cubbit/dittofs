package engine

import (
	"context"
	"fmt"

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

	b, err := e.beginUnbind(id)
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

// beginUnbind takes the lifecycle entry of share id and answers its binding.
// It answers no binding for a share that has none or is removed already.
func (e *Engine) beginUnbind(id entity.ShareID) (*binding, error) {
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
	if b == nil || b.removed {
		return nil, nil
	}

	e.busy[id] = struct{}{}

	return b, nil
}
