// Package enginewire composes the engine's dependencies over the metadata
// store and open state, and starts and stops the two. The node and the
// engine's test fixture both build, start and stop the engine through it, so
// the fixture runs the wiring and the order the node ships.
//
// decision: this is the one composition code imported by another package:
// the node and enginetest both build, start and stop the engine through it,
// so the fixture runs the wiring and the order the node ships, departing
// from RFC 8 §2.1's "nothing imports it". Only internal/node and enginetest
// import it.
package enginewire

import (
	"context"
	"crypto/rand"
	"fmt"
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
	// none to the journal; the node passes no settings, so its journals
	// open on the defaults. Move the journal's settings to Apply, and narrow this to
	// storage and mount options, when the node first applies configured
	// journal settings.
	Journal []journal.Option
}

// Compose builds open state over p.Store and returns it with the engine's
// Deps: each device's journal opened by journal.Open, the store's views, open
// state's hold on a clone's source, and a Tracer that starts no span. It
// opens and starts nothing; the caller passes the Deps to engine.New, and
// Start and Stop start and stop the two in the order the node ships.
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

// Start starts open state with sink and order, which raises the node epoch,
// then the engine over shares in one call. When the engine refuses to
// start, open state is stopped again, so a failed Start leaves both as
// built. Its error names which of the two failed.
func Start(ctx context.Context, open *openstate.Table, e *engine.Engine, sink openstate.Sink, order openstate.Order, shares []engine.Share) error {
	if err := open.Start(ctx, sink, order); err != nil {
		return fmt.Errorf("open state: %w", err)
	}
	if err := e.Start(ctx, shares); err != nil {
		open.Stop()
		return fmt.Errorf("engine: %w", err)
	}
	return nil
}

// Stop closes the engine, then stops open state. An engine whose close
// fails, a join of its clone batches unfinished among the causes, leaves
// open state running, since those batches still send it their CloneClose;
// a later Stop closes the engine again.
func Stop(ctx context.Context, open *openstate.Table, e interface{ Close(context.Context) error }) error {
	if err := e.Close(ctx); err != nil {
		return fmt.Errorf("engine: %w", err)
	}
	open.Stop()
	return nil
}

// Binding is share s as the engine binds it, with journal limit limit: its
// namespace the one its write generation names, serving or quiesced as its
// record says.
func Binding(s entity.Share, limit int64) engine.Share {
	b := engine.Share{ID: s.ID, Tag: journal.ShareTag(s.Tag), Shard: s.Shard, Limit: limit, State: engine.Serving}
	for _, ns := range s.Namespaces {
		if ns.Generation == s.Write {
			b.Namespace = ns.Namespace
		}
	}
	if s.State == entity.ShareQuiesced {
		b.State = engine.Quiesced
	}
	return b
}
