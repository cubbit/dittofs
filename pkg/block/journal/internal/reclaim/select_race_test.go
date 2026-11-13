package reclaim

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/index"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/unread"
	"github.com/cubbit/dittofs/pkg/block/journal/journaltest/kit"
)

// runPasses runs one Select per goroutine, all released together, and returns
// the sorted union of what they were handed.
func runPasses(sels []*Selector, limit int) []core.SegmentID {
	got := make([][]core.SegmentID, len(sels))
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range sels {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			got[i] = ids(sels[i].Select(Scope{}, limit))
		}()
	}
	close(start)
	wg.Wait()
	var union []core.SegmentID
	for _, g := range got {
		union = append(union, g...)
	}
	slices.Sort(union)
	return union
}

func TestTwoPassesNeverShare(t *testing.T) {
	const passes, segs = 8, 40
	seed := func(f *selFixture) []core.SegmentID {
		var want []core.SegmentID
		for range segs {
			want = append(want, f.sealedSeg(t, map[core.ShareTag]int64{selTag: 100}))
		}
		return want
	}

	t.Run("goroutines sharing one selector", func(t *testing.T) {
		f := newSelFixture(t)
		want := seed(f)
		s := f.over(unread.New(nil), NewPins())
		sels := make([]*Selector, passes)
		for i := range sels {
			sels[i] = s
		}
		if got := runPasses(sels, segs); !slices.Equal(got, want) {
			t.Fatalf("union of the passes = %v, want each of %v exactly once", got, want)
		}
	})

	t.Run("several selectors over one set and no pins", func(t *testing.T) {
		f := newSelFixture(t)
		want := seed(f)
		sels := make([]*Selector, passes)
		for i := range sels {
			sels[i] = f.selector()
		}
		if got := runPasses(sels, segs); !slices.Equal(got, want) {
			t.Fatalf("union of the passes = %v, want each of %v exactly once", got, want)
		}
	})
}

func TestPinRefusedWhileSelectRuns(t *testing.T) {
	const segs = 40
	f := newSelFixture(t)
	var all []core.SegmentID
	for range segs {
		all = append(all, f.sealedSeg(t, map[core.ShareTag]int64{selTag: 100}))
	}
	pins := NewPins()
	s := f.over(unread.New(nil), pins)

	var wg sync.WaitGroup
	var got []core.SegmentID
	pinned := make([]bool, len(all))
	wg.Add(2)
	go func() { defer wg.Done(); got = ids(s.Select(Scope{}, segs)) }()
	go func() {
		defer wg.Done()
		for i, id := range all {
			pinned[i] = pins.Pin(id)
		}
	}()
	wg.Wait()

	// Every segment is either selected or pinned, never both, and never neither.
	for i, id := range all {
		sel := slices.Contains(got, id)
		if sel == pinned[i] {
			t.Errorf("segment %d: selected = %v, pinned = %v, want exactly one", id, sel, pinned[i])
		}
	}
}

func TestSecondSelectorOverPinsPanics(t *testing.T) {
	f := newSelFixture(t)
	pins := NewPins()
	f.over(unread.New(nil), pins)
	defer func() {
		if recover() == nil {
			t.Fatal("a second NewSelector over wired Pins returned, want a panic")
		}
	}()
	f.over(unread.New(nil), pins)
}

func TestSelectedAndPin(t *testing.T) {
	f := newSelFixture(t)
	a := f.sealedSeg(t, map[core.ShareTag]int64{selTag: 100})
	pins := NewPins()
	s := f.over(unread.New(nil), pins)

	if s.Selected(a) {
		t.Fatalf("Selected(%d) before any selection = true, want false", a)
	}
	if got, want := ids(s.Select(Scope{}, 10)), []core.SegmentID{a}; !slices.Equal(got, want) {
		t.Fatalf("candidates = %v, want %v", got, want)
	}
	if !s.Selected(a) {
		t.Fatalf("Selected(%d) after Select = false, want true", a)
	}
	if pins.Pin(a) || pins.Pinned(a) {
		t.Fatalf("Pin(%d) of a selected segment held a pin, want it refused", a)
	}
	s.Done(a)
	if s.Selected(a) {
		t.Fatalf("Selected(%d) after Done = true, want false", a)
	}
	if !pins.Pin(a) || !pins.Pinned(a) {
		t.Fatalf("Pin(%d) after Done refused, want it held", a)
	}

	// A claim another path holds is not this selector's.
	pins.Unpin(a)
	if !f.set.Claim(a) {
		t.Fatalf("Claim(%d) = false, want true", a)
	}
	if s.Selected(a) {
		t.Fatalf("Selected(%d) under another holder's claim = true, want false", a)
	}
	s.Done(a) // not this selector's: must not release the other claim
	if f.set.Claim(a) {
		t.Fatalf("Done(%d) released a claim it did not take", a)
	}
	f.set.Release(a)

	// A retired segment is not selected, and a nil set pins nothing.
	if got := ids(s.Select(Scope{}, 10)); !slices.Equal(got, []core.SegmentID{a}) {
		t.Fatalf("candidates = %v, want %v", got, a)
	}
	if _, err := f.set.Retire(a); err != nil {
		t.Fatalf("Retire: %v", err)
	}
	if s.Selected(a) {
		t.Fatalf("Selected(%d) after retirement = true, want false", a)
	}
	s.Done(a)
	var none *Pins
	if none.Pin(a) || none.Pinned(a) {
		t.Fatalf("a nil *Pins pinned %d, want nothing", a)
	}
	none.Unpin(a)
}

func TestRetiredBetweenListAndClaim(t *testing.T) {
	f := newSelFixture(t)
	a := f.sealedSeg(t, map[core.ShareTag]int64{selTag: 100})
	b := f.sealedSeg(t, map[core.ShareTag]int64{selTag: 100})
	s := f.selector()
	s.SetAfterSnapshot(func() {
		if _, err := f.set.Retire(a); err != nil {
			t.Errorf("Retire(%d): %v", a, err)
		}
	})
	if got, want := ids(s.Select(Scope{}, 10)), []core.SegmentID{b}; !slices.Equal(got, want) {
		t.Fatalf("candidates = %v, want %v: %d retired after the listing", got, want, a)
	}
	if s.Selected(a) {
		t.Fatalf("Selected(%d) = true, want false: its claim never took", a)
	}
	for _, r := range f.set.Snapshot() {
		if r.ID == a {
			t.Fatalf("segment %d still in the set after Retire", a)
		}
	}
	if f.set.Claim(b) {
		t.Fatalf("Claim(%d) = true, want false: the pass still holds it", b)
	}
}

// failedUnread marks the range of seg at off unread through a read that fails
// every attempt, the clock advancing as a slow read's would.
func failedUnread(t *testing.T, un *unread.Set, clk kit.DrivenClock, seg core.SegmentID, off int64, k unread.Kind, file core.FileID) {
	t.Helper()
	ref := unread.Ref{SegmentRange: core.SegmentRange{Segment: seg, Off: off, Len: 50}, Kind: k, File: file}
	_, err := un.Try(context.Background(), ref, func() ([]byte, error) {
		clk.Advance(time.Second)
		return nil, errors.New("the read failed")
	})
	if err == nil || !un.Suspect(seg) {
		t.Fatalf("Try of a failing read = %v, suspect %v, want an error and a suspect segment", err, un.Suspect(seg))
	}
}

func TestSuspectUnreadMixed(t *testing.T) {
	const at, ln = 100, 50
	// supersede writes file 1's extent again from another segment, so the
	// record at offset at of seg is no longer the held one.
	supersede := func(t *testing.T, f *selFixture) {
		t.Helper()
		other := f.sealedSeg(t, map[core.ShareTag]int64{selTag: 100})
		p := index.Placed{Extent: core.Extent{Len: ln}, Record: index.RecordAt{Segment: other}, Version: selVer(2)}
		if _, err := f.ix.WriteRetain(selTag, selFid(1), p, 2); err != nil {
			t.Fatalf("WriteRetain: %v", err)
		}
	}
	seed := func(t *testing.T, f *selFixture) core.SegmentID {
		t.Helper()
		seg := f.sealedSeg(t, map[core.ShareTag]int64{selTag: 100})
		p := index.Placed{Extent: core.Extent{Len: ln}, Record: index.RecordAt{Segment: seg, Offset: at}, Version: selVer(1)}
		if _, err := f.ix.WriteRetain(selTag, selFid(1), p, 1); err != nil {
			t.Fatalf("WriteRetain: %v", err)
		}
		return seg
	}

	t.Run("a dead content record does not excuse a live header-only one", func(t *testing.T) {
		f := newSelFixture(t)
		clk := kit.NewStopwatchClock()
		un := unread.New(clk)
		seg := seed(t, f)
		supersede(t, f)
		if _, _, err := f.ix.LiftSuperseded(selFid(1), selVer(2), core.Extent{Len: ln}); err != nil {
			t.Fatalf("LiftSuperseded: %v", err)
		}
		failedUnread(t, un, clk, seg, at, unread.Content, selFid(1))
		failedUnread(t, un, clk, seg, at+1000, unread.HeaderOnly, selFid(1))
		if got := ids(f.over(un, nil).Select(Scope{Segments: []core.SegmentID{seg}}, 10)); len(got) != 0 {
			t.Fatalf("candidates = %v, want none: the header-only record is unread", got)
		}
	})

	t.Run("a retained content record still pins", func(t *testing.T) {
		f := newSelFixture(t)
		clk := kit.NewStopwatchClock()
		un := unread.New(clk)
		seg := seed(t, f)
		supersede(t, f) // superseded but not lifted: retained, no longer held
		if !f.ix.Names(selFid(1), index.RecordAt{Segment: seg, Offset: at}) {
			t.Fatal("the index does not name the retained record; the fixture is wrong")
		}
		failedUnread(t, un, clk, seg, at, unread.Content, selFid(1))
		if got := ids(f.over(un, nil).Select(Scope{Segments: []core.SegmentID{seg}}, 10)); len(got) != 0 {
			t.Fatalf("candidates = %v, want none: an unread retained record still backs a piece", got)
		}
	})
}

// TestPinRefusedOnceClaimRecorded pins the order claim runs in: the segment is
// recorded as selected before its pin is read, so a pin landing between the two
// is refused and a pin and a claim cannot both stand.
func TestPinRefusedOnceClaimRecorded(t *testing.T) {
	f := newSelFixture(t)
	a := f.sealedSeg(t, map[core.ShareTag]int64{selTag: 100})
	pins := NewPins()
	s := f.over(unread.New(nil), pins)
	var pinned, fired bool
	s.SetAfterClaim(func(id core.SegmentID) {
		fired = true
		pinned = pins.Pin(id)
	})

	got := ids(s.Select(Scope{}, 10))
	if !fired {
		t.Fatal("the after-claim hook never ran, want it run once per claim")
	}
	if pinned {
		t.Errorf("Pin(%d) between the claim and the pin read = true, want false: the segment is already selected", a)
	}
	if want := []core.SegmentID{a}; !slices.Equal(got, want) {
		t.Errorf("Select = %v, want %v: a refused pin leaves the claim standing", got, want)
	}
}

// TestPinBeforeClaimRefused pins claim's own pin read: a pin landing after a
// segment passed Select's filter and before its claim is refused by the claim.
func TestPinBeforeClaimRefused(t *testing.T) {
	f := newSelFixture(t)
	a := f.sealedSeg(t, map[core.ShareTag]int64{selTag: 100})
	b := f.sealedSeg(t, map[core.ShareTag]int64{selTag: 100})
	f.live(t, b, 1, 10) // b sorts after a
	pins := NewPins()
	s := f.over(unread.New(nil), pins)
	pinnedB := false
	s.SetAfterClaim(func(id core.SegmentID) {
		if id == a {
			pinnedB = pins.Pin(b)
		}
	})
	got := ids(s.Select(Scope{}, 10))
	if !pinnedB {
		t.Fatalf("Pin(%d) before its claim = false, want true", b)
	}
	if slices.Contains(got, b) {
		t.Fatalf("Select = %v, want %d left out: it was pinned before its claim", got, b)
	}
}
