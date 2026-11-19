package api

import (
	"bytes"
	"context"
	"encoding/binary"
	"iter"
	"net/http"
	"slices"

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

// nodeConnections is GET /nodes/{id}/connections: the connections listing,
// for this node's ID alone; any other ID is 404 before any adapter is read,
// as GET /nodes/{id} answers a node that never registered.
func nodeConnections(ctx context.Context, s *Server, r *http.Request, rt *route, id [16]byte) (answer, error) {
	if entity.NodeID(id) != s.d.Node {
		return answer{}, errNotFound
	}
	return list(connections)(ctx, s, r, rt, id)
}

// connRow is a connection with its key.
type connRow struct {
	apiv1.Connection
	key []byte
}

// connections is the connections open on this node now, read from its
// adapters, never from the store (RFC 23 §4.2), in key order, resuming
// after the key after.
//
// ponytail: it pages through list, which reads every node's status once a
// page though no connection uses it, an O(nodes) store read beside a
// node-local one. Give list a listing that reads no status once that read
// shows in a page's profile.
var connections = listing[connRow, apiv1.Connection]{
	records: func(_ context.Context, d Deps, after []byte, _ map[string]string) iter.Seq2[connRow, error] {
		var rows []connRow
		for _, c := range d.Connections() {
			if k := connKey(c); bytes.Compare(k, after) > 0 {
				rows = append(rows, connRow{c, k})
			}
		}
		slices.SortFunc(rows, func(a, b connRow) int { return bytes.Compare(a.key, b.key) })
		return func(yield func(connRow, error) bool) {
			for _, r := range rows {
				if !yield(r, nil) {
					return
				}
			}
		}
	},
	key:  func(r connRow) []byte { return r.key },
	item: func(r connRow, _ perPage) apiv1.Connection { return r.Connection },
}

// connKey is c's key, which orders connections by Since, then Remote, then
// Protocol: Since's Unix nanoseconds, big endian, which order as the times
// do, an accept time being after 1970; Remote's text, ended by a zero byte,
// which no address's text holds; then Protocol.
func connKey(c apiv1.Connection) []byte {
	k := binary.BigEndian.AppendUint64(nil, uint64(c.Since.UnixNano()))
	k = append(append(k, c.Remote...), 0)
	return append(k, c.Protocol...)
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
		//
		// decision: the cause is the condition's whole error text, not the
		// closed reason its owner reports, because health.Condition holds no
		// closed reason yet. Each condition's severity and since, the status
		// of the installation, the node and each scope, the share counts and
		// the worst shares wait on the same change. Overturned when the
		// health registry grows those fields.
		if c.Cause != nil {
			w.Cause = c.Cause.Error()
		}
		h.Conditions = append(h.Conditions, w)
	}
	return h, nil
}
