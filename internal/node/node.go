// Package node is the server: one DittoFS node, built from what its host
// supplies, started and stopped by it. Only the server's start command and
// the node's test harness import it.
package node

import (
	"context"
	"errors"
	"log/slog"

	"github.com/cubbit/dittofs/pkg/block/engine"
	"github.com/cubbit/dittofs/pkg/config"
	"github.com/cubbit/dittofs/pkg/metadata/kv"
	"github.com/cubbit/dittofs/pkg/metrics"
)

// Config is what the host supplies and the node cannot build itself.
type Config struct {
	// Bootstrap is the configuration the host read before anything opened.
	Bootstrap config.Bootstrap
	// KV is the backend the node's metadata store opens on.
	KV kv.KV
	// Clock is the node's one source of time.
	Clock engine.Clock
	// Metrics is the registry the node's series register in.
	Metrics *metrics.Registry
	// Logger is where the node logs.
	Logger *slog.Logger
}

// Node is one running DittoFS node.
type Node struct {
	cfg Config
}

// New builds a node from cfg. It opens nothing; Start does.
func New(cfg Config) (*Node, error) {
	return &Node{cfg: cfg}, nil
}

// Start opens the node's stores and starts its services. It does not start
// one yet: it returns errors.ErrUnsupported.
func (n *Node) Start(ctx context.Context) error {
	return errors.ErrUnsupported
}

// Stop stops what Start started. It returns errors.ErrUnsupported until
// Start starts anything.
func (n *Node) Stop(ctx context.Context) error {
	return errors.ErrUnsupported
}
