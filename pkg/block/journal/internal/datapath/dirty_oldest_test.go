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

// Several writers dirty, move and clean files over their own run of seconds
// while a sampler reads the stored oldest: every read is none or a second some
// writer used, and once they stop the stored value equals a scan of the dirty
// set. A settle dropped from apply leaves the stored value on a stale top.
func TestOldestDirtyUnderConcurrentApply(t *testing.T) {
	const writers, perWriter, rounds = 4, 40, 60
	base := int64(1_000_000)
	// Writer w uses the seconds base + w*1000 + [0, perWriter).
	used := func(k int64) bool {
		off := k - base
		return off >= 0 && off < writers*1000 && off%1000 < perWriter
	}
	var l dirtyList
	stop := make(chan struct{})
	var sampler sync.WaitGroup
	sampler.Add(1)
	go func() {
		defer sampler.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			if k := l.oldest.Load() ^ noOldest; k != noOldest && !used(k) {
				t.Errorf("sampled oldest %d, want none or a second a writer used", k)
				return
			}
		}
	}()
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
	close(stop)
	sampler.Wait()

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
