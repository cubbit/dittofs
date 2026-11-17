package apiv1

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
