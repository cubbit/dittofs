package reclaim_test

import (
	"errors"
	"testing"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/reclaim"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/segment"
)

const (
	// sealMax is the footprint maximum the limit tests run a journal at: a
	// few segments past what the spare pool and one active segment hold, so
	// the journal sits at its limit once the pool has settled. The pool keeps
	// a headroom spare per stream.
	sealMax = (3+copyStreams)*copySegCap + copySegCap/2
	// sealHeadroom is the headroom the half test runs: eight records, whose
	// drawing side of four goes below half after three draws.
	sealHeadroom = 8
)

// atLimit fails the test unless the fixture's journal is at its limit, the
// state the headroom is drawn in: a footprint plus a segment cap past the
// maximum.
func atLimit(t *testing.T, p *passFixture) {
	t.Helper()
	st := p.f.core.Stats()
	if st.UsedBytes+copySegCap <= st.MaxBytes {
		t.Fatalf("precondition: the journal holds %d of %d bytes, want it at its limit (footprint plus a segment cap of %d past the maximum)", st.UsedBytes, st.MaxBytes, copySegCap)
	}
}

// TestShareSeal_AtLimit is a share-scoped pass at the journal's limit: the
// share's unreclaimed bytes sit in an active segment no selection offers, the
// pass seals it, selects it and returns its storage. The marker draws on the
// headroom: one draw, and the pass is not refused by the share's limit.
func TestShareSeal_AtLimit(t *testing.T) {
	p := &passFixture{f: newCopyFixtureBounded(t, nil, sealMax, copyHeadroomRecords)}
	file := copyFile(1)
	rec, _, _ := p.f.plant(t, copyTag, file, []byte("unreclaimed bytes in an active segment"), 0)
	p.f.waitSpares(t)
	atLimit(t, p)
	if st, _ := p.f.set.State(rec.Segment); st != segment.Active {
		t.Fatalf("precondition: segment %d is in state %v, want it Active", rec.Segment, st)
	}
	before := p.f.core.Stats()

	freed, err := p.f.core.Repack(t.Context(), copySegCap, reclaim.Scope{Share: copyTag})
	if err != nil {
		t.Fatalf("Repack of the share: %v", err)
	}

	if !p.setGone(rec.Segment) {
		t.Errorf("segment %d is still in the set after a share-scoped pass, want it sealed, selected and retired", rec.Segment)
	}
	if freed <= 0 {
		t.Errorf("the pass freed %d bytes, want the sealed segment's storage back", freed)
	}
	if got := p.f.core.Stats().Counters.HeadroomDraws - before.Counters.HeadroomDraws; got != 1 {
		t.Errorf("headroom draws rose by %d, want 1: the seal marker draws on the headroom", got)
	}
}

// TestShareSeal_HeadroomAtHalf is the same pass once the headroom is below
// half: a seal admitted as a class that frees nothing, or against the share's
// limit, would be refused here, and the pass would free nothing.
func TestShareSeal_HeadroomAtHalf(t *testing.T) {
	p := &passFixture{f: newCopyFixtureBounded(t, nil, sealMax, sealHeadroom)}
	// Three seals draw three records of the four the drawing side holds,
	// which leaves it below half.
	for round := range 4 {
		file := copyFile(1)
		rec, _, _ := p.f.plant(t, copyTag, file, []byte("unreclaimed bytes in an active segment"), 0)
		p.f.waitSpares(t)
		atLimit(t, p)
		if _, err := p.f.core.Repack(t.Context(), copySegCap, reclaim.Scope{Share: copyTag}); err != nil {
			t.Fatalf("round %d: Repack of the share: %v", round, err)
		}
		if !p.setGone(rec.Segment) {
			t.Fatalf("round %d: segment %d is still in the set, want the share's active segment sealed and retired with %d draws spent", round, rec.Segment, p.f.core.Stats().Counters.HeadroomDraws)
		}
		p.f.waitSpares(t)
	}
	if got := p.f.core.Stats().Counters.HeadroomDraws; got != 4 {
		t.Errorf("headroom draws are %d, want 4: one per seal", got)
	}
}

// TestShareSeal_OtherScopesSealNothing is a zero-scope pass and a
// segment-set pass: neither seals the active segment.
func TestShareSeal_OtherScopesSealNothing(t *testing.T) {
	for name, scope := range map[string]func(core.SegmentID) reclaim.Scope{
		"zero": func(core.SegmentID) reclaim.Scope { return reclaim.Scope{} },
		"segment set": func(seg core.SegmentID) reclaim.Scope {
			return reclaim.Scope{Share: copyTag, Segments: []core.SegmentID{seg}}
		},
	} {
		t.Run(name, func(t *testing.T) {
			p := &passFixture{f: newCopyFixtureBounded(t, nil, sealMax, copyHeadroomRecords)}
			rec, _, _ := p.f.plant(t, copyTag, copyFile(1), []byte("unreclaimed bytes in an active segment"), 0)
			p.f.waitSpares(t)
			before := p.f.core.Stats().Counters.HeadroomDraws

			if _, err := p.f.core.Repack(t.Context(), copySegCap, scope(rec.Segment)); err != nil {
				t.Fatalf("Repack: %v", err)
			}

			if st, err := p.f.set.State(rec.Segment); err != nil || st != segment.Active {
				t.Errorf("segment %d is in state %v (%v) after a %s pass, want it still Active", rec.Segment, st, err, name)
			}
			if got := p.f.core.Stats().Counters.HeadroomDraws; got != before {
				t.Errorf("headroom draws moved from %d to %d, want a pass that seals nothing to draw nothing", before, got)
			}
		})
	}
}

// TestSealer_OnePerStreamUntilRetired seals a share's active segment, then
// asks again while that segment is still in the set: the stream has rolled
// onto a new active segment holding the share's bytes, and nothing seals it
// until the first is retired.
func TestSealer_OnePerStreamUntilRetired(t *testing.T) {
	p := &passFixture{f: newCopyFixture(t)}
	file := copyFile(1)
	sl := reclaim.NewSealer(p.f.core.Streams(), p.f.set, nil, nil, nil)

	first, _, _ := p.f.plant(t, copyTag, file, []byte("first"), 0)
	if n := sl.SealShare(copyTag); n != 1 {
		t.Fatalf("the first SealShare sealed %d segments, want 1", n)
	}
	if st, _ := p.f.set.State(first.Segment); st != segment.Sealed {
		t.Fatalf("segment %d is in state %v after the seal, want Sealed", first.Segment, st)
	}

	p.f.waitSpares(t)
	second, _, _ := p.f.plant(t, copyTag, file, []byte("second"), 0)
	if second.Segment == first.Segment {
		t.Fatalf("the second write landed in segment %d again, want the stream rolled", second.Segment)
	}
	if n := sl.SealShare(copyTag); n != 0 {
		t.Errorf("SealShare sealed %d segments while the first was still in the set, want 0", n)
	}
	if st, _ := p.f.set.State(second.Segment); st != segment.Active {
		t.Errorf("segment %d is in state %v, want it still Active", second.Segment, st)
	}

	if _, err := p.f.core.Retire(first.Segment, segment.NotHeld); err != nil {
		t.Fatalf("Retire of segment %d: %v", first.Segment, err)
	}
	if n := sl.SealShare(copyTag); n != 1 {
		t.Errorf("SealShare sealed %d segments after the first was retired, want 1", n)
	}
}

// TestSealOpen_RefusesASegmentTheStreamLeft is a seal aimed at a segment the
// stream rolled away from: it is refused and seals nothing, never the
// successor the stream holds now.
func TestSealOpen_RefusesASegmentTheStreamLeft(t *testing.T) {
	p := &passFixture{f: newCopyFixture(t)}
	file := copyFile(1)
	stream := p.f.core.Streams().For(file)

	first, _, _ := p.f.plant(t, copyTag, file, []byte("first"), 0)
	p.f.sealSource(t, file)
	p.f.waitSpares(t)
	second, _, _ := p.f.plant(t, copyTag, file, []byte("second"), 0)

	if _, err := stream.SealOpen(first.Segment, true); !errors.Is(err, segment.ErrNotOpenSegment) {
		t.Fatalf("SealOpen of the segment the stream left: error %v, want %v", err, segment.ErrNotOpenSegment)
	}
	if st, _ := p.f.set.State(second.Segment); st != segment.Active {
		t.Fatalf("the successor is in state %v after a refused seal of its predecessor, want Active", st)
	}
	if _, err := stream.SealOpen(second.Segment, true); err != nil {
		t.Fatalf("SealOpen of the open segment: %v", err)
	}
}
