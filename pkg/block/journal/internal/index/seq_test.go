package index

import (
	"testing"
	"unsafe"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
)

// seqWrite publishes p as a client write of seq through the handle.
func seqWrite(t *testing.T, ix *Index, p Placed, seq core.Seq) {
	t.Helper()
	if _, err := ix.WriteRetain(testTag, testFile, p, seq); err != nil {
		t.Fatalf("WriteRetain(%+v): %v", p, err)
	}
}

// TestHeldPiecesCarryTheirRecordsSeq checks that every read-out of a held
// piece names the sequence number of the record it was written with, that a
// piece split by a later write keeps it on both sides, and that the retained
// part keeps it too.
func TestHeldPiecesCarryTheirRecordsSeq(t *testing.T) {
	ix := New(1 << 20)
	seqWrite(t, ix, write(0, 300, rec(1, 64), 1), 7)

	if p, ok := ix.Lookup(testFile, 10); !ok || p.Seq != 7 {
		t.Fatalf("Lookup = %+v, %v; want seq 7", p, ok)
	}
	if placed, _ := ix.Span(testFile, ext(0, 300)); len(placed) != 1 || placed[0].Seq != 7 {
		t.Fatalf("Span = %+v, want one piece of seq 7", placed)
	}
	if held := ix.HeldAt(testFile, rec(1, 64)); len(held) != 1 || held[0].Seq != 7 {
		t.Fatalf("HeldAt = %+v, want one piece of seq 7", held)
	}

	// A write inside it splits it in three: head and tail stay the first
	// record's, the middle is the second's, and the cut part is retained.
	seqWrite(t, ix, write(100, 100, rec(2, 64), 2), 9)
	placed, _ := ix.Span(testFile, ext(0, 300))
	want := []core.Seq{7, 9, 7}
	if len(placed) != len(want) {
		t.Fatalf("Span = %+v, want three pieces", placed)
	}
	for i, p := range placed {
		if p.Seq != want[i] {
			t.Fatalf("piece %d = %+v, want seq %d", i, p, want[i])
		}
	}
	ret := ix.Retained(testFile)
	if len(ret) != 1 || ret[0].Seq != 7 {
		t.Fatalf("Retained = %+v, want the cut part with seq 7", ret)
	}
}

// TestFillCarriesTheFillsSeq checks that a fill holds the absent parts under
// the fill record's sequence number, and leaves a held piece's own.
func TestFillCarriesTheFillsSeq(t *testing.T) {
	ix := New(1 << 20)
	seqWrite(t, ix, write(100, 100, rec(1, 64), 1), 5)
	fp := Placed{Extent: ext(0, 300), Record: rec(2, 64), Version: ver(1), Seq: 11}
	if _, err := ix.fs.fill(testTag, testFile, fp, ver(1)); err != nil {
		t.Fatalf("fill: %v", err)
	}
	placed, _ := ix.Span(testFile, ext(0, 300))
	want := []core.Seq{11, 5, 11}
	if len(placed) != len(want) {
		t.Fatalf("Span = %+v, want three pieces", placed)
	}
	for i, p := range placed {
		if p.Seq != want[i] {
			t.Fatalf("piece %d = %+v, want seq %d", i, p, want[i])
		}
	}
}

// TestFillUnderAMarkerCarriesTheFillsSeq checks the parts a newer removal
// marker leaves a fill keep the fill record's sequence number.
func TestFillUnderAMarkerCarriesTheFillsSeq(t *testing.T) {
	ix := New(1 << 20)
	seqWrite(t, ix, write(400, 100, rec(1, 64), 1), 5)
	if _, err := ix.Lose(testFile, ext(100, 100), ver(3)); err != nil {
		t.Fatalf("Lose: %v", err)
	}
	fp := Placed{Extent: ext(0, 300), Record: rec(2, 64), Version: ver(2), Seq: 12}
	if _, err := ix.fs.fill(testTag, testFile, fp, ver(3)); err != nil {
		t.Fatalf("fill: %v", err)
	}
	held := ix.HeldAt(testFile, rec(2, 64))
	if len(held) != 2 {
		t.Fatalf("HeldAt the fill = %+v, want the two parts the marker leaves", held)
	}
	for _, p := range held {
		if p.Seq != 12 {
			t.Fatalf("filled piece %+v, want seq 12", p)
		}
	}
}

// TestRepointKeepsTheSeq checks that a record moved to another copy of it
// keeps its sequence number, the copy's by construction.
func TestRepointKeepsTheSeq(t *testing.T) {
	ix := New(1 << 20)
	seqWrite(t, ix, write(0, 300, rec(1, 64), 1), 7)
	if n, _, err := ix.RepointRecord(testFile, rec(1, 64), rec(3, 64)); err != nil || n != 1 {
		t.Fatalf("RepointRecord = %d, %v", n, err)
	}
	if held := ix.HeldAt(testFile, rec(3, 64)); len(held) != 1 || held[0].Seq != 7 {
		t.Fatalf("HeldAt the copy = %+v, want one piece of seq 7", held)
	}
}

// TestFailWindowHoldsAgainWithTheOlderSeq checks that content a failed record
// superseded is held again under its own record's sequence number.
func TestFailWindowHoldsAgainWithTheOlderSeq(t *testing.T) {
	ix := New(1 << 20)
	seqWrite(t, ix, write(0, 300, rec(1, 64), 1), 4)
	seqWrite(t, ix, write(0, 300, rec(2, 64), 2), 8)
	fw, _, err := ix.FailWindow(testFile, []WindowRecord{{Record: rec(2, 64), Version: ver(2), Extent: ext(0, 300)}})
	if err != nil {
		t.Fatalf("FailWindow: %v", err)
	}
	if len(fw.Dropped) != 1 || fw.Dropped[0].Seq != 8 {
		t.Fatalf("Dropped = %+v, want the failed record's piece with seq 8", fw.Dropped)
	}
	if p, ok := ix.Lookup(testFile, 0); !ok || p.Seq != 4 || p.Record != rec(1, 64) {
		t.Fatalf("Lookup = %+v, %v; want the older record held again with seq 4", p, ok)
	}
}

// TestPiecesOfTwoSeqsAtOnePlaceDoNotJoin checks that two pieces that would
// otherwise continue one another are kept apart when they name different
// sequence numbers: they are not one record.
func TestPiecesOfTwoSeqsAtOnePlaceDoNotJoin(t *testing.T) {
	var s heldSet
	s.put(Piece{Extent: ext(0, 100), Record: rec(1, 64), Version: ver(1), Seq: 1})
	s.put(Piece{Extent: ext(100, 100), Record: rec(1, 64), Skip: 100, Version: ver(1), Seq: 2})
	if s.len() != 2 {
		t.Fatalf("%d entries, want 2: pieces of two sequence numbers joined", s.len())
	}
	if placed, _ := s.span(ext(0, 200), nil, nil); len(placed) != 2 {
		t.Fatalf("span = %+v, want two pieces", placed)
	}
}

// TestRebuildCarriesTheRecordsSeq checks that a rebuilt write and fill hold
// their pieces under the record's own sequence number.
func TestRebuildCarriesTheRecordsSeq(t *testing.T) {
	ix := New(1 << 20)
	var ctr Counters
	b := newRebuilder(&ix.fs, &ctr)
	f := fid(3)
	for _, r := range []Record{
		recd(RecWrite, f, 21, 1, 1, 64, 0, 100),
		recd(RecFill, f, 22, 2, 1, 4096, 100, 100),
	} {
		if err := b.Feed(r); err != nil {
			t.Fatalf("Feed: %v", err)
		}
	}
	if err := b.Finish(ver(0)); err != nil {
		t.Fatalf("Finish: %v", err)
	}
	want := map[RecordAt]core.Seq{rec(1, 64): 21, rec(1, 4096): 22}
	placed, _ := ix.Span(f, ext(0, 200))
	if len(placed) != 2 {
		t.Fatalf("Span = %+v, want two pieces", placed)
	}
	for _, p := range placed {
		if p.Seq != want[p.Record] {
			t.Fatalf("piece %+v, want seq %d", p, want[p.Record])
		}
	}
}

// TestHeldEntryStaysFortyBytes pins the entry's size: the sequence number
// fits in what the version's epoch half took, so the index memory bound
// holds as before.
func TestHeldEntryStaysFortyBytes(t *testing.T) {
	if n := unsafe.Sizeof(held{}); n != 40 {
		t.Fatalf("held is %d bytes, want 40", n)
	}
}

// TestToHeldPanicsOnAnEpoch checks that a piece whose version carries an
// epoch, which no record of this format can, is refused as a caller bug
// rather than stored without it.
func TestToHeldPanicsOnAnEpoch(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("toHeld took a version with an epoch")
		}
	}()
	toHeld(Piece{Extent: ext(0, 1), Version: core.Version{Epoch: 1, Counter: 1}})
}

// TestOwnerNamesTheFilesTag checks Owner answers the tag a write claimed the
// file for, and nothing for a file with no entry.
func TestOwnerNamesTheFilesTag(t *testing.T) {
	ix := New(1 << 20)
	if _, ok := ix.Owner(testFile); ok {
		t.Fatal("Owner answers a file with no entry")
	}
	seqWrite(t, ix, write(0, 10, rec(1, 64), 1), 1)
	if tag, ok := ix.Owner(testFile); !ok || tag != testTag {
		t.Fatalf("Owner = %d, %v; want %d", tag, ok, testTag)
	}
}
