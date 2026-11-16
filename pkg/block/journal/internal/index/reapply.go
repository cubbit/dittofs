package index

import (
	"slices"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
)

// DropTorn repairs one torn record the open judged never written: the
// placement at — segment and offset where the record sits — loses the
// pieces it placed inside the record's extent, then everything its replay
// displaced inside the record's reach is re-placed from the survivors —
// the answer set of a repair, already exclusive of the torn record's own
// placement, a surviving copy of the same record included — exactly as the
// rebuild that first applied them would: every kind's reach clips to the
// reach's window, the clip deciding each on its own rule (a truncate's and a
// delete's removal runs to the end of the file), writes and fills hold
// their pieces in precedence order — the greatest of the records reaching a
// byte holds it, last applied wins being exactly precedence — removals cut
// the runs they reach, and the marks set and clear the offloaded bit in
// their own pass after the pieces, in the order of their sequence numbers.
// Nothing outside the record's reach changes and nothing a record noted on
// its first pass is re-noted: the drop-range notes, the counters and the
// removal markers the records raised the first time stay as they were, for
// the notes name the record, not its placement, and the repair only
// re-places — save the torn record's own, as the teardown by its kind
// below takes it.
//
// A reach over no byte displaces nothing to re-place, but its teardown
// still runs: a truncate past every offset left a marker of length 0 that
// Since yields, and a mark or a write of the shape would have changed
// nothing but its state, which the fold would have re-placed over no
// window. The survivors may carry two copies of one record — carried
// forward, or re-appended beside its original — and the fold keeps what the
// rebuild keeps, the one at the greater (segment, offset). A survivor whose
// reach meets no byte of the record's reach answers nothing unless it is
// the record itself, whose copy keeps the teardown from running.
//
// The two steps take the file's lock in the order the open applies them:
// the un-place settles first, as a cut does, so a drop that empties the
// held set frees it with its runs before the fold runs — the fold cannot
// bring those runs back. The call creates no entry for a file without one,
// and joins no piece across the reach's ends.
func (ix *Index) DropTorn(id core.FileID, at RecordAt, torn Record, survivors []Record) {
	if !ix.fs.view(id, func(*file) {}) {
		return
	}
	_, _ = ix.fs.update(id, func(f *file) error {
		f.held.unplace(at, core.Extent{Off: torn.Off, Len: torn.Len})
		return nil
	})
	_, _ = ix.fs.update(id, func(f *file) error {
		f.repairTorn(torn.Reach(), torn, survivors)
		return nil
	})
}

// repairTorn takes the torn record's own leavings under the file's lock,
// then folds the survivors over f's held set inside the window: the
// teardown FIRST, answering the record's kind, so the fold — one
// precedence-ordered fold of placings, cuts and marks, the same fold the
// rebuild replays a file's records by — re-notes the surviving chain's runs
// after it, exactly the runs a rebuild never fed the torn record holds.
// With no copy of it among the survivors the teardown runs in every kind
// that raised something; the fold runs only over a reach that spans bytes,
// for a zero window is the rebuild's whole-file pass, which a repair never
// is.
func (f *file) repairTorn(window core.Extent, torn Record, recs []Record) {
	// The surviving-copy guard reads the survivors the caller handed over,
	// before the fold: a fold over a window returns the survivors its
	// equal-numbers collapse kept — every numbers the input held, a copy
	// whose reach misses the window included — and with no fold run at all
	// the input is the survivors itself, so the guard answers the same
	// either way.
	//
	// A surviving copy keeps the torn record's leavings: a teardown never
	// runs while a record with these exact numbers stays among the
	// survivors, for a run and a marker name the record by version, never
	// its placement, and the surviving copy's supersede is as true as the
	// placement the verdict dropped was.
	//
	// The teardown answers the torn record's kind, and runs before the
	// fold: dropOverWas and dropOverBy match the version alone, and a fill
	// carries the write's version — the same one the torn write holds — so
	// a teardown that ran after the fold would take the chains the fold
	// just re-noted for the surviving writes (a rebuild without the record
	// chains the surviving write over what it displaced, and the fold
	// re-notes that chain itself). Runs come from writes, markers from
	// removals: a write goes at both ends — the runs its supersede raised
	// and the chains naming it as the superseded one — while a fill raised
	// no run of its own but the writes over it named it as theirs, so its
	// teardown takes the chains alone; a removal's marker goes so Since
	// yields no delete or truncate of content the index actually holds. A
	// mark carries the version of the content it marked — the same version
	// as a surviving write, a different sequence number — so the removals
	// alone key the marker teardown, never the marks; and the kinds that
	// raised nothing at all — release, offloaded, unmark — tear down
	// nothing: a mark changed bits alone, and every bit went with the piece
	// it sat on under the un-place.
	//
	// decision: this keeps only the runs and markers that are still here. A
	// drop that emptied the file's held set lost the runs already — the
	// un-place's settle frees the empty set with its runs, whichever copy of
	// the record survives — and the fold re-notes nothing, so an open that
	// un-places a record covering the file's whole held content leaves no
	// run of it even where a good copy re-places. Widening the settle to keep
	// an emptied set's runs changes what Since answers over a live journal's
	// releases, so it is not taken from here; revisit if a torn-drop repair
	// is ever required to report the supersede of a surviving copy
	// whole-file.
	if !slices.ContainsFunc(recs, func(r Record) bool { return r.Numbers == torn.Numbers }) {
		switch torn.Kind {
		case RecWrite:
			f.held.dropOverBy(torn.Numbers.Version, window)
			f.held.dropOverWas(torn.Numbers.Version, window)
		case RecFill:
			f.held.dropOverWas(torn.Numbers.Version, window)
		case RecTruncate, RecDeallocate, RecDelete, RecCloneTarget:
			f.markers = slices.DeleteFunc(f.markers, func(m marker) bool { return m.version == torn.Numbers.Version })
		}
	}
	if window.Len > 0 {
		_, _ = f.fold(toBufs(recs), window) // a repair's fold cannot err: its removals cut, never the marking ops
	}
}

// toBufs adapts the repair's records to the fold's buffered shape —
// everything the fold reads of each, nothing the repair has no use for —
// taking the copy the fold's sort then owns.
func toBufs(recs []Record) []bufRec {
	out := make([]bufRec, len(recs))
	for i, r := range recs {
		out[i] = bufRec{
			kind: r.Kind, seg: r.Segment, at: r.At,
			off: r.Off, n: r.Len, numbers: r.Numbers, clone: r.Clone,
		}
	}
	return out
}
