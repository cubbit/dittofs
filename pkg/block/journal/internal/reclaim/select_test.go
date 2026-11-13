package reclaim

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/index"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/unread"
	"github.com/cubbit/dittofs/pkg/block/journal/journaltest/kit"
)

const shareB = core.ShareTag(2)

// sealedSeg takes a new segment into the fixture's set, accounts one record
// per (selTag, bytes) of tags to it and seals it. Its live bytes are planted
// separately by live.
func (f *selFixture) sealedSeg(t *testing.T, tags map[core.ShareTag]int64) core.SegmentID {
	t.Helper()
	seg := f.takeIn(t)
	for tg, n := range tags {
		if err := f.set.AddRecord(seg, tg, n, 0); err != nil {
			t.Fatalf("AddRecord: %v", err)
		}
	}
	if err := f.set.Seal(seg, false); err != nil {
		t.Fatalf("Seal: %v", err)
	}
	return seg
}

// live plants n held bytes in seg, under a file of its own.
func (f *selFixture) live(t *testing.T, seg core.SegmentID, file byte, n int64) {
	t.Helper()
	p := index.Placed{
		Extent:  core.Extent{Off: 0, Len: n},
		Record:  index.RecordAt{Segment: seg},
		Version: selVer(uint64(file)),
	}
	if _, err := f.ix.WriteRetain(selTag, selFid(file), p, core.Seq(file)); err != nil {
		t.Fatalf("WriteRetain: %v", err)
	}
}

// retainedOnly plants n bytes in seg that are retained and not held: written
// there, then superseded from a segment of their own and not lifted.
func (f *selFixture) retainedOnly(t *testing.T, seg core.SegmentID, file byte, n int64) core.SegmentID {
	t.Helper()
	f.live(t, seg, file, n)
	other := f.sealedSeg(t, map[core.ShareTag]int64{selTag: 100})
	p := index.Placed{Extent: core.Extent{Len: n}, Record: index.RecordAt{Segment: other}, Version: selVer(uint64(file) + 100)}
	if _, err := f.ix.WriteRetain(selTag, selFid(file), p, core.Seq(file)+100); err != nil {
		t.Fatalf("WriteRetain: %v", err)
	}
	return other
}

func (f *selFixture) selector() *Selector {
	return NewSelector(f.set, f.ix, unread.New(nil), nil, nil)
}

func ids(cs []Candidate) []core.SegmentID {
	out := make([]core.SegmentID, len(cs))
	for i, c := range cs {
		out[i] = c.ID
	}
	return out
}

func TestSelectOrder(t *testing.T) {
	f := newSelFixture(t)
	if err := f.set.Seal(f.seg, false); err != nil {
		t.Fatalf("Seal: %v", err)
	}
	// f.seg holds nothing, so it comes first; a, b and c hold 90, 10 and 50 live
	// of 100. d and e also hold 50, so c, d and e tie, and the lower id wins:
	// they come out in the order they were made.
	a := f.sealedSeg(t, map[core.ShareTag]int64{selTag: 100})
	b := f.sealedSeg(t, map[core.ShareTag]int64{selTag: 100})
	c := f.sealedSeg(t, map[core.ShareTag]int64{selTag: 100})
	d := f.sealedSeg(t, map[core.ShareTag]int64{selTag: 100})
	e := f.sealedSeg(t, map[core.ShareTag]int64{selTag: 100})
	f.live(t, a, 1, 90)
	f.live(t, b, 2, 10)
	f.live(t, c, 3, 50)
	f.live(t, d, 4, 50)
	f.live(t, e, 5, 50)

	// g holds no held bytes but 95 retained ones (written, then superseded
	// without lifting), so it counts as 95 live and sorts after a. The
	// superseding record sits in h, 95 held, which ties with g and comes later.
	g := f.sealedSeg(t, map[core.ShareTag]int64{selTag: 100})
	h := f.retainedOnly(t, g, 6, 95)

	sel := f.selector()
	got := sel.Select(Scope{}, 100)
	want := []core.SegmentID{f.seg, b, c, d, e, a, g, h}
	if !slices.Equal(ids(got), want) {
		t.Fatalf("order = %v, want %v (least live first, ties by id)", ids(got), want)
	}
	if got[1].Live != 10 || got[1].Unreclaimed != selCap-10 {
		t.Fatalf("candidate b = live %d unreclaimed %d, want 10 and %d", got[1].Live, got[1].Unreclaimed, selCap-10)
	}
	sel.Done(want...)
	if lim := sel.Select(Scope{}, 2); !slices.Equal(ids(lim), want[:2]) {
		t.Fatalf("max 2 returned %v, want %v", ids(lim), want[:2])
	}
}

func TestSelectSkipsActive(t *testing.T) {
	f := newSelFixture(t) // f.seg stays Active, holding nothing: sparsest
	a := f.sealedSeg(t, map[core.ShareTag]int64{selTag: 100})
	f.live(t, a, 1, 90)

	sel := f.selector()
	got := ids(sel.Select(Scope{}, 100))
	if want := []core.SegmentID{a}; !slices.Equal(got, want) {
		t.Fatalf("candidates = %v, want %v: an Active segment is never one", got, want)
	}
	sel.Done(got...)

	// An Ended segment is a candidate.
	e := f.takeIn(t)
	if err := f.set.End(e); err != nil {
		t.Fatalf("End: %v", err)
	}
	got = ids(sel.Select(Scope{}, 100))
	if want := []core.SegmentID{e, a}; !slices.Equal(got, want) {
		t.Fatalf("candidates = %v, want %v: an Ended segment is one", got, want)
	}
}

func TestSelectScopeShare(t *testing.T) {
	f := newSelFixture(t)
	if err := f.set.Seal(f.seg, false); err != nil {
		t.Fatalf("Seal: %v", err)
	}
	onlyB := f.sealedSeg(t, map[core.ShareTag]int64{shareB: 100}) // sparsest
	mixed := f.sealedSeg(t, map[core.ShareTag]int64{selTag: 20, shareB: 80})
	onlyA := f.sealedSeg(t, map[core.ShareTag]int64{selTag: 100})
	f.live(t, mixed, 1, 40)
	f.live(t, onlyA, 2, 90)
	f.live(t, onlyB, 3, 1)

	s := f.selector()
	// pass runs one selection and ends it, so the next sees every segment again.
	pass := func(sc Scope) []core.SegmentID {
		got := ids(s.Select(sc, 100))
		s.Done(got...)
		return got
	}
	if got, want := pass(Scope{Share: selTag}), []core.SegmentID{mixed, onlyA}; !slices.Equal(got, want) {
		t.Fatalf("share scope = %v, want %v", got, want)
	}
	if got, want := pass(Scope{Segments: []core.SegmentID{onlyA}}), []core.SegmentID{onlyA}; !slices.Equal(got, want) {
		t.Fatalf("segment scope = %v, want %v", got, want)
	}
	if got := pass(Scope{Share: selTag, Segments: []core.SegmentID{onlyB, onlyA}}); !slices.Equal(got, []core.SegmentID{onlyA}) {
		t.Fatalf("both scopes = %v, want only %v", got, onlyA)
	}
	if got, want := len(pass(Scope{})), 4; got != want {
		t.Fatalf("zero scope returned %d candidates, want %d", got, want)
	}
}

func TestSelectNoRead(t *testing.T) {
	f := newSelFixture(t)
	a := f.sealedSeg(t, map[core.ShareTag]int64{selTag: 100})
	f.live(t, a, 1, 50)
	ops := []string{kit.OpCreate, kit.OpOpen, kit.OpAllocate, kit.OpWrite, kit.OpRead, kit.OpReadDirect,
		kit.OpSync, kit.OpTruncate, kit.OpClose, kit.OpRename, kit.OpRemove, kit.OpList, kit.OpSyncDir}
	log := func() []int {
		out := make([]int, len(ops))
		for i, op := range ops {
			out[i] = f.seam.Calls(op)
		}
		return out
	}
	before := log()
	if got := f.selector().Select(Scope{Share: selTag}, 10); len(got) != 1 {
		t.Fatalf("Select returned %d candidates, want 1", len(got))
	}
	if after := log(); !slices.Equal(after, before) {
		t.Fatalf("storage calls %v after Select, want %v: a selection touches no storage", after, before)
	}
}

func TestSelectSkipsPinned(t *testing.T) {
	f := newSelFixture(t)
	a := f.sealedSeg(t, map[core.ShareTag]int64{selTag: 100})
	pins := NewPins()
	s := f.over(unread.New(nil), pins)

	if !pins.Pin(a) || !pins.Pinned(a) {
		t.Fatalf("Pin(%d) then Pinned = false, want true", a)
	}
	if got := ids(s.Select(Scope{}, 10)); len(got) != 0 {
		t.Fatalf("candidates = %v, want none: segment %d is pinned", got, a)
	}
	pins.Unpin(a)
	if pins.Pinned(a) {
		t.Fatalf("Pinned(%d) after Unpin = true, want false", a)
	}
	if got, want := ids(s.Select(Scope{}, 10)), []core.SegmentID{a}; !slices.Equal(got, want) {
		t.Fatalf("candidates = %v, want %v after Unpin", got, want)
	}
}

func TestSelectSkipsClaimed(t *testing.T) {
	f := newSelFixture(t)
	a := f.sealedSeg(t, map[core.ShareTag]int64{selTag: 100})
	if !f.set.Claim(a) {
		t.Fatalf("Claim(%d) = false, want true", a)
	}
	s := f.selector()
	if got := ids(s.Select(Scope{}, 10)); len(got) != 0 {
		t.Fatalf("candidates = %v, want none: segment %d is claimed", got, a)
	}
	f.set.Release(a)
	if got, want := ids(s.Select(Scope{}, 10)), []core.SegmentID{a}; !slices.Equal(got, want) {
		t.Fatalf("candidates = %v, want %v after Release", got, want)
	}
}

func TestSuspectUnread(t *testing.T) {
	const at, ln = 100, 50
	ctx := context.Background()
	// unreadAt marks the range at offset at of seg unread through a read that
	// fails every attempt, the clock advancing as a slow read's would.
	unreadAt := func(t *testing.T, un *unread.Set, clk kit.DrivenClock, seg core.SegmentID, k unread.Kind, file core.FileID) unread.Ref {
		t.Helper()
		ref := unread.Ref{SegmentRange: core.SegmentRange{Segment: seg, Off: at, Len: ln}, Kind: k, File: file}
		_, err := un.Try(ctx, ref, func() ([]byte, error) {
			clk.Advance(time.Second)
			return nil, errors.New("the read failed")
		})
		if err == nil || !un.Suspect(seg) {
			t.Fatalf("Try of a failing read = %v, suspect %v, want an error and a suspect segment", err, un.Suspect(seg))
		}
		return ref
	}
	// content seeds a segment whose record at offset at holds extent [0,ln)
	// of file 1.
	content := func(t *testing.T, f *selFixture) core.SegmentID {
		t.Helper()
		seg := f.sealedSeg(t, map[core.ShareTag]int64{selTag: 100})
		p := index.Placed{Extent: core.Extent{Len: ln}, Record: index.RecordAt{Segment: seg, Offset: at}, Version: selVer(1)}
		if _, err := f.ix.WriteRetain(selTag, selFid(1), p, 1); err != nil {
			t.Fatalf("WriteRetain: %v", err)
		}
		return seg
	}

	t.Run("content backing a held extent waits for its read", func(t *testing.T) {
		f := newSelFixture(t)
		clk := kit.NewStopwatchClock()
		un := unread.New(clk)
		seg := content(t, f)
		ref := unreadAt(t, un, clk, seg, unread.Content, selFid(1))
		s := f.over(un, nil)
		if got := ids(s.Select(Scope{}, 10)); len(got) != 0 {
			t.Fatalf("candidates = %v, want none: an unread record backs a held extent", got)
		}
		if _, err := un.Try(ctx, ref, func() ([]byte, error) { return nil, nil }); err != nil {
			t.Fatalf("Try of a succeeding read: %v", err)
		}
		if got, want := ids(s.Select(Scope{}, 10)), []core.SegmentID{seg}; !slices.Equal(got, want) {
			t.Fatalf("candidates = %v, want %v once the record reads", got, want)
		}
	})

	t.Run("content superseded by a later write is dead", func(t *testing.T) {
		f := newSelFixture(t)
		clk := kit.NewStopwatchClock()
		un := unread.New(clk)
		seg := content(t, f)
		unreadAt(t, un, clk, seg, unread.Content, selFid(1))
		// A later write covers the whole extent from another segment, and the
		// superseded content is lifted: nothing names the unread record, held
		// or retained.
		other := f.sealedSeg(t, map[core.ShareTag]int64{selTag: 100})
		p := index.Placed{Extent: core.Extent{Len: ln}, Record: index.RecordAt{Segment: other}, Version: selVer(2)}
		if _, err := f.ix.WriteRetain(selTag, selFid(1), p, 2); err != nil {
			t.Fatalf("WriteRetain: %v", err)
		}
		if _, err := f.ix.LiftSuperseded(selFid(1), selVer(2), core.Extent{Len: ln}); err != nil {
			t.Fatalf("LiftSuperseded: %v", err)
		}
		if f.ix.Names(selFid(1), index.RecordAt{Segment: seg, Offset: at}) {
			t.Fatal("the index still names the superseded record")
		}
		before := f.seam.Calls(kit.OpRead)
		got := ids(f.over(un, nil).Select(Scope{Segments: []core.SegmentID{seg}}, 10))
		if want := []core.SegmentID{seg}; !slices.Equal(got, want) {
			t.Fatalf("candidates = %v, want %v: a dead unread record pins nothing", got, want)
		}
		if after := f.seam.Calls(kit.OpRead); after != before {
			t.Fatalf("%d reads after Select, want %d: a selection reads nothing", after, before)
		}
	})

	t.Run("content of an unknown file always pins", func(t *testing.T) {
		f := newSelFixture(t)
		clk := kit.NewStopwatchClock()
		un := unread.New(clk)
		seg := content(t, f)
		unreadAt(t, un, clk, seg, unread.Content, core.FileID{})
		if got := ids(f.over(un, nil).Select(Scope{}, 10)); len(got) != 0 {
			t.Fatalf("candidates = %v, want none: an unread record of an unknown file cannot be shown dead", got)
		}
	})

	t.Run("a header-only record always pins", func(t *testing.T) {
		f := newSelFixture(t)
		clk := kit.NewStopwatchClock()
		un := unread.New(clk)
		seg := f.sealedSeg(t, map[core.ShareTag]int64{selTag: 100})
		unreadAt(t, un, clk, seg, unread.HeaderOnly, selFid(1))
		if got := ids(f.over(un, nil).Select(Scope{}, 10)); len(got) != 0 {
			t.Fatalf("candidates = %v, want none: a header-only record pins even when its file is known", got)
		}
	})
}

// over is a selector over the fixture with the unread set and pins the test
// holds, and no durability.
func (f *selFixture) over(un *unread.Set, pins *Pins) *Selector {
	return NewSelector(f.set, f.ix, un, nil, pins)
}
