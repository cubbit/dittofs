package datapath

import (
	"math/rand/v2"
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
		got := l.oldest.Load() ^ oldestBits
		switch {
		case want == nil && got != noOldest:
			t.Fatalf("step %d: oldest = %d, want none", step, got)
		case want != nil && got != want.Unix():
			t.Fatalf("step %d: oldest = %d, want %d", step, got, want.Unix())
		}
	}
}
