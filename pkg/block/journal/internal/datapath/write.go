package datapath

import (
	"context"
	"fmt"
	"math"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/capacity"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/format"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/index"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/segment"
)

// recordBytes is what one record of n payload bytes costs the journal: the
// size function the stream, the format and the capacity wiring all spell
// together.
func recordBytes(n int64) int64 {
	return format.RecordHeader{Kind: format.KindWrite, Extent: core.Extent{Len: n}}.Size()
}

// emptyRoomOf returns the payload bytes one write record takes in an empty
// segment of cap bytes — the same closed form the stream's fit and Room draw.
func emptyRoomOf(cap int64) int64 {
	room := ((cap - format.SegmentHeaderSize - sealOver(1)) &^ 7) -
		(int64(format.RecordHeaderSize) + format.RecordHeader{Kind: format.KindWrite}.BodySize())
	return max(room, 0)
}

// sealOver returns the seal's reserve over n records: the marker, one
// footer entry per record and the trailer, as the format sizes them.
func sealOver(n int64) int64 {
	return format.SealMarkerSize + n*format.CatalogEntrySize + format.TrailerSize
}

// streamOfID returns the stream the file's appends route to: the FNV-1a
// hash of its FileID over the stream count, the same choice Streams.For
// draws, so the admission's per-stream room grant keys on the stream its
// own appends land on.
func streamOfID(id core.FileID, n int) int {
	const (
		offset = 14695981039346656037
		prime  = 1099511628211
	)
	b := id.Bytes()
	h := uint64(offset)
	for _, x := range b {
		h ^= uint64(x)
		h *= prime
	}
	return int(h % uint64(n))
}

// WriteAt appends p at off of file id as a write record under s's tag, then
// publishes it to the placement index, and returns the version it carries.
// mtime is the modification time the record carries.
//
// A cancelled ctx is refused first and changes nothing; a handle whose close
// began is refused next, with core.ErrClosed, and a call that runs holds its
// handle's Close back until it returns. The file's tag is checked next — a
// file that belongs to another share is refused with core.ErrOtherShare,
// whatever the call's own shape, before the payload's length or the extent's
// are even looked at. A zero-length p then returns the zero version and does
// nothing: no version is drawn and nothing is appended or counted. A
// negative off, or an off whose end overflows, is refused with
// core.ErrInvalidExtent before anything is drawn or appended. The tag check
// claims nothing, so a shape refused after it leaves the file as it was.
//
// The write's record bytes are reserved against the journal's limits before
// its version is drawn or a byte written: the one admission, capacity.Take,
// refuses naming the limit that would not take them — the share's, the
// journal's, the index's, the version limit's or the spare pool's — with the
// shortfall, at once, with no I/O and holding nothing behind. Its grant
// covers every piece the write splits into; the pieces take their spares
// from the grant's hold, and the unused part returns when the write does.
//
// The version is drawn, and the record appended and published, with the
// file's append domain held, so the versions of one file are appended in the
// order they were assigned. The tag is checked again under that domain,
// ahead of the capacity admission and the draw, so checking a file's tag and
// claiming it are one step: two first writes of one new FileID through two
// handles cannot both pass the check. The domain is the only one held across
// the record write, and publishing follows the write: the index never names
// a record that is not yet in its segment. Publishing never waits on a sync.
//
// Any error returns the zero version. A write that fails part-way leaves the
// pieces it already published held; the error says the write as a whole did
// not complete, and the grant's unused bytes and spares return with it.
func (c *Core) WriteAt(ctx context.Context, s *Share, id core.FileID, off int64, p []byte, mtime int64) (core.Version, error) {
	if err := ctx.Err(); err != nil {
		return core.Version{}, err
	}
	exit, err := s.enter()
	if err != nil {
		return core.Version{}, err
	}
	defer exit()
	// The check takes no share of the append domain: it claims nothing, so
	// the call stands down before the domain is taken. It refuses a call
	// naming a file another share owns before the call's own shape is looked
	// at — a zero-length write or an invalid extent names a file all the
	// same, and the wrong handle never reaches those refusals. The check
	// runs again under the domain, where the claim it precedes is made.
	if err := c.checkTag(s, id); err != nil {
		return core.Version{}, err
	}
	if len(p) == 0 {
		return core.Version{}, nil
	}
	if off < 0 || off > math.MaxInt64-int64(len(p)) {
		return core.Version{}, core.ErrInvalidExtent
	}
	v, n, err := c.appendAndPublish(ctx, s, id, off, p, mtime)
	c.writtenBytes.Add(uint64(n))
	return v, err
}

// appendAndPublish does WriteAt's work under the file's append domain and
// returns the version drawn and the payload bytes published.
func (c *Core) appendAndPublish(ctx context.Context, s *Share, id core.FileID, off int64, p []byte, mtime int64) (core.Version, int64, error) {
	unlock := c.domains.Lock(id)
	defer unlock()

	// The tag check runs here again, under the domain the claim publishes
	// under, so the two are one step: a first write through another handle
	// waits on the domain and meets the claim, not an unclaimed entry. A
	// refused write draws no version, appends nothing and counts no written
	// byte. The capacity admission below slots in right after this check,
	// before the draw; the check takes the file index shared and releases
	// it before the admission runs, so the admission names no lock of the
	// file's.
	if err := c.checkTag(s, id); err != nil {
		return core.Version{}, 0, err
	}
	stream := c.streams.For(id)
	// The reservation first: the write's payload, its record bytes drawn
	// by the take from the room the stream names — against the share's
	// limit, the journal's maximum, the index's bound, the version limit
	// and the spares its pieces need, in one decision.
	grant, err := c.cap.Take(ctx, capacity.Request{
		Tag:        s.Tag,
		File:       id,
		Stream:     streamOfID(id, c.streams.Len()),
		Extent:     core.Extent{Off: off, Len: int64(len(p))},
		Payload:    int64(len(p)),
		ShareLimit: s.Limit(),
	})
	if err != nil {
		return core.Version{}, 0, err
	}
	defer grant.Release()

	v, err := c.counters.Draw()
	if err != nil {
		return core.Version{}, 0, err
	}
	var roll segment.Roll
	if hold := grant.Hold(); hold != nil {
		roll = segment.Roll{Hold: hold}
	}
	var done int64
	for done < int64(len(p)) {
		seq, err := c.counters.NextSeq()
		if err != nil {
			return core.Version{}, done, err
		}
		rest := p[done:]
		pl, err := stream.AppendRoll(format.RecordHeader{
			Kind:    format.KindWrite,
			File:    id,
			Extent:  core.Extent{Off: off + done, Len: int64(len(rest))},
			Seq:     seq,
			Version: v,
			Tag:     s.Tag,
		}, format.RecordBody{Payload: rest, MTime: mtime}, roll)
		if err != nil {
			// decision: a write that fails on a later piece, a device
			// failing mid-write beyond what the streams' own refusal of a
			// spoiled segment covers, returns no version, yet the pieces it
			// already published stay held under the version it drew, so
			// reads serve bytes the caller has no version to record. The
			// capacity admission keeps its own refusals — a write past the
			// limits or short of a spare — off this path: they answer
			// before the version is drawn. What remains is a failure of the
			// medium itself, which no reservation can predict. Overturned
			// by returning the version with the error, if the caller can
			// then record the partial write.
			return core.Version{}, done, err
		}
		if pl.N <= 0 {
			// A stream that places nothing and reports no error would have
			// this loop append the same rest forever.
			return core.Version{}, done, fmt.Errorf("datapath: the stream placed no byte of a %d-byte piece of file %v at offset %d", len(rest), id, off+done)
		}
		// The record is in its segment and counted as the share's: its
		// own record size — the header, body and padding it occupies —
		// is what the reservation held for it, and this hands it over,
		// so ReservedBytes never covers a record the share already
		// counts and never misses one still landing.
		grant.Wrote(pl.Loc.Len)
		if c.afterAppend != nil {
			c.afterAppend()
		}
		// decision: the index may still refuse a publish after its record
		// is in the segment. The tag check above refuses a file another
		// share owns before a version is drawn, and the file's tag cannot
		// change while this call holds the file's append domain — only the
		// claim that publishes under this same domain writes a tag, and it
		// holds the domain from its check to its publish — so a tag refusal
		// here needs another tag to claim the file inside WriteRetain
		// itself, without the domain: no path does that today, and a path
		// that did would be the fault shape the rival mark exists for,
		// reached only by a bug. The entry bound, likewise, is checked
		// before the draw — the admission's CheckWrite above — so a
		// refusal here needs the window between that check and this
		// publish, which a concurrent write's own admission can pass in:
		// the admission of each runs before its version is drawn, and
		// neither reserves the bound its record heads for until its
		// publish. The caller gets the error and the record stays on
		// disk, never published by this run. The refusals arrived over
		// the admission are checked there, before any version is drawn;
		// what is left here is the window no pre-check closes, and a
		// refusal in it is overturn evidence for a bound the admission
		// reserves as its own.
		hd, err := c.index.WriteRetain(s.Tag, id, index.Placed{
			Extent:  core.Extent{Off: off + done, Len: pl.N},
			Record:  index.RecordAt{Segment: pl.Loc.Segment, Offset: uint32(pl.Loc.Off)},
			Version: v,
		}, seq)
		if err != nil {
			return core.Version{}, done, err
		}
		c.Account(hd)
		// decision: the index exports no write that skips retaining what it
		// supersedes, so a piece whose record a sync already covered lifts
		// here, straight after the publish, rather than leaving the content
		// retained until the durability worker's pass. The lift is per record
		// extent: it drops only what this piece's own record superseded, so a
		// durable piece never drops what a sibling piece of the same version,
		// not yet durable, still has to hold. The index's error from the lift
		// is unreachable today, because the function it runs returns nil; it
		// is ignored, and the worker lifts again after this write releases the
		// file's append domain and logs a failure. Its delta is accounted
		// before the error is looked at. Overturned by an index write that
		// takes a durable flag per piece.
		//
		// ponytail: Durable scans every stream's ledger forward under that
		// stream's mutex, once per published piece, and a ledger holds up to
		// one sync bound of writes, so the cost grows with concurrent small
		// writes. Upgrade to a scan from the end of each ledger, or one stream,
		// inside Durable when a profile of small concurrent writes shows it.
		if c.dur.Durable(pl.Loc) {
			lh, _ := c.index.LiftSuperseded(id, v, core.Extent{Off: off + done, Len: pl.N})
			c.Account(lh)
		}
		done += pl.N
	}
	return v, done, nil
}
