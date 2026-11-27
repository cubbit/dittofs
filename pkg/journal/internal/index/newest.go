package index

import (
	"cmp"
	"slices"

	"github.com/cubbit/dittofs/pkg/journal/internal/core"
)

// newestRecord is the greatest record, in segment then offset order, that a
// held or retained piece of a file was put or kept from since the file's
// entry was made or the figure was last recomputed, with the hull [lo, hi) of
// every extent a piece of that record was given. Pieces only ever narrow, cut
// or merge with their own record's neighbours, and every new piece goes
// through heldSet.put or retainedSet.add, which raise the figure, so two
// facts hold under the file's lock: no piece of the file is from a record
// greater than at, and every piece from at lies inside [lo, hi). A piece from
// at found inside the hull therefore makes at the file's newest record, and
// none found there means no piece anywhere is from it.
type newestRecord struct {
	at     RecordAt
	lo, hi int64
}

// newer reports whether a is a later place than b: a later segment, or the
// same segment at a later offset.
func newer(a, b RecordAt) bool {
	return a.Segment > b.Segment || a.Segment == b.Segment && a.Offset > b.Offset
}

// raise notes a piece of extent e put or kept from record r. The zero record
// names none and is ignored.
func (n *newestRecord) raise(r RecordAt, e core.Extent) {
	switch {
	case r == (RecordAt{}):
	case n.at == (RecordAt{}) || newer(r, n.at):
		*n = newestRecord{at: r, lo: e.Off, hi: e.Off + e.Len}
	case r == n.at:
		n.lo, n.hi = min(n.lo, e.Off), max(n.hi, e.Off+e.Len)
	}
}

// newestNamed reports whether a held or retained piece of the file is still
// from f.newest's record, looking only inside its hull: the held pieces that
// overlap it, and the retained pieces that start in it. The caller holds the
// file's lock, shared or exclusive.
func (f *file) newestNamed() bool {
	n := f.newest
	if n.at == (RecordAt{}) || n.hi <= n.lo {
		return false
	}
	found := false
	f.held.visit(core.Extent{Off: n.lo, Len: n.hi - n.lo}, func(p Piece) bool {
		found = p.Record == n.at
		return !found
	})
	if found {
		return true
	}
	ps := f.retained.pieces
	i, _ := slices.BinarySearchFunc(ps, n.lo, func(r Retained, off int64) int { return cmp.Compare(r.Extent.Off, off) })
	for ; i < len(ps) && ps[i].Extent.Off < n.hi; i++ {
		if ps[i].Record == n.at {
			return true
		}
	}
	return false
}

// recomputeNewest rebuilds f.newest from every held and retained piece. The
// caller holds the file's lock exclusively.
func (f *file) recomputeNewest() {
	f.newest = newestRecord{}
	f.held.visit(whole, func(p Piece) bool {
		f.newest.raise(p.Record, p.Extent)
		return true
	})
	for _, r := range f.retained.pieces {
		f.newest.raise(r.Record, r.Extent)
	}
}

// Newest returns the newest record, in segment then offset order, that a held
// or retained piece of id is from: the greatest place FileEntries names. ok is
// false when id holds no such piece. It costs a lookup of the record's own
// extent while a piece of that record remains, and one walk of the file's
// pieces, under its exclusive lock, the first time after the last piece of it
// left.
func (ix *Index) Newest(id core.FileID) (at RecordAt, ok bool) {
	return ix.fs.newestRecord(id)
}

func (fs *files) newestRecord(id core.FileID) (RecordAt, bool) {
	var at RecordAt
	stale := false
	fs.view(id, func(f *file) {
		if f.newestNamed() {
			at = f.newest.at
		} else {
			stale = f.newest.at != (RecordAt{})
		}
	})
	if !stale {
		return at, at != (RecordAt{})
	}
	// The figure's record has no piece left: walk the file once, under its
	// exclusive lock, and keep what the walk finds. The walk changes no
	// entry, so the update's settling has nothing to do.
	_ = fs.note(id, func(f *file) error {
		if !f.newestNamed() {
			f.recomputeNewest()
		}
		at = f.newest.at
		return nil
	})
	return at, at != (RecordAt{})
}
