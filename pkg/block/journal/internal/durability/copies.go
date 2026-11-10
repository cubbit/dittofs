package durability

import (
	"errors"
	"fmt"
	"io"
	"io/fs"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/format"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/index"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/segment"
)

// copyEntry is another copy of a record the journal knows of: where it sits
// and the payload checksum the record was written with.
type copyEntry struct {
	seq      core.Seq
	loc      segment.Location
	checksum uint32
}

// copyVerdict is what a read of a copy showed.
type copyVerdict uint8

const (
	// copyVerifies is a copy whose header verifies at its place, names the
	// sequence number and whose body matches the kept checksum.
	copyVerifies copyVerdict = iota + 1
	// copyBad is a copy that was read whole and does not verify.
	copyBad
	// copyGone is a copy whose segment no longer exists.
	copyGone
)

// ownedCopy is a record served from another copy of it, handed to holdCopy.
// AtOpen and Synced are the Corruption's, so a record the worker later has to
// lose is dropped as the read that found it would have dropped it.
type ownedCopy struct {
	File    core.FileID
	Seq     core.Seq
	Record  index.RecordAt
	Copy    segment.Location
	Share   core.ShareTag
	Version core.Version
	Extent  core.Extent
	AtOpen  bool
	Synced  bool
}

// registerOriginal registers c, the original of a re-appended record whose
// new place has completed, as another copy of the record, unless c's segment
// has already left st's set. A later copy of the same number replaces the
// earlier one, and the entry stays until Retired reports its segment retired.
//
// It is called only once the new segment's sync has completed and the index
// is repointed to the new copy, so the record whose re-append registers a
// copy is synced at its new place, and no failed sync can take it back while
// the table names its copy. A re-append whose own sync fails registers
// nothing: the new copy is a record of a failed window of the new segment,
// under the same rules as any other. A hold's re-append registers nothing
// either, so after one the table can still name the earlier copy while the
// record's newest place is unsynced (useCopy names that ceiling).
//
// ponytail: the table knows only the copies registered here, the originals of
// re-appends in failed segments. Copies a repack carried are not registered,
// so a corrupt carried copy is a loss even if its source is still on disk.
// Upgrade to registering the source of a carry once a loss is seen that a
// surviving copy would have spared. Retired scans the whole table, which is
// bounded by the records re-appended out of failed segments still on disk.
//
// A segment that has left the set is refused because a failed segment is
// announced resolved when its window empties, which is before the new place
// syncs, so it can be retired in between, and Retired runs once per segment
// and would never remove the entry. The set is asked
// under copyMu: a segment the set still holds is removed from it before
// Retired is called, so that Retired's drop takes copyMu after this call and
// removes the entry. copyMu is held only across the set's mutex, a leaf
// that takes nothing while held. It holds no other lock of this layer.
//
// decision: the set is asked rather than a record of retired segments kept,
// which every segment's retirement would grow for the life of the journal,
// or the damaged rows' retired flag, which only a damaged segment has.
func (d *Durability) registerOriginal(st *stream, c copyEntry) {
	set := st.s.Set()
	d.copyMu.Lock()
	defer d.copyMu.Unlock()
	if set != nil && segmentGone(set, c.loc.Segment) {
		return
	}
	if d.copies == nil {
		d.copies = map[core.Seq]copyEntry{}
	}
	d.copies[c.seq] = c
}

// RegisterCopy puts a copy of the record numbered seq at loc, written with the
// payload checksum checksum, in the copy table directly, as registerOriginal
// does once a re-append's new place has completed, without asking the set
// whether loc's segment is still there.
//
// decision: only tests call it, the durability tests and the datapath tests
// that stage a copy of a record, and it stays exported because the datapath
// tests are in another package. It skips registerOriginal's check for a
// segment that has left the set, which a test staging a copy never needs.
// Overturned by a shared test kit that can reach the table, which would let
// this method go.
func (d *Durability) RegisterCopy(seq core.Seq, loc segment.Location, checksum uint32) {
	d.copyMu.Lock()
	defer d.copyMu.Unlock()
	if d.copies == nil {
		d.copies = map[core.Seq]copyEntry{}
	}
	d.copies[seq] = copyEntry{seq: seq, loc: loc, checksum: checksum}
}

// copyOf returns the table's copy of the record numbered seq.
func (d *Durability) copyOf(seq core.Seq) (copyEntry, bool) {
	d.copyMu.Lock()
	defer d.copyMu.Unlock()
	e, ok := d.copies[seq]
	return e, ok
}

// dropCopies removes every table entry whose copy sits in seg.
func (d *Durability) dropCopies(seg core.SegmentID) {
	d.copyMu.Lock()
	defer d.copyMu.Unlock()
	for seq, e := range d.copies {
		if e.loc.Segment == seg {
			delete(d.copies, seq)
		}
	}
}

// readCopy reads the copy c whole through st's stream and verifies it: its
// header at its own place, its sequence number, and its body against the
// checksum the table kept from the write, never the one read back with it. It
// runs with no lock of this layer held.
//
// decision: only a read that shows something decides. A copy whose segment
// is gone (ErrSegmentGone from the descriptor cache, or ENOENT from the
// storage) is a copy that does not verify, as is one read whole that fails to
// verify; every other error (EIO, a short read, ErrNoDescriptor, a closed
// stream or cache) is returned and decides nothing, so a transient error or a
// spent descriptor budget never drops a record a copy would have spared. The
// ceiling: a persistent EIO on a copy errors every read of that record until
// the copy's failed segment is retired, which for a read-only journal, whose
// retirement does not run, is until it closes. Overturned by a bounded retry
// of an EIO read that then counts the copy unreadable, or by a ruling that an
// EIO read drops the extent.
func (d *Durability) readCopy(st *stream, c copyEntry) (copyVerdict, error) {
	if c.loc.Len < format.RecordHeaderSize {
		return copyBad, nil
	}
	buf := make([]byte, c.loc.Len)
	read := st.s.ReadAt
	if d.readCopyAt != nil {
		read = d.readCopyAt
	}
	n, err := read(c.loc.Segment, buf, c.loc.Off)
	// A whole read that also reports EOF read every byte asked for.
	if n == len(buf) && errors.Is(err, io.EOF) {
		err = nil
	}
	switch {
	case errors.Is(err, segment.ErrSegmentGone), errors.Is(err, fs.ErrNotExist):
		return copyGone, nil
	case err != nil && !errors.Is(err, io.EOF):
		return 0, fmt.Errorf("durability: reading the copy of seq %d at %+v: %w", c.seq, c.loc, err)
	case n < len(buf):
		return 0, fmt.Errorf("durability: reading the copy of seq %d at %+v: %w: %d of %d bytes", c.seq, c.loc, io.ErrUnexpectedEOF, n, len(buf))
	}

	id := st.s.Identity()
	h, err := format.DecodeRecordHeader(buf[:format.RecordHeaderSize], id, c.loc.Segment, c.loc.Off)
	if err != nil || h.Seq != c.seq || h.Size() != c.loc.Len {
		return copyBad, nil
	}
	h.PayloadCRC = c.checksum
	body := buf[format.RecordHeaderSize : format.RecordHeaderSize+h.BodySize()]
	if _, err := format.VerifyRecordBody(body, id, h); err != nil {
		return copyBad, nil
	}
	return copyVerifies, nil
}

// useCopy serves c's record from cp, a copy that verified. The caller holds
// c.File's append domain. Outside a read-only journal the record is handed to
// holdCopy first, and the index is repointed to the copy only under the copy
// segment's claim, which keeps a retirement from unlinking what the index then
// names; while the claim is owed the read is served from the copy with nothing
// repointed. In a read-only journal, where retirement does not run, it is
// repointed with no hand-off. The repoint's delta is accounted. The corrupt
// record's segment is counted damaged only when the record was proven synced:
// damage to an unsynced copy is a torn write, which is not reported. ok is
// false when the index no longer names the record, an overwrite or a failed
// window having taken it, or when the copy's segment has left the set, and the
// caller then goes on to the drop.
//
// decision: whether the record was synced is read under the stream's mutex
// without taking it out of the ledger or a pending window, as the drop does.
// The record is served, not lost, so its entry stays for the worker to
// complete or fail, and what it superseded is lifted or held again as for any
// other record. The answer can be stale by a sync the worker has not applied,
// which only decides whether the segment is counted damaged. The ceiling: a
// served record left in a window that then fails is re-appended from its
// failed segment, and repointed from the copy, only when that re-read
// verifies; when it does not, or when it is left unsynced as the journal turns
// read-only, it is failed by the worker as any other, and although the index
// names the copy and nothing is dropped, the worker still writes a
// failed-window loss record for it (a read-only journal only tries to),
// publishes the event, raises the generation and marks the file, although the
// copy is served until the hold's re-append moves it, which a read-only
// journal never makes. No copy the window's re-append registers reaches it on
// that re-append alone, as registerOriginal is called only after the record it
// serves for is synced, which no window then holds and the read-only pass
// does not fail; only a copy that outlives that place does, once a hold's
// re-append has moved the record to a place not yet synced. A hold's re-append
// and the window's can each append a copy of the record, two records sharing
// one sequence number and version, which is harmless in content; the loss or
// replay consequence is the sequence-number exclusion of RFC 1 section 3.8,
// as for any dropped record's copy. Overturned if a copy is ever registered
// before that sync, or by re-appending such a record from the copy that
// verified, as reappendHeld does for a held record, when its re-read from the
// window fails.
func (d *Durability) useCopy(st *stream, c Corruption, cp copyEntry) (CorruptOutcome, bool) {
	synced := c.Synced
	if !c.AtOpen {
		synced = st.syncedAt(c.Record)
	}
	oc := ownedCopy{
		File:    c.File,
		Seq:     c.Seq,
		Record:  c.Record,
		Copy:    cp.loc,
		Share:   c.Share,
		Version: c.Version,
		Extent:  c.Extent,
		AtOpen:  c.AtOpen,
		Synced:  c.Synced,
	}
	repoint, added := true, false
	if !d.isReadOnly() {
		if d.onHoldCopy != nil {
			d.onHoldCopy(oc)
		}
		var v holdVerdict
		v, added = d.holdCopy(st, oc)
		switch v {
		case holdGone:
			// The copy's segment left the set: the copy is a loss, and the
			// caller drops the record.
			return CorruptOutcome{}, false
		case holdOwed:
			// The segment's claim is owed, so nothing is repointed into it:
			// the read is served from the copy it verified.
			repoint = false
		}
	}
	if repoint {
		to := index.RecordAt{Segment: cp.loc.Segment, Offset: uint32(cp.loc.Off)}
		moved, hd, err := d.index.RepointRecord(c.File, c.Record, to)
		d.accountDelta(hd)
		if err == nil {
			// Whatever it moved, the index names the corrupt place no more,
			// so the held record no longer keeps its segment blocked.
			d.unpin(st, cp.loc.Segment, c.Seq)
		}
		switch {
		case err != nil:
			// The copy verified, so the read is served from it all the same;
			// the index still names the corrupt record, and the next read of
			// it comes back here.
			d.log.Error("journal could not repoint a corrupt record to its copy", "file", c.File, "seq", c.Seq, "err", err)
		case moved == 0 && d.index.Names(c.File, to):
			// Another read repointed the record to this copy between this
			// read finding it corrupt and taking the domain: the copy is what
			// the index serves, so this read is served it too and is no loss
			// (a copy that verifies never is). Nothing was taken out of the
			// ledger yet, which only the drop does, so nothing is undone; a
			// hold this call added stays, as the copy is in use.
		case moved == 0:
			// A record this call added to the hold set joined it before the
			// repoint, which moved nothing, so its hold is released here, by
			// the hold set's owner: left in the set, it would keep the copy's
			// segment held and unretired for a record nothing serves from it.
			// A record another read had already added is that read's hold,
			// and stays.
			if added {
				if d.onUnholdCopy != nil {
					d.onUnholdCopy(oc)
				}
				d.unholdCopy(st, oc)
			}
			return CorruptOutcome{}, false
		}
	}
	if synced {
		d.markDamaged(c.Record.Segment)
	}
	return CorruptOutcome{Kind: CorruptUseCopy, Copy: cp.loc}, true
}

// syncedAt reports whether the record at r was synced: one in the ledger
// that a sync covered, or one in none of the ledger, a pending window, the
// lost set and the moves, as takeCorrupt decides it. A record re-appended out
// of its window is the old place of a copy and was never synced there. It
// changes nothing.
//
// ponytail: a linear scan of the ledger, the pending windows, the lost set
// and the moves, as takeCorrupt's, and upgraded with it.
//
// A place in the moves matters here only when the repoint that took the
// record off it failed, since the index otherwise no longer names it and the
// read never reaches this far.
func (st *stream) syncedAt(r index.RecordAt) bool {
	st.mu.Lock()
	defer st.mu.Unlock()
	// A record re-appended out of its window, whose copy has not completed,
	// was never synced where it was: its old place leads to the copy.
	for loc := range st.moved {
		if loc.Segment == r.Segment && loc.Off == int64(r.Offset) {
			return false
		}
	}
	for i := range st.ledger {
		if recordAt(st.ledger[i].w) == r {
			return st.covered(st.ledger[i].w.Loc)
		}
	}
	for _, w := range st.windows {
		for i := range w.entries {
			if recordAt(w.entries[i].w) == r {
				return false
			}
		}
	}
	for loc := range st.lost {
		if loc.Segment == r.Segment && loc.Off == int64(r.Offset) {
			return false
		}
	}
	return true
}
