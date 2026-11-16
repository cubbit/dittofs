package datapath

import (
	"context"
	"math"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/capacity"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
)

// Reserve takes, at once, everything writes writes of bytes record bytes in
// all will need, as the admission takes it: the bytes against the share's
// limit and the journal's maximum, a footer entry and an index entry per
// piece, one version per write below the version limit, and the spares the
// bytes roll into from the pool. A reservation is taken once and its writes
// draw on it through WriteReserved, so an operation that must not fail
// part-way — a clone's copies — finds every refusal before it changes
// anything.
//
// A cancelled ctx and a handle whose close began are refused as WriteAt
// refuses them; a request of negative bytes or non-positive writes is a
// caller bug, core.ErrInvalidExtent.
func (c *Core) Reserve(ctx context.Context, s *Share, bytes int64, writes int) (*capacity.Reservation, error) {
	exit, err := beginCall(ctx, s)
	if err != nil {
		return nil, err
	}
	defer exit()
	if bytes <= 0 || writes <= 0 {
		return nil, core.ErrInvalidExtent
	}
	return c.cap.Reserve(ctx, capacity.ReserveRequest{
		Tag:        s.Tag,
		Bytes:      bytes,
		Writes:     writes,
		ShareLimit: s.Limit(),
	})
}

// WriteReserved appends p at off of file id as a write record drawn from the
// reservation r, under s's tag, and returns the version it carries. A
// reservation is owned by one goroutine at a time — its draws, its writes and
// its Release — so a caller never runs two WriteReserved of one r at once; see
// capacity.Reservation. It is
// WriteAt with the reservation's grant in place of the admission's: the tag
// check, the append domain, the publish and the counters are the same, and no
// limit of the journal refuses it while r holds enough. A write r no longer
// holds enough for is refused with core.ErrReservationExhausted before any
// version is drawn or byte written. A cancelled ctx and a zero-length or
// invalid write are refused as WriteAt refuses them, drawing nothing.
func (c *Core) WriteReserved(ctx context.Context, s *Share, r *capacity.Reservation, id core.FileID, off int64, p []byte, mtime int64) (core.Version, error) {
	exit, err := beginCall(ctx, s)
	if err != nil {
		return core.Version{}, err
	}
	defer exit()
	if err := c.checkTag(s, id); err != nil {
		return core.Version{}, err
	}
	if len(p) == 0 {
		return core.Version{}, nil
	}
	if off < 0 || off > math.MaxInt64-int64(len(p)) {
		return core.Version{}, core.ErrInvalidExtent
	}
	// A reservation is only the share's that took it: its bytes and entries
	// were charged there, and another share's write would spend them under the
	// wrong tag. That is a caller bug, refused before anything is drawn.
	if s.Tag != r.Tag() {
		return core.Version{}, core.ErrInvalidSetting
	}
	// The append loop draws this write's version from the reservation, under
	// the file's append domain, exactly where a plain write's Take draws its
	// own: the versions of one file are appended in the order they were
	// assigned, so a reserved write parked before the domain can never carry an
	// older version than a plain write that took the domain first.
	v, n, err := c.appendAndPublish(ctx, s, id, off, p, mtime, r)
	c.writtenBytes.Add(uint64(n))
	return v, err
}
