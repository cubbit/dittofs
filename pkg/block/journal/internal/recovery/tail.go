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
	fateSealed                      // sealed, the seal proven: a successor names it, or a marker no successor names was proven on the device at this open
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

	// TornTails counts the torn tails the cut-and-seal phase truncated, one
	// per torn-tail fate whose cut it made durable. It is the count the
	// journal's torn_tails counter reads: set by the seal phase alone.
	TornTails int
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

// unverifiedSet is the id of every segment whose header slot did not verify,
// the zero-header ones included.
func unverifiedSet(att Attached) map[core.SegmentID]bool {
	unverified := make(map[core.SegmentID]bool, len(att.Unverified)+len(att.ZeroHeader))
	for _, id := range slices.Concat(att.Unverified, att.ZeroHeader) {
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
// An unnamed unverified-header segment in which no record header verifies
// anywhere and no seal marker does is a spare: it takes no fate or damage
// here, the spare phase owning it.
func classifyTails(rd *reader, identity core.JournalID, named, unverified map[core.SegmentID]bool, scans []segmentScan) (tails, error) {
	// The refusal is the first one met, so the visit order is the segment
	// id's, never the listing order the rebuild answered in.
	scans = slices.Clone(scans)
	slices.SortStableFunc(scans, func(a, b segmentScan) int { return cmp.Compare(a.ID, b.ID) })

	var out tails
	for _, sc := range scans {
		id := sc.ID
		// decision: this skip ignores sc.Ranges, so a segment whose records all
		// rotted, its bytes not zero, goes silent too: no fate and no damage.
		// It holds because no verifying header anywhere means nothing on the
		// segment can be proven synced or reported, and the spare phase owns
		// its bytes. Overturned by a spare re-adoption rule that needs more
		// than the record-less test.
		// A spare: no record header verifies anywhere in the segment, no seal
		// marker does, and no successor names it, so nothing in it was ever
		// durable. Re-adopting it is the spares phase's, a step the run lists but does
		// not yet build, so until then nothing changes it; it has no fate here.
		if unverified[id] && !named[id] && !sc.Sealed && sc.VerifiedEnd == format.SegmentHeaderSize &&
			len(sc.Aside) == 0 && len(sc.Dropped) == 0 {
			continue
		}
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
				// it is corruption. One no successor names, or whose header
				// slot did not verify, was never offered its catalog, so its
				// footer is read here: the entries inside
				// the range are the records it held, each confined as a
				// corrupt drop. A range with no entry in it names no record.
				var confined []format.CatalogEntry
				if !named[id] || unverified[id] {
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
					// A reconcile kind fails the open as unbuilt on every other path
					// too: this journal cannot yet feed one, so a footer
					// entry naming one is never confined as a corrupt drop.
					if e.Kind > format.KindForget {
						return tails{}, fmt.Errorf("%w: a %s record in segment %d", core.ErrNotImplemented, e.Kind, id)
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
		// decision: only the newest segment of each stream has its
		// sync-proven payloads read at open (the kinds pass's fold,
		// confineRotten), so a record those reads find rotten reaches this
		// loop as a drop the fold proved synced and the corrupt arm takes
		// below; a record ending past the synced-through belongs to the
		// tail proof instead. Every other segment's payload is read by no
		// open phase, so its rot is caught by the read that serves the
		// record's extent, through the corrupt path that raises the loss
		// event and drops it, and stays unreported until something reads
		// it.
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

	sortTails(&out)
	return out, nil
}

// unsealByProof re-answers one segment's verdicts the moment the marker
// proof judges its seal marker absent: the segment never sealed, so what
// the sealed classing raised on the seal's word alone comes back off it —
// the corrupt entries whose drops an unsealed classing answers a torn slot
// or a torn tail instead (a drop the catalogue confined, or one a sync
// covers, stays corrupt: those never were the seal's doing), and the
// damaged verdict that hung on the corrupt ones with them. The fate is the
// one an unsealed classing draws: the segment ended where a torn slot or a
// torn header answers, the tail otherwise, its cut at the end of the last
// record that verified — never past it at the sealed area's end, where the
// absent marker left nothing but the seal's own claim.
//
// The drops that leave Corrupt answer their place exactly as the classing's
// own switch answers them, judged on the placement each corrupt entry
// names, over the scan's own copy of the drop — the one with the Synced the
// decode left on it, for the classing's copy in the corrupt entry sets
// every one. A torn slot below the last verifying record ends the segment
// as the loss record's own body failing to read would have had the
// journal never sealed; the tail the unsealed one draws always exists, for
// the seal slot itself is one once its marker goes.
func unsealByProof(tl *tails, fate *segmentFate, sc *segmentScan, unverified map[core.SegmentID]bool) {
	// The corrupt entry set: keep what an unsealed classing corrupts — a
	// drop the catalogue confined names no scanned header, and stays; one
	// a scan's own Synced or a synced-through covering it names stays; the
	// rest answer their place and leave.
	syncedAt := make(map[int64]bool, len(sc.Dropped))
	for _, d := range sc.Dropped {
		if d.Synced || d.Header.Offset < sc.SyncedThrough {
			syncedAt[d.Header.Offset] = true
		}
	}
	damaged := unverified[sc.ID] && sc.SyncedThrough >= format.SegmentHeaderSize
	tornSlot := false
	out := tl.Corrupt[:0]
	for _, c := range tl.Corrupt {
		if c.Segment != sc.ID {
			out = append(out, c)
			continue
		}
		if c.Drop.Entry.RecordOffset != 0 || syncedAt[c.Drop.Header.Offset] {
			damaged = true
			out = append(out, c)
			continue
		}
		if c.Drop.Header.Offset < sc.VerifiedEnd {
			tornSlot = true
		}
	}
	tl.Corrupt = out
	if damaged {
		if !slices.Contains(tl.Damaged, sc.ID) {
			tl.Damaged = append(tl.Damaged, sc.ID)
			slices.Sort(tl.Damaged)
		}
	} else {
		tl.Damaged = slices.DeleteFunc(tl.Damaged, func(id core.SegmentID) bool { return id == sc.ID })
	}
	// The unsealed fate: a torn header, or a torn slot below the last
	// verifying record, ends the segment as the classing's own switch
	// would; the seal slot alone makes the tail otherwise, drawn at the
	// same floor the classing draws one at, clamped to the file's size as
	// the flow's own cut rule clamps.
	tailAt := min(max(sc.VerifiedEnd, sc.SyncedThrough), sc.Size)
	if tornSlot || (unverified[sc.ID] && sc.SyncedThrough < format.SegmentHeaderSize) {
		fate.Kind = fateEnded
	} else {
		fate.Kind = fateTornTail
	}
	fate.Offset = tailAt
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
		core.ErrOpenRefused, id, r.Off, r.End)
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
	// A segment no record slot survived in — a file cut below its first
	// record slot, or a whole long one holding no record that verifies —
	// names no surviving record, so the missing end is everything on disk.
	// Both show it the same way: no record means the scan's record end was
	// never advanced past the first record slot.
	if sc.Size <= format.SegmentHeaderSize || sc.RecordsEnd <= format.SegmentHeaderSize {
		start = 0
	}
	return fmt.Errorf("%w: segment %d: a named predecessor's missing end at [%d,%d) that names no record",
		core.ErrOpenRefused, sc.ID, start, sc.Size)
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
