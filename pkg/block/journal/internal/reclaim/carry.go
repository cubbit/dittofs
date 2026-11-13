package reclaim

import (
	"bytes"
	"cmp"
	"context"
	"fmt"
	"slices"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/capacity"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/format"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/index"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/segment"
)

// CarriedRec is one header-only record a carry placed: its registration,
// whose Seg and Loc name the copy's own place, beside the index place of the
// copy the pass's later repoint and retirement steps read. The Rec the
// copy registered keeps every field the source carried — the kind, file,
// extent, pair, sequence number and tag, the loss record's named number and
// flags — only Seg and Loc moving to the placement.
type CarriedRec struct {
	Rec Rec
	To  index.RecordAt
}

// CarryResult is what one Carry placed. A carry appends and registers each
// placed record before it moves to the next, so a unit that abandons midway
// answers the records it had already placed; the pass's later steps own
// them, as they own the copies its content half appended. The deferred
// losses it wrote the records of name no end: the deferral ends only once
// the record is durable, which the caller that waits on the location owns.
type CarryResult struct {
	Carried  []CarriedRec
	Deferred []DeferredCopy
}

// DeferredLoss is one loss whose record a deferral owes: the registry entry
// the layer that keeps the deferred losses hands the pass, mapped field by
// field — the dropped record's owner, place and numbers, why it was dropped,
// and the flags its own record would carry. Declared here, core types only,
// because the pass never imports the layer that keeps the registry; the
// wiring the pass gets maps the entry into this.
type DeferredLoss struct {
	Share     core.ShareTag
	File      core.FileID
	Extent    core.Extent
	Version   core.Version
	Seq       core.Seq
	Reason    core.LossReason
	Offloaded bool
	Synced    bool
}

// DeferredCopy is one deferred loss the carry wrote the record of: the
// registry entry it built the record from, and the place the record landed,
// the location whose durability ends the deferral.
type DeferredCopy struct {
	Loss DeferredLoss
	Loc  segment.Location
}

// Carry places a fresh copy of every header-only record registered live in
// seg, the claimed source segment whose content the caller already copied.
// res is the same unit's Copy result: its copies are the pass's own, on
// disk already, so the drop decision below must count them — a copy the
// pass carries content forward under carries an old version below the
// removals that could outrank it, and a decision that reads the ranges
// without the copies' notes would drop those removals as dead weight.
//
// Before the walk, each copy in res.Copies is noted through the index over
// the extent it covers with the pair its record carries — a record can
// never sit on disk unnoted, and the copies are on disk — and the drop
// decision runs once over their files beside the source's: the re-test
// releases each registered record the index now answers droppable, which
// is the drops the segment gets; the walk enumerates what the re-test left,
// so a record it released is carried nowhere and a record it kept is —
// except a release the copies overturn, which the prologue's re-learn
// takes back before the walk. A note that fails abandons the unit before
// anything is read: an unnoted copy on disk is the worse failure, so the
// error answers whole. The walk below still re-asks each record it visits
// under its file's domain, a narrowing race guard against what moved
// between the re-test and the visit, so the two decisions cannot disagree.
//
// The walk visits one file at a time under the file's append domain, in
// (file, sequence) order; a forget record names no file, so it takes no
// domain and appends on the zero file's deterministic stream. A file whose
// tag the forgetting mark answers is skipped whole, exactly as the content
// half skips it; a record the drop test answers droppable under the domain
// — a settle racing the walk may have made it dead weight — is re-tested
// and skipped. Any read, decode or verify failure abandons the unit: the
// error answers with nothing appended after it and nothing dropped, so a
// rotten header-only record is never silently left behind while the pass
// retires its source segment.
func (c *Copier) Carry(ctx context.Context, seg core.SegmentID, res Result) (CarryResult, error) {
	if err := ctx.Err(); err != nil {
		return CarryResult{}, err
	}
	// The pass's own copies, counted before the decision: every placement
	// piece of a split copy is its own entry in res.Copies, so each piece
	// is noted where it landed.
	files := make([]core.FileID, 0, len(res.Copies))
	for _, cp := range res.Copies {
		if err := c.ix.NotePlaced(cp.Tag, cp.File, cp.Loc.Segment, cp.Extent, index.Numbers{Version: cp.Version, Seq: cp.Seq}); err != nil {
			return CarryResult{}, fmt.Errorf("reclaim: carrying segment %d: noting the copy of file %v seq %d the unit itself appended: %w", seg, cp.File, cp.Seq, err)
		}
		// A file only the pass's copies touch holds no registered record of
		// this segment, so the retirement must relay its gone step by the
		// pass's own files list, or its drop ranges outlive the retiring
		// segment.
		files = append(files, cp.File)
	}
	// The re-test's files: beside the source's registered records, the
	// copies' files — the copies can cover a file the source's own
	// header-only records never named.
	for _, rec := range c.tracker.registeredIn(seg) {
		files = append(files, rec.File)
	}
	c.tracker.Retest(files...)
	// A release a re-test spent — this one or an earlier event's — is
	// recoverable while the record's bytes stay in this segment, and the
	// copies just noted are what overturns it: the copy the pass placed
	// under a settled removal outranks it from another segment, so the
	// re-learn registers the record again and the walk reaches it through
	// the registry it re-entered. A record the copies do not overturn
	// stays released: dead weight, dropping with the segment.
	c.tracker.relearnReleased(seg)
	// The walk enumerates what the re-test and the re-learn left registered:
	// each record released without recovery is the drop this segment gets,
	// carried nowhere.
	recs := c.tracker.registeredIn(seg)
	for _, rec := range recs {
		if rec.Loc == (segment.Location{}) {
			// A registered record with no place can be neither read nor
			// moved: the registry's word that a segment holds a record the
			// carry cannot reach, a bug in whoever registered it, not a
			// record to skip past.
			return CarryResult{}, fmt.Errorf("reclaim: carrying segment %d: the record of file %v seq %d is registered without its place, want the placement its append reported", seg, rec.File, rec.Seq)
		}
	}
	var out CarryResult
	for i := 0; i < len(recs); {
		j := i + 1
		for j < len(recs) && recs[j].File == recs[i].File {
			j++
		}
		if err := ctx.Err(); err != nil {
			return out, err
		}
		var err error
		if recs[i].File == (core.FileID{}) {
			err = c.carryForgetRecs(ctx, recs[i:j], &out)
		} else {
			unlock := c.dom.Lock(recs[i].File)
			err = c.carryFile(ctx, recs[i].File, recs[i:j], &out)
			unlock()
		}
		if err != nil {
			return out, err
		}
		i = j
	}
	// Every deferred loss the segment owes, beside the walk's records: each
	// becomes the loss record its deferral promised, written the way the
	// walk's copies are. An admission or append error abandons the unit like
	// the walk's own aborts, the records already placed answering for
	// themselves.
	//
	// The read is a snapshot: a deferral raised in the gap after it stays
	// uncounted here, so the retirement path must re-check Held after it,
	// which counts what the gap raised.
	if c.deferred != nil {
		for _, entry := range c.deferred(seg) {
			if err := ctx.Err(); err != nil {
				return out, err
			}
			if err := c.carryDeferredLoss(ctx, seg, entry, &out); err != nil {
				return out, err
			}
		}
	}
	return out, nil
}

// carryDeferredLoss writes the record of one deferred loss the walk's
// segment holds, under the entry's file append domain on the file's stream:
// the admission every appending call asks runs first, the repack copy — the
// one class the headroom cannot refuse — and its draw pairs with drawn once
// the record appended; a fresh sequence number follows, since a deferred
// loss record never had one; then the record itself, built from the entry,
// under a repack roll. The finished placement is noted through the index
// and registered live like every record the walk places, and the result
// answers it for the caller that waits out the location's durability to end
// the deferral. This ends nothing: EndDeferral, and the SetDeferred that
// lowers the source with it, are the durable-waiting caller's.
func (c *Copier) carryDeferredLoss(ctx context.Context, seg core.SegmentID, entry DeferredLoss, out *CarryResult) error {
	if c.admit == nil {
		return fmt.Errorf("reclaim: carrying segment %d: a deferred loss of file %v seq %d arrived with no admission wired", seg, entry.File, entry.Seq)
	}
	unlock := c.dom.Lock(entry.File)
	defer unlock()
	if c.forgetting != nil && c.forgetting(entry.Share) {
		// The walk skips a forgotten tag's file whole, and the deferral's
		// record answers the same skip: a record written now would carry a
		// fresh sequence number above the forget record's, outside its
		// snapshot, and would replay a loss for content the forget drops.
		// Nothing is appended or registered for it — the tag's drop subsumes
		// its loss — and the deferral stays standing in the registry, for
		// the forget's own path to clear as it clears the tag.
		return nil
	}
	// No failure from here owes a Refund: a refusal moved no headroom
	// counter, and the RepackCopy admission this draw took drew nothing on
	// the headroom ledger, so a failed append refunds nothing.
	draw, err := c.admit(capacity.RepackCopy)
	if err != nil {
		return fmt.Errorf("reclaim: carrying segment %d: admitting the deferred loss of file %v seq %d on the repack reserve: %w", seg, entry.File, entry.Seq, err)
	}
	seq, err := c.counters.NextSeq()
	if err != nil {
		return fmt.Errorf("reclaim: carrying segment %d: numbering the deferred loss of file %v seq %d: %w", seg, entry.File, entry.Seq, err)
	}
	h := format.RecordHeader{
		Kind:    format.KindLoss,
		File:    entry.File,
		Extent:  entry.Extent,
		Seq:     seq,
		Version: entry.Version,
		Tag:     entry.Share,
	}
	body := format.RecordBody{Loss: format.Loss{
		Dropped:   entry.Seq,
		Synced:    entry.Synced,
		Offloaded: entry.Offloaded,
		Reason:    lossReasonOf(entry.Reason),
	}}
	// ponytail: in a copy a loss record takes its real bytes, 184 — a
	// 96-byte header, a 16-byte payload and a 72-byte footer entry — against
	// at least 168 for the header-only record it replaces, so a segment of
	// nothing but dropped header-only records needs a tenth more than one
	// segment's cap of repack reserve. Upgrade to sizing the reserve at
	// 11/10 of a cap when a deferred loss is seen in the field.
	pl, err := c.streams.For(entry.File).AppendRoll(h, body, segment.Roll{Repack: true})
	if err != nil {
		return fmt.Errorf("reclaim: carrying segment %d: placing the deferred loss record of seq %d: %w", seg, entry.Seq, err)
	}
	if c.drawn != nil {
		c.drawn(draw)
	}
	if err := c.ix.NotePlaced(entry.Share, entry.File, pl.Loc.Segment, entry.Extent, index.Numbers{Version: entry.Version, Seq: seq}); err != nil {
		return fmt.Errorf("reclaim: carrying segment %d: noting the deferred loss record of seq %d: %w", seg, entry.Seq, err)
	}
	// Superseded carries the entry's reason: a deferred loss can be one a
	// superseding record outranked, and that copy answers the rule that
	// leaves no marker to settle. An unsynced copy registers without the
	// original's presence marks — the marks are keyed by the original's
	// Rec — so its drop test runs without the named-record holder set.
	//
	// decision: unsynced, the copy's drop test bounded by the MayOutrank
	// outranking guard alone, which keeps it while any segment holding the
	// named record still has its ranges noted. Overturned by a holder whose
	// ranges the index no longer knows — a retired-but-unrelayed segment
	// would be one.
	c.tracker.Register(Rec{
		Kind: format.KindLoss, File: entry.File, Seg: pl.Loc.Segment, Ext: entry.Extent,
		Pair: index.Numbers{Version: entry.Version, Seq: seq},
		Seq:  seq, Named: entry.Seq, Tag: entry.Share, Synced: entry.Synced,
		Superseded: entry.Reason == core.LossSuperseded,
		Loc:        pl.Loc,
	})
	out.Deferred = append(out.Deferred, DeferredCopy{Loss: entry, Loc: pl.Loc})
	return nil
}

// carryFile carries one file's records under the file's append domain, which
// the caller holds.
func (c *Copier) carryFile(ctx context.Context, file core.FileID, recs []Rec, out *CarryResult) error {
	tag, ok := c.ix.Owner(file)
	if !ok {
		// The forget path's drop clears the file's tag under this same
		// domain, so a Forget racing the pass reads here as the tag cleared:
		// the file is skipped, its records left to the drop. An error
		// would fail the unit over a race the forget owns.
		return nil
	}
	if c.forgetting != nil && c.forgetting(tag) {
		// The whole file is abandoned, exactly as the content half abandons
		// it: nothing of it is appended or registered, since the tag's drop
		// is the forget's own.
		return nil
	}
	for _, rec := range recs {
		if c.tracker.Droppable(rec) {
			// A settle or a retirement racing the walk may have made the
			// record dead weight: the re-test takes the registration whose
			// drop already stands, and nothing is carried for it.
			c.tracker.Retest(file)
			continue
		}
		cr, err := c.carryRecord(ctx, rec)
		if err != nil {
			return err
		}
		out.Carried = append(out.Carried, cr)
	}
	return nil
}

// carryForgetRecs carries the forget records of the walk, which name no
// file: no append domain exists to hold, so their copies are appended on the
// zero file's deterministic stream and registered with the snapshot the
// source carried — the forget's liveness rule moves with the record, since
// the original's own registration will go when the source segment retires.
// The tag a forget record names is the tag it ends, so the forgetting mark
// that answers true for it abandons its copy exactly as a file's whole skip
// does.
func (c *Copier) carryForgetRecs(ctx context.Context, recs []Rec, out *CarryResult) error {
	for _, rec := range recs {
		if c.forgetting != nil && c.forgetting(rec.Tag) {
			continue
		}
		if c.tracker.Droppable(rec) {
			c.tracker.Retest(rec.File)
			continue
		}
		cr, err := c.carryRecord(ctx, rec)
		if err != nil {
			return err
		}
		out.Carried = append(out.Carried, cr)
	}
	return nil
}

// carryRecord places one fresh copy of rec, wherever the caller's walk
// stands: it reads the record whole at rec's place through the same
// unread-set read a content source takes — so the verify, the unread
// marking and the I/O shape are the content half's own — decodes and
// verifies it with the journal's codec, appends the same header and body
// under a repack roll, registers the copy at its placement, and notes the
// copy through the index so it is never on disk unnoted.
func (c *Copier) carryRecord(ctx context.Context, rec Rec) (CarriedRec, error) {
	at := index.RecordAt{Segment: rec.Loc.Segment, Offset: uint32(rec.Loc.Off)}
	identity := c.streams.For(rec.File).Identity()
	b, err := c.readSource(ctx, rec.Loc.Segment, rec.File, at)
	if err != nil {
		// An I/O failure abandons the unit as a content source's does: Try
		// marked the place unread, and nothing was appended or dropped.
		return CarriedRec{}, fmt.Errorf("reclaim: carrying the record of seq %d: %w", rec.Seq, err)
	}
	h, derr := format.DecodeRecordHeader(b[:format.RecordHeaderSize], identity, rec.Loc.Segment, rec.Loc.Off)
	if derr == nil {
		// The read returned the header and the body its decode sized whole,
		// so the slice below is exactly the record's body.
		var body format.RecordBody
		body, derr = format.VerifyRecordBody(b[format.RecordHeaderSize:], identity, h)
		if derr == nil {
			return c.placeCarried(rec, h, body)
		}
	}
	// decision: a header-only record that does not decode or verify never
	// reaches the corruption handler — it backs no held piece, so a corrupt
	// report would count damage it cannot and would strand the marker such
	// an answer would leave. The unit is abandoned instead: an error
	// answers, nothing further is appended and nothing dropped, so a rotten
	// removal record is never left silently behind while its source segment
	// is retired over it. Overturned by a ruled loss record for a rotten
	// removal, which this path would then write in place of the error.
	return CarriedRec{}, fmt.Errorf("reclaim: carrying segment %d: the record of file %v seq %d does not decode or verify: %w", rec.Loc.Segment, rec.File, rec.Seq, derr)
}

// placeCarried appends the verified record under a repack roll and finishes
// the placement: the copy registers live at the place it landed — a forget
// record with the snapshot its source carried, cloned so the registered
// copy owns its own — and the copy's own segment is noted through the
// index over the extent the record covers. A forget record names no file,
// so there is no index entry to note: its placement is the tracker's alone.
func (c *Copier) placeCarried(rec Rec, h format.RecordHeader, body format.RecordBody) (CarriedRec, error) {
	// The same header as the source's, kind, file, extent, sequence number,
	// version and tag unchanged; the stream fills the segment, the offset
	// and the synced-through of the place it lands.
	nh := format.RecordHeader{
		Kind:    h.Kind,
		File:    h.File,
		Extent:  h.Extent,
		Seq:     h.Seq,
		Version: h.Version,
		Tag:     h.Tag,
	}
	pl, err := c.streams.For(rec.File).AppendRoll(nh, body, segment.Roll{Repack: true})
	if err != nil {
		return CarriedRec{}, fmt.Errorf("reclaim: placing the carried record of seq %d: %w", rec.Seq, err)
	}
	cp := rec
	cp.Seg = pl.Loc.Segment
	cp.Loc = pl.Loc
	if rec.Kind == format.KindForget {
		c.tracker.RegisterForget(cp, c.tracker.forgetSnapshotOf(rec))
	} else {
		c.tracker.Register(cp)
	}
	if rec.File != (core.FileID{}) {
		if err := c.ix.NotePlaced(rec.Tag, rec.File, pl.Loc.Segment, covered(rec), rec.Pair); err != nil {
			return CarriedRec{}, fmt.Errorf("reclaim: noting the carried record of seq %d: %w", rec.Seq, err)
		}
	}
	return CarriedRec{Rec: cp, To: index.RecordAt{Segment: pl.Loc.Segment, Offset: uint32(pl.Loc.Off)}}, nil
}

// relearnReleased re-asks, for the pass about to carry seg, every
// registration a re-test's release spent in seg, with the pass's copies now
// noted through the index: a record that still answers droppable stays
// released — its release was sound, and nothing the pass did overturns it,
// so it drops with the segment as the dead weight it is — while one the
// copies now keep is registered again, once per registration the release
// spent, through the same register path the original took so the counts
// move, and the walk below reaches it through the registry it re-entered.
// The ask runs under rm against the copies' notes, and the registrations
// run after it, outside the lock, so no registering path is re-entered
// under rm.
func (t *Tracker) relearnReleased(seg core.SegmentID) {
	t.rm.Lock()
	rel := t.released[seg]
	if len(rel) == 0 {
		t.rm.Unlock()
		return
	}
	var again []Rec
	for rec, n := range rel {
		if t.droppableLocked(rec) {
			continue
		}
		delete(rel, rec)
		for range n {
			again = append(again, rec)
		}
	}
	if len(rel) == 0 {
		delete(t.released, seg)
	}
	t.rm.Unlock()
	// A record re-released after this point re-enters the released set by
	// count, so the second pass re-learns it exactly the same way.
	for _, rec := range again {
		t.Register(rec)
	}
}

// registeredIn returns the distinct records registered live in seg, sorted
// by (file, sequence) so a unit's walk is deterministic whatever the
// registry's map order is: one Rec per distinct record, whatever live
// registrations count it — the copy a carry places registers one more, and
// the next walk finds the record once either way.
//
// ponytail: registeredIn scans every registered record of every file under
// rm, twice per unit per segment — once before the re-test for its file
// list, once after for the walk. Upgrade to a per-segment files index in
// the segment state when a pass over a many-file journal shows the scan
// in a profile.
func (t *Tracker) registeredIn(seg core.SegmentID) []Rec {
	t.rm.Lock()
	var out []Rec
	for _, rs := range t.recs {
		for rec := range rs {
			if rec.Seg == seg {
				out = append(out, rec)
			}
		}
	}
	t.rm.Unlock()
	slices.SortFunc(out, func(a, b Rec) int {
		aI, bI := a.File.Bytes(), b.File.Bytes()
		if n := bytes.Compare(aI[:], bI[:]); n != 0 {
			return n
		}
		return cmp.Compare(a.Seq, b.Seq)
	})
	return out
}

// forgetSnapshotOf returns a clone of the forget record rec's snapshot: the
// segments still holding records of its tag below it, the rule its copy's
// registration re-enters. A clone, so the registered copy owns its own
// snapshot; nil when the record carries none.
func (t *Tracker) forgetSnapshotOf(rec Rec) []core.SegmentID {
	t.rm.Lock()
	defer t.rm.Unlock()
	snap := t.forgets[rec]
	if len(snap) == 0 {
		return nil
	}
	out := make([]core.SegmentID, 0, len(snap))
	for s := range snap {
		out = append(out, s)
	}
	return out
}

// lossReasonOf maps a reason to the number its loss record carries. It
// panics on a reason that names none, which is a wiring bug: the layer whose
// registry the entry came from refused one at Lose already.
func lossReasonOf(r core.LossReason) format.LossReason {
	switch r {
	case core.LossCorrupt:
		return format.LossCorrupt
	case core.LossStale:
		return format.LossStale
	case core.LossFailedWindow:
		return format.LossFailedSync
	case core.LossSuperseded:
		return format.LossSuperseded
	case core.LossAcknowledged:
		return format.LossAcknowledged
	}
	panic(fmt.Sprintf("reclaim: loss reason %d names no reason", r))
}
