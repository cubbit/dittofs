package reclaim

import (
	"context"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/index"
)

// ReadSource reads one record of seg whole through the unread set, the same
// read the pass's own first phase makes of every source, so a test can drive
// one record's read exactly as the copier does.
func (c *Copier) ReadSource(ctx context.Context, seg core.SegmentID, file core.FileID, rec index.RecordAt) ([]byte, error) {
	return c.readSource(ctx, seg, file, rec)
}

// SetAfterSnapshot sets a hook that runs between Select's ledger snapshot and
// its claims.
func (s *Selector) SetAfterSnapshot(fn func()) { s.afterSnapshot = fn }

// SetAfterClaim sets a hook that runs in claim once a segment is recorded as
// claimed and before its pin is read.
func (s *Selector) SetAfterClaim(fn func(core.SegmentID)) { s.afterClaim = fn }
