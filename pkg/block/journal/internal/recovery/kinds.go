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

	// Withheld is the numbers of the records the feed withholds, folded by
	// their two axes — the highest sequence number and the highest version
	// among them independently — so the counters open above every number
	// the disk still holds on both axes; zero until the feed computes it
	// over the same scans.
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
	// reason names, the extents its records feed clipped over and the synced
	// bit every such loss payload carried — constant true today, since only
	// a synced superseded loss reaches the map, kept a field so the drops a
	// confined remainder confines read the bit the records named.
	superseded map[core.Seq]supersededLoss

	// forget holds the greatest seq each share tag's forget records name.
	forget map[core.ShareTag]core.Seq

	// failed holds the seqs the loss sink's failed list named, the records a
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

// decodeAsides reads the bodies of the aside records the rebuild set aside,
// decodes them and decides each record against the kinds' own rules, over
// the scans the rebuild answered in — confined drops land on the same scans
// the tail's classing reads — and through the same bound reader every open
// read takes. loss deliveries reach the sink when one is given, and none is
// required: a nil one means no failed list and no placement.
//
// A loss record whose header names a zero-length extent, or one that names
// no file, refuses the open naming its segment and its offset. A loss body
// that fails its checksum, or a checksum-valid body this format version does
// not define — a reason or a flag word no writer writes — confines to its
// own record as a corrupt drop, exactly as the catalogue's read of the same
// kinds confines one, and the pass reads on: a lost loss record errs toward
// fetching the extent again.
func decodeAsides(rd *reader, identity core.JournalID, losses LossSink, scs []segmentScan) (kindState, error) {
	ks := kindState{
		whole:      map[core.Seq]bool{},
		superseded: map[core.Seq]supersededLoss{},
		forget:     map[core.ShareTag]core.Seq{},
		failed:     map[core.Seq]bool{},
		pieces:     map[lossPiece]bool{},
		confined:   map[index.RecordAt]bool{},
		copies:     map[copyKey][]index.RecordAt{},
	}
	if losses != nil {
		for _, seq := range losses.Failed() {
			ks.failed[seq] = true
		}
	}

	// decoded keeps what the first pass read — each aside's record and the
	// scan it sits on — so the second pass decides the records against the
	// complete maps without reading again.
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
				vb, err := verifyAsideBody(rd, identity, sc, h)
				if err != nil {
					if errors.Is(err, core.ErrCorrupt) {
						continue // confined on its scan, for the classing
					}
					return ks, err
				}
				dl = append(dl, decoded{sc: sc, h: h, l: vb.Loss})
			case format.KindForget:
				df = append(df, decoded{h: h})
			case format.KindHold, format.KindUnhold, format.KindStamp:
				dh = append(dh, decoded{sc: sc, h: h})
			}
		}
	}

	// The cover maps are built from every fact the pass decoded, before any
	// aside's own effect is decided, so whether an aside is covered never
	// depends on the order its segment was scanned in. The loss names are
	// gated on covered like every effect a loss carries is: a covered loss
	// record holds no effect anywhere, its withhold and its clip together or
	// neither — a name that acted alone would leave a hole in the feed with
	// no event, cut or marker explaining it, and a clip with no cut is a
	// state no rule of the disk can produce. The forget map completes before
	// the gate, so a forget covering a loss needs no scan order; a loss named
	// only by another loss gains its name only if that one was decoded
	// first and uncovered — a chain of mutually-naming losses is damage no
	// writer writes. A record that another rule also drops is covered only
	// by that record, never by its own name.
	for _, d := range df {
		if f, ok := ks.forget[d.h.Tag]; !ok || d.h.Seq > f {
			ks.forget[d.h.Tag] = d.h.Seq
		}
	}
	for _, d := range dl {
		if ks.covered(d.h.Seq, d.h.Tag) {
			continue // another rule already drops this loss record
		}
		if d.l.Synced && d.l.Reason == format.LossSuperseded {
			sup := ks.superseded[d.l.Dropped]
			sup.extents = append(sup.extents, d.h.Extent)
			sup.synced = true
			ks.superseded[d.l.Dropped] = sup
		} else {
			ks.whole[d.l.Dropped] = true
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
		// A covered forget reaches the cover map above but never this
		// slice: the record that covers its seq drops it whole, so withholds
		// below its seq are still the truth of the feed, while a collected
		// forget would name a tag whose seq the feed dropped another way.
		ks.Forgets = append(ks.Forgets, OpenForget{Tag: tag, Seq: highest[tag]})
	}

	for _, d := range dh {
		if ks.covered(d.h.Seq, d.h.Tag) {
			// A covered record has no effect on any phase, and its body
			// needs no verdict of its own: the record is dropped whole
			// either way, and reading its bytes would only verdict them
			// for a classing the drop supersedes.
			continue
		}
		if _, err := verifyAsideBody(rd, identity, d.sc, d.h); err != nil {
			if errors.Is(err, core.ErrCorrupt) {
				continue // confined on its scan, for the classing
			}
			return ks, err
		}
		// decision: a hold, unhold or stamp record that verifies and is not
		// covered refuses the open, naming its kind and its segment,
		// because the snapshot state those kinds record is not built and an
		// open that served past one would serve without knowing a
		// snapshot was held. No journal holding one may open until the
		// holds' own rules are implemented, which is the only thing that
		// withdraws this marker.
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

// verifyAsideBody reads one aside record's body through the open's bound and
// verifies it whole, as the only path that hands a record's bytes out, and
// decodes it. A body that does not verify — the checksum its bytes fail, or
// content this format version does not define — confines to its own record
// as a corrupt drop on its scan, carried for the tail's classing, and the
// bare core.ErrCorrupt answers alone; any other read error returns as itself
// and is the open's refusal.
func verifyAsideBody(rd *reader, identity core.JournalID, sc *segmentScan, h format.RecordHeader) (format.RecordBody, error) {
	body := make([]byte, h.BodySize())
	n, rerr := rd.ReadAt(format.SegmentName(h.Segment), body, h.Offset+format.RecordHeaderSize)
	if rerr != nil && !errors.Is(rerr, io.EOF) {
		return format.RecordBody{}, rerr
	}
	vb, verr := format.VerifyRecordBody(body[:n], identity, h)
	if verr != nil {
		sc.Dropped = append(sc.Dropped, droppedRecord{Header: h,
			Err: fmt.Errorf("%w: segment %d: the %s at %d: %v",
				core.ErrCorrupt, h.Segment, h.Kind, h.Offset, verr)})
		return format.RecordBody{}, core.ErrCorrupt
	}
	return vb, nil
}

// keptRuns returns the runs of e the cuts leave, in offset order: the runs a
// record a synced superseded loss names feeds over, its extent minus the
// union of every extent the loss records naming it cover.
func keptRuns(e core.Extent, cuts []core.Extent) []core.Extent {
	cs := slices.Clone(cuts)
	slices.SortFunc(cs, func(a, b core.Extent) int { return cmp.Compare(a.Off, b.Off) })
	end := e.Off + e.Len
	var out []core.Extent
	cursor := e.Off
	for _, c := range cs {
		lo, hi := max(c.Off, e.Off), min(c.Off+c.Len, end)
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
// decision: a record the loss extents cover whole is verified not at all —
// no byte it holds would ever feed, so no read of it would serve — and a
// removal, release or mark the loss names is left to its own kind's rules,
// the clip being a content cut. The whole-payload verify is the ceiling this
// pays for keeping the format's one checksum; overturned by a ruled checksum
// per payload piece, which a partly-superseded record could yet verify by.
func (ks *kindState) verifyRemainders(rd *reader, identity core.JournalID, scs []segmentScan) error {
	for i := range scs {
		sc := &scs[i]
		for _, rec := range sc.Records {
			sup, named := ks.superseded[rec.Numbers.Seq]
			if !named || (rec.Kind != index.RecWrite && rec.Kind != index.RecFill) {
				continue
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
// finished: the extent and the version it cuts over come from the loss
// record's own header — the version of the record the journal dropped — and
// the cut drops every record below it inside its extent. A superseded one
// cuts with no unsettled marker, as its own rule decides; every other reason
// leaves one, unsettled until the caller settles at or above its version.
// The cuts apply in the decode's read order, and markers insert by version,
// so the order the cuts apply in decides nothing the rebuilt state shows.
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
		var err error
		if lo.Ev.Reason == core.LossSuperseded {
			_, err = ix.LoseSuperseded(lo.Ev.File, lo.Ev.Extent, lo.Ev.Version)
		} else {
			_, err = ix.Lose(lo.Ev.File, lo.Ev.Extent, lo.Ev.Version)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// subtractCopies is the bad-copy half of the copies rule, run once the tail's
// classing has judged every drop: a corrupt drop an equal-sequence record
// another scan holds — in a segment whose fate the open keeps, neither torn
// nor ended — is removed from Corrupt, for the live copy serves what the drop
// guarded and the loss record a later open would append for it would drop a
// record a twin contradicts. Damaged is not touched: the classing's verdict
// on the segment stands whatever serves in its place, and the tail phases cut
// and report by it.
func (ks *kindState) subtractCopies(scans []segmentScan, tl tails) []corruptRecord {
	// kept answers whether the open keeps a segment — the fates neither
	// torn-tail nor ended. A segment with no fate the classing left keeps:
	// only the classing names an ending, and nothing else ends one.
	fates := make(map[core.SegmentID]fateKind, len(tl.Fates))
	for _, f := range tl.Fates {
		fates[f.ID] = f.Kind
	}
	kept := func(id core.SegmentID) bool {
		k, judged := fates[id]
		return !judged || (k != fateTornTail && k != fateEnded)
	}
	// live is every seq a kept-fate segment holds a record of, in whichever
	// segment: the one membership test below, not one per drop.
	var live map[core.Seq][]core.SegmentID
	for i := range scans {
		if !kept(scans[i].ID) {
			continue
		}
		for _, rec := range scans[i].Records {
			if live == nil {
				live = map[core.Seq][]core.SegmentID{}
			}
			live[rec.Numbers.Seq] = append(live[rec.Numbers.Seq], scans[i].ID)
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
// (segment, offset); where that keeps a copy in a segment the open ends or
// saw fail while another fed copy sits in a segment the open keeps, the piece
// moves to the greatest kept copy — a read then names a segment the open
// holds open. Copies the repoint leaves note like today: the collapse's word
// stands.
func (ks *kindState) repointCopies(ix *index.Index, tl tails) {
	if len(ks.copies) == 0 {
		return
	}
	fates := make(map[core.SegmentID]fateKind, len(tl.Fates))
	for _, f := range tl.Fates {
		fates[f.ID] = f.Kind
	}
	kept := func(id core.SegmentID) bool {
		k, judged := fates[id]
		return !judged || (k != fateTornTail && k != fateEnded)
	}
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
		if kept(cps[keep].Segment) {
			continue // the collapse's word stands: nothing to move
		}
		// The greatest copy among the kept segments' own.
		best := -1
		for i, at := range cps {
			if !kept(at.Segment) {
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
// over the records that actually sat on the segments the open scanned.
type feedFilter struct {
	ks *kindState
	// losses indexes, per named seq, the collected losses naming it;
	// forgets, per tag, the collected forgets of the tag; drops, per failed
	// seq, the collected read-only note of the seq.
	losses  map[core.Seq][]int
	forgets map[core.ShareTag][]int
	drops   map[core.Seq]int
}

// newFeedFilter makes the filter's lookups over the collected losses,
// forgets and read-only drops, in collection order.
func (ks *kindState) newFeedFilter() *feedFilter {
	f := &feedFilter{ks: ks,
		losses:  make(map[core.Seq][]int, len(ks.Losses)),
		forgets: make(map[core.ShareTag][]int, len(ks.Forgets)),
		drops:   make(map[core.Seq]int, len(ks.ReadOnlyDrops)),
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
//   - a synced superseded loss naming the record clips the runs of the
//     record's own extent outside the union of the loss extents, and feeds
//     the clipped runs under the record's own numbers;
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
func (f *feedFilter) feed(rec index.Record, do func(index.Record) error) error {
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

	withheld := ks.failed[seq] || ks.whole[seq] ||
		ks.confined[index.RecordAt{Segment: rec.Segment, Offset: rec.At}]
	for _, i := range f.forgets[rec.Tag] {
		if seq < ks.Forgets[i].Seq {
			withheld = true
		}
	}
	if !withheld {
		if sup, named := ks.superseded[seq]; named &&
			(rec.Kind == index.RecWrite || rec.Kind == index.RecFill) {
			kept := keptRuns(core.Extent{Off: rec.Off, Len: rec.Len}, sup.extents)
			if len(kept) > 0 {
				for _, run := range kept {
					rr := rec
					rr.Off, rr.Len = run.Off, run.Len
					if err := do(rr); err != nil {
						return err
					}
				}
				f.noteCopy(rec)
				return nil
			}
			withheld = true // the loss extents cover the record's whole extent
		}
	}
	if withheld {
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
	// decision: the clip a superseded loss record carries applies to the
	// content records it names — its write and fill alone. A removal it names
	// feeds whole: the loss's own cut is a content cut below the loss's
	// version, while a removal's marker runs from its offset to the end of
	// the file, so clipping the removal to the loss's extent would leave the
	// bytes past the extent held by the removal's unclipped half. Overturned
	// by a loss reason that ever names removal records.
	if err := do(rec); err != nil {
		return err
	}
	f.noteCopy(rec)
	return nil
}

// noteCopy joins where rec sits to the copies of its seq, one entry per fed
// record: a record clipped into runs is one copy, and the runs share where
// it sits.
func (f *feedFilter) noteCopy(rec index.Record) {
	k := copyKey{file: rec.File, seq: rec.Numbers.Seq}
	at := index.RecordAt{Segment: rec.Segment, Offset: rec.At}
	if !slices.Contains(f.ks.copies[k], at) {
		f.ks.copies[k] = append(f.ks.copies[k], at)
	}
}
