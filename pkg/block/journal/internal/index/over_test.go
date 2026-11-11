package index

import (
	"reflect"
	"slices"
	"testing"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
)

const k4 = 4096

// sameChanges reports whether two Since results are equal, Over included.
func sameChanges(a, b []Change) bool {
	return slices.EqualFunc(a, b, func(x, y Change) bool { return reflect.DeepEqual(x, y) })
}

// sinceOver returns, per yielded write of id above from, its version and Over.
func sinceOver(fs *files, id core.FileID, from uint64) map[core.Version][]core.Extent {
	got := make(map[core.Version][]core.Extent)
	for _, c := range fs.since(id, ver(from), nil) {
		if c.Kind == core.ChangeWrite {
			got[c.Version] = append(got[c.Version], c.Over...)
		}
	}
	return got
}

func wantOver(t *testing.T, got map[core.Version][]core.Extent, want map[core.Version][]core.Extent) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("writes yielded = %v, want %v", got, want)
	}
	for v, w := range want {
		if g, ok := got[v]; !ok || !slices.Equal(g, w) {
			t.Fatalf("Over of version %d = %v (yielded %v), want %v", v.Counter, g, ok, w)
		}
	}
}

// TestOverNamesSupersededRunAfterEntryLeft: v3 is wholly superseded, so no
// piece of it is held, yet the write that superseded it still names its run
// for a caller whose applied version is below 3, and not for one at 3.
func TestOverNamesSupersededRunAfterEntryLeft(t *testing.T) {
	var fs files
	id := fid(1)
	change(t, &fs, id, func(f *file) error {
		f.write(write(0, k4, rec(1, 0), 3))
		f.write(write(0, 2*k4, rec(1, 64), 5))
		return nil
	})
	wantOver(t, sinceOver(&fs, id, 0), map[core.Version][]core.Extent{ver(5): {ext(0, k4)}})
	wantOver(t, sinceOver(&fs, id, 3), map[core.Version][]core.Extent{ver(5): nil})
}

// TestOverChain: v3 under v5 under v7. The v5 piece left the index too, but
// the runs it superseded are reported by the write that now holds the bytes,
// while the run's own version is above from.
func TestOverChain(t *testing.T) {
	var fs files
	id := fid(1)
	change(t, &fs, id, func(f *file) error {
		f.write(write(0, k4, rec(1, 0), 3))
		f.write(write(0, 2*k4, rec(1, 64), 5))
		f.write(write(0, 2*k4, rec(1, 128), 7))
		return nil
	})
	wantOver(t, sinceOver(&fs, id, 0), map[core.Version][]core.Extent{ver(7): {ext(0, 2*k4)}})
	wantOver(t, sinceOver(&fs, id, 3), map[core.Version][]core.Extent{ver(7): {ext(0, 2*k4)}})
	wantOver(t, sinceOver(&fs, id, 5), map[core.Version][]core.Extent{ver(7): nil})
}

// TestOverSettleDropsRuns: a settle at the superseding write's version drops
// its runs and keeps the write; a settle below keeps the runs.
func TestOverSettleDropsRuns(t *testing.T) {
	var fs files
	id := fid(1)
	change(t, &fs, id, func(f *file) error {
		f.write(write(0, k4, rec(1, 0), 3))
		f.write(write(0, k4, rec(1, 64), 5))
		return nil
	})
	fs.settleRemovals(id, ver(4))
	wantOver(t, sinceOver(&fs, id, 0), map[core.Version][]core.Extent{ver(5): {ext(0, k4)}})
	fs.settleRemovals(id, ver(5))
	wantOver(t, sinceOver(&fs, id, 0), map[core.Version][]core.Extent{ver(5): nil})
}

// TestOverPartialSplit: a write over the middle of a larger write names only
// the run it overlapped, even though the larger write keeps both ends.
func TestOverPartialSplit(t *testing.T) {
	var fs files
	id := fid(1)
	change(t, &fs, id, func(f *file) error {
		f.write(write(0, 4*k4, rec(1, 0), 3))
		f.write(write(k4, k4, rec(1, 64), 5))
		return nil
	})
	wantOver(t, sinceOver(&fs, id, 0), map[core.Version][]core.Extent{
		ver(3): nil, ver(5): {ext(k4, k4)},
	})
}

// TestOverIsEmptyForMarkersAndLoss: a removal over a write leaves no Over on
// anything, and a write over a removed extent supersedes no write.
func TestOverIsEmptyForMarkersAndLoss(t *testing.T) {
	var fs files
	id := fid(1)
	change(t, &fs, id, func(f *file) error {
		f.write(write(0, k4, rec(1, 0), 3))
		f.truncate(0, ver(4))
		f.write(write(0, k4, rec(1, 64), 5))
		return nil
	})
	for _, c := range fs.since(id, core.Version{}, nil) {
		if len(c.Over) != 0 {
			t.Fatalf("change %+v carries Over %v, want none", c, c.Over)
		}
	}
}

// TestOverOrphanedRunWaitsForSettle: when the superseding write's own
// piece is released, its run waits for a settle at its version and does not
// leak onto an unrelated write.
func TestOverOrphanedRunWaitsForSettle(t *testing.T) {
	var fs files
	id := fid(1)
	change(t, &fs, id, func(f *file) error {
		f.write(write(0, k4, rec(1, 0), 3))
		f.write(write(0, k4, rec(1, 64), 5))
		f.held.markOffloaded(ext(0, k4), ver(0), ver(5))
		if _, err := f.held.release(ext(0, k4), nil); err != nil {
			return err
		}
		f.write(write(k4, k4, rec(1, 128), 7))
		return nil
	})
	wantOver(t, sinceOver(&fs, id, 0), map[core.Version][]core.Extent{ver(7): nil})
	if n := overCount(&fs, id); n != 1 {
		t.Fatalf("runs kept = %d, want 1 until settled", n)
	}
	fs.settleRemovals(id, ver(5))
	if n := overCount(&fs, id); n != 0 {
		t.Fatalf("runs kept after settle = %d, want 0", n)
	}
}

func overCount(fs *files, id core.FileID) int {
	n := 0
	fs.view(id, func(f *file) { n = len(f.held.over) })
	return n
}

// TestOverRetainedSupersede: a write that retains what it supersedes names
// the same run as a plain write.
func TestOverRetainedSupersede(t *testing.T) {
	var fs files
	id := fid(1)
	change(t, &fs, id, func(f *file) error {
		f.write(write(0, k4, rec(1, 0), 3))
		f.writeRetain(write(0, k4, rec(1, 64), 5))
		return nil
	})
	wantOver(t, sinceOver(&fs, id, 0), map[core.Version][]core.Extent{ver(5): {ext(0, k4)}})
}

// TestOverChainPartial: a later write over part of the middle write takes
// that part's run, and the middle write keeps the rest clipped to what it
// still holds.
func TestOverChainPartial(t *testing.T) {
	var fs files
	id := fid(1)
	h := int64(k4 / 2)
	change(t, &fs, id, func(f *file) error {
		f.write(write(0, 8*h, rec(1, 0), 3))
		f.write(write(0, 4*h, rec(1, 64), 5))
		f.write(write(0, h, rec(1, 128), 7))
		return nil
	})
	wantOver(t, sinceOver(&fs, id, 0), map[core.Version][]core.Extent{
		ver(3): nil, ver(5): {ext(h, 3*h)}, ver(7): {ext(0, h)},
	})
}

// TestOverFailedWindowDropsItsRuns: failing the record that superseded a write
// holds the write again and forgets the failed record's runs.
func TestOverFailedWindowDropsItsRuns(t *testing.T) {
	var fs files
	id := fid(1)
	change(t, &fs, id, func(f *file) error {
		f.write(write(0, k4, rec(1, 0), 3))
		f.writeRetain(write(0, k4, rec(2, 0), 5))
		f.failWindow([]WindowRecord{failed(rec(2, 0), 5)}, nil)
		return nil
	})
	wantOver(t, sinceOver(&fs, id, 0), map[core.Version][]core.Extent{ver(3): nil})
	if n := overCount(&fs, id); n != 0 {
		t.Fatalf("runs kept after the window failed = %d, want 0", n)
	}
}
