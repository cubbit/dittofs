package durability

import (
	"context"
	"slices"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/format"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/index"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/segment"
)

// failWindows fails the content records of every pending window of the
// stream. It runs on the worker, after the windows are taken, while the
// journal takes writes; a read-only journal fails its windows in
// failReadOnly instead.
func (d *Durability) failWindows(st *stream) {
	st.mu.Lock()
	if len(st.windows) == 0 {
		st.mu.Unlock()
		return
	}
	ws := slices.Clone(st.windows)
	st.mu.Unlock()
	for _, w := range ws {
		d.failWindow(st, w)
	}
}

// recordAt is where the index places the record e was written at.
func recordAt(w segment.Written) index.RecordAt {
	return index.RecordAt{Segment: w.Loc.Segment, Offset: uint32(w.Loc.Off)}
}

// isLoss reports whether w is a loss record.
func isLoss(w segment.Written) bool { return w.Kind == format.KindLoss }

// failWindow fails the content records of a failed segment's window, raises
// the loss generation once for the window, names every dropped record in a
// loss record and an event, and resolves the window once it holds nothing
// more. It runs on the stream's worker. A read-only journal fails its windows
// in failReadOnly instead.
//
// The index is changed under the file's append domain, one file at a time, so
// a writer that appended into the failed segment and has not yet published
// does so before the drop, and the drop then finds the record. The loss
// records are appended with no domain held: an append may roll, seal and sync,
// and a read of the file must not wait on it. st.mu is never held across a
// call into the index or the domains.
//
// decision: outside a read-only journal only content records are failed, and
// a window that still holds another kind keeps it and stays pending, with the
// interim answer check gives. Such a record holds nothing in the index and has
// no content to lose, but its operation must not be forgotten, and it is the
// re-append of the record that carries it forward; failing it here would drop
// the operation. Withdrawn once that re-append exists: the record is then
// neither failed nor kept here. A read-only journal, which can append nothing,
// fails them too (failReadOnly).
func (d *Durability) failWindow(st *stream, w *window) {
	st.mu.Lock()
	var failing []entry
	for i := range w.entries {
		if isContent(w.entries[i].w) {
			failing = append(failing, w.entries[i])
		}
	}
	if len(failing) == 0 {
		// Nothing to drop and no generation to raise, but the window is
		// processed: a header-only record may now answer its Sync.
		if !w.failed {
			w.failed = true
			st.d.notifyAll()
		}
		st.mu.Unlock()
		return
	}
	st.mu.Unlock()
	if d.beforeFail != nil {
		d.beforeFail(w)
	}

	// Raised before anything is published or answered, so a reader that sees a
	// loss event, or a Sync that returns its failure, finds the generation up.
	d.RaiseWindow()

	files, byFile := byFileOf(failing)
	for _, f := range files {
		d.failFile(st, w.id, f, byFile[f], false)
	}
	d.settle(st, w, func(e entry) bool { return isContent(e.w) }, w.err, false)
}

// byFileOf groups entries by file, the files in the order they first appear
// and each file's entries in the order given.
func byFileOf(entries []entry) ([]core.FileID, map[core.FileID][]entry) {
	var files []core.FileID
	byFile := map[core.FileID][]entry{}
	for _, e := range entries {
		if _, ok := byFile[e.w.File]; !ok {
			files = append(files, e.w.File)
		}
		byFile[e.w.File] = append(byFile[e.w.File], e)
	}
	return files, byFile
}

// settle takes the entries fails selects out of the window, now that their
// index changes are made, records each as lost with err, marks every file
// owed a report, and resolves the window once it holds nothing more. ro says
// a read-only journal failed them: a header-only record among them was undone.
func (d *Durability) settle(st *stream, w *window, fails func(entry) bool, err error, ro bool) {
	// The records leave the window and every file's mark is set in one step,
	// so a waiter never sees a record gone with no report owed, and a Sync
	// naming several files of the window hears of all of them at once. A file
	// whose only failed records are loss records is not marked: a loss
	// record's drop stands, so its file is owed nothing. A file with an undone
	// header-only record is not marked for it: the file joins st.undone, so
	// every Sync naming it, then and later, returns the bare refusal and never
	// a *core.SyncError (check), and one waiting on the record fails through
	// st.lost. Its content records, if any, still mark it, and the refusal
	// then answers in place of that mark (await).
	st.mu.Lock()
	var marked []core.FileID
	for _, e := range w.entries {
		if !fails(e) {
			continue
		}
		undone := ro && !isContent(e.w) && !isLoss(e.w)
		st.lost[e.w.Loc] = lostRecord{err: err, undone: undone}
		switch {
		case undone:
			if st.undone == nil {
				st.undone = map[core.FileID]error{}
			}
			st.undone[e.w.File] = err
		case !isLoss(e.w) && !slices.Contains(marked, e.w.File):
			marked = append(marked, e.w.File)
		}
	}
	w.entries = slices.DeleteFunc(w.entries, fails)
	for _, f := range marked {
		st.markErrLocked(w, f, err)
	}
	w.failed = true
	empty := len(w.entries) == 0
	st.d.notifyAll()
	st.mu.Unlock()
	if d.afterFail != nil {
		d.afterFail()
	}
	if empty {
		d.resolve(st, w)
	}
}

// failFile fails the records of one file, given in the order they were
// appended: the index first, under the file's domain, then a loss record for
// each piece the failure dropped. win names the failed window in the log, 0
// for a read-only pass, which fails several windows at once and is logged as
// that pass instead. Outside a
// read-only journal every entry is content and is dropped in one step. In a
// read-only journal (ro) the entries are taken newest first, so each change
// is reversed on the index its record left: a run of content records is
// dropped together, a header-only record is undone through Config.Undo, and a
// loss record is left as it is. Every failed record but a loss record then
// joins the directory's failed list.
func (d *Durability) failFile(st *stream, win uint64, f core.FileID, entries []entry, ro bool) {
	var dropped []index.Piece
	var run []entry // content records, newest first
	flush := func() {
		if len(run) == 0 {
			return
		}
		slices.Reverse(run)
		dropped = append(dropped, d.failContent(st, win, f, run)...)
		run = run[:0]
	}

	// decision: a record that names no file, as a hold, unhold, stamp,
	// offloaded, unmark or forget record can, is undone under the zero file's
	// append domain. It is a placeholder: those records publish under no file
	// append domain, so holding this one excludes nothing their publish
	// holds, and an undo can meet such a record between its append and its
	// publish. Replaced by the lock each of those records publishes under,
	// once the code that appends them names it; until then the undo of one
	// can be lost if it races its own publish.
	unlock := d.domains.Lock(f)
	for i := len(entries) - 1; i >= 0; i-- {
		e := entries[i]
		switch {
		case isContent(e.w):
			run = append(run, e)
		case !ro || isLoss(e.w):
			// A loss record's drop stands: a loss only ever errs toward
			// fetching the extent again.
		default:
			flush()
			if d.undo != nil {
				d.accountDelta(d.undo(e.w))
			}
		}
	}
	flush()
	unlock()

	for _, e := range entries {
		if isContent(e.w) {
			d.loseRecord(e, dropped)
		}
		if ro && !isLoss(e.w) {
			d.dir.addFailed(e.w.Seq)
		}
	}
}

// failContent drops the content records of f from the index and returns the
// pieces the drop cut. The caller holds f's append domain.
func (d *Durability) failContent(st *stream, win uint64, f core.FileID, entries []entry) []index.Piece {
	recs := make([]index.WindowRecord, len(entries))
	for i, e := range entries {
		recs[i] = index.WindowRecord{Record: recordAt(e.w), Version: e.w.Version, Extent: e.w.Extent}
	}
	fw, hd, err := d.index.FailWindow(f, recs)
	if err != nil {
		// The drop did not happen, so the index may still hold the failed
		// bytes; the records are lost all the same.
		which := []any{"window", win}
		if win == 0 {
			which = []any{"pass", "read-only"}
		}
		d.log.Error("journal could not fail a window's records in the index",
			append([]any{"stream", st.no}, append(which, "file", f, "err", err)...)...)
	}
	d.accountDelta(hd)
	// A newer record that took over what a failed one had superseded holds it
	// retained until that record syncs. One that already synced was lifted
	// before the failed record handed it the parts, so it is lifted now; one
	// still in the ledger or a window lifts when it completes.
	for _, c := range fw.Covers {
		if !d.hasEntry(st, f, c) {
			d.lift(f, c.Version, c.Extent)
		}
	}
	return fw.Dropped
}

// hasEntry reports whether the stream's ledger or a pending window holds the
// content record a cover names: one of file at the cover's version whose
// extent holds the cover's part. A write split across segments has several
// records of one version, and a sibling's entry is not this record's.
func (d *Durability) hasEntry(st *stream, file core.FileID, c index.Cover) bool {
	st.mu.Lock()
	defer st.mu.Unlock()
	// Only content covers: a loss record's header carries the dropped
	// record's version and is not a cover.
	match := func(e entry) bool {
		return isContent(e.w) && e.w.File == file && e.w.Version == c.Version &&
			e.w.Extent.Off <= c.Extent.Off && c.Extent.Off+c.Extent.Len <= e.w.Extent.Off+e.w.Extent.Len
	}
	if slices.ContainsFunc(st.ledger, match) {
		return true
	}
	for _, o := range st.windows {
		if slices.ContainsFunc(o.entries, match) {
			return true
		}
	}
	return false
}

// loseRecord appends the loss record and publishes the event for each piece
// of e that the failure dropped. A record with no piece left, overwritten
// since it was appended, is named once by its header extent, so that every
// failed record has a loss record.
func (d *Durability) loseRecord(e entry, dropped []index.Piece) {
	at := recordAt(e.w)
	l := Loss{
		Share:   e.w.Tag,
		File:    e.w.File,
		Extent:  e.w.Extent,
		Version: e.w.Version,
		Seq:     e.w.Seq,
		Reason:  core.LossFailedWindow,
		Raised:  true,
	}
	named := false
	for _, p := range dropped {
		if p.Record != at {
			continue
		}
		named = true
		l.Extent, l.Offloaded = p.Extent, p.Offloaded
		d.lose(l)
	}
	if !named {
		d.lose(l)
	}
}

// lose hands the loss to Lose. An error leaves the drop in place: Lose
// publishes the event, lists the record as owed whatever it could not append,
// and logs the failure, so the error is dropped here.
func (d *Durability) lose(l Loss) {
	_, _ = d.Lose(context.Background(), l)
}
