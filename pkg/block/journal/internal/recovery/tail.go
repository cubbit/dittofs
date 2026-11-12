package recovery

import (
	"cmp"
	"errors"
	"fmt"
	"slices"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/format"
)

// fateKind says what the open does with one segment's tail.
type fateKind uint8

const (
	fateIntact      fateKind = iota // could have been open at a crash; whole; the tail's proof owns it
	fateSealed                      // sealed, the seal proven: a successor names it
	fateSealToProve                 // sealed by a marker no successor names; re-proven on the device later
	fateTornTail                    // a torn tail from Offset; truncated there and sealed
	fateEnded                       // ended at Offset; truncated there and then emptied
)

// String names the fate as the rules above spell it.
func (k fateKind) String() string {
	switch k {
	case fateIntact:
		return "intact"
	case fateSealed:
		return "sealed"
	case fateSealToProve:
		return "seal-to-prove"
	case fateTornTail:
		return "torn-tail"
	case fateEnded:
		return "ended"
	}
	return "unknown"
}

// segmentFate is one segment's fate. Offset is the tail's start for
// fateTornTail and fateEnded, the end of the last verifying record, and 0
// for every other kind and for an ended segment with no tail to cut.
type segmentFate struct {
	ID     core.SegmentID
	Kind   fateKind
	Offset int64
}

// corruptRecord is a record dropped as corrupt, with the synced flag
// recovery proved for it.
type corruptRecord struct {
	Segment core.SegmentID
	Drop    droppedRecord
}

// at is the offset of the record the drop names: the catalogue entry's where
// one named it, the scanned header's otherwise.
func (c corruptRecord) at() int64 {
	if c.Drop.Entry.RecordOffset != 0 {
		return c.Drop.Entry.RecordOffset
	}
	return c.Drop.Header.Offset
}

// tails is the classing's answer for every own and unverified-header segment.
type tails struct {
	// Fates holds one fate per segment, ascending by id.
	Fates []segmentFate
	// Damaged holds every segment the open reports damaged, ascending.
	// A torn tail, a torn slot nothing proves, an intact segment and a
	// torn header nothing proves are never in it.
	Damaged []core.SegmentID
	// Corrupt holds the corrupt drops, ascending by segment, then by the
	// offset of the record the drop names.
	Corrupt []corruptRecord
}

// classifyTails decides, for each scanned segment, whether each bad fact is
// a crash's tear or damage, and gives the segment its fate. It reads only
// the attach answer and the scans, plus the open's reader for the one
// probe it makes, and fails the open when a range names no record it can
// class: see refuseRange and refuseMissingEnd.
func classifyTails(rd *reader, att Attached, scans []segmentScan) (tails, error) {
	// A segment a verified header names as predecessor was sealed before
	// its successor was made; a segment no header names could have been
	// open at a crash. An id naming itself names nothing, and a name with
	// no segment on disk is ignored.
	named := map[core.SegmentID]bool{}
	for _, s := range att.Own {
		if p := s.Header.Predecessor; p != 0 && p != s.ID {
			named[p] = true
		}
	}
	unverified := map[core.SegmentID]bool{}
	for _, id := range att.Unverified {
		unverified[id] = true
	}

	// The refusal is the first one met, so the visit order is the segment
	// id's, never the listing order the rebuild answered in.
	scans = slices.Clone(scans)
	slices.SortStableFunc(scans, func(a, b segmentScan) int { return cmp.Compare(a.ID, b.ID) })

	var out tails
	for _, sc := range scans {
		id := sc.ID
		damaged, tornSlot, tail := false, false, false
		tailAt := sc.RecordsEnd // where a torn tail starts: before any torn drop at it
		for _, r := range sc.Ranges {
			switch {
			case named[id] && !sc.Sealed && r.Off >= sc.RecordsEnd:
				// A named predecessor's tail: the missing end below judges it.
			case named[id] || sc.Sealed:
				// A segment already sealed holds no torn slot: a crash
				// cannot tear bytes below a seal, so a refused range in
				// it is corruption.
				return tails{}, refuseRange(id, r)
			case r.Off >= sc.RecordsEnd:
				tail = true // the torn tail, or a rotten seal marker nothing names
			case r.Off < sc.SyncedThrough:
				return tails{}, refuseRange(id, r) // a sync proved these bytes whole
			default:
				// A slot a crash tore before any proof reached it: it
				// was never acknowledged, so it is never reported.
				tornSlot = true
			}
		}

		// A named predecessor was sealed before its successor was made, so
		// its seal marker must verify. One that does not is a missing end
		// the open refuses, unless the marker alone rotted.
		//
		// decision: a named segment shorter than the first record slot opens
		// with its damage reported and is never refused as a missing end: the
		// missing end runs from the last verifying record to the segment's
		// length, and under the first record slot that range inverts, so the
		// refusal would name an empty one. The ceiling is an open that serves
		// content the truncated records superseded, since nothing readable
		// names them now. Overturned when the reconcile work gives the open a
		// verdict for content nothing names, or when the format rules bounds
		// for a truncation below the first record slot.
		if named[id] && !sc.Sealed && !sc.SealedByCatalog && sc.Size >= format.SegmentHeaderSize {
			exempt, err := markerAloneRotted(rd, sc)
			if err != nil {
				return tails{}, err
			}
			if !exempt {
				return tails{}, refuseMissingEnd(sc)
			}
			damaged = true
		}

		// The header slot of an unverified-header segment is damaged
		// where a proof covers it (a record's synced-through past it, a
		// seal marker after it, or a successor naming the segment) and
		// torn where none does.
		tornHeader := false
		if unverified[id] {
			if sc.SyncedThrough >= format.SegmentHeaderSize || sc.Sealed || named[id] {
				damaged = true
			} else {
				tornHeader = true
			}
		}

		// A dropped record the catalogue confined is already synced. A scanned
		// one is synced where a proof covers it, which makes it corrupt; below
		// the last verifying record without one it is a torn slot, and past
		// that record it is the torn tail. Neither is ever written or reported.
		for _, d := range sc.Dropped {
			switch {
			case d.Synced || sc.Sealed || named[id] || d.Header.Offset < sc.SyncedThrough:
				d.Synced = true
				damaged = true
				out.Corrupt = append(out.Corrupt, corruptRecord{Segment: id, Drop: d})
			case d.Header.Offset >= sc.VerifiedEnd:
				tail = true
				tailAt = min(tailAt, d.Header.Offset)
			default:
				tornSlot = true
			}
		}
		if damaged {
			out.Damaged = append(out.Damaged, id)
		}

		// The cut never goes below a verifying record: RecordsEnd is where
		// the last one ends, and it is the tail's start wherever one exists.
		f := segmentFate{ID: id}
		switch {
		case named[id]:
			f.Kind = fateSealed
		case sc.Sealed:
			f.Kind = fateSealToProve
		case tornSlot || tornHeader:
			f.Kind = fateEnded
			if tail {
				f.Offset = tailAt
			}
		case tail:
			f.Kind, f.Offset = fateTornTail, tailAt
		default:
			f.Kind = fateIntact
		}
		out.Fates = append(out.Fates, f)
	}

	slices.SortFunc(out.Fates, func(a, b segmentFate) int { return cmp.Compare(a.ID, b.ID) })
	slices.Sort(out.Damaged)
	slices.SortStableFunc(out.Corrupt, func(a, b corruptRecord) int {
		return cmp.Or(cmp.Compare(a.Segment, b.Segment), cmp.Compare(a.at(), b.at()))
	})
	return out, nil
}

// refuseRange is the open's refusal of a range inside a segment whose
// catalog was not verified that names no record: bytes a crash cannot have
// torn, since a seal, a successor or a sync proves them whole.
//
// decision: this is the whole open's refusal rather than a dropped record,
// because no catalog names what the range held and the reconcile that would
// recover it is not built, so serving past it could serve without knowing
// what was lost. The ceiling is a journal that stays closed for a rotted
// record header in a segment with no verifying catalog, for a truncated
// named predecessor and for a rotted record below a mark's Through, until
// the reconcile work lands, which withdraws this marker.
func refuseRange(id core.SegmentID, r format.ScanRange) error {
	return fmt.Errorf("%w: segment %d: a refused range at [%d,%d) that names no record",
		core.ErrNotImplemented, id, r.Off, r.End)
}

// refuseMissingEnd is the same refusal for a named predecessor that ends
// without its seal marker: the bytes from the last verifying record to the
// file's end. It carries refuseRange's decision.
func refuseMissingEnd(sc segmentScan) error {
	return fmt.Errorf("%w: segment %d: a named predecessor's missing end at [%d,%d) that names no record",
		core.ErrNotImplemented, sc.ID, sc.RecordsEnd, sc.Size)
}

// markerAloneRotted reports whether a named predecessor's missing seal marker
// is the only thing wrong with its end: every record before it verifies and
// its trailer verifies, naming a footer exactly past the marker's own slot.
// A read the bound did not clear refuses the open on the range it collected,
// never as a verdict on a trailer nobody read.
func markerAloneRotted(rd *reader, sc segmentScan) (bool, error) {
	footerOff, err := format.ReadTrailer(readerAt{rd: rd, name: format.SegmentName(sc.ID)}, sc.Size)
	switch {
	case err == nil:
		return footerOff == sc.RecordsEnd+format.SealMarkerSize, nil
	case errors.Is(err, format.ErrNoFooter):
		return false, nil
	case isContextErr(err):
		return false, err
	}
	if ranges := rd.Unreadable(); len(ranges) > 0 {
		return false, &core.UnreadableRangesError{Ranges: ranges}
	}
	return false, err
}
