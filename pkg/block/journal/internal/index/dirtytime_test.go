package index

import (
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
)

var t0 = time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)

// writtenAt is a client write of n bytes at off, made at t0+secs seconds.
func writtenAt(off, n int64, seg uint32, v uint64, secs int) Placed {
	p := write(off, n, rec(1, seg), v)
	p.At = t0.Add(time.Duration(secs) * time.Second)
	return p
}

// markOffloaded marks e offloaded through the file's update and returns the delta.
func markOffloaded(t *testing.T, ix *Index, e ...[2]int64) HeldDelta {
	t.Helper()
	d, err := ix.fs.update(testFile, func(f *file) error {
		for _, x := range e {
			f.held.markOffloaded(ext(x[0], x[1]), ver(1), ver(100))
		}
		return nil
	})
	if err != nil {
		t.Fatalf("mark: %v", err)
	}
	return d
}

func TestOldestDirtyAfterPartialOffload(t *testing.T) {
	ix := New(1 << 20)
	d := writeRetained(t, ix, writtenAt(0, 4096, 0, 1, 1))
	if !d.OldestDirty.Equal(t0.Add(1 * time.Second)) {
		t.Fatalf("after A: OldestDirty = %v, want t1", d.OldestDirty)
	}
	d = writeRetained(t, ix, writtenAt(8192, 4096, 4096, 2, 2))
	if !d.OldestDirty.Equal(t0.Add(1 * time.Second)) {
		t.Fatalf("after B: OldestDirty = %v, want t1 (A is still dirty)", d.OldestDirty)
	}
	d = markOffloaded(t, ix, [2]int64{0, 4096})
	if !d.OldestDirty.Equal(t0.Add(2 * time.Second)) {
		t.Fatalf("after offloading A: OldestDirty = %v, want t2", d.OldestDirty)
	}
	d = markOffloaded(t, ix, [2]int64{8192, 4096})
	if d.Dirty != BecameClean || !d.OldestDirty.IsZero() {
		t.Fatalf("after offloading B: Dirty = %v OldestDirty = %v, want BecameClean and zero", d.Dirty, d.OldestDirty)
	}
}

func TestRetainedCountsForOldestDirty(t *testing.T) {
	ix := New(1 << 20)
	writeRetained(t, ix, writtenAt(0, 4096, 0, 1, 1))
	// The overwrite retains the dirty first write; offloading the new
	// content leaves only the retained piece dirty.
	writeRetained(t, ix, writtenAt(0, 4096, 4096, 2, 5))
	d, err := ix.fs.update(testFile, func(f *file) error {
		f.held.markOffloaded(ext(0, 4096), ver(2), ver(100))
		return nil
	})
	if err != nil {
		t.Fatalf("mark: %v", err)
	}
	if d.Dirty == BecameClean || !d.OldestDirty.Equal(t0.Add(1*time.Second)) {
		t.Fatalf("Dirty = %v OldestDirty = %v, want the retained piece's t1", d.Dirty, d.OldestDirty)
	}
}

func TestOverwriteKeepsOlderDirtyTime(t *testing.T) {
	ix := New(1 << 20)
	writeRetained(t, ix, writtenAt(0, 8192, 0, 1, 1))
	d := writeRetained(t, ix, writtenAt(0, 4096, 8192, 2, 9))
	if !d.OldestDirty.Equal(t0.Add(1 * time.Second)) {
		t.Fatalf("OldestDirty = %v, want t1: the overwrite must not reset it", d.OldestDirty)
	}
}

// dirtyBuckets writes n single-version dirty extents of one file through the
// index, the i-th at the time at(i) gives, and returns the file's dirty-time
// bucket count and its oldest dirty time.
func dirtyBuckets(t *testing.T, n int, at func(i int) time.Time) (buckets int, oldest time.Time) {
	t.Helper()
	ix := New(1 << 40)
	for i := range n {
		p := write(int64(i)*4096, 4096, rec(core.SegmentID(1+i/60_000), uint32(i%60_000)*4096), uint64(i+1))
		p.At = at(i)
		d, err := ix.WriteRetain(testTag, testFile, p, core.Seq(i+1))
		if err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
		oldest = d.OldestDirty
	}
	ix.fs.view(testFile, func(f *file) { buckets = len(f.dirtyAt.vers) })
	return buckets, oldest
}

// TestDirtyTimeTableIsBoundedBySeconds: a file of many dirty versions keeps a
// bucket per second of backlog, not a record per version, and reports a time
// at or before the oldest write and less than a second before it.
func TestDirtyTimeTableIsBoundedBySeconds(t *testing.T) {
	const n = 100_000
	for _, tc := range []struct {
		name       string
		at         func(i int) time.Time
		maxBuckets int
	}{
		{"one second", func(i int) time.Time { return t0.Add(time.Duration(i) * time.Second / n) }, 2},
		{"many seconds", func(i int) time.Time { return t0.Add(time.Duration(i/1000) * time.Second) }, n/1000 + 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			buckets, oldest := dirtyBuckets(t, n, tc.at)
			if buckets > tc.maxBuckets {
				t.Fatalf("%d buckets for %d versions, want at most %d", buckets, n, tc.maxBuckets)
			}
			if first := tc.at(0); oldest.After(first) || first.Sub(oldest) >= time.Second {
				t.Fatalf("OldestDirty = %v, want within a second at or before %v", oldest, first)
			}
		})
	}
}
