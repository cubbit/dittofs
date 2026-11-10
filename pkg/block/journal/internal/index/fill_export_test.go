package index

import (
	"errors"
	"slices"
	"testing"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
)

// TestFillGapsNetOfHeldAndMarkers: the gaps a fill at v would hold are the
// sub-extents nothing holds, minus what a removal marker above v masks — a
// marker at v masks nothing — in offset order. A file with no entry holds
// nothing, so a positive extent comes back whole, and the read creates no
// entry.
func TestFillGapsNetOfHeldAndMarkers(t *testing.T) {
	ix := New(1 << 20)
	id := fid(21)
	if _, err := ix.WriteRetain(testTag, id, write(10, 10, rec(1, 0), 1), 1); err != nil {
		t.Fatalf("WriteRetain: %v", err)
	}
	// A loss above the fill's version leaves a marker over [30,40).
	if _, err := ix.Lose(id, ext(30, 10), ver(5)); err != nil {
		t.Fatalf("Lose: %v", err)
	}

	// Held [10,20), the marker over [30,40) above v: the gaps run around
	// the piece and around the marker.
	want := []core.Extent{ext(0, 10), ext(20, 10), ext(40, 10)}
	if got := ix.FillGaps(id, ext(0, 50), ver(2)); !slices.Equal(got, want) {
		t.Fatalf("FillGaps = %v, want %v", got, want)
	}

	// At the marker's own version it masks nothing, so [30,40) is a gap like
	// any other: only the held piece keeps its two sides apart.
	want = []core.Extent{ext(0, 10), ext(20, 30)}
	if got := ix.FillGaps(id, ext(0, 50), ver(5)); !slices.Equal(got, want) {
		t.Fatalf("FillGaps at the marker's version = %v, want %v", got, want)
	}

	// A zero-length extent has no gaps.
	if got := ix.FillGaps(id, ext(3, 0), ver(2)); len(got) != 0 {
		t.Fatalf("FillGaps of a zero-length extent = %v, want none", got)
	}

	// A file with no entry holds nothing: the whole extent is one gap, and
	// the read leaves the file table as it was.
	other := fid(22)
	if got := ix.FillGaps(other, ext(4, 100), ver(0)); !slices.Equal(got, []core.Extent{ext(4, 100)}) {
		t.Fatalf("FillGaps of a file with no entry = %v, want [4,104)", got)
	}
	if n := ix.fs.len(); n != 1 {
		t.Fatalf("file table holds %d entries after the reads, want 1", n)
	}
}

// TestCheckFillStaleAndOtherShare: the pre-check refuses the tag first, as
// every journal call does, then a fill older than the version the file is
// checked against; neither refusal claims the file, changes an entry or
// counts against the index. A file with no entry is checked against its floor
// bucket, so a raised floor refuses a fill below it.
func TestCheckFillStaleAndOtherShare(t *testing.T) {
	ix := New(1 << 20)
	id := fid(31)
	if _, err := ix.WriteRetain(testTag, id, write(0, 4096, rec(1, 0), 3), 3); err != nil {
		t.Fatalf("WriteRetain: %v", err)
	}

	// The tag is refused before the staleness, as Fill refuses it.
	err := ix.CheckFill(testTag+1, id, ver(0))
	var other *core.OtherShareError
	if !errors.As(err, &other) || !errors.Is(err, core.ErrOtherShare) {
		t.Fatalf("CheckFill through another share = %v, want ErrOtherShare", err)
	}
	if err != nil && errors.Is(err, core.ErrStaleFill) {
		t.Fatalf("CheckFill through another share = %v: the staleness outran the tag", err)
	}

	// A fill resolved before the write that changed the file is refused
	// with ErrStaleFill.
	if err := ix.CheckFill(testTag, id, ver(1)); !errors.Is(err, core.ErrStaleFill) {
		t.Fatalf("a stale CheckFill = %v, want ErrStaleFill", err)
	}
	// Neither refusal claimed the file or changed its entry, and neither
	// counts against the index.
	es := ix.FileEntries(id)
	if len(es) != 1 || es[0].Kind != EntryHeld || es[0].Tag != testTag {
		t.Fatalf("the entries after the refused checks = %+v, want the one held piece under the tag", es)
	}
	if c, r := ix.fs.extentCount(), ix.fs.indexRefusals(); c != 1 || r != 0 {
		t.Fatalf("after the refused checks: count = %d refusals = %d, want 1 and 0", c, r)
	}

	// A fill of the file as it stands passes and reserves nothing.
	if err := ix.CheckFill(testTag, id, ver(3)); err != nil {
		t.Fatalf("a fresh CheckFill = %v, want nil", err)
	}
	if c := ix.fs.extentCount(); c != 1 {
		t.Fatalf("after a passed CheckFill, count = %d, want 1: the check reserves nothing", c)
	}

	// A file with no entry is nobody's, so the tag check passes; it is
	// checked against its bucket, so a floor above asOf refuses it. The
	// check leaves the file with no entry.
	fresh := fid(32)
	if err := ix.CheckFill(testTag, fresh, ver(0)); err != nil {
		t.Fatalf("CheckFill of a file with no entry = %v, want nil", err)
	}
	if n := ix.fs.len(); n != 1 {
		t.Fatalf("file table holds %d entries after the checks, want 1", n)
	}
	ix.RaiseFloor(ver(4))
	if err := ix.CheckFill(testTag, fresh, ver(0)); !errors.Is(err, core.ErrStaleFill) {
		t.Fatalf("CheckFill below the floor = %v, want ErrStaleFill", err)
	}
}

// TestFillExportHoldsOffloaded: the exported Fill holds its record offloaded,
// under the seq it is given rather than p's, where nothing is held, and
// changes no change version, so what a read reports of the file is unmoved.
func TestFillExportHoldsOffloaded(t *testing.T) {
	ix := New(1 << 20)
	id := fid(7)
	asOf := ix.AsOf(id)

	d, err := ix.Fill(testTag, id, write(0, 100, rec(1, 0), 7), 9, asOf)
	if err != nil {
		t.Fatalf("Fill: %v", err)
	}
	if d.File != id || d.Tag != testTag || d.HeldBytes != 100 || d.DirtyBytes != 0 {
		t.Fatalf("Fill's delta = %+v, want 100 held bytes, none dirty, under the tag", d)
	}

	placed, missing := ix.Span(id, ext(0, 100))
	if len(missing) != 0 || len(placed) != 1 ||
		placed[0].Record != rec(1, 0) || placed[0].Version != ver(7) {
		t.Fatalf("Span = %+v, %+v, want the fill's record at version 7 and nothing missing", placed, missing)
	}
	// The piece is held offloaded, and under the seq Fill was given, not
	// p's — a record's seq is never its version's counter.
	if got, ok := ix.Lookup(id, 0); !ok || got.Seq != core.Seq(9) {
		t.Fatalf("Lookup = %+v, %v, want the piece under seq 9", got, ok)
	}
	es := ix.FileEntries(id)
	if len(es) != 1 || es[0].Kind != EntryHeld || !es[0].Offloaded {
		t.Fatalf("the entries = %+v, want one held piece with its offloaded bit set", es)
	}

	// A fill changes no version, so the version a read reports is unmoved.
	if got := ix.AsOf(id); got != asOf {
		t.Fatalf("AsOf moved from %v to %v: a fill changes no version", asOf, got)
	}

	// A fill raises no change version, so a stale fill needs a change: once
	// a write moves the file's version above asOf, a fill of it changes
	// nothing, claim included.
	if _, err := ix.WriteRetain(testTag, id, write(100, 10, rec(1, 100), 8), 8); err != nil {
		t.Fatalf("WriteRetain: %v", err)
	}
	if _, ok := ix.Lookup(id, 100); !ok {
		t.Fatal("the write that moved the version is not held")
	}
	if _, err := ix.Fill(testTag, id, write(200, 10, rec(1, 200), 7), 10, asOf); !errors.Is(err, core.ErrStaleFill) {
		t.Fatalf("a stale Fill = %v, want ErrStaleFill", err)
	}
	if _, ok := ix.Lookup(id, 200); ok {
		t.Fatal("a stale fill held its record")
	}
}

// TestCheckFillIndexBoundTagFree: the tag-free bound check counts every
// gap a fill of e could add — a marker only ever shrinks a real fill's
// parts, so a mask never shrinks the figure: a marker a fill's version
// outlives is caught here too, never only by the fill's own admission.
// It reserves nothing, claims nothing and creates no entry.
func TestCheckFillIndexBoundTagFree(t *testing.T) {
	// Held [0,20) and [30,40) with a marker at version 5 over [20,30): a
	// fill resolved above the marker holds [20,30) as one entry — the
	// figure the bound must name. The count of three (two pieces and the
	// marker) sits at the bound, so that fourth entry is refused; a figure
	// that let the marker mask its gap would have passed it.
	ix := New(3)
	id := fid(51)
	for i, p := range []Placed{write(0, 20, rec(1, 0), 1), write(30, 10, rec(1, 30), 2)} {
		if _, err := ix.WriteRetain(testTag, id, p, core.Seq(i+1)); err != nil {
			t.Fatalf("WriteRetain: %v", err)
		}
	}
	if _, err := ix.Lose(id, ext(20, 10), ver(5)); err != nil {
		t.Fatalf("Lose: %v", err)
	}
	if err := ix.CheckFillIndexBound(id, ext(0, 40)); err == nil {
		t.Fatal("CheckFillIndexBound of the gap the marker could not mask was granted at the bound, want a refusal")
	}
	// The refused check reserved nothing: the count still stands at three.
	if c := ix.fs.extentCount(); c != 3 {
		t.Fatalf("after the check, count = %d, want 3", c)
	}
	// A fill inside a held piece adds no entry, so it fits the headroom.
	if err := ix.CheckFillIndexBound(id, ext(5, 5)); err != nil {
		t.Fatalf("CheckFillIndexBound inside a held piece = %v, want nil", err)
	}

	// A file with no entry holds nothing, so a fill of bytes is one wholly
	// absent extent; at a bound holding one entry already, that second one
	// is refused — and the check leaves the file table as it was.
	ix2 := New(1)
	if _, err := ix2.WriteRetain(testTag, fid(53), write(0, 10, rec(1, 0), 1), 1); err != nil {
		t.Fatalf("WriteRetain: %v", err)
	}
	fresh := fid(52)
	if err := ix2.CheckFillIndexBound(fresh, ext(0, 100)); err == nil {
		t.Fatal("CheckFillIndexBound of a wholly absent file was granted at the bound, want a refusal")
	}
	if n := ix2.fs.len(); n != 1 {
		t.Fatalf("file table holds %d entries, want the one the fixture wrote: the check created none", n)
	}
}

// TestFillNotesItsRecordInDropRanges: a fill's record is noted in the file's
// drop ranges as a write's is, so a removal record in another segment may not
// be dropped while the fill's segment lives.
func TestFillNotesItsRecordInDropRanges(t *testing.T) {
	ix := New(1 << 20)
	id := fid(51)
	if _, err := ix.Fill(testTag, id, write(0, 100, rec(1, 0), 7), 9, ix.AsOf(id)); err != nil {
		t.Fatalf("Fill: %v", err)
	}
	if !ix.fs.mayOutrank(id, 2, ext(0, 100), Numbers{Version: ver(8), Seq: 10}) {
		t.Fatal("a removal record in another segment may be dropped while the fill's record still lives")
	}
}

// A fill's pre-check checks the tag and the staleness and no bound: a count
// above the limit refuses neither a fill of a held extent nor a fill of a
// fresh one, whose refusal is the capacity gate's, after its read-only gate.
func TestCheckFillChecksNoBound(t *testing.T) {
	ix := New(1 << 20)
	a := fid(52)
	if _, err := ix.WriteRetain(testTag, a, write(0, 4096, rec(1, 0), 1), 1); err != nil {
		t.Fatalf("WriteRetain: %v", err)
	}
	ix.fs.limit.Store(0)
	if err := ix.CheckFill(testTag, a, ver(1)); err != nil {
		t.Fatalf("CheckFill with the count above the limit = %v, want nil", err)
	}
	if err := ix.CheckFill(testTag, fid(53), ver(0)); err != nil {
		t.Fatalf("CheckFill of a fresh file with the count above the limit = %v, want nil", err)
	}
	if r := ix.fs.indexRefusals(); r != 0 {
		t.Fatalf("refusals = %d, want 0", r)
	}
}

// A fill refused as stale has its record in a segment already: the index
// claims the file and notes the record in its drop ranges, and holds nothing.
func TestFillStaleNotesItsRecord(t *testing.T) {
	ix := New(1 << 20)
	id := fid(54)
	ix.RaiseFloor(ver(5))
	if _, err := ix.Fill(testTag, id, write(0, 100, rec(1, 0), 1), 9, ver(0)); !errors.Is(err, core.ErrStaleFill) {
		t.Fatalf("Fill = %v, want ErrStaleFill", err)
	}
	if tag, ok := ix.Owner(id); !ok || tag != testTag {
		t.Fatalf("Owner = %v, %v, want the fill's tag", tag, ok)
	}
	if !ix.fs.mayOutrank(id, 2, ext(0, 100), Numbers{Version: ver(8), Seq: 10}) {
		t.Fatal("a removal record in another segment may be dropped while the refused fill's record lives")
	}
	if _, ok := ix.Lookup(id, 0); ok {
		t.Fatal("a stale fill held its record")
	}
}

// A fill reserves what it adds at its commit, so fills of different files that
// all passed a read-only pre-check at the bound's edge cannot all commit: the
// one past the limit is refused naming the index, and the count stays put.
func TestFillReservesAgainstTheBound(t *testing.T) {
	ix := New(2)
	if _, err := ix.WriteRetain(testTag, fid(80), write(0, 10, rec(1, 0), 1), 1); err != nil {
		t.Fatalf("WriteRetain: %v", err)
	}
	b, c := fid(81), fid(82)
	// Both pass the pre-check at limit-1, as two concurrent fills would.
	for _, id := range []core.FileID{b, c} {
		if err := ix.CheckFillIndexBound(id, ext(0, 10)); err != nil {
			t.Fatalf("CheckFillIndexBound: %v", err)
		}
	}
	if _, err := ix.Fill(testTag, b, write(0, 10, rec(1, 100), 2), 2, ver(0)); err != nil {
		t.Fatalf("Fill within the bound: %v", err)
	}
	if n := ix.fs.extentCount(); n != 2 {
		t.Fatalf("count = %d after the fill within the bound, want 2", n)
	}
	_, err := ix.Fill(testTag, c, write(0, 10, rec(1, 200), 3), 3, ver(0))
	var ns *core.NoSpaceError
	if !errors.As(err, &ns) || ns.Limit != core.LimitIndex {
		t.Fatalf("Fill past the bound = %v, want a NoSpaceError naming the index", err)
	}
	if n := ix.fs.extentCount(); n != 2 {
		t.Fatalf("count = %d after the refused fill, want 2: it reserved or held something", n)
	}
	// The record is in its segment already: the file is claimed and the record
	// noted, with nothing held.
	if tag, ok := ix.Owner(c); !ok || tag != testTag {
		t.Fatalf("Owner = %v, %v, want the fill's tag", tag, ok)
	}
	if !ix.fs.mayOutrank(c, 2, ext(0, 10), Numbers{Version: ver(8), Seq: 10}) {
		t.Fatal("a removal record in another segment may be dropped while the refused fill's record lives")
	}
	if _, ok := ix.Lookup(c, 0); ok {
		t.Fatal("a fill refused by the bound held its record")
	}
}

// A fill refused as stale after its admission gives the reservation back: the
// count is what it was.
func TestFillStaleReleasesItsReservation(t *testing.T) {
	ix := New(4)
	ix.RaiseFloor(ver(5))
	if _, err := ix.Fill(testTag, fid(83), write(0, 10, rec(1, 0), 1), 1, ver(0)); !errors.Is(err, core.ErrStaleFill) {
		t.Fatalf("Fill = %v, want ErrStaleFill", err)
	}
	if n := ix.fs.extentCount(); n != 0 {
		t.Fatalf("count = %d after a stale fill, want 0", n)
	}
}
