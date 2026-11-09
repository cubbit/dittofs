package durability

import (
	"cmp"
	"slices"
)

// lostRecord is a record a failure took: a failed window dropped it, or a
// read-only journal failed it.
type lostRecord struct {
	// err is the failure the record is reported with: the error that ended
	// its segment, or the read-only refusal.
	err error
	// undone is set for a header-only record a read-only journal undid: every
	// Sync and WaitDurable that waits on it fails with err, bare, and so does
	// every Sync naming its file (stream.undone).
	undone bool
}

// failReadOnly fails every record of the stream that no sync covered, once the
// journal is read-only: each pending window's records, and the ledger's
// records past the stream's last successful sync, which it first moves into
// windows of their segments so they read pending throughout. Every window is
// then failed whole: content is dropped from the index and named in a loss
// event with no loss record appended, header-only
// records are undone through Config.Undo, a loss record's drop stands, and
// every failed record but a loss record joins the directory's failed list.
// The generation is raised once for the whole entry, across every stream and
// pass, before anything is published or answered.
//
// It runs on the stream's worker on every pass while the journal is
// read-only, so a record tracked after one pass, from an append admitted just
// before the entry, is failed by the next. A record a sync covered before the
// entry is not failed: the completion pass completes it. A sync reported after
// the entry covers nothing (the Synced sink drops it), so its records are
// failed here even when the sync succeeded.
//
// Each file's records are gathered from every window and taken newest first,
// in the order the stream tracked them, so every undo and drop finds the index
// as its record left it. Neither the windows' order nor their segments can
// stand in for that order: st.windows is in end order, a seal of an older
// segment can end after a newer one failed, the ledger can still hold an
// older segment's records, its seal in flight, while a newer segment's window
// is pending, and a segment's number says when its spare was made, not when
// the stream took it. A file's appends are serialized under its
// append domain and each is tracked inside its append, so the tracked order
// is the file's append order.
//
// decision: a read-only journal fails header-only records too, against the
// rule that a failed window keeps them for their re-append, because nothing
// can be appended. They are undone in memory rather than kept because the
// engine syncs a removal before its metadata transaction: a failed Sync means
// the removal did not happen, and a removal kept in memory would hide content
// the metadata still names. The ceiling is a restart while read-only: the
// failed list lives in the process, so the next start's recovery can find a
// failed truncate or deallocate whole, even one a sync in flight at the
// transition made durable, and apply it after its Sync was told it failed.
// Overturned by a journal that keeps a second device it can still append to,
// or by failed removals seen to survive a restart, which would record them in
// the metadata store.
func (d *Durability) failReadOnly(st *stream) {
	ro := d.Admit()
	if ro == nil {
		return
	}
	d.wakeOnce()

	st.mu.Lock()
	var fresh []*window
	st.removeLocked(func(e *entry) bool {
		return !e.done && !st.covered(e.w.Loc)
	}, func(e entry) {
		i := slices.IndexFunc(fresh, func(w *window) bool { return w.seg == e.w.Loc.Segment })
		if i < 0 {
			fresh = append(fresh, &window{id: d.windowSeq.Add(1), seg: e.w.Loc.Segment, err: ro, readOnly: true})
			i = len(fresh) - 1
		}
		fresh[i].entries = append(fresh[i].entries, e)
	})
	st.windows = append(st.windows, fresh...)
	ws := slices.Clone(st.windows)
	var all []entry
	for _, w := range ws {
		all = append(all, w.entries...)
	}
	st.mu.Unlock()
	if len(all) == 0 {
		// A window Corrupt took the last record out of holds nothing more
		// and is resolved here, as settle resolves one it empties.
		for _, w := range ws {
			d.resolve(st, w)
		}
		return
	}

	// A loss record alone loses nothing, so it raises nothing. The raise
	// lands before anything is published or answered.
	if slices.ContainsFunc(all, func(e entry) bool { return !isLoss(e.w) }) {
		d.raiseOnce()
	}
	if d.beforeFail != nil {
		for _, w := range ws {
			d.beforeFail(w)
		}
	}
	slices.SortStableFunc(all, func(a, b entry) int { return cmp.Compare(a.tracked, b.tracked) })
	files, byFile := byFileOf(all)
	for _, f := range files {
		d.failFile(st, 0, f, byFile[f], true)
	}
	every := func(entry) bool { return true }
	for _, w := range ws {
		if d.settle(st, w, every, ro, true) {
			d.resolve(st, w)
		}
	}
}
