package index

import (
	"math"
	"slices"
	"testing"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
)

// TestReplayWindowRestoresWhatTheTornRecordDisplaced runs the repair over a
// window holding every reach the fold must know — a write, one record in two
// homes, a removal to the file's end, a mark and a fill — and answers with
// exactly the index a rebuild without the torn record would have produced:
// the same held pieces with the same offloaded bits, the same removal marker,
// the same bytes accounted to each segment, and the torn write's supersede
// runs gone with it.
func TestReplayWindowRestoresWhatTheTornRecordDisplaced(t *testing.T) {
	const KiB = 1024
	alice := fid(1)
	torn := recd(RecWrite, alice, 9, 9, 1, 64, 0, 16*KiB)
	rest := []Record{
		recd(RecWrite, alice, 5, 5, 1, 0, 0, 4*KiB),
		recd(RecTruncate, alice, 6, 6, 1, 16, 2*KiB, 0),
		recd(RecOffloaded, alice, 7, 7, 1, 32, 0, 4*KiB),
		recd(RecWrite, alice, 8, 8, 1, 48, 8*KiB, 8*KiB),
		recd(RecWrite, alice, 8, 8, 3, 60, 8*KiB, 8*KiB), // the same record carried at its greater home
		recd(RecFill, alice, 10, 10, 2, 0, 10*KiB, 2*KiB),
	}
	window := core.Extent{Off: 0, Len: 16 * KiB}

	rebuild := func(recs []Record) *Index {
		t.Helper()
		ix := New(math.MaxInt64)
		b := newRebuilder(&ix.fs, &Counters{})
		for _, r := range recs {
			if err := b.Feed(r); err != nil {
				t.Fatalf("Feed(seq %v) = %v", r.Numbers.Seq, err)
			}
		}
		if err := b.Finish(ver(0)); err != nil {
			t.Fatalf("Finish: %v", err)
		}
		return ix
	}
	overOf := func(ix *Index) []overRun {
		t.Helper()
		var over []overRun
		ix.fs.view(alice, func(f *file) { over = slices.Clone(f.held.over) })
		return over
	}

	// The answer the repair must reach: the same records, the torn write
	// never fed.
	want := rebuild(rest)
	// The damaged state: the torn write overwrote everything, its drop and
	// repair applied exactly as the tail proof applies them.
	got := rebuild(append(slices.Clone(rest), torn))
	got.Unplace(alice, RecordAt{Segment: 1, Offset: 64}, window)
	got.ReplayWindow(alice, window, torn.Numbers.Version, rest)

	if gotP, wantP := heldPieces(&got.fs, alice), heldPieces(&want.fs, alice); !slices.Equal(gotP, wantP) {
		t.Errorf("the repaired index holds:\n got  %+v\n want %+v: the answer a rebuild without the torn record would give", gotP, wantP)
	}
	if gotO, wantO := overOf(got), overOf(want); !slices.Equal(gotO, wantO) {
		t.Errorf("the repaired index keeps the over runs %+v, want %+v: the torn write's runs go with it", gotO, wantO)
	}
	if gotM, wantM := markerCount(&got.fs, alice), markerCount(&want.fs, alice); gotM != wantM {
		t.Errorf("the repaired index keeps %d markers, want the %d the rebuild left: a repair re-notes nothing", gotM, wantM)
	}
	for _, seg := range []core.SegmentID{1, 2, 3} {
		gotB, wantB := (&got.fs).segmentBytes(seg), (&want.fs).segmentBytes(seg)
		if gotB != wantB {
			t.Errorf("segment %d backs got %d, want %d: the repair accounts the re-placed bytes the same", seg, gotB, wantB)
		}
	}
}
