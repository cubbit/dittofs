package datapath

import (
	"context"
	"fmt"
	"math"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/format"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/index"
)

// WriteAt appends p at off of file id as a write record under s's tag, then
// publishes it to the placement index, and returns the version it carries.
// mtime is the modification time the record carries.
//
// A cancelled ctx is refused first and changes nothing. A zero-length p
// returns the zero version and does nothing: no version is drawn and nothing
// is appended or counted. A negative off, or an off whose end overflows, is
// refused with core.ErrInvalidExtent before anything is drawn or appended.
//
// The version is drawn, and the record appended and published, with the
// file's append domain held, so the versions of one file are appended in the
// order they were assigned. The domain is the only one held across the record
// write, and publishing follows the write: the index never names a record
// that is not yet in its segment. Publishing never waits on a sync.
//
// Any error returns the zero version. A write that fails part-way leaves the
// pieces it already published held; the error says the write as a whole did
// not complete.
func (c *Core) WriteAt(ctx context.Context, s *Share, id core.FileID, off int64, p []byte, mtime int64) (core.Version, error) {
	if err := ctx.Err(); err != nil {
		return core.Version{}, err
	}
	if len(p) == 0 {
		return core.Version{}, nil
	}
	if off < 0 || off > math.MaxInt64-int64(len(p)) {
		return core.Version{}, core.ErrInvalidExtent
	}
	v, n, err := c.appendAndPublish(s, id, off, p, mtime)
	c.writtenBytes.Add(uint64(n))
	return v, err
}

// appendAndPublish does WriteAt's work under the file's append domain and
// returns the version drawn and the payload bytes published.
func (c *Core) appendAndPublish(s *Share, id core.FileID, off int64, p []byte, mtime int64) (core.Version, int64, error) {
	unlock := c.domains.Lock(id)
	defer unlock()

	v, err := c.counters.Draw()
	if err != nil {
		return core.Version{}, 0, err
	}
	stream := c.streams.For(id)
	var done int64
	for done < int64(len(p)) {
		seq, err := c.counters.NextSeq()
		if err != nil {
			return core.Version{}, done, err
		}
		rest := p[done:]
		pl, err := stream.Append(format.RecordHeader{
			Kind:    format.KindWrite,
			File:    id,
			Extent:  core.Extent{Off: off + done, Len: int64(len(rest))},
			Seq:     seq,
			Version: v,
			Tag:     s.Tag,
		}, format.RecordBody{Payload: rest, MTime: mtime})
		if err != nil {
			return core.Version{}, done, err
		}
		if pl.N <= 0 {
			// A stream that places nothing and reports no error would have
			// this loop append the same rest forever.
			return core.Version{}, done, fmt.Errorf("datapath: the stream placed no byte of a %d-byte piece of file %v at offset %d", len(rest), id, off+done)
		}
		if c.afterAppend != nil {
			c.afterAppend()
		}
		// decision: the index may refuse a publish after its record is in the
		// segment, by a tag that owns the file elsewhere or by the entry bound.
		// The caller gets the error and the record stays on disk, never
		// published by this run. That is accepted because only one share exists
		// and no bound is set low, so the refusal is unreachable until checks
		// that refuse before a version is drawn land; they close the window and
		// this marker goes then.
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
