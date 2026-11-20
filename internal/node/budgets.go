package node

import (
	"context"

	"github.com/cubbit/dittofs/pkg/resources"
	"github.com/prometheus/client_golang/prometheus"
)

// The budgets' figures for a process memory limit of 8 GiB, the node RFC 24
// §2.3 works: a sixteenth of it for each of the receive, reply,
// connection-state and hold pools, and RFC 24 §4.4's connection_limit for
// it, the connection-state pool over twice the 64 KiB a connection costs.
//
// ponytail: fixed figures, whatever memory the host has, and the runtime's
// soft limit left unset. Derive them from the limit with Detect and SplitFor,
// setting the soft limit to fifteen sixteenths of it, exporting
// memory_limit_bytes, raising memory_limit_below_floors and bounding
// connection_limit by half the open-file limit too, once pkg/resources has
// Detect and SplitFor.
const (
	poolCeiling     = 512 << 20
	connectionLimit = 4096
)

// partBudgets is the zero check's part.
const partBudgets = "budgets"

// budgetNotZero is the zero check's record, which the test harness fails on.
const budgetNotZero = "node: budget not at zero after the adapters and the filesystem stopped"

// pool is one of the node's budgets as its series and the zero check read
// it: its pool label and the bytes reserved from it now.
type pool struct {
	name     string
	reserved func() int64
}

// newBudgets builds the node's budgets on the request path, receive and
// reply waiting and connection state and hold refusing, each at poolCeiling,
// and its connection count at connectionLimit (RFC 24 §2.3, §2.5, §4.4),
// with the pools that read them.
func newBudgets() (*resources.Budgets, []pool) {
	b := &resources.Budgets{
		Receive:    resources.NewWaitBudget("receive", poolCeiling),
		Reply:      resources.NewWaitBudget("reply", poolCeiling),
		Connection: resources.NewBudget("connection", poolCeiling),
		Hold:       resources.NewBudget("hold", poolCeiling),
		Conns:      resources.NewConnLimit(connectionLimit),
	}
	return b, []pool{
		{"receive", b.Receive.Reserved},
		{"reply", b.Reply.Reserved},
		{"connection", b.Connection.Reserved},
		{"hold", b.Hold.Reserved},
	}
}

// budgetSeries is RFC 24 §10's memory_pool_reserved_bytes and
// memory_pool_ceiling_bytes over the node's pools, read at each scrape.
type budgetSeries []pool

var (
	poolReserved = prometheus.NewDesc("dittofs_memory_pool_reserved_bytes", "Bytes reserved from the memory pool.", []string{"pool"}, nil)
	poolCeilings = prometheus.NewDesc("dittofs_memory_pool_ceiling_bytes", "The most the memory pool reserves at once.", []string{"pool"}, nil)
)

func (budgetSeries) Describe(ch chan<- *prometheus.Desc) {
	ch <- poolReserved
	ch <- poolCeilings
}

func (s budgetSeries) Collect(ch chan<- prometheus.Metric) {
	for _, p := range s {
		ch <- prometheus.MustNewConstMetric(poolReserved, prometheus.GaugeValue, float64(p.reserved()), p.name)
		ch <- prometheus.MustNewConstMetric(poolCeilings, prometheus.GaugeValue, poolCeiling, p.name)
	}
}

// checkBudgets is RFC 24 §2.7's zero check: it logs at error each pool still
// holding bytes, naming the pool and the bytes, and the connection count
// while it counts a connection, as pool conns with its connections. It
// returns nil: a pool or count not at zero holds nothing a later part waits
// for.
//
// decision: the check is the stop of a part started just before the
// filesystem's, which the filesystem's part uses, so it runs once the
// filesystem has stopped, and not at all while a filesystem past its bound
// may still release what it holds; Stop has no step of its own for it.
// Overturned if a part that charges a budget stops after the filesystem.
func (n *Node) checkBudgets(context.Context) error {
	for _, p := range n.pools {
		if r := p.reserved(); r != 0 {
			n.cfg.Logger.Error(budgetNotZero, "pool", p.name, "bytes", r)
		}
	}
	if c := n.budgets.Conns.InUse(); c != 0 {
		n.cfg.Logger.Error(budgetNotZero, "pool", "conns", "connections", c)
	}
	return nil
}
