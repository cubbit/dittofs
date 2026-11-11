package datapath

import (
	"bytes"
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/durability"
)

func idBytes(f core.FileID) []byte { b := f.Bytes(); return b[:] }

func filesOf(c *Core, s *Share) []core.FileID {
	out := c.Files(s)
	slices.SortFunc(out, func(a, b core.FileID) int { return bytes.Compare(idBytes(a), idBytes(b)) })
	return out
}

func TestSettleDropsMarkersAndLossNotExtents(t *testing.T) {
	c, _ := newTestCore(t)
	s := mustShare(t, c, 1)
	f := testFile(1)
	ctx := context.Background()
	v1 := mustWrite(t, c, s, f, 0, pattern(16<<10, 1))
	syncFiles(t, c, f)
	d1, err := c.Deallocate(ctx, s, f, ex(4<<10, 2<<10))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Durability().Lose(ctx, durability.Loss{Share: 1, File: f, Extent: ex(20<<10, 4<<10), Version: v1, Seq: 900, Reason: core.LossCorrupt, Synced: true}); err != nil {
		t.Fatal(err)
	}
	d2, err := c.Deallocate(ctx, s, f, ex(8<<10, 2<<10))
	if err != nil {
		t.Fatal(err)
	}
	v3 := mustWrite(t, c, s, f, 12<<10, pattern(2<<10, 3))

	// One Settle at the second marker drops both markers, the loss and, by
	// version, every older one.
	c.Settle(s, f, d2)
	for _, ch := range sinceAll(t, c, s, f, core.Version{}) {
		if ch.Kind != core.ChangeWrite {
			t.Fatalf("after Settle(%v) Since still yields %+v (markers were %v and %v)", d2, ch, d1, d2)
		}
	}
	// A Settle above every version leaves each held extent held and readable.
	c.Settle(s, f, v3)
	want, err := c.Extents(s, f)
	if err != nil || len(want) == 0 {
		t.Fatalf("Extents = %v, %v", want, err)
	}
	buf := make([]byte, 2<<10)
	n, missing, held, _, err := c.ReadAt(ctx, s, f, 12<<10, buf)
	if err != nil || n != len(buf) || len(missing) != 0 || len(held) == 0 {
		t.Fatalf("ReadAt above the markers = n %d missing %v held %v err %v, want the write held", n, missing, held, err)
	}
	if !slices.Equal(buf, pattern(2<<10, 3)) {
		t.Fatal("the held extent's bytes changed after Settle")
	}
	// A Settle below a held write's version does not drop it either.
	c.Settle(s, f, core.Version{})
	if got, _ := c.Extents(s, f); !slices.Equal(got, want) {
		t.Fatalf("Extents after Settle = %v, want %v", got, want)
	}
}

func TestSettleUnappliedRemovalReadsLost(t *testing.T) {
	c, _ := newTestCore(t)
	s := mustShare(t, c, 1)
	f := testFile(1)
	ctx := context.Background()
	mustWrite(t, c, s, f, 0, pattern(16<<10, 1))
	d, err := c.Deallocate(ctx, s, f, ex(4<<10, 4<<10))
	if err != nil {
		t.Fatal(err)
	}
	c.Settle(s, f, d)
	if got := sinceAll(t, c, s, f, core.Version{}); slices.ContainsFunc(got, func(ch core.Change) bool { return ch.Kind == core.ChangeDeallocate }) {
		t.Fatalf("the marker survived Settle: %+v", got)
	}
	buf := make([]byte, 4<<10)
	_, missing, _, _, err := c.ReadAt(ctx, s, f, 4<<10, buf)
	if err != nil {
		t.Fatal(err)
	}
	if want := []core.Extent{ex(4<<10, 4<<10)}; !slices.Equal(missing, want) {
		t.Fatalf("missing = %v, want %v: the removal's extent holds nothing", missing, want)
	}
}

func TestFilesListsLossOnlyFile(t *testing.T) {
	c, _ := newTestCore(t)
	s := mustShare(t, c, 1)
	f, held := testFile(1), testFile(2)
	ctx := context.Background()
	v := mustWrite(t, c, s, f, 0, pattern(4<<10, 1))
	syncFiles(t, c, f)
	mustWrite(t, c, s, held, 0, pattern(4<<10, 2))
	// Truncate to nothing leaves f with no extent; its marker is then settled,
	// leaving only a later synced loss.
	tr, err := c.Truncate(ctx, s, f, 0)
	if err != nil {
		t.Fatal(err)
	}
	c.Settle(s, f, tr)
	if got := filesOf(c, s); !slices.Equal(got, []core.FileID{held}) {
		t.Fatalf("Files = %v, want only the file holding an extent", got)
	}
	if _, err := c.Durability().Lose(ctx, durability.Loss{Share: 1, File: f, Extent: ex(0, 4<<10), Version: tr, Seq: 900, Reason: core.LossCorrupt, Synced: true}); err != nil {
		t.Fatalf("Lose: %v (v=%v)", err, v)
	}
	if got := filesOf(c, s); !slices.Equal(got, []core.FileID{f, held}) {
		t.Fatalf("Files = %v, want the loss-only file and the held one", got)
	}
	if ext, _ := c.Extents(s, f); len(ext) != 0 {
		t.Fatalf("loss-only file holds %v", ext)
	}
	c.Settle(s, f, tr)
	if got := filesOf(c, s); !slices.Equal(got, []core.FileID{held}) {
		t.Fatalf("Files after Settle = %v, want the loss gone", got)
	}
}

func TestExtentsOffsetOrder(t *testing.T) {
	c, _ := newTestCore(t)
	s := mustShare(t, c, 1)
	f := testFile(1)
	mustWrite(t, c, s, f, 40<<10, pattern(4<<10, 1))
	mustWrite(t, c, s, f, 0, pattern(4<<10, 2))
	mustWrite(t, c, s, f, 20<<10, pattern(4<<10, 3))
	got, err := c.Extents(s, f)
	if err != nil {
		t.Fatal(err)
	}
	if want := []core.Extent{ex(0, 4<<10), ex(20<<10, 4<<10), ex(40<<10, 4<<10)}; !slices.Equal(got, want) {
		t.Fatalf("Extents = %v, want %v", got, want)
	}
	if got, err := c.Extents(s, testFile(9)); err != nil || len(got) != 0 {
		t.Fatalf("Extents of an unknown file = %v, %v, want none", got, err)
	}
}

func TestFilesOtherShareExcluded(t *testing.T) {
	c, _ := newTestCore(t)
	s1, s2 := mustShare(t, c, 1), mustShare(t, c, 2)
	f1, f2 := testFile(1), testFile(2)
	mustWrite(t, c, s1, f1, 0, pattern(4<<10, 1))
	mustWrite(t, c, s2, f2, 0, pattern(4<<10, 2))
	if got := filesOf(c, s1); !slices.Equal(got, []core.FileID{f1}) {
		t.Fatalf("share 1 Files = %v, want %v", got, f1)
	}
	if _, err := c.Extents(s2, f1); err == nil {
		t.Fatal("Extents of another share's file returned no error")
	} else {
		wantOtherShare(t, err, f1, 2, 1)
	}
	// Settle leaves another share's file untouched.
	d, err := c.Deallocate(context.Background(), s1, f1, ex(0, 2<<10))
	if err != nil {
		t.Fatal(err)
	}
	c.Settle(s2, f1, d)
	if got := sinceAll(t, c, s1, f1, core.Version{}); !slices.ContainsFunc(got, func(ch core.Change) bool { return ch.Kind == core.ChangeDeallocate }) {
		t.Fatalf("another share's Settle dropped the marker: %+v", got)
	}
	// A closed handle: Extents errors, Files yields nothing, Settle is a no-op.
	if err := s1.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Extents(s1, f1); !errors.Is(err, core.ErrClosed) {
		t.Fatalf("Extents on a closed handle = %v, want ErrClosed", err)
	}
	if got := c.Files(s1); len(got) != 0 {
		t.Fatalf("Files on a closed handle = %v", got)
	}
	c.Settle(s1, f1, d)
}
