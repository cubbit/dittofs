package reclaim

import "github.com/cubbit/dittofs/pkg/block/journal/internal/core"

// SetAfterSnapshot sets a hook that runs between Select's ledger snapshot and
// its claims.
func (s *Selector) SetAfterSnapshot(fn func()) { s.afterSnapshot = fn }

// SetAfterClaim sets a hook that runs in claim once a segment is recorded as
// claimed and before its pin is read.
func (s *Selector) SetAfterClaim(fn func(core.SegmentID)) { s.afterClaim = fn }
