package index

import (
	"cmp"
	"slices"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
)

// ReplayWindow re-applies to id, inside the extent window alone, the records
// recs reach there — the answer set of a repair, already exclusive of the
// record whose drop called for it — exactly as the rebuild that first applied
// them would: every kind's reach is clipped to the window, the clip deciding
// each on its own rule (a truncate's and a delete's removal runs to the end of
// the file), writes and fills hold their pieces in precedence order — the
// greatest of the records reaching a byte holds it, last applied wins being
// exactly precedence — removals cut the runs they reach, and the marks set
// and clear the offloaded bit in their own pass after the pieces, in the
// order of their sequence numbers. Nothing outside the window changes and
// nothing a record noted on its first pass is re-noted: the drop-range notes,
// the counters and the removal markers the records raised the first time stay
// as they were, for the notes name the record, not its placement, and the
// repair only re-places. The over runs of the write at version torn — the
// repair's one record, whose pieces the caller has already unplaced — go with
// it, exactly as a failed window drops the failed write's.
//
// recs may carry two copies of one record — carried forward, or re-appended
// beside its original — and the call keeps what the rebuild keeps, the one at
// the greater (segment, offset). A rec whose reach meets no byte of the window
// answers nothing.
//
// It takes the file's lock, creates no entry for a file without one, and
// joins no piece across the window's ends.
func (ix *Index) ReplayWindow(id core.FileID, window core.Extent, torn core.Version, recs []Record) {
	if window.Len <= 0 {
		return
	}
	if !ix.fs.view(id, func(*file) {}) {
		return
	}
	_, _ = ix.fs.update(id, func(f *file) error {
		f.replayWindow(window, torn, recs)
		return nil
	})
}

// replayWindow folds recs over f's held set inside the window, under the
// file's lock, as ReplayWindow's contract spells it: one pass of placings and
// cuts in precedence order, one pass of marks in sequence order, then the
// over runs of the dropped write gone.
func (f *file) replayWindow(window core.Extent, torn core.Version, recs []Record) {
	lo, hi := window.Off, window.Off+window.Len
	// windowed clips e to the window, or answers nothing where they meet at
	// no byte; the empty extent Len 0 never reaches the held set below.
	windowed := func(e core.Extent) core.Extent {
		l, h := max(e.Off, lo), min(e.Off+e.Len, hi)
		if l >= h {
			return core.Extent{Len: 0}
		}
		return core.Extent{Off: l, Len: h - l}
	}
	// reachOf answers what a record's replay reached, before the clip: its
	// kind's own extent, or the removal to the end a truncate's and a
	// delete's reach names, exactly as the rebuild replays the kind.
	reachOf := func(r Record) core.Extent {
		switch r.Kind {
		case RecTruncate:
			return toEnd(r.Off)
		case RecDelete:
			return toEnd(0)
		default:
			return core.Extent{Off: r.Off, Len: r.Len}
		}
	}

	order := slices.Clone(recs)
	slices.SortFunc(order, func(a, b Record) int {
		// The precedence the rebuild orders by, then the placement tie-break
		// that keeps its order total over distinct copies of one record.
		return cmp.Or(
			a.Numbers.Compare(b.Numbers),
			cmp.Compare(a.Segment, b.Segment),
			cmp.Compare(a.At, b.At),
		)
	})
	// decision: records with equal numbers are one record, whatever its copy
	// count, and the greater (segment, offset) is kept — the rebuild's own
	// rule, so the repair's placement names the same record's home the
	// rebuild's would. Both copies were noted on the record's first pass and
	// the notes stay, so the drop ranges lose nothing here.
	kept := order[:0]
	for i := 0; i < len(order); {
		j := i + 1
		for j < len(order) && order[i].Numbers == order[j].Numbers {
			j++
		}
		kept = append(kept, order[j-1])
		i = j
	}
	clear(order[len(kept):])
	order = kept

	marks := make([]Record, 0, len(order))
	for _, r := range order {
		w := windowed(reachOf(r))
		if w.Len == 0 {
			continue
		}
		switch r.Kind {
		case RecWrite, RecFill:
			f.held.put(Piece{
				Extent: w,
				Record: RecordAt{Segment: r.Segment, Offset: r.At},
				// The payload starts at the extent's first byte, so a clipped
				// piece's skip is the clip's own head; the rebuild places the
				// unclipped piece with skip 0, and this names the same bytes.
				Skip:    uint32(w.Off - r.Off),
				Version: r.Numbers.Version,
				Seq:     r.Numbers.Seq,
			})
			if r.Kind == RecFill {
				marks = append(marks, r) // its own mark, as the rebuild's bit pass takes it
			}
		case RecRelease, RecTruncate, RecDeallocate, RecDelete, RecCloneTarget:
			// A removal cut the pieces its reach covered at its place in the
			// precedence: everything already applied below it, and nothing
			// above — the records above re-place theirs. The marker it left
			// the first time stays: it names the removal, not one placement.
			f.held.cut(w)
		case RecOffloaded, RecUnmark:
			marks = append(marks, r)
		}
	}

	// The bit pass, over the pieces the fold built and in the order of the
	// marks' sequence numbers, as the rebuild's own: the highest-seq mark
	// covering a piece at or above its version decides its bit. Every piece
	// inside the window entered by a put, bit unset, so the fold of the marks
	// reaching the window decides every bit in it from the same start the
	// rebuild's fold began from.
	slices.SortStableFunc(marks, func(a, b Record) int {
		return cmp.Or(
			cmp.Compare(a.Numbers.Seq, b.Numbers.Seq),
			cmp.Compare(a.Segment, b.Segment),
			cmp.Compare(a.At, b.At),
		)
	})
	for _, m := range marks {
		w := windowed(core.Extent{Off: m.Off, Len: m.Len})
		if w.Len == 0 {
			continue
		}
		if m.Kind == RecUnmark {
			f.held.unmark(w, m.Numbers.Version)
		} else {
			f.held.markOffloaded(w, core.Version{}, m.Numbers.Version)
		}
	}

	// The dropped write's runs go with it: a supersede nobody made is not a
	// change any later read may be told. A removal or a mark raised no run,
	// and the runs of the writes that stayed are still true, so only the
	// dropped write's are dropped.
	f.held.dropOverBy(torn, window)
}
