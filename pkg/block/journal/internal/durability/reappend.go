package durability

import (
	"errors"
	"fmt"
	"io"
	"slices"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/format"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/segment"
)

// errUnverified is a content record that cannot be re-appended because the
// bytes of its failed segment, read again, do not verify against the checksum
// the ledger kept: it is failed, not retried.
var errUnverified = errors.New("durability: the record does not verify on re-read")

// resolveWindows resolves every pending window of the stream, oldest first:
// each record is re-appended to the stream's current segment, unless it is a
// content record whose re-read does not verify, which is failed. A window that
// then holds nothing more is resolved. It runs on the worker, after the
// windows are taken.
//
// A record the stream refuses stays in its window, which stays pending, and
// the pass ends there, so a record never lands ahead of one that came before
// it, in its window or an earlier one; the next pass tries it again.
func (d *Durability) resolveWindows(st *stream) {
	st.mu.Lock()
	if len(st.windows) == 0 {
		st.mu.Unlock()
		return
	}
	ws := slices.Clone(st.windows)
	st.mu.Unlock()
	for _, w := range ws {
		refused := d.reappendWindow(st, w)
		st.mu.Lock()
		empty := len(w.entries) == 0
		st.mu.Unlock()
		if empty {
			d.resolve(st, w)
		}
		if refused {
			return
		}
	}
}

// reappendWindow re-appends the window's records in the order they were
// tracked, stops at the first the stream refuses, and fails the content
// records whose re-read did not verify, once, with the generation raised once
// for them. It reports whether the stream refused a record.
func (d *Durability) reappendWindow(st *stream, w *window) (refused bool) {
	st.mu.Lock()
	rest := slices.Clone(w.entries)
	st.mu.Unlock()
	var unverified []entry
	for _, e := range rest {
		err := d.reappendEntry(st, w, e)
		if err == nil {
			continue
		}
		if errors.Is(err, errUnverified) {
			d.log.Error("journal could not verify a failed window's record on re-read", "stream", st.no, "window", w.id, "seq", e.w.Seq, "err", err)
			unverified = append(unverified, e)
			continue
		}
		d.log.Error("journal could not re-append a failed window's record", "stream", st.no, "window", w.id, "seq", e.w.Seq, "kind", e.w.Kind, "err", err)
		refused = true
		break
	}
	d.failEntries(st, w, unverified)
	return refused
}

// reappendEntry appends the record e of the window w again, to the stream's
// current segment. A header-only record is appended from the bytes the ledger
// kept; a content record from its failed segment's bytes, read again and
// verified. The record keeps its sequence number and version: it is the same
// operation at a new place, so no generation is raised, no loss record is
// written and no event is published. The old place then leads to the new one,
// so a waiter on it follows the record, and the record leaves the window. A
// failed append changes nothing. A content record that does not verify
// returns errUnverified.
//
// st.mu is not held across the re-read or the append, and no file's append
// domain is taken: the record touches no index entry, and an append may roll,
// seal and sync.
func (d *Durability) reappendEntry(st *stream, w *window, e entry) error {
	id := st.s.Identity()
	var h format.RecordHeader
	var body format.RecordBody
	var err error
	if isContent(e.w) {
		h, body, err = d.rereadRecord(st, e)
	} else {
		h, body, err = decodeKept(id, e)
	}
	if err != nil {
		return err
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

// decodeKept decodes the header-only record e from the bytes the ledger kept.
func decodeKept(id core.JournalID, e entry) (format.RecordHeader, format.RecordBody, error) {
	b := e.w.Bytes
	if len(b) < format.RecordHeaderSize {
		return format.RecordHeader{}, format.RecordBody{}, fmt.Errorf("durability: the record of seq %d kept %d bytes, want its %d-byte header at least", e.w.Seq, len(b), format.RecordHeaderSize)
	}
	h, err := format.DecodeRecordHeader(b[:format.RecordHeaderSize], id, e.w.Loc.Segment, e.w.Loc.Off)
	if err != nil {
		return format.RecordHeader{}, format.RecordBody{}, fmt.Errorf("durability: the kept record of seq %d does not decode: %w", e.w.Seq, err)
	}
	end := format.RecordHeaderSize + h.BodySize()
	if int64(len(b)) < end {
		return format.RecordHeader{}, format.RecordBody{}, fmt.Errorf("durability: the kept record of seq %d has %d bytes, want %d", e.w.Seq, len(b), end)
	}
	body, err := format.VerifyRecordBody(b[format.RecordHeaderSize:end], id, h)
	if err != nil {
		return format.RecordHeader{}, format.RecordBody{}, fmt.Errorf("durability: the kept record of seq %d does not verify: %w", e.w.Seq, err)
	}
	return h, body, nil
}

// rereadRecord reads the content record e again from its failed segment and
// verifies it: its header at its own place and naming its sequence number, and
// its body against the payload checksum the ledger kept from the write, never
// the one read back with it, so bytes that agree only with themselves are
// refused. It runs with no lock of this layer held. Anything that does not
// verify returns errUnverified.
//
// decision: a failed segment's bytes are trusted only after they verify, and
// no copy of a record's content is kept, so a record the pages do not
// reproduce whole is failed. A read error of any kind (EIO, a short read, a
// segment that is gone) counts as unverified, where readCopy treats a
// transient error as deciding nothing: here a record held for a retry keeps its
// whole window, and every Sync waiting on it, pending. The ceiling is the
// records of a segment whose sync failed and whose pages the kernel drops, or
// a read that fails once and would succeed again. Overturned by a bounded
// in-memory copy of recent content, or by a ruling that an EIO re-read
// retries.
func (d *Durability) rereadRecord(st *stream, e entry) (format.RecordHeader, format.RecordBody, error) {
	unverified := func(why string, err error) (format.RecordHeader, format.RecordBody, error) {
		if err != nil {
			return format.RecordHeader{}, format.RecordBody{}, fmt.Errorf("%w: seq %d at %+v: %s: %w", errUnverified, e.w.Seq, e.w.Loc, why, err)
		}
		return format.RecordHeader{}, format.RecordBody{}, fmt.Errorf("%w: seq %d at %+v: %s", errUnverified, e.w.Seq, e.w.Loc, why)
	}
	if e.w.Loc.Len < format.RecordHeaderSize {
		return unverified("the place is shorter than a header", nil)
	}
	buf := make([]byte, e.w.Loc.Len)
	read := st.s.ReadAt
	if d.readCopyAt != nil {
		read = d.readCopyAt
	}
	n, err := read(e.w.Loc.Segment, buf, e.w.Loc.Off)
	// A whole read that also reports EOF read every byte asked for.
	if n == len(buf) && errors.Is(err, io.EOF) {
		err = nil
	}
	if err != nil {
		return unverified("the read failed", err)
	}
	if n < len(buf) {
		return unverified(fmt.Sprintf("short read of %d of %d bytes", n, len(buf)), nil)
	}
	id := st.s.Identity()
	h, err := format.DecodeRecordHeader(buf[:format.RecordHeaderSize], id, e.w.Loc.Segment, e.w.Loc.Off)
	if err != nil {
		return unverified("the header does not decode", err)
	}
	if h.Seq != e.w.Seq || h.Size() != e.w.Loc.Len {
		return unverified("the header names another record", nil)
	}
	h.PayloadCRC = e.w.PayloadCRC
	body, err := format.VerifyRecordBody(buf[format.RecordHeaderSize:format.RecordHeaderSize+h.BodySize()], id, h)
	if err != nil {
		return unverified("the body does not match the kept checksum", err)
	}
	return h, body, nil
}

// heldLocked reports whether the ledger, a pending window or the failed
// records hold the record at loc. The caller holds mu.
func (st *stream) heldLocked(loc segment.Location) bool {
	if _, lost := st.lost[loc]; lost || st.inWindowLocked(loc) {
		return true
	}
	return slices.ContainsFunc(st.ledger, func(e entry) bool { return e.w.Loc == loc })
}
