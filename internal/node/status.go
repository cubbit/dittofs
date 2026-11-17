package node

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"time"

	"github.com/cubbit/dittofs/pkg/health"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/metadata/kvstore"
	"github.com/cubbit/dittofs/pkg/node/apiv1"
	"github.com/prometheus/client_golang/prometheus"
)

const (
	// statusInterval is the status writer's wait between turns, so the
	// status is written at most once per interval.
	statusInterval = time.Second
	// behindAfter is how long a counter may stay read above the position
	// reconciled through before config_generation_behind is raised.
	behindAfter = 5 * time.Second
	// stepStatus names the status writer's loop.
	stepStatus = "status"
)

// The reconciled counters, in the order a counterSet holds them, as the
// series label them.
const (
	kindSettings = iota
	kindShares
	kindPrincipals
)

var counterKinds = [...]string{kindSettings: "settings", kindShares: "shares", kindPrincipals: "principals"}

// counterSet is one value per reconciled counter, indexed by its kind.
type counterSet [len(counterKinds)]uint64

func countersOf(c kvstore.Counters) counterSet {
	return counterSet{kindSettings: c.Settings, kindShares: c.ShareList, kindPrincipals: c.Principals}
}

// statusWriter is the status writer's state, which only its loop touches.
type statusWriter struct {
	// last is the canonical bytes of the status last committed.
	last []byte
	// turned, when set, is called at the end of each turn with the status
	// it built and whether it committed it.
	turned func(s entity.NodeStatus, wrote bool)
}

// statusLoop runs a status turn at once and then one every statusInterval
// on the node's clock, counted from the start of the turn before, until ctx
// ends, reporting each to loop.
//
// ponytail: a pass that moves the positions is reported at the next turn,
// up to statusInterval later, rather than waking the writer; the rate limit
// would hold such a write back as long anyway. Wake the writer from the pass
// if a reader ever needs the status sooner than the rate limit allows.
func (n *Node) statusLoop(ctx context.Context, loop *health.Loop) {
	for {
		next := n.cfg.Clock.After(statusInterval)
		n.statusTurn(ctx)
		loop.Turn()
		select {
		case <-ctx.Done():
			return
		case <-next:
		}
		loop.Idle()
	}
}

// statusTurn builds the node's status and writes it through PutNodeStatus
// when its canonical bytes differ from those last committed. The last
// committed moves only once the write commits, so a write that failed is
// made again at the next turn.
func (n *Node) statusTurn(ctx context.Context) {
	s := n.nodeStatus()
	b, _ := json.Marshal(s) // a NodeStatus holds integers, strings and bytes, which always marshal
	wrote := false
	if !bytes.Equal(b, n.status.last) {
		err := n.bounded(ctx, func(ctx context.Context) error { return n.meta.Nodes().PutNodeStatus(ctx, s) })
		switch {
		case err == nil:
			n.status.last, wrote = b, true
		case ctx.Err() == nil:
			n.cfg.Logger.Warn("node status: write", "error", err)
		}
	}
	if n.status.turned != nil {
		n.status.turned(s, wrote)
	}
}

// nodeStatus is what the node reports: the positions it has reconciled
// through, the records it refused, the operation kinds its runner has no
// step for, and its health conditions.
//
// ponytail: the restart-pending set and the secret versions are empty, as
// nothing applies a restart-class setting or resolves a secret yet; and the
// claim generation is zero, as no namespace is claimed. Fill each with the
// component that first applies, resolves or claims it.
func (n *Node) nodeStatus() entity.NodeStatus {
	r := &n.rec
	r.mu.Lock()
	pos := r.pos
	r.mu.Unlock()
	refused := r.refusals()
	for _, kind := range n.runner.refusedKinds() {
		refused = append(refused, refusedKind(kind))
	}
	return entity.NodeStatus{
		Node:       n.cfg.Bootstrap.Node,
		Settings:   pos.Settings,
		ShareList:  pos.ShareList,
		Principals: pos.Principals,
		Refused:    refused,
		Conditions: encodeConditions(n.health.Conditions(nil)),
	}
}

// refusedKind is the refusal of every operation of kind, which the runner
// has no step for.
//
// decision: an operation kind is not a record and has no position, so its
// refusal names the kind as the record's ID, of kind operation, at
// generation zero; an operation's own ID is sixteen bytes, so a reader tells
// the two apart by length. Report each operation instead if a reader needs
// to name the operation left pending rather than its kind.
func refusedKind(kind string) entity.RefusedRecord {
	p := apiv1.Problem{Status: http.StatusNotImplemented, Title: "the node has no step for the operation's kind", Detail: kind}
	b, _ := json.Marshal(p) // a Problem holds strings, ints and bools, which always marshal
	return entity.RefusedRecord{Record: entity.ResourceRef{Kind: entity.ResourceOperation, ID: []byte(kind)}, Error: b}
}

// condition is a health condition as the status stores it.
type condition struct {
	Name    string       `json:"name"`
	Scope   health.Scope `json:"scope"`
	ID      string       `json:"id,omitempty"`
	Subject string       `json:"subject,omitempty"`
	Cause   string       `json:"cause,omitempty"`
	Detail  string       `json:"detail,omitempty"`
}

// encodeConditions is conds as the status stores them: a JSON array, sorted
// so the same conditions encode to the same bytes whatever order their
// sources listed them in; nil when there are none.
//
// ponytail: the conditions are stored as this package's own JSON, with no
// severity or since, as no reader decodes them yet. Move to RFC 25's
// condition shape with its first reader.
func encodeConditions(conds []health.Condition) []byte {
	if len(conds) == 0 {
		return nil
	}
	out := make([]condition, len(conds))
	for i, c := range conds {
		out[i] = condition{Name: c.Name, Scope: c.Subject.Scope, ID: c.Subject.ID, Subject: c.Subject.Name, Detail: c.Detail}
		if c.Cause != nil {
			out[i].Cause = c.Cause.Error()
		}
	}
	slices.SortFunc(out, func(a, b condition) int {
		return cmp.Or(cmp.Compare(a.Name, b.Name), cmp.Compare(a.Scope, b.Scope), cmp.Compare(a.ID, b.ID),
			cmp.Compare(a.Subject, b.Subject), cmp.Compare(a.Cause, b.Cause), cmp.Compare(a.Detail, b.Detail))
	})
	b, _ := json.Marshal(out) // strings and ints always marshal
	return b
}

// lags is how long each counter has been read above the position reconciled
// through, zero for one that is not.
func (n *Node) lags() (lag [len(counterKinds)]time.Duration) {
	now := n.cfg.Clock.Elapsed()
	r := &n.rec
	r.mu.Lock()
	defer r.mu.Unlock()
	for k, above := range r.above {
		if above {
			lag[k] = now - r.aboveSince[k]
		}
	}
	return lag
}

// nodeConditions is the node's own conditions as the health evaluator reads
// them: start_failed after a Start that failed, and config_generation_behind
// for each counter read above its position for longer than behindAfter.
type nodeConditions struct{ n *Node }

func (c *nodeConditions) Conditions(dst []health.Condition) []health.Condition {
	dst = append(dst, c.n.startFailed()...)
	for k, lag := range c.n.lags() {
		if lag > behindAfter {
			dst = append(dst, health.Condition{
				Name:    "config_generation_behind",
				Subject: c.n.subject(),
				Detail:  fmt.Sprintf("the %s counter has been read above the position reconciled through for longer than %v", counterKinds[k], behindAfter),
			})
		}
	}
	return dst
}

// subject is the node as a condition's subject.
func (n *Node) subject() health.Subject {
	return health.Subject{Scope: health.ScopeNode, ID: fmt.Sprintf("%x", n.cfg.Bootstrap.Node[:])}
}

// nodeSeries is the node's reconcile as the registry's series, read at each
// scrape.
type nodeSeries struct{ n *Node }

var (
	reconciledPosition = prometheus.NewDesc("dittofs_node_reconciled_position", "The counter value the node has reconciled through.", []string{"kind"}, nil)
	reconcileErrors    = prometheus.NewDesc("dittofs_node_reconcile_errors_total", "Reconcile steps that failed, by what they apply.", []string{"kind"}, nil)
	restartPending     = prometheus.NewDesc("dittofs_node_restart_pending", "Records awaiting a restart on this node.", nil, nil)
	generationLag      = prometheus.NewDesc("dittofs_config_generation_lag_seconds", "How long the counter has been read above the position reconciled through.", []string{"kind"}, nil)
)

func (nodeSeries) Describe(ch chan<- *prometheus.Desc) {
	for _, d := range []*prometheus.Desc{reconciledPosition, reconcileErrors, restartPending, generationLag} {
		ch <- d
	}
}

func (s nodeSeries) Collect(ch chan<- prometheus.Metric) {
	r := &s.n.rec
	r.mu.Lock()
	pos := countersOf(r.pos)
	r.mu.Unlock()
	lag := s.n.lags()
	for k, kind := range counterKinds {
		ch <- prometheus.MustNewConstMetric(reconciledPosition, prometheus.GaugeValue, float64(pos[k]), kind)
		ch <- prometheus.MustNewConstMetric(reconcileErrors, prometheus.CounterValue, float64(r.errors[k].Load()), kind)
		ch <- prometheus.MustNewConstMetric(generationLag, prometheus.GaugeValue, lag[k].Seconds(), kind)
	}
	ch <- prometheus.MustNewConstMetric(restartPending, prometheus.GaugeValue, 0)
}
