package journal

import (
	"context"

	"github.com/cubbit/dittofs/pkg/journal/internal/reclaim"
)

// RepackScope aims a repack pass: at the segments holding one share's records,
// at a set of segments, or, when zero, at every segment.
type RepackScope struct {
	Share    ShareTag    // zero: any share
	Segments []SegmentID // empty: any segment
}

// Repack copies the still-live records of the best candidate segments within
// scope into another segment, repoints the placement index and retires the
// originals, copying at most budget bytes. It returns freed, the storage its
// retirements returned to the filesystem or to the spare pool, once they are
// durable. The storage in use, UsedBytes less the spares at the segment cap,
// falls at the same moment, never earlier: an unlink lowers UsedBytes itself,
// while a recycle leaves it where it was and adds a spare.
//
// Repack preserves every held and retained extent, the bytes they produce and
// their offloaded bits, and discards nothing. It decides which segments to
// repack without reading them, taking the ones with the least live payload
// first, and never selects a segment a running offer pins.
//
// A done context is answered before anything changes; a pass the journal's
// close interrupts between its units returns ErrClosed having completed the
// unit in flight. A zero-or-negative budget runs no unit.
func (j *Journal) Repack(ctx context.Context, budget int64, scope RepackScope) (freed int64, err error) {
	// decision: a zero Journal, one no open built, refuses Repack with
	// ErrNotImplemented rather than panicking, the same guard Share keeps:
	// no caller holds a zero value past building it, and the journaltest
	// fixture's cleanup builds one. Overturned by a caller that calls Repack
	// on a zero value expecting a real answer, when Open makes zero values
	// unreachable.
	if j.core == nil {
		return 0, ErrNotImplemented
	}
	if j.closed.Load() {
		return 0, ErrClosed
	}
	return j.core.Repack(ctx, budget, reclaim.Scope{Share: scope.Share, Segments: scope.Segments})
}
