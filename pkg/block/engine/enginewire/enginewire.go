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
	"encoding/hex"
	"fmt"
	"log/slog"

	"github.com/cubbit/dittofs/pkg/block/engine"
	"github.com/cubbit/dittofs/pkg/block/journal"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/metadata/kvstore"
	"github.com/cubbit/dittofs/pkg/openstate"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// Params is what the composition takes from its caller.
type Params struct {
	Node    entity.NodeID
	Devices []string // one journal directory per device
	Store   *kvstore.Store
	Clock   engine.Clock
	Logger  *slog.Logger
	// Tracer is the host's tracer, nil when tracing is off. Composed into
	// each journal's Tracer option over the journal's own identity, so no
	// option is passed and the journal's nil-guarded emitters pay nothing
	// while it is nil. The engine's own Deps.Tracer stays noTrace.
	Tracer trace.Tracer
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
	return open, engine.Deps{
		Node:    p.Node,
		Devices: p.Devices,
		Open: func(ctx context.Context, dir string, floor journal.Version, expect journal.JournalID, recorded []entity.SyncMark) (engine.Journal, error) {
			marks := make([]journal.SyncMark, len(recorded))
			for i, m := range recorded {
				marks[i] = journal.SyncMark(m)
			}
			j, err := journal.Open(ctx, dir, floor, expect, marks, journalOptions(p, expect)...)
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

// journalOptions adds to the options every journal opens with the two the
// identity the open resolved carries: the tracer adapted to the journal's
// own, and the logger naming this journal — each only when the caller
// supplied it, so an untraced or unlogged journal passes neither option and
// the journal's nil-guarded emitters pay nothing.
func journalOptions(p Params, expect journal.JournalID) []journal.Option {
	opts := []journal.Option{journal.WithClock(p.Clock)}
	if p.Tracer != nil {
		opts = append(opts, journal.WithTracer(journalTracer(p.Tracer, expect)))
	}
	if p.Logger != nil {
		opts = append(opts, journal.WithLogger(p.Logger.With("journal", hex.EncodeToString(expect[:]))))
	}
	// The caller's own options come last, so one it names itself, a logger or
	// a tracer, replaces the one derived from the node's.
	return append(opts, p.Journal...)
}

// journalTracer adapts the host's OpenTelemetry tracer to the journal's own
// Tracer: each span is named journal.<op>, the journal's prefix on the op the
// journal chose, and carries the journal's identity as an attribute, built
// once here, when the adapter is built. The span ends with the operation's
// error, recorded and surfaced as the span's failure.
//
// decision: the failure's status message is the raw error, which can carry the
// journal directory's absolute path, though a span is meant to hold no path.
// It holds because that path is the operator's bootstrap setting, not one a
// user chose, and spans leave only to the operator's own collector. Set the
// status from the error's class alone once spans leave to anyone else.
//
// decision: the adapter starts a span for every op the journal names, with no
// IsSampled check on the parent, so RFC 25 §6.1's sampled-parent rule is
// knowingly unapplied: a request-path journal op — a reserve_refused, a fill
// — is always traced as a root of its own, and a traced call allocates a
// span whatever its parent. It holds because only the dev
// DITTOFS_OTLP_ENDPOINT gate ever supplies a tracer, no adapter or request
// span exists on the node yet, and a request context the SDK built without a
// sampled parent is rooted by the provider's ParentBased(AlwaysSample)
// anyway. Withdraw it when a request span reaches the node: the journal marks
// its background contexts, the adapter gates the request-path ops on
// IsSampled, and the engine's own Deps.Tracer takes the same adaptation.
func journalTracer(t trace.Tracer, id journal.JournalID) journal.Tracer {
	ident := attribute.String("journal", hex.EncodeToString(id[:]))
	return func(ctx context.Context, op journal.SpanOp, attrs ...slog.Attr) (context.Context, func(err error)) {
		kvs := make([]attribute.KeyValue, 0, len(attrs)+1)
		kvs = append(kvs, ident)
		for _, a := range attrs {
			if kv, ok := otelAttr(a); ok {
				kvs = append(kvs, kv)
			}
		}
		c, s := t.Start(ctx, "journal."+string(op), trace.WithAttributes(kvs...))
		return c, func(err error) {
			if err != nil {
				s.RecordError(err)
				s.SetStatus(codes.Error, err.Error())
			}
			s.End()
		}
	}
}

// otelAttr is a slog.Attr as the span attribute of its value, the four value
// shapes the journal's spans carry — string, bool and both integer kinds.
// Anything else answers none, keeping the span's attributes the closed set
// the journal's own emitters pass.
func otelAttr(a slog.Attr) (attribute.KeyValue, bool) {
	k := attribute.Key(a.Key)
	switch a.Value.Kind() {
	case slog.KindString:
		return k.String(a.Value.String()), true
	case slog.KindBool:
		return k.Bool(a.Value.Bool()), true
	case slog.KindInt64:
		return k.Int64(a.Value.Int64()), true
	case slog.KindUint64:
		// decision: the uint64 figure narrows to int64, so a value at or
		// above 2^63 wraps negative. No figure the journal puts on a span —
		// a stream number, segment count, moved or freed bytes — can reach
		// it. Switch to a string attribute if one ever could.
		return k.Int64(int64(a.Value.Uint64())), true
	}
	return attribute.KeyValue{}, false
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
