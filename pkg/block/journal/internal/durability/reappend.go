package durability

import (
	"errors"
	"fmt"
	"io"
	"slices"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/format"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/index"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/segment"
)

// errUnverified is a content record that cannot be re-appended because the
// bytes of its failed segment, read again, do not verify against the checksum
// the ledger kept: it is failed, not retried.
var errUnverified = errors.New("durability: the record does not verify on re-read")

// resolveWindows resolves every pending window of the stream, oldest first:
// each record is re-appended to the stream's current segment, and a content
// record repointed there, unless it is a content record whose re-read does not
// verify, which is failed. A window that then holds nothing more is resolved.
// It runs on the worker, after the windows are taken.
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
// written and no event is published. A content record is then repointed in
// the index and its original registered as a copy (repointReappended). The
// old place then leads to the new one, so a waiter on it follows the record,
// and the record leaves the window. A failed append changes nothing. A content
// record that does not verify returns errUnverified.
//
// st.mu is not held across the re-read, the append or a call into the index,
// and no file's append domain is held across the re-read or the append, which
// may roll, seal and sync.
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
	// A record Corrupt took out of the window while it was read again is
	// Corrupt's to drop and report: no copy of it is appended.
	st.mu.Lock()
	in := st.inWindowLocked(e.w.Loc)
	st.mu.Unlock()
	if !in {
		return nil
	}
	p, err := d.appendAgain(st, h, body)
	if err != nil {
		return err
	}
	if isContent(e.w) {
		d.repointReappended(st, e, p.Loc)
		return nil
	}
	// A header-only record names no index entry and no read reports it
	// corrupt, so nothing but the worker takes it out of the window.
	st.mu.Lock()
	st.leaveWindowLocked(w, e, p.Loc)
	st.mu.Unlock()
	return nil
}

// appendAgain appends the verified record h, body to the stream's current
// segment under the sequence number, version, file, extent and tag it already
// has: the same operation at a new place. The stream fills in the segment, the
// offset and the synced-through. It holds no lock of this layer.
func (d *Durability) appendAgain(st *stream, h format.RecordHeader, body format.RecordBody) (segment.Placed, error) {
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
		return segment.Placed{}, err
	}
	if d.afterAppend != nil {
		d.afterAppend()
	}
	return p, nil
}

// reappendHeld appends again the record the held copy c names, from that copy
// to the stream's current segment, and repoints the index from the copy to the
// new place. It is the re-append the worker calls for a held record, with the
// copy's segment claimed and the index already naming the copy. The copy is
// read at c.Copy and verified against the checksum the copy table kept for the
// record's sequence number, never the one read back with it, so bytes that
// agree only with themselves are refused; the record then keeps its sequence
// number and version, as for a failed window's record. No file domain is held
// across the read or the append, and nothing waits on a sync: the new record
// is tracked and synced by the worker like any other, and a failure of that
// sync starts a window of its own.
//
// decision: a copy that no longer verifies, or one the table no longer
// knows, is an error, and the record stays held for the worker's next pass: it
// verified when the read was served from it and its segment has been claimed
// since, so a failure now is a transient read or a table entry a retirement
// removed, and a loss decided here would drop a record the claim still backs.
// The ceiling is a copy that went bad after the read served it, which is then
// retried on every pass with no backoff and keeps its segment claimed.
// Overturned by a ruling that a held copy which fails verification is a loss.
//
// The record's file domain is taken only for the repoint, and only if the
// index still names the copy: a write over the record since it was held has
// the extent itself, the repoint then moves only what is still the record's,
// and the hold ends all the same.
func (d *Durability) reappendHeld(st *stream, c ownedCopy) error {
	cp, ok := d.copyOf(c.Seq)
	if !ok {
		return fmt.Errorf("durability: no kept checksum for the held copy of seq %d at %+v", c.Seq, c.Copy)
	}
	e := entry{w: segment.Written{
		Loc:        c.Copy,
		Seq:        c.Seq,
		Version:    c.Version,
		File:       c.File,
		Kind:       format.KindWrite,
		Extent:     c.Extent,
		Tag:        c.Share,
		PayloadCRC: cp.checksum,
	}}
	h, body, err := d.rereadRecord(st, e)
	if err != nil {
		return err
	}
	p, err := d.appendAgain(st, h, body)
	if err != nil {
		return err
	}

	from := index.RecordAt{Segment: c.Copy.Segment, Offset: uint32(c.Copy.Off)}
	to := index.RecordAt{Segment: p.Loc.Segment, Offset: uint32(p.Loc.Off)}
	unlock := d.domains.Lock(c.File)
	if d.index.Names(c.File, from) {
		_, hd, err := d.index.RepointRecord(c.File, from, to)
		d.accountDelta(hd)
		if err != nil {
			unlock()
			return fmt.Errorf("durability: repointing the held record of seq %d: %w", c.Seq, err)
		}
	}
	unlock()
	st.mu.Lock()
	st.want = true
	st.mu.Unlock()
	st.wake()
	return nil
}

// repointReappended finishes the re-append of the content record e, whose
// copy the stream placed at to. Under the record's file append domain, and
// only while e is still in its window, it registers the original as a copy of
// the record, repoints the index from the original to the copy, and takes e
// out of the window with the old place leading to the new one. It takes no
// other domain and holds st.mu only to read and change the window, never
// across the index or the copy table.
//
// The window check and the repoint share the domain because Corrupt takes a
// record out of a window under the same domain: a Corrupt is then wholly
// before the repoint, and the record is no longer in the window, or wholly
// after it, and finds the index naming the copy.
//
// decision: a record Corrupt took out of the window after the worker's check
// before the append, and before this step took the domain, has had its copy
// appended all the same: no domain is held across an append, so the check
// and the append cannot be made one step. The copy is left as a record no
// index entry names, taken out of the ledger and into the lost set, so no
// wait and no later window of its segment ever handles it, and repack finds
// nothing live in it. It stays on disk under the record's sequence number:
// at recovery the loss record Corrupt wrote for that number excludes every
// record carrying it, the copy included, whatever their order (RFC 1 §3.8),
// and a crash before that loss record is durable finds the record whole, as
// it finds any dropped record of a failed window whose loss record is not
// durable. When the index no longer named the record, so Corrupt wrote no
// loss record, the overwrite that took it outranks the copy by version, or the
// earlier drop that took it wrote the loss record that excludes it.
// Overturned if recovery ever stops excluding a dropped record by its
// sequence number, or if a Corrupt may wait on a re-append, which would let
// it serve the copy instead.
func (d *Durability) repointReappended(st *stream, e entry, to segment.Location) {
	f := e.w.File
	// A read that found the record corrupt may have repointed the index to
	// another copy of it; that copy is read before the original replaces it
	// in the table.
	cp, haveCopy := d.copyOf(e.w.Seq)
	unlock := d.domains.Lock(f)
	defer unlock()

	st.mu.Lock()
	w := st.windowOfLocked(e.w.Loc)
	if w == nil {
		st.untrackLocked(to)
		st.mu.Unlock()
		return
	}
	st.mu.Unlock()

	d.RegisterCopy(e.w.Seq, e.w.Loc, e.w.PayloadCRC)
	at := index.RecordAt{Segment: to.Segment, Offset: uint32(to.Off)}
	froms := []index.RecordAt{recordAt(e.w)}
	if haveCopy {
		froms = append(froms, index.RecordAt{Segment: cp.loc.Segment, Offset: uint32(cp.loc.Off)})
	}
	for _, from := range froms {
		// decision: only the pieces the index still has at the record's
		// places are moved, held or retained. A write that superseded the
		// record since its append holds the extent itself, so nothing held is
		// moved and the overwrite stays what is served; what it superseded is
		// retained until it syncs, and that retained piece is moved to the
		// copy, so the failed segment backs nothing once the window resolves,
		// and a later failure of the overwrite's window holds the record again
		// at the copy, never at a segment repack may have removed. A record
		// the index no longer names at all was dropped or fully overwritten
		// and synced, and has nothing to move. Overturned if the index ever
		// keeps a piece of a record anywhere but held or retained.
		if !d.index.Names(f, from) {
			continue
		}
		_, hd, err := d.index.RepointRecord(f, from, at)
		d.accountDelta(hd)
		if err != nil {
			d.log.Error("journal could not repoint a re-appended record", "stream", st.no, "file", f, "seq", e.w.Seq, "err", err)
		}
	}

	st.mu.Lock()
	st.leaveWindowLocked(w, e, to)
	st.mu.Unlock()
}

// leaveWindowLocked takes the re-appended record e out of the window w, makes
// its old place lead to to while the copy is still held, and asks for the
// sync that makes the copy durable. The caller holds mu.
func (st *stream) leaveWindowLocked(w *window, e entry, to segment.Location) {
	i := slices.IndexFunc(w.entries, func(o entry) bool { return o.w.Loc == e.w.Loc })
	if i < 0 {
		return
	}
	w.entries = slices.Delete(w.entries, i, i+1)
	// The new record may already be synced and completed, which clears its
	// entry: then nothing is left to follow, and the old place, in no window
	// or ledger, reads as done.
	if st.heldLocked(to) {
		st.moved[e.w.Loc] = to
	}
	st.want = true
	st.d.notifyAll()
}

// windowOfLocked returns the pending window holding the record at loc, or nil.
// The caller holds mu.
func (st *stream) windowOfLocked(loc segment.Location) *window {
	for _, w := range st.windows {
		if slices.ContainsFunc(w.entries, func(o entry) bool { return o.w.Loc == loc }) {
			return w
		}
	}
	return nil
}

// untrackLocked takes the record at loc out of the ledger into the lost set,
// so it never reads durable and no wait or window handles it. The caller
// holds mu.
func (st *stream) untrackLocked(loc segment.Location) {
	st.removeLocked(func(e *entry) bool { return e.w.Loc == loc }, nil)
	st.lost[loc] = lostRecord{err: errRecordCorrupt}
	st.d.notifyAll()
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
	if h.Seq != e.w.Seq || h.Size() != e.w.Loc.Len || h.File != e.w.File || h.Version != e.w.Version || h.Extent != e.w.Extent {
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
