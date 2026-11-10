package datapath

import (
	"context"
	"errors"
	"fmt"
	"math"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/capacity"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/format"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/index"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/segment"
)

// Fill appends the parts of p at off of file id that nothing holds as fill
// records at version v under s's tag, and holds them offloaded: p is content
// the caller read from the remote tier, so it is already durable there and
// never needs uploading. Parts a held piece covers, or a removal marker above
// v masks, are left as they are and nothing of them is appended; a fill whose
// extent is fully held appends nothing. asOf is the change version the
// caller's read of the remote tier stood at: a fill refused with
// core.ErrStaleFill — the file changed after it — appends nothing.
//
// Fill draws no version: v is the version the caller read the content at, and
// each record carries it with a fresh sequence number. The refusals come in
// WriteAt's order — a cancelled ctx, a closed handle, another share's file,
// then a zero-length p, which does nothing, and an invalid extent — and the
// rest is decided under the file's append domain, where the absent parts are
// read, so a write that lands afterwards is serialised after the fill and
// supersedes it. Its record bytes are admitted by every limit a write is, but
// the version limit.
func (c *Core) Fill(ctx context.Context, s *Share, id core.FileID, off int64, p []byte, asOf, v core.Version) (err error) {
	if err := ctx.Err(); err != nil {
		return err
	}
	exit, err := s.enter()
	if err != nil {
		return err
	}
	defer exit()
	if err := c.checkTag(s, id); err != nil {
		return err
	}
	if len(p) == 0 {
		return nil
	}
	if off < 0 || off > math.MaxInt64-int64(len(p)) {
		return core.ErrInvalidExtent
	}
	if c.tracer != nil {
		var end func(error)
		ctx, end = c.tracer(ctx, core.SpanFill)
		defer func() { end(err) }()
	}
	n, err := c.fillAndPublish(ctx, s, id, off, p, asOf, v)
	c.filledBytes.Add(uint64(n))
	return err
}

// fillAndPublish does Fill's work under the file's append domain and returns
// the payload bytes published.
func (c *Core) fillAndPublish(ctx context.Context, s *Share, id core.FileID, off int64, p []byte, asOf, v core.Version) (int64, error) {
	unlock := c.domains.Lock(id)
	defer unlock()

	if err := c.checkTag(s, id); err != nil {
		return 0, err
	}
	gaps := c.index.FillGaps(id, core.Extent{Off: off, Len: int64(len(p))}, v)
	// Staleness is checked even when nothing is absent: a fill of a file that
	// changed since the caller's read is refused whole either way.
	if err := c.index.CheckFill(s.Tag, id, asOf); err != nil {
		if errors.Is(err, core.ErrStaleFill) {
			c.fillRefused.Add(1)
		}
		return 0, err
	}
	if len(gaps) == 0 {
		return 0, nil
	}
	// Take sizes the payload as one split with one record header; each further
	// gap appends a record of its own, so it adds its header and body bytes and
	// the padding its length may need, and the reservation fails closed.
	var payload int64
	for _, g := range gaps {
		payload += g.Len
	}
	payload += int64(len(gaps)-1) * (segment.RecordBytes(0) + 7)
	last := gaps[len(gaps)-1]
	stream := c.streams.For(id)
	grant, err := c.cap.Take(ctx, capacity.Request{
		Tag:        s.Tag,
		File:       id,
		Stream:     c.streams.IndexFor(id),
		Extent:     core.Extent{Off: gaps[0].Off, Len: last.Off + last.Len - gaps[0].Off},
		Payload:    payload,
		ShareLimit: s.Limit(),
		Fill:       true,
	})
	if err != nil {
		return 0, err
	}
	defer grant.Release()

	var roll segment.Roll
	if hold := grant.Hold(); hold != nil {
		roll = segment.Roll{Hold: hold}
	}
	var filled int64
	// decision: a failure on a later piece leaves the earlier pieces held
	// offloaded, since their bytes are remote content at v, not lost writes.
	for _, g := range gaps {
		for done := int64(0); done < g.Len; {
			seq, err := c.counters.NextSeq()
			if err != nil {
				return filled, err
			}
			rest := p[g.Off-off+done : g.Off-off+g.Len]
			pl, err := stream.AppendRoll(format.RecordHeader{
				Kind:    format.KindFill,
				File:    id,
				Extent:  core.Extent{Off: g.Off + done, Len: int64(len(rest))},
				Seq:     seq,
				Version: v,
				Tag:     s.Tag,
			}, format.RecordBody{Payload: rest}, roll)
			if err != nil {
				return filled, err
			}
			if pl.N <= 0 {
				return filled, fmt.Errorf("datapath: the stream placed no byte of a %d-byte piece of file %v at offset %d", len(rest), id, g.Off+done)
			}
			grant.Wrote(pl.Loc.Len)
			// decision: the index may still refuse the publish after its record is
			// in the segment: as stale, a floor raise by another file's change
			// between the pre-check and here, or by the bound, the entries this
			// piece adds not fitting the count other files' commits, or this fill's own
			// earlier pieces, have raised since Take's read-only check. Either way the record stays on disk,
			// but the index has noted it in the file's drop ranges and claimed
			// the file, so a later removal record is kept while its segment
			// lives, and it reserved nothing. Recovery replays it as held
			// offloaded content, which is correct remote content at v; nothing
			// resurrects. The caller reads the remote again. Overturned by a
			// refusal here that costs a caller more than one remote read.
			hd, err := c.index.Fill(s.Tag, id, index.Placed{
				Extent:  core.Extent{Off: g.Off + done, Len: pl.N},
				Record:  index.RecordAt{Segment: pl.Loc.Segment, Offset: uint32(pl.Loc.Off)},
				Version: v,
			}, seq, asOf)
			if err != nil {
				if errors.Is(err, core.ErrStaleFill) {
					c.fillRefused.Add(1)
				}
				return filled, err
			}
			c.Account(hd)
			filled += hd.HeldBytes
			done += pl.N
		}
	}
	return filled, nil
}
