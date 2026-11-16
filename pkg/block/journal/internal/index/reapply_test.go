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
	got.DropTorn(alice, RecordAt{Segment: 1, Offset: 64}, torn, rest)

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

// TestARepairKeepsTheRunsOfACopyThatStays holds the repair's runs rule: the
// drop was of one placement of the record, and a run names the record by
// version, not its placement — so where a surviving copy reaches the fold,
// the supersede runs the record's version raised stay, exactly the runs a
// rebuild fed the surviving copies alone would hold, and the piece answers
// from the surviving home. The torn record covers only part of the older
// write, so the un-place leaves the older write's tail held and its runs —
// and the record's — alive for the fold to keep.
func TestARepairKeepsTheRunsOfACopyThatStays(t *testing.T) {
	alice := fid(1)
	old := recd(RecWrite, alice, 5, 5, 1, 32, 0, 8192)  // the older write, 0–8 KiB
	keep := recd(RecWrite, alice, 9, 9, 1, 64, 0, 4096) // the surviving copy, the lesser home
	torn := recd(RecWrite, alice, 9, 9, 3, 60, 0, 4096) // the greater home the verdict dropped — same numbers

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

	// The answer the repair must reach: the same records with the torn
	// placement never fed, for the copy that stays is the record.
	want := rebuild([]Record{old, keep})
	// The damaged state: the rebuild kept the torn placement as the record's
	// home — the greater one — its drop and repair applied exactly as the
	// tail proof applies them, the surviving copy fed with the record it
	// overwrote.
	got := rebuild([]Record{old, keep, torn})
	got.DropTorn(alice, RecordAt{Segment: 3, Offset: 60}, torn, []Record{old, keep})

	if gotO, wantO := overOf(got), overOf(want); !slices.Equal(gotO, wantO) {
		t.Errorf("the repaired index keeps the over runs %+v, want %+v: the surviving copy's supersede keeps its runs", gotO, wantO)
	}
	if gotP, wantP := heldPieces(&got.fs, alice), heldPieces(&want.fs, alice); !slices.Equal(gotP, wantP) {
		t.Errorf("the repaired index holds:\n got  %+v\n want %+v: the surviving copy re-places the record over what it overwrote", gotP, wantP)
	}
}

// TestATornRemovalTakesItsMarkerWithIt holds the repair's own rule for a
// torn removal: "treated as never written" must reach the state a rebuild
// that was never fed the record holds, and that state has no marker — for
// each removal kind, the fold that re-places what the removal displaced
// must also take the removal's marker with the tear, or Since keeps
// reporting a delete, truncate or deallocate of content the index actually
// holds. A surviving copy of the removal is the one thing that keeps both,
// the marker exactly as the supersede runs: a marker names the removal's
// version, never its placement.
func TestATornRemovalTakesItsMarkerWithIt(t *testing.T) {
	const KiB = 1024
	alice := fid(1)
	rows := []struct {
		name string
		torn Record
	}{
		// The torn removal is v9; the write it displaced, v5, is the
		// survivor the repair re-places. recd spells each kind's extent the
		// way the scan feeds it: a delete carries none, a truncate's Off is
		// the new size alone.
		{"delete", recd(RecDelete, alice, 9, 9, 1, 64, 0, 0)},
		{"truncate", recd(RecTruncate, alice, 9, 9, 1, 64, 0, 0)},
		{"deallocate", recd(RecDeallocate, alice, 9, 9, 1, 64, 0, 4*KiB)},
	}
	old := recd(RecWrite, alice, 5, 5, 1, 0, 0, 4*KiB)

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

	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			// The answer the repair must reach: the write alone, the torn
			// removal never fed — no marker, Since yielding the write alone.
			want := rebuild([]Record{old})
			// The damaged state: the removal cut the write and left its
			// marker; the drop and repair run exactly as the tail proof
			// applies them.
			got := rebuild([]Record{old, row.torn})
			got.DropTorn(alice, RecordAt{Segment: 1, Offset: 64}, row.torn, []Record{old})

			if gotM, wantM := markerCount(&got.fs, alice), markerCount(&want.fs, alice); gotM != wantM {
				t.Errorf("the repaired index keeps %d markers, want the %d a rebuild never fed the removal leaves", gotM, wantM)
			}
			if gotS, wantS := got.Since(alice, ver(0)), want.Since(alice, ver(0)); !sameChanges(gotS, wantS) {
				t.Errorf("Since got %+v, want %+v: a torn removal is never a change any caller is told", gotS, wantS)
			}
			if gotP, wantP := heldPieces(&got.fs, alice), heldPieces(&want.fs, alice); !slices.Equal(gotP, wantP) {
				t.Errorf("the repaired index holds:\n got  %+v\n want %+v: the write the removal displaced holds its extent again", gotP, wantP)
			}
		})
	}

	t.Run("a surviving copy keeps the removal's marker", func(t *testing.T) {
		torn := recd(RecTruncate, alice, 9, 9, 1, 64, 0, 0)
		keep := recd(RecTruncate, alice, 9, 9, 3, 60, 0, 0) // the same removal at its greater home
		want := rebuild([]Record{old, keep})
		got := rebuild([]Record{old, keep, torn})
		got.DropTorn(alice, RecordAt{Segment: 1, Offset: 64}, torn, []Record{old, keep})

		if gotM, wantM := markerCount(&got.fs, alice), markerCount(&want.fs, alice); gotM != wantM {
			t.Errorf("the repaired index keeps %d markers, want the %d the surviving copy stands for", gotM, wantM)
		}
		if gotS, wantS := got.Since(alice, ver(0)), want.Since(alice, ver(0)); !sameChanges(gotS, wantS) {
			t.Errorf("Since got %+v, want %+v: the surviving copy keeps the removal's change", gotS, wantS)
		}
	})
}
