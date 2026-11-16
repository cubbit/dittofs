package index

import (
	"errors"
	"slices"
	"testing"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
)

// lossCalls are the two synced-loss calls over the same content and the same
// extent: LoseSuperseded, for a loss record whose reason is that the content
// was superseded, and Lose, for every other reason, the negative control.
var lossCalls = []struct {
	name string
	call func(*Index, core.FileID, core.Extent, core.Version, core.Seq) (HeldDelta, []core.SegmentID, error)
	// yieldsLoss is whether Since yields a marker for the loss.
	yieldsLoss bool
}{
	{"superseded", (*Index).LoseSuperseded, false},
	{"synced", func(ix *Index, id core.FileID, e core.Extent, v core.Version, _ core.Seq) (HeldDelta, []core.SegmentID, error) {
		return ix.Lose(id, e, v)
	}, true},
}

// TestSupersededLossCutsItsExtentWithNoMarker checks that a superseded synced
// loss applies the cut any synced loss does — content below its version inside
// its extent stops being held, content at or above it and outside the extent
// are untouched — and leaves no marker for it, so Since yields nothing for the
// drop metadata itself asked for and no settle ever names it.
func TestSupersededLossCutsItsExtentWithNoMarker(t *testing.T) {
	for _, row := range lossCalls {
		t.Run(row.name, func(t *testing.T) {
			ix := New(1 << 20)
			id := fid(1)
			a, b := rec(1, 0), rec(2, 0)
			change(t, &ix.fs, id, func(f *file) error {
				// a at v3 crosses e's lower end, b at v5 its upper end: a is
				// below the loss's version inside e, b is at it.
				f.write(write(0, 6, a, 3))
				f.write(write(6, 4, b, 5))
				return nil
			})
			// The named seq holds nothing in this row, so the superseded
			// arm's extra clause is inert and the two legs cut alike.
			hd, segs, err := row.call(ix, id, ext(4, 4), ver(5), 4)
			if err != nil {
				t.Fatalf("loss = %v, want no error", err)
			}
			if hd.File != id || hd.HeldBytes != -2 || hd.DirtyBytes != -2 {
				t.Fatalf("HeldDelta = %+v, want File %v, HeldBytes -2, DirtyBytes -2", hd, id)
			}
			// The superseded cut names the segments it lowered exactly as a
			// markered loss does over the same content: the one record the
			// cut took a piece of.
			if !slices.Equal(segs, []core.SegmentID{1}) {
				t.Fatalf("%s named segments %v, want [1]: the cut lowered a's segment alone", row.name, segs)
			}
			want := []Piece{
				{Extent: ext(0, 4), Record: a, Skip: 0, Version: ver(3)},
				{Extent: ext(6, 4), Record: b, Version: ver(5)},
			}
			if got := heldPieces(&ix.fs, id); !slices.Equal(got, want) {
				t.Fatalf("held after the loss:\n got  %+v\n want %+v", got, want)
			}
			var losses []Change
			for _, ch := range ix.Since(id, ver(0)) {
				if ch.Kind == core.ChangeLoss {
					losses = append(losses, ch)
				}
			}
			if !row.yieldsLoss {
				if len(losses) != 0 {
					t.Fatalf("since yielded %+v for a superseded loss, want no loss marker", losses)
				}
			} else if want := []Change{{Kind: core.ChangeLoss, Extent: ext(4, 4), Version: ver(5)}}; !sameChanges(losses, want) {
				t.Fatalf("since's loss markers = %+v, want the one over (4, 8) at v5", losses)
			}
			if got, want := ix.MarkerSettled(id, ver(5)), !row.yieldsLoss; got != want {
				t.Fatalf("MarkerSettled(v5) = %v, want %v", got, want)
			}
			if row.yieldsLoss {
				return
			}
			// The same loss again cuts nothing more: no piece below v5 remains
			// inside e and the seq it names holds nothing, so what is held
			// is unchanged.
			if _, _, err := ix.LoseSuperseded(id, ext(4, 4), ver(5), 4); err != nil {
				t.Fatalf("second superseded loss = %v, want no error", err)
			}
			if got := heldPieces(&ix.fs, id); !slices.Equal(got, want) {
				t.Fatalf("held after the second loss:\n got  %+v\n want %+v", got, want)
			}
			// Settling afterwards changes nothing: there is no marker to drop
			// and nothing else a settle touches for this file.
			before := ix.Since(id, ver(0))
			ix.SettleFor(7, id, ver(5))
			if after := ix.Since(id, ver(0)); !sameChanges(after, before) {
				t.Fatalf("since after SettleFor:\n got  %+v\n want the same %+v", after, before)
			}
			if !ix.MarkerSettled(id, ver(0)) {
				t.Fatal("MarkerSettled(v0) = false after a settle over a superseded loss, want true")
			}
		})
	}
}

// TestSupersededLossRaisesTheChangeVersion checks the half of the superseded
// cut that holds the file's later calls to it: cutting below its version
// raises the file's change version, as a markered loss does, so a fill
// resolved against a read taken before the loss is refused stale instead of
// re-holding, at the fill's version, the very bytes the cut dropped. Without
// the raise, the fill carries no marker to be masked by — the cut leaves
// none — and wins the file back.
func TestSupersededLossRaisesTheChangeVersion(t *testing.T) {
	ix := New(1 << 20)
	id := fid(1)
	a := rec(1, 0)
	change(t, &ix.fs, id, func(f *file) error {
		f.write(write(0, 12, a, 3))
		return nil
	})
	if _, _, err := ix.LoseSuperseded(id, ext(4, 4), ver(9), 4); err != nil {
		t.Fatalf("LoseSuperseded = %v, want no error", err)
	}
	if _, err := ix.Fill(7, id, write(4, 4, rec(2, 0), 6), 30, ver(4)); !errors.Is(err, core.ErrStaleFill) {
		t.Fatalf("Fill resolved before a superseded loss = %v, want ErrStaleFill: the cut raised the change version the staleness check reads", err)
	}
	// The refused fill re-held nothing: the extent the cut dropped stays unheld.
	want := []Piece{
		{Extent: ext(0, 4), Record: a, Version: ver(3)},
		{Extent: ext(8, 4), Record: a, Skip: 8, Version: ver(3)},
	}
	if got := heldPieces(&ix.fs, id); !slices.Equal(got, want) {
		t.Fatalf("held after the refused fill:\n got  %+v\n want the pre-loss write split around the cut extent", got)
	}
}

// TestSupersededLossRefusesAZeroLengthExtent checks that a superseded loss of
// a zero-length extent is refused with ErrZeroLength and changes nothing, as
// every synced loss is.
func TestSupersededLossRefusesAZeroLengthExtent(t *testing.T) {
	ix := New(1 << 20)
	id := fid(1)
	a := rec(1, 0)
	change(t, &ix.fs, id, func(f *file) error {
		f.write(write(0, 12, a, 3))
		return nil
	})
	hd, segs, err := ix.LoseSuperseded(id, ext(4, 0), ver(5), 0)
	if !errors.Is(err, core.ErrZeroLength) {
		t.Fatalf("superseded loss of a zero-length extent = %v, want ErrZeroLength", err)
	}
	if hd != (HeldDelta{}) {
		t.Fatalf("HeldDelta of a refused loss = %+v, want the zero delta", hd)
	}
	if segs != nil {
		t.Fatalf("segments of a refused loss = %v, want nil", segs)
	}
	if want := []Piece{{Extent: ext(0, 12), Record: a, Version: ver(3)}}; !slices.Equal(heldPieces(&ix.fs, id), want) {
		t.Fatalf("held after the refusal:\n got  %+v\n want %+v", heldPieces(&ix.fs, id), want)
	}
	if n := markerCount(&ix.fs, id); n != 0 {
		t.Fatalf("markers = %d after a refused superseded loss, want 0", n)
	}
}

// TestSupersededLossExcludesTheNamedRecordOverItsExtent pins the cut's own
// rule at the writer's convention, where the loss record's version is the
// dropped record's own: the version clause alone never reaches the named
// content, and the sequence clause is what excludes it over the loss
// extent. Four rows over one shape: the named record's piece over e goes
// and its remainder stays; older content under the loss's version goes as
// any cut takes it; a different record at the loss's exact version — one
// the loss does not name — stays held over e, so the name is a sequence
// match, never a version equality; and the named record outside e is
// untouched, the loss answering only over the extent its header carries.
func TestSupersededLossExcludesTheNamedRecordOverItsExtent(t *testing.T) {
	cut := func(ix *Index, id core.FileID, e core.Extent, v core.Version, dropped core.Seq) error {
		t.Helper()
		_, _, err := ix.LoseSuperseded(id, e, v, dropped)
		return err
	}
	got := func(ix *Index, id core.FileID) []Piece {
		t.Helper()
		return heldPieces(&ix.fs, id)
	}
	t.Run("the named record is excluded over e by its sequence", func(t *testing.T) {
		ix := New(1 << 20)
		id := fid(1)
		r := rec(2, 0)
		change(t, &ix.fs, id, func(f *file) error {
			f.write(Placed{Extent: ext(0, 128), Record: r, Version: ver(5), Seq: 2})
			return nil
		})
		if err := cut(ix, id, ext(0, 64), ver(5), 2); err != nil {
			t.Fatalf("LoseSuperseded = %v, want no error", err)
		}
		want := []Piece{
			{Extent: ext(64, 64), Record: r, Skip: 64, Version: ver(5), Seq: 2},
		}
		if got := got(ix, id); !slices.Equal(got, want) {
			t.Fatalf("held after the named cut:\n got  %+v\n want %+v: the piece over the loss extent goes by its sequence, the remainder stays", got, want)
		}
	})
	t.Run("older content over e goes as the version clause takes it", func(t *testing.T) {
		ix := New(1 << 20)
		id := fid(1)
		q := rec(1, 0)
		change(t, &ix.fs, id, func(f *file) error {
			f.write(Placed{Extent: ext(0, 128), Record: q, Version: ver(3), Seq: 1})
			return nil
		})
		if err := cut(ix, id, ext(0, 64), ver(5), 2); err != nil {
			t.Fatalf("LoseSuperseded = %v, want no error", err)
		}
		want := []Piece{
			{Extent: ext(64, 64), Record: q, Skip: 64, Version: ver(3), Seq: 1},
		}
		if got := got(ix, id); !slices.Equal(got, want) {
			t.Fatalf("held after the version cut:\n got  %+v\n want %+v: the content below the loss's version over e goes, the rest stays", got, want)
		}
	})
	t.Run("a different record at the loss's version stays", func(t *testing.T) {
		ix := New(1 << 20)
		id := fid(1)
		x := rec(3, 0)
		change(t, &ix.fs, id, func(f *file) error {
			// X carries the loss's exact version over e and a sequence the
			// loss does not name: the loss said the dropped content was
			// superseded, and nothing about its neighbours.
			f.write(Placed{Extent: ext(0, 64), Record: x, Version: ver(5), Seq: 70})
			return nil
		})
		if err := cut(ix, id, ext(0, 64), ver(5), 2); err != nil {
			t.Fatalf("LoseSuperseded = %v, want no error", err)
		}
		want := []Piece{
			{Extent: ext(0, 64), Record: x, Version: ver(5), Seq: 70},
		}
		if got := got(ix, id); !slices.Equal(got, want) {
			t.Fatalf("held after the cut naming another record:\n got  %+v\n want %+v: content at the loss's version the loss does not name stays", got, want)
		}
	})
	t.Run("the named record outside e is untouched", func(t *testing.T) {
		ix := New(1 << 20)
		id := fid(1)
		r := rec(2, 128)
		change(t, &ix.fs, id, func(f *file) error {
			f.write(Placed{Extent: ext(128, 64), Record: r, Version: ver(5), Seq: 2})
			return nil
		})
		if err := cut(ix, id, ext(0, 64), ver(5), 2); err != nil {
			t.Fatalf("LoseSuperseded = %v, want no error", err)
		}
		want := []Piece{
			{Extent: ext(128, 64), Record: r, Version: ver(5), Seq: 2},
		}
		if got := got(ix, id); !slices.Equal(got, want) {
			t.Fatalf("held after the cut over another extent:\n got  %+v\n want %+v: the loss answers only over the extent its header carries", got, want)
		}
	})
}
