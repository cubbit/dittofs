package recovery

import (
	"slices"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/format"
)

// spareCandidates returns the segments the spares phase may re-adopt,
// ascending by id: the segments of the Own, Unverified and ZeroHeader classes
// — a spare no stream took, a segment whose header slot does not verify, and
// a recycle a crash interrupted after its rename, whose old header names
// this journal under another id — in which no record header verifies anywhere
// (the scan's VerifiedEnd sits at the first record slot) and no seal marker
// verifies (the scan is not sealed). Nothing durable was ever written into
// such a segment — a take is made durable only with the records after it — so
// re-adopting one loses nothing. A segment a successor names is not among
// them whatever its records hold: it was sealed before its successor was
// made.
//
// The identity is checked before any record-less test, so a segment whose
// header verifies naming another journal is never a candidate: no record of
// another journal verifies under this identity, so the record-less test
// alone would take a foreign segment for a spare, and re-adopting it would
// zero what may be the only copy of another journal's content. A segment with
// no scan cannot be a candidate either: its read failed, and the open refuses
// on the range the reader collected before any phase re-adopts — unreadable
// is never read as empty.
func spareCandidates(att Attached, scans []segmentScan, named map[core.SegmentID]bool) []core.SegmentID {
	scanned := make(map[core.SegmentID]segmentScan, len(scans))
	for _, sc := range scans {
		scanned[sc.ID] = sc
	}

	// The classes that name a segment of this journal; the foreign class is
	// excluded here, before any record-less test.
	ids := make([]core.SegmentID, 0, len(att.Own)+len(att.Unverified)+len(att.ZeroHeader))
	for _, s := range att.Own {
		ids = append(ids, s.ID)
	}
	ids = append(ids, att.Unverified...)
	ids = append(ids, att.ZeroHeader...)
	slices.Sort(ids)

	var out []core.SegmentID
	for _, id := range ids {
		sc, ok := scanned[id]
		switch {
		case !ok, named[id], sc.Sealed, sc.VerifiedEnd != format.SegmentHeaderSize:
			// Unreadable, a sealed predecessor, or a segment holding a
			// record: nothing the spares phase may take.
		default:
			out = append(out, id)
		}
	}
	return out
}
