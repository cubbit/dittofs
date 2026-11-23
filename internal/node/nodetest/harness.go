// Package nodetest builds a node in process for a test: an installation
// initialised under t.TempDir(), over Badger on disk or in memory, on a test
// clock.
package nodetest

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cubbit/dittofs/internal/install"
	"github.com/cubbit/dittofs/internal/node"
	"github.com/cubbit/dittofs/internal/prof"
	"github.com/cubbit/dittofs/pkg/block/engine"
	"github.com/cubbit/dittofs/pkg/block/journal"
	"github.com/cubbit/dittofs/pkg/block/journal/journaltest"
	"github.com/cubbit/dittofs/pkg/bootstrap"
	"github.com/cubbit/dittofs/pkg/keyprovider"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/metadata/kv/badgerkv"
	"github.com/cubbit/dittofs/pkg/metadata/kvstore"
	"github.com/cubbit/dittofs/pkg/metrics"
	"github.com/cubbit/dittofs/pkg/sim"
)

// start is when every harness's clock starts.
var start = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

var nodeID = entity.NodeID{'n', 'o', 'd', 'e'}

// journalOptions is what the harness opens its journals with: a suitable
// mount and two append streams. At the default sixteen, a journal makes a
// content and a headroom spare per stream at the 256 MiB cap, 8.25 GiB per
// open, which tests running in parallel would multiply past a CI disk; two
// streams make five. A test's own WithSettings, appended after, replaces it.
// A fresh slice each call, so a test appending to one aliases no other.
func journalOptions() []journal.Option {
	return []journal.Option{journaltest.SuitableMount(), journal.WithSettings(journal.Settings{Streams: 2})}
}

// passphrase opens the harness's storage and protocol key files.
const passphrase = "nodetest passphrase"

// Harness is one node built over its own installation.
type Harness struct {
	node    *node.Node
	newNode func(node.Config) (*node.Node, error)
	cfg     node.Config
	dir     string // Badger's directory, empty when it is in memory
	clock   *sim.TestClock
	store   *kvstore.Store
}

// Option changes how New builds the harness.
type Option func(*options)

type options struct {
	watch    bool
	inMemory bool
	newNode  func(node.Config) (*node.Node, error)
	password string
}

// AdminPassword has New give the administrator password as its first
// password, through the environment as dfs init's --admin-password-env
// does, so it need not be changed before a call.
func AdminPassword(password string) Option { return func(o *options) { o.password = password } }

// Watch has New watch the test with prof.Watch, registered before anything
// the harness opens so that its check runs after the harness closed it all.
// Watch a top-level test only, never a subtest on its own.
//
// decision: a watched test stays serial. prof.Watch checks the whole
// process's goroutines and descriptors, so a test running beside it would
// fail it with its own. Serial tests all end before a parallel one starts, so
// the watched tests keep their check and the unwatched ones run in parallel.
// Withdraw it, and run the node's tests in parallel, if the leak check moves
// to the end of the test binary.
func Watch() Option { return func(o *options) { o.watch = true } }

// InMemory keeps Badger in memory rather than on disk. Such a harness
// cannot Restart: nothing outlives the KV it closes.
func InMemory() Option { return func(o *options) { o.inMemory = true } }

// WithNew is the constructor New and Restart build the node through;
// without it they call node.New.
func WithNew(f func(node.Config) (*node.Node, error)) Option {
	return func(o *options) { o.newNode = f }
}

// New initialises an installation under t.TempDir() through install.Run, as
// dfs init does: the metadata store, one journal device, the storage key and
// administrator, its password generated and discarded unless
// AdminPassword gives one. It writes the protocol key file too, which the
// bootstrap names. It then builds the node
// over them, its API, metrics listener and adapters on loopback. Every KV the
// harness opens is closed when the test ends. A test may run it in parallel
// unless it gives AdminPassword, whose password travels through the
// environment.
func New(t testing.TB, opts ...Option) *Harness {
	t.Helper()
	o := options{newNode: node.New}
	for _, opt := range opts {
		opt(&o)
	}
	if o.watch {
		prof.Watch(t, prof.Options{})
	}
	ctx := t.Context()
	root := t.TempDir()
	h := &Harness{newNode: o.newNode, clock: sim.NewTestClock(start)}
	if !o.inMemory {
		h.dir = filepath.Join(root, "store")
	}
	dev := filepath.Join(root, "device")
	if err := os.Mkdir(dev, 0o700); err != nil {
		t.Fatalf("nodetest: device directory: %v", err)
	}
	b := bootstrap.Bootstrap{Journals: []bootstrap.Journal{{Path: dev}}}
	b.Node.ID = nodeID
	b.API.Listen = []string{"127.0.0.1:0"}
	b.Metrics.Listen = "127.0.0.1:0"
	b.Adapters.Listen = []string{"127.0.0.1"}
	b.Keys.Storage.Kind = keyprovider.KindLocal
	b.Keys.Storage.Local.File = keyFile(t, root, "storage.key")
	b.Keys.Protocol.Kind = keyprovider.KindLocal
	b.Keys.Protocol.Local.File = keyFile(t, root, "protocol.key")
	h.cfg = node.Config{
		Bootstrap: b,
		Clock:     h.clock,
		Logger:    slog.New(failOnBudget{slog.NewTextHandler(t.Output(), nil), t}),
		Journal:   journalOptions(),
	}
	h.openKV(t)
	var env string
	if o.password != "" {
		env = "NODETEST_ADMIN_PASSWORD"
		t.Setenv(env, o.password)
	}
	err := install.Run(ctx, install.Deps{
		PasswordEnv: env,
		Bootstrap:   &h.cfg.Bootstrap,
		KV:          h.cfg.KV,
		StorageKey:  storageKey(t, b.Keys.Storage.Local.File),
		Clock:       h.clock,
		Journal:     journalOptions(),
		Out:         io.Discard,
		Logger:      h.cfg.Logger,
	})
	if err != nil {
		t.Fatalf("nodetest: install: %v", err)
	}
	h.openStore(t)
	h.build(t)
	return h
}

// generated holds the storage and protocol key files every harness writes,
// by name, generated once for the test binary: each costs a key derivation,
// slow under the race detector, and no test needs two harnesses' keys to
// differ. They are weak, so each open the node makes costs little too.
var generated = sync.OnceValues(func() (map[string][]byte, error) {
	keys := map[string][]byte{}
	for _, name := range []string{"storage.key", "protocol.key"} {
		b, err := keyprovider.GenerateWeakKeyFile(passphrase)
		if err != nil {
			return nil, err
		}
		keys[name] = b
	}
	return keys, nil
})

// opened is the storage key every harness's installation is sealed under,
// opened once from the first harness's storage key file. It holds no
// goroutine or descriptor, so it is never closed.
var opened struct {
	once sync.Once
	kp   keyprovider.KeyProvider
	err  error
}

// storageKey opens the storage key file at path, or returns the one opened
// already, for install.Run to seal the installation under: every harness's
// storage key file holds the same key.
func storageKey(t testing.TB, path string) keyprovider.KeyProvider {
	t.Helper()
	opened.once.Do(func() {
		opened.kp, opened.err = keyprovider.NewProvider(context.Background(), keyprovider.Config{Kind: keyprovider.KindLocal, File: path})
	})
	if opened.err != nil {
		t.Fatalf("nodetest: open the storage key: %v", opened.err)
	}
	return opened.kp
}

// keyFile writes the generated key file name under dir, sealed with the
// passphrase it sets in the environment, and returns its path.
//
// decision: the passphrase is set with os.Setenv and never restored, rather
// than with t.Setenv, which panics in a parallel test. Every harness sets the
// same value, so tests running at once agree on it. It is set on every call,
// not once, because a serial test's t.Setenv of the same variable restores
// what it found, which may be no value. Withdraw this if the key provider
// takes the passphrase other than from the environment.
func keyFile(t testing.TB, dir, name string) string {
	t.Helper()
	if err := os.Setenv("DITTOFS_ENCRYPTION_PASSPHRASE", passphrase); err != nil {
		t.Fatalf("nodetest: set the key files' passphrase: %v", err)
	}
	keys, err := generated()
	if err != nil {
		t.Fatalf("nodetest: generate the key files: %v", err)
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, keys[name], 0o600); err != nil {
		t.Fatalf("nodetest: write %s: %v", name, err)
	}
	return path
}

// Restart stops the node and builds a new one over a new KV on the same
// directories, with a new metrics registry.
func (h *Harness) Restart(t testing.TB) {
	t.Helper()
	if h.dir == "" {
		t.Fatal("nodetest: Restart of an in-memory harness: its KV keeps nothing once closed, so there is no installation to reopen")
	}
	if err := h.node.Stop(t.Context()); err != nil {
		t.Fatalf("nodetest: stop before the restart: %v", err)
	}
	h.openKV(t)
	h.openStore(t)
	h.build(t)
}

// Node is the node built last: after Restart, the new one.
func (h *Harness) Node() *node.Node { return h.node }

// Clock is the node's clock, which only the test advances.
func (h *Harness) Clock() *sim.TestClock { return h.clock }

// Metrics is the registry the node built last registers its series in.
func (h *Harness) Metrics() *metrics.Registry { return h.cfg.Metrics }

// NodeID is the node's identity, which its records are kept under.
func (h *Harness) NodeID() entity.NodeID { return nodeID }

// StoreDir is Badger's directory, empty for an in-memory harness: what a
// bootstrap names for a command run over the harness's installation, as dfs
// reset-admin is.
func (h *Harness) StoreDir() string { return h.dir }

// StorageKeyFile is the storage key file the harness's bootstrap names,
// opened with the passphrase New sets.
func (h *Harness) StorageKeyFile() string { return h.cfg.Bootstrap.Keys.Storage.Local.File }

// ProtocolKeyFile is the protocol key file the harness's bootstrap names,
// opened with the passphrase New sets.
func (h *Harness) ProtocolKeyFile() string { return h.cfg.Bootstrap.Keys.Protocol.Local.File }

// Store is a metadata store over the node's KV, for a test to write records
// and read them back, as another node's would.
func (h *Harness) Store() *kvstore.Store { return h.store }

// openKV opens Badger as the next node's KV and closes it when the test
// ends, so a New or Restart failing after the open leaves nothing open.
// Close is idempotent, so a KV its node already closed closes again
// harmlessly.
func (h *Harness) openKV(t testing.TB) {
	t.Helper()
	db, err := badgerkv.Open(badgerkv.Options{Dir: h.dir, NodeID: nodeID, Clock: h.clock.Now})
	if err != nil {
		t.Fatalf("nodetest: open the KV: %v", err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("nodetest: close the KV: %v", err)
		}
	})
	h.cfg.KV = db
}

func (h *Harness) openStore(t testing.TB) {
	t.Helper()
	st, err := kvstore.Open(t.Context(), h.cfg.KV)
	if err != nil {
		t.Fatalf("nodetest: open the store: %v", err)
	}
	h.store = st
}

// build builds a node over the harness's KV, which the node then owns.
func (h *Harness) build(t testing.TB) {
	t.Helper()
	h.cfg.Metrics = metrics.New("test", "test")
	n, err := h.newNode(h.cfg)
	if err != nil {
		t.Fatalf("nodetest: build the node: %v", err)
	}
	h.node = n
}

// budgetNotZero is the record a node logs at its stop for each budget not at
// zero (RFC 24 §2.7).
const budgetNotZero = "node: budget not at zero after the adapters and the filesystem stopped"

// failOnBudget hands every record on, and fails t on a budget not at zero.
// The node logs that record on the logger it is handed, never on one derived
// with With, so only Handle is wrapped.
type failOnBudget struct {
	slog.Handler
	t testing.TB
}

func (h failOnBudget) Handle(ctx context.Context, r slog.Record) error {
	if r.Message == budgetNotZero {
		var attrs []string
		r.Attrs(func(a slog.Attr) bool {
			attrs = append(attrs, a.String())
			return true
		})
		h.t.Errorf("nodetest: %s: %s", r.Message, strings.Join(attrs, " "))
	}
	return h.Handler.Handle(ctx, r)
}

// Preflight runs the node's start check with a mount that suits the host, so
// a test of what the check reads does not depend on the mount of the machine
// it runs on. The tests of dfs start call it here because only start.go may
// import the node in that package.
func Preflight(ctx context.Context, storeDir, v1Root string, devices []string, clock engine.Clock) error {
	return node.Preflight(ctx, storeDir, v1Root, devices, clock, journaltest.SuitableMount())
}
