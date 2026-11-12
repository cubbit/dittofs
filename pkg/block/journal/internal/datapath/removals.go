package datapath

import (
	"context"
	"math"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/capacity"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/format"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/index"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/lockorder"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/segment"
)

// Truncate stops holding every extent of file id at or beyond size, and
// narrows an extent straddling it. It draws a version inside the file's
// serialised append, appends a header-only record carrying it, leaves a
// removal marker, and returns once that record is durable.
//
// Its refusals come in WriteAt's order: a cancelled ctx, a closed handle,
// another share's file, then a negative size, which is no extent and is
// refused with core.ErrInvalidExtent before anything is drawn or appended. Its
// record draws on the reserved headroom, so it is never refused by the share's
// limit or the journal's maximum; what can refuse it is the headroom, the
// version limit and, when the truncate splits a held extent, the index's cap.
func (c *Core) Truncate(ctx context.Context, s *Share, id core.FileID, size int64) (core.Version, error) {
	r := removal{
		class:  capacity.Truncate,
		change: core.ChangeTruncate,
		kind:   format.KindTruncate,
		// A truncate's header names the new size and writes length 0; its
		// marker runs from that size to the end of the file.
		header: core.Extent{Off: size},
		marker: toEnd(size),
	}
	if size < 0 {
		r.bad = core.ErrInvalidExtent
	}
	return c.remove(ctx, s, id, r)
}

// Deallocate stops holding extent e of file id, whatever its offloaded bit. It
// draws a version, appends a header-only record carrying it, leaves a removal
// marker, and returns once that record is durable.
//
// A zero-length e is refused with core.ErrZeroLength and appends nothing, and
// so is one whose offset is negative or whose end overflows, with
// core.ErrInvalidExtent: neither is an extent the journal could marker. Its
// refusals are Truncate's.
func (c *Core) Deallocate(ctx context.Context, s *Share, id core.FileID, e core.Extent) (core.Version, error) {
	r := removal{
		class:  capacity.Deallocate,
		change: core.ChangeDeallocate,
		kind:   format.KindDeallocate,
		header: e,
		marker: e,
	}
	r.bad = shapeError(e)
	return c.remove(ctx, s, id, r)
}

// shapeError is the refusal a removal extent earns: a zero-length one is
// core.ErrZeroLength, which a record would read back as a removal to the end
// of the file, and a negative offset or length, or an end that overflows, is
// core.ErrInvalidExtent. It answers nil for an extent a record can carry.
func shapeError(e core.Extent) error {
	switch {
	case e.Len == 0:
		return core.ErrZeroLength
	case e.Len < 0, e.Off < 0, e.Off > math.MaxInt64-e.Len:
		return core.ErrInvalidExtent
	}
	return nil
}

// cloneShapeError is shapeError for a clone target's destination, and the
// clone's source too: a source offset that is negative or whose end overflows
// the copy's length, and an AsOf carrying an epoch, are shapes the record's
// encoding refuses by panicking, so they are refused here with
// core.ErrInvalidExtent before anything is drawn or appended.
func cloneShapeError(e core.Extent, spec core.CloneSpec) error {
	if err := shapeError(e); err != nil {
		return err
	}
	if spec.SrcOff < 0 || spec.SrcOff > math.MaxInt64-spec.Len || spec.AsOf.Epoch != 0 {
		return core.ErrInvalidExtent
	}
	return nil
}

// Delete stops holding every extent of file id, draws a version, appends a
// header-only record carrying it, leaves a removal marker over the whole file,
// and returns once that record is durable. Its record names no extent — a
// delete and a truncate write length 0 — and its refusals are Truncate's.
func (c *Core) Delete(ctx context.Context, s *Share, id core.FileID) (core.Version, error) {
	return c.remove(ctx, s, id, removal{
		class:  capacity.Delete,
		change: core.ChangeDelete,
		kind:   format.KindDelete,
		header: core.Extent{},
		marker: toEnd(0),
	})
}

// CloneTarget stops holding (spec.DstOff, spec.Len) of file id as Deallocate
// does, and its record and marker carry spec, so a restart that finds the
// marker resumes the clone rather than leaving the range reading as a hole. It
// draws a version, appends a header-only record carrying it and spec, and
// returns once that record is durable.
//
// A zero-length spec is refused with core.ErrZeroLength and appends nothing.
// core.ErrInvalidExtent refuses one whose destination cannot exist — a negative
// offset or length, or an end that overflows — and one whose source cannot be
// named: a negative SrcOff, a SrcOff whose end overflows Len, or an AsOf
// carrying an epoch. Its record frees nothing, so besides Deallocate's refusals
// it is refused naming the headroom once less than half of it remains.
func (c *Core) CloneTarget(ctx context.Context, s *Share, id core.FileID, spec core.CloneSpec) (core.Version, error) {
	e := core.Extent{Off: spec.DstOff, Len: spec.Len}
	r := removal{
		class:  capacity.CloneTarget,
		change: core.ChangeCloneTarget,
		kind:   format.KindCloneTarget,
		header: e,
		marker: e,
		spec:   &spec,
	}
	r.bad = cloneShapeError(e, spec)
	return c.remove(ctx, s, id, r)
}

// removal is one of the four removals as remove runs it: the headroom class
// its record draws on, the change its marker is, the record kind it appends,
// the extent its header names, the extent its marker covers and, for a clone
// target, the spec its record and marker carry.
type removal struct {
	class  capacity.RecordClass
	change core.ChangeKind
	kind   format.Kind
	header core.Extent
	marker core.Extent
	spec   *core.CloneSpec
	// bad is the shape refusal the call earned, returned once the file's tag
	// has been checked, so another share's file is refused before its own
	// extent is looked at.
	bad error
}

// toEnd returns the extent from off to the end of the file, the range a
// truncate's and a delete's marker covers.
func toEnd(off int64) core.Extent { return core.Extent{Off: off, Len: math.MaxInt64 - off} }

// remove appends spec's record for file id under s's tag, publishes the
// removal to the placement index, and returns the version it drew once that
// record is durable.
//
// A cancelled ctx is refused first and changes nothing; a handle whose close
// began is refused next, with core.ErrClosed, and a call that runs holds its
// handle's Close back until it returns. The file's tag is checked next, as
// WriteAt's is, so a file another share owns is refused before the removal's
// own shape is looked at — the shape is checked at each public method, ahead
// of this call. The tag is checked again under the file's append domain,
// where the claim is made.
//
// The record draws on the reserved headroom and is never refused by the
// share's limit or the journal's maximum. The order under the domain is
// the one the refusals owe: the headroom's admission, the index's pre-check
// for a removal that splits a held extent, and only then the version draw, so
// a refusal by any of them draws no version and appends nothing. The removal
// is admitted before its record is appended, and applied to the index after,
// so a marker never names a record that is not yet in its segment.
//
// The file's append domain is released before the durability wait: the
// durability worker resolves the file's window under that same domain, so
// holding it across the wait would wait on the worker that waits on this
// call. The wait takes no context and consults no mark: a cancelled ctx after
// the append returns the removal's own answer, not the context's error, and a
// read-only journal answers the refusal bare.
func (c *Core) remove(ctx context.Context, s *Share, id core.FileID, spec removal) (core.Version, error) {
	if err := ctx.Err(); err != nil {
		return core.Version{}, err
	}
	exit, err := s.enter()
	if err != nil {
		return core.Version{}, err
	}
	defer exit()
	if err := c.checkTag(s, id); err != nil {
		return core.Version{}, err
	}
	if spec.bad != nil {
		return core.Version{}, spec.bad
	}
	v, at, err := c.appendRemoval(ctx, s, id, spec)
	if err != nil {
		return core.Version{}, err
	}
	// decision: the wait below must not run with the file append domain
	// held, so this assertion is on every removal. With no recorder
	// installed — production — the call still runs and returns at the nil
	// check, so it costs one branch and no allocation. Its ceiling: it
	// sees only this goroutine's own holds, so it cannot see another
	// goroutine holding the domain, which the wait's own blocking makes
	// moot — this call's domain still held would deadlock on the worker
	// it waits for. The wait's sibling check, AssertNoneHeld on the
	// offload callback, has no caller yet: the offload path that must run
	// it does not exist in this journal, so no recorded run reaches it,
	// and the recorded test of a removal completing while a held callback
	// runs comes with that path.
	c.rec.AssertNotHeld(lockorder.FileAppend, "removal wait")
	// decision: the wait runs with the append domain released and returns its
	// own refusal as it is — a read-only journal's, which matches
	// core.ErrJournalReadOnly and is never a *core.SyncError. A failed wait
	// returns no version, as a failed write does: a read-only journal undoes
	// each unsynced header-only record, so the removal did not happen and the
	// version it drew names nothing. The undo is durability.Config.Undo, which
	// the core does not wire yet: until it does, a read-only failure leaves
	// the marker in the index, and this return is the promise the caller
	// relies on once it is. Wiring Undo, which reverses a header-only record's
	// publish, closes that gap; until then the index can still carry a marker
	// for a removal the caller was told did not happen.
	if err := c.dur.WaitDurable(at); err != nil {
		return core.Version{}, err
	}
	return v, nil
}

// appendRemoval does remove's work under the file's append domain and returns
// the version drawn and the location of the record it appended.
func (c *Core) appendRemoval(ctx context.Context, s *Share, id core.FileID, spec removal) (core.Version, segment.Location, error) {
	unlock := c.domains.Lock(id)
	defer unlock()

	if err := c.checkTag(s, id); err != nil {
		return core.Version{}, segment.Location{}, err
	}
	draw, err := c.headroom.Admit(spec.class)
	if err != nil {
		// The headroom's refusal reaches every class of removal, of held
		// content included: a removal admitted with no draw would be bounded
		// by nothing, and would spend the open reserve the next open's records
		// need. Only the removals that free space are spared the share's limit
		// and the journal's maximum. The read-only refusal is Admit's first
		// check and matches no *core.NoSpaceError, so it is returned here
		// unchanged.
		return core.Version{}, segment.Location{}, err
	}
	// A class answered not-appendable below half the headroom appends nothing:
	// no removal is of such a class today — only an Offloaded record's, which
	// sets its bits in memory — so the refusal is here rather than an append
	// the table forbade. No token was drawn, so nothing is refunded.
	if !draw.Append {
		return core.Version{}, segment.Location{}, &core.NoSpaceError{Limit: core.LimitHeadroom, Missing: 1, Unit: core.UnitEntries}
	}
	if err := c.index.CheckRemoval(id, spec.marker); err != nil {
		c.headroom.Refund(draw)
		return core.Version{}, segment.Location{}, err
	}
	// Drawn through the capacity so a version a reservation holds is not
	// spent here; the file's append domain is already held.
	v, err := c.cap.DrawVersion(ctx)
	if err != nil {
		c.headroom.Refund(draw)
		return core.Version{}, segment.Location{}, err
	}
	seq, err := c.counters.NextSeq()
	if err != nil {
		c.headroom.Refund(draw)
		return core.Version{}, segment.Location{}, err
	}
	var body format.RecordBody
	if spec.spec != nil {
		body.Clone = format.CloneSource{
			Share: spec.spec.SrcShare,
			File:  spec.spec.Src,
			Off:   spec.spec.SrcOff,
			AsOf:  spec.spec.AsOf,
		}
	}
	pl, err := c.streams.For(id).Append(format.RecordHeader{
		Kind:    spec.kind,
		File:    id,
		Extent:  spec.header,
		Seq:     seq,
		Version: v,
		Tag:     s.Tag,
	}, body)
	if err != nil {
		// The record never landed, so the headroom never spent it.
		c.headroom.Refund(draw)
		return core.Version{}, segment.Location{}, err
	}
	c.headroom.Drawn(draw)
	// decision: the removal is applied to the index whatever the allowance
	// says, past its ceiling if it must be. The pre-check above answers the
	// caller before anything is appended, but another file's reservation can
	// pass between it and here; the record is in its segment by now, so a
	// refusal left here would have a rebuild apply the removal the caller was
	// told did not happen. A marker applied past the bound overruns the
	// allowance by the removal's own entries, which the commit's true delta
	// keeps counted, and the next admitted operation sees it. Overturned by
	// reserving the allowance before the append, which needs the entries a
	// removal will add known ahead of the index change that decides them.
	hd, err := c.index.ApplyRemoval(s.Tag, id, spec.change, spec.marker, v, seq, index.RecordAt{Segment: pl.Loc.Segment, Offset: uint32(pl.Loc.Off)}, spec.spec)
	if err != nil {
		// The claim's refusal is unreachable here, the tag having been
		// checked under this same domain a moment ago; the record stays on
		// disk and a rebuild applies what this run could not.
		return core.Version{}, segment.Location{}, err
	}
	c.Account(hd)
	return v, pl.Loc, nil
}
