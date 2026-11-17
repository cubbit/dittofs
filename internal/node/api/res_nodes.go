package api

import (
	"context"
	"iter"

	"github.com/cubbit/dittofs/pkg/health"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/node/apiv1"
)

// nodeOf is n on the wire, with the positions its status in statuses
// reports, none when it holds none.
//
// decision: a status whose positions are all zero reads as none. The
// store reads a registered node that has reported no status as the zero
// status, so a report of nothing reconciled and no report at all are one
// read; both have reconciled nothing. Overturned by a read that tells them
// apart, should a client need to.
//
// decision: a node's generation is its record's epoch. The record changes
// only at a start, which raises the epoch before the node registers, so the
// epoch moves with every change. Overturned by the first write of the
// record outside a start, a takeover marking it lapsed among them, which
// then raises a generation of its own.
func nodeOf(n entity.Node, statuses []entity.NodeStatus) apiv1.Node {
	v := apiv1.Node{ID: idString(n.ID), Generation: n.Epoch,
		Status: apiv1.NodeStatus{Address: n.Address, Binary: n.Binary, Domain: n.Domain, Lapsed: n.Lapsed}}
	for _, st := range statuses {
		if st.Node == n.ID && st.Settings|st.ShareList|st.Principals != 0 {
			v.Status.Reconciled = &apiv1.Reconciled{Settings: st.Settings, ShareList: st.ShareList, Principals: st.Principals}
		}
	}
	return v
}

// nodes is GET /nodes, every node's record in ID order.
var nodes = listing[entity.Node, apiv1.Node]{
	records: func(ctx context.Context, d Deps, after []byte, _ map[string]string) iter.Seq2[entity.Node, error] {
		var from entity.NodeID
		copy(from[:], after)
		return d.Nodes.NodeRecords(ctx, from)
	},
	key:  func(n entity.Node) []byte { return n.ID[:] },
	item: func(n entity.Node, p perPage) apiv1.Node { return nodeOf(n, p.statuses) },
}

// node is GET /nodes/{id}, 404 for a node that never registered.
//
// ponytail: it reads every node's status to find this node's, O(nodes).
// Read one node's status when the store offers that read.
func node(ctx context.Context, d Deps, rq request[none]) (apiv1.Node, error) {
	if d.Nodes == nil {
		return apiv1.Node{}, errNoStore
	}
	n, found, err := d.Nodes.Node(ctx, entity.NodeID(rq.ID))
	if err != nil {
		return apiv1.Node{}, err
	}
	if !found {
		return apiv1.Node{}, errNotFound
	}
	statuses, err := d.Nodes.RegisteredStatuses(ctx)
	if err != nil {
		return apiv1.Node{}, err
	}
	return nodeOf(n, statuses), nil
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
		// decision: a viewer reads each condition's cause, its error's text,
		// as the route table asks. That is safe only while no health Source
		// puts a credential in an error it raises. Overturned by the first
		// cause that can carry one, and then by setting Cause from a typed
		// reason rather than an error's text.
		if c.Cause != nil {
			w.Cause = c.Cause.Error()
		}
		h.Conditions = append(h.Conditions, w)
	}
	return h, nil
}
