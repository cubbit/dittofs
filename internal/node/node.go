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
	"sync/atomic"

	"github.com/cubbit/dittofs/internal/identity"
	"github.com/cubbit/dittofs/internal/identity/directory"
	"github.com/cubbit/dittofs/internal/identity/netlogon"
	"github.com/cubbit/dittofs/internal/node/api"
	"github.com/cubbit/dittofs/internal/node/api/apisettings"
	"github.com/cubbit/dittofs/internal/node/api/auth"
	"github.com/cubbit/dittofs/internal/sidecar"
	"github.com/cubbit/dittofs/internal/tlsconfig"
	"github.com/cubbit/dittofs/internal/vfs"
	"github.com/cubbit/dittofs/pkg/adapter/settings"
	"github.com/cubbit/dittofs/pkg/block/engine"
	"github.com/cubbit/dittofs/pkg/block/engine/enginewire"
	"github.com/cubbit/dittofs/pkg/block/journal"
	"github.com/cubbit/dittofs/pkg/bootstrap"
	"github.com/cubbit/dittofs/pkg/health"
	"github.com/cubbit/dittofs/pkg/keyprovider"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/metadata/kv"
	"github.com/cubbit/dittofs/pkg/metadata/kvstore"
	"github.com/cubbit/dittofs/pkg/metrics"
	"github.com/cubbit/dittofs/pkg/node/apiv1"
	"github.com/cubbit/dittofs/pkg/openstate"
	"github.com/cubbit/dittofs/pkg/resources"
	"github.com/cubbit/dittofs/pkg/secrets"
	"go.opentelemetry.io/otel/trace"
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
	// Tracer is the host's tracer, nil when tracing is off. Each journal
	// the engine opens starts its spans below it, naming the journal by its
	// identity, and identity its sign-in and resolution spans; nil traces
	// nothing.
	//
	// decision: the host builds the tracing provider and hands it here, and
	// only the journals and identity take the tracer; the root building the
	// provider from the tracing settings and handing the tracer to every
	// component is not built. This field goes once the root builds the provider from those
	// settings.
	Tracer trace.Tracer
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
	identity *identity.Server
	// secrets is the secret provider, over the storage key and the
	// protocol key when the bootstrap names one.
	secrets  *secrets.Provider
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
	// snap is the one deadlined, single-flight engine.Stats fetch the engine
	// series, the journal series and the health evaluator read.
	snap *snapshot
	// storageKey is the storage role's wrapping key the bootstrap's
	// keys.storage names; Stop closes it.
	storageKey keyprovider.KeyProvider
	// protocolKey is the protocol role's wrapping key keys.protocol names,
	// nil when it names none; Stop closes it.
	protocolKey keyprovider.KeyProvider
	// tokens issues and verifies access tokens over the installation's
	// signing key, for sign-in and the rotation. Stop clears the versions
	// they hold.
	tokens *auth.Tokens
	// auth signs callers in and answers who each API call is.
	auth *auth.Service
	// ending, when set, is called by step 3 of each share removal once its
	// bound is set on the clock, before it waits on anything.
	ending func(entity.ShareID)
	// machineRef is the machine password's reference last applied, which
	// identity's State does not hold.
	machineRef atomic.Pointer[entity.SecretRef]
}

// New resolves the adapters' addresses, refusing an adapters.listen it
// cannot bind, opens the metadata store over cfg.KV, refusing a KV that
// holds no installation or one this binary does not read, and opens the
// storage key keys.storage names, refusing a bootstrap that names none and
// dialling a KMIP server when it names one there, and the protocol key
// keys.protocol names, when it names one. It then builds open
// state and the engine, each empty from its own Deps, the budgets and their
// series, the secret provider and the access tokens over the storage key,
// the identity server over the store, the secret provider, the logger as
// its logon sink, the tracer, the directory's and the domain controllers'
// dialers and the store's control plane, the
// filesystem over the engine and identity, the operation runner with the steps of the kinds this
// node drives, the watchdog, the adapters' one Deps, the sidecar runner and
// a slot per protocol, and the API over the store's control plane and node
// view, the runner's progress, the health evaluator, the slots' connections,
// the writer's wait and identity's directory and join hold. It starts nothing: no journal,
// goroutine or listener exists until Start, but for the socket an unset
// adapters.listen opens and closes at once to learn whether tcp6 does.
//
// ponytail: the node both builds and drives its components. Split the
// builder out when it gains dependencies a multi-node test cannot replace
// cheaply.
//
// decision: a configured storage key that cannot be opened refuses the
// build, the data plane included, though only the control plane uses the
// key: the node needs it to seal the signing key's versions and to verify
// access tokens, so a node built without it could serve no signed-in caller.
// Overturned if the key opens lazily on first use, which
// would let the data plane start while a KMIP server is down.
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
func New(cfg Config) (*Node, error) { return newNode(cfg, seams{}) }

// seams is what only a test hands the node in place of its own: the API's
// Authenticate and SigningKey, and the directory's and the domain
// controllers' dialers. With no Authenticate, the API takes the node's
// sign-in and, with no SigningKey, the access tokens' current key; a test's
// Authenticate with no SigningKey leaves the API none. A nil dialer is the
// production one.
type seams struct {
	authenticate func(*http.Request) (auth.Caller, error)
	signingKey   func(context.Context) ([]byte, error)
	directory    directory.Dialer
	dc           netlogon.Dialer
}

// newNode is New with the seams in, which only a test sets.
func newNode(cfg Config, in seams) (*Node, error) {
	if cfg.KV == nil {
		return nil, errors.New("node: Config.KV is missing")
	}
	// decision: a bootstrap that names no journal device is a missing wiring
	// piece, refused before the store opens — equally over a store that is
	// not initialised, whose refusal the operator would otherwise read as
	// the advice to run the init step against a node that has nowhere to
	// keep journals. Overturned when the node builds over a store that names
	// its devices, which would make an empty list the store's fact to refuse.
	if len(cfg.Bootstrap.Journals) == 0 {
		return nil, errors.Join(fmt.Errorf("node: %w", errNoDevice), cfg.KV.Close())
	}
	if cfg.Bootstrap.Keys.Storage.Kind == "" {
		return nil, errors.Join(errNoStorageKey, cfg.KV.Close())
	}
	n, err := build(cfg, in)
	if err != nil {
		return nil, errors.Join(err, cfg.KV.Close())
	}
	return n, nil
}

// errNoStorageKey refuses a bootstrap that names no storage key, as dfs init
// does.
var errNoStorageKey = errors.New("node: keys.storage is not set: the administrator's password and the signing key are sealed under it")

// build builds the node; once it has opened the storage key, a step that
// fails after closes it.
func build(cfg Config, in seams) (_ *Node, err error) {
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

	n := &Node{cfg: cfg, rec: newReconciler(newTable(), newReaches()), sweep: newSweeper()}
	if n.meta, err = kvstore.Open(context.Background(), cfg.KV); err != nil {
		// decision: over Badger a store whose first key has one of v1's
		// prefixes is v1's, the only program that wrote one at
		// metadata.badger.path, so it is refused as v1. This format's records
		// with the format record lost keep ErrForeignStore's wording, as does
		// any store over another backend, where v1 never ran: neither is
		// v1's to clear. Overturned if a Badger directory can hold another
		// program's keys under v1's prefixes.
		if b := cfg.Bootstrap.Metadata.Backend; errors.Is(err, kvstore.ErrV1Store) && (b == "" || b == "badger") {
			return nil, v1Store{refuseV1Found(cfg.Bootstrap.Metadata.Badger.Path, "a metadata store"), err}
		}
		return nil, fmt.Errorf("node: metadata store: %w", err)
	}
	// decision: the storage key opens under no context a signal can cut,
	// since New takes none and Config may not grow one. A KMIP key's open is
	// bounded instead: two round trips for the current key and two for each
	// retired one, each at most kmip.timeout, 5 s unless set, so a signal
	// during New waits out at most that before dfs start stops. Overturned
	// when New takes the start's context.
	if n.storageKey, err = keyprovider.NewProvider(context.Background(), cfg.Bootstrap.Keys.Storage.Provider()); err != nil {
		return nil, fmt.Errorf("node: the storage key: %w", err)
	}
	defer func() {
		if err != nil {
			err = errors.Join(err, n.closeKeys())
		}
	}()
	if cfg.Bootstrap.Keys.Protocol.Kind != "" {
		if n.protocolKey, err = keyprovider.NewProvider(context.Background(), cfg.Bootstrap.Keys.Protocol.Provider()); err != nil {
			return nil, fmt.Errorf("node: the protocol key: %w", err)
		}
	}
	open, deps, err := enginewire.Compose(enginewire.Params{
		Node:    cfg.Bootstrap.Node.ID,
		Devices: journalPaths(cfg.Bootstrap.Journals),
		Store:   n.meta,
		Clock:   cfg.Clock,
		Logger:  cfg.Logger,
		Tracer:  cfg.Tracer,
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
	snap := newSnapshot(n.engine.Stats, cfg.Clock, newPartialCounter())
	n.snap = snap
	if err := reg.Register(snap.partial); err != nil {
		return nil, fmt.Errorf("node: export partial counter: %w", err)
	}
	if err := reg.Register(engineSeries{snap}); err != nil {
		return nil, fmt.Errorf("node: engine series: %w", err)
	}
	if err := reg.Register(&journalSeries{snap: snap}); err != nil {
		return nil, fmt.Errorf("node: journal series: %w", err)
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
	n.health.Add(&engineConditions{snap: snap, subject: n.subject()})
	n.health.Add(&nodeConditions{n})
	p, err := n.openSecrets(context.Background())
	if err != nil {
		return nil, err
	}
	n.secrets = p
	dial := in.directory
	if dial == nil {
		dial = directory.DialTLS
	}
	dc := in.dc
	if dc == nil {
		dc = netlogon.NewDialer((&net.Dialer{}).DialContext)
	}
	if n.identity, err = identity.New(identity.Deps{Store: n.meta, Secrets: p, Clock: cfg.Clock, Metrics: cfg.Metrics, Logger: cfg.Logger,
		Tracer: cfg.Tracer, Logons: identity.LogLines(cfg.Logger), Directory: dial, DomainController: dc,
		ControlPlane: n.meta.ControlPlane()}); err != nil {
		return nil, fmt.Errorf("node: %w", err)
	}
	n.health.Add(identityConditions{n})
	if n.fs, err = vfs.New(vfs.Deps{Meta: n.meta, Open: n.open, Engine: n.engine, Metrics: reg, Logger: cfg.Logger, Clock: cfg.Clock, Running: n.rec.serves,
		Identity: n.identity, Auth: n.identity, Names: n.identity}); err != nil {
		return nil, fmt.Errorf("node: %w", err)
	}
	steps := operationSteps(n.meta.Nodes())
	steps[string(apiv1.OpShareDelete)] = stepRow{run: n.shareDeletion, progress: n.removalProgress}
	steps[string(apiv1.OpRotateSigningKey)] = n.rotationRow(p)
	steps[string(apiv1.OpJoinDomain)] = stepRow{run: n.identity.JoinStep, progress: n.identity.JoinProgress}
	steps[string(apiv1.OpRotateMachinePassword)] = stepRow{run: n.identity.RotateStep, progress: n.identity.RotateProgress, failed: n.identity.RotateFailed}
	steps[string(apiv1.OpResolvePrincipal)] = stepRow{run: n.identity.ResolvePrincipalStep, progress: n.identity.ResolvePrincipalProgress}
	steps[string(apiv1.OpTestDirectory)] = stepRow{run: n.identity.TestDirectoryStep, progress: n.identity.TestDirectoryProgress}
	steps[string(apiv1.OpNTHashDelete)] = stepRow{run: n.identity.NTHashDeleteStep, progress: n.identity.NTHashDeleteProgress}
	n.runner = newRunner(n.meta.ControlPlane(), cfg.Clock, cfg.Bootstrap.Node.ID, cfg.Logger, steps)
	n.watchdog = health.NewWatchdog(cfg.Clock)
	n.sidecars = sidecar.New()
	n.drains, n.endDrains = context.WithCancel(context.Background())
	n.nfs, n.smb = newSlots(adapterDeps(n.fs, n.budgets, cfg.Metrics, cfg.Logger, n.watchdog), n.sidecars, addrs,
		[]uint16{listenPort(cfg.Bootstrap.API.Listen[0]), listenPort(cfg.Bootstrap.Metrics.Listen)}, cfg.Clock, n.drains)
	n.health.Add(&adapterConditions{n})
	n.life.ready.Store(readyStarting)
	// The session lifetime is the api section's default until the settings
	// reach the node's components.
	if n.auth, err = auth.NewService(auth.Deps{Store: n.meta.ControlPlane(), Accounts: n.identity, Tokens: n.tokens,
		Sealer: newAuthSealer(p), Settings: apisettings.Defaults, Clock: cfg.Clock, Metrics: reg}); err != nil {
		return nil, fmt.Errorf("node: %w", err)
	}
	if in.authenticate == nil {
		in.authenticate = n.auth.Authenticate
		if in.signingKey == nil {
			in.signingKey = n.tokens.SigningKey
		}
	}
	// decision: the API is handed no tracer, as Config's tracer is the
	// journals', not the API's: the API's own tracer is RFC 23's own wiring,
	// fed from Config once it lands, so the API's spans go to the no-op
	// tracer. An empty api.socket serves no local socket.
	n.api = api.New(api.Deps{
		Ready:        n.readiness,
		Served:       n.life.served.Load,
		Live:         n.watchdog.Live,
		Addr:         cfg.Bootstrap.API.Listen[0],
		MetricsAddr:  cfg.Bootstrap.Metrics.Listen,
		TLS:          tlsconfig.Config{CertFile: cfg.Bootstrap.API.TLS.CertFile, KeyFile: cfg.Bootstrap.API.TLS.KeyFile},
		Socket:       cfg.Bootstrap.API.Socket,
		Metrics:      cfg.Metrics,
		Logger:       cfg.Logger,
		Authenticate: in.authenticate,
		Credentials:  n.auth,
		SigningKey:   in.signingKey,
		Store:        n.meta.ControlPlane(),
		Settings:     &n.rec.table,
		Nodes:        n.meta.Nodes(),
		Progress:     n.runner.progressOf,
		Health:       n.health,
		Connections:  n.connections,
		Clock:        cfg.Clock,
		Node:         cfg.Bootstrap.Node.ID,
		Applied:      n.applied,
		Directory:    n.identity,
		Joins:        n.identity,
		Sealer:       providerSealer{p},
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
