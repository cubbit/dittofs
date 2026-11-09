//go:build integration

package runtime

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/controlplane/models"
	"github.com/cubbit/dittofs/pkg/controlplane/runtime/shares"
	"github.com/cubbit/dittofs/pkg/controlplane/store"
	"github.com/cubbit/dittofs/pkg/health"
	"github.com/cubbit/dittofs/pkg/metadata"
	"github.com/google/uuid"

	"github.com/cubbit/dittofs/pkg/metadata/store/badger/badgertest"
)

// newRuntimeForChecks returns a Runtime wired to an in-memory SQLite
// control-plane store. Tests in this file rely on a real store for
// the block store checker path; the other paths don't touch it.
func newRuntimeForChecks(t *testing.T) (*Runtime, store.Store) {
	t.Helper()
	cfg := store.Config{
		Type:   "sqlite",
		SQLite: store.SQLiteConfig{Path: ":memory:"},
	}
	cpStore, err := store.New(&cfg)
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	rt := New(cpStore)
	setJournalRoot(t, rt)
	return rt, cpStore
}

// countingMetaStore wraps the real in-memory metadata store and
// counts calls to Healthcheck. It embeds the real store to inherit
// every other method of the large MetadataStore interface.
type countingMetaStore struct {
	metadata.Store
	calls int64
}

func newCountingMetaStore(t testing.TB) *countingMetaStore {
	return &countingMetaStore{
		Store: badgertest.NewInMemory(t),
	}
}

func (c *countingMetaStore) Healthcheck(ctx context.Context) health.Report {
	atomic.AddInt64(&c.calls, 1)
	return c.Store.Healthcheck(ctx)
}

func (c *countingMetaStore) Count() int64 { return atomic.LoadInt64(&c.calls) }

func TestStatusCheckers_MetadataStore_CachedAcrossCalls(t *testing.T) {
	rt, _ := newRuntimeForChecks(t)
	rt.statusCheckers = newCheckerCache(5 * time.Second)

	meta := newCountingMetaStore(t)
	if err := rt.RegisterMetadataStore("m1", meta); err != nil {
		t.Fatalf("RegisterMetadataStore: %v", err)
	}

	c := rt.MetadataStoreChecker("m1")
	_ = c.Healthcheck(context.Background())
	_ = c.Healthcheck(context.Background())
	_ = c.Healthcheck(context.Background())

	if got := meta.Count(); got != 1 {
		t.Errorf("expected 1 underlying Healthcheck call, got %d", got)
	}
}

// TestStatusCheckers_TTLExpiry proves newCheckerCache passes its TTL through
// to the cached checkers: a call after the TTL window re-probes the store
// instead of returning the cached report.
func TestStatusCheckers_TTLExpiry(t *testing.T) {
	rt, _ := newRuntimeForChecks(t)
	ttl := 10 * time.Millisecond
	rt.statusCheckers = newCheckerCache(ttl)

	meta := newCountingMetaStore(t)
	if err := rt.RegisterMetadataStore("m-ttl", meta); err != nil {
		t.Fatalf("RegisterMetadataStore: %v", err)
	}

	_ = rt.MetadataStoreChecker("m-ttl").Healthcheck(context.Background())
	if got := meta.Count(); got != 1 {
		t.Fatalf("first call probe count = %d, want 1", got)
	}

	time.Sleep(ttl * 5)

	_ = rt.MetadataStoreChecker("m-ttl").Healthcheck(context.Background())
	if got := meta.Count(); got < 2 {
		t.Errorf("probe count after the TTL = %d, want at least 2 (the cache did not expire)", got)
	}
}

func TestStatusCheckers_MetadataStore_WrapperIsCached(t *testing.T) {
	rt, _ := newRuntimeForChecks(t)
	meta := newCountingMetaStore(t)
	_ = rt.RegisterMetadataStore("m1", meta)

	first := rt.MetadataStoreChecker("m1")
	second := rt.MetadataStoreChecker("m1")
	if first != second {
		t.Errorf("MetadataStoreChecker returned different wrappers for the same name")
	}
}

func TestStatusCheckers_MetadataStore_UnknownReturnsNotLoaded(t *testing.T) {
	rt, _ := newRuntimeForChecks(t)
	c := rt.MetadataStoreChecker("nope")
	rep := c.Healthcheck(context.Background())
	if rep.Status != health.StatusUnknown {
		t.Errorf("status = %s, want unknown", rep.Status)
	}
	if !strings.Contains(rep.Message, "not loaded") {
		t.Errorf("message = %q, want it to mention 'not loaded'", rep.Message)
	}
}

func TestStatusCheckers_ShareChecker_CachesWorstOfProbe(t *testing.T) {
	rt, _ := newRuntimeForChecks(t)
	rt.statusCheckers = newCheckerCache(5 * time.Second)

	meta := newCountingMetaStore(t)
	if err := rt.RegisterMetadataStore("share-meta", meta); err != nil {
		t.Fatalf("RegisterMetadataStore: %v", err)
	}

	// Inject a share directly via shares.Service's test helper so we
	// don't need to drag block store / quota setup into this test.
	rt.sharesSvc.InjectShareForTesting(&shares.Share{
		Name:          "/s",
		MetadataStore: "share-meta",
	})

	c := rt.ShareChecker("/s")
	_ = c.Healthcheck(context.Background())
	_ = c.Healthcheck(context.Background())
	_ = c.Healthcheck(context.Background())

	if got := meta.Count(); got != 1 {
		t.Errorf("expected 1 underlying Healthcheck call, got %d", got)
	}
}

func TestStatusCheckers_BlockStore_UnknownWhenStoreMissing(t *testing.T) {
	rt, _ := newRuntimeForChecks(t)
	c := rt.BlockStoreChecker("does-not-exist")
	rep := c.Healthcheck(context.Background())
	if rep.Status != health.StatusUnknown {
		t.Errorf("missing config status = %s, want unknown", rep.Status)
	}
	if !strings.Contains(rep.Message, "not found") {
		t.Errorf("message = %q, expected 'not found'", rep.Message)
	}
}

func TestStatusCheckers_BlockStore_MemoryStoreHealthy(t *testing.T) {
	rt, cpStore := newRuntimeForChecks(t)
	rt.statusCheckers = newCheckerCache(5 * time.Second)

	bs := &models.BlockStoreConfig{
		ID: uuid.New().String(), Name: "mem", Type: "memory",
		CreatedAt: time.Now(),
	}
	if _, err := cpStore.CreateBlockStore(context.Background(), bs); err != nil {
		t.Fatalf("CreateBlockStore: %v", err)
	}

	rep := rt.BlockStoreChecker("mem").Healthcheck(context.Background())
	if rep.Status != health.StatusHealthy {
		t.Errorf("memory block store status = %s, want healthy (msg=%q)", rep.Status, rep.Message)
	}

	// Second call inside TTL still returns healthy; this just locks
	// in that the cached wrapper doesn't drift on repeat access.
	rep2 := rt.BlockStoreChecker("mem").Healthcheck(context.Background())
	if rep2.Status != health.StatusHealthy {
		t.Errorf("second call status = %s, want healthy", rep2.Status)
	}
}

func TestStatusCheckers_CanceledContextDoesNotPanic(t *testing.T) {
	rt, _ := newRuntimeForChecks(t)
	meta := newCountingMetaStore(t)
	_ = rt.RegisterMetadataStore("m1", meta)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	// CachedChecker detaches the probe context, so a pre-canceled
	// caller ctx surfaces either as a completed probe (if fast) or
	// as a "canceled" unknown report (if the caller's select fires
	// first). Either way the call MUST NOT panic and MUST return a
	// populated Report.
	rep := rt.MetadataStoreChecker("m1").Healthcheck(ctx)
	if rep.Status == "" {
		t.Errorf("expected a populated status, got empty")
	}
}
