package durability

import (
	"fmt"
	"slices"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/format"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/segment"
)

// resolveWindows resolves every pending window of the stream: it fails the
// window's content records and re-appends every other record to the stream's
// current segment, then resolves each window that holds nothing more. It runs
// on the worker, after the windows are taken.
//
// A record that cannot be re-appended stays in its window, which stays
// pending, and the next pass tries it again.
func (d *Durability) resolveWindows(st *stream) {
	st.mu.Lock()
	if len(st.windows) == 0 {
		st.mu.Unlock()
		return
	}
	ws := slices.Clone(st.windows)
	st.mu.Unlock()
	for _, w := range ws {
		d.failWindow(st, w)
		d.reappendWindow(st, w)
		st.mu.Lock()
		empty := len(w.entries) == 0
		st.mu.Unlock()
		if empty {
			d.resolve(st, w)
		}
	}
}

// reappendWindow re-appends the window's header-only records, in the order they
// were tracked, and stops at the first the stream refuses, so a record never
// lands ahead of one that came before it.
func (d *Durability) reappendWindow(st *stream, w *window) {
	st.mu.Lock()
	var rest []entry
	for i := range w.entries {
		if !isContent(w.entries[i].w) {
			rest = append(rest, w.entries[i])
		}
	}
	st.mu.Unlock()
	for _, e := range rest {
		if err := d.reappendEntry(st, w, e); err != nil {
			d.log.Error("journal could not re-append a failed window's record", "stream", st.no, "window", w.id, "seq", e.w.Seq, "kind", e.w.Kind, "err", err)
			return
		}
	}
}

// reappendEntry appends the header-only record e of the window w again, to
// the stream's current segment, from the bytes the ledger kept. The record
// keeps its sequence number and version: it is the same operation at a new
// place, so no generation is raised, no loss record is written and no event is
// published. The old place then leads to the new one, so a waiter on it
// follows the record, and the record leaves the window. A failed append
// changes nothing.
//
// st.mu is not held across the append, and no file's append domain is taken:
// the record touches no index entry, and an append may roll, seal and sync.
func (d *Durability) reappendEntry(st *stream, w *window, e entry) error {
	id := st.s.Identity()
	b := e.w.Bytes
	if len(b) < format.RecordHeaderSize {
		return fmt.Errorf("durability: the record of seq %d kept %d bytes, want its %d-byte header at least", e.w.Seq, len(b), format.RecordHeaderSize)
	}
	h, err := format.DecodeRecordHeader(b[:format.RecordHeaderSize], id, e.w.Loc.Segment, e.w.Loc.Off)
	if err != nil {
		return fmt.Errorf("durability: the kept record of seq %d does not decode: %w", e.w.Seq, err)
	}
	end := format.RecordHeaderSize + h.BodySize()
	if int64(len(b)) < end {
		return fmt.Errorf("durability: the kept record of seq %d has %d bytes, want %d", e.w.Seq, len(b), end)
	}
	body, err := format.VerifyRecordBody(b[format.RecordHeaderSize:end], id, h)
	if err != nil {
		return fmt.Errorf("durability: the kept record of seq %d does not verify: %w", e.w.Seq, err)
	}
	// The stream fills in the segment, offset and synced-through.
	nh := format.RecordHeader{
		Kind:    h.Kind,
		File:    h.File,
		Extent:  h.Extent,
		Seq:     h.Seq,
		Version: h.Version,
		Tag:     h.Tag,
	}
	p, err := st.s.Append(nh, body)
	if err != nil {
		return err
	}

	st.mu.Lock()
	defer st.mu.Unlock()
	i := slices.IndexFunc(w.entries, func(o entry) bool { return o.w.Loc == e.w.Loc })
	if i < 0 {
		// A read took the record out of the window meanwhile and reports it
		// itself; the copy just appended completes like any other record.
		return nil
	}
	w.entries = slices.Delete(w.entries, i, i+1)
	// The new record may already be synced and completed, which clears its
	// entry: then nothing is left to follow, and the old place, in no window
	// or ledger, reads as done.
	if st.heldLocked(p.Loc) {
		st.moved[e.w.Loc] = p.Loc
	}
	st.want = true
	st.d.notifyAll()
	return nil
}

// heldLocked reports whether the ledger, a pending window or the failed
// records hold the record at loc. The caller holds mu.
func (st *stream) heldLocked(loc segment.Location) bool {
	if _, lost := st.lost[loc]; lost || st.inWindowLocked(loc) {
		return true
	}
	return slices.ContainsFunc(st.ledger, func(e entry) bool { return e.w.Loc == loc })
}
