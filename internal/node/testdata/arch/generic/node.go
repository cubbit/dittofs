// The node's surface with a method on a generic receiver the rule cannot resolve.
package node

import "context"

type Config struct {
	Bootstrap any
	KV        any
	Clock     any
	Metrics   any
	Logger    any
}

type Node struct{ cfg Config }

func New(cfg Config) (*Node, error) { return &Node{cfg: cfg}, nil }

func (n *Node) Start(ctx context.Context) error { return nil }

func (n *Node) Stop(ctx context.Context) error { return nil }

func (n *Node) helper() {}

type box[T any] struct{}

func (b *box[T]) Open() {}
