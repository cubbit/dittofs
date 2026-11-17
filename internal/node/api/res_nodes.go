package api

import (
	"context"
	"iter"

	"github.com/cubbit/dittofs/pkg/health"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/node/apiv1"
)

// nodeOf is n on the wire.
func nodeOf(n entity.Node) apiv1.Node {
	return apiv1.Node{ID: idString(n.ID), Address: n.Address, Binary: n.Binary, Domain: n.Domain, Lapsed: n.Lapsed}
}

// nodes is GET /nodes, every node's record in ID order.
var nodes = listing[entity.Node, apiv1.Node]{
	records: func(ctx context.Context, d Deps, after []byte, _ map[string]string) iter.Seq2[entity.Node, error] {
		var from entity.NodeID
		copy(from[:], after)
		return d.Nodes.NodeRecords(ctx, from)
	},
	key:  func(n entity.Node) []byte { return n.ID[:] },
	item: func(n entity.Node, _ perPage) apiv1.Node { return nodeOf(n) },
}

// node is GET /nodes/{id}, 404 for a node that never registered.
func node(ctx context.Context, d Deps, rq request[none]) (apiv1.Node, error) {
	if d.Nodes == nil {
		return apiv1.Node{}, errNoStore
	}
	n, found, err := d.Nodes.Node(ctx, entity.NodeID(rq.ID))
	if err == nil && !found {
		err = errNotFound
	}
	return nodeOf(n), err
}

// conditions is the node's health conditions, its own and the
// installation's.
type conditions interface {
	Conditions(dst []health.Condition) []health.Condition
}

// scopes is each health scope's name on the wire.
var scopes = map[health.Scope]string{health.ScopeStore: "store", health.ScopeNamespace: "namespace",
	health.ScopeJournal: "journal", health.ScopeShare: "share", health.ScopeNode: "node",
	health.ScopeInstallation: "installation"}

// healthOf is GET /api/v1/health: the readiness word Ready answers and
// every condition Health holds, each with its cause, none when the node has
// no Health.
func healthOf(_ context.Context, d Deps, _ request[none]) (apiv1.Health, error) {
	h := apiv1.Health{Ready: d.Ready(), Conditions: []apiv1.Condition{}}
	if d.Health == nil {
		return h, nil
	}
	for _, c := range d.Health.Conditions(nil) {
		w := apiv1.Condition{Name: c.Name, Scope: scopes[c.Subject.Scope], ID: c.Subject.ID, Subject: c.Subject.Name, Detail: c.Detail}
		if c.Cause != nil {
			w.Cause = c.Cause.Error()
		}
		h.Conditions = append(h.Conditions, w)
	}
	return h, nil
}
