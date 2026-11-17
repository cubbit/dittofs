package recovery

import (
	"context"
	"fmt"
	"io"
	"slices"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/format"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/index"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/segment"
)

// copyInput is one whole record handed to the copier: the bytes a verified
// read kept, header and body both — the padding the encoder writes itself —
// and the placement the bytes verified at, with the record's own two numbers
// carried for the caller that moves what the record backs to the copy. One
// input is one record, never a merge of them: the copy keeps every record's
// extent and numbers its own.
type copyInput struct {
	Bytes   []byte         // the whole verified record, header and body
	Segment core.SegmentID // the source segment the bytes verified in
	At      int64          // the record's own offset in its segment
	Seq     core.Seq
	Version core.Version
}

// copier writes whole records, keeping their numbers, into a spare drawn from
// the repack reserve — never a share's limit, never a content or headroom
// spare, which the classes never lend — and seals the target.
//
// decision: the tail's re-appends and the emptying's copies share this one
// copier, whose target is sealed, so a second crash never reads a copy back
// as an unproven tail — the marker proves the copy's sync to the next open.
// Withdrawn if J6 shows those seals at open as a cost that outweighs the
// second crash's proof.
type copier struct {
	deps     Deps
	identity core.JournalID

	// reserve holds the ids of the spares the spares phase adopted into the
	// repack reserve, ascending, drawn front-first: the one class a
	// re-appending open may take from, the running journal's repack rolls'
	// own.
	reserve []core.SegmentID

	// taken holds every spare the copier drew — the target the records
	// landed in and every failed one before it — so the phase answers the
	// adoptions no later pool wiring may receive again.
	taken []core.SegmentID
}

// copy writes recs into one spare drawn from the repack reserve, seals it, and
// returns the target's id. The records land whole and in order, contiguous
// from the first record slot, each re-encoded under the target's segment and
// its own offset there while keeping the sequence number, version, kind, file,
// extent and tag its source header carried. No spare is drawn before every
// record's bytes have verified against its source header, so a caller that
// hands the copier a record it did not read whole draws nothing.
//
// A target whose write or sync fails is a failed segment — ended in the
// ledger the seal writer holds, never sealed, nothing of it proved — and the
// same records go once more into another spare of the reserve. A second
// refusal in a row, or no spare ready and none to be made, returns the
// error — the refusal the failing attempt answered with, where one ran, the
// spare refusal where none did — and leaves the source in place, for the
// caller ends no source before its copy holds.
func (c *copier) copy(ctx context.Context, recs []copyInput) (target core.SegmentID, err error) {
	if len(recs) == 0 {
		return 0, nil
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	whole := make([]format.RecordHeader, 0, len(recs))
	bodies := make([]format.RecordBody, 0, len(recs))
	end := int64(format.SegmentHeaderSize)
	for _, r := range recs {
		if len(r.Bytes) < format.RecordHeaderSize {
			return 0, fmt.Errorf("the record of seq %d at %d of segment %d kept %d bytes, want its %d-byte header at least", r.Seq, r.At, r.Segment, len(r.Bytes), format.RecordHeaderSize)
		}
		h, derr := format.DecodeRecordHeader(r.Bytes[:format.RecordHeaderSize], c.identity, r.Segment, r.At)
		if derr != nil {
			return 0, fmt.Errorf("the record of seq %d at %d of segment %d does not decode as the copier's input: %w", r.Seq, r.At, r.Segment, derr)
		}
		if h.Seq != r.Seq || h.Version != r.Version {
			return 0, fmt.Errorf("the record of seq %d version %v at %d of segment %d names the seq %d version %v it was handed", r.Seq, r.Version, r.At, r.Segment, h.Seq, h.Version)
		}
		// The body is judged only on the bytes the input actually kept: a
		// slice naming bytes past their end would decide on bytes no read
		// returned, so a short input is refused for its own length.
		if int64(len(r.Bytes)) < format.RecordHeaderSize+h.BodySize() {
			return 0, fmt.Errorf("the record of seq %d at %d of segment %d kept %d bytes, want its %d-byte header and the %d-byte body it names", r.Seq, r.At, r.Segment, len(r.Bytes), format.RecordHeaderSize, h.BodySize())
		}
		body, verr := format.VerifyRecordBody(r.Bytes[format.RecordHeaderSize:format.RecordHeaderSize+h.BodySize()], c.identity, h)
		if verr != nil {
			return 0, fmt.Errorf("the record of seq %d at %d of segment %d does not verify as the copier's input: %w", r.Seq, r.At, r.Segment, verr)
		}
		whole = append(whole, h)
		bodies = append(bodies, body)
		end += h.Size()
	}
	// decision: one copy takes one spare, and a batch that cannot fit it
	// refuses the open rather than spilling into a second target, for each
	// refusal the retry answers is per batch and a spill would leave some
	// records copied and others held over a failed spare. The tail's records
	// are at most one sync bound's worth per stream, which a crash's lost
	// tails across every stream together can exceed. Overturned by an open
	// whose re-append set measures past one cap, which then needs the packing
	// loop that seals a full target and draws another.
	need := end + format.SealMarkerSize + int64(len(recs))*format.CatalogEntrySize + format.TrailerSize
	if need > c.deps.SegmentCap {
		return 0, fmt.Errorf("the copy of %d records needing %d bytes cannot fit one spare of the %d-byte cap", len(recs), need, c.deps.SegmentCap)
	}

	var refused error
	for range 2 {
		// Each spare's doorstep: the write this batch is about to start, so
		// an already-landed cancellation answers here and no spare is drawn
		// into a write a caller already ended.
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		id, ok := c.draw()
		if !ok {
			if refused != nil {
				// The refusal in a row is the write's or the sync's own —
				// the device's word for why the open stops — for a spare
				// count names a shortage the failing device already answered
				// for.
				return 0, refused
			}
			return 0, &core.NoSpaceError{Limit: core.LimitSpare, Missing: 1, Unit: core.UnitSegments}
		}
		err := c.writeTarget(id, whole, bodies)
		if err == nil {
			return id, nil
		}
		if isContextErr(err) {
			return 0, err
		}
		refused = err
	}
	return 0, refused
}

// draw takes the oldest spare of the copier's reserve, naming it among the
// taken; the caller keeps the answer's whole.
func (c *copier) draw() (core.SegmentID, bool) {
	if len(c.reserve) == 0 {
		return 0, false
	}
	id := c.reserve[0]
	c.reserve = c.reserve[1:]
	c.taken = append(c.taken, id)
	return id, true
}

// writeTarget writes the encoded records into the spare id, which no stream
// owns, and seals it there: the segment's header then every record
// contiguously from the first record slot in one write, then the seal
// writer's own two syncs carrying first the records to the device and then
// the marker, the footer and the trailer. The seal's marker ends the file
// where its records end, and the full seal puts the trailer in the cap's last
// bytes, so the cap-sized spare needs no truncation. A write or sync that
// fails ends the target in the ledger before the error answers — a failed
// segment, never sealed — for a segment whose bytes the device never took
// whole is one no marker may speak for.
func (c *copier) writeTarget(id core.SegmentID, recs []format.RecordHeader, bodies []format.RecordBody) error {
	// The ledger the seal writer hands its steps: a spare accounted at its
	// cap, taken as the segment it becomes for this one write, and ended by
	// the seal writer itself where a step fails.
	set := segment.NewSet(c.deps.SegmentCap, c.deps.Log)
	if err := set.AdoptSpare(id); err != nil {
		return err
	}
	if err := set.Take(id); err != nil {
		return err
	}

	// The whole batch as one buffer, the shape the packing loop spells: the
	// header naming this journal and the target's own id, no predecessor —
	// the ended sources are never a predecessor a later open would have to
	// answer for — then each record re-encoded at the offset it lands at.
	buf := format.AppendSegmentHeader(nil, format.SegmentHeader{Identity: c.identity, ID: id})
	entries := make([]format.CatalogEntry, 0, len(recs))
	for i := range recs {
		h := recs[i]
		h.Segment, h.Offset = id, int64(len(buf))
		// A copy's header names no point a sync completed in the target, for
		// none had when each record was written: the first sync the target's
		// bytes reach is the seal writer's own first step.
		h.SyncedThrough = 0
		buf = format.AppendRecord(buf, c.identity, h, bodies[i])
		entries = append(entries, format.CatalogEntry{
			File: h.File, Extent: h.Extent, RecordOffset: h.Offset,
			Seq: h.Seq, Version: h.Version, Kind: h.Kind, Tag: h.Tag,
		})
	}
	marker := int64(len(buf))

	cache := segment.NewDescriptors(c.deps.FS, format.SegmentName, core.NewDescriptorBudget(1))
	ref, aerr := cache.Acquire(id)
	if aerr != nil {
		_ = cache.Shutdown() // no file opened: the release only drops the cache's bookkeeping
		return aerr
	}
	err := error(nil)
	if n, werr := ref.WriteAt(buf, 0); werr != nil {
		err = werr
	} else if n != len(buf) {
		err = io.ErrShortWrite
	}
	if err == nil {
		// The seal's own syncs are the copy's proof: nothing of this open
		// reports them further, for no durability layer runs at recovery, and
		// the sealed target itself is what a later open reads whole.
		_, err = segment.WriteSeal(set, ref, c.identity, id, marker, entries, false, func(core.SegmentID, int64) {})
	}
	ref.Release()
	if serr := cache.Shutdown(); err == nil {
		err = serr
	}
	return err
}

// runReappend is the SpanOpenReappend phase of the open: the records the
// tail's proof classed lost by the device, and every record it handed over
// whole off a failed window, are written back by the copier into a spare of
// the repack reserve; the payload records among them are repointed to the
// copy, so the open serves their extents from the bytes just written rather
// than the placement the device lost; and every record's source segment is
// ended, so the cutting phase truncates it without a marker and the emptying
// unlinks it.
//
// The phase draws on the repack reserve alone — the spare the spares phase
// adopted for that class — and consults no share's figures and no journal
// limit, so a journal opened at its maximum still re-appends (R54): the
// reserve is the pool's own byte, and the running journal's refill returns
// it after the open.
//
// The phase keeps the proof's state untouched: st.Proof still names the
// records it handed over and the segments whose sync failed, the answers the
// later phases read. The spares the copier drew leave the adoptions the pool
// would receive, for each is a data segment now — a sealed target, or a
// failed one the next open re-reads as an unsealed segment and re-proves.
func runReappend(ctx context.Context, deps Deps, rd *reader, st *State, expect core.JournalID, _ core.Version, _ []core.SyncMark) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(st.Proof.Reappends) == 0 {
		return nil
	}

	c := &copier{deps: deps, identity: expect, reserve: reserveSpares(st)}
	recs := make([]copyInput, len(st.Proof.Reappends))
	for i, rec := range st.Proof.Reappends {
		recs[i] = copyInput{Bytes: rec.First, Segment: rec.Segment, At: rec.At, Seq: rec.Header.Seq, Version: rec.Header.Version}
	}
	target, err := c.copy(ctx, recs)
	if err != nil {
		return err
	}
	// The drawn spares are segments now — sealed targets and failed ones —
	// so the adoptions they came from answer to no pool wiring later.
	st.Spares.Adopted = slices.DeleteFunc(st.Spares.Adopted, func(a AdoptedSpare) bool {
		return slices.Contains(c.taken, a.ID)
	})

	// The repoint, record by record at the placement the copier packed it
	// into: only the payload kinds hold pieces the index serves, so only they
	// move; every other kind's placement stays at its source, whose bytes the
	// ended cut keeps below it on disk until the emptying unlinks the
	// segment — a marker, a loss registration or a clone spec is metadata,
	// never a served byte, and served bytes are what the device lost.
	at := int64(format.SegmentHeaderSize)
	var sources []core.SegmentID
	for _, rec := range st.Proof.Reappends {
		if h := rec.Header; h.Kind == format.KindWrite || h.Kind == format.KindFill {
			if _, _, rerr := st.Rebuilt.Index.RepointRecord(h.File,
				index.RecordAt{Segment: rec.Segment, Offset: uint32(rec.At)},
				index.RecordAt{Segment: target, Offset: uint32(at)}); rerr != nil {
				return fmt.Errorf("repointing the re-appended record of seq %d: %w", h.Seq, rerr)
			}
		}
		if !slices.Contains(sources, rec.Segment) {
			sources = append(sources, rec.Segment)
		}
		at += rec.Header.Size()
	}

	// The sources: the tail's proof already ended every segment it handed
	// records over — a lost record's source and a failed window's both —
	// with the cut naming where the truncating phase truncates. The fold
	// answers only a source left with no fate at all, a shape the proof's
	// own answers cannot produce; it is ended whole here rather than left
	// for a later phase that never promised to find it.
	for _, src := range sources {
		if !slices.ContainsFunc(st.Tails.Fates, func(f segmentFate) bool { return f.ID == src }) {
			endSegment(st, src)
		}
	}
	return nil
}

// reserveSpares names the spares the open may draw its re-appends from, in
// the take order the pool's own reserve answers in: the ids the spares phase
// adopted into the repack reserve, ascending.
func reserveSpares(st *State) []core.SegmentID {
	var out []core.SegmentID
	for _, a := range st.Spares.Adopted {
		if a.Class == segment.Reserve {
			out = append(out, a.ID)
		}
	}
	return out
}
