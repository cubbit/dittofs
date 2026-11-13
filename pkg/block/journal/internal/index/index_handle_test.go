package index

import (
	"errors"
	"slices"
	"testing"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
)

const testTag core.ShareTag = 1

var testFile = fid(7)

// writeRetained publishes a client write through the handle and fails the
// test on a refusal.
func writeRetained(t *testing.T, ix *Index, p Placed) HeldDelta {
	t.Helper()
	d, err := ix.WriteRetain(testTag, testFile, p, core.Seq(p.Version.Counter))
	if err != nil {
		t.Fatalf("WriteRetain(%+v): %v", p, err)
	}
	return d
}

// written is p as writeRetained holds it: under the sequence number it gives
// a write, its version's counter.
func written(p Piece) Piece {
	p.Seq = core.Seq(p.Version.Counter)
	return p
}

// TestHandleWriteRetainServesNewestAndRetainsTheOlder: a write over an older
// one is what a read finds, and the older stays retained as superseded by it,
// still counted against its own segment, until the lift drops it.
func TestHandleWriteRetainServesNewestAndRetainsTheOlder(t *testing.T) {
	ix := New(1 << 20)
	writeRetained(t, ix, write(0, 4096, rec(1, 0), 1))
	writeRetained(t, ix, write(0, 4096, rec(2, 0), 2))

	got, ok := ix.Lookup(testFile, 100)
	if !ok || got.Version != ver(2) || got.Record != rec(2, 0) {
		t.Fatalf("Lookup = %+v, %v; want version 2 in record (2,0)", got, ok)
	}
	ret := ix.Retained(testFile)
	if len(ret) != 1 || ret[0].Version != ver(1) || ret[0].By != ver(2) {
		t.Fatalf("Retained = %+v, want v1 superseded by v2", ret)
	}
	if sb := ix.SegmentBytes(1); sb.Retained != 4096 || sb.Held != 0 {
		t.Fatalf("SegmentBytes(1) = %+v, want 4096 retained and nothing held", sb)
	}

	d, _, err := ix.LiftSuperseded(testFile, ver(2), whole)
	if err != nil {
		t.Fatalf("LiftSuperseded: %v", err)
	}
	if d.RetainedBytes != -4096 {
		t.Fatalf("lift delta RetainedBytes = %d, want -4096", d.RetainedBytes)
	}
	if got := ix.Retained(testFile); len(got) != 0 {
		t.Fatalf("Retained after the lift = %+v, want none", got)
	}
	if sb := ix.SegmentBytes(1); sb.Retained != 0 {
		t.Fatalf("SegmentBytes(1).Retained after the lift = %d, want 0", sb.Retained)
	}
	if sb := ix.SegmentBytes(2); sb.Held != 4096 {
		t.Fatalf("SegmentBytes(2).Held = %d, want 4096: the lift must not touch the superseder", sb.Held)
	}
}

// TestHandleMovesNameTheSegmentsTheyTouched: the removal, lift and loss moves
// return the distinct, ascending ids of the segments whose per-segment
// counters the move lowered, and only those; a segment whose bytes the move
// did not touch is absent from the list.
func TestHandleMovesNameTheSegmentsTheyTouched(t *testing.T) {
	// threeSegments writes one file as three records in three segments:
	// [0,4K) in segment 1, [4K,8K) in 2 and [8K,12K) in 3.
	threeSegments := func(t *testing.T) *Index {
		ix := New(1 << 20)
		writeRetained(t, ix, write(0, 4096, rec(1, 0), 1))
		writeRetained(t, ix, write(4096, 4096, rec(2, 0), 2))
		writeRetained(t, ix, write(8192, 4096, rec(3, 0), 3))
		return ix
	}
	// retainedUsed writes a superseded record in segment 1 whose byte a lift
	// drops, beside two untouched segments: 2 holds the superseder, 3 holds
	// content no move covers.
	retainedUsed := func(t *testing.T) *Index {
		ix := New(1 << 20)
		writeRetained(t, ix, write(0, 4096, rec(1, 0), 1))
		writeRetained(t, ix, write(0, 4096, rec(2, 0), 2))
		writeRetained(t, ix, write(8192, 4096, rec(3, 0), 3))
		return ix
	}
	tests := []struct {
		name  string
		setup func(t *testing.T) *Index
		run   func(t *testing.T, ix *Index) []core.SegmentID
		want  []core.SegmentID
		check func(t *testing.T, ix *Index)
	}{
		{
			name:  "ApplyRemoval",
			setup: threeSegments,
			run: func(t *testing.T, ix *Index) []core.SegmentID {
				_, segs, err := ix.ApplyRemoval(testTag, testFile, core.ChangeDeallocate, ext(0, 4096), ver(4), 4, rec(4, 0), nil)
				if err != nil {
					t.Fatalf("ApplyRemoval: %v", err)
				}
				return segs
			},
			want: []core.SegmentID{1},
			check: func(t *testing.T, ix *Index) {
				if sb := ix.SegmentBytes(1); sb.Held != 0 {
					t.Fatalf("ApplyRemoval lowered SegmentBytes(1).Held to %d, want 0", sb.Held)
				}
				if sb := ix.SegmentBytes(3); sb.Held != 4096 {
					t.Fatalf("ApplyRemoval moved SegmentBytes(3).Held to %d, want 4096", sb.Held)
				}
			},
		},
		{
			name:  "Lose",
			setup: threeSegments,
			run: func(t *testing.T, ix *Index) []core.SegmentID {
				_, segs, err := ix.Lose(testFile, ext(4096, 4096), ver(4))
				if err != nil {
					t.Fatalf("Lose: %v", err)
				}
				return segs
			},
			want: []core.SegmentID{2},
			check: func(t *testing.T, ix *Index) {
				if sb := ix.SegmentBytes(2); sb.Held != 0 {
					t.Fatalf("Lose lowered SegmentBytes(2).Held to %d, want 0", sb.Held)
				}
				if sb := ix.SegmentBytes(3); sb.Held != 4096 {
					t.Fatalf("Lose moved SegmentBytes(3).Held to %d, want 4096", sb.Held)
				}
			},
		},
		{
			name:  "LiftSuperseded",
			setup: retainedUsed,
			run: func(t *testing.T, ix *Index) []core.SegmentID {
				_, segs, err := ix.LiftSuperseded(testFile, ver(2), whole)
				if err != nil {
					t.Fatalf("LiftSuperseded: %v", err)
				}
				return segs
			},
			want: []core.SegmentID{1},
			check: func(t *testing.T, ix *Index) {
				if sb := ix.SegmentBytes(1); sb.Retained != 0 {
					t.Fatalf("LiftSuperseded lowered SegmentBytes(1).Retained to %d, want 0", sb.Retained)
				}
				if sb := ix.SegmentBytes(3); sb.Held != 4096 {
					t.Fatalf("LiftSuperseded moved SegmentBytes(3).Held to %d, want 4096", sb.Held)
				}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ix := tt.setup(t)
			got := tt.run(t, ix)
			if !slices.Equal(got, tt.want) {
				t.Fatalf("%s named segments %v, want %v", tt.name, got, tt.want)
			}
			tt.check(t, ix)
		})
	}
}

// TestHandleLookupAndRetainedOfUnknownFile: a read of a file with no entry
// finds nothing and creates no entry.
func TestHandleLookupAndRetainedOfUnknownFile(t *testing.T) {
	ix := New(1 << 20)
	if _, ok := ix.Lookup(testFile, 0); ok {
		t.Fatal("Lookup of an unknown file found a piece")
	}
	if got := ix.Retained(testFile); len(got) != 0 {
		t.Fatalf("Retained of an unknown file = %+v", got)
	}
	if n := ix.fs.len(); n != 0 {
		t.Fatalf("reads created %d entries", n)
	}
}

// TestHandleWriteRetainRefusesAnotherShare: a write through a tag that does
// not own the file is refused and holds nothing.
func TestHandleWriteRetainRefusesAnotherShare(t *testing.T) {
	ix := New(1 << 20)
	writeRetained(t, ix, write(0, 4096, rec(1, 0), 1))
	if _, err := ix.WriteRetain(testTag+1, testFile, write(0, 4096, rec(2, 0), 2), 2); err == nil {
		t.Fatal("a write through another share was accepted")
	}
	if got, _ := ix.Lookup(testFile, 0); got.Version != ver(1) {
		t.Fatalf("Lookup served %+v after a refused write", got)
	}
}

// TestHandleWriteRetainRefusesPastTheExtentLimit: the bound New was given is
// the one admission enforces.
func TestHandleWriteRetainRefusesPastTheExtentLimit(t *testing.T) {
	ix := New(1)
	writeRetained(t, ix, write(0, 4096, rec(1, 0), 1))
	_, err := ix.WriteRetain(testTag, testFile, write(8192, 4096, rec(1, 4096), 2), 2)
	if _, ok := err.(*core.NoSpaceError); !ok {
		t.Fatalf("second extent past a limit of 1: err = %v, want *core.NoSpaceError", err)
	}
}

// TestLiftSupersededOfNothingChangesNothing: a version that superseded
// nothing lifts nothing, and a file with no entry gets none.
func TestLiftSupersededOfNothingChangesNothing(t *testing.T) {
	ix := New(1 << 20)
	writeRetained(t, ix, write(0, 4096, rec(1, 0), 1))
	writeRetained(t, ix, write(0, 4096, rec(2, 0), 2))
	before := ix.Retained(testFile)

	d, _, err := ix.LiftSuperseded(testFile, ver(9), whole)
	if err != nil || d != (HeldDelta{}) {
		t.Fatalf("lift of a version that superseded nothing = %+v, %v; want zero", d, err)
	}
	if got := ix.Retained(testFile); len(got) != len(before) {
		t.Fatalf("Retained changed: %+v -> %+v", before, got)
	}

	d, _, err = ix.LiftSuperseded(fid(99), ver(2), whole)
	if err != nil || d != (HeldDelta{}) {
		t.Fatalf("lift on a file with no entry = %+v, %v; want zero", d, err)
	}
	if _, ok := ix.Lookup(fid(99), 0); ok {
		t.Fatal("the lift gave an unknown file content")
	}
	if n := ix.fs.len(); n != 1 {
		t.Fatalf("entries = %d, want 1: the lift created one", n)
	}
}

// TestIndexLose_ReturnsDelta: the handle's Lose leaves a loss marker over the
// extent and returns the delta of what it stopped holding; a zero-length
// extent is refused.
func TestIndexLose_ReturnsDelta(t *testing.T) {
	ix := New(1 << 20)
	writeRetained(t, ix, write(0, 100, rec(1, 0), 1))
	hd, _, err := ix.Lose(testFile, ext(0, 100), ver(2))
	if err != nil {
		t.Fatalf("Lose: %v", err)
	}
	if hd.File != testFile || hd.HeldBytes != -100 || hd.DirtyBytes != -100 {
		t.Fatalf("Lose delta = %+v, want -100 held and dirty on the file", hd)
	}
	if _, ok := ix.Lookup(testFile, 0); ok {
		t.Fatal("a lost extent is still held")
	}
	if hd, _, err := ix.Lose(testFile, ext(0, 0), ver(3)); !errors.Is(err, core.ErrZeroLength) || hd != (HeldDelta{}) {
		t.Fatalf("zero-length Lose = %+v, %v; want the zero delta and ErrZeroLength", hd, err)
	}
}
