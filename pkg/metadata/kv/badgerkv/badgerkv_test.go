package badgerkv

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/metadata/kv"
	"github.com/cubbit/dittofs/pkg/metadata/kv/internal/backend"
	"github.com/cubbit/dittofs/pkg/metadata/kv/kvtest"
)

func open(t *testing.T, opts Options) *Store {
	t.Helper()
	s, err := Open(opts)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestConformance(t *testing.T) {
	kvtest.Run(t, func(t *testing.T) kv.KV {
		s := open(t, Options{})
		t.Cleanup(func() {
			if err := s.Close(); err != nil {
				t.Error(err)
			}
		})
		return s
	})
}

// The suite on disk, with every commit synced: the backend as it ships.
func TestConformanceOnDisk(t *testing.T) {
	if testing.Short() {
		t.Skip("syncs every commit")
	}
	kvtest.Run(t, func(t *testing.T) kv.KV {
		s := open(t, Options{Dir: t.TempDir()})
		t.Cleanup(func() {
			if err := s.Close(); err != nil {
				t.Error(err)
			}
		})
		return s
	})
}

// A returned Update reads back after a reopen. A process restart does not
// lose the page cache, so this proves the commit reached the store, not the
// device; a power-loss test needs a harness that drops the cache.
func TestCommitSurvivesReopen(t *testing.T) {
	dir := t.TempDir()
	s := open(t, Options{Dir: dir})
	if err := s.Update(context.Background(), func(tx kv.Txn) error {
		return tx.Set([]byte("k"), []byte("v"))
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s = open(t, Options{Dir: dir})
	defer func() {
		if err := s.Close(); err != nil {
			t.Error(err)
		}
	}()
	if err := s.View(context.Background(), func(r kv.Reader) error {
		v, err := r.Get([]byte("k"))
		if err == nil && string(v) != "v" {
			t.Errorf("got %q", v)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) set(t time.Time) {
	c.mu.Lock()
	c.t = t
	c.mu.Unlock()
}

func txnNow(t *testing.T, s *Store) time.Time {
	t.Helper()
	var now time.Time
	if err := s.Update(context.Background(), func(tx kv.Txn) error {
		now = tx.Now()
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return now
}

func TestNowAcrossRestartWithClockSteppedBack(t *testing.T) {
	dir := t.TempDir()
	start := time.Unix(1_800_000_000, 0)
	clock := &fakeClock{t: start}
	s := open(t, Options{Dir: dir, Clock: clock.now})
	before := txnNow(t, s)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	clock.set(start.Add(-time.Hour))
	s = open(t, Options{Dir: dir, Clock: clock.now})
	defer func() {
		if err := s.Close(); err != nil {
			t.Error(err)
		}
	}()
	if after := txnNow(t, s); after.Before(before) {
		t.Fatalf("Now went from %v to %v across a restart", before, after)
	}
}

func TestNowStaysUnderCeiling(t *testing.T) {
	start := time.Unix(1_800_000_000, 0)
	clock := &fakeClock{t: start}
	s := open(t, Options{Clock: clock.now})
	defer func() {
		if err := s.Close(); err != nil {
			t.Error(err)
		}
	}()

	// The clock jumps past the ceiling before the next raise: Now holds at
	// the ceiling rather than pass it.
	clock.set(start.Add(time.Hour))
	if now, limit := txnNow(t, s), start.Add(backend.CeilingAhead); now.After(limit) {
		t.Fatalf("Now %v passed the ceiling %v", now, limit)
	}
	// Once raised, Now follows the clock again.
	if err := s.raiseCeiling(); err != nil {
		t.Fatal(err)
	}
	if now := txnNow(t, s); !now.Equal(start.Add(time.Hour)) {
		t.Fatalf("Now %v after the raise, want the clock's %v", now, start.Add(time.Hour))
	}
}

// Concurrent commits share syncs: fewer syncs run than commits.
func TestConcurrentCommitsShareSyncs(t *testing.T) {
	s := open(t, Options{Dir: t.TempDir()})
	defer func() {
		if err := s.Close(); err != nil {
			t.Error(err)
		}
	}()
	before := s.sync.syncs.Load()
	const writers = 32
	var wg sync.WaitGroup
	errs := make(chan error, writers)
	for w := range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- s.Update(context.Background(), func(tx kv.Txn) error {
				return tx.Set([]byte{byte(w)}, []byte("v"))
			})
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if n := s.sync.syncs.Load() - before; n >= writers {
		t.Errorf("%d syncs for %d concurrent commits: they did not share syncs", n, writers)
	}
}

// A seek below a key reads only the versions under its own prefix: n keys
// set and then deleted below it, as the keys of n released files lie below
// the next file's, cost it nothing, and a seek over those n keys reads their
// two versions each and stops at the prefix's edge, never reaching the live
// key below it.
func TestBeforeStopsAtItsPrefix(t *testing.T) {
	for _, n := range []int{1, 1000} {
		s := open(t, Options{})
		t.Cleanup(func() {
			if err := s.Close(); err != nil {
				t.Error(err)
			}
		})

		var keys [][]byte
		for i := range n {
			keys = append(keys, fmt.Appendf(nil, "b/%04d", i))
		}

		for _, op := range []func(kv.Txn, []byte) error{
			func(tx kv.Txn, k []byte) error {
				return tx.Set(k, []byte("v"))
			},
			func(tx kv.Txn, k []byte) error {
				return tx.Delete(k)
			},
		} {
			if err := s.Update(context.Background(), func(tx kv.Txn) error {
				for _, k := range keys {
					if err := op(tx, k); err != nil {
						return err
					}
				}

				return tx.Set([]byte("a"), []byte("live"))
			}); err != nil {
				t.Fatal(err)
			}
		}

		snap := s.db.NewTransaction(false)
		for _, c := range []struct {
			prefix, key string
			visited     int
		}{
			{"c/", "c/9", 0},
			{"b/", "b/~", 2 * n},
		} {
			b := newBelow(snap, []byte(c.prefix), []byte(c.key))
			if b.Valid() || b.visited != c.visited {
				t.Errorf(
					"n=%d: below %q under %q: found %v, visited %d versions, want none and %d",
					n,
					c.key,
					c.prefix,
					b.Valid(),
					b.visited,
					c.visited,
				)
			}

			b.it.Close()
		}

		snap.Discard()
	}
}
