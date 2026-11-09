package datapath

import (
	"context"
	"math"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
)

// ReadAt resolves the span of file id that len(p) bytes at off cover, and
// returns the change version the read reports. It returns the bytes it served
// in n, the extents the journal holds nothing for in missing, the content
// versions of the pieces it served in held, and the version in asOf.
//
// A cancelled ctx is refused first and changes nothing; a handle whose close
// began is refused next, with core.ErrClosed. A zero-length p serves nothing,
// reports nothing missing and does no resolve. A negative off, or an off whose
// end overflows, is refused with core.ErrInvalidExtent.
//
// For a non-empty span the read currently serves nothing: n is zero, p is left
// untouched, and every byte the index does not hold is reported missing. A
// file with no entry reports the whole extent missing.
func (c *Core) ReadAt(ctx context.Context, s *Share, id core.FileID, off int64, p []byte) (n int, missing []core.Extent, held []core.Held, asOf core.Version, err error) {
	if err := ctx.Err(); err != nil {
		return 0, nil, nil, core.Version{}, err
	}
	exit, err := s.enter()
	if err != nil {
		return 0, nil, nil, core.Version{}, err
	}
	defer exit()
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
	_, missing = c.index.Span(id, core.Extent{Off: off, Len: int64(len(p))})

	// The pieces Span placed are read, verified and copied into p here, and
	// their versions returned in held; nothing is served yet.
	return 0, missing, nil, asOf, nil
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
