package datapath

import (
	"encoding/binary"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/journal/internal/durability"
)

var _ durability.Domains = (*fileLocks)(nil)

func fid(n uint64) core.FileID {
	var b [16]byte
	binary.BigEndian.PutUint64(b[8:], n)
	return core.NewFileID(b)
}

// refs reports how many holders and waiters id's entry counts, 0 if absent.
func (t *fileLocks) refs(id core.FileID) int {
	s := t.shardOf(id)
	s.mu.Lock()
	defer s.mu.Unlock()
	if e, ok := s.m[id]; ok {
		return e.refs
	}
	return 0
}

func TestFileLocks_SameFileSerialises(t *testing.T) {
	tbl := newFileLocks()
	id := fid(1)
	var inside, overlaps int32
	counter := 0
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				unlock := tbl.Lock(id)
				if atomic.AddInt32(&inside, 1) != 1 {
					atomic.AddInt32(&overlaps, 1)
				}
				counter++ // plain write: -race flags any overlap
				atomic.AddInt32(&inside, -1)
				unlock()
			}
		}()
	}
	wg.Wait()
	if overlaps != 0 {
		t.Fatalf("%d overlapping holders of one file's domain", overlaps)
	}
	if counter != 16*50 {
		t.Fatalf("counter = %d, want %d", counter, 16*50)
	}
}

func TestFileLocks_DifferentFilesRunTogether(t *testing.T) {
	tbl := newFileLocks()
	u1 := tbl.Lock(fid(1))
	done := make(chan func())
	go func() { done <- tbl.Lock(fid(2)) }()
	select {
	case u2 := <-done:
		u2()
	case <-time.After(5 * time.Second):
		t.Fatal("a second file's domain waited on the first")
	}
	u1()
}

func TestFileLocks_EntryDroppedWhenIdle(t *testing.T) {
	tbl := newFileLocks()
	id := fid(7)
	u1 := tbl.Lock(id)
	acquired := make(chan func())
	go func() { acquired <- tbl.Lock(id) }()
	deadline := time.Now().Add(5 * time.Second)
	for tbl.refs(id) < 2 {
		if time.Now().After(deadline) {
			t.Fatal("second locker never queued")
		}
		time.Sleep(time.Millisecond)
	}
	if got := tbl.size(); got != 1 {
		t.Fatalf("size = %d while one holds and one waits, want 1", got)
	}
	u1()
	if got := tbl.size(); got != 1 {
		t.Fatalf("size = %d after the first unlock with a waiter, want 1", got)
	}
	u2 := <-acquired
	u2()
	if got := tbl.size(); got != 0 {
		t.Fatalf("size = %d after the last unlock, want 0", got)
	}
}

func TestFileLocks_BoundedByFilesInFlight(t *testing.T) {
	tbl := newFileLocks()
	for i := uint64(0); i < 10000; i++ {
		tbl.Lock(fid(i))()
	}
	if got := tbl.size(); got != 0 {
		t.Fatalf("size = %d after 10000 files locked in turn, want 0", got)
	}
}

// sameShard returns n files other than of that hash to of's shard.
func sameShard(tbl *fileLocks, of core.FileID, n int) []core.FileID {
	var out []core.FileID
	for i := uint64(1 << 32); len(out) < n; i++ {
		if f := fid(i); f != of && tbl.shardOf(f) == tbl.shardOf(of) {
			out = append(out, f)
		}
	}
	return out
}

// freeLen reports how many released entries id's shard keeps for reuse.
func (t *fileLocks) freeLen(id core.FileID) int {
	s := t.shardOf(id)
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.free)
}

func TestFileLocks_ReusedEntryServesItsNewFile(t *testing.T) {
	tbl := newFileLocks()
	a := fid(1)
	b := sameShard(tbl, a, 1)[0]
	tbl.Lock(a)()
	if got := tbl.freeLen(a); got != 1 {
		t.Fatalf("free list after a's release = %d entries, want 1", got)
	}
	ub := tbl.Lock(b)
	if got := tbl.freeLen(a); got != 0 {
		t.Fatalf("free list after b's lock = %d entries, want 0: b did not reuse a's entry", got)
	}
	// a's domain is its own again while b holds the reused entry.
	done := make(chan func())
	go func() { done <- tbl.Lock(a) }()
	select {
	case ua := <-done:
		ua()
	case <-time.After(5 * time.Second):
		t.Fatal("a's domain waited on b, which holds a's reused entry")
	}
	if got := tbl.refs(b); got != 1 {
		t.Fatalf("b's refs = %d while b holds, want 1", got)
	}
	ub()
	if got := tbl.size(); got != 0 {
		t.Fatalf("size = %d after both files released, want 0", got)
	}
}

func TestFileLocks_FreeListBounded(t *testing.T) {
	tbl := newFileLocks()
	a := fid(1)
	files := append(sameShard(tbl, a, 2*fileLockFree), a)
	var unlocks []func()
	for _, f := range files {
		unlocks = append(unlocks, tbl.Lock(f))
	}
	for _, u := range unlocks {
		u()
	}
	if got := tbl.freeLen(a); got != fileLockFree {
		t.Fatalf("free list after %d releases in one shard = %d entries, want %d", len(files), got, fileLockFree)
	}
	if got := tbl.size(); got != 0 {
		t.Fatalf("size = %d after every file released, want 0", got)
	}
}
