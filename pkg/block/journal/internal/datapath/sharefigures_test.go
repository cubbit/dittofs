package datapath

import (
	"bytes"
	"context"
	"sync"
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/index"
)

func wantShare(t *testing.T, c *Core, when string, tag core.ShareTag, want shareHeld) {
	t.Helper()
	if got := c.shareFigures()[tag]; got != want {
		t.Fatalf("%s: share %d figures = %+v, want %+v", when, tag, got, want)
	}
}

func TestShareFiguresFollowEachTag(t *testing.T) {
	c, _ := newTestCore(t)
	s1, s2 := mustShare(t, c, 1), mustShare(t, c, 2)
	f1, f2 := testFile(1), testFile(2)
	mustWrite(t, c, s1, f1, 0, make([]byte, 4096))
	mustWrite(t, c, s2, f2, 0, make([]byte, 8192))
	wantShare(t, c, "after writes", 1, shareHeld{Held: 4096, Dirty: 4096})
	wantShare(t, c, "after writes", 2, shareHeld{Held: 8192, Dirty: 8192})
	if _, ok := c.shareFigures()[0]; ok {
		t.Fatal("tag 0 is listed in the share figures")
	}

	// A fill adds held bytes that are already offloaded: held rises, dirty not.
	if err := c.Fill(context.Background(), s1, f1, 4096, bytes.Repeat([]byte{1}, 4096), c.Index().AsOf(f1), ver(1)); err != nil {
		t.Fatalf("Fill: %v", err)
	}
	wantShare(t, c, "after fill", 1, shareHeld{Held: 8192, Dirty: 4096})

	// Overwriting retains the covered bytes under the same share.
	mustWrite(t, c, s2, f2, 0, make([]byte, 4096))
	wantShare(t, c, "after overwrite", 2, shareHeld{Held: 8192, Dirty: 8192, Retained: 4096})

	if _, err := c.Delete(context.Background(), s2, f2); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	wantShare(t, c, "after delete", 2, shareHeld{})
	wantShare(t, c, "other share untouched", 1, shareHeld{Held: 8192, Dirty: 4096})
}

// A removal lowers dirty bytes with held bytes in one delta, and the counters
// and the share's figures end equal.
func TestShareFiguresMatchCounters(t *testing.T) {
	var mu sync.Mutex
	var seen []index.HeldDelta
	c, _ := newTestCore(t, func(o *coreOptions) {
		o.accountSpy = func(d index.HeldDelta) { mu.Lock(); seen = append(seen, d); mu.Unlock() }
	})
	s := mustShare(t, c, 3)
	f := testFile(3)
	mustWrite(t, c, s, f, 0, make([]byte, 4096))
	if _, err := c.Truncate(context.Background(), s, f, 0); err != nil {
		t.Fatalf("Truncate: %v", err)
	}
	mu.Lock()
	last := seen[len(seen)-1]
	mu.Unlock()
	if last.HeldBytes >= 0 || last.DirtyBytes >= 0 {
		t.Fatalf("the removal's delta = %+v, want held and dirty both falling", last)
	}
	h, d, r := c.heldBytes()
	if got := c.shareFigures()[3]; got != (shareHeld{h, d, r}) || got != (shareHeld{}) {
		t.Fatalf("share figures = %+v, counters = %d/%d/%d, want both zero", got, h, d, r)
	}
}

// A reader never sees a share's dirty bytes above its held bytes while deltas
// rise and fall, because it takes the three figures as one step.
func TestShareFiguresReadAsOneStep(t *testing.T) {
	c, _ := newTestCore(t)
	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-done:
				return
			default:
			}
			if f := c.shareFigures()[7]; f.Dirty > f.Held {
				t.Errorf("share figures = %+v, dirty above held", f)
				return
			}
		}
	}()
	for i := 0; i < 2000; i++ {
		c.Account(index.HeldDelta{Tag: 7, File: testFile(9), HeldBytes: 100, DirtyBytes: 100})
		c.Account(index.HeldDelta{Tag: 7, File: testFile(9), HeldBytes: -100, DirtyBytes: -100})
	}
	close(done)
	wg.Wait()
}

func TestOldestDirty(t *testing.T) {
	c, clk := dirtyClockCore(t)
	s := mustShare(t, c, 1)
	ctx := context.Background()
	if got := c.oldestDirty(); !got.IsZero() {
		t.Fatalf("a clean core's oldest dirty = %v, want zero", got)
	}
	a, b := testFile(1), testFile(2)
	t0 := clk.Now()
	mustWrite(t, c, s, a, 0, make([]byte, 4096))
	clk.Advance(time.Minute)
	t1 := clk.Now()
	mustWrite(t, c, s, b, 0, make([]byte, 4096))
	if got := c.oldestDirty(); !got.Equal(t0) {
		t.Fatalf("oldest dirty = %v, want %v", got, t0)
	}
	if _, err := c.Delete(ctx, s, a); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if got := c.oldestDirty(); !got.Equal(t1) {
		t.Fatalf("after the oldest file went, oldest dirty = %v, want %v", got, t1)
	}
	if _, err := c.Delete(ctx, s, b); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if got := c.oldestDirty(); !got.IsZero() {
		t.Fatalf("after all files went, oldest dirty = %v, want zero", got)
	}
}
