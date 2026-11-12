package recovery

import (
	"errors"
	"fmt"
	"io"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/format"
)

// droppedRecord is one catalogue entry the tail's proof will judge: the
// entry the footer named, the header this rebuild read for it — the record's
// decoded header where one verified, zero where none did — the error its
// read failed with, and whether the record it names was synced. The
// catalogue's own read fills it for every record it confines, and the
// tail's classing owns what it carries.
type droppedRecord struct {
	Entry  format.CatalogEntry
	Header format.RecordHeader
	Err    error
	Synced bool
}

// headerFromEntry is the header a catalogue entry claims for its record:
// every column the entry carries, the header's own offset the entry says the
// record sits at, and a SyncedThrough of zero — an entry has no
// synced-through column, so a footer read may claim nothing of one. It is the
// header the kinds whose record is not read back at open feed through.
func headerFromEntry(segID core.SegmentID, e format.CatalogEntry) format.RecordHeader {
	return format.RecordHeader{
		Kind:    e.Kind,
		Segment: segID,
		File:    e.File,
		Extent:  e.Extent,
		Seq:     e.Seq,
		Version: e.Version,
		Tag:     e.Tag,
		Offset:  e.RecordOffset,
	}
}

// catalogSegment builds one own segment's scan from the catalogue its
// trailer names: format.ReadFooter verifies the trailer, the seal marker
// just below the footer's entries, the entry count and every entry's checksum
// before any record is fed, and each entry feeds through the same conversion
// the header walk feeds. The content kinds an entry stands for feed from the
// header the entry claims, their records never read back — that is the
// point of the catalogue. The header-only kinds whose bodies the index or
// the later phases need — a clone target's 56-byte spec, a loss's flags, a
// hold's, unhold's or stamp's cut — are read at the entry's own offset and
// verified whole, and the header each lands on is checked against the entry
// that named it: the segments win, the entry being only a summary a writer
// derived from the record.
//
// A record that does not verify — a header that decodes nowhere the entry
// names, a claim the record contradicts, a body its checksum refuses —
// confines to itself: it drops into the scan's Dropped, carried for the
// tail's classing, and every neighbour the footer names still attaches.
// The refusals that stay the whole open's are the scan's own: a read the
// bound did not clear, a counter or sequence number this journal never
// issues, a format this binary does not read, and the reconcile kinds
// this journal cannot yet feed.
//
// A trailer that yields no verifying footer answers (zero scan over the
// segment's measured size, false, nil): the caller scans, and the two
// sources must give one answer. Any other error returns as itself, as the
// scan's own do — a read the bound did not clear is the open's refusal, and
// nothing here wraps it in a verdict.
//
// The scan it returns carries what the later phases read: Sealed, for a
// verifying footer implies its marker; SealedByCatalog, the source they must
// know; Size, the segment's own; and End at the marker, where the walk would
// have stopped. SyncedThrough stays zero: an entry carries no synced-through
// column, and every record the footer names sits below the marker that
// seals it — the tail's own classing owns whatever the seal hides.
//
// ponytail: a segment sealed without a footer — sealed at a crash — is
// scanned at every open until it is retired; upgrade to writing the footer
// once recovery shows those scans.
func catalogSegment(rd *reader, identity core.JournalID, info SegmentInfo) (segmentScan, bool, error) {
	// The size goes through the bound the same way the scan's does: an open
	// or a Size no bounded attempt completes is the open's own refusal, and
	// the unreadable range segSize collected answers the open, never a
	// catalogue-size verdict of bytes nobody measured.
	size, err := segSize(rd, info.ID)
	if err != nil {
		return segmentScan{ID: info.ID}, false, err
	}
	footer, ferr := format.ReadFooter(readerAt{rd: rd, name: format.SegmentName(info.ID)}, size, identity, info.ID)
	if ferr != nil {
		if errors.Is(ferr, format.ErrNoFooter) {
			return segmentScan{ID: info.ID, Size: size}, false, nil
		}
		return segmentScan{ID: info.ID, Size: size}, false, ferr
	}
	sc := segmentScan{
		ID:              info.ID,
		Sealed:          true,
		SealedByCatalog: true,
		End:             footer.Offset - format.SealMarkerSize,
		Size:            size,
	}
	for _, e := range footer.Entries {
		// The entry's numbers meet the largest-value rule first, exactly as
		// a decoded header's numbers would: whatever its kind does with
		// them, a counter or a sequence number the journal never issues
		// refuses the open here — never confined, never demoted, whatever
		// the record it names would have shown.
		if nerr := checkNumbers(headerFromEntry(info.ID, e)); nerr != nil {
			return sc, false, nerr
		}
		// confine answers one record that did not verify: the corruption
		// its own bytes showed — a header that decodes nowhere the entry
		// names, a claim the record contradicts, a body its checksum
		// refuses — drops that record and carries it for the tail's
		// classing while the read goes on, and returns true. Anything
		// else is the open's own refusal and returns false.
		confine := func(h format.RecordHeader, err error) bool {
			if !errors.Is(err, core.ErrCorrupt) {
				return false
			}
			sc.drop(e, h, err)
			return true
		}
		switch {
		case e.Kind == format.KindCloneTarget:
			// A clone target is the one content kind an entry cannot stand
			// for: its 56-byte spec is the record's own body, and the
			// spec's checksum must be verified whole before anything feeds
			// it. The header is read and decoded at the entry's offset and
			// held to the entry's word; a spec that does not verify
			// confines to this record alone, never the open and never the
			// segment.
			h, herr := recordHeaderFor(rd, identity, info.ID, e)
			if herr != nil {
				if confine(h, herr) {
					continue
				}
				return sc, false, herr
			}
			rec, rerr := contentRecord(rd, identity, h)
			if rerr != nil {
				if confine(h, rerr) {
					continue
				}
				return sc, false, rerr
			}
			sc.Records = append(sc.Records, rec)
		case e.Kind <= format.KindUnmark: // the remaining content kinds, 1-6, 8 and 9
			rec, rerr := contentRecord(rd, identity, headerFromEntry(info.ID, e))
			if rerr != nil {
				return sc, false, rerr
			}
			sc.Records = append(sc.Records, rec)
		case e.Kind <= format.KindStamp: // the aside kinds whose bodies carry facts, 10-13
			// A loss's flags and a hold's, unhold's or stamp's cut live only
			// in the record's body, so the record is read at the entry's
			// offset and verified whole, and the verified header is what
			// the aside carries — never the entry's claim of it.
			h, herr := recordHeaderFor(rd, identity, info.ID, e)
			if herr != nil {
				if confine(h, herr) {
					continue
				}
				return sc, false, herr
			}
			body := make([]byte, h.BodySize())
			n, rerr := rd.ReadAt(format.SegmentName(info.ID), body, e.RecordOffset+format.RecordHeaderSize)
			if rerr != nil && !errors.Is(rerr, io.EOF) {
				// A read the bound did not clear is the open's refusal,
				// answered as itself: the open refuses on the collected
				// range, never on a verdict over bytes no read returned.
				return sc, false, rerr
			}
			// A short read reaches VerifyRecordBody with the bytes it
			// returned, and the length it reports names the damage. None
			// of these kinds carries a version in its body — only a clone
			// target's AsOf can, and that body the arm above reads — so
			// every refusal here is the record's own damage.
			if _, verr := format.VerifyRecordBody(body[:n], identity, h); verr != nil {
				sc.drop(e, h, fmt.Errorf("%w: segment %d: the %s at %d: %v",
					core.ErrCorrupt, info.ID, e.Kind, e.RecordOffset, verr))
				continue
			}
			sc.Aside = append(sc.Aside, asideRecord{Header: h})
		case e.Kind == format.KindForget:
			// decision: a forget record is carried as the header its entry
			// claims, its record not read back at open. A forget names only
			// its share tag, and its body is empty, so the read-back the
			// kinds above take would buy only the header's own checksum
			// over fields the entry already mirrored and the trailer's
			// checksum covered. The ceiling is what the footer's word
			// buys: a forget record whose bytes disagree with its entry is
			// caught by no read of this open — widened when a later phase
			// reads the forget record back for its own rules.
			sc.Aside = append(sc.Aside, asideRecord{Header: headerFromEntry(info.ID, e)})
		default: // the reconcile kinds, 15 and 16
			return sc, false, fmt.Errorf("%w: a %s record in segment %d", core.ErrNotImplemented, e.Kind, info.ID)
		}
	}
	return sc, true, nil
}

// readRecordHeaderAt reads and decodes one record header at the offset a
// catalogue entry names, whole through the open's bound: an entry carries
// no checksum of the record it names, so the header is the record's own
// word wherever the two disagree. A read error the bound did not clear
// returns as itself; a header that does not verify, was written somewhere
// else, or names a kind of another format refuses as the scan's own
// refusals do, and a version of a later epoch as a format this binary does
// not read.
func readRecordHeaderAt(rd *reader, identity core.JournalID, segID core.SegmentID, off int64) (format.RecordHeader, error) {
	b := make([]byte, format.RecordHeaderSize)
	n, rerr := rd.ReadAt(format.SegmentName(segID), b, off)
	if rerr != nil && !errors.Is(rerr, io.EOF) {
		// A read the bound did not clear is the caller's error, answered as
		// itself: the open refuses on the collected range, or with the
		// context's own error for a cancellation.
		return format.RecordHeader{}, rerr
	}
	if int64(n) < format.RecordHeaderSize {
		return format.RecordHeader{}, fmt.Errorf("%w: segment %d: the record at %d holds %d of its %d header bytes",
			core.ErrCorrupt, segID, off, n, format.RecordHeaderSize)
	}
	h, derr := format.DecodeRecordHeader(b, identity, segID, off)
	switch {
	case derr == nil:
		return h, nil
	case errors.Is(derr, format.ErrNonZeroEpoch), errors.Is(derr, format.ErrUnknownKind):
		// A version of a later epoch and a kind this format version does
		// not define are formats this binary does not read, as a header
		// carrying one is, not damage — the same triage the scan gives.
		return format.RecordHeader{}, fmt.Errorf("%w: segment %d: the record at %d: %v",
			core.ErrUnknownFormat, segID, off, derr)
	default:
		return format.RecordHeader{}, fmt.Errorf("%w: segment %d: the record at %d: %v",
			core.ErrCorrupt, segID, off, derr)
	}
}

// recordHeaderFor is the fact-check over the header of the record one
// header-only kind's entry names: the header is read at the entry's own
// offset and decoded, then held to the entry's word and to the
// largest-value rule, returning the record's verified header. An error
// wrapping core.ErrCorrupt — the header does not verify where the entry
// put it, or it claims what the entry does not — is the record's own
// corruption, and the caller drops that record and reads on, confined to
// it. Any other error refuses the open exactly as the scan's own reads do:
// a read the bound did not clear, a version of a later epoch. A number this
// journal never issues never reaches the check below as a refusal: the
// largest-value rule has already run over the entry at the loop top, and
// entryMatchesHeader pins the record's numbers to the entry's, so the
// checkNumbers call stands only as the backstop for a day that pinning ends.
func recordHeaderFor(rd *reader, identity core.JournalID, segID core.SegmentID, e format.CatalogEntry) (format.RecordHeader, error) {
	h, herr := readRecordHeaderAt(rd, identity, segID, e.RecordOffset)
	if herr != nil {
		return h, herr
	}
	if merr := entryMatchesHeader(e, h); merr != nil {
		return h, merr
	}
	if nerr := checkNumbers(h); nerr != nil {
		return h, nerr
	}
	return h, nil
}

// entryMatchesHeader holds one catalogue entry to the header of the record
// it names: the entry is a summary a writer derived from the record, so
// wherever the two disagree the record wins and the disagreement is
// corruption. The first disagreeing column names both sides; the header's
// own offset is never compared, for the header was decoded at the offset
// the entry names, and its checksums cover its own bytes alone.
func entryMatchesHeader(e format.CatalogEntry, h format.RecordHeader) error {
	if e.File != h.File {
		return fmt.Errorf("%w: segment %d: the record at %d names the FileID % x, the entry the FileID % x",
			core.ErrCorrupt, h.Segment, h.Offset, h.File.Bytes(), e.File.Bytes())
	}
	if e.Extent != h.Extent {
		return fmt.Errorf("%w: segment %d: the record at %d claims the extent %+v, the entry %+v",
			core.ErrCorrupt, h.Segment, h.Offset, h.Extent, e.Extent)
	}
	if e.Seq != h.Seq {
		return fmt.Errorf("%w: segment %d: the record at %d carries seq %d, the entry %d",
			core.ErrCorrupt, h.Segment, h.Offset, h.Seq, e.Seq)
	}
	if e.Version != h.Version {
		return fmt.Errorf("%w: segment %d: the record at %d carries version %+v, the entry %+v",
			core.ErrCorrupt, h.Segment, h.Offset, h.Version, e.Version)
	}
	if e.Kind != h.Kind {
		return fmt.Errorf("%w: segment %d: the record at %d is a %s record, the entry a %s",
			core.ErrCorrupt, h.Segment, h.Offset, h.Kind, e.Kind)
	}
	if e.Tag != h.Tag {
		return fmt.Errorf("%w: segment %d: the record at %d names the share tag %d, the entry %d",
			core.ErrCorrupt, h.Segment, h.Offset, h.Tag, e.Tag)
	}
	return nil
}

// drop confines one catalogue record this read could not feed: the entry
// that named it, the header its record verified — zero where none did —
// and the error it failed with, carried for the tail's classing while the
// read goes on. Every neighbour the footer names still attaches, and
// nothing here demotes the segment. Synced is true because the record sat
// below a seal marker that verifies — a seal is made durable only over
// records that were, and it is what makes the footer naming them readable
// at all.
func (sc *segmentScan) drop(e format.CatalogEntry, h format.RecordHeader, err error) {
	sc.Dropped = append(sc.Dropped, droppedRecord{Entry: e, Header: h, Err: err, Synced: true})
}
