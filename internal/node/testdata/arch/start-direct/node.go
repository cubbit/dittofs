// The node starting and stopping open state and the engine itself rather
// than through enginewire.
package node

import (
	"context"

	"github.com/cubbit/dittofs/pkg/engine"
	"github.com/cubbit/dittofs/pkg/openstate"
)

func start(ctx context.Context, open *openstate.Table, e *engine.Engine, sink openstate.Sink, order openstate.Order) error {
	if err := open.Start(ctx, sink, order); err != nil {
		return err
	}
	return e.Start(ctx, nil)
}

func stop(ctx context.Context, open *openstate.Table, e *engine.Engine) error {
	err := e.Close(ctx)
	open.Stop()
	return err
}
