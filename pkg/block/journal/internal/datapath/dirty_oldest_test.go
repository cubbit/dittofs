package datapath

import (
	"math/rand/v2"
	"sync"
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/index"
)

// The maintained oldest equals the lowest second a brute-force scan finds,
// over a seeded run of adds, moves and removals that crosses the heap's
// compaction.
func TestOldestDirtyMatchesAScan(t *testing.T) {
	var l dirtyList
	rng := rand.New(rand.NewPCG(1, 2))
	base := time.Unix(1_000_000, 0)
	at := map[int]time.Time{}
	for step := range 5000 {
		i := rng.IntN(300)
		_, listed := at[i]
		if listed && rng.IntN(2) == 0 {
			l.apply(index.HeldDelta{Tag: 1, File: benchFile(i), Dirty: index.BecameClean})
			delete(at, i)
		} else {
			ts := base.Add(time.Duration(rng.IntN(100000)) * time.Second)
			d := index.HeldDelta{Tag: 1, File: benchFile(i), OldestDirty: ts}
			if !listed {
				d.Dirty = index.BecameDirty
			}
			l.apply(d)
			at[i] = ts
		}
		var want *time.Time
		for _, ts := range at {
			if want == nil || ts.Before(*want) {
				w := ts
				want = &w
			}
		}
		if bound := 2*len(l.byTime) + 16; len(l.keys) > bound {
			t.Fatalf("step %d: heap holds %d entries for %d live seconds, want at most %d: compaction did not run", step, len(l.keys), len(l.byTime), bound)
		}
		got := l.oldest.Load() ^ noOldest
		switch {
		case want == nil && got != noOldest:
			t.Fatalf("step %d: oldest = %d, want none", step, got)
		case want != nil && got != want.Unix():
			t.Fatalf("step %d: oldest = %d, want %d", step, got, want.Unix())
		}
	}
}

// After several writers dirty, move and clean files at once, the stored
// oldest equals a scan of the dirty set. It guards quiescent agreement only:
// apply publishes the minimum under the list's mutex, so there is no window
// between computing it and storing it for a test to land a second apply in,
// and a stale store would show here only by chance. The sequential test above
// pins every step and is the one that fails when the publish is dropped; this
// one is here to give the race detector concurrent apply calls.
func TestOldestDirtyQuiescentAfterConcurrentApply(t *testing.T) {
	const writers, perWriter, rounds = 4, 40, 60
	base := int64(1_000_000)
	var l dirtyList
	var wg sync.WaitGroup
	for w := range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rng := rand.New(rand.NewPCG(uint64(w), 7))
			listed := map[int]bool{}
			for range rounds {
				i := rng.IntN(perWriter)
				id := benchFile(w*perWriter + i)
				ts := time.Unix(base+int64(w)*1000+int64(rng.IntN(perWriter)), 0)
				switch {
				case listed[i] && rng.IntN(2) == 0:
					l.apply(index.HeldDelta{Tag: 1, File: id, Dirty: index.BecameClean})
					delete(listed, i)
				case listed[i]:
					l.apply(index.HeldDelta{Tag: 1, File: id, OldestDirty: ts})
				default:
					l.apply(index.HeldDelta{Tag: 1, File: id, Dirty: index.BecameDirty, OldestDirty: ts})
					listed[i] = true
				}
			}
		}()
	}
	wg.Wait()

	l.mu.Lock()
	defer l.mu.Unlock()
	want := int64(noOldest)
	for _, e := range l.m {
		if k := e.oldest.Unix(); want == noOldest || k < want {
			want = k
		}
	}
	if got := l.oldest.Load() ^ noOldest; got != want {
		t.Fatalf("quiescent oldest = %d, want %d (a scan of %d dirty files)", got, want, len(l.m))
	}
}
