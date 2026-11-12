package recovery

import (
	"cmp"
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
// the attach answer and the scans, and returns a nil error.
func classifyTails(att Attached, scans []segmentScan) (tails, error) {
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

	var out tails
	for _, sc := range scans {
		id := sc.ID
		damaged, tornSlot, tail := false, false, false
		for _, r := range sc.Ranges {
			switch {
			case named[id] || sc.Sealed:
				// A segment already sealed holds no torn slot: a crash
				// cannot tear bytes below a seal, so a refused range in
				// it is corruption.
				damaged = true
			case r.Off >= sc.RecordsEnd:
				tail = true // the torn tail, or a rotten seal marker nothing names
			case r.Off < sc.SyncedThrough:
				damaged = true // a sync proved these bytes whole
			default:
				// A slot a crash tore before any proof reached it: it
				// was never acknowledged, so it is never reported.
				tornSlot = true
			}
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

		if len(sc.Dropped) > 0 {
			damaged = true
			for _, d := range sc.Dropped {
				out.Corrupt = append(out.Corrupt, corruptRecord{Segment: id, Drop: d})
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
				f.Offset = sc.RecordsEnd
			}
		case tail:
			f.Kind, f.Offset = fateTornTail, sc.RecordsEnd
		default:
			f.Kind = fateIntact
		}
		out.Fates = append(out.Fates, f)
	}

	slices.SortFunc(out.Fates, func(a, b segmentFate) int { return cmp.Compare(a.ID, b.ID) })
	slices.Sort(out.Damaged)
	slices.SortStableFunc(out.Corrupt, func(a, b corruptRecord) int {
		return cmp.Or(cmp.Compare(a.Segment, b.Segment), cmp.Compare(a.Drop.Entry.RecordOffset, b.Drop.Entry.RecordOffset))
	})
	return out, nil
}
