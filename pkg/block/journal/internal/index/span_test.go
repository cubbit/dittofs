package index

import (
	"slices"
	"testing"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
)

// spanFile returns an index holding [10,20) and [30,40) of testFile.
func spanFile(t *testing.T) *Index {
	t.Helper()
	ix := New(1 << 20)
	writeRetained(t, ix, write(10, 10, rec(1, 0), 1))
	writeRetained(t, ix, write(30, 10, rec(2, 0), 2))
	return ix
}

func spanOf(ix *Index, e core.Extent) ([]Placed, []core.Extent) {
	return ix.Span(testFile, e)
}

// TestSpanReportsExactGaps: a query over a file holding two pieces gets both
// pieces and every gap, the ends included.
func TestSpanReportsExactGaps(t *testing.T) {
	ix := spanFile(t)
	placed, missing := spanOf(ix, ext(0, 50))
	if len(placed) != 2 || placed[0].Extent != ext(10, 10) || placed[1].Extent != ext(30, 10) {
		t.Fatalf("placed = %+v, want [10,20) and [30,40)", placed)
	}
	if want := []core.Extent{ext(0, 10), ext(20, 10), ext(40, 10)}; !slices.Equal(missing, want) {
		t.Fatalf("missing = %+v, want %+v", missing, want)
	}
}

// TestSpanStartAndEndInAGap: a query that begins and ends inside gaps reports
// the partial gaps at its edges and clips the pieces to it.
func TestSpanStartAndEndInAGap(t *testing.T) {
	ix := spanFile(t)
	placed, missing := spanOf(ix, ext(5, 30))
	if len(placed) != 2 || placed[0].Extent != ext(10, 10) || placed[1].Extent != ext(30, 5) {
		t.Fatalf("placed = %+v, want [10,20) and [30,35)", placed)
	}
	if want := []core.Extent{ext(5, 5), ext(20, 10)}; !slices.Equal(missing, want) {
		t.Fatalf("missing = %+v, want %+v", missing, want)
	}
	placed, missing = spanOf(ix, ext(22, 5))
	if len(placed) != 0 || !slices.Equal(missing, []core.Extent{ext(22, 5)}) {
		t.Fatalf("inside a gap: placed %+v missing %+v, want the whole query missing", placed, missing)
	}
}

// TestSpanOfUnknownFileCreatesNoEntry: a file with no entry is wholly
// missing, and the read leaves the file table as it was.
func TestSpanOfUnknownFileCreatesNoEntry(t *testing.T) {
	ix := New(1 << 20)
	placed, missing := ix.Span(fid(99), ext(4, 100))
	if len(placed) != 0 || !slices.Equal(missing, []core.Extent{ext(4, 100)}) {
		t.Fatalf("Span = %+v, %+v, want nothing placed and the query missing", placed, missing)
	}
	if placed, missing = ix.Span(fid(99), ext(4, 0)); len(placed) != 0 || len(missing) != 0 {
		t.Fatalf("empty query: %+v, %+v, want nothing", placed, missing)
	}
	if n := ix.fs.len(); n != 0 {
		t.Fatalf("file table holds %d entries after a read of an unknown file, want 0", n)
	}
}

// TestSpanIgnoresOffloadedBit: flipping the offloaded bit of part of a held
// piece, and back, leaves what a read is told identical.
func TestSpanIgnoresOffloadedBit(t *testing.T) {
	ix := spanFile(t)
	q := ext(0, 50)
	p0, m0 := spanOf(ix, q)
	for i := range 2 {
		if _, err := ix.fs.update(testFile, func(f *file) error {
			if i == 0 {
				if n := f.held.markOffloaded(ext(12, 4), ver(1), ver(2)); n != 1 {
					t.Fatalf("markOffloaded marked %d pieces, want 1", n)
				}
			} else {
				f.held.markOffloaded(ext(0, 50), ver(1), ver(2))
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		p, m := spanOf(ix, q)
		if !slices.Equal(p, p0) || !slices.Equal(m, m0) {
			t.Fatalf("flip %d changed Span:\n before %+v %+v\n after  %+v %+v", i, p0, m0, p, m)
		}
	}
}
