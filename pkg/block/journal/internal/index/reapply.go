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
// re-places — save the torn record's own, runs and marker alike, which go
// with the tear below.
//
// The survivors may carry two copies of one record — carried forward, or
// re-appended beside its original — and the fold keeps what the rebuild
// keeps, the one at the greater (segment, offset). A survivor whose reach
// meets no byte of the record's reach answers nothing.
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
	window := torn.Reach()
	if window.Len <= 0 {
		return // a reach over no byte displaced nothing to re-place
	}
	_, _ = ix.fs.update(id, func(f *file) error {
		f.repairTorn(window, torn.Numbers, survivors)
		return nil
	})
}

// repairTorn folds the survivors over f's held set inside the window, under
// the file's lock, and takes the torn record's own leavings with it: one
// precedence-ordered fold of placings, cuts and marks — the same fold the
// rebuild replays a file's records by — then the runs and the marker of the
// torn record, where no copy of it stayed among the survivors.
func (f *file) repairTorn(window core.Extent, torn Numbers, recs []Record) {
	kept, _ := f.fold(toBufs(recs), window) // a repair's fold cannot err: its removals cut, never the marking ops

	// The torn record's own runs and marker go with it: a supersede and a
	// removal nobody's copy keeps are not changes any later read may be
	// told, and "treated as never written" means the index must hold what a
	// rebuild that was never fed the record holds — a marker left by its
	// first replay would have Since yield a delete or truncate of content
	// the index actually holds. A removal or a mark raised no run, and the
	// runs of the writes that stayed are still true, so only the torn
	// record's own are dropped. A surviving copy among the survivors keeps
	// runs and marker alike: both name the record by version, never its
	// placement, and the surviving copy's supersede is as true as the
	// placement the verdict dropped was.
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
	if slices.ContainsFunc(kept, func(r bufRec) bool { return r.numbers == torn }) {
		return
	}
	f.held.dropOverBy(torn.Version, window)
	f.markers = slices.DeleteFunc(f.markers, func(m marker) bool { return m.version == torn.Version })
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
