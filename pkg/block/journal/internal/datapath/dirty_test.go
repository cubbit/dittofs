package datapath

import (
	"context"
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/index"
	"github.com/cubbit/dittofs/pkg/block/journal/journaltest/kit"
)

// listed returns the share's dirty list as a map of file to oldest time.
func listed(c *Core, s *Share) map[core.FileID]time.Time {
	out := map[core.FileID]time.Time{}
	for _, d := range c.DirtyFiles(s) {
		out[d.File] = d.Oldest
	}
	return out
}

// wantListed fails unless the share's list is exactly want.
func wantListed(t *testing.T, c *Core, s *Share, when string, want map[core.FileID]time.Time) {
	t.Helper()
	got := listed(c, s)
	if len(got) != len(want) {
		t.Fatalf("%s: dirty list = %v, want %v", when, got, want)
	}
	for f, at := range want {
		if g, ok := got[f]; !ok || !g.Equal(at) {
			t.Fatalf("%s: dirty list = %v, want %v", when, got, want)
		}
	}
}

func dirtyClockCore(t *testing.T) (*Core, kit.DrivenClock) {
	t.Helper()
	clk := kit.NewStopwatchClock()
	c, _ := newTestCore(t, func(o *coreOptions) { o.clock = clk })
	return c, clk
}

// The time a file is listed with is the injected clock's reading at its
// write, not the write's mtime, and a later write does not move it.
func TestDirtyFilesCarryTheWritesClockTime(t *testing.T) {
	c, clk := dirtyClockCore(t)
	s := mustShare(t, c, 1)
	a, b := testFile(1), testFile(2)
	t0 := clk.Now()
	mustWrite(t, c, s, a, 0, make([]byte, 4096))
	clk.Advance(10 * time.Second)
	t1 := clk.Now()
	mustWrite(t, c, s, b, 0, make([]byte, 4096))
	wantListed(t, c, s, "two writes", map[core.FileID]time.Time{a: t0, b: t1})

	clk.Advance(10 * time.Second)
	mustWrite(t, c, s, a, 8192, make([]byte, 4096))
	wantListed(t, c, s, "a later write to a", map[core.FileID]time.Time{a: t0, b: t1})
}

// A file stays listed while a retained piece holds an unset byte, with that
// piece's time, and takes the survivor's time once the sync lifts the piece.
// It is dropped when nothing unset remains.
func TestDirtyFilesListsRetainedAndDrops(t *testing.T) {
	c, clk := dirtyClockCore(t)
	s := mustShare(t, c, 1)
	f := testFile(3)
	t0 := clk.Now()
	mustWrite(t, c, s, f, 0, make([]byte, 4096))
	// The advance below separates the writes' clock times, and it also fires
	// the sync-bound timer the first write armed. The bound sync must have
	// covered the first write before the second goes on: one that lags past
	// the second write covers its record too, and the coverage legitimately
	// lifts what it superseded before the setup reads it. Pending counts the
	// driven clock's armed After channels, and the worker's sync-bound timer
	// is the only one a fresh core asks for; a test that arms another After
	// of its own must wait on the bound timer's event some other way.
	eventually(t, "the first write's sync-bound timer to arm", func() bool { return clk.Pending() > 0 })
	syncs := c.Durability().Stats().Syncs
	clk.Advance(time.Minute)
	eventually(t, "the bound sync to cover the first write", func() bool {
		return c.Durability().Stats().Syncs > syncs
	})
	t1 := clk.Now()
	mustWrite(t, c, s, f, 0, make([]byte, 4096)) // supersedes all of the first
	if r := retainedBytes(c, f); r != 4096 {
		t.Fatalf("setup: retained = %d, want 4096", r)
	}
	wantListed(t, c, s, "the first write retained", map[core.FileID]time.Time{f: t0})

	syncFiles(t, c, f)
	if r := retainedBytes(c, f); r != 0 {
		t.Fatalf("after the sync retained = %d, want 0", r)
	}
	wantListed(t, c, s, "the retained piece lifted", map[core.FileID]time.Time{f: t1})

	if _, err := c.Delete(context.Background(), s, f); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	wantListed(t, c, s, "after Delete", map[core.FileID]time.Time{})
}

// A retained piece split by a partial overwrite keeps the file listed with the
// oldest write among the pieces that remain.
func TestDirtyFilesAfterRetainedSplit(t *testing.T) {
	c, clk := dirtyClockCore(t)
	s := mustShare(t, c, 1)
	f := testFile(4)
	t0 := clk.Now()
	mustWrite(t, c, s, f, 0, make([]byte, 4096))
	clk.Advance(time.Minute)
	mustWrite(t, c, s, f, 1024, make([]byte, 1024)) // retains the middle of the first
	clk.Advance(time.Minute)
	mustWrite(t, c, s, f, 1536, make([]byte, 1024)) // splits what the second left
	syncFiles(t, c, f)
	// The first write's remnants outside the overwrites stay held, so t0 is
	// still the oldest unset content.
	wantListed(t, c, s, "after the split and sync", map[core.FileID]time.Time{f: t0})
	if _, err := c.Truncate(context.Background(), s, f, 0); err != nil {
		t.Fatalf("Truncate: %v", err)
	}
	wantListed(t, c, s, "after Truncate to 0", map[core.FileID]time.Time{})
}

// A removal that covers all of a file's content unlists it; one that leaves
// dirty content keeps it listed with the remaining content's time.
func TestDirtyFilesAfterRemovals(t *testing.T) {
	c, clk := dirtyClockCore(t)
	s := mustShare(t, c, 1)
	ctx := context.Background()
	f := testFile(5)
	mustWrite(t, c, s, f, 0, make([]byte, 4096))
	clk.Advance(time.Minute)
	t1 := clk.Now()
	mustWrite(t, c, s, f, 4096, make([]byte, 4096))

	if _, err := c.Deallocate(ctx, s, f, core.Extent{Off: 0, Len: 4096}); err != nil {
		t.Fatalf("Deallocate: %v", err)
	}
	wantListed(t, c, s, "after deallocating the older half", map[core.FileID]time.Time{f: t1})
	if _, err := c.Truncate(ctx, s, f, 4096); err != nil {
		t.Fatalf("Truncate: %v", err)
	}
	wantListed(t, c, s, "after truncating the rest", map[core.FileID]time.Time{})
}

// A failed window that drops a file's only unset extent unlists the file; a
// file whose other write survives stays listed.
func TestDirtyFilesAfterDroppedWindow(t *testing.T) {
	h, c := newHeld(t, true)
	s := mustShare(t, c, 1)
	f := testFile(6)
	h.f = f
	h.arm() // the sync starts in the gap of f's first write, and fails
	mustWrite(t, c, s, f, 0, make([]byte, 4096))
	// The worker waits for the file's domain, so the write alone is listed.
	if _, ok := listed(c, s)[f]; !ok {
		t.Fatalf("the written file is not listed before its window fails: %v", listed(c, s))
	}
	h.open()
	if err := <-h.done; err == nil {
		t.Fatal("Sync returned no error although its sync failed")
	}
	eventually(t, "the failed write to be dropped", func() bool {
		held, _, _ := c.heldBytes()
		return held == 0
	})
	wantListed(t, c, s, "after the window dropped the only extent", map[core.FileID]time.Time{})
}

// Another share's files are not listed.
func TestDirtyFilesPerShard(t *testing.T) {
	c, clk := dirtyClockCore(t)
	s1, s2 := mustShare(t, c, 1), mustShare(t, c, 2)
	a, b := testFile(7), testFile(8)
	t0 := clk.Now()
	mustWrite(t, c, s1, a, 0, make([]byte, 100))
	mustWrite(t, c, s2, b, 0, make([]byte, 100))
	wantListed(t, c, s1, "share 1", map[core.FileID]time.Time{a: t0})
	wantListed(t, c, s2, "share 2", map[core.FileID]time.Time{b: t0})
}

// DirtyFiles serves the list: it reads no record and opens no segment. (The
// index keeps no walk counter, so that it is not walked is shown by the
// list's own contents answering without the index.)
func TestDirtyFilesTouchesNoIndexNoRecord(t *testing.T) {
	c, seam := newTestCore(t)
	s := mustShare(t, c, 1)
	for i := byte(1); i <= 20; i++ {
		mustWrite(t, c, s, testFile(i), 0, make([]byte, 512))
	}
	reads, direct, opens := seam.Calls(kit.OpRead), seam.Calls(kit.OpReadDirect), seam.Calls(kit.OpOpen)
	if n := len(c.DirtyFiles(s)); n != 20 {
		t.Fatalf("DirtyFiles listed %d files, want 20", n)
	}
	if got := seam.Calls(kit.OpRead); got != reads {
		t.Fatalf("DirtyFiles made %d storage reads, want 0", got-reads)
	}
	if got := seam.Calls(kit.OpReadDirect); got != direct {
		t.Fatalf("DirtyFiles made %d direct storage reads, want 0", got-direct)
	}
	if got := seam.Calls(kit.OpOpen); got != opens {
		t.Fatalf("DirtyFiles made %d storage opens, want 0", got-opens)
	}
}

// A delta that changes no byte but moves the dirty line is applied; the zero
// delta is not.
func TestAccountAppliesTransitionWithoutByteChange(t *testing.T) {
	c, _ := newTestCore(t)
	s := mustShare(t, c, 1)
	f := testFile(9)
	at := time.Unix(1000, 0)
	c.Account(index.HeldDelta{})
	wantListed(t, c, s, "zero delta", map[core.FileID]time.Time{})
	c.Account(index.HeldDelta{Tag: 1, File: f, Dirty: index.BecameDirty, OldestDirty: at})
	wantListed(t, c, s, "BecameDirty with no bytes", map[core.FileID]time.Time{f: at})
	at2 := at.Add(time.Hour)
	c.Account(index.HeldDelta{Tag: 1, File: f, OldestDirty: at2})
	wantListed(t, c, s, "oldest moved with no bytes", map[core.FileID]time.Time{f: at2})
	c.Account(index.HeldDelta{Tag: 1, File: f, Dirty: index.BecameClean})
	wantListed(t, c, s, "BecameClean with no bytes", map[core.FileID]time.Time{})
	// A delta for an unlisted file that is not dirty does not list it.
	c.Account(index.HeldDelta{Tag: 1, File: f, OldestDirty: at})
	wantListed(t, c, s, "a state for an unlisted file", map[core.FileID]time.Time{})
}
