// Package node is the server: one DittoFS node, built from what its host
// supplies, started and stopped by it. Only the server's start command and
// the node's test harness import it.
package node

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"

	"github.com/cubbit/dittofs/internal/node/api"
	"github.com/cubbit/dittofs/internal/node/api/auth"
	"github.com/cubbit/dittofs/internal/sidecar"
	"github.com/cubbit/dittofs/internal/vfs"
	"github.com/cubbit/dittofs/pkg/adapter/settings"
	"github.com/cubbit/dittofs/pkg/block/engine"
	"github.com/cubbit/dittofs/pkg/block/engine/enginewire"
	"github.com/cubbit/dittofs/pkg/block/journal"
	"github.com/cubbit/dittofs/pkg/bootstrap"
	"github.com/cubbit/dittofs/pkg/health"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/metadata/kv"
	"github.com/cubbit/dittofs/pkg/metadata/kvstore"
	"github.com/cubbit/dittofs/pkg/metrics"
	"github.com/cubbit/dittofs/pkg/node/apiv1"
	"github.com/cubbit/dittofs/pkg/openstate"
	"github.com/cubbit/dittofs/pkg/resources"
)

// Config is what the host supplies and the node cannot build itself.
type Config struct {
	// Bootstrap is the configuration the host read before anything opened.
	Bootstrap bootstrap.Bootstrap
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
	sweep    sweeper
	health   *health.Evaluator
	watchdog *health.Watchdog
	api      *api.Server
	life     lifecycle
	budgets  *resources.Budgets
	pools    []pool // budgets' pools, for their series and the zero check
	// The adapters: one slot per protocol over the one runner that drives
	// the sidecars around them.
	nfs      *slot[settings.NFS]
	smb      *slot[settings.SMB]
	sidecars *sidecar.Runner
	// drains is what every adapter's drain runs under, a pass's included:
	// reconcile's stop does not end it, and stop step 2 ends it, through
	// endDrains, at its drain deadline.
	drains    context.Context
	endDrains context.CancelFunc
	// adapting is set by start step 9: from then on apply calls the slots.
	// Written before reconcile's loop starts, and read by apply alone.
	adapting bool
	// ending, when set, is called by step 3 of each share removal once its
	// bound is set on the clock, before it waits on anything.
	ending func(entity.ShareID)
}

// New resolves the adapters' addresses, refusing an adapters.listen it
// cannot bind, opens the metadata store over cfg.KV, refusing a KV that
// holds no installation or one this binary does not read, then builds open
// state, the engine and the filesystem, each empty from its own Deps, the
// budgets and their series, the operation runner with the steps of the
// kinds this node drives, the watchdog, the adapters' one Deps, the sidecar
// runner and a slot per protocol, and the API over the store's control
// plane and node view, the runner's progress, the health evaluator, the
// slots' connections and the writer's wait. It starts nothing: no journal,
// goroutine or listener exists until Start, but for the socket an unset
// adapters.listen opens and closes at once to learn whether tcp6 does.
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
func New(cfg Config) (*Node, error) { return newNode(cfg, nil) }

// newNode is New with authenticate as the API's Authenticate, which only a
// test sets.
func newNode(cfg Config, authenticate func(*http.Request) (auth.Caller, error)) (*Node, error) {
	if cfg.KV == nil {
		return nil, errors.New("node: Config.KV is missing")
	}
	n, err := build(cfg, authenticate)
	if err != nil {
		return nil, errors.Join(err, cfg.KV.Close())
	}
	return n, nil
}

func build(cfg Config, authenticate func(*http.Request) (auth.Caller, error)) (*Node, error) {
	if cfg.Metrics == nil {
		return nil, errors.New("node: Config.Metrics is missing")
	}
	// An empty address would bind a random port on every interface.
	if len(cfg.Bootstrap.API.Listen) == 0 || cfg.Bootstrap.Metrics.Listen == "" {
		return nil, errors.New("node: api.listen and metrics.listen must both be set")
	}
	// decision: RFC 13 Appendix C makes api.listen a list of addresses, but
	// the API binds one, so a second address is refused rather than left
	// unbound while the operator believes the API listens there. The refusal
	// names the key and the count, never an address, which may come from the
	// environment (RFC 13 §6). Withdraw it once the API listens on every
	// address the key lists.
	if n := len(cfg.Bootstrap.API.Listen); n > 1 {
		return nil, fmt.Errorf(
			"node: api.listen names %d addresses, and the node's API binds one address for now",
			n,
		)
	}
	tcp6 := false
	if len(cfg.Bootstrap.Adapters.Listen) == 0 {
		if l, err := net.Listen("tcp6", "[::]:0"); err == nil {
			tcp6 = true
			_ = l.Close()
		}
	}
	addrs, err := listenAddrs(cfg.Bootstrap.Adapters.Listen, tcp6)
	if err != nil {
		return nil, err
	}
	n := &Node{cfg: cfg, rec: newReconciler(), sweep: newSweeper()}
	if n.meta, err = kvstore.Open(context.Background(), cfg.KV); err != nil {
		return nil, fmt.Errorf("node: metadata store: %w", err)
	}
	open, deps, err := enginewire.Compose(enginewire.Params{
		Node:    cfg.Bootstrap.Node.ID,
		Devices: journalPaths(cfg.Bootstrap.Journals),
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
	n.budgets, n.pools = newBudgets()
	if err := reg.Register(budgetSeries(n.pools)); err != nil {
		return nil, fmt.Errorf("node: budget series: %w", err)
	}
	if err := reg.Register(nodeSeries{n}); err != nil {
		return nil, fmt.Errorf("node: node series: %w", err)
	}
	if err := reg.Register(n.sweep.idempotency); err != nil {
		return nil, fmt.Errorf("node: sweep series: %w", err)
	}
	n.health = &health.Evaluator{}
	n.health.Add(&engineConditions{n.engine.Stats})
	n.health.Add(&nodeConditions{n})
	if n.fs, err = vfs.New(vfs.Deps{Meta: n.meta, Open: n.open, Engine: n.engine, Metrics: reg, Logger: cfg.Logger, Running: n.rec.serves}); err != nil {
		return nil, fmt.Errorf("node: %w", err)
	}
	steps := operationSteps(n.meta.Nodes())
	steps[string(apiv1.OpShareDelete)] = stepRow{n.shareDeletion, n.removalProgress}
	n.runner = newRunner(n.meta.ControlPlane(), cfg.Clock, cfg.Bootstrap.Node.ID, cfg.Logger, steps)
	n.watchdog = health.NewWatchdog(cfg.Clock)
	n.sidecars = sidecar.New()
	n.drains, n.endDrains = context.WithCancel(context.Background())
	n.nfs, n.smb = newSlots(adapterDeps(n.fs, n.budgets, cfg.Metrics, cfg.Logger, n.watchdog), n.sidecars, addrs,
		[]uint16{listenPort(cfg.Bootstrap.API.Listen[0]), listenPort(cfg.Bootstrap.Metrics.Listen)}, cfg.Clock, n.drains)
	n.health.Add(&adapterConditions{n})
	n.life.ready.Store(readyStarting)
	// decision: the API gets no TLS, local socket, signing key or tracer,
	// and in production no Authenticate, because nothing the node is handed
	// holds them yet: the bootstrap carries no certificate, key or socket
	// path, the installation's signing key is a secret no secret store
	// reads, and Config names no tracer. So a start on an address but
	// loopback is refused for want of a certificate, no local socket is
	// served, every call but a public row's answers 401, every POST and
	// DELETE, which must carry an Idempotency-Key, answers 503 or 428, and
	// spans go to the no-op tracer. Fill
	// each from the bootstrap, the secret store, sign-in and the host's
	// tracer as they land.
	n.api = api.New(api.Deps{
		Ready:        n.readiness,
		Served:       n.life.served.Load,
		Live:         n.watchdog.Live,
		Addr:         cfg.Bootstrap.API.Listen[0],
		MetricsAddr:  cfg.Bootstrap.Metrics.Listen,
		Metrics:      cfg.Metrics,
		Logger:       cfg.Logger,
		Authenticate: authenticate,
		Store:        n.meta.ControlPlane(),
		Nodes:        n.meta.Nodes(),
		Progress:     n.runner.progressOf,
		Health:       n.health,
		Connections:  n.connections,
		Clock:        cfg.Clock,
		Node:         cfg.Bootstrap.Node.ID,
		Applied:      n.applied,
	})
	return n, nil
}

// connections is every connection open now on the adapters the slots' last
// calls left running, serving or still draining, each adapter.ConnInfo as
// the API's Connection, so the API imports no adapter (RFC 23 §10's CP16).
func (n *Node) connections() []apiv1.Connection {
	var cs []apiv1.Connection
	for _, o := range []*outcome{n.nfs.last.Load(), n.smb.last.Load()} {
		if o == nil || o.running == nil {
			continue
		}
		for c := range o.running.Connections() {
			cs = append(cs, apiv1.Connection{Protocol: c.Protocol, Remote: c.Remote.String(), Session: c.Session, Since: c.Since})
		}
	}
	return cs
}

// journalPaths is the directory of each journal device, in order.
func journalPaths(journals []bootstrap.Journal) []string {
	paths := make([]string, len(journals))
	for i, j := range journals {
		paths[i] = j.Path
	}

	return paths
}
