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

// repairTorn folds the survivors over f's held set inside the window, under
// the file's lock, and takes the torn record's own leavings with it: one
// precedence-ordered fold of placings, cuts and marks — the same fold the
// rebuild replays a file's records by — then the teardown of the torn
// record, answering its kind, where no copy of it stayed among the
// survivors.
func (f *file) repairTorn(window core.Extent, torn Record, recs []Record) {
	// The fold runs only over a reach that spans bytes: a zero window is
	// the rebuild's whole-file pass, which a repair never is. With no fold
	// run, kept is every survivor unclipped, so the surviving-copy guard
	// below reads the same rule as over a window — a record with these
	// exact numbers survives — where the fold's kept is the survivors the
	// window kept, the record's own copy among them.
	kept := toBufs(recs)
	if window.Len > 0 {
		kept, _ = f.fold(kept, window) // a repair's fold cannot err: its removals cut, never the marking ops
	}

	// A surviving copy keeps the torn record's leavings: a teardown never
	// runs while a record with these exact numbers stays among the
	// survivors, for a run and a marker name the record by version, never
	// its placement, and the surviving copy's supersede is as true as the
	// placement the verdict dropped was.
	//
	// The teardown answers the torn record's kind, for a mark carries the
	// version of the content it marked — the same version as a surviving
	// write, a different sequence number — and a teardown keyed on the
	// version alone would take the surviving write's runs with the mark's.
	// Runs come from writes, markers from removals: a write's own go at
	// both ends, the runs its supersede raised and the chains naming it as
	// the superseded one (a rebuild without the record chains the
	// surviving write over what it displaced — a chain the fold above
	// re-noted itself), and a removal's marker goes so Since yields no
	// delete or truncate of content the index actually holds. The kinds
	// beside a write — fill, release, offloaded, unmark — tear down
	// nothing: a mark changed bits alone, a fill's piece went with the
	// un-place, and a release raised nothing to tear down.
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
	if slices.ContainsFunc(kept, func(r bufRec) bool { return r.numbers == torn.Numbers }) {
		return
	}
	switch torn.Kind {
	case RecWrite:
		f.held.dropOverBy(torn.Numbers.Version, window)
		f.held.dropOverWas(torn.Numbers.Version, window)
	case RecTruncate, RecDeallocate, RecDelete, RecCloneTarget:
		f.markers = slices.DeleteFunc(f.markers, func(m marker) bool { return m.version == torn.Numbers.Version })
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
