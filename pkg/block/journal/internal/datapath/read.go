package datapath

import (
	"cmp"
	"context"
	"fmt"
	"math"
	"slices"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/durability"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/format"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/index"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/segment"
)

// ReadAt fills p with the bytes of file id at off that the journal holds. It
// returns the count of bytes it copied into p in n, the extents of
// (off, len(p)) it holds nothing for in missing, the content version of each
// piece it served in held, and the file's change version in asOf. missing and
// held are in offset order, and missing is exact: it names the bytes not
// served, never widened to a record, and the bytes of p under it are left
// untouched, never zero-filled.
//
// A cancelled ctx is refused first and changes nothing; a handle whose close
// began is refused next, with core.ErrClosed. A negative off, or an off whose
// end overflows, is refused with core.ErrInvalidExtent. A zero-length p serves
// nothing, reports nothing missing and does no resolve.
//
// asOf is read with the file's append domain held, and the span resolved after
// it is released (see readAsOf). Each distinct record the span names is read
// once, whole, with no lock held, and no byte of it is copied into p until its
// header decoded at its own place, matched every index entry that named it,
// and its whole payload verified. A record that fails a check is handed to
// Corrupt: its pieces are served from the copy Corrupt names, or reported
// missing when Corrupt dropped them. A record whose segment was retired
// between the resolve and the read is resolved again once, and what the index
// then names is served or reported missing.
//
// An I/O error is returned wrapped and changes nothing. So is a segment
// reference refused under descriptor pressure, wrapping
// segment.ErrNoDescriptor, which the caller may retry. So is a refusal of a
// record the index still names whose sequence number nothing trustworthy
// names, which wraps core.ErrCorrupt. On any error n and held, in offset
// order, still describe what was copied into p before it, and missing is nil.
func (c *Core) ReadAt(ctx context.Context, s *Share, id core.FileID, off int64, p []byte) (n int, missing []core.Extent, held []core.Held, asOf core.Version, err error) {
	if err = ctx.Err(); err != nil {
		return 0, nil, nil, core.Version{}, err
	}
	exit, err := s.enter()
	if err != nil {
		return 0, nil, nil, core.Version{}, err
	}
	defer exit()
	// The offset is checked before the zero-length shortcut, unlike WriteAt's:
	// an empty read still answers with asOf, so it is refused for an offset no
	// read may name rather than answered.
	if off < 0 || off > math.MaxInt64-int64(len(p)) {
		return 0, nil, nil, core.Version{}, core.ErrInvalidExtent
	}

	asOf = c.readAsOf(id)
	if len(p) == 0 {
		return 0, nil, nil, asOf, nil
	}
	if c.beforeResolve != nil {
		c.beforeResolve()
	}
	placed, gaps := c.index.Span(id, core.Extent{Off: off, Len: int64(len(p))})
	if c.beforeRead != nil {
		c.beforeRead()
	}

	r := reading{c: c, ctx: ctx, id: id, st: c.streams.For(id), off: off, p: p, missing: gaps}
	err = r.serve(placed, true)
	slices.SortFunc(r.held, func(a, b core.Held) int { return cmp.Compare(a.Extent.Off, b.Extent.Off) })
	if err != nil {
		return r.n, nil, r.held, asOf, err
	}
	return r.n, coalesce(r.missing), r.held, asOf, nil
}

// readAsOf reads the version a read of id reports with the file's append
// domain held, and releases the domain before it returns.
//
// decision: asOf is read under the append domain so that it never names a
// change the read did not see: a write publishes under that domain, so the
// version read here is either wholly before a write's publish or wholly after
// it, and the Span that follows resolves at least everything asOf names. A
// write that lands between the release and the Span is resolved but reported
// under the older asOf, which is safe because asOf only has to be a lower
// bound. The domain is held only for this read, so a read never waits on a
// record write. Overturned by an index that returns the version together
// with the span under one file lock.
func (c *Core) readAsOf(id core.FileID) core.Version {
	unlock := c.domains.Lock(id)
	defer unlock()
	return c.index.AsOf(id)
}

// reading is one ReadAt's progress: what it copied into p and what it found
// missing so far. It holds no lock.
type reading struct {
	c   *Core
	ctx context.Context
	id  core.FileID
	st  *segment.Stream
	off int64
	p   []byte

	n       int
	held    []core.Held
	missing []core.Extent
}

// recordPieces is the pieces of a span that one record backs.
type recordPieces struct {
	at     index.RecordAt
	pieces []index.Placed
}

// serve serves placed, reading each distinct record once. again allows one
// more resolve of the pieces of a record whose segment is gone.
func (r *reading) serve(placed []index.Placed, again bool) error {
	for _, g := range byRecord(placed) {
		if err := r.serveRecord(g, again); err != nil {
			return err
		}
	}
	return nil
}

// serveRecord reads g's record, verifies it and copies its pieces into p, or
// hands it to Corrupt and serves what Corrupt answers.
func (r *reading) serveRecord(g recordPieces, again bool) error {
	payload, h, verdict, err := r.c.readRecord(r.ctx, r.st, r.id, g.at, g.pieces)
	switch {
	case segmentGone(err):
		return r.resolveAgain(g, again)
	case err != nil:
		return err
	case verdict == recordVerifies:
		r.copyOut(payload, g.pieces)
		return nil
	}

	cr, known := r.corruption(g, h, verdict)
	if !known {
		// A record the index no longer names was dropped or repointed by
		// another read's Corrupt since this read resolved it: what the index
		// names now is resolved instead of refusing a record nothing serves.
		if !r.c.index.Names(r.id, g.at) {
			return r.resolveAgain(g, again)
		}
		return fmt.Errorf("datapath: the record of file %v at segment %d offset %d does not verify and its sequence number is unknown: %w", r.id, g.at.Segment, g.at.Offset, core.ErrCorrupt)
	}
	// readRecord released its segment reference before it returned,
	// and the read holds no domain: Corrupt takes the file's append domain
	// itself.
	out, err := r.c.dur.Corrupt(r.ctx, cr)
	if err != nil {
		return err
	}
	if r.c.afterCorrupt != nil {
		r.c.afterCorrupt()
	}
	if out.Kind != durability.CorruptUseCopy {
		r.miss(g.pieces)
		return nil
	}

	// decision: a copy Corrupt verified is read again here and checked like
	// any record, and one that no longer verifies, or whose segment is gone,
	// is reported missing with no second Corrupt. Corrupt has repointed the
	// index to the copy, or serves it unrepointed while its segment's claim is
	// owed, so a later read finds the copy bad and hands it to Corrupt then;
	// this read only never serves a byte it did not verify. Overturned if a
	// copy that fails after its repoint must be dropped by the read that found
	// it.
	at := index.RecordAt{Segment: out.Copy.Segment, Offset: uint32(out.Copy.Off)}
	payload, _, verdict, err = r.c.readRecord(r.ctx, r.st, r.id, at, g.pieces)
	switch {
	case segmentGone(err):
		r.miss(g.pieces)
	case err != nil:
		return err
	case verdict == recordVerifies:
		r.copyOut(payload, g.pieces)
	default:
		r.miss(g.pieces)
	}
	return nil
}

// corruption names g's record for Corrupt, and reports false when nothing
// trustworthy names its sequence number. The record's version is the one the
// index entries carry. Its sequence number, extent and share come from the
// stream's ledger while it still holds the record, which is what the writer
// reported for that place and no damage on disk can change; otherwise from
// the header, when the header decodes and is still this record's: a write or
// a fill of this file at the version the index entries carry, disagreeing
// with them, if at all, only in its extent.
//
// decision: a record the ledger no longer holds, and the index still names,
// is refused with an error wrapping core.ErrCorrupt instead of handed to
// Corrupt when its header cannot name its sequence number: a header that does
// not decode or overruns its segment, or one that decodes but is of another
// kind, file or version. Corrupt needs the
// record's own sequence number to find its copy and to name the loss, and the
// index keeps none per record. A header that fails its checksum supplies
// none; one of another kind, file or version supplies the number of the
// record it describes, not of the one the index meant, and handed to Corrupt
// that number can find the other record's copy and repoint this file's
// entries to it, so the record is never dropped and its loss never recorded.
// The read serves nothing of it, reports nothing missing for it and changes
// nothing, so every read of that range fails until an open finds the record.
// A record the index no longer names is resolved again instead: another
// read's Corrupt took it after this read resolved it. Withdraw this when
// the index records a sequence number per record, or when the loss key and
// the loss record accept an unknown one and Corrupt is then called without a
// number.
func (r *reading) corruption(g recordPieces, h format.RecordHeader, verdict recordVerdict) (durability.Corruption, bool) {
	cr := durability.Corruption{
		File:    r.id,
		Record:  g.at,
		Version: g.pieces[0].Version,
	}
	if w, ok := r.c.dur.Tracked(r.id, g.at); ok {
		cr.Share, cr.Seq, cr.Extent = w.Tag, w.Seq, w.Extent
		return cr, true
	}
	ours := verdict != recordHeaderDamaged && (h.Kind == format.KindWrite || h.Kind == format.KindFill) &&
		h.File == r.id && h.Version == cr.Version
	if !ours {
		return durability.Corruption{}, false
	}
	cr.Share, cr.Seq, cr.Extent = h.Tag, h.Seq, h.Extent
	return cr, true
}

// resolveAgain serves g's pieces from what the index names for them now,
// their record's segment having been retired after the read resolved them.
// It resolves once: when again is false the pieces are reported missing, and
// what the one resolve finds gone too is reported missing by serve.
//
// decision: the pieces of a record whose segment is gone are resolved again
// only once, and what that resolve names is read with no further resolve, so
// a read racing repeated retirements reports the extent missing rather than
// looping.
// A segment is retired only once the index names nothing in it, so one
// resolve finds where its pieces went. Overturned if a read must keep
// retrying.
func (r *reading) resolveAgain(g recordPieces, again bool) error {
	if !again {
		r.miss(g.pieces)
		return nil
	}
	var placed []index.Placed
	for _, pc := range g.pieces {
		p, gaps := r.c.index.Span(r.id, pc.Extent)
		placed = append(placed, p...)
		r.missing = append(r.missing, gaps...)
	}
	return r.serve(placed, false)
}

// copyOut copies each piece's bytes of a verified payload into p and records
// the piece as held.
func (r *reading) copyOut(payload []byte, pieces []index.Placed) {
	for _, pc := range pieces {
		dst := r.p[pc.Extent.Off-r.off:][:pc.Extent.Len]
		r.n += copy(dst, payload[pc.Skip:int64(pc.Skip)+pc.Extent.Len])
		r.held = append(r.held, core.Held{Extent: pc.Extent, Version: pc.Version})
	}
}

// miss reports each piece's extent missing.
func (r *reading) miss(pieces []index.Placed) {
	for _, pc := range pieces {
		r.missing = append(r.missing, pc.Extent)
	}
}

// byRecord groups placed by the record each piece names, in the order each
// record first appears.
func byRecord(placed []index.Placed) []recordPieces {
	var groups []recordPieces
	at := map[index.RecordAt]int{}
	for _, pc := range placed {
		i, ok := at[pc.Record]
		if !ok {
			i = len(groups)
			at[pc.Record] = i
			groups = append(groups, recordPieces{at: pc.Record})
		}
		groups[i].pieces = append(groups[i].pieces, pc)
	}
	return groups
}

// coalesce sorts missing by offset and joins extents that touch, so a gap the
// index never held and a neighbouring extent a dropped record held are
// reported as one extent, as they would be had neither been held.
func coalesce(missing []core.Extent) []core.Extent {
	slices.SortFunc(missing, func(a, b core.Extent) int { return cmp.Compare(a.Off, b.Off) })
	out := missing[:0]
	for _, m := range missing {
		if k := len(out) - 1; k >= 0 && out[k].Off+out[k].Len == m.Off {
			out[k].Len += m.Len
			continue
		}
		out = append(out, m)
	}
	return out
}
