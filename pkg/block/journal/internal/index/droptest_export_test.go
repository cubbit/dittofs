package index

import (
	"testing"
)

// TestMayOutrank_PerExtentPerFile: the exported drop query answers per
// extent and per file. A removal of A over an extent no record of A
// overlaps outranks nothing, wherever an older record of A lies; the same
// removal over A's record's extent may still outrank the record; and a
// record of another file alone, at the same extent and a lower pair, keeps
// no removal of a file that holds none.
func TestMayOutrank_PerExtentPerFile(t *testing.T) {
	ix := New(1 << 20)
	a, b := fid(1), fid(2)
	// A has a record in segment 2 over [0, 1MiB) at pair (3,1).
	noteRec(t, &ix.fs, a, 2, ext(0, 1<<20), nums(3, 1))

	// A removal in segment 1 over [1GiB, 2GiB) at pair (5,2): no record of A
	// overlaps it, so the removal outranks nothing.
	if got := ix.MayOutrank(a, 1, ext(1<<30, 1<<30), nums(5, 2)); got {
		t.Fatalf("MayOutrank of A's removal over [1GiB, 2GiB) = %v, want false: no record of A overlaps it", got)
	}

	// The same removal over [0, 1MiB): A's record in segment 2 overlaps it
	// at the lower pair, so the removal may still outrank it.
	if got := ix.MayOutrank(a, 1, ext(0, 1<<20), nums(5, 2)); !got {
		t.Fatalf("MayOutrank of A's removal over [0, 1MiB) = %v, want true: A's record in segment 2 overlaps it at the lower pair", got)
	}

	// Note B's record over the same extent at a pair below the removal's:
	// B's own removal is kept by it, which is the fixture's proof the
	// record was noted at all.
	noteRec(t, &ix.fs, b, 2, ext(0, 1<<20), nums(3, 1))
	if got := ix.MayOutrank(b, 1, ext(0, 1<<20), nums(5, 2)); !got {
		t.Fatalf("MayOutrank of B's own removal over [0, 1MiB) = %v, want true: B's record was not noted", got)
	}

	// A removal of a file whose only overlapping record is another file's:
	// the query stays within the one file.
	other := fid(3)
	if got := ix.MayOutrank(other, 1, ext(0, 1<<20), nums(5, 2)); got {
		t.Fatalf("MayOutrank of %v's removal over [0, 1MiB) = %v, want false: only B's record overlaps it and the query answers only in terms of the one file", other, got)
	}

	// A lower record inside the removal's own segment keeps nothing: the
	// drop query asks only about records in other segments.
	if got := ix.MayOutrank(a, 2, ext(0, 1<<20), nums(5, 2)); got {
		t.Fatalf("MayOutrank of A's removal naming its own segment = %v, want false: a lower record in the removal's own segment keeps it never", got)
	}

	// A record of an equal or higher pair does not keep the removal: an
	// equal one is the record itself or a carried copy of it, and a higher
	// one is outranked by it, so the removal outranks nothing there.
	noteRec(t, &ix.fs, a, 3, ext(1<<21, 1024), nums(5, 2))
	if got := ix.MayOutrank(a, 1, ext(1<<21, 1024), nums(5, 2)); got {
		t.Fatalf("MayOutrank of a removal beside a record of an equal pair = %v, want false", got)
	}
	noteRec(t, &ix.fs, a, 4, ext(1<<22, 1024), nums(6, 1))
	if got := ix.MayOutrank(a, 1, ext(1<<22, 1024), nums(5, 2)); got {
		t.Fatalf("MayOutrank of a removal beside a record of a higher pair = %v, want false", got)
	}
}

// TestMarkerSettled_AtOrBelow: the settled query reports whether every
// removal of id not yet settled sits above v. Two markers answer one
// query: a lower one settled and a higher one still waiting leave the
// answer below the higher one false even after the lower one's settle. A
// removal at v leaves the answer false; a settle at v drops its marker and
// moves it true; a later removal at a higher version leaves the old v
// true and the new one false until it is settled too. A file with no
// marker, including one with no entry at all, has nothing unsettled at any
// v.
func TestMarkerSettled_AtOrBelow(t *testing.T) {
	ix := New(1 << 20)
	id := fid(21)

	// A file with no entry has nothing unsettled.
	if got := ix.MarkerSettled(id, ver(5)); !got {
		t.Fatalf("MarkerSettled of a file with no entry = %v, want true: nothing is unsettled", got)
	}

	// A loss at version 5 leaves a marker over [0, 4KiB), unsettled at 5.
	if _, _, err := ix.Lose(id, ext(0, 4096), ver(5)); err != nil {
		t.Fatalf("Lose: %v", err)
	}
	if got := ix.MarkerSettled(id, ver(5)); got {
		t.Fatalf("MarkerSettled at the loss's own version = %v, want false: the marker it left waits for the settle", got)
	}

	// Settling at the loss's version drops the marker it left.
	ix.fs.settleRemovals(id, ver(5))
	if got := ix.MarkerSettled(id, ver(5)); !got {
		t.Fatalf("MarkerSettled after a settle at the loss's version = %v, want true: its marker is gone", got)
	}

	// A later loss at version 8 leaves the answer at 5 alone, and moves the
	// one at 8.
	if _, _, err := ix.Lose(id, ext(4096, 8), ver(8)); err != nil {
		t.Fatalf("Lose: %v", err)
	}
	if got := ix.MarkerSettled(id, ver(5)); !got {
		t.Fatalf("MarkerSettled at 5 beside a later loss at 8 = %v, want true: the removal at 5 is settled", got)
	}
	if got := ix.MarkerSettled(id, ver(8)); got {
		t.Fatalf("MarkerSettled at the new loss's version = %v, want false: its marker waits for the settle", got)
	}
	ix.fs.settleRemovals(id, ver(8))
	if got := ix.MarkerSettled(id, ver(8)); !got {
		t.Fatalf("MarkerSettled after a settle of the later loss = %v, want true", got)
	}

	// Two markers at once: the first settled, the second above it. The
	// answer below the second is false — one unsettled marker at or below
	// v names a removal Settle has not covered — and above the second,
	// after its settle, true again.
	if _, _, err := ix.Lose(id, ext(8192, 8), ver(12)); err != nil {
		t.Fatalf("Lose: %v", err)
	}
	if _, _, err := ix.Lose(id, ext(16384, 8), ver(16)); err != nil {
		t.Fatalf("Lose: %v", err)
	}
	if got := ix.MarkerSettled(id, ver(12)); got {
		t.Fatalf("MarkerSettled at 12 with the loss at 12 still waiting = %v, want false", got)
	}
	if got := ix.MarkerSettled(id, ver(16)); got {
		t.Fatalf("MarkerSettled at 16 with two unsettled markers below = %v, want false: one unsettled marker alone is enough", got)
	}
	// The earlier settle below them changed nothing for the queries at and
	// above the first unsettled loss; settling all of them moves the
	// answer back — none of the settle arguments below covers a later
	// removal.
	ix.fs.settleRemovals(id, ver(15))
	if got := ix.MarkerSettled(id, ver(12)); !got {
		t.Fatalf("MarkerSettled at 12 after a settle at 15 = %v, want true: both markers at or below 15 are settled", got)
	}
	if got := ix.MarkerSettled(id, ver(20)); got {
		t.Fatalf("MarkerSettled at 20 after a settle at 15 = %v, want false: the loss at 16 still waits for the settle", got)
	}
	ix.fs.settleRemovals(id, ver(20))
	if got := ix.MarkerSettled(id, ver(20)); !got {
		t.Fatalf("MarkerSettled at 20 after a settle of both = %v, want true", got)
	}
}
