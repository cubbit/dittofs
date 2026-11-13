package recovery

import (
	"fmt"
	"maps"
	"slices"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/format"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/index"
)

// OpenLoss is one loss record the open found on disk, decoded: its event,
// with AtOpen set and the number left for the sink to assign, and the seq of
// the record it drops.
type OpenLoss struct {
	Ev      core.LossEvent
	Dropped core.Seq

	// Segments holds every segment holding a copy of the dropped record, the
	// liveness the copy that retires them empties by. Filled by the feed.
	Segments []core.SegmentID
}

// OpenForget is one share tag the forget records on disk forgot: the tag and
// the greatest seq its forgets reached, so no record of the tag below it
// feeds.
type OpenForget struct {
	Tag core.ShareTag
	Seq core.Seq

	// Segments holds every segment holding a record of the tag below Seq, the
	// liveness the copy that retires them empties by. Filled by the feed.
	Segments []core.SegmentID
}

// ReadOnlyDrop is one record a read-only journal failed earlier in this
// process — a seq on the loss sink's failed list the feed finds on disk —
// withheld whole and owed the unsynced loss record a later open appends.
type ReadOnlyDrop struct {
	// Ev is built from the found record's own header: its file, extent,
	// version and share tag, with the reason a failed sync window takes.
	Ev      core.LossEvent
	Dropped core.Seq

	// Segments holds every segment still holding the record. Filled by the
	// feed.
	Segments []core.SegmentID
}

// kindResults is the kinds pass's collected answer: the loss records found on
// disk, one per delivered piece; one forget per share tag, the greatest seq
// its records named; and the read-only drops and the withheld numbers the
// feed computes over the same records.
type kindResults struct {
	// Losses holds every verified loss record the pass delivered to the loss
	// sink, in the order the pass read it, an event a sink already published
	// when one was given.
	Losses []OpenLoss

	// Forgets holds one entry per forgotten share tag, ascending by tag, the
	// greatest seq the tag's forget records name.
	Forgets []OpenForget

	// ReadOnlyDrops holds the records the journal failed to write the loss
	// record for that this open finds on disk, one per failed seq, filled by
	// the feed that withholds them.
	ReadOnlyDrops []ReadOnlyDrop

	// Withheld is the highest numbers of the records the feed withholds, so
	// the counters open above numbers the disk still holds; zero until the
	// feed computes it over the same scans.
	Withheld index.Numbers
}

// kindState is the decoded aside records' whole state: the maps the feed
// filter withholds by, built over every fact the pass decoded, and the
// collected outputs the State carries.
type kindState struct {
	kindResults

	// whole holds every seq a loss record drops whole — one whose reason is
	// not superseded, or whose payload's synced bit says unsynced, the two
	// read independently.
	whole map[core.Seq]bool

	// superseded holds, per seq a synced loss record of the superseded
	// reason names, the extents its records feed clipped over.
	superseded map[core.Seq][]core.Extent

	// forget holds the greatest seq each share tag's forget records name.
	forget map[core.ShareTag]core.Seq

	// failed holds the seqs the loss sink's failed list named, the records a
	// read-only journal failed earlier in this process.
	failed map[core.Seq]bool

	// pieces holds the (dropped, extent) pairs already delivered, so two
	// identical loss records — a copy carried forward — report one loss.
	pieces map[lossPiece]bool
}

// lossPiece is one extent of one dropped record, the key a loss record names.
type lossPiece struct {
	seq core.Seq
	ext core.Extent
}

// decodeAsides decides the aside records the rebuild set aside against the
// kinds' own rules, over the scans the rebuild answered in. The pass reads
// nobody's bytes: the header walk read and verified each loss, hold, unhold
// and stamp body already — the catalogue's read of the same kinds is the
// same one, so a record whose body does not verify confines as a corrupt
// drop on its own scan whatever its segment's source, and never reaches
// this pass — and each aside carries the body that read decoded, which the
// pass consumes. loss deliveries reach the sink when one is given, and none
// is required: a nil one means no failed list and no placement.
//
// A loss record whose header names a zero-length extent, or one that names
// no file, refuses the open naming its segment and its offset. A body that
// fails its checksum never reached the pass: a lost loss record errs toward
// fetching the extent again.
func decodeAsides(rd *reader, identity core.JournalID, losses LossSink, scs []segmentScan) (kindState, error) {
	ks := kindState{
		whole:      map[core.Seq]bool{},
		superseded: map[core.Seq][]core.Extent{},
		forget:     map[core.ShareTag]core.Seq{},
		failed:     map[core.Seq]bool{},
		pieces:     map[lossPiece]bool{},
	}
	if losses != nil {
		for _, seq := range losses.Failed() {
			ks.failed[seq] = true
		}
	}

	// decoded keeps each aside the scan carried — its header, the body the
	// walk read and verified, and the scan it sits on — so the second pass
	// decides the records against the complete maps with no read of its own.
	type decoded struct {
		sc *segmentScan
		h  format.RecordHeader
		l  format.Loss
	}
	var (
		dl []decoded // the loss records that decoded
		df []decoded // the forget records, headerborne facts alone
		dh []decoded // the hold, unhold and stamp records
	)
	for i := range scs {
		sc := &scs[i]
		for _, a := range sc.Aside {
			h := a.Header
			switch h.Kind {
			case format.KindLoss:
				if h.Extent.Len == 0 {
					return ks, fmt.Errorf("%w: segment %d: the loss record at %d names a zero-length extent",
						core.ErrZeroLength, h.Segment, h.Offset)
				}
				// A loss record must name the file of the record it drops.
				// The header's own field rules already refuse one naming no
				// file, so this backstop keeps the pass whole should that
				// rule ever lift.
				if h.File == (core.FileID{}) {
					return ks, fmt.Errorf("%w: segment %d: the loss record at %d names no file",
						core.ErrCorrupt, h.Segment, h.Offset)
				}
				// The body reached the scan verified: the walk confined one
				// that did not verify as a corrupt drop, so every record
				// here crosses with a whole claim of its flags.
				dl = append(dl, decoded{sc: sc, h: h, l: a.Body.Loss})
			case format.KindForget:
				df = append(df, decoded{h: h})
			case format.KindHold, format.KindUnhold, format.KindStamp:
				dh = append(dh, decoded{sc: sc, h: h})
			}
		}
	}

	// The cover maps are built from every fact the pass decoded, before any
	// aside's own effect is decided, so whether an aside is covered never
	// depends on the order its segment was scanned in. Loss withholding is
	// the feed filter's, and a loss record naming a record that another rule
	// also drops is covered only by that record, never by its own name.
	for _, d := range dl {
		if d.l.Synced && d.l.Reason == format.LossSuperseded {
			ks.superseded[d.l.Dropped] = append(ks.superseded[d.l.Dropped], d.h.Extent)
		} else {
			ks.whole[d.l.Dropped] = true
		}
	}
	for _, d := range df {
		if f, ok := ks.forget[d.h.Tag]; !ok || d.h.Seq > f {
			ks.forget[d.h.Tag] = d.h.Seq
		}
	}

	for _, d := range dl {
		if ks.covered(d.h.Seq, d.h.Tag) {
			continue // another rule already drops this loss record
		}
		piece := lossPiece{seq: d.l.Dropped, ext: d.h.Extent}
		if ks.pieces[piece] {
			continue // the piece is delivered: a carried-forward copy reports once
		}
		ks.pieces[piece] = true
		ev := core.LossEvent{
			Share:     d.h.Tag,
			File:      d.h.File,
			Extent:    d.h.Extent,
			Version:   d.h.Version,
			Reason:    fromFormat(d.l.Reason),
			Offloaded: d.l.Offloaded,
			Synced:    d.l.Synced,
			AtOpen:    true,
		}
		if losses != nil {
			losses.Place(ev, d.l.Dropped)
		}
		ks.Losses = append(ks.Losses, OpenLoss{Ev: ev, Dropped: d.l.Dropped})
	}

	highest := map[core.ShareTag]core.Seq{}
	for _, d := range df {
		if ks.covered(d.h.Seq, d.h.Tag) {
			continue // another rule already drops this forget record
		}
		if f, ok := highest[d.h.Tag]; !ok || d.h.Seq > f {
			highest[d.h.Tag] = d.h.Seq
		}
	}
	for _, tag := range slices.Sorted(maps.Keys(highest)) {
		ks.Forgets = append(ks.Forgets, OpenForget{Tag: tag, Seq: highest[tag]})
	}

	for _, d := range dh {
		if ks.covered(d.h.Seq, d.h.Tag) {
			// A covered record has no effect on any phase. Its body's
			// verdict is the walk's either way — a covered record whose
			// body rotted confines on its scan like any record of the
			// segment — so the covered gate alone keeps the refusal off.
			continue
		}
		// decision: a hold, unhold or stamp record that reached its scan
		// verified — the walk or the catalogue read its body whole — and
		// that no drop covers refuses the open, naming its kind and its
		// segment, because the snapshot state those kinds record is not
		// built and an open that served past one would serve without
		// knowing a snapshot was held. No journal holding one may open
		// until the holds' own rules are implemented, which is the only
		// thing that withdraws this marker.
		return ks, fmt.Errorf("%w: a %s record in segment %d at %d that no drop covers",
			core.ErrNotImplemented, d.h.Kind, d.h.Segment, d.h.Offset)
	}
	return ks, nil
}

// covered reports whether another rule the pass decoded already drops the
// aside whose numbers are named: a loss record naming its seq — whole or over
// an extent, whichever rule it carries — the failed list, or a forget of its
// own tag above it. Such a record has no effect on any phase.
func (ks *kindState) covered(seq core.Seq, tag core.ShareTag) bool {
	if ks.whole[seq] || ks.failed[seq] {
		return true
	}
	if _, held := ks.superseded[seq]; held {
		return true
	}
	if f, ok := ks.forget[tag]; ok && seq < f {
		return true
	}
	return false
}

// fromFormat maps a decoded loss reason to the reason the event it names
// carries. It panics on a reason that names none, which no decoded body can
// hold, for the format's decoder refuses one.
func fromFormat(r format.LossReason) core.LossReason {
	switch r {
	case format.LossCorrupt:
		return core.LossCorrupt
	case format.LossStale:
		return core.LossStale
	case format.LossFailedSync:
		return core.LossFailedWindow
	case format.LossSuperseded:
		return core.LossSuperseded
	case format.LossAcknowledged:
		return core.LossAcknowledged
	}
	panic(fmt.Sprintf("recovery: loss reason %d names no reason", uint8(r)))
}
