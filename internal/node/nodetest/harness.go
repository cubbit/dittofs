// Package nodetest builds a node in process for a test: an installation
// initialised under t.TempDir(), over Badger on disk or in memory, on a test
// clock.
package nodetest

import (
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/cubbit/dittofs/internal/node"
	"github.com/cubbit/dittofs/internal/prof"
	"github.com/cubbit/dittofs/pkg/block/journal"
	"github.com/cubbit/dittofs/pkg/block/journal/journaltest"
	"github.com/cubbit/dittofs/pkg/bootstrap"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/metadata/kv/badgerkv"
	"github.com/cubbit/dittofs/pkg/metadata/kvstore"
	"github.com/cubbit/dittofs/pkg/metrics"
	"github.com/cubbit/dittofs/pkg/sim"
)

// start is when every harness's clock starts.
var start = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

var (
	install = [16]byte{'n', 'o', 'd', 'e', 't', 'e', 's', 't'}
	nodeID  = entity.NodeID{'n', 'o', 'd', 'e'}
)

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

// New initialises an installation, the metadata store and one journal under
// t.TempDir(), records the journal for the node's one device, and builds a
// node over them. Every KV the harness opens is closed when the test ends.
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
	jid, err := journal.Init(ctx, dev, install, journal.WithClock(h.clock), journaltest.SuitableMount())
	if err != nil {
		t.Fatalf("nodetest: journal init: %v", err)
	}

	b := bootstrap.Bootstrap{Journals: []bootstrap.Journal{{Path: dev}}}
	b.Node.ID = nodeID
	b.API.Listen = []string{"127.0.0.1:0"}
	b.Metrics.Listen = "127.0.0.1:0"
	h.cfg = node.Config{
		Bootstrap: b,
		Clock:     h.clock,
		Logger:    slog.New(slog.NewTextHandler(t.Output(), nil)),
		Journal:   []journal.Option{journaltest.SuitableMount()},
	}
	h.openKV(t)
	if err := kvstore.Init(ctx, h.cfg.KV, install); err != nil {
		t.Fatalf("nodetest: store init: %v", err)
	}
	h.openStore(t)
	if err := h.store.RecordJournal(ctx, jid, nodeID, entity.JournalDir(dev)); err != nil {
		t.Fatalf("nodetest: record the journal: %v", err)
	}
	h.build(t)
	return h
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
