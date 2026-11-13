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
	call func(*Index, core.FileID, core.Extent, core.Version) (HeldDelta, error)
	// yieldsLoss is whether Since yields a marker for the loss.
	yieldsLoss bool
}{
	{"superseded", (*Index).LoseSuperseded, false},
	{"synced", (*Index).Lose, true},
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
			hd, err := row.call(ix, id, ext(4, 4), ver(5))
			if err != nil {
				t.Fatalf("loss = %v, want no error", err)
			}
			if hd.File != id || hd.HeldBytes != -2 || hd.DirtyBytes != -2 {
				t.Fatalf("HeldDelta = %+v, want File %v, HeldBytes -2, DirtyBytes -2", hd, id)
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
			// inside e, so what is held is unchanged.
			if _, err := ix.LoseSuperseded(id, ext(4, 4), ver(5)); err != nil {
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
	hd, err := ix.LoseSuperseded(id, ext(4, 0), ver(5))
	if !errors.Is(err, core.ErrZeroLength) {
		t.Fatalf("superseded loss of a zero-length extent = %v, want ErrZeroLength", err)
	}
	if hd != (HeldDelta{}) {
		t.Fatalf("HeldDelta of a refused loss = %+v, want the zero delta", hd)
	}
	if want := []Piece{{Extent: ext(0, 12), Record: a, Version: ver(3)}}; !slices.Equal(heldPieces(&ix.fs, id), want) {
		t.Fatalf("held after the refusal:\n got  %+v\n want %+v", heldPieces(&ix.fs, id), want)
	}
	if n := markerCount(&ix.fs, id); n != 0 {
		t.Fatalf("markers = %d after a refused superseded loss, want 0", n)
	}
}
