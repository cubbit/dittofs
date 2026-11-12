package durability

import (
	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/format"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/index"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/segment"
)

// isContent reports whether w is a record of file content: a write or a fill.
// It is the records that hold extents in the index, and so the ones that
// supersede retained content; every other kind carries its whole encoding in
// w.Bytes and holds nothing.
func isContent(w segment.Written) bool {
	return w.Kind == format.KindWrite || w.Kind == format.KindFill
}

// lift stops retaining what the record of version v and header extent e
// superseded in file, now that the record is synced, accounts the change, and
// reports the move: the segments whose retained counters it lowered are named
// through Config.Emptied, gated on at, the superseding record's place — the
// causality record a retirement of the superseded segments waits on. A
// failure is logged and stands: the extent stays retained, which wastes space
// and loses nothing.
//
// decision: the caller holds file's append domain. A writer publishes the
// content it superseded as retained while it holds that domain, from its
// append to its publish, so a sync that covers the record in that gap lifts
// only after the publish and finds the extent to drop. Lifting outside the
// domain would find nothing to drop, and the extent would stay retained for
// good. Overturned by a publish that no longer follows the append under one
// domain hold.
//
// decision: a caller that cannot place the superseding record — a cover a
// failed window's records handed the parts to, which the index names by
// version alone — passes the zero location and the move reports nothing:
// every such lift names a superseder that already synced (a cover is lifted
// here only once its record left the ledger, and a corrupt record is lifted
// only once it was synced), so nothing rides on the absent gate, and there
// is no place to gate a report on. Overturned by a caller that lifts a
// superseder which has not synced yet: that caller must place the record so
// the report can gate a retirement on it.
func (d *Durability) lift(file core.FileID, v core.Version, e core.Extent, at segment.Location) {
	hd, segs, err := d.index.LiftSuperseded(file, v, e)
	if err != nil {
		d.log.Error("journal could not lift superseded content", "file", file, "version", v, "extent", e, "err", err)
		return
	}
	d.accountDelta(hd)
	d.reportEmptied(segs, at)
}

// reportEmptied hands the segments a move of this layer lowered to
// Config.Emptied, with at the location of the causality record whose
// durability a retirement of the segments waits on. Nothing is reported when
// the move lowered nothing, when no record place is had to gate on (a
// deferred loss whose record another path owes, one whose append was
// refused), or when no hook is set.
//
// decision: a mover can report Emptied for a segment whose retirement waits
// on a record later than its own: another mover's unsynced cut can have
// taken the segment's held bytes to zero before this report's crossing,
// leaving the segment's own offer with nothing to name but the crossing it
// observed. The hand-over that consumes the report gates on the pending-tail
// barrier, which holds the retirement until every stream's appended record
// — resident in a ledger or held by a pending window — is durable, so this
// report needs no timing premise of its own. The barrier is exact while
// every record that empties a segment is appended before the cut that
// empties it, which every mover does. Overturned by a mover publishing a cut
// whose record reaches neither a ledger nor a pending window before the
// hand-over claims the segment.
func (d *Durability) reportEmptied(segs []core.SegmentID, at segment.Location) {
	if len(segs) == 0 || at == (segment.Location{}) || d.emptied == nil {
		return
	}
	d.emptied(segs, at)
}

// accountDelta hands hd to Config.Account when one is set and hd changes
// something.
func (d *Durability) accountDelta(hd index.HeldDelta) {
	if d.account != nil && hd != (index.HeldDelta{}) {
		d.account(hd)
	}
}
