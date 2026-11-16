// Package enginewire composes the engine's dependencies over the metadata
// store and open state. The node and the engine's test fixture both build
// the engine from it, so the fixture runs the wiring the node ships.
//
// decision: this is the one composition code imported by another package:
// the node and enginetest both build the engine from it, so the fixture runs
// the wiring the node ships, departing from RFC 8 §2.1's "nothing imports
// it". Only internal/node and enginetest import it.
package enginewire

import (
	"context"
	"crypto/rand"
	"log/slog"

	"github.com/cubbit/dittofs/pkg/block/engine"
	"github.com/cubbit/dittofs/pkg/block/journal"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/metadata/kvstore"
	"github.com/cubbit/dittofs/pkg/openstate"
)

// Params is what the composition takes from its caller.
type Params struct {
	Node    entity.NodeID
	Devices []string // one journal directory per device
	Store   *kvstore.Store
	Clock   engine.Clock
	Logger  *slog.Logger
	// Journal are the options each device's journal opens with, after one
	// reading Clock.
	//
	// decision: the journal's settings arrive here, as journal.WithSettings,
	// because Engine.Apply takes only the engine's own settings and forwards
	// none to the journal; the node passes none, so its journals open on the
	// defaults. Move the journal's settings to Apply, and narrow this to
	// storage and mount options, when the node first applies configured
	// journal settings.
	Journal []journal.Option
}

// Compose builds open state over p.Store and returns it with the engine's
// Deps: each device's journal opened by journal.Open, the store's views, open
// state's hold on a clone's source, and a Tracer that starts no span. It
// opens and starts nothing; the caller passes the Deps to engine.New.
//
// decision: Compose shares construction only. Starting and stopping, the
// table started before the engine and stopped after it, is still sequenced
// by each caller; share it the same way when the node's Start lands, so the
// fixture runs the order the node ships.
func Compose(p Params) (*openstate.Table, engine.Deps, error) {
	open, err := openstate.New(openstate.Deps{Records: p.Store, Node: p.Node, Clock: p.Clock, Rand: rand.Reader, Logger: p.Logger})
	if err != nil {
		return nil, engine.Deps{}, err
	}
	opts := append([]journal.Option{journal.WithClock(p.Clock)}, p.Journal...)
	return open, engine.Deps{
		Node:    p.Node,
		Devices: p.Devices,
		Open: func(ctx context.Context, dir string, floor journal.Version, expect journal.JournalID) (engine.Journal, error) {
			j, err := journal.Open(ctx, dir, floor, expect, nil, opts...)
			if err != nil {
				return nil, err
			}
			return engine.JournalOf(j), nil
		},
		Existence: p.Store.Existence(),
		Content:   p.Store.Content(),
		Lifecycle: p.Store.Lifecycle(),
		Holds:     openstate.EngineHolds(open, p.Store.Opens()),
		Clock:     p.Clock,
		Logger:    p.Logger,
		Tracer:    noTrace,
	}, nil
}

func noTrace(ctx context.Context, _ string, _ ...slog.Attr) (context.Context, func(error)) {
	return ctx, func(error) {}
}
