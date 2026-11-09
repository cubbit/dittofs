package datapath

import (
	"context"
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
			return v, done, err
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
			return v, done, err
		}
		hd, err := c.index.WriteRetain(s.Tag, id, index.Placed{
			Extent:  core.Extent{Off: off + done, Len: pl.N},
			Record:  index.RecordAt{Segment: pl.Loc.Segment, Offset: uint32(pl.Loc.Off)},
			Version: v,
		}, seq)
		if err != nil {
			return v, done, err
		}
		c.Account(hd)
		done += pl.N
	}
	return v, done, nil
}
