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

// sweeper is the sweep's series, and the hook its loop calls.
type sweeper struct {
	// idempotency counts the idempotency records the sweep deleted.
	idempotency prometheus.Counter
	// turned, when set, is called at the end of each turn with what each of
	// its Sweep calls that answered deleted.
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
// records each call deleted. Each call is one bounded batch, reported to loop
// as a turn, so a long backlog keeps the loop live and a call that hangs
// does not.
//
// ponytail: a Sweep that fails is logged at each turn and raises no health
// condition, though one stopped by a record that does not decode fails the
// same way at every turn until it is repaired. Raise a condition once an
// operator must see a stuck sweep from the health call rather than the log.
func (n *Node) sweepTurn(ctx context.Context, loop *health.Loop) {
	var calls []kvstore.Swept
	for ctx.Err() == nil {
		sw, err := n.meta.Nodes().Sweep(ctx, auditRetention)
		loop.Turn()
		if err != nil {
			if ctx.Err() == nil {
				n.cfg.Logger.Error("sweep: delete the expired records", "error", err, "undecodable", kvstore.SweepRowUndecodable(err))
			}
			break
		}
		n.sweep.idempotency.Add(float64(sw.Idempotency))
		calls = append(calls, sw)
		if !sw.More {
			break
		}
	}
	if n.sweep.turned != nil {
		n.sweep.turned(calls)
	}
}
