package node

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
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

// The reconciled counters, in the order countersOf holds them, as the series
// and config_generation_behind name them.
const (
	kindSettings = iota
	kindShares
	kindPrincipals
)

var counterKinds = [...]string{kindSettings: "settings", kindShares: "shares", kindPrincipals: "principals"}

// countersOf is c's values, indexed by their kind.
func countersOf(c kvstore.Counters) [len(counterKinds)]uint64 {
	return [...]uint64{kindSettings: c.Settings, kindShares: c.ShareList, kindPrincipals: c.Principals}
}

// statusWriter is the status writer's state, which only its loop touches.
type statusWriter struct {
	fault storeFault
	// last is the canonical bytes of the status last committed.
	last []byte
	// running, when set, answers a write a turn stopped waiting on past
	// storeBound, which may still commit.
	running <-chan error
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
//
// A write the turn stopped waiting on past storeBound may still commit, and
// a later write would then be overwritten by its older status. So while it
// runs no turn writes, and once it settles last is forgotten, so the turn
// writes the status as it then is whatever the late write left.
func (n *Node) statusTurn(ctx context.Context) {
	w := &n.status
	s := n.nodeStatus()
	b, _ := json.Marshal(s) // a NodeStatus holds integers, strings and bytes, which always marshal
	if w.running != nil {
		select {
		case err := <-w.running:
			n.cfg.Logger.Debug("node status: a write past its bound settled", "error", err)
			w.running, w.last = nil, nil
		default:
		}
	}
	wrote := false
	if w.running == nil && !bytes.Equal(b, w.last) {
		running, err := n.bounded(ctx, func(ctx context.Context) error { return n.meta.Nodes().PutNodeStatus(ctx, s) })
		w.running = running
		if err == nil {
			w.last, wrote = b, true
		}
		w.fault.observed(ctx, n, stepStatus, err, slog.Attr{})
	} else {
		w.fault.summarize(ctx, n, stepStatus)
	}
	if w.turned != nil {
		w.turned(s, wrote)
	}
}

// nodeStatus is what the node reports: the positions it has reconciled
// through, the records it refused, the operation kinds its runner has no
// step for, and its health conditions.
//
// ponytail: the node reports three high-water positions and the exceptions,
// not one generation per record, so the status stays one small record
// however many records there are. "Applied" is inferred from a position,
// which holds only because the node acts on changes in counter order and
// lists every change it did not apply. Report per-record positions if a
// component ever applies records out of order.
//
// ponytail: the restart-pending set and the secret versions are empty, as
// nothing applies a restart-class setting or resolves a secret yet; and the
// claim generation is zero, as no namespace is claimed. Fill each with the
// component that first applies, resolves or claims it.
func (n *Node) nodeStatus() entity.NodeStatus {
	pos, refused := n.rec.report()
	for _, kind := range n.runner.refusedKinds() {
		refused = append(refused, refusedKind(kind))
	}
	return entity.NodeStatus{
		Node:       n.cfg.Bootstrap.Node.ID,
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
// generation zero. An operation's own ID is sixteen bytes and the store
// refuses an operation of a kind that long, so a reader tells the two apart
// by length. Report each operation instead if a reader needs to name the
// operation left pending rather than its kind.
func refusedKind(kind string) entity.RefusedRecord {
	p := apiv1.Problem{Status: http.StatusNotImplemented, Title: "the node has no step for the operation's kind", Detail: kind}
	b, _ := json.Marshal(p) // a Problem holds strings, ints and bools, which always marshal
	return entity.RefusedRecord{Record: entity.ResourceRef{Kind: entity.ResourceOperation, ID: []byte(kind)}, Error: b}
}

// condition is a health condition as the status stores it: what raised it
// and what it is about, without its cause and detail.
type condition struct {
	Name    string `json:"name"`
	Scope   string `json:"scope"`
	ID      string `json:"id,omitempty"`
	Subject string `json:"subject,omitempty"`
}

// maxPerName is how many conditions of one name the status stores.
const maxPerName = 32

// encodeConditions is conds as the status stores them: a JSON array, sorted
// so the same conditions encode to the same bytes whatever order their
// sources listed them in, at most maxPerName of each name; nil when there
// are none. A condition's cause and detail are left out, as their text
// changes while the condition stands, journal_full's figures for one, and
// each change would rewrite the status at every turn.
//
// ponytail: the conditions are stored as this package's own JSON, with no
// cause, detail, severity or since, as no reader decodes them yet; the
// health call serves them whole, live. Move to RFC 25's condition shape,
// its volatile text kept out of the change check, with its first reader.
//
// ponytail: past maxPerName conditions of one name, those sorting last are
// dropped silently, so the record stays small when every share raises one.
// Store a count of those dropped once a reader needs to know.
func encodeConditions(conds []health.Condition) []byte {
	if len(conds) == 0 {
		return nil
	}
	out := make([]condition, len(conds))
	for i, c := range conds {
		out[i] = condition{Name: c.Name, Scope: c.Subject.Scope.String(), ID: c.Subject.ID, Subject: c.Subject.Name}
	}
	slices.SortFunc(out, func(a, b condition) int {
		return cmp.Or(cmp.Compare(a.Name, b.Name), cmp.Compare(a.Scope, b.Scope), cmp.Compare(a.ID, b.ID), cmp.Compare(a.Subject, b.Subject))
	})
	out = slices.Compact(out)
	var kept []condition
	for i, c := range out {
		if i < maxPerName || out[i-maxPerName].Name != c.Name {
			kept = append(kept, c)
		}
	}
	b, _ := json.Marshal(kept) // strings always marshal
	return b
}

// positions is the counters the node has reconciled through and how long
// each has been read above it, read together.
func (n *Node) positions() (pos [len(counterKinds)]uint64, lag [len(counterKinds)]time.Duration) {
	r := &n.rec
	r.mu.Lock()
	defer r.mu.Unlock()
	return countersOf(r.pos), r.lags(n.cfg.Clock.Elapsed())
}

// nodeConditions is the node's own conditions as the health evaluator reads
// them: start_failed after a Start that failed, and config_generation_behind
// for each counter read above its position for longer than behindAfter, its
// subject the node named by the counter's kind.
//
// ponytail: config_generation_behind names the counter that is behind, not
// the records behind it, which the node does not know without reading them.
// Name them once a reader needs which records a node has not applied.
type nodeConditions struct{ n *Node }

func (c *nodeConditions) Conditions(dst []health.Condition) []health.Condition {
	dst = append(dst, c.n.startFailed()...)
	_, lags := c.n.positions()
	for k, lag := range lags {
		if lag > behindAfter {
			sub := c.n.subject()
			sub.Name = counterKinds[k]
			dst = append(dst, health.Condition{
				Name:    "config_generation_behind",
				Subject: sub,
				Detail:  fmt.Sprintf("the %s counter has been read above the position reconciled through for longer than %v", counterKinds[k], behindAfter),
			})
		}
	}
	return dst
}

// subject is the node as a condition's subject.
func (n *Node) subject() health.Subject {
	return health.Subject{
		Scope: health.ScopeNode,
		ID:    fmt.Sprintf("%x", n.cfg.Bootstrap.Node.ID[:]),
	}
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
	pos, lag := s.n.positions()
	for k, kind := range counterKinds {
		ch <- prometheus.MustNewConstMetric(reconciledPosition, prometheus.GaugeValue, float64(pos[k]), kind)
		ch <- prometheus.MustNewConstMetric(reconcileErrors, prometheus.CounterValue, float64(s.n.rec.errors[k].Load()), kind)
		ch <- prometheus.MustNewConstMetric(generationLag, prometheus.GaugeValue, lag[k].Seconds(), kind)
	}
	ch <- prometheus.MustNewConstMetric(restartPending, prometheus.GaugeValue, 0)
}
