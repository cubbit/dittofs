package index

import (
	"testing"
	"time"
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
