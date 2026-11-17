package node

import (
	"github.com/cubbit/dittofs/internal/vfs"
	"github.com/cubbit/dittofs/pkg/block/engine"
	"github.com/cubbit/dittofs/pkg/health"
)

// NewStepping is New with stepped called after each start step completes,
// named as Start's error names it; before each part's stop is waited on,
// its bound already running, as "wait " and the part's name; and after each
// part stops, as "stop " and its name.
func NewStepping(stepped func(n *Node, step string)) func(Config) (*Node, error) {
	return func(cfg Config) (*Node, error) {
		n, err := New(cfg)
		if err == nil {
			n.life.stepped = func(step string) { stepped(n, step) }
		}
		return n, err
	}
}

// Addrs is the API's and the metrics listener's bound addresses, nil before
// Start opened them.
func (n *Node) Addrs() (api, metrics string) {
	if n.life.api == nil {
		return "", ""
	}
	return n.life.api.String(), n.life.met.String()
}

// StartFailed is the node's start_failed condition, nil unless a Start failed.
func (n *Node) StartFailed() []health.Condition { return n.startFailed() }

// Filesystem is the filesystem the node built.
func (n *Node) Filesystem() *vfs.Filesystem { return n.fs }

// Engine is the engine the node built.
func (n *Node) Engine() *engine.Engine { return n.engine }
