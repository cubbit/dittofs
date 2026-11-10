package index

import (
	"slices"
	"testing"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
)

// heldOf reads the file's held pieces through the handle's own table.
func heldOf(ix *Index, id core.FileID) []Piece {
	var ps []Piece
	ix.fs.view(id, func(f *file) { ps = f.held.pieces(nil) })
	return ps
}

// TestRepointRecordMovesHeldAndRetained: a held piece and a retained piece at
// from end up at to with their version, offloaded bit and superseder kept.
func TestRepointRecordMovesHeldAndRetained(t *testing.T) {
	ix := New(1 << 20)
	writeRetained(t, ix, write(0, 4096, rec(1, 0), 5))
	writeRetained(t, ix, write(0, 2048, rec(2, 0), 10)) // retains v5's first half, holds v10

	// A skip other than zero must survive the move.
	if _, err := ix.fs.update(testFile, func(f *file) error {
		f.held.repoint(ext(0, 2048), rec(2, 0), rec(2, 0), 64)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	before := heldOf(ix, testFile)
	retBefore := ix.Retained(testFile)

	moved, _, err := ix.RepointRecord(testFile, rec(2, 0), rec(7, 128))
	if err != nil {
		t.Fatalf("RepointRecord: %v", err)
	}
	if moved != 1 {
		t.Fatalf("moved = %d, want 1 (the held piece of rec(2,0))", moved)
	}
	got := heldOf(ix, testFile)
	want := slices.Clone(before)
	for i := range want {
		if want[i].Record == rec(2, 0) {
			want[i].Record = rec(7, 128)
		}
	}
	if !slices.Equal(got, want) {
		t.Fatalf("held:\n got  %+v\n want %+v", got, want)
	}
	if !slices.Equal(ix.Retained(testFile), retBefore) {
		t.Fatalf("retained changed: %+v, want %+v", ix.Retained(testFile), retBefore)
	}

	// v5 is held in its second half and retained, superseded by v10, in its
	// first: both sit at rec(1,0).
	moved, _, err = ix.RepointRecord(testFile, rec(1, 0), rec(8, 0))
	if err != nil {
		t.Fatal(err)
	}
	if moved != 2 {
		t.Fatalf("moved = %d, want 2", moved)
	}
	r := ix.Retained(testFile)
	if len(r) != 1 || r[0].Record != rec(8, 0) || r[0].By != ver(10) || r[0].Version != ver(5) || r[0].Offloaded || r[0].Extent != ext(0, 2048) {
		t.Fatalf("retained = %+v, want v5 [0,2048) at rec(8,0) by v10", r)
	}
}

// TestRepointRecordLeavesOtherRecords: a piece at another record is untouched.
func TestRepointRecordLeavesOtherRecords(t *testing.T) {
	ix := New(1 << 20)
	writeRetained(t, ix, write(0, 4096, rec(1, 0), 5))
	writeRetained(t, ix, write(8192, 4096, rec(1, 4096), 7))
	moved, _, err := ix.RepointRecord(testFile, rec(1, 0), rec(2, 0))
	if err != nil {
		t.Fatal(err)
	}
	if moved != 1 {
		t.Fatalf("moved = %d, want 1", moved)
	}
	want := []Piece{
		piece(0, 4096, rec(2, 0), 0, 5, false),
		piece(8192, 4096, rec(1, 4096), 0, 7, false),
	}
	if got := heldOf(ix, testFile); !slices.Equal(got, want) {
		t.Fatalf("held:\n got  %+v\n want %+v", got, want)
	}
}

// TestRepointRecordMissingFile: no entry and no piece at from change nothing,
// and no entry is created.
func TestRepointRecordMissingFile(t *testing.T) {
	ix := New(1 << 20)
	moved, hd, err := ix.RepointRecord(testFile, rec(1, 0), rec(2, 0))
	if moved != 0 || hd != (HeldDelta{}) || err != nil {
		t.Fatalf("got %d, %+v, %v; want zero", moved, hd, err)
	}
	if ix.fs.view(testFile, func(*file) {}) {
		t.Fatal("RepointRecord created an entry")
	}
	writeRetained(t, ix, write(0, 4096, rec(1, 0), 5))
	moved, hd, err = ix.RepointRecord(testFile, rec(9, 0), rec(2, 0))
	if moved != 0 || hd != (HeldDelta{}) || err != nil {
		t.Fatalf("no piece at from: got %d, %+v, %v; want zero", moved, hd, err)
	}
}

// TestRepointRecordAccountsSegments: bytes leave from's segment and join to's.
func TestRepointRecordAccountsSegments(t *testing.T) {
	ix := New(1 << 20)
	writeRetained(t, ix, write(0, 4096, rec(1, 0), 5))
	writeRetained(t, ix, write(0, 1024, rec(2, 0), 10))
	if got := ix.SegmentBytes(1); got != (SegmentBytes{Held: 3072, Retained: 1024}) {
		t.Fatalf("segment 1 = %+v before", got)
	}
	if _, _, err := ix.RepointRecord(testFile, rec(1, 0), rec(3, 0)); err != nil {
		t.Fatal(err)
	}
	if got := ix.SegmentBytes(1); got != (SegmentBytes{}) {
		t.Fatalf("segment 1 = %+v, want empty", got)
	}
	if got := ix.SegmentBytes(3); got != (SegmentBytes{Held: 3072, Retained: 1024}) {
		t.Fatalf("segment 3 = %+v, want 3072 held, 1024 retained", got)
	}
	if got := ix.SegmentBytes(2); got != (SegmentBytes{Held: 1024}) {
		t.Fatalf("segment 2 = %+v, want 1024 held", got)
	}
	checkSegments(t, 0, &ix.fs, 1, 2, 3)
}

// TestNamesReadsHeldAndRetainedPieces: a record is named while a held or a
// retained piece sits at it, and not once nothing does, nor for a file with no
// entry, for which no entry is created.
func TestNamesReadsHeldAndRetainedPieces(t *testing.T) {
	ix := New(1 << 20)
	if ix.Names(testFile, rec(1, 0)) {
		t.Fatal("a record of a file with no entry is named")
	}
	if ix.fs.view(testFile, func(*file) {}) {
		t.Fatal("Names created an entry")
	}
	writeRetained(t, ix, write(0, 4096, rec(1, 0), 5))
	if !ix.Names(testFile, rec(1, 0)) {
		t.Fatal("a held record is not named")
	}
	writeRetained(t, ix, write(0, 4096, rec(2, 0), 10)) // retains v5 whole
	if !ix.Names(testFile, rec(1, 0)) {
		t.Fatal("a retained record is not named")
	}
	if ix.Names(testFile, rec(3, 0)) {
		t.Fatal("a record no piece sits at is named")
	}
}

// TestHeldAtReturnsTheRecordsHeldPieces: HeldAt returns only the held pieces
// at the record, split where an overwrite cut them, with no retained piece, and
// nothing for a file with no entry, for which no entry is created.
func TestHeldAtReturnsTheRecordsHeldPieces(t *testing.T) {
	ix := New(1 << 20)
	if got := ix.HeldAt(testFile, rec(1, 0)); len(got) != 0 {
		t.Fatalf("HeldAt of a file with no entry = %+v, want none", got)
	}
	if ix.fs.view(testFile, func(*file) {}) {
		t.Fatal("HeldAt created an entry")
	}
	writeRetained(t, ix, write(0, 4096, rec(1, 0), 5))
	writeRetained(t, ix, write(1024, 1024, rec(2, 0), 10)) // retains v5's middle
	got := ix.HeldAt(testFile, rec(1, 0))
	if len(got) != 2 || got[0].Extent != ext(0, 1024) || got[1].Extent != ext(2048, 2048) ||
		got[0].Offloaded || got[1].Offloaded {
		t.Fatalf("HeldAt = %+v, want v5's [0,1024) and [2048,4096), not offloaded", got)
	}
	if got := ix.HeldAt(testFile, rec(3, 0)); len(got) != 0 {
		t.Fatalf("HeldAt of a record no piece sits at = %+v, want none", got)
	}
}

// TestATruncateOverAReappendedRecordStillOutranksIt: a record re-appended
// under its original sequence number and version stays below a truncate drawn
// over the original, both in the running index, where the re-append repoints
// the record, and in one rebuilt from the records on disk, where the copy sits
// in a segment after the truncate's. A copy that took a new version would beat
// the truncate, which is why the re-append keeps the original's numbers.
func TestATruncateOverAReappendedRecordStillOutranksIt(t *testing.T) {
	f := fid(1)
	x := core.Extent{Off: 0, Len: 4}
	orig := RecordAt{Segment: 1, Offset: 64}
	again := RecordAt{Segment: 3, Offset: 64}
	wrote := Numbers{Version: ver(10), Seq: 20}
	cut := Numbers{Version: ver(11), Seq: 21}

	ix := New(1 << 20)
	if _, err := ix.WriteRetain(1, f, Placed{Extent: x, Record: orig, Version: wrote.Version}, wrote.Seq); err != nil {
		t.Fatalf("WriteRetain: %v", err)
	}
	if _, err := ix.fs.update(f, func(fl *file) error { fl.truncate(0, cut.Version); return nil }); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	if _, _, err := ix.RepointRecord(f, orig, again); err != nil {
		t.Fatalf("RepointRecord: %v", err)
	}
	if p, ok := ix.Lookup(f, 0); ok {
		t.Fatalf("the running index serves %+v over a truncate, want nothing", p)
	}

	rebuild := func(copyAt Numbers) (Placed, bool) {
		ix := New(1 << 20)
		b := newRebuilder(&ix.fs, &Counters{})
		for _, r := range []Record{
			{Kind: RecWrite, File: f, Tag: 1, Segment: orig.Segment, At: orig.Offset, Off: x.Off, Len: x.Len, Numbers: wrote},
			{Kind: RecTruncate, File: f, Tag: 1, Segment: 2, At: 64, Off: 0, Numbers: cut},
			{Kind: RecWrite, File: f, Tag: 1, Segment: again.Segment, At: again.Offset, Off: x.Off, Len: x.Len, Numbers: copyAt},
		} {
			if err := b.Feed(r); err != nil {
				t.Fatalf("Feed(%+v): %v", r, err)
			}
		}
		if err := b.Finish(ver(0)); err != nil {
			t.Fatalf("Finish: %v", err)
		}
		return ix.Lookup(f, 0)
	}
	if p, ok := rebuild(wrote); ok {
		t.Fatalf("the rebuilt index serves the re-appended copy %+v over the truncate, want nothing", p)
	}
	if _, ok := rebuild(Numbers{Version: ver(12), Seq: 22}); !ok {
		t.Fatal("a copy above the truncate's version is not served: the check would not catch a re-append that took new numbers")
	}
}
