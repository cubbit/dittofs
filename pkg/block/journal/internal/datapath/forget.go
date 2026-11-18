package datapath

import (
	"context"
	"fmt"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/capacity"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/format"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/index"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/reclaim"
)

// Forget drops tag whole: it appends one header-only forget record naming
// the tag, drawn on the reserved headroom and durable before it returns, and
// drops from the index every held and retained extent, removal marker and
// hold mark of the tag's files — so nothing of the tag is left to settle,
// Since never yields it, and recovery, applying the record to every record
// of the tag below its sequence number, rebuilds none of it.
//
// The order the answer runs in: the context is checked first, then tag 0
// with core.ErrInvalidSetting and a core whose Close began with
// core.ErrClosed; the tag's forgetting mark goes on the registry next, with
// an open handle of the tag refusing the whole call with core.ErrHandleOpen,
// and while the mark stands a Share of the tag is refused the same way,
// wrapped to name the forget. The mark is released on every return, so a
// refused or failed forget leaves the tag usable.
//
// A tag no segment holds a record of, with no file in the index either, is
// answered nil without appending anything: recovery has nothing to apply,
// and a start that names an already-forgotten tag does no I/O.
//
// The record draws on the reserved headroom, never a share's limit or the
// journal's maximum, so it is refused only by an exhausted headroom — a
// NoSpaceError naming core.LimitHeadroom — by the version limit's absence or
// by its own append failing. Nothing is dropped before the record is
// durable: a failed window answers the error and the tag stays whole, and a
// crash finding the record undropped finds the tag with all its extents.
//
// Once the record is durable, each of the tag's files is dropped under its
// own append domain, the index held for the write — its extents and markers
// through Account, its tag marks, its drop ranges for the records below the
// record's sequence number — and the segments the drop emptied are offered
// to the retirement worker with the forget record as their causality
// record. The forget record itself registers live against the snapshot of
// the segments that held the tag's records when the barrier passed: it stays
// live while any of them remains, so a segment holding only the forgotten
// tag's records retires only once the record that ends them is durable. The
// tag leaves the stranded set, and its record-byte figures leave the segment
// set, so Stats no longer lists a tag whose records still sit on disk.
func (c *Core) Forget(ctx context.Context, tag core.ShareTag) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if tag == 0 {
		return core.ErrInvalidSetting
	}
	if c.closed() {
		return core.ErrClosed
	}

	// The mark goes on under the registry's mutex, beside the refusal an
	// open handle of the tag reads: the two are one step against a Share of
	// the tag, so an attach cannot land behind the barrier. Lock order at
	// this site and below: the registry's mutex outermost, the forgetting
	// mark's inside it, the stranded set's innermost.
	//
	// decision: the open-handle refusal answers core.ErrHandleOpen bare,
	// unlike the wrapped refusal a Share of the forgetting tag reads, because
	// the call names its one obstacle by the tag it was given and the caller
	// closing its handles — the only caller that could retry — learns
	// everything it needs from the bare one. Overturned by a caller that must
	// know which handle blocks, which an ErrHandleOpen carrying the handle
	// would answer.
	c.registryMu.Lock()
	if p := c.registry.Load(); p != nil {
		for _, s := range *p {
			if s.Tag == tag {
				c.registryMu.Unlock()
				return core.ErrHandleOpen
			}
		}
	}
	c.markForgetting(tag)
	c.registryMu.Unlock()
	defer c.clearForgetting(tag)

	// The files the index still lists for the tag are the ones it drops: a
	// file whose every extent was already released holds no piece a copy
	// could resurrect, and its records on disk ride their segments out with
	// the liveness snapshot alone.
	//
	// decision: a tag nothing holds anywhere — no record byte in any
	// segment, no file in the index — is answered nil with no record
	// appended, because a forget record of a tag that holds nothing can
	// change nothing recovery would serve, and a start that names every
	// forgotten tag again would make one record per tag of no effect. Its
	// ceiling: an unlisted stranding the call still leaves — the answer
	// takes the tag out of the stranded set, which needs no record, since
	// the set is memory the caller's next start re-makes. Overturned by a
	// caller the journal owes a durable receipt of the forget to.
	files := c.index.Files(tag)
	if len(files) == 0 && c.set.ShareBytes(tag) == 0 {
		c.unstrand(tag)
		return nil
	}

	// The barrier: each of the tag's files' append domain taken and released
	// once, never two at once. A repack copy of the tag whose held-check ran
	// before the mark is appending inside one of these domains, so it has
	// appended by the time the domain is released and its sequence number is
	// below the record drawn after the barrier; every copy whose check has
	// not run yet reads the mark under its own domain and is abandoned.
	for _, f := range files {
		unlock := c.domains.Lock(f)
		unlock()
	}

	// The liveness snapshot, taken after the barrier and before the record
	// is appended: the segments holding the tag's record bytes now are the
	// only ones that can hold a record of the tag below the record drawn
	// next, and the registration re-checks each of them against the set, so
	// one the worker retires between here and there costs the snapshot
	// nothing.
	var below []core.SegmentID
	for _, row := range c.set.Snapshot() {
		if row.Tags[tag] > 0 {
			below = append(below, row.ID)
		}
	}

	// The record: admitted from the reserved headroom like a removal's,
	// appended on the zero file's stream — a forget names no file — and
	// carrying a sequence number the recovery's withhold rule reads. The
	// format refuses a forget record that carries anything else.
	draw, err := c.headroom.Admit(capacity.Forget)
	if err != nil {
		return err
	}
	if !draw.Append {
		return &core.NoSpaceError{Limit: core.LimitHeadroom, Missing: 1, Unit: core.UnitEntries}
	}
	seq, err := c.counters.NextSeq()
	if err != nil {
		c.headroom.Refund(draw)
		return err
	}
	pl, err := c.streams.For(core.FileID{}).Append(format.RecordHeader{
		Kind: format.KindForget,
		Seq:  seq,
		Tag:  tag,
	}, format.RecordBody{})
	if err != nil {
		// The record never landed, so the headroom never spent it.
		c.headroom.Refund(draw)
		return err
	}
	c.headroom.Drawn(draw)

	// decision: nothing is dropped before the record is durable, so a
	// failed window — the record unsynced, its segment ended — answers the
	// error with the tag whole: the in-memory index needs no undo for a
	// record that never became publishable, and a crash finding the record
	// undurable drops it and rebuilds the tag's extents exactly as they
	// were. Overturned by the durability layer gaining an undo for an
	// appended-but-unsynced header-only record, which would allow a drop
	// beside a failed one and a re-issuable forget in the same window.
	if err := c.dur.WaitDurable(pl.Loc); err != nil {
		return err
	}

	// The drop, per file under its append domain, the index held for the
	// write: the delta runs through Account as any index change does, and
	// the segments whose held bytes the drop emptied are offered to the
	// retirement worker under the same domain, with the forget record as the
	// causality record the offer's gate reads — the record is durable
	// already, so the gate passes at once. One domain at a time, like the
	// barrier: R46's rule keeps the two halves of the forget from holding
	// what a repack copy of another file could be waiting on.
	for _, f := range files {
		unlock := c.domains.Lock(f)
		hd, named, derr := c.index.ForgetFile(f, seq)
		if derr != nil {
			unlock()
			return fmt.Errorf("datapath: forgetting file %v: %w", f, derr)
		}
		if terr := c.index.ForgetTagMarks(f, tag); terr != nil {
			unlock()
			return fmt.Errorf("datapath: forgetting the marks of file %v: %w", f, terr)
		}
		c.Account(hd)
		for _, seg := range named {
			if !c.liveness.Held(seg) {
				c.retiring.Emptied(seg, pl.Loc)
			}
		}
		unlock()
	}

	// The forget record's liveness: registered against the snapshot taken at
	// the barrier, it is live while any segment that held the tag's records
	// below it remains, so no retirement frees the record's own segment
	// early and a reopen with records of the tag still on disk finds the
	// record that ends them.
	c.liveness.RegisterForget(reclaim.Rec{
		Kind: format.KindForget,
		Seg:  pl.Loc.Segment,
		Pair: index.Numbers{Seq: seq},
		Seq:  seq,
		Tag:  tag,
		Loc:  pl.Loc,
	}, below)

	// The tag's accounting goes with its extents: the set's per-segment
	// record-byte figures leave — Stats stops listing the tag while its
	// records wait out their segments — and the released bytes enter the
	// tag's cumulative retired figure once, which the reconciliation this
	// shares its records with reads next, settling a seeded share's figure to
	// nothing now rather than at whichever retirement reports first.
	c.set.ForgetTag(tag)
	c.cap.RetiredEverySeeded()
	c.unstrand(tag)
	return nil
}
