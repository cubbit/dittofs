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

// segmentFate is one segment's fate. Offset is the start of the torn tail to
// cut for fateTornTail and fateEnded: the end of the last verifying record,
// never below a synced-through a verifying header names. It is 0 for every
// other kind and for an ended segment with no tail to cut, so 0 means nothing
// to cut. The later phase that truncates must never truncate at 0: no record
// sits below the first record slot, where a cut would begin, so 0 is never a
// cut point and truncating there would empty the verified records.
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

// successorSet is every own segment another own segment names as its
// predecessor: a segment a verified header names was sealed before its
// successor was made, one no header names could have been open at a crash.
// An id naming itself names nothing, and a name with no segment on disk is
// ignored. The run builds it once and hands the one set to the rebuild and
// to the classing, so the two never disagree on who is named.
func successorSet(own []SegmentInfo) map[core.SegmentID]bool {
	named := make(map[core.SegmentID]bool, len(own))
	for _, s := range own {
		if p := s.Header.Predecessor; p != 0 && p != s.ID {
			named[p] = true
		}
	}
	return named
}

// unverifiedSet is the id of every segment whose header slot did not verify
// and whose records the rebuild scanned. A zero-header segment is not in it:
// the rebuild scans none.
func unverifiedSet(att Attached) map[core.SegmentID]bool {
	unverified := make(map[core.SegmentID]bool, len(att.Unverified))
	for _, id := range att.Unverified {
		unverified[id] = true
	}
	return unverified
}

// classifyTails decides, for each scanned segment, whether each bad fact is
// a crash's tear or damage, and gives the segment its fate. It reads only
// the scans and the two sets the run built once, named and unverified, plus
// the open's reader for the probes it makes, and fails the open when a range
// names no record it can class: see refuseRange and refuseMissingEnd.
//
// A zero-header segment is never scanned: its header is written before any
// record, so every proof a record can carry needs a header durable first and
// a zeroed one hides no synced record. A named one still refuses as a
// missing end, whatever its length; an unnamed one takes no fate and no
// damage here, the spare phase owning it.
func classifyTails(rd *reader, identity core.JournalID, named, unverified map[core.SegmentID]bool, zeroHeader []core.SegmentID, scans []segmentScan) (tails, error) {
	// The refusal is the first one met, so the visit order is the segment
	// id's, never the listing order the rebuild answered in.
	scans = slices.Clone(scans)
	zero := map[core.SegmentID]bool{}
	for _, id := range zeroHeader {
		if !named[id] {
			continue
		}
		size, err := segSize(rd, id)
		if err != nil {
			return tails{}, err
		}
		zero[id] = true
		scans = append(scans, segmentScan{ID: id, Size: size})
	}
	slices.SortStableFunc(scans, func(a, b segmentScan) int { return cmp.Compare(a.ID, b.ID) })

	var out tails
	for _, sc := range scans {
		id := sc.ID
		damaged, tornSlot, tail := false, false, false
		tailAt := sc.VerifiedEnd // where a torn tail starts: before any torn drop at it
		var footer format.Footer
		footerRead, footerOK := false, false
		var err error
		for _, r := range sc.Ranges {
			switch {
			case named[id] && !sc.Sealed && r.Off >= sc.RecordsEnd:
				// A named predecessor's tail: the missing end below judges it.
			case named[id] || sc.Sealed:
				// A segment already sealed holds no torn slot: a crash
				// cannot tear bytes below a seal, so a refused range in
				// it is corruption. One no successor names was never offered
				// its catalog, so its footer is read here: the entries inside
				// the range are the records it held, each confined as a
				// corrupt drop. A range with no entry in it names no record.
				var confined []format.CatalogEntry
				if !named[id] {
					if !footerRead {
						footer, footerOK, err = readFooter(rd, identity, sc)
						if err != nil {
							return tails{}, err
						}
						footerRead = true
					}
					if footerOK {
						for _, e := range footer.Entries {
							if e.RecordOffset >= r.Off && e.RecordOffset < r.End {
								confined = append(confined, e)
							}
						}
					}
				}
				if len(confined) == 0 {
					return tails{}, refuseRange(id, r)
				}
				for _, e := range confined {
					// The same largest-value rule the catalogue read applies
					// to the entries it feeds: a number this journal never
					// issues refuses the open, never a corrupt drop.
					if nerr := checkNumbers(headerFromEntry(id, e)); nerr != nil {
						return tails{}, nerr
					}
					sc.Dropped = append(sc.Dropped, droppedRecord{
						Entry: e, Header: headerFromEntry(id, e), Synced: true,
						Err: fmt.Errorf("%w: segment %d: a record at %d the footer names has a refused header",
							core.ErrCorrupt, id, e.RecordOffset),
					})
				}
			case r.Off < sc.SyncedThrough:
				// A sync proved these bytes whole, whether or not the record
				// that names it verified whole itself: a clone target whose
				// body failed still counts its synced-through.
				return tails{}, refuseRange(id, r)
			case r.Off >= sc.VerifiedEnd:
				tail = true // the torn tail, or a rotten seal marker nothing names
			default:
				// A slot a crash tore before any proof reached it: it
				// was never acknowledged, so it is never reported.
				tornSlot = true
			}
		}

		// A named predecessor was sealed before its successor was made, so
		// its seal marker must verify. One that does not is a missing end
		// the open refuses, unless the marker alone rotted.
		if named[id] && !sc.Sealed {
			// A zero-header segment holds no verifying trailer: under its
			// header's size none can, so the exemption is always absent.
			exempt := false
			if !zero[id] {
				var err error
				if exempt, err = markerAloneRotted(rd, sc); err != nil {
					return tails{}, err
				}
			}
			if !exempt {
				return tails{}, refuseMissingEnd(sc)
			}
			damaged = true
		}

		// The header slot of an unverified-header segment is damaged
		// where a proof covers it (a record's synced-through past it or a
		// seal marker after it) and torn where none does. A successor
		// naming the segment needs no leg of its own: an unsealed one
		// refused as a missing end above, an exempt one is damaged
		// there, and a sealed one falls to the marker's leg.
		tornHeader := false
		if unverified[id] {
			if sc.SyncedThrough >= format.SegmentHeaderSize || sc.Sealed {
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
				// The tail starts at VerifiedEnd, before the record, never
				// at it: the drop did not verify, so nothing below may reach.
				tail = true
			default:
				tornSlot = true
			}
		}
		// decision: a record whose header verifies and whose payload rots is
		// reported by no open phase, here included: the open never reads a
		// content payload, so the scan indexes the record and the read that
		// serves its extent catches the rot, through the corrupt path that
		// raises the loss event and drops it. The ceiling is a rotted payload
		// that stays unreported until something reads it. Overturned only if
		// the open is ever ruled to verify live payloads at rebuild.
		if damaged {
			out.Damaged = append(out.Damaged, id)
		}

		// The cut never goes below a verifying record: VerifiedEnd is where
		// the last one that verified whole ends, and it is the tail's start
		// wherever one exists.
		//
		// Nor does it go below a synced-through a verifying header names: the
		// bytes under it were proven whole, and the last verifying record may
		// end below it when the records that named it are clone targets whose
		// body failed. No input reaches past the file — a tail's offset is
		// always at or past the synced-through and inside the segment — so
		// this needs no cap.
		tailAt = max(tailAt, sc.SyncedThrough)
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
// file's end. It carries refuseRange's decision. Where no record can have
// verified, a file no longer than its header slot, the whole file is the
// missing end, [0, size): never an empty range at the slot's end. Otherwise
// the start is the last verifying record's end, clamped to the size.
//
// decision: a named predecessor ending without its seal marker refuses the
// open whatever its length, the missing end [0, size) where no verifying
// record survives. The successor proves the segment was sealed, so
// everything past its last surviving record may be gone and nothing on disk
// names what the lost records superseded. The ceiling is a journal that stays
// closed over such a truncation until the reconcile work gives the open a
// verdict for content nothing names, which withdraws this marker.
func refuseMissingEnd(sc segmentScan) error {
	start := min(sc.RecordsEnd, sc.Size)
	if sc.Size <= format.SegmentHeaderSize {
		start = 0
	}
	return fmt.Errorf("%w: segment %d: a named predecessor's missing end at [%d,%d) that names no record",
		core.ErrNotImplemented, sc.ID, start, sc.Size)
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

// readFooter reads the footer of a sealed segment no successor names: the
// footer and true when it verifies, false when it does not. A read the bound
// did not clear refuses the open on the range it collected, never as "no
// footer", exactly as markerAloneRotted's.
func readFooter(rd *reader, identity core.JournalID, sc segmentScan) (format.Footer, bool, error) {
	footer, err := format.ReadFooter(readerAt{rd: rd, name: format.SegmentName(sc.ID)}, sc.Size, identity, sc.ID)
	switch {
	case err == nil:
		return footer, true, nil
	case errors.Is(err, format.ErrNoFooter):
		return format.Footer{}, false, nil
	case isContextErr(err):
		return format.Footer{}, false, err
	}
	if ranges := rd.Unreadable(); len(ranges) > 0 {
		return format.Footer{}, false, &core.UnreadableRangesError{Ranges: ranges}
	}
	return format.Footer{}, false, err
}
