// Package node is the server: one DittoFS node, built from what its host
// supplies, started and stopped by it. Only the server's start command and
// the node's test harness import it.
package node

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/cubbit/dittofs/internal/vfs"
	"github.com/cubbit/dittofs/pkg/block/engine"
	"github.com/cubbit/dittofs/pkg/block/engine/enginewire"
	"github.com/cubbit/dittofs/pkg/config"
	"github.com/cubbit/dittofs/pkg/metadata/kv"
	"github.com/cubbit/dittofs/pkg/metadata/kvstore"
	"github.com/cubbit/dittofs/pkg/metrics"
	"github.com/cubbit/dittofs/pkg/openstate"
	"github.com/prometheus/client_golang/prometheus"
)

// Config is what the host supplies and the node cannot build itself.
type Config struct {
	// Bootstrap is the configuration the host read before anything opened.
	Bootstrap config.Bootstrap
	// KV is the backend the node's metadata store opens on. The node owns it
	// from New on: Stop closes it, and so does a New that fails.
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
	cfg    Config
	meta   *kvstore.Store
	open   *openstate.Table
	engine *engine.Engine
	fs     *vfs.Filesystem
}

// New opens the metadata store over cfg.KV, refusing a KV that holds no
// installation or one this binary does not read, then builds open state,
// the engine and the filesystem, each empty from its own Deps. It starts
// nothing: no journal, goroutine or listener exists until Start.
//
// ponytail: the node both builds and drives its components. Split the
// builder out when it gains dependencies a multi-node test cannot replace
// cheaply.
//
// decision: New builds over another installation's KV, because nothing in
// Config names the installation to expect. That KV is refused only at the
// engine's Start, when it opens each journal and journal.Open checks the
// journal's identity against the one the store records. By then Start has
// already written to that KV (the node's record and its epoch come before
// the engine), so Start must check the installation before its first write.
// Check it here instead once the bootstrap carries its ID.
//
// decision: a node is built once per metrics registry. Stop does not
// unregister the filesystem's collectors, so a second New on the same
// registry is refused as a duplicate registration; the test harness gives
// each build a fresh registry. Unregister in Stop when an in-process
// rebuild on one registry is needed.
func New(cfg Config) (*Node, error) {
	if cfg.KV == nil {
		return nil, errors.New("node: Config.KV is missing")
	}
	n, err := build(cfg)
	if err != nil {
		return nil, errors.Join(err, cfg.KV.Close())
	}
	return n, nil
}

func build(cfg Config) (*Node, error) {
	n := &Node{cfg: cfg}
	var err error
	if n.meta, err = kvstore.Open(context.Background(), cfg.KV); err != nil {
		return nil, fmt.Errorf("node: metadata store: %w", err)
	}
	open, deps, err := enginewire.Compose(enginewire.Params{
		Node:    cfg.Bootstrap.Node,
		Devices: cfg.Bootstrap.Devices,
		Store:   n.meta,
		Clock:   cfg.Clock,
		Logger:  cfg.Logger,
	})
	if err != nil {
		return nil, fmt.Errorf("node: %w", err)
	}
	n.open = open
	if n.engine, err = engine.New(deps); err != nil {
		return nil, fmt.Errorf("node: %w", err)
	}
	var reg prometheus.Registerer
	if cfg.Metrics != nil {
		reg = cfg.Metrics.Registry()
		if err := reg.Register(engineSeries{n.engine.Stats}); err != nil {
			return nil, fmt.Errorf("node: engine series: %w", err)
		}
	}
	if n.fs, err = vfs.New(vfs.Deps{Meta: n.meta, Open: n.open, Engine: n.engine, Metrics: reg, Logger: cfg.Logger}); err != nil {
		return nil, fmt.Errorf("node: %w", err)
	}
	return n, nil
}

// Start starts the services New built. It does not start one yet: it
// returns errors.ErrUnsupported.
func (n *Node) Start(ctx context.Context) error {
	return errors.ErrUnsupported
}

// Stop closes what New opened: the KV the metadata store is open on.
func (n *Node) Stop(ctx context.Context) error {
	return n.cfg.KV.Close()
}
