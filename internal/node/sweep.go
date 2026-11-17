package node

import (
	"context"
	"time"

	"github.com/cubbit/dittofs/pkg/health"
	"github.com/cubbit/dittofs/pkg/metadata/kvstore"
	"github.com/prometheus/client_golang/prometheus"
)

const (
	// auditRetention is how long the sweep keeps an audit record: RFC 23
	// §6's floor for audit.retention, until the node reads that setting.
	auditRetention = 90 * 24 * time.Hour
	// sweepInterval is the sweep's wait between turns, counted from the start
	// of the turn before: the watchdog's idle period, so the wait itself keeps
	// the loop live.
	sweepInterval = health.IdleEvery
	// stepSweep names the sweep's loop.
	stepSweep = "sweep"
)

// sweeper is the sweep's series and the state only its loop touches.
type sweeper struct {
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
// ponytail: a Sweep that fails is logged at each turn and raises no health
// condition, though one stopped by a record that does not decode fails the
// same way at every turn until it is repaired. Raise a condition once an
// operator must see a stuck sweep from the health call rather than the log.
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
			return
		}
	}
	for ctx.Err() == nil {
		var sw kvstore.Swept
		running, err := n.bounded(ctx, func(ctx context.Context) error {
			got, err := n.meta.Nodes().Sweep(ctx, auditRetention)
			if err == nil {
				w.idempotency.Add(float64(got.Idempotency))
			}
			sw = got
			return err
		})
		loop.Turn()
		w.running = running
		if err != nil {
			if ctx.Err() == nil {
				n.cfg.Logger.Warn("sweep: delete the expired records", "error", err, "undecodable", kvstore.SweepRowUndecodable(err))
			}
			return
		}
		calls = append(calls, sw)
		if !sw.More {
			return
		}
	}
}
