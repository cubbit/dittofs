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
	"testing"
	"time"

	"github.com/cubbit/dittofs/internal/install"
	"github.com/cubbit/dittofs/internal/node"
	"github.com/cubbit/dittofs/internal/prof"
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

// passphrase opens the harness's storage key file.
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
}

// Watch has New watch the test with prof.Watch, registered before anything
// the harness opens so that its check runs after the harness closed it all.
// Watch a top-level test only, never a subtest on its own.
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
// dfs init does: the metadata store, one journal device and the
// administrator, its password generated and discarded. It then builds a node
// over them, its API, metrics listener and adapters on loopback. Every KV the
// harness opens is closed when the test ends. It sets the key file's
// passphrase in the environment, so a test using it cannot run in parallel.
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
	h.cfg = node.Config{
		Bootstrap: b,
		Clock:     h.clock,
		Logger:    slog.New(failOnBudget{slog.NewTextHandler(t.Output(), nil), t}),
		Journal:   []journal.Option{journaltest.SuitableMount()},
	}
	h.openKV(t)
	err := install.Run(ctx, install.Deps{
		Bootstrap:  &h.cfg.Bootstrap,
		KV:         h.cfg.KV,
		StorageKey: storageKey(t, root),
		Clock:      h.clock,
		Journal:    []journal.Option{journaltest.SuitableMount()},
		Out:        io.Discard,
		Logger:     h.cfg.Logger,
	})
	if err != nil {
		t.Fatalf("nodetest: install: %v", err)
	}
	h.openStore(t)
	h.build(t)
	return h
}

// storageKey writes a storage key file under dir and opens it, closed when
// the test ends.
func storageKey(t testing.TB, dir string) keyprovider.KeyProvider {
	t.Helper()
	t.Setenv("DITTOFS_ENCRYPTION_PASSPHRASE", passphrase)
	b, err := keyprovider.GenerateKeyFile(passphrase)
	if err != nil {
		t.Fatalf("nodetest: generate the storage key: %v", err)
	}
	path := filepath.Join(dir, "storage.key")
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatalf("nodetest: write the storage key: %v", err)
	}
	kp, err := keyprovider.NewProvider(t.Context(), keyprovider.Config{Kind: keyprovider.KindLocal, File: path})
	if err != nil {
		t.Fatalf("nodetest: open the storage key: %v", err)
	}
	t.Cleanup(func() { _ = kp.Close() })
	return kp
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
