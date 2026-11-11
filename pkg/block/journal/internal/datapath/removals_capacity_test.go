package datapath

import (
	"context"
	"testing"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/capacity"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/format"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/index"
	"github.com/cubbit/dittofs/pkg/block/journal/journaltest/kit"
)

// removalAtTheLimit returns a core at the journal's limit with a share on it,
// as atTheLimit does, and the file a warm write gave an entry of its own.
func removalAtTheLimit(t *testing.T) (*Core, *Share, core.FileID) {
	t.Helper()
	c, _, s, f := atTheLimit(t)
	return c, s, f
}

// TestRemovalDrainsTheHeadroom checks removals of content the journal does
// not hold draw the headroom down: the record-count admission counts each
// while the journal is at its limit. It pins the fixture the refusals below
// rely on.
func TestRemovalDrainsTheHeadroom(t *testing.T) {
	ctx := context.Background()
	c, s, _ := removalAtTheLimit(t)
	draws := c.headroom.Draws()
	const n = 5
	for i := 0; i < n; i++ {
		if _, err := c.Deallocate(ctx, s, testFile(9), core.Extent{Off: 2 * int64(i), Len: 1}); err != nil {
			t.Fatalf("deallocate %d of nothing held: %v", i+1, err)
		}
	}
	if got := c.headroom.Draws(); got != draws+n {
		t.Fatalf("headroom_draws = %d after %d removals, want %d", got, n, draws+n)
	}
}

// drainHeadroom draws the headroom down to its exhaustion with removals of a
// file the journal does not hold: they free nothing, so every one that is
// admitted dates the headroom. It returns once the next such removal is
// refused naming the headroom, at the edge where the open reserve is all that
// is left.
func drainHeadroom(t *testing.T, c *Core, s *Share) {
	t.Helper()
	ctx := context.Background()
	reserve := capacity.OpenReserveRecords(acceptanceRecords)
	drain := testFile(9)
	for i := 0; c.headroom.Draws() < uint64(acceptanceRecords-reserve); i++ {
		if _, err := c.Deallocate(ctx, s, drain, core.Extent{Off: 2 * int64(i), Len: 1}); err != nil {
			t.Fatalf("deallocate %d of nothing held, above the reserve: %v", i+1, err)
		}
	}
	if !c.headroom.BelowHalf() {
		t.Fatal("the drained headroom is not below half")
	}
}

// TestUnheldRemovalRefusedAtTheExhaustedHeadroom checks that a removal of
// content the journal does not hold is refused naming the headroom once it is
// exhausted — never naming the share or the journal — drawing no version and
// no headroom record.
func TestUnheldRemovalRefusedAtTheExhaustedHeadroom(t *testing.T) {
	ctx := context.Background()
	c, s, _ := removalAtTheLimit(t)
	drainHeadroom(t, c, s)
	next, draws := c.counters.NextVersion(), c.headroom.Draws()
	_, err := c.Deallocate(ctx, s, testFile(9), core.Extent{Off: 1 << 20, Len: 1})
	ns := refused(t, err, core.LimitHeadroom)
	if ns.Limit == core.LimitShare || ns.Limit == core.LimitJournal {
		t.Fatalf("the removal was refused naming %s, want the headroom", ns.Limit)
	}
	if got := c.counters.NextVersion(); got != next {
		t.Fatalf("a headroom refusal drew a version: next is %v, was %v", got, next)
	}
	if got := c.headroom.Draws(); got != draws {
		t.Fatalf("a headroom refusal drew %d headroom records", got-draws)
	}
}

// TestRemovalOfHeldContentAdmittedAtTheExhaustedHeadroom checks that a
// truncate, a deallocate and a delete of content the journal holds are admitted
// at an exhausted headroom and leave their markers: a removal of held content
// frees space, so only a removal of content the journal does not hold may be
// refused naming the headroom. Neither the share's limit nor the journal's
// maximum may refuse one either.
func TestRemovalOfHeldContentAdmittedAtTheExhaustedHeadroom(t *testing.T) {
	ctx := context.Background()
	for name, call := range map[string]func(*Core, *Share, core.FileID) (core.Version, error){
		"truncate": func(c *Core, s *Share, f core.FileID) (core.Version, error) {
			return c.Truncate(ctx, s, f, 0)
		},
		"deallocate": func(c *Core, s *Share, f core.FileID) (core.Version, error) {
			return c.Deallocate(ctx, s, f, core.Extent{Off: 0, Len: 8})
		},
		"delete": func(c *Core, s *Share, f core.FileID) (core.Version, error) {
			return c.Delete(ctx, s, f)
		},
	} {
		t.Run(name, func(t *testing.T) {
			c, s, f := removalAtTheLimit(t)
			drainHeadroom(t, c, s)
			// The file the warm write filled still holds content, so the
			// removal frees space and is admitted.
			v, err := call(c, s, f)
			if err != nil {
				t.Fatalf("%s of held content at an exhausted headroom = %v, want it admitted", name, err)
			}
			if v == (core.Version{}) {
				t.Fatalf("%s of held content drew no version", name)
			}
			want := map[string]core.ChangeKind{
				"truncate":   core.ChangeTruncate,
				"deallocate": core.ChangeDeallocate,
				"delete":     core.ChangeDelete,
			}[name]
			if m := markerOfKind(t, c, f, want); m.Version != v {
				t.Fatalf("%s marker = %+v, want it at the returned version %v", name, m, v)
			}
		})
	}
}

// TestCloneTargetRefusedBelowHalfTheHeadroom checks a clone target, which
// frees nothing, is refused naming the headroom once less than half of it
// remains — over content the journal holds as over content it does not — while
// a deallocate that frees space is admitted.
func TestCloneTargetRefusedBelowHalfTheHeadroom(t *testing.T) {
	ctx := context.Background()
	c, s, f := removalAtTheLimit(t)
	for i := 0; i < 11; i++ {
		if _, err := c.Deallocate(ctx, s, f, core.Extent{Off: 2 * int64(i), Len: 1}); err != nil {
			t.Fatalf("deallocate %d of nothing held: %v", i+1, err)
		}
	}
	if !c.headroom.BelowHalf() {
		t.Fatal("eleven removals left the headroom above half")
	}
	next := c.counters.NextVersion()
	_, err := c.CloneTarget(ctx, s, testFile(2), core.CloneSpec{DstOff: 0, Len: 8})
	_ = refused(t, err, core.LimitHeadroom)
	// Over content the journal holds too: a clone target frees nothing, so it
	// keeps the refusal the removals that free space are spared.
	_, err = c.CloneTarget(ctx, s, f, core.CloneSpec{DstOff: 0, Len: 8})
	_ = refused(t, err, core.LimitHeadroom)
	if got := c.counters.NextVersion(); got != next {
		t.Fatalf("a headroom refusal drew a version: next is %v, was %v", got, next)
	}
	if _, err := c.Deallocate(ctx, s, f, core.Extent{Off: 1 << 20, Len: 1}); err != nil {
		t.Fatalf("a deallocate below half the headroom = %v, want it admitted", err)
	}
}

// TestRemovalRefusedByTheIndexDrawsNoVersion checks a removal that would
// split a held extent is refused naming the index once the count stands at
// the removal ceiling, counts one index refusal and draws no version, while a
// removal that splits nothing is admitted at the same count.
func TestRemovalRefusedByTheIndexDrawsNoVersion(t *testing.T) {
	ctx := context.Background()
	const limit = 80
	const ceiling = limit + limit/8
	c, seam := newTestCore(t, func(o *coreOptions) { o.extentLimit = limit })
	s := mustShare(t, c, 1)
	f := testFile(1)
	mustWrite(t, c, s, f, 0, make([]byte, 3*4096))
	// Lift the count to the ceiling through removals that ask no admission of
	// their own: every admitted write above the bound would be refused, and
	// the ceiling is where the allowance ends.
	other := testFile(2)
	for k := int64(0); len(c.Index().FileEntries(other)) < ceiling; k++ {
		at := index.RecordAt{Segment: core.SegmentID(1 + k/16), Offset: uint32(k%16) * 8}
		if _, err := c.Index().ApplyRemoval(s.Tag, other, core.ChangeDeallocate, core.Extent{Off: k * 8, Len: 8}, ver(uint64(k)+1), core.Seq(uint64(k)+1), at, nil); err != nil {
			t.Fatalf("padding the count: %v", err)
		}
	}
	if got := c.Index().Refused(); got != 0 {
		t.Fatalf("padding counted %d index refusals", got)
	}
	next, writes := c.counters.NextVersion(), seam.Calls(kit.OpWrite)
	_, err := c.Deallocate(ctx, s, f, core.Extent{Off: 4096, Len: 4096})
	_ = refused(t, err, core.LimitIndex)
	if got := c.Index().Refused(); got != 1 {
		t.Fatalf("index_refusals = %d after the refused removal, want 1", got)
	}
	if got := c.counters.NextVersion(); got != next {
		t.Fatalf("an index refusal drew a version: next is %v, was %v", got, next)
	}
	if got := seam.Calls(kit.OpWrite); got != writes {
		t.Fatalf("the refused removal made %d record writes", got-writes)
	}
	// A removal that splits nothing is admitted at the same count.
	if _, err := c.Deallocate(ctx, s, f, core.Extent{Off: 0, Len: 4096}); err != nil {
		t.Fatalf("a narrowing deallocate at the ceiling = %v, want it admitted", err)
	}
	if got := c.Index().Refused(); got != 1 {
		t.Fatalf("index_refusals = %d after an admitted removal, want 1", got)
	}
}

// TestRemovalAppliesPastTheBoundWhenItsPreCheckWonTheRace parks a removal
// after its pre-check but before its record is written, raises the index's
// count past the removal ceiling from another file meanwhile, and releases
// it: the record is on disk, so the removal is applied past the bound and
// leaves its marker, rather than refused for a record a rebuild would apply.
func TestRemovalAppliesPastTheBoundWhenItsPreCheckWonTheRace(t *testing.T) {
	ctx := context.Background()
	const limit = 80
	const ceiling = limit + limit/8
	c, seam := newTestCore(t, func(o *coreOptions) { o.extentLimit = limit })
	s := mustShare(t, c, 1)
	f := testFile(1)
	mustWrite(t, c, s, f, 0, make([]byte, 3*4096))

	hold := seam.HoldFirst(t, kit.OpWrite, "*")
	done := make(chan struct {
		v   core.Version
		err error
	}, 1)
	go func() {
		v, err := c.Deallocate(ctx, s, f, core.Extent{Off: 4096, Len: 4096})
		done <- struct {
			v   core.Version
			err error
		}{v, err}
	}()
	within(t, hold.Reached(), "the removal up to its record write")

	// The rival: another file's removals raise the count to the ceiling while
	// the removal above is past its pre-check. They apply without admission,
	// so the padding itself is refused by nothing.
	other := testFile(2)
	for k := int64(0); len(c.Index().FileEntries(other)) < ceiling; k++ {
		at := index.RecordAt{Segment: core.SegmentID(100 + k/16), Offset: uint32(k%16) * 8}
		if _, err := c.Index().ApplyRemoval(s.Tag, other, core.ChangeDeallocate, core.Extent{Off: k * 8, Len: 8}, ver(uint64(k)+1), core.Seq(uint64(k)+1), at, nil); err != nil {
			hold.Release()
			t.Fatalf("padding the count: %v", err)
		}
	}
	hold.Release()

	got := within(t, done, "the removal whose pre-check won the race")
	if got.err != nil {
		t.Fatalf("the removal = %v, want it applied past the bound, with its record already on disk", got.err)
	}
	if m := markerOfKind(t, c, f, core.ChangeDeallocate); m.Extent != (core.Extent{Off: 4096, Len: 4096}) || m.Version != got.v {
		t.Fatalf("marker = %+v, want extent (4096, 4096) at %v", m, got.v)
	}
	if rec := removalOf(t, seam, format.KindDeallocate); rec.Header.Version != got.v {
		t.Fatalf("the record on disk carries %v, the call returned %v", rec.Header.Version, got.v)
	}
}
