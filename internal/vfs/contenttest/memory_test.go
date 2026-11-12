package contenttest_test

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cubbit/dittofs/internal/vfs"
	"github.com/cubbit/dittofs/internal/vfs/contenttest"
	"github.com/cubbit/dittofs/pkg/block/engine"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/metadata/kv"
	"github.com/cubbit/dittofs/pkg/metadata/kv/badgerkv"
	"github.com/cubbit/dittofs/pkg/metadata/kvstore"
	"github.com/cubbit/dittofs/pkg/metadata/kvstore/kvstoretest"
	"github.com/cubbit/dittofs/pkg/sim"
)

// The two declarations of Content hold the same methods.
var (
	_ vfs.Content         = contenttest.Content(nil)
	_ contenttest.Content = vfs.Content(nil)
)

// The suite holds over the in-memory Content.
func TestMemory(t *testing.T) {
	contenttest.Run(t, func(t *testing.T) contenttest.Subject {
		clock := sim.NewTestClock(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
		m := contenttest.New(clock)
		share, other := entity.NewShareID(), entity.NewShareID()
		m.Bind(share)
		m.Bind(other)
		return contenttest.Subject{
			Content: m,
			Share:   share,
			Other:   other,
			File:    func(*testing.T) entity.FileID { return entity.NewFileID() },
			Clock:   clock,
			Lose:    func(_ *testing.T, f entity.FileID) { m.Lose(share, f) },
		}
	})
}

// Existence commits running at once over a store, as two chowns of one file
// may, commit each staged write once: its Done is called once, and the
// store's record holds its size.
func TestMemoryOverAStoreCommitsEachWriteOnce(t *testing.T) {
	fs := kvstoretest.NewFS(t, func(tb testing.TB) kv.KV {
		db, err := badgerkv.Open(badgerkv.Options{})
		if err != nil {
			tb.Fatal(err)
		}
		tb.Cleanup(func() { _ = db.Close() })
		return db
	})
	m := contenttest.NewOver(sim.NewTestClock(time.Unix(0, 0)), fs.Store)
	m.Bind(fs.Share.ID)
	h, f := fs.Create(t, fs.Admin, fs.Root, "f", kvstore.Regular(0o644))
	var dones atomic.Int32
	const writes = 8
	for i := range writes {
		if _, err := m.Write(t.Context(), f.Share, f.ID, int64(i), []byte{'x'}, engine.WriteOpts{Done: func() { dones.Add(1) }}); err != nil {
			t.Fatalf("Write %d: %v", i, err)
		}
	}
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			if err := m.CommitExistence(t.Context(), f.Share, f.ID); err != nil {
				t.Errorf("CommitExistence: %v", err)
			}
		})
	}
	wg.Wait()
	if got := dones.Load(); got != writes {
		t.Errorf("Dones called %d times, want %d", got, writes)
	}
	if rec, err := fs.Store.Files().Get(t.Context(), fs.Admin, h); err != nil || rec.Size != writes {
		t.Errorf("the store's record: size %d, %v; want %d", rec.Size, err, writes)
	}
}
