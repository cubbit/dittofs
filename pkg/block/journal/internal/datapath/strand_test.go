package datapath

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/block/journal/journaltest/kit"
)

// strandTag is the tag the strand tests strand, otherTag the tag its fellow
// handle attaches under.
const (
	strandTag core.ShareTag = 0x31
	otherTag  core.ShareTag = 0x32
)

// A read of T's file through U's handle is refused by the tag check before
// Strand, and the stranding changes nothing about that; through T's own
// handle the bytes are served after it.
func TestStrandRefusedReadThroughOtherTag(t *testing.T) {
	c, _ := newTestCore(t)
	tt, u := mustShare(t, c, strandTag), mustShare(t, c, otherTag)
	f := testFile(23)
	mustWrite(t, c, tt, f, 0, []byte("bb"))
	p := sentinels(2)
	if _, _, _, _, err := c.ReadAt(context.Background(), u, f, 0, p); !errors.Is(err, core.ErrOtherShare) {
		t.Fatalf("a read of F through U before Strand = %v, want ErrOtherShare (the tag check refuses it)", err)
	}
	if err := c.Strand(strandTag); err != nil {
		t.Fatalf("Strand(%d): %v", strandTag, err)
	}
	if _, _, _, _, err := c.ReadAt(context.Background(), u, f, 0, p); !errors.Is(err, core.ErrOtherShare) {
		t.Fatalf("a read of F through U after Strand = %v, want ErrOtherShare", err)
	}
	if n, missing, _, _, err := c.ReadAt(context.Background(), tt, f, 0, p); err != nil || n != 2 || len(missing) != 0 || string(p) != "bb" {
		t.Fatalf("a read of F through T after Strand = %d bytes, missing %v, err %v, p %q: a stranding must not hold the tag's own handle back", n, missing, err, p)
	}
}

// Strand appends nothing: the storage the seam served is exactly what it was,
// and the tag reads stranded.
func TestStrandAppendsNothing(t *testing.T) {
	c, seam := newTestCore(t)
	s := mustShare(t, c, strandTag)
	mustWrite(t, c, s, testFile(24), 0, []byte("cc"))
	// The write's roll took a spare, and the pool makes its replacement in
	// the background; once the spares are back the pool is idle, so nothing
	// reaches storage after this point but what Strand itself does.
	awaitSpares(t, c, 0)
	writes, syncs := seam.Calls(kit.OpWrite), seam.Calls(kit.OpSync)
	recs := readRecords(t, seam)

	if err := c.Strand(strandTag); err != nil {
		t.Fatalf("Strand(%d): %v", strandTag, err)
	}
	if !c.Stranded(strandTag) {
		t.Fatalf("Stranded(%d) = false after Strand, want true", strandTag)
	}
	if got := seam.Calls(kit.OpWrite); got != writes {
		t.Fatalf("%d storage writes after Strand, want %d", got, writes)
	}
	if got := seam.Calls(kit.OpSync); got != syncs {
		t.Fatalf("%d storage syncs after Strand, want %d", got, syncs)
	}
	if got := readRecords(t, seam); len(got) != len(recs) {
		t.Fatalf("%d records on disk after Strand, want %d", len(got), len(recs))
	}
}

// The refusals: tag 0, the tag of no share, is refused like Share refuses it;
// a core whose Close began refuses the stranding; a tag already stranded
// returns nil again, for the set keeps each tag once.
func TestStrandRefusals(t *testing.T) {
	c, _ := newTestCore(t)
	if err := c.Strand(0); !errors.Is(err, core.ErrInvalidSetting) {
		t.Fatalf("Strand(0) = %v, want ErrInvalidSetting", err)
	}
	if c.Stranded(0) {
		t.Fatal("Stranded(0) = true, want false")
	}
	if err := c.Strand(strandTag); err != nil {
		t.Fatalf("Strand(%d) = %v, want nil", strandTag, err)
	}
	if err := c.Strand(strandTag); err != nil {
		t.Fatalf("a second Strand(%d) = %v, want nil", strandTag, err)
	}
	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := c.Strand(strandTag); !errors.Is(err, core.ErrClosed) {
		t.Fatalf("Strand(%d) after Close = %v, want ErrClosed", strandTag, err)
	}
	// The set a closed core already holds it keeps: closing the core strands
	// nothing new, and nothing of what the caller stranded is dropped by it.
	if !c.Stranded(strandTag) {
		t.Fatalf("Stranded(%d) = false after Close, want true", strandTag)
	}
}

// Strand, Stranded, Share and a handle's Close race, on distinct tags: every
// tag a Strand named reads stranded afterwards, whatever the racing calls did
// to the handles of other tags.
func TestStrandConcurrent(t *testing.T) {
	c, _ := newTestCore(t)
	const callers = 8
	var wg sync.WaitGroup
	for g := range callers {
		tag := core.ShareTag(10 + g)
		s := mustShare(t, c, tag)
		if err := c.Strand(tag); err != nil {
			t.Fatalf("Strand(%d): %v", tag, err)
		}
		for i := range callers {
			wg.Go(func() {
				_ = c.Strand(core.ShareTag(100 + i))
				_ = c.Stranded(tag)
				if _, err := c.Share(core.ShareTag(200+i), 0, core.Version{}); err != nil {
					t.Errorf("Share(%d): %v", 200+i, err)
					return
				}
				if err := s.Close(); err != nil && !errors.Is(err, core.ErrClosed) {
					t.Errorf("Close: %v", err)
				}
			})
		}
		wg.Wait()
		for i := range callers {
			if !c.Stranded(core.ShareTag(100 + i)) {
				t.Errorf("Stranded(%d) = false after the racing calls, want true", 100+i)
			}
		}
		if !c.Stranded(tag) {
			t.Errorf("Stranded(%d) = false after the racing calls, want true", tag)
		}
	}
	for g := range callers {
		if err := c.Strand(core.ShareTag(300 + g)); err != nil {
			t.Fatalf("Strand(%d) after the closed-handle rounds: %v", 300+g, err)
		}
	}
}
