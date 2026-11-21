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
	//
	// decision: a second Forget of the tag while the first runs is refused
	// with the wrapped ErrHandleOpen, the same answer Share reads. The mark
	// is a set entry the first call clears on its return, so without this
	// gate a second Forget's drop runs under a mark the first already
	// lifted, cutting the writes of a handle a caller attached in the gap —
	// and the engine's calls are serialised per share, not per journal, so
	// nothing but the gate holds the two apart. The refusal is honest to
	// retry: it ends with the first call's return, the engine's start
	// retrying whatever time the forget took. Overturned by the forgetting
	// mark counting calls instead of naming tags, which would let a second
	// wait behind the first rather than answer at once.
	c.registryMu.Lock()
	if p := c.registry.Load(); p != nil {
		for _, s := range *p {
			if s.Tag == tag {
				c.registryMu.Unlock()
				return core.ErrHandleOpen
			}
		}
	}
	if c.isForgetting(tag) {
		c.registryMu.Unlock()
		return fmt.Errorf("datapath: a forget of tag %d is in progress: %w", tag, core.ErrHandleOpen)
	}
	c.markForgetting(tag)
	c.registryMu.Unlock()
	defer c.clearForgetting(tag)

	// Every file the tag claims is dropped: the file it owns with no piece
	// and no marker left — every extent settled away, its records still
	// riding their segments — is claimed no less than a held one, and a
	// rivalled file is claimed by whichever side the forget must clear; both
	// enter the barrier and the drop, so the running journal leaves the same
	// file claimed as a reopen would.
	//
	// decision: a tag nothing holds anywhere — no record byte in any
	// segment, no file it claims, and no deferred loss the registry owes
	// for it — is answered nil with no record appended, because a forget
	// record of a tag that holds nothing can change nothing recovery would
	// serve, and a start that names every forgotten tag again would make one
	// record per tag of no effect. The registry ask is part of the census
	// because a deferred loss is something the journal holds for the tag
	// whatever the set and the index answer: its record is owed to whatever
	// copy retires its segment, an unsynced record's bytes never entered the
	// set's counters, and the answer nil would leave the tag's latch — and
	// the record a later pass writes under it — standing behind a caller
	// told the tag holds nothing. The ask is taken once here and the sweep
	// below spends it: the registry gains entries only on the open's own
	// loss pass, never beside a running core, so the one census serves both.
	// The ceiling stays: an unlisted stranding the call still leaves — the
	// answer takes the tag out of the stranded set, which needs no record,
	// since the set is memory the caller's next start re-makes. Overturned
	// by a caller the journal owes a durable receipt of the forget to.
	files := c.index.FilesClaimedBy(tag)
	// decision: the one census taken here serves both the emptiness check
	// above and the deferral sweep below, sound only while the open's own
	// loss pass is the registry's single writer: no path beside it raises a
	// deferral while a core runs, so the answer cannot go stale between
	// its two consumers. Overturned by a second writer of the registry —
	// any path that defers a loss outside the open's loss pass — when each
	// consumer must take its own read.
	deferred := c.dur.DeferredOfTag(tag)
	if len(files) == 0 && c.set.ShareBytes(tag) == 0 && len(deferred) == 0 {
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
	c.headroom.Drawn(draw, pl.Loc.Segment)
	c.cap.Landed(tag, pl.Loc.Len)

	// The forget record's liveness, registered the moment its bytes landed:
	// the registration's own contract counts the record before its segment
	// can seal — the removals register right beside their drawn append — and
	// a record appended but unregistered leaves a window its segment may
	// seal, retire as not-held and take with it, a crash handing the
	// tracker back a durable forget nothing answered for while a below
	// segment still stands with the tag's records for a rebuild to apply.
	// The snapshot was taken at the barrier: the record is live while any
	// segment that held the tag's records below it remains, so no retirement
	// frees the record's own segment early, and every later error path — a
	// refused drop, a marks refusal — still finds the record registered.
	forgetRec := reclaim.Rec{
		Kind: format.KindForget,
		Seg:  pl.Loc.Segment,
		Pair: index.Numbers{Seq: seq},
		Seq:  seq,
		Tag:  tag,
		Loc:  pl.Loc,
	}
	c.liveness.RegisterForget(forgetRec, below)

	// decision: nothing is dropped before the record is durable, so a
	// failed window — the record unsynced, its segment ended — answers the
	// error with the tag whole: the in-memory index needs no undo for a
	// record that never became publishable, and a crash finding the record
	// undurable drops it and rebuilds the tag's extents exactly as they
	// were. Overturned by the durability layer gaining an undo for an
	// appended-but-unsynced header-only record, which would allow a drop
	// beside a failed one and a re-issuable forget in the same window.
	if err := c.dur.WaitDurable(pl.Loc); err != nil {
		// The refusal takes the registration back with it: the record drew
		// live the moment its bytes landed, but a caller answered with a
		// failure owns nothing it told the tag left whole. What a leaked
		// registration would cost is its home — the failure ends the
		// segment, and the read-only fence a persistent failure raises
		// stops every pass for the journal's life, so a live count nothing
		// resolves stays held for ever. The failed record answers droppable
		// once unregistered — its snapshot left with the registration — so
		// the re-learn a later unit runs keeps it dead, and the home retires
		// with the record's bytes uncarried once the journal serves again.
		//
		// decision: a refused forget unregisters its record, so a caller
		// told the forget failed owns nothing registered. One channel
		// stays open: a one-shot sync failure, raising no read-only
		// fence, lets the window resolution re-append the record durably
		// behind the error, and it then binds on disk. Close this
		// channel once the re-append suppresses a record whose forget
		// answered an error, or an admission-level undo owns it.
		c.liveness.Unregister(forgetRec)
		return err
	}

	// The drop, per file under its append domain, the index held for the
	// write: the delta runs through Account as any index change does, and
	// the segments whose held bytes fell are collected for the offers after
	// the loop. One domain at a time, like the barrier: R46's rule keeps the
	// two halves of the forget from holding what a repack copy of another
	// file could be waiting on.
	var named []core.SegmentID
	for _, f := range files {
		unlock := c.domains.Lock(f)
		hd, nm, derr := c.index.ForgetFile(f, seq, tag)
		if derr != nil {
			unlock()
			return fmt.Errorf("datapath: forgetting file %v: %w", f, derr)
		}
		c.Account(hd)
		if terr := c.index.ForgetTagMarks(f, tag); terr != nil {
			unlock()
			return fmt.Errorf("datapath: forgetting the marks of file %v: %w", f, terr)
		}
		named = append(named, nm...)
		unlock()
	}

	// The tag's deferred losses end here, by the registry's own tag census
	// — not the set's figures, which cannot guide this sweep: a deferral
	// can sit against a segment whose per-tag bytes read zero, the crash
	// shape where the dropped record's bytes never entered the counters,
	// and the census took it at the predicate above. A pass would
	// otherwise write the loss record above the forget's, under a tag the
	// caller was told was gone; the forget discards the content the loss
	// was owed for, so the registry entry goes without its record, its
	// latch on the segment with it. Entries a concurrent pass already
	// carried answer false and change nothing.
	for seg, ls := range deferred {
		for _, l := range ls {
			if c.dur.EndDeferral(seg, l) {
				c.liveness.SetDeferred(seg, false)
			}
		}
	}

	// The re-test the marker clear owes: the drop removed every marker of
	// the tag's files without the settle a removal waits for, so their
	// registered removal records — live while their markers stand — release
	// here as Settle's relay releases them. It runs after every file's own
	// drop, holding no append domain, which no file of the tag is waiting
	// on. Before the re-test the offers below would read the segments still
	// held and skip them.
	c.liveness.Retest(files...)

	// The emptied segments are offered to the retirement worker with the
	// forget record as the causality record the offer's gate reads — the
	// record is durable already, so the gate passes at once, and the
	// deferrals, the re-test and the drops above have all let go.
	for _, seg := range named {
		if !c.liveness.Held(seg) {
			c.retiring.Emptied(seg, pl.Loc)
		}
	}

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
