package node

import (
	"context"
	"log/slog"
	"math"
	"time"

	"github.com/cubbit/dittofs/internal/node/api/apisettings"
	"github.com/cubbit/dittofs/pkg/health"
	"github.com/cubbit/dittofs/pkg/metadata/kvstore"
	"github.com/cubbit/dittofs/pkg/settings"
	"github.com/prometheus/client_golang/prometheus"
)

const (
	// auditKeep is the retention the sweep passes when the node holds no
	// published audit value: the largest duration, so no audit record goes
	// and every other kind is swept, the store refusing the zero a lesser
	// hold-back would read as.
	auditKeep = time.Duration(math.MaxInt64)
	// sweepInterval is the sweep's wait between turns, counted from the start
	// of the turn before: the watchdog's idle period, so the wait itself keeps
	// the loop live.
	sweepInterval = health.IdleEvery
	// stepSweep names the sweep's loop.
	stepSweep = "sweep"
)

// auditRetain is how long the sweep keeps an audit record: the audit
// section's value in the published snapshot, read each turn, so a change of
// audit.retention reaches the sweep at its next turn.
//
// decision: no published value, one refused at start or a section held back,
// keeps every audit record, the safe side: keeping is undone by fixing the
// setting, while a sweep on a guessed retention deletes what no value
// asked to. Overturned once a reader needs to know the audit sweep is
// held back, which would then be reported rather than implied.
func (n *Node) auditRetain() time.Duration {
	a, err := settings.Get[apisettings.Audit](
		n.rec.snapshot.Load(), "audit", n.cfg.Bootstrap.Node.ID)
	if err != nil {
		return auditKeep
	}

	return a.Retention
}

// sweeper is the sweep's series and the state only its loop touches.
type sweeper struct {
	fault storeFault
	// idempotency counts the idempotency records the sweep deleted.
	idempotency prometheus.Counter
	// running, when set, answers a Sweep call a turn stopped waiting on past
	// storeBound, which may still commit.
	running <-chan error
	// turned, when set, is called at the end of each turn with what each of
	// its Sweep calls that answered within storeBound deleted.
	turned func([]kvstore.Swept)
}

func newSweeper() sweeper {
	return sweeper{idempotency: prometheus.NewCounter(prometheus.CounterOpts{
		Name: "dittofs_api_idempotency_records_deleted_total",
		Help: "Idempotency records the expiry sweep deleted.",
	})}
}

// The sweep is the node's garbage collection of the control-plane records
// that expire. Several kinds of record are written with a lifetime and are
// never deleted by the call that wrote them:
//
//   - an idempotency record, which answers a retried write with its first
//     answer, 24 hours after the write;
//   - an operation, 7 days after it ends;
//   - a session or an API token, at its expiry;
//   - a sign-in count, 15 minutes after its last failure for a source and 24
//     hours after its last attempt for an account and source pair;
//   - an audit record, once it is older than the audit retention.
//
// Each of them is indexed by its expiry when it is written. The store's
// Sweep reads that index from the oldest row and deletes what is due, and
// nothing else deletes them: without the sweep the store grows with every
// write and sign-in, for ever.
//
// sweepLoop runs a sweep turn at once and then one every sweepInterval on
// the node's clock until ctx ends, reporting each to loop.
func (n *Node) sweepLoop(ctx context.Context, loop *health.Loop) {
	for {
		next := n.cfg.Clock.After(sweepInterval)
		n.sweepTurn(ctx, loop)
		select {
		case <-ctx.Done():
			return
		case <-next:
		}
		loop.Idle()
	}
}

// sweepTurn calls the store's Sweep, again at once while it answers More,
// so a backlog is cleared within one turn, and counts the idempotency
// records each call deleted. Each call is one batch, bounded by storeBound on
// the node's clock and reported to loop as a turn, so a long backlog keeps
// the loop live, and so does a store that hangs: a call past its bound ends
// the turn.
//
// A call past its bound may still commit, and is counted if it does. While
// it runs, no turn calls Sweep, so at most one call is outstanding however
// long the store hangs.
//
// ponytail: a stopped sweep is visible in its incident logs. Raise a health
// condition once an operator must see a stuck sweep from the health call.
func (n *Node) sweepTurn(ctx context.Context, loop *health.Loop) {
	w := &n.sweep
	var calls []kvstore.Swept
	if w.turned != nil {
		defer func() { w.turned(calls) }()
	}
	if w.running != nil {
		select {
		case err := <-w.running:
			n.cfg.Logger.Debug("sweep: a call past its bound settled", "error", err)
			w.running = nil
		default:
			w.fault.summarize(ctx, n, stepSweep)
			return
		}
	}
	for ctx.Err() == nil {
		retention := n.auditRetain()
		var sw kvstore.Swept
		running, err := n.bounded(ctx, func(ctx context.Context) error {
			got, err := n.meta.Nodes().Sweep(ctx, retention)
			if err == nil {
				w.idempotency.Add(float64(got.Idempotency))
			}
			sw = got
			return err
		})
		loop.Turn()
		w.running = running
		if err != nil {
			w.fault.observed(ctx, n, stepSweep, err, slog.Bool("undecodable", kvstore.SweepRowUndecodable(err)))
			return
		}
		calls = append(calls, sw)
		if !sw.More {
			w.fault.observed(ctx, n, stepSweep, nil, slog.Attr{})
			return
		}
	}
}
