package contenttest_test

import (
	"errors"
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
	pvfs "github.com/cubbit/dittofs/pkg/vfs"
	"github.com/cubbit/dittofs/pkg/vfs/vfserr"
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

// The suite holds over the in-memory Content over a store, each file of it
// created there and unlinked, so a release finds no name left.
func TestMemoryOverAStore(t *testing.T) {
	contenttest.Run(t, func(t *testing.T) contenttest.Subject {
		clock := sim.NewTestClock(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
		fs := kvstoretest.NewFS(t, memKV)
		m := contenttest.NewOver(clock, fs.Store)
		other := entity.NewShareID()
		m.Bind(fs.Share.ID)
		m.Bind(other)
		return contenttest.Subject{
			Content: m,
			Share:   fs.Share.ID,
			Other:   other,
			File: func(t *testing.T) entity.FileID {
				_, f := fs.Create(t, fs.Admin, fs.Root, "f", kvstore.Regular(0o644))
				if _, err := fs.Store.Namespace().Unlink(t.Context(), fs.Admin, fs.Root, []byte("f")); err != nil {
					t.Fatalf("Unlink: %v", err)
				}
				return f.ID
			},
			Clock: clock,
			Lose:  func(_ *testing.T, f entity.FileID) { m.Lose(fs.Share.ID, f) },
		}
	})
}

// Existence commits running at once over a store, as two chowns of one file
// may, commit each staged write once: its Done is called once, and the
// store's record holds its size.
func TestMemoryOverAStoreCommitsEachWriteOnce(t *testing.T) {
	fs := kvstoretest.NewFS(t, memKV)
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

// A Done runs once the commit that called it has let go of every lock, so a
// Done that commits another file returns, as the engine's do after its guard.
func TestMemoryCallsDonesOutsideItsLocks(t *testing.T) {
	for _, c := range []struct {
		name   string
		commit func(m *contenttest.Memory, s entity.ShareID, f entity.FileID) error
	}{
		{"CommitExistence", func(m *contenttest.Memory, s entity.ShareID, f entity.FileID) error {
			return m.CommitExistence(t.Context(), s, f)
		}},
		{"Delete", func(m *contenttest.Memory, s entity.ShareID, f entity.FileID) error {
			_, err := m.Delete(t.Context(), s, f)
			return err
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			m := contenttest.New(sim.NewTestClock(time.Unix(0, 0)))
			s, a, b := entity.NewShareID(), entity.NewFileID(), entity.NewFileID()
			m.Bind(s)
			var inner error
			done := func() { inner = m.CommitExistence(t.Context(), s, b) }
			if _, err := m.Write(t.Context(), s, a, 0, []byte("x"), engine.WriteOpts{Done: done}); err != nil {
				t.Fatalf("Write: %v", err)
			}
			returned := make(chan error, 1)
			go func() { returned <- c.commit(m, s, a) }()
			// No clock governs a lock wait, so this bounded wait is the assertion.
			select {
			case err := <-returned:
				if err != nil || inner != nil {
					t.Errorf("%s: %v, its Done's CommitExistence: %v; want nil, nil", c.name, err, inner)
				}
			case <-time.After(10 * time.Second):
				t.Fatalf("%s: a Done committing another file has not returned in 10s, want it to return", c.name)
			}
		})
	}
}

// A write staged while an existence commit is in the store stays pending,
// its Done uncalled, until the next commit takes it to the store.
func TestMemoryOverAStoreKeepsAWriteStagedDuringACommit(t *testing.T) {
	fs, held := kvstoretest.NewHeldFS(t, memKV)
	m := contenttest.NewOver(sim.NewTestClock(time.Unix(0, 0)), fs.Store)
	m.Bind(fs.Share.ID)
	h, f := fs.Create(t, fs.Admin, fs.Root, "f", kvstore.Regular(0o644))
	var first, second atomic.Int32
	write := func(off int64, dones *atomic.Int32) error {
		_, err := m.Write(t.Context(), f.Share, f.ID, off, []byte{'x'}, engine.WriteOpts{Done: func() { dones.Add(1) }})
		return err
	}
	if err := write(0, &first); err != nil {
		t.Fatalf("Write: %v", err)
	}
	commitErr, writeErr := held.Race(func() error { return m.CommitExistence(t.Context(), f.Share, f.ID) }, func() error { return write(1, &second) })
	if commitErr != nil || writeErr != nil {
		t.Fatalf("CommitExistence raced by a Write: %v, %v; want nil, nil", commitErr, writeErr)
	}
	if first.Load() != 1 || second.Load() != 0 {
		t.Errorf("after the commit: Dones called %d and %d times, want 1 for the committed write, 0 for the one staged meanwhile", first.Load(), second.Load())
	}
	if _, pending, err := m.Overlay(t.Context(), f.Share, f.ID); err != nil || !pending {
		t.Errorf("Overlay after the commit: pending %v, %v; want the write staged meanwhile pending", pending, err)
	}
	if err := m.CommitExistence(t.Context(), f.Share, f.ID); err != nil {
		t.Fatalf("the second CommitExistence: %v", err)
	}
	if second.Load() != 1 {
		t.Errorf("after the second commit: the write staged meanwhile had its Done called %d times, want 1", second.Load())
	}
	if rec, err := fs.Store.Files().Get(t.Context(), fs.Admin, h); err != nil || rec.Size != 2 {
		t.Errorf("the store's record: size %d, %v; want 2", rec.Size, err)
	}
}

// A write staged while a release is in the store is released with it: its
// Done is called, as is that of the write the release committed.
func TestMemoryOverAStoreReleasesAWriteStagedDuringARelease(t *testing.T) {
	fs, held := kvstoretest.NewHeldFS(t, memKV)
	m := contenttest.NewOver(sim.NewTestClock(time.Unix(0, 0)), fs.Store)
	m.Bind(fs.Share.ID)
	_, f := fs.Create(t, fs.Admin, fs.Root, "f", kvstore.Regular(0o644))
	if _, err := fs.Store.Namespace().Unlink(t.Context(), fs.Admin, fs.Root, []byte("f")); err != nil {
		t.Fatalf("Unlink: %v", err)
	}
	var dones atomic.Int32
	write := func(off int64) error {
		_, err := m.Write(t.Context(), f.Share, f.ID, off, []byte{'x'}, engine.WriteOpts{Done: func() { dones.Add(1) }})
		return err
	}
	if err := write(0); err != nil {
		t.Fatalf("Write: %v", err)
	}
	deleteErr, writeErr := held.Race(func() error { _, err := m.Delete(t.Context(), f.Share, f.ID); return err }, func() error { return write(1) })
	if deleteErr != nil || writeErr != nil {
		t.Fatalf("Delete raced by a Write: %v, %v; want nil, nil", deleteErr, writeErr)
	}
	if got := dones.Load(); got != 2 {
		t.Errorf("after the release: Dones called %d times, want 2", got)
	}
}

// A release of a file the store records a durable open of is aborted as
// vfserr.ErrFileOpen, the one error the filesystem's release clears its
// releasing mark for, rather than as the store's ErrHeld.
func TestMemoryOverAStoreAnswersAHeldReleaseFileOpen(t *testing.T) {
	fs := kvstoretest.NewFS(t, memKV)
	m := contenttest.NewOver(sim.NewTestClock(time.Unix(0, 0)), fs.Store)
	m.Bind(fs.Share.ID)
	_, f := fs.Create(t, fs.Admin, fs.Root, "f", kvstore.Regular(0o644))
	id, err := fs.Store.NextID(t.Context(), kvstore.IDOpen)
	if err != nil {
		t.Fatalf("NextID: %v", err)
	}
	open := entity.OpenRecord{ID: entity.OpenID(id), File: f.ID, Client: 1, Principal: fs.Admin.Principal, Access: 0x3}
	// The unlink of the last name writes the open durable: it holds the file.
	if _, err := fs.Store.Namespace().Unlink(t.Context(), fs.Admin, fs.Root, []byte("f"), open); err != nil {
		t.Fatalf("Unlink: %v", err)
	}
	if _, err := m.Delete(t.Context(), f.Share, f.ID); !errors.Is(err, vfserr.ErrFileOpen) {
		t.Errorf("Delete of a file held by a durable open: %v, want vfserr.ErrFileOpen", err)
	}
}

// A clone of nothing but hole within the destination stages no write, yet
// moves the destination's modification time, as any clone does.
func TestMemoryCloneOfAHoleMovesTheTimes(t *testing.T) {
	clock := sim.NewTestClock(time.Unix(0, 0))
	m := contenttest.New(clock)
	s, src, dst := entity.NewShareID(), entity.NewFileID(), entity.NewFileID()
	m.Bind(s)
	if err := m.Truncate(t.Context(), s, src, 4); err != nil {
		t.Fatalf("Truncate of the source: %v", err)
	}
	if _, err := m.Write(t.Context(), s, dst, 0, []byte("01234567"), engine.WriteOpts{}); err != nil {
		t.Fatalf("Write: %v", err)
	}
	clock.Advance(time.Second)
	if err := m.Clone(t.Context(), s, src, s, dst, pvfs.Extent{SrcOff: 0, DstOff: 2, Len: 4}); err != nil {
		t.Fatalf("Clone: %v", err)
	}
	if o, _, err := m.Overlay(t.Context(), s, dst); err != nil || !o.Mtime.Equal(clock.Now()) || o.Size != 8 {
		t.Errorf("Overlay after the clone: mtime %v, size %d, %v; want %v, 8", o.Mtime, o.Size, err, clock.Now())
	}
}

func memKV(tb testing.TB) kv.KV {
	db, err := badgerkv.Open(badgerkv.Options{})
	if err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(func() { _ = db.Close() })
	return db
}
