package recovery

import (
	"bytes"
	"cmp"
	"errors"
	"fmt"
	"io"
	"maps"
	"slices"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/format"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/index"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/reclaim"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/segment"
)

// OpenLoss is one loss record the open found on disk, decoded: its event,
// with AtOpen set and the number left for the open's publishing to assign, and the seq of
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
// process — a seq on the directory's failed list the feed finds on disk —
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
// its records named; the read-only drops and the withheld numbers the
// feed computes over the same records; and the liveness and accounting facts
// the feed collects over every record the scans attached.
type kindResults struct {
	// Losses holds every verified loss record the pass collected, in the
	// order the pass read it, its events unfired until the open's own
	// wiring places them into its loss ring.
	Losses []OpenLoss

	// Forgets holds one entry per forgotten share tag, ascending by tag, the
	// greatest seq the tag's forget records name.
	Forgets []OpenForget

	// ReadOnlyDrops holds the records the journal failed to write the loss
	// record for that this open finds on disk, one per failed seq, filled by
	// the feed that withholds them.
	ReadOnlyDrops []ReadOnlyDrop

	// Withheld is the numbers of the records the feed withholds whole — a
	// failed-list, whole-loss-named, forgotten or confined record, the ruler
	// of Rule 6 and the location rule — folded by their two axes, the
	// highest sequence number and the highest version among them
	// independently, so the counters open above every number the disk still
	// holds on both axes; a synced superseded cut leaves no residue to fold,
	// for its record fed. Zero until the feed computes it over the same
	// scans.
	Withheld index.Numbers

	// WithheldAt holds the locations of the records the rules named by
	// Withheld's failed-list, whole-loss and forget axes withheld — by their
	// numbers, wherever they sit — one entry per placement the feed served
	// nothing of. The confined copies stay out: the classing already carries
	// them as corrupt drops on their own scans. The tail's proof reads this
	// set so a record the feed withheld whole is proven, handed over and
	// re-placed nowhere.
	WithheldAt map[index.RecordAt]bool

	// Records holds one Rec per header-only record the scans attached — a
	// release, truncate, deallocate, delete, clone target, offloaded or
	// unmark record — filled by the feed, one entry per copy at the
	// placement its own slot spells, withheld copies included: every copy's
	// bytes keep its own segment held until its own release, whatever rule
	// withheld the record from the index. The aside kinds' own registrations
	// — a loss record, a forget — sit in LossRecs and ForgetRecs beside
	// these.
	Records []reclaim.Rec

	// LossRecs holds one Rec per attached loss record no rule covers,
	// filled by the decode pass per copy — the pieces it delivered once
	// each collect twice, for the two copies carry the record between them
	// — spelled from the record's own header and body the way the append
	// path registers the record it lands: the flags pick the loss's own
	// drop rule, and the placement names where the loss record's own bytes
	// sit, so a registration and its release key the same whole record.
	LossRecs []reclaim.Rec

	// ForgetRecs holds one Rec per attached forget record no rule covers,
	// per copy, spelled from the record's own header at the placement its
	// own slot gives: the tag is the one field its caller must spell out,
	// and a carried-forward forget registers in its own segment like any
	// other record.
	ForgetRecs []reclaim.Rec

	// Charging holds, per segment, per share tag, the record bytes of every
	// record the scans attached: each record's whole size and the same less
	// its payload, the two figures the journal's append charges the segment
	// and tag at the moment a record lands. The feed fills it over the
	// content records and the aside headers alike — the append charged each
	// of them, so the per-tag figures a reopen answers equal the running
	// journal's — withheld records included, for a record the rules
	// withhold stays on disk and stays charged. Nil until the feed runs.
	Charging map[core.SegmentID]map[core.ShareTag]Charge

	// MarkedOnDisk holds one entry per sequence number a caller's sync mark
	// names, false until the feed met a record the walks attached that
	// carries the number and true then, withheld copies included: a copy a
	// rule withheld from the index still verifies on disk as any fed one
	// does, wherever it sits. The lost-tail check reads it as the answer to
	// whether the number names a copy that verifies anywhere; the aside
	// kinds never feed, so their headers the check answers from the scans
	// themselves. Nil until the rebuild is run under marks.
	MarkedOnDisk map[core.Seq]bool
}

// Charge is one share tag's record bytes on one segment: the whole sizes of
// everything the segment holds of the tag, and the same less the payloads the
// records carry — a payload only a write and a fill have. A reopen hands the
// two figures to the segment ledger beside its adopted segments, the same
// pair the append charges record by record.
type Charge struct {
	Bytes    int64
	Overhead int64
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
	// reason names, the extents its records feed clipped over and the synced
	// bit every such loss payload carried — constant true today, since only
	// a synced superseded loss reaches the map, kept a field so the drops a
	// confined remainder confines read the bit the records named.
	superseded map[core.Seq]supersededLoss

	// forget holds the greatest seq each share tag's forget records name.
	forget map[core.ShareTag]core.Seq

	// failed holds the seqs the open's failed-list data named, the records a
	// read-only journal failed earlier in this process.
	failed map[core.Seq]bool

	// pieces holds the (dropped, extent) pairs already delivered, so two
	// identical loss records — a copy carried forward — report one loss.
	pieces map[lossPiece]bool

	// confined holds, by location, the content records whose superseded kept
	// remainder did not verify: the feed withholds that exact copy — never
	// its sequence number, or a healthy equal-seq twin elsewhere would
	// withhold with it — and the corrupt drops its kept runs became name the
	// same place for the classing and the append a later open makes.
	confined map[index.RecordAt]bool

	// copies holds, per fed record — its file and the seq every copy
	// carries — where the fed copies sit, for the repoint pass after the
	// cuts: a copy the collapse kept in a segment the open ends or saw fail
	// moves to the greatest copy a kept segment holds.
	copies map[copyKey][]index.RecordAt
}

// supersededLoss is the union of every synced-superseded loss record naming
// one seq: the extents its records feed clipped over and the synced bit the
// loss payloads carried.
type supersededLoss struct {
	extents []core.Extent
	synced  bool
}

// copyKey is one record across its copies: the file it belongs to and the
// sequence number every copy of it carries.
type copyKey struct {
	file core.FileID
	seq  core.Seq
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
// pass consumes. The collected losses land in State.Kinds.Losses and go no
// further here: the open's own wiring places them into its loss ring once
// the journal serves, so an open that refuses carries them nowhere — the
// run returns no state at all — and the pieces stay unreported for the
// retry. The failed list the pass withholds by is plain data the caller
// hands over, empty for none.
//
// A loss record whose header names a zero-length extent, or one that names
// no file, refuses the open naming its segment and its offset. A body that
// fails its checksum never reached the pass: a lost loss record errs toward
// fetching the extent again.
func decodeAsides(rd *reader, identity core.JournalID, failed []core.Seq, scs []segmentScan) (kindState, error) {
	ks := kindState{
		whole:       map[core.Seq]bool{},
		superseded:  map[core.Seq]supersededLoss{},
		forget:      map[core.ShareTag]core.Seq{},
		failed:      map[core.Seq]bool{},
		pieces:      map[lossPiece]bool{},
		confined:    map[index.RecordAt]bool{},
		copies:      map[copyKey][]index.RecordAt{},
		kindResults: kindResults{WithheldAt: map[index.RecordAt]bool{}},
	}
	for _, seq := range failed {
		ks.failed[seq] = true
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
	// depends on the order its segment was scanned in. Which of the decoded
	// loss and forget records hold an effect at all is decided first, by
	// effectiveAsides below, and every map the pass builds — the names the
	// feed withholds by, the forget cover both covered() and the feed read,
	// and the collected open losses and forgets — builds from the effective
	// alone: a covered record holds no effect anywhere, its withhold and its
	// cover of another aside together or neither, so a rule that withholds
	// nothing of its own covers nothing either. A record that another rule
	// drops is covered only by that record, never by its own name.
	effLoss, effForget := ks.effectiveAsides(dl, df)
	for i := range df {
		if !effForget[i] {
			continue
		}
		h := df[i].h
		ks.ForgetRecs = append(ks.ForgetRecs, reclaim.Rec{
			Kind: format.KindForget,
			Seg:  h.Segment,
			Pair: index.Numbers{Seq: h.Seq},
			Seq:  h.Seq,
			Tag:  h.Tag,
			Loc:  segment.Location{Segment: h.Segment, Off: h.Offset, Len: h.Size()},
		})
		if f, ok := ks.forget[h.Tag]; !ok || h.Seq > f {
			ks.forget[h.Tag] = h.Seq
		}
	}
	for i := range dl {
		if !effLoss[i] {
			continue
		}
		if dl[i].l.Synced && dl[i].l.Reason == format.LossSuperseded {
			sup := ks.superseded[dl[i].l.Dropped]
			sup.extents = append(sup.extents, dl[i].h.Extent)
			sup.synced = true
			ks.superseded[dl[i].l.Dropped] = sup
		} else {
			ks.whole[dl[i].l.Dropped] = true
		}
	}

	for i := range dl {
		if !effLoss[i] {
			continue
		}
		d := dl[i]
		// One registration per attached copy, before the piece it names is
		// delivered once: a carried-forward copy's bytes keep its own segment
		// held exactly as the record the copy carries does.
		ks.LossRecs = append(ks.LossRecs, reclaim.Rec{
			Kind:  format.KindLoss,
			File:  d.h.File,
			Seg:   d.h.Segment,
			Ext:   d.h.Extent,
			Pair:  index.Numbers{Version: d.h.Version, Seq: d.h.Seq},
			Seq:   d.h.Seq,
			Named: d.l.Dropped,
			Tag:   d.h.Tag,
			// The flags that pick the loss's own drop rule.
			Synced:     d.l.Synced,
			Superseded: d.l.Reason == format.LossSuperseded,
			Loc:        segment.Location{Segment: d.h.Segment, Off: d.h.Offset, Len: d.h.Size()},
		})
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
		ks.Losses = append(ks.Losses, OpenLoss{Ev: ev, Dropped: d.l.Dropped})
	}

	// The forget map above is the tag maxima over the effective forgets
	// alone; the collected answer reads it rather than re-maxing the same
	// records a second map in, so the pass holds the forget facts once.
	for _, tag := range slices.Sorted(maps.Keys(ks.forget)) {
		ks.Forgets = append(ks.Forgets, OpenForget{Tag: tag, Seq: ks.forget[tag]})
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

// decoded is one aside record the scan carried: its header, the scan it
// sits on — nil for a forget, whose facts are headerborne alone — and the
// body the walk read and verified, decoded, body and header both whole.
type decoded struct {
	sc *segmentScan
	h  format.RecordHeader
	l  format.Loss
}

// effectiveAsides decides which of the decoded loss and forget records hold
// an effect at all — the ones no effective rule drops. Rule 6 is uniform: a
// covered record has no effect anywhere, its name, its cover of another
// aside and its hold on the feed together or neither, so the maps the feed
// withholds by, the forget map covered() reads and the collected open
// forgets build from the answer here, and a rule that withholds nothing of
// its own covers nothing either.
//
// Every cover runs from a higher sequence number to a lower one: a loss
// names a record the journal appended before itself, a forget covers seqs
// below its own, and the failed list names the records of a read-only
// window earlier than the open. One pass over the asides merged by seq
// descending therefore judges each record before the cover it could carry of
// others exists, against the names and the forget maxima the effective
// records of higher seq already built — so a rule another rule covers
// covers nothing: its own name is cancelled before the records it would
// name are judged, and they keep their covers. No record is ever judged
// against its own name, which enters the maps only after it. A loss naming
// a record of a higher seq is damage the journal never appends, and it
// covers nothing under this pass either; an equal seq between two asides is
// damage too — the journal issues one number once — and the pass keeps its
// own order among them, losses before forgets, so the answer stays
// deterministic whatever the damage.
func (ks *kindState) effectiveAsides(dl, df []decoded) (effLoss, effForget []bool) {
	effLoss, effForget = make([]bool, len(dl)), make([]bool, len(df))
	// named holds every seq an effective loss record names, whole or
	// superseded — covered answers by presence alone — and forgot each
	// tag's greatest effective forget seq. Both grow as the pass descends.
	named := map[core.Seq]bool{}
	forgot := map[core.ShareTag]core.Seq{}
	covered := func(seq core.Seq, tag core.ShareTag) bool {
		if ks.failed[seq] || named[seq] {
			return true
		}
		if f, ok := forgot[tag]; ok && seq < f {
			return true
		}
		return false
	}
	// order walks every aside once, highest seq first, keeping its own
	// index so the answer stays in the pass's decode order; the loss
	// records precede the forget records at an equal seq, the pass's tie.
	type asideAt struct {
		d      decoded
		forget bool
		i      int
	}
	order := make([]asideAt, 0, len(dl)+len(df))
	for i, d := range dl {
		order = append(order, asideAt{d: d, i: i})
	}
	for i, d := range df {
		order = append(order, asideAt{d: d, forget: true, i: i})
	}
	slices.SortStableFunc(order, func(a, b asideAt) int {
		return cmp.Compare(b.d.h.Seq, a.d.h.Seq)
	})
	for _, a := range order {
		if covered(a.d.h.Seq, a.d.h.Tag) {
			continue // another effective rule drops this record whole
		}
		if a.forget {
			if f, ok := forgot[a.d.h.Tag]; !ok || a.d.h.Seq > f {
				forgot[a.d.h.Tag] = a.d.h.Seq
			}
			effForget[a.i] = true
			continue
		}
		named[a.d.l.Dropped] = true
		effLoss[a.i] = true
	}
	return effLoss, effForget
}

// withheldWhole reports whether a rule drops rec whole, whatever a
// superseded loss names over an extent beside it: a seq on the failed
// list, a whole loss name, or a forget of the record's tag above its seq.
// The feed's whole-withhold and the remainder pass's skip read this one
// answer, so a record one rule drops whole is never read back at open — its
// kept runs confine nowhere while it withholds anyway, and a rotten record
// another rule already holds whole would otherwise confine fresh at every
// open, its corrupt drop naming a record the feed serves nothing of.
func (ks *kindState) withheldWhole(rec index.Record) bool {
	seq := rec.Numbers.Seq
	if ks.failed[seq] || ks.whole[seq] {
		return true
	}
	if f, ok := ks.forget[rec.Tag]; ok && seq < f {
		return true
	}
	return false
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
// carries. The panics on a reason that names none are chosen over refusing
// because no decoded body can hold one — the format's decoder already
// refused it before the body reached this converter.
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

// keptRuns returns the runs of e the cuts leave, in offset order: the runs of
// a record a synced superseded loss names that the loss's own cut does not
// take, its extent minus the union of every extent the loss records naming
// it cover. The record feeds whole and the cut splits it after the rebuild
// finishes; this shape is the remainder pass's alone — what it verifies and
// what a rotten record's confinements name — never the feed's.
func keptRuns(e core.Extent, cuts []core.Extent) []core.Extent {
	cs := slices.Clone(cuts)
	slices.SortFunc(cs, func(a, b core.Extent) int { return cmp.Compare(a.Off, b.Off) })
	end := e.Off + e.Len
	var out []core.Extent
	cursor := e.Off
	for _, c := range cs {
		// A cut that does not overlap the record clips nothing of it, its
		// clamped run empty: asking it for a run would feed a bogus piece
		// from the record's start up to the cut's own off, past the
		// record's end or before its start.
		lo, hi := max(c.Off, e.Off), min(c.Off+c.Len, end)
		if lo >= hi {
			continue
		}
		if lo > cursor {
			out = append(out, core.Extent{Off: cursor, Len: lo - cursor})
		}
		cursor = max(cursor, hi)
	}
	if cursor < end {
		out = append(out, core.Extent{Off: cursor, Len: end - cursor})
	}
	return out
}

// verifyRemainders is the open's check of a partly-superseded record, run
// over the buffered records between the decode pass and the tail's classing:
// every write and fill a synced superseded loss names keeps a run outside
// the union of the loss extents only by feeding clipped, so its whole
// payload is read through the open's reader and verified here — the one
// content payload an open checks, for a record whose kept bytes would never
// meet a serving read that verifies them.
//
// The record that fails confines its kept runs as corrupt drops on its own
// scan — one per run, each carrying the run's extent over the record's own
// header, with the synced bit the loss payload named — so the tail's classing
// judges them like any other dropped record, and the filter withholds that
// exact copy by location, never the seq it shares with a healthy twin
// elsewhere. A read no bounded attempt cleared refuses the open on the
// ranges the reader collected, never as a corrupt verdict.
//
// decision: a record the loss extents cover whole, one another rule already
// withholds whole (wholeWithheld) or a removal, release or mark the loss
// names, is verified not at all — no byte it holds would ever feed, so no
// read of it would serve, and a record another rule withholds whole never
// meets the read that would catch it, so confining it would re-drop, every
// open, a record the feed serves nothing of. The whole-payload verify is
// the ceiling this pays for keeping the format's one checksum; overturned
// by a ruled checksum per payload piece, which a partly-superseded record
// could yet verify by.
func (ks *kindState) verifyRemainders(rd *reader, identity core.JournalID, scs []segmentScan) error {
	for i := range scs {
		sc := &scs[i]
		for _, rec := range sc.Records {
			sup, named := ks.superseded[rec.Numbers.Seq]
			if !named || (rec.Kind != index.RecWrite && rec.Kind != index.RecFill) {
				continue
			}
			if ks.withheldWhole(rec) {
				continue // another rule drops the record whole: no run of it feeds
			}
			kept := keptRuns(core.Extent{Off: rec.Off, Len: rec.Len}, sup.extents)
			if len(kept) == 0 {
				continue
			}
			verified, err := verifyContentBody(rd, identity, rec)
			if err != nil {
				return err
			}
			if verified {
				continue
			}
			at := index.RecordAt{Segment: rec.Segment, Offset: rec.At}
			ks.confined[at] = true
			for _, run := range kept {
				kind := format.KindWrite
				if rec.Kind == index.RecFill {
					kind = format.KindFill
				}
				// The drop's header is the record's own columns with the
				// run's extent, so the classing judges the drop where the
				// record sits and the loss record a later open appends for
				// it names the run, not the extent the loss superseded.
				sc.Dropped = append(sc.Dropped, droppedRecord{
					Header: format.RecordHeader{
						Kind: kind, Segment: rec.Segment, File: rec.File, Extent: run,
						Seq: rec.Numbers.Seq, Version: rec.Numbers.Version, Tag: rec.Tag,
						Offset: int64(rec.At),
					},
					Synced: sup.synced,
					Err: fmt.Errorf("%w: segment %d: the %s at %d fails its own verification over the run its loss did not supersede",
						core.ErrCorrupt, rec.Segment, kind, rec.At),
				})
			}
		}
	}
	return nil
}

// applyCuts is each synced loss record's release into the index the rebuild
// finished: the losses' whole pass over every file, where each cut lands
// over the index Finish built. The extent and the version come from the
// loss record's own header — the version of the record the journal dropped
// — and a superseded one cuts with no marker, as its own rule decides,
// while every other reason leaves one, unsettled until the caller settles
// at or above its version. The cuts apply in the decode's read order, and
// markers insert by version, so the order the cuts apply in decides
// nothing the rebuilt state shows.
//
// decision: an unsynced loss record applies no cut — the record it names was
// never proven whole, and a cut would drop, as lost, content the journal
// that crashed before the sync may still write again over the same extent.
// Overturned by a rule that makes the sync's proof depend on nothing later.
func (ks *kindState) applyCuts(ix *index.Index) error {
	for _, lo := range ks.Losses {
		if !lo.Ev.Synced {
			continue
		}
		// The segments the cuts lower are discarded: the open has no
		// worker that retires a segment over them, so the ids would name
		// work nothing does.
		var err error
		if lo.Ev.Reason == core.LossSuperseded {
			_, _, err = ix.LoseSuperseded(lo.Ev.File, lo.Ev.Extent, lo.Ev.Version, lo.Dropped)
		} else {
			_, _, err = ix.Lose(lo.Ev.File, lo.Ev.Extent, lo.Ev.Version)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// keptHome is the open's home policy as one predicate: whether the record
// at off in segment id survives the open — any segment in a fate the
// classing names except an ending; a segment no fate names keeps, for only
// the classing names an ending and nothing else ends one; an ended segment
// keeps none of its records; and a torn tail takes only the records at or
// above its cut, the ones below surviving with the segment it seals. The
// subtraction over the corrupt drops and the repoint over the fed copies
// both judge a home by this, so the policy is written once.
func keptHome(tl tails) func(core.SegmentID, int64) bool {
	fates := make(map[core.SegmentID]segmentFate, len(tl.Fates))
	for _, f := range tl.Fates {
		fates[f.ID] = f
	}
	return func(id core.SegmentID, off int64) bool {
		f, judged := fates[id]
		if !judged {
			return true
		}
		switch f.Kind {
		case fateEnded:
			// decision: every record of an ended segment is a bad home
			// whatever its place, for the phase that ends it truncates it
			// and then empties it — a fate with no cut still ends the
			// whole segment. Overturned by an ended fate that leaves any
			// record behind.
			return false
		case fateTornTail:
			// decision: only the records at or above a torn tail's cut go
			// with it — the tail truncates the segment at the cut and then
			// seals it, so a record whose slot starts below the cut
			// survives with the segment the open keeps. Overturned by a
			// torn-tail fate that drops a record below its cut.
			return off < f.Offset
		default:
			return true
		}
	}
}

// subtractCopies is the bad-copy half of the copies rule, run once the tail's
// classing has judged every drop: a corrupt drop an equal-sequence record
// another scan holds in a home the open keeps — keptHome's one policy, which
// keeps a record below a torn tail's cut like any intact segment's — is
// removed from Corrupt, for the live copy serves what the drop guarded and
// the loss record a later open would append for it would drop a record a
// twin contradicts. Damaged is not touched: the classing's verdict on the
// segment stands whatever serves in its place, and the tail phases cut and
// report by it.
func (ks *kindState) subtractCopies(scans []segmentScan, tl tails) []corruptRecord {
	kept := keptHome(tl)
	// live is every seq a surviving home holds a FED record of, in
	// whichever segment: the one membership test below, not one per drop.
	// A location the remainder pass confined never joins, for the open
	// itself found that copy rotten: two rotten twins would each count the
	// other live, both would subtract and neither drop would ever reach the
	// loss record a later open appends for it — the very copy the open
	// found dead counts only against itself.
	var live map[core.Seq][]core.SegmentID
	for i := range scans {
		for _, rec := range scans[i].Records {
			if !kept(rec.Segment, int64(rec.At)) {
				continue // the open ends this record's home
			}
			if ks.confined[index.RecordAt{Segment: rec.Segment, Offset: rec.At}] {
				continue // the open confined this copy: it fed nothing
			}
			if live == nil {
				live = map[core.Seq][]core.SegmentID{}
			}
			live[rec.Numbers.Seq] = append(live[rec.Numbers.Seq], rec.Segment)
		}
	}
	if live == nil {
		return tl.Corrupt
	}
	out := tl.Corrupt[:0]
	for _, c := range tl.Corrupt {
		for _, s := range live[c.Drop.Header.Seq] {
			if s != c.Segment {
				// A bad copy does not mask the good one that replaced it.
				goto subtract
			}
		}
		out = append(out, c)
	subtract:
	}
	return out
}

// repointCopies is the copy preference the deliverable spells out, run after
// the cuts over the finished index: for every sequence number fed with two or
// more copies, the rebuild's collapse keeps the one at the greatest
// (segment, offset); where that keeps a copy in a home the open ends — an
// ended segment, or a record at or above a torn tail's cut — while another
// fed copy sits in a surviving home, the piece moves to the greatest
// surviving copy — a read then names a home the open keeps. Copies the
// repoint leaves note like today: the collapse's word stands.
func (ks *kindState) repointCopies(ix *index.Index, tl tails) {
	if len(ks.copies) == 0 {
		return
	}
	kept := keptHome(tl)
	greatest := func(cps []index.RecordAt) int {
		best := 0
		for i := 1; i < len(cps); i++ {
			if cps[i].Segment > cps[best].Segment ||
				(cps[i].Segment == cps[best].Segment && cps[i].Offset > cps[best].Offset) {
				best = i
			}
		}
		return best
	}
	for _, k := range sortedCopyKeys(ks.copies) {
		cps := ks.copies[k]
		if len(cps) < 2 {
			continue
		}
		keep := greatest(cps)
		if kept(cps[keep].Segment, int64(cps[keep].Offset)) {
			continue // the collapse's word stands: the kept copy survives the open
		}
		// The greatest copy among the surviving homes' own.
		best := -1
		for i, at := range cps {
			if !kept(at.Segment, int64(at.Offset)) {
				continue
			}
			if best < 0 || at.Segment > cps[best].Segment ||
				(at.Segment == cps[best].Segment && at.Offset > cps[best].Offset) {
				best = i
			}
		}
		if best < 0 {
			continue
		}
		// decision: a repoint that fails is dropped, not surfaced: the
		// piece still serves from the end the collapse kept until the tail's
		// cut empties it, so a failure here loses a preference, not the
		// read, and refusing the open would drop a journal that serves.
		// Overturned by a failure that leaves the piece unreachable instead.
		_, _, _ = ix.RepointRecord(k.file, cps[keep], cps[best])
	}
}

// sortedCopyKeys orders the fed-copy keys, by seq then file, so the repoint
// pass runs one deterministic order over the copies on disk.
func sortedCopyKeys(m map[copyKey][]index.RecordAt) []copyKey {
	keys := slices.Collect(maps.Keys(m))
	slices.SortFunc(keys, func(a, b copyKey) int {
		x, y := a.file.Bytes(), b.file.Bytes()
		return cmp.Or(cmp.Compare(a.seq, b.seq), bytes.Compare(x[:], y[:]))
	})
	return keys
}

// verifyContentBody reads one buffered content record's header and whole body
// through the open's bound and verifies both, the only read of a content
// payload an open makes. It answers whether the record verified; a read the
// bound did not clear returns as its own error and never as a verdict.
func verifyContentBody(rd *reader, identity core.JournalID, rec index.Record) (bool, error) {
	name := format.SegmentName(rec.Segment)
	hb := make([]byte, format.RecordHeaderSize)
	n, rerr := rd.ReadAt(name, hb, int64(rec.At))
	if rerr != nil && !errors.Is(rerr, io.EOF) {
		return false, refusedRead(rd, rerr)
	}
	h, derr := format.DecodeRecordHeader(hb[:n], identity, rec.Segment, int64(rec.At))
	if derr != nil {
		return false, nil // the bytes changed under the open: verification fails
	}
	body := make([]byte, h.BodySize())
	bn, berr := rd.ReadAt(name, body, h.Offset+format.RecordHeaderSize)
	if berr != nil && !errors.Is(berr, io.EOF) {
		return false, refusedRead(rd, berr)
	}
	if _, verr := format.VerifyRecordBody(body[:bn], identity, h); verr != nil {
		return false, nil
	}
	return true, nil
}

// refusedRead answers a read no bounded attempt cleared: as itself when the
// reader collected no range — a cancellation the open itself caused — and as
// the collected ranges otherwise, so the open's refusal names every range the
// device did not answer, never a verdict on bytes the bound never returned.
func refusedRead(rd *reader, err error) error {
	if isContextErr(err) {
		return err
	}
	if ranges := rd.Unreadable(); len(ranges) > 0 {
		return &core.UnreadableRangesError{Ranges: ranges}
	}
	return err
}

// feedFilter is the serial feed's per-record decision, built once over the
// kinds the decode pass collected before a single record is fed. Each
// buffered record meets every on-disk aside rule in the fixed order below —
// withheld records' segments and fed copies are collected as the feed runs,
// so the liveness the State carries and the repoint pass reads are answers
// over the records that actually sat on the segments the open scanned; the
// header-only records' Recs and every record's charge collect beside them,
// before any rule withholds anything, so the assembly reads answers over
// every record the scans attached.
type feedFilter struct {
	ks *kindState
	// losses indexes, per named seq, the collected losses naming it;
	// forgets, per tag, the collected forgets of the tag; drops, per failed
	// seq, the collected read-only note of the seq; marked is the marked-seq
	// map the feed raises — nil until the marks' sequence numbers reach it.
	losses  map[core.Seq][]int
	forgets map[core.ShareTag][]int
	drops   map[core.Seq]int
	marked  map[core.Seq]bool
}

// newFeedFilter makes the filter's lookups over the collected losses,
// forgets and read-only drops, in collection order, and the charge table the
// feed fills through it.
func (ks *kindState) newFeedFilter() *feedFilter {
	ks.Charging = make(map[core.SegmentID]map[core.ShareTag]Charge)
	f := &feedFilter{ks: ks,
		losses:  make(map[core.Seq][]int, len(ks.Losses)),
		forgets: make(map[core.ShareTag][]int, len(ks.Forgets)),
		drops:   make(map[core.Seq]int, len(ks.ReadOnlyDrops)),
		marked:  ks.MarkedOnDisk,
	}
	for i := range ks.Losses {
		f.losses[ks.Losses[i].Dropped] = append(f.losses[ks.Losses[i].Dropped], i)
	}
	for i := range ks.Forgets {
		f.forgets[ks.Forgets[i].Tag] = append(f.forgets[ks.Forgets[i].Tag], i)
	}
	for i := range ks.ReadOnlyDrops {
		f.drops[ks.ReadOnlyDrops[i].Dropped] = i
	}
	return f
}

// appendSeg joins id to the tail of segs once, so one record seen twice in
// one segment names it once.
func appendSeg(segs []core.SegmentID, id core.SegmentID) []core.SegmentID {
	if !slices.Contains(segs, id) {
		segs = append(segs, id)
	}
	return segs
}

// feed judges one buffered record against every rule the decoded asides
// carry, and feeds what survives to do — the serial feed drives a whole scan
// by it, so this is the only path a converted record reaches the builder
// through. The rules decide, in feed order:
//
//   - a seq on the failed list, a forget of the record's tag above its seq,
//     or a loss record naming the seq whole withholds the record whole;
//   - a location the remainder pass confined withholds that exact copy — by
//     where it sits, never by its seq, or a healthy equal-seq twin elsewhere
//     would withhold with it;
//   - a synced superseded loss naming the record shapes nothing here: the
//     record feeds whole, and the loss's own cut splits it where the
//     rebuild has finished, so the runs the loss leaves hold with the
//     record's own single place and skip and no run is silently lost;
//   - everything else feeds, and every fed location joins the copies the
//     repoint pass reads.
//
// Every rule that names the record — a loss whole or superseded, a forget of
// the tag, the failed list — collects the segment the record sits in,
// whatever the decision was: withheld records' homes included, for a withheld
// record stays on disk until the same copy that retires it does, and the
// liveness the State carries is what names when that may be.
//
// A withheld record's numbers fold into Withheld, so the counters a later
// pass raises open above the numbers the feed never saw.
//
// Before any rule decides anything, the record raises the marks' own note:
// a walk-attached copy carrying a sequence number a caller's sync mark names
// sets that number's entry, for a withheld copy verifies on disk exactly as a
// fed one does and the lost-tail check reads the mark's whole question —
// does the record verify anywhere — from the map alone.
func (f *feedFilter) feed(rec index.Record, do func(index.Record) error) error {
	f.note(rec)
	if _, named := f.marked[rec.Numbers.Seq]; named {
		f.marked[rec.Numbers.Seq] = true
	}
	ks := f.ks
	seq := rec.Numbers.Seq
	for _, i := range f.losses[seq] {
		ks.Losses[i].Segments = appendSeg(ks.Losses[i].Segments, rec.Segment)
	}
	for _, i := range f.forgets[rec.Tag] {
		if seq < ks.Forgets[i].Seq {
			ks.Forgets[i].Segments = appendSeg(ks.Forgets[i].Segments, rec.Segment)
		}
	}
	if ks.failed[seq] {
		if i, ok := f.drops[seq]; ok {
			ks.ReadOnlyDrops[i].Segments = appendSeg(ks.ReadOnlyDrops[i].Segments, rec.Segment)
		} else {
			// One note per seq, its event the first record the feed finds
			// answers with — the feed runs in one deterministic order, so
			// the note names the same record whatever segment holds a copy.
			ks.ReadOnlyDrops = append(ks.ReadOnlyDrops, ReadOnlyDrop{
				Ev: core.LossEvent{
					Share:   rec.Tag,
					File:    rec.File,
					Extent:  core.Extent{Off: rec.Off, Len: rec.Len},
					Version: rec.Numbers.Version,
					Reason:  core.LossFailedWindow,
				},
				Dropped:  seq,
				Segments: []core.SegmentID{rec.Segment},
			})
			f.drops[seq] = len(ks.ReadOnlyDrops) - 1
		}
	}

	withheld := ks.withheldWhole(rec) ||
		ks.confined[index.RecordAt{Segment: rec.Segment, Offset: rec.At}]
	if withheld {
		if ks.withheldWhole(rec) {
			// The location joins the withheld set the later phases read: a
			// record the rules drop whole is judged nowhere past this feed.
			// The confined copies stay out, for the classing already names
			// them as corrupt drops where they sit.
			ks.WithheldAt[index.RecordAt{Segment: rec.Segment, Offset: rec.At}] = true
		}
		// The withheld numbers fold by their two axes, the highest sequence
		// number and the highest version among the withheld independently:
		// no one record need carry both, and the counters a later pass
		// raises must open above every number the feed never saw on both
		// axes, so the pair the fold answers with may be one no record held.
		if rec.Numbers.Seq > ks.Withheld.Seq {
			ks.Withheld.Seq = rec.Numbers.Seq
		}
		if rec.Numbers.Version.Compare(ks.Withheld.Version) > 0 {
			ks.Withheld.Version = rec.Numbers.Version
		}
		return nil
	}
	if err := do(rec); err != nil {
		return err
	}
	f.noteCopy(rec)
	return nil
}

// noteCopy joins where rec sits to the copies of its seq, one entry per fed
// record.
func (f *feedFilter) noteCopy(rec index.Record) {
	k := copyKey{file: rec.File, seq: rec.Numbers.Seq}
	at := index.RecordAt{Segment: rec.Segment, Offset: rec.At}
	if !slices.Contains(f.ks.copies[k], at) {
		f.ks.copies[k] = append(f.ks.copies[k], at)
	}
}

// note collects one attached record's liveness and accounting facts, before
// any rule decides anything about it: the record charges its segment and
// tag its whole size and the same less its payload, withheld records among
// them — the append charged each record at the moment it landed, whatever
// followed — and a record of one of the header-only kinds notes one Rec at
// the placement its own slot spells, per copy, for every copy's bytes keep
// its own segment held until its own release.
func (f *feedFilter) note(rec index.Record) {
	size := format.RecordSize(fedFormatKind(rec.Kind), rec.Len)
	payload := int64(0)
	if rec.Kind == index.RecWrite || rec.Kind == index.RecFill {
		payload = rec.Len
	}
	f.charge(rec.Segment, rec.Tag, size, payload)
	if k, headerOnly := fedHeaderOnlyKind(rec.Kind); headerOnly {
		f.ks.Records = append(f.ks.Records, reclaim.Rec{
			Kind: k,
			File: rec.File,
			Seg:  rec.Segment,
			Ext:  core.Extent{Off: rec.Off, Len: rec.Len},
			Pair: rec.Numbers,
			Seq:  rec.Numbers.Seq,
			Tag:  rec.Tag,
			Loc:  segment.Location{Segment: rec.Segment, Off: int64(rec.At), Len: size},
		})
	}
}

// charge adds one record's figures to the segment and tag it sits in: the
// whole size of its slot, and the same less its payload. Everything a scan
// attached charges alike — the rules below decide what the index holds, not
// what the segment's storage holds — so the charge answers the pair the
// append charged, and a reopen hands the ledger the same figures back.
func (f *feedFilter) charge(seg core.SegmentID, tag core.ShareTag, size, payload int64) {
	tags := f.ks.Charging[seg]
	if tags == nil {
		tags = make(map[core.ShareTag]Charge)
		f.ks.Charging[seg] = tags
	}
	c := tags[tag]
	c.Bytes += size
	c.Overhead += size - payload
	tags[tag] = c
}

// chargeAsides charges every aside record one scan set aside to the segment
// and tag its own header names — the whole size of its slot, no payload —
// beside the content records the feed charges: the append charged each of
// them too, so a segment holding nothing but asides still answers the
// figures the running journal left on it. skip keeps out an aside placement
// the tail proof judged torn, which charges like any other record the
// journal never wrote.
//
// decision: a record the walk confined as a corrupt drop — a clone target
// whose body did not verify, an aside body that failed — charges nothing,
// for it was never attached and the segment that holds it waits on the tail
// phases' re-append and emptying, which charge what they re-append. The
// figures of a reopened journal therefore miss only bytes that are on their
// way out; overturned by a tail phase that ever leaves a confined record's
// bytes on the segment it keeps.
func (f *feedFilter) chargeAsides(sc *segmentScan, skip map[index.RecordAt]bool) {
	for _, a := range sc.Aside {
		if skip[index.RecordAt{Segment: a.Header.Segment, Offset: uint32(a.Header.Offset)}] {
			continue
		}
		f.charge(a.Header.Segment, a.Header.Tag, a.Header.Size(), 0)
	}
}

// fedFormatKind maps the kind of a record the rebuild feeds back to the
// format kind its header carried — the inverse of contentRecord's per-kind
// conversion — so the facts a collected record owes can ask the format its
// own size rules. Every attached record's kind is one of the nine.
func fedFormatKind(k index.RecordKind) format.Kind {
	switch k {
	case index.RecWrite:
		return format.KindWrite
	case index.RecFill:
		return format.KindFill
	case index.RecRelease:
		return format.KindRelease
	case index.RecTruncate:
		return format.KindTruncate
	case index.RecDeallocate:
		return format.KindDeallocate
	case index.RecDelete:
		return format.KindDelete
	case index.RecCloneTarget:
		return format.KindCloneTarget
	case index.RecOffloaded:
		return format.KindOffloaded
	case index.RecUnmark:
		return format.KindUnmark
	}
	panic(fmt.Sprintf("recovery: record kind %d names no fed kind", uint8(k)))
}

// fedHeaderOnlyKind answers the format kind one of the header-only content
// kinds maps to — release through unmark, the seven the liveness tracker
// registers — and false for the two that carry a payload.
func fedHeaderOnlyKind(k index.RecordKind) (format.Kind, bool) {
	switch k {
	case index.RecRelease:
		return format.KindRelease, true
	case index.RecTruncate:
		return format.KindTruncate, true
	case index.RecDeallocate:
		return format.KindDeallocate, true
	case index.RecDelete:
		return format.KindDelete, true
	case index.RecCloneTarget:
		return format.KindCloneTarget, true
	case index.RecOffloaded:
		return format.KindOffloaded, true
	case index.RecUnmark:
		return format.KindUnmark, true
	}
	return 0, false
}
