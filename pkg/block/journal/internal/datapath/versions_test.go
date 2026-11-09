package datapath

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
)

// setLimit sets the core's version limit at counter, for the tests below and
// the later tasks.
func setLimit(t *testing.T, c *Core, counter uint64) {
	t.Helper()
	if err := c.SetVersionLimit(core.Version{Counter: counter}); err != nil {
		t.Fatalf("SetVersionLimit(%d): %v", counter, err)
	}
}

// indexEntries returns how many entries a core's index holds for one file:
// held pieces, retained extents and removal markers alike, the way the
// inspection read-out enumerates them. A draw appends nothing, so the count
// taken before the draws must equal the one taken after them.
func indexEntries(t *testing.T, c *Core, f core.FileID) int64 {
	t.Helper()
	return int64(len(c.Index().FileEntries(f)))
}

// wantNoSpace refuses err unless it is a NoSpaceError naming the version
// limit: the named way the version limit refuses, matched by ErrNoSpace.
func wantNoSpace(t *testing.T, where string, err error) {
	t.Helper()
	var e *core.NoSpaceError
	if !errors.As(err, &e) {
		t.Fatalf("%s = %v, want a NoSpaceError naming the version limit", where, err)
	}
	if e.Limit != core.LimitVersion {
		t.Fatalf("%s names the %s limit, want the version one", where, e.Limit)
	}
	if e.Unit != core.UnitVersions {
		t.Fatalf("%s counts %s, want versions", where, e.Unit)
	}
	if !errors.Is(err, core.ErrNoSpace) {
		t.Fatalf("%s is not matched by ErrNoSpace: %v", where, err)
	}
}

// Two draws return strictly increasing versions and append nothing: the
// segment set's byte total and the index's entry count are the same after them
// as before.
func TestDraw_TwoDrawsIncreaseAndAppendNothing(t *testing.T) {
	c, _ := newTestCore(t)
	s := c.AddShare(1)
	f := testFile(1)
	v0 := mustWrite(t, c, s, f, 0, make([]byte, 4096))

	beforeBytes := c.set.Counters().UsedBytes
	beforeEntries := indexEntries(t, c, f)

	v1, err := c.Draw()
	if err != nil {
		t.Fatalf("Draw: %v", err)
	}
	v2, err := c.Draw()
	if err != nil {
		t.Fatalf("Draw: %v", err)
	}
	if v1.Compare(v2) >= 0 {
		t.Fatalf("the draws returned %v then %v, want strictly increasing", v1, v2)
	}
	if v1.Compare(v0) <= 0 {
		t.Fatalf("the first draw returned %v, not above the write's %v", v1, v0)
	}

	afterBytes := c.set.Counters().UsedBytes
	afterEntries := indexEntries(t, c, f)
	if afterBytes != beforeBytes {
		t.Fatalf("the draws moved the segment set's byte total from %d to %d", beforeBytes, afterBytes)
	}
	if afterEntries != beforeEntries {
		t.Fatalf("the draws moved the index's entry count from %d to %d", beforeEntries, afterEntries)
	}
	if p, ok := c.Index().Lookup(f, 0); !ok || p.Version != v0 {
		t.Fatalf("Lookup = %+v, %v; want the write's piece at %v: a draw publishes nothing", p, ok, v0)
	}
}

// A fresh core issues no version until the first limit is set: built over a
// zero State, with no SetVersionLimit call, both Draw and WriteAt are refused
// by the version limit.
func TestVersionLimit_NothingIssuedBeforeTheFirstLimit(t *testing.T) {
	c, _ := newTestCore(t, func(o *coreOptions) { o.noLimit = true })
	_, err := c.Draw()
	wantNoSpace(t, "Draw before the first limit", err)
	_, err = c.WriteAt(context.Background(), c.AddShare(1), testFile(1), 0, []byte("a"), 1)
	wantNoSpace(t, "WriteAt before the first limit", err)
}

// The limit refuses Draw and WriteAt once the counter reaches it, and each
// succeeds again after it is raised.
func TestVersionLimit_RefusesAtTheLimitAndSucceedsAfterARaise(t *testing.T) {
	c, _ := newTestCore(t)
	s := c.AddShare(1)
	f := testFile(1)
	// The limit is exclusive: a limit of 3 issues the two versions below it,
	// 1 and 2, and refuses the draw of 3.
	note := core.Version{Counter: 3}
	setLimit(t, c, note.Counter)

	v1, err := c.Draw()
	if err != nil {
		t.Fatalf("Draw below the limit: %v", err)
	}
	v2, err := c.Draw()
	if err != nil {
		t.Fatalf("Draw below the limit: %v", err)
	}
	for i, v := range []core.Version{v1, v2} {
		if v.Compare(core.Version{Counter: uint64(i + 1)}) != 0 {
			t.Fatalf("draw %d returned %v, want counter %d", i, v, i+1)
		}
		if v.Compare(note) >= 0 {
			t.Fatalf("draw %d returned %v, at or above the limit %v", i, v, note)
		}
	}

	_, err = c.Draw()
	wantNoSpace(t, "Draw at the limit", err)
	_, err = c.WriteAt(context.Background(), s, f, 0, []byte("a"), 1)
	wantNoSpace(t, "WriteAt at the limit", err)

	setLimit(t, c, note.Counter+3)
	if _, err = c.Draw(); err != nil {
		t.Fatalf("Draw after the raise: %v", err)
	}
	if _, err = c.WriteAt(context.Background(), s, f, 0, []byte("a"), 1); err != nil {
		t.Fatalf("WriteAt after the raise: %v", err)
	}
}

// A mark with a non-zero epoch half is refused and the limit it leaves
// unchanged still binds: the refused mark carries a counter far above it that
// would let a refused write through if it were applied before the epoch check.
func TestVersionLimit_EpochMarkRefusedLimitStillBinds(t *testing.T) {
	c, _ := newTestCore(t)
	s := c.AddShare(1)
	f := testFile(1)
	note := core.Version{Counter: 3}
	setLimit(t, c, note.Counter)

	if _, err := c.Draw(); err != nil {
		t.Fatalf("Draw below the limit: %v", err)
	}
	if _, err := c.Draw(); err != nil {
		t.Fatalf("Draw below the limit: %v", err)
	}
	// The counter stands at 2: the next version is at the limit and refused,
	// and the refused mark below would raise it far above if it were applied.
	if v := c.counters.NextVersion(); v.Compare(note) != 0 {
		t.Fatalf("the next version is %v, want the limit %v", v, note)
	}

	bad := core.Version{Epoch: 1, Counter: 1 << 40}
	if err := c.SetVersionLimit(bad); !errors.Is(err, core.ErrUnknownFormat) {
		t.Fatalf("SetVersionLimit of %v = %v, want ErrUnknownFormat", bad, err)
	}

	_, err := c.Draw()
	wantNoSpace(t, "Draw after the refused mark", err)
	_, err = c.WriteAt(context.Background(), s, f, 0, []byte("a"), 1)
	wantNoSpace(t, "WriteAt after the refused mark", err)
	if v := c.counters.NextVersion(); v.Compare(note) < 0 {
		t.Fatalf("the refused mark lowered the counter's next below the limit: %v", v)
	}
}

// No version repeats under concurrency: 8 goroutines each do 200 draws and
// 200 writes on distinct files, and every version returned, drawn or written,
// is distinct. The crash half of the no-repeat rule belongs to the open path
// and is not tested here.
func TestNoVersionRepeats_ConcurrentDrawAndWrite(t *testing.T) {
	c, _ := newTestCore(t)
	setLimit(t, c, 1+8*(200+200)+100)
	s := c.AddShare(1)

	var mu sync.Mutex
	seen := map[core.Version]bool{}
	add := func(v core.Version, where string) {
		mu.Lock()
		defer mu.Unlock()
		if seen[v] {
			panic(fmt.Sprintf("%s returned %v twice", where, v))
		}
		seen[v] = true
	}

	var wg sync.WaitGroup
	for g := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			f := testFile(byte(g + 1))
			for range 200 {
				v, err := c.Draw()
				if err != nil {
					panic(err)
				}
				add(v, "Draw")
				w, err := c.WriteAt(context.Background(), s, f, 0, []byte("x"), 1)
				if err != nil {
					panic(err)
				}
				add(w, "WriteAt")
			}
		}()
	}
	wg.Wait()
	if n := len(seen); n != 8*(200+200) {
		t.Fatalf("%d distinct versions across the goroutines, want %d", n, 8*(200+200))
	}
}
