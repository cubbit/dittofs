package index

import (
	"slices"
	"testing"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
)

// totalsOf reads the file's counters through the handle's own table.
func totalsOf(ix *Index, id core.FileID) heldTotals {
	var t heldTotals
	ix.fs.view(id, func(f *file) { t = f.totals() })
	return t
}

// TestFailWindowServesTheLastSyncedWriteAndDropsOnlyThatRecord: a failed
// record is dropped by its record, not by its version, so the synced write
// under it is held again and is not among the dropped pieces.
func TestFailWindowServesTheLastSyncedWriteAndDropsOnlyThatRecord(t *testing.T) {
	ix := New(1 << 20)
	writeRetained(t, ix, write(0, 4096, rec(1, 0), 5))
	if _, err := ix.LiftSuperseded(testFile, ver(5), whole); err != nil {
		t.Fatal(err)
	}
	writeRetained(t, ix, write(0, 4096, rec(2, 0), 10))

	fw, _, err := ix.FailWindow(testFile, []WindowRecord{failed(rec(2, 0), 10)})
	if err != nil {
		t.Fatalf("FailWindow: %v", err)
	}
	got, ok := ix.Lookup(testFile, 0)
	if !ok || got.Version != ver(5) || got.Record != rec(1, 0) {
		t.Fatalf("Lookup = %+v, %v; want the synced v5 in record (1,0)", got, ok)
	}
	want := []Piece{written(piece(0, 4096, rec(2, 0), 0, 10, false))}
	if !slices.Equal(fw.Dropped, want) {
		t.Fatalf("Dropped = %+v, want %+v", fw.Dropped, want)
	}
	if got := ix.Retained(testFile); len(got) != 0 {
		t.Fatalf("Retained = %+v, want none", got)
	}
}

// TestFailWindowLeavesOtherRecordsAlone: only the part a window record
// overwrote comes back; the rest of the file, and the rest of the record that
// overwrote only part of another, is untouched.
func TestFailWindowLeavesOtherRecordsAlone(t *testing.T) {
	ix := New(1 << 20)
	writeRetained(t, ix, write(0, 4096, rec(1, 0), 5))
	writeRetained(t, ix, write(8192, 4096, rec(1, 4096), 7))
	if _, err := ix.LiftSuperseded(testFile, ver(7), whole); err != nil {
		t.Fatal(err)
	}
	writeRetained(t, ix, write(8192, 2048, rec(2, 0), 9)) // over v7's first half

	if _, _, err := ix.FailWindow(testFile, []WindowRecord{failed(rec(2, 0), 9)}); err != nil {
		t.Fatal(err)
	}
	var pieces []Piece
	ix.fs.view(testFile, func(f *file) { pieces = f.held.pieces(nil) })
	// The held-again half and the untouched half of v7 are adjacent parts of
	// one record and read back as one piece.
	want := []Piece{
		written(piece(0, 4096, rec(1, 0), 0, 5, false)),
		written(piece(8192, 4096, rec(1, 4096), 0, 7, false)),
	}
	if !slices.Equal(pieces, want) {
		t.Fatalf("held:\n got  %+v\n want %+v", pieces, want)
	}
}

// TestFailWindowDropsAGapUnderNoHold: a part the failed record superseded that
// a removal has since taken is under no content, and with no snapshot hold to
// keep it, it is dropped rather than left retained under a version that never
// comes round again.
func TestFailWindowDropsAGapUnderNoHold(t *testing.T) {
	ix := New(1 << 20)
	writeRetained(t, ix, write(0, 4096, rec(1, 0), 5))
	writeRetained(t, ix, write(0, 4096, rec(2, 0), 10))
	if _, err := ix.fs.update(testFile, func(f *file) error { return f.deallocate(ext(0, 4096), ver(11)) }); err != nil {
		t.Fatal(err)
	}

	if _, _, err := ix.FailWindow(testFile, []WindowRecord{failed(rec(2, 0), 10)}); err != nil {
		t.Fatal(err)
	}
	if got := ix.Retained(testFile); len(got) != 0 {
		t.Fatalf("Retained = %+v, want the gap dropped", got)
	}
	if sb := ix.SegmentBytes(1); sb.Retained != 0 {
		t.Fatalf("SegmentBytes(1).Retained = %d, want 0", sb.Retained)
	}
}

// TestFailWindowNamesTheCoveringVersions: a part newer content outside the
// window has covered stays retained by that content, and the version is
// returned for the caller to lift if it has synced.
func TestFailWindowNamesTheCoveringVersions(t *testing.T) {
	ix := New(1 << 20)
	writeRetained(t, ix, write(0, 4096, rec(1, 0), 5))
	writeRetained(t, ix, write(0, 4096, rec(2, 0), 10))
	writeRetained(t, ix, write(0, 2048, rec(3, 0), 12))

	fw, _, err := ix.FailWindow(testFile, []WindowRecord{failed(rec(2, 0), 10)})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(fw.Covers, []Cover{{Version: ver(12), Extent: ext(0, 2048)}}) {
		t.Fatalf("Covers = %v, want [v12]", fw.Covers)
	}
	ret := ix.Retained(testFile)
	if len(ret) != 1 || ret[0].Version != ver(5) || ret[0].By != ver(12) || ret[0].Extent != ext(0, 2048) {
		t.Fatalf("Retained = %+v, want v5 [0,2K) by v12", ret)
	}
}

// TestFailWindowDeltaMatchesTheFileCounters: the delta FailWindow returns is
// the change in the file's held, dirty and retained bytes.
func TestFailWindowDeltaMatchesTheFileCounters(t *testing.T) {
	ix := New(1 << 20)
	writeRetained(t, ix, write(0, 8192, rec(1, 0), 5))
	writeRetained(t, ix, write(4096, 8192, rec(2, 0), 10))
	before := totalsOf(ix, testFile)

	_, d, err := ix.FailWindow(testFile, []WindowRecord{failed(rec(2, 0), 10)})
	if err != nil {
		t.Fatal(err)
	}
	after := totalsOf(ix, testFile)
	if d.HeldBytes != after.held-before.held || d.DirtyBytes != after.dirty-before.dirty ||
		d.RetainedBytes != after.retained-before.retained {
		t.Fatalf("delta %+v does not match totals %+v -> %+v", d, before, after)
	}
	if d.HeldBytes == 0 || d.RetainedBytes == 0 {
		t.Fatalf("delta %+v moved nothing; the test proves nothing", d)
	}
}

// TestFailWindowOfUnknownFile: failing a window of a file with no entry does
// nothing and creates no entry.
func TestFailWindowOfUnknownFile(t *testing.T) {
	ix := New(1 << 20)
	fw, d, err := ix.FailWindow(testFile, []WindowRecord{failed(rec(2, 0), 10)})
	if err != nil || d != (HeldDelta{}) || len(fw.Dropped) != 0 || ix.fs.len() != 0 {
		t.Fatalf("FailWindow of an unknown file = %+v, %+v, %v, entries %d", fw, d, err, ix.fs.len())
	}
}

// splitWrite sets up v1 over [0,8) synced, then v2 as two records of one
// version: [0,4) in record (2,0) and [4,8) in record (3,0), the shape of a
// write split across segments.
func splitWrite(t *testing.T) *Index {
	t.Helper()
	ix := New(1 << 20)
	writeRetained(t, ix, write(0, 8, rec(1, 0), 1))
	if _, err := ix.LiftSuperseded(testFile, ver(1), whole); err != nil {
		t.Fatal(err)
	}
	writeRetained(t, ix, write(0, 4, rec(2, 0), 2))
	writeRetained(t, ix, write(4, 4, rec(3, 0), 2))
	return ix
}

// TestLiftSupersededOfOnePieceLeavesTheOtherPiecesOfItsVersion: a write split
// into two records carries one version on both, so the lift of the first
// record's sync drops what that record superseded, [0,4), and keeps what the
// second still has to hold until it syncs.
func TestLiftSupersededOfOnePieceLeavesTheOtherPiecesOfItsVersion(t *testing.T) {
	ix := splitWrite(t)
	if got := ix.Retained(testFile); len(got) != 2 {
		t.Fatalf("Retained = %+v, want both halves of v1 retained by v2", got)
	}
	if _, err := ix.LiftSuperseded(testFile, ver(2), ext(0, 4)); err != nil {
		t.Fatal(err)
	}
	ret := ix.Retained(testFile)
	if len(ret) != 1 || ret[0].Extent != ext(4, 4) || ret[0].By != ver(2) {
		t.Fatalf("Retained after lifting [0,4) = %+v, want only v1 [4,8) by v2", ret)
	}
}

// TestFailWindowOfOnePieceKeepsTheOtherPiecesRetained: the first record's
// window fails and the second's has not: the second record's retained piece
// stays, so its own failure can still serve the synced v1, and the first
// record's content reads as v1 at once.
func TestFailWindowOfOnePieceKeepsTheOtherPiecesRetained(t *testing.T) {
	ix := splitWrite(t)
	first := WindowRecord{Record: rec(2, 0), Version: ver(2), Extent: ext(0, 4)}
	if _, _, err := ix.FailWindow(testFile, []WindowRecord{first}); err != nil {
		t.Fatal(err)
	}
	if got, ok := ix.Lookup(testFile, 0); !ok || got.Version != ver(1) {
		t.Fatalf("Lookup(0) = %+v, %v; want the synced v1", got, ok)
	}
	if got, ok := ix.Lookup(testFile, 4); !ok || got.Version != ver(2) {
		t.Fatalf("Lookup(4) = %+v, %v; want the second record's v2, which did not fail", got, ok)
	}
	ret := ix.Retained(testFile)
	if len(ret) != 1 || ret[0].Extent != ext(4, 4) || ret[0].Version != ver(1) {
		t.Fatalf("Retained = %+v, want v1 [4,8) still retained for the second record", ret)
	}

	second := WindowRecord{Record: rec(3, 0), Version: ver(2), Extent: ext(4, 4)}
	if _, _, err := ix.FailWindow(testFile, []WindowRecord{second}); err != nil {
		t.Fatal(err)
	}
	if got, ok := ix.Lookup(testFile, 4); !ok || got.Version != ver(1) {
		t.Fatalf("Lookup(4) after the second fails = %+v, %v; want the synced v1, not a hole", got, ok)
	}
}

// TestFailedWindowNamesEachRecordOfASplitCoverWithItsOwnExtent: the content
// that covers a failed record is a write split into two records of one
// version, so the lift the caller makes when one of them has already synced
// must name that record's extent, or it would drop the other's part too.
func TestFailedWindowNamesEachRecordOfASplitCoverWithItsOwnExtent(t *testing.T) {
	var f file
	f.write(write(0, 8, rec(1, 0), 1))
	f.writeRetain(write(0, 8, rec(2, 0), 2)) // the window that fails
	f.writeRetain(write(0, 4, rec(3, 0), 3)) // a split write over it
	f.writeRetain(write(4, 4, rec(4, 0), 3))
	_, covers, _ := f.failWindow([]WindowRecord{{Record: rec(2, 0), Version: ver(2), Extent: ext(0, 8)}}, nil)
	want := []Cover{{Version: ver(3), Extent: ext(0, 4)}, {Version: ver(3), Extent: ext(4, 4)}}
	if !slices.Equal(covers, want) {
		t.Fatalf("covers = %+v, want %+v", covers, want)
	}
	// The first record's sync lifts its own part only.
	f.retained.drop(whole, supersededBy(ver(3), ext(0, 4)), nil)
	for _, r := range f.retained.list(nil) {
		if r.By == ver(3) && r.Extent.Off < 4 {
			t.Fatalf("a part of the first record is still retained: %+v", r)
		}
	}
	if len(f.retained.list(nil)) == 0 {
		t.Fatal("the lift of the first record dropped the second's part")
	}
}

// TestFailedWindowNamesACoverOfASplitSiblingOutsideTheWindow: a write split in
// two records of one version, of which the window holds the first and a later
// segment the second. The part handed to the second record is no part of the
// window's own records, so it is named as a cover: the lift when that record's
// sync completes must find it, or it would stay retained for good once the
// record has already synced.
func TestFailedWindowNamesACoverOfASplitSiblingOutsideTheWindow(t *testing.T) {
	var f file
	f.write(write(0, 8, rec(1, 0), 1))
	f.writeRetain(write(0, 8, rec(2, 0), 2)) // in the window
	f.writeRetain(write(0, 4, rec(2, 8), 3)) // the split write's first record, in the window
	f.writeRetain(write(4, 4, rec(3, 0), 3)) // its second, in a segment of its own
	_, covers, _ := f.failWindow([]WindowRecord{
		{Record: rec(2, 0), Version: ver(2), Extent: ext(0, 8)},
		{Record: rec(2, 8), Version: ver(3), Extent: ext(0, 4)},
	}, nil)
	want := []Cover{{Version: ver(3), Extent: ext(4, 4)}}
	if !slices.Equal(covers, want) {
		t.Fatalf("covers = %+v, want %+v", covers, want)
	}
	f.retained.drop(whole, supersededBy(ver(3), ext(4, 4)), nil)
	if left := f.retained.list(nil); len(left) != 0 {
		t.Fatalf("retained %+v after the sibling's lift, want none", left)
	}
}
