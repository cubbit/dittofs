// The node's surface with a Config with a field the host does not supply.
package node

import "context"

type Config struct {
	Bootstrap any
	KV        any
	Clock     any
	Metrics   any
	Logger    any
	Journal   any
	Fault     any
}

type Node struct{ cfg Config }

func New(cfg Config) (*Node, error) { return &Node{cfg: cfg}, nil }

func (n *Node) Start(ctx context.Context) error { return nil }

func (n *Node) Stop(ctx context.Context) error { return nil }

func (n *Node) helper() {}
