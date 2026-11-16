// The node's surface with one export too many: Extra.
package node

import "context"

type Config struct{ Logger any }

type Node struct{}

func New(cfg Config) (*Node, error) { return &Node{}, nil }

func (n *Node) Start(ctx context.Context) error { return nil }

func (n *Node) Stop(ctx context.Context) error { return nil }

func (n *Node) helper() {}

func Extra() {}
