package recovery

import (
	"errors"
	"fmt"
	"io"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/format"
)

// droppedRecord is one catalogue entry the tail's proof will judge: the
// entry the footer named, the header this rebuild read for it — the entry's
// own claim where the record was not read back, the record's decoded header
// where it was — the error its read failed with, and whether the record it
// names was synced. No phase fills it yet; it is the shape the tail's own
// classing will carry.
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
// the header walk feeds — the content kinds from the header the entry
// claims, a clone target from its own record, read at the entry's offset and
// verified whole, the aside kinds as the headers the entries claim. The
// checksums the entries are covered by are the footer's own: the records
// they summarize are never read back for the kinds an entry stands for, the
// point of the catalogue.
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
		// them, a counter or a sequence number the journal never issues is
		// refused here.
		if nerr := checkNumbers(headerFromEntry(info.ID, e)); nerr != nil {
			return sc, false, nerr
		}
		switch {
		case e.Kind == format.KindCloneTarget:
			// A clone target is the one content kind an entry cannot stand
			// for: its 56-byte spec is the record's own body, and the
			// spec's checksum must be verified whole before anything feeds
			// it. The header is read and decoded at the entry's offset;
			// nothing here checks it against the entry yet.
			h, cerr := cloneHeaderAt(rd, identity, info.ID, e)
			if cerr != nil {
				return sc, false, cerr
			}
			rec, rerr := contentRecord(rd, identity, h)
			if rerr != nil {
				return sc, false, rerr
			}
			sc.Records = append(sc.Records, rec)
		case e.Kind <= format.KindUnmark: // the remaining content kinds, 1-6, 8 and 9
			rec, rerr := contentRecord(rd, identity, headerFromEntry(info.ID, e))
			if rerr != nil {
				return sc, false, rerr
			}
			sc.Records = append(sc.Records, rec)
		case e.Kind <= format.KindForget: // the aside kinds, 10-14
			// decision: the aside kinds are carried as the headers the
			// entries claim, their records not read back at open. A footer
			// checksum covers its entries, and each of these kinds' own
			// rules belong to the phases that read their bodies. The
			// ceiling is what the footer's word buys: an aside record torn
			// past its header is caught only where its body is read, not
			// here — widened when the verified read of these kinds' own
			// bytes replaces this arm.
			sc.Aside = append(sc.Aside, asideRecord{Header: headerFromEntry(info.ID, e)})
		default: // the reconcile kinds, 15 and 16
			return sc, false, fmt.Errorf("%w: segment %d: a %s record", core.ErrNotImplemented, info.ID, e.Kind)
		}
	}
	return sc, true, nil
}

// cloneHeaderAt reads and decodes the record header one catalogue entry
// names at its own offset, whole through the open's bound: the header a
// clone target's spec is verified against, for an entry carries no checksum
// of the record it names. A read error the bound did not clear returns as
// itself; a header that does not verify, was written somewhere else, or
// names a kind of another format refuses as the scan's own refusals do.
func cloneHeaderAt(rd *reader, identity core.JournalID, segID core.SegmentID, e format.CatalogEntry) (format.RecordHeader, error) {
	b := make([]byte, format.RecordHeaderSize)
	n, rerr := rd.ReadAt(format.SegmentName(segID), b, e.RecordOffset)
	if rerr != nil && !errors.Is(rerr, io.EOF) {
		// A read the bound did not clear is the caller's error, answered as
		// itself: the open refuses on the collected range, or with the
		// context's own error for a cancellation.
		return format.RecordHeader{}, rerr
	}
	if int64(n) < format.RecordHeaderSize {
		return format.RecordHeader{}, fmt.Errorf("%w: segment %d: the clone target at %d holds %d of its %d header bytes",
			core.ErrCorrupt, segID, e.RecordOffset, n, format.RecordHeaderSize)
	}
	h, derr := format.DecodeRecordHeader(b, identity, segID, e.RecordOffset)
	switch {
	case derr == nil:
		return h, nil
	case errors.Is(derr, format.ErrNonZeroEpoch):
		// A version of a later epoch is a format this binary does not read,
		// as a header carrying one is, not damage.
		return format.RecordHeader{}, fmt.Errorf("%w: segment %d: the clone target at %d: %v",
			core.ErrUnknownFormat, segID, e.RecordOffset, derr)
	default:
		return format.RecordHeader{}, fmt.Errorf("%w: segment %d: the clone target at %d: %v",
			core.ErrCorrupt, segID, e.RecordOffset, derr)
	}
}
