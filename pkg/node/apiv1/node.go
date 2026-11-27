package apiv1

import "time"

// Node is one node as a resource.
type Node = Resource[NodeSpec, NodeStatus]

// NodeSpec is what an operator declares of a node: nothing yet.
type NodeSpec struct{}

// NodeStatus is a node as it registered at its last start: the address it
// serves on, the binary version it runs, its failure domain, and whether a
// takeover marked it lapsed; and the positions it reports having reconciled
// through, absent until it reports any.
type NodeStatus struct {
	Address    string      `json:"address"`
	Binary     string      `json:"binary"`
	Domain     string      `json:"domain"`
	Lapsed     bool        `json:"lapsed"`
	Reconciled *Reconciled `json:"reconciled,omitempty"`
}

// Reconciled is the position a node has reconciled through of each
// reconciled kind's counter.
type Reconciled struct {
	Settings   uint64 `json:"settings"`
	ShareList  uint64 `json:"share_list"`
	Principals uint64 `json:"principals"`
}

// Connection is one connection open on a node, as GET
// /api/v1/nodes/{id}/connections lists it (RFC 23 §4.2): the protocol it
// speaks, the client's address, the protocol's session it carries, empty
// where it carries none, and when it was accepted.
type Connection struct {
	Protocol string    `json:"protocol"`
	Remote   string    `json:"remote"`
	Session  string    `json:"session"`
	Since    time.Time `json:"since"`
}
