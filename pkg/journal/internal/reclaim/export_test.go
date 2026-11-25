package reclaim

import (
	"context"

	"github.com/cubbit/dittofs/pkg/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/journal/internal/index"
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

// LivePlacementsOf answers how many live registrations the registry holds of
// rec's identity in rec's own segment — the original, the copy a carry or a
// rebuild placed — read from tests to observe the identity class the class
// hold rule keeps: a registration spent by a re-test or a retirement, or one
// a rebuild never placed, answers zero, without handing tests the registry
// itself. The record locates by its segment rather than its whole key so a
// test reads a placement whose location the caller it drove did not report.
func (t *Tracker) LivePlacementsOf(rec Rec) int {
	t.rm.Lock()
	defer t.rm.Unlock()
	n := 0
	for r := range t.identities[identityOf(rec)][rec.Seg] {
		if rs, ok := t.recs[r.File]; ok {
			n += rs[r]
		}
	}
	return n
}
