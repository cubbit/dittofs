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
	gaps := []core.Extent{ext(4096, 512)}

	// The tag is refused before the staleness, as Fill refuses it.
	err := ix.CheckFill(testTag+1, id, ver(0), gaps)
	var other *core.OtherShareError
	if !errors.As(err, &other) || !errors.Is(err, core.ErrOtherShare) {
		t.Fatalf("CheckFill through another share = %v, want ErrOtherShare", err)
	}
	if err != nil && errors.Is(err, core.ErrStaleFill) {
		t.Fatalf("CheckFill through another share = %v: the staleness outran the tag", err)
	}

	// A fill resolved before the write that changed the file is refused
	// with ErrStaleFill.
	if err := ix.CheckFill(testTag, id, ver(1), gaps); !errors.Is(err, core.ErrStaleFill) {
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
	if err := ix.CheckFill(testTag, id, ver(3), gaps); err != nil {
		t.Fatalf("a fresh CheckFill = %v, want nil", err)
	}
	if c := ix.fs.extentCount(); c != 1 {
		t.Fatalf("after a passed CheckFill, count = %d, want 1: the check reserves nothing", c)
	}

	// A file with no entry is nobody's, so the tag check passes; it is
	// checked against its bucket, so a floor above asOf refuses it. The
	// check leaves the file with no entry.
	fresh := fid(32)
	if err := ix.CheckFill(testTag, fresh, ver(0), nil); err != nil {
		t.Fatalf("CheckFill of a file with no entry = %v, want nil", err)
	}
	if n := ix.fs.len(); n != 1 {
		t.Fatalf("file table holds %d entries after the checks, want 1", n)
	}
	ix.RaiseFloor(ver(4))
	if err := ix.CheckFill(testTag, fresh, ver(0), nil); !errors.Is(err, core.ErrStaleFill) {
		t.Fatalf("CheckFill below the floor = %v, want ErrStaleFill", err)
	}
}

// TestCheckFillIndexBound: against a count standing at the bound, a fill
// whose gaps would add an entry is refused with the NoSpaceError naming the
// index the fill's admission would, counted once per check, and the refusal
// reserves nothing; a gap the file already holds adds no entry, so it passes
// at the bound.
func TestCheckFillIndexBound(t *testing.T) {
	ix := New(2)
	a, b, c := fid(41), fid(42), fid(43)
	for i, id := range []core.FileID{a, b} {
		p := write(int64(i)*4096, 4096, rec(1, uint32(i)*4096), uint64(i+1))
		if _, err := ix.WriteRetain(testTag, id, p, core.Seq(i+1)); err != nil {
			t.Fatalf("WriteRetain: %v", err)
		}
	}
	if got := ix.fs.extentCount(); got != 2 {
		t.Fatalf("count = %d, want 2", got)
	}

	// A gap on a third file is one wholly absent extent, so at the bound the
	// check is refused with the shortfall of one entry.
	err := ix.CheckFill(testTag, c, ver(0), []core.Extent{ext(0, 100)})
	assertIndexRefusal(t, err)
	var ns *core.NoSpaceError
	if !errors.As(err, &ns) {
		t.Fatalf("CheckFill refused with %T, want *core.NoSpaceError", err)
	}
	if ns.Limit != core.LimitIndex || ns.Unit != core.UnitEntries || ns.Missing != 1 {
		t.Fatalf("CheckFill's refusal = %+v, want the index, 1 missing, in entries", ns)
	}
	if r := ix.fs.indexRefusals(); r != 1 {
		t.Fatalf("after one refused CheckFill, refusals = %d, want 1", r)
	}
	// The refused check reserved nothing, so the count stands at the bound.
	if got := ix.fs.extentCount(); got != 2 {
		t.Fatalf("after a refused CheckFill, the count = %d, want the 2 the commit stands at", got)
	}

	// A gap the file already holds adds no entry, so even at the bound the
	// check passes it, as the admission's whole-gap rule would.
	if err := ix.CheckFill(testTag, a, ver(1), []core.Extent{ext(0, 100)}); err != nil {
		t.Fatalf("CheckFill of a held gap at the bound = %v, want nil", err)
	}
	if r := ix.fs.indexRefusals(); r != 1 {
		t.Fatalf("after a passed CheckFill, refusals = %d, want the 1 of the refusal alone", r)
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
