package reclaim

import (
	"math"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
)

// Reserve is what a pass asks of the repack reserve: how much it holds, and
// where a copy's bytes are counted. Its zero value bounds nothing and counts
// nothing, for a pass over a journal with no reserve to ask.
type Reserve struct {
	// Held reports the bytes of the repack reserve's ready spares now.
	Held func() int64
	// Count counts n record bytes of a copy of tag's records, made out of
	// source segment src, to the journal's reserve while src remains.
	Count func(src core.SegmentID, tag core.ShareTag, n int64)
	// Transfer hands every copy counted against src to its share. The
	// retirement of src does it too, for every segment it retires; the pass
	// calls it for a source it abandons, whose copies are the share's
	// records from then on.
	Transfer func(src core.SegmentID)
}

// held is what the repack reserve holds when the pass begins, the most the
// pass's units may copy between them: a pass runs its units one source at a
// time and retires each as it goes, but only the reserve's own refill says
// when the reserve is whole again, so the pass counts every unit it ran
// against the one reading and leaves the rest for the next call. The caller's
// budget bounds the selection beside it, never a unit in flight, so a budget
// under one unit's copies still runs the one unit; the reserve admits or
// refuses that unit whole.
func (r Reserve) held() int64 {
	if r.Held == nil {
		return math.MaxInt64
	}
	return r.Held()
}

// admits reports whether a unit expected to copy need bytes fits in what is
// left of allowed after the bytes the pass already copied.
//
// ponytail: need is the candidate's live bytes, which leave out the record
// headers and footer entries its copies add, so a unit can pass the reserve by
// a few hundred bytes a record. A copy that outgrows the spare it rolled into
// draws on the pool's refill. Upgrade to sizing a candidate's copies by its
// record count when a pass is seen refused for a spare it admitted.
func admits(allowed, copied, need int64) bool { return copied+need <= allowed }
