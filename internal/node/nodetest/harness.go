// Package nodetest builds a node in process for a test: an installation
// initialised under t.TempDir(), over Badger on disk, on a test clock.
package nodetest

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/cubbit/dittofs/internal/node"
	"github.com/cubbit/dittofs/internal/prof"
	"github.com/cubbit/dittofs/pkg/block/journal"
	"github.com/cubbit/dittofs/pkg/block/journal/journaltest"
	"github.com/cubbit/dittofs/pkg/config"
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

// Harness is one node built over its own installation. Its embedded Node
// is the node built last; Restart replaces it.
type Harness struct {
	*node.Node
	cfg     node.Config
	dir     string // Badger's directory
	clock   *sim.TestClock
	store   *kvstore.Store
	stopped bool
}

// Option changes how New builds the harness.
type Option func(*options)

type options struct{ watch bool }

// Watch has New watch the test with prof.Watch, registered before anything
// the harness opens so that its check runs after the harness closed it all.
// Watch a top-level test only, never a subtest on its own.
func Watch() Option { return func(o *options) { o.watch = true } }

// New initialises an installation, the metadata store and one journal under
// t.TempDir(), records the journal for the node's one device, and builds a
// node over them. The node is stopped when the test ends.
func New(t testing.TB, opts ...Option) *Harness {
	t.Helper()
	var o options
	for _, opt := range opts {
		opt(&o)
	}
	if o.watch {
		prof.Watch(t, prof.Options{})
	}
	ctx := t.Context()
	root := t.TempDir()
	h := &Harness{dir: filepath.Join(root, "store"), clock: sim.NewTestClock(start)}
	dev := filepath.Join(root, "device")
	if err := os.Mkdir(dev, 0o700); err != nil {
		t.Fatalf("nodetest: device directory: %v", err)
	}
	jid, err := journal.Init(ctx, dev, install, journal.WithClock(h.clock), journaltest.SuitableMount())
	if err != nil {
		t.Fatalf("nodetest: journal init: %v", err)
	}
	h.cfg = node.Config{
		Bootstrap: config.Bootstrap{Node: nodeID, Devices: []string{dev}},
		Clock:     h.clock,
		Logger:    slog.New(slog.NewTextHandler(t.Output(), nil)),
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
	t.Cleanup(func() {
		if err := h.Stop(context.Background()); err != nil {
			t.Errorf("nodetest: stop at the test's end: %v", err)
		}
	})
	return h
}

// Restart stops the node and builds a new one over a new KV on the same
// directories, with a new metrics registry.
func (h *Harness) Restart(t testing.TB) {
	t.Helper()
	if err := h.Stop(t.Context()); err != nil {
		t.Fatalf("nodetest: stop before the restart: %v", err)
	}
	h.openKV(t)
	h.openStore(t)
	h.build(t)
}

// Stop stops the node once; a later Stop, the test's end's included, does
// nothing.
func (h *Harness) Stop(ctx context.Context) error {
	if h.stopped {
		return nil
	}
	h.stopped = true
	return h.Node.Stop(ctx)
}

// Clock is the node's clock, which only the test advances.
func (h *Harness) Clock() *sim.TestClock { return h.clock }

// Store is a metadata store over the node's KV, for a test to write records
// and read them back, as another node's would.
func (h *Harness) Store() *kvstore.Store { return h.store }

// openKV opens Badger in the harness's directory as the next node's KV.
func (h *Harness) openKV(t testing.TB) {
	t.Helper()
	db, err := badgerkv.Open(badgerkv.Options{Dir: h.dir, NodeID: nodeID, Clock: h.clock.Now})
	if err != nil {
		t.Fatalf("nodetest: open the KV: %v", err)
	}
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
	n, err := node.New(h.cfg)
	if err != nil {
		t.Fatalf("nodetest: node.New: %v", err)
	}
	h.Node, h.stopped = n, false
}
