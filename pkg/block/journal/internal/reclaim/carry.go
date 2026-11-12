package reclaim

import (
	"bytes"
	"cmp"
	"context"
	"fmt"
	"slices"

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
// them, as they own the copies its content half appended.
type CarryResult struct {
	Carried []CarriedRec
}

// Carry places a fresh copy of every header-only record registered live in
// seg, the claimed source segment whose content the caller already copied:
// each record is read whole at its place through the same unread-set read
// the content half uses, decoded and verified with the journal's own codec,
// and re-appended under a repack roll with its sequence number and version
// unchanged — the pair is the record's identity, and a copy reissued under a
// fresh number would outrank the records the source once covered.
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
	return out, nil
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

// registeredIn returns the distinct records registered live in seg, sorted
// by (file, sequence) so a unit's walk is deterministic whatever the
// registry's map order is: one Rec per distinct record, whatever live
// registrations count it — the copy a carry places registers one more, and
// the next walk finds the record once either way.
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
