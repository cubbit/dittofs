// The node's surface with a Node with an exported field.
package node

import "context"

type Config struct {
	Bootstrap any
	KV        any
	Clock     any
	Metrics   any
	Logger    any
	Tracer    any
	Journal   any
}

type Node struct {
	State string
	cfg   Config
}

func New(cfg Config) (*Node, error) { return &Node{cfg: cfg}, nil }

func (n *Node) Start(ctx context.Context) error { return nil }

func (n *Node) Stop(ctx context.Context) error { return nil }

func (n *Node) helper() {}

func Preflight() {}
