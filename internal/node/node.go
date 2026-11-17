// Package node is the server: one DittoFS node, built from what its host
// supplies, started and stopped by it. Only the server's start command and
// the node's test harness import it.
package node

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/cubbit/dittofs/internal/node/api"
	"github.com/cubbit/dittofs/internal/vfs"
	"github.com/cubbit/dittofs/pkg/block/engine"
	"github.com/cubbit/dittofs/pkg/block/engine/enginewire"
	"github.com/cubbit/dittofs/pkg/block/journal"
	"github.com/cubbit/dittofs/pkg/config"
	"github.com/cubbit/dittofs/pkg/health"
	"github.com/cubbit/dittofs/pkg/metadata/kv"
	"github.com/cubbit/dittofs/pkg/metadata/kvstore"
	"github.com/cubbit/dittofs/pkg/metrics"
	"github.com/cubbit/dittofs/pkg/node/apiv1"
	"github.com/cubbit/dittofs/pkg/openstate"
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
	// Journal are options for every journal the node opens, after one
	// reading Clock: the engine's and the one Start reads each device's
	// identity with. A test passes journaltest.SuitableMount here, so the
	// host's own mount cannot refuse its temporary directories.
	Journal []journal.Option
}

// Node is one running DittoFS node.
type Node struct {
	cfg      Config
	meta     *kvstore.Store
	open     *openstate.Table
	engine   *engine.Engine
	fs       *vfs.Filesystem
	runner   *runner
	rec      reconciler
	status   statusWriter
	health   *health.Evaluator
	watchdog *health.Watchdog
	api      *api.Server
	life     lifecycle
}

// New opens the metadata store over cfg.KV, refusing a KV that holds no
// installation or one this binary does not read, then builds open state,
// the engine and the filesystem, each empty from its own Deps, the
// operation runner with the steps of the kinds this node drives, the
// watchdog and the API's listeners. It starts nothing: no journal, goroutine
// or listener exists until Start.
//
// ponytail: the node both builds and drives its components. Split the
// builder out when it gains dependencies a multi-node test cannot replace
// cheaply.
//
// decision: New builds over another installation's KV, because nothing in
// Config names the installation to expect. Start refuses that KV at its first
// step, before its first write, by reading the installation each device's
// journal names and comparing it with the store's. Check it here instead once
// the bootstrap carries the installation's ID.
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
	if cfg.Metrics == nil {
		return nil, errors.New("node: Config.Metrics is missing")
	}
	// An empty address would bind a random port on every interface.
	if cfg.Bootstrap.APIAddr == "" || cfg.Bootstrap.MetricsAddr == "" {
		return nil, errors.New("node: Config.Bootstrap.APIAddr and MetricsAddr must both be set")
	}
	n := &Node{cfg: cfg, rec: newReconciler()}
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
		Journal: cfg.Journal,
	})
	if err != nil {
		return nil, fmt.Errorf("node: %w", err)
	}
	n.open = open
	if n.engine, err = engine.New(deps); err != nil {
		return nil, fmt.Errorf("node: %w", err)
	}
	reg := cfg.Metrics.Registry()
	if err := reg.Register(engineSeries{n.engine.Stats}); err != nil {
		return nil, fmt.Errorf("node: engine series: %w", err)
	}
	if err := reg.Register(nodeSeries{n}); err != nil {
		return nil, fmt.Errorf("node: node series: %w", err)
	}
	n.health = &health.Evaluator{}
	n.health.Add(&engineConditions{n.engine.Stats})
	n.health.Add(&nodeConditions{n})
	if n.fs, err = vfs.New(vfs.Deps{Meta: n.meta, Open: n.open, Engine: n.engine, Metrics: reg, Logger: cfg.Logger, Running: n.rec.serves}); err != nil {
		return nil, fmt.Errorf("node: %w", err)
	}
	steps := operationSteps(n.meta.Nodes())
	steps[string(apiv1.OpShareDelete)] = stepRow{n.shareDeletion, n.removalProgress}
	n.runner = newRunner(n.meta.ControlPlane(), cfg.Clock, cfg.Bootstrap.Node, cfg.Logger, steps)
	n.watchdog = health.NewWatchdog(cfg.Clock)
	n.life.ready.Store(readyStarting)
	n.api = api.New(api.Deps{
		Ready:       n.life.readiness,
		Live:        n.watchdog.Live,
		Addr:        cfg.Bootstrap.APIAddr,
		MetricsAddr: cfg.Bootstrap.MetricsAddr,
		Metrics:     cfg.Metrics,
		Logger:      cfg.Logger,
	})
	return n, nil
}
