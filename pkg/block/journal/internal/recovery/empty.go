package recovery

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"slices"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/durability"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/format"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/index"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/reclaim"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/segment"
)

// Emptying is the open's emptying phase, the one the phases list covers with
// a span-only entry its loop runs as a no-op: the phase's work waits for the
// open's loss records, which the loss pass appends and syncs only after the
// assembled journal exists — no phase of the recovery itself can run that
// pass — so the work runs here, where the open stands past it, before the
// journal serves and before the retirement worker starts (the
// loss records first, then every sealed segment that is not held is
// retired, then the ended segments are emptied, the unheld ones first).
//
// The span the phases list opens covers the recovery's own end; the work of
// the phase opens it once more here, so the trace of an open shows both: the
// named no-op where the phase sits in the list, and the real span its work
// ran under. The two-step shape of the list's one entry — retiring first,
// emptying the ended segments after, each retirement refilling the reserve
// before the next ended segment's work draws a spare from it — lives in this
// file: the retire step runs first, and the emptying step the list's
// span also names joins it here.
func Emptying(ctx context.Context, deps Deps, st *State, expect core.JournalID, floor core.Version, marks []core.SyncMark) (err error) {
	if err := ctx.Err(); err != nil {
		return err
	}
	ctx, end := startSpan(ctx, deps, core.SpanOpenEmptying)
	defer func() { end(err) }()
	rd := newReader(ctx, deps.FS, deps.Clock, deps.Log)
	empty := &emptying{deps: deps, rd: rd, st: st, identity: expect, marks: marks}
	if err := empty.runRetire(ctx); err != nil {
		return err
	}
	// ponytail: a freed take no copy drew by the end of the emptying is not
	// handed back to the pool: it stays a record-less cap-sized file the next
	// open's spares phase re-adopts. Upgrade to returning it to the pool when
	// the pool wiring can adopt a take mid-run.
	return empty.runEmptying(ctx)
}

// runRetire is the emptying's first step: every sealed segment the liveness
// tracker answers not held is retired, through the retirement path
// the assembled journal runs — recycle or unlink, the pool's refill after
// each included — before any ended segment is emptied.
//
// The order this step runs in — past the loss pass, whose records are
// durable before the journal it assembled reaches here — is what lets a
// retiree go: a segment may hold the only copy of a record the crash
// corrupted, and the mask its loss record makes is on disk before the
// segment's unlink takes the record with it. Nothing is asserted of that
// order here: the caller hands the state the loss pass already gated on.
func (e *emptying) runRetire(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if e.st.Assembled.Set == nil {
		return errors.New("recovery: the emptying's state carries no assembled ledger, so nothing is a retirement's to take")
	}
	if e.deps.Liveness == nil || e.deps.Retire == nil {
		return errors.New("recovery: the emptying's deps carry no liveness tracker or retirement path: the open hands the ones the journal it assembled runs")
	}
	// The deferred-loss registry, read the way the open reads the failed
	// list: a segment the registry answers as holding a deferred loss's
	// dropped record is never retired here, for its copy has not been made,
	// and retiring it would take the record the copy the deferral owes its
	// own record to would read.
	deferred, err := durability.DeferredFor(e.deps.Dir, e.identity)
	if err != nil {
		return err
	}
	for _, row := range e.st.Assembled.Set.Snapshot() {
		var reason segment.Reason
		switch row.State {
		case segment.Sealed:
			reason = segment.NotHeld
		case segment.Ended:
			// decision: an ended segment is never retired here, held or not:
			// the emptying step owns every one. An unheld ended segment may
			// hold the one good copy of a record whose duplicate the index
			// serves in its place, and only the emptying reads that duplicate
			// whole before the segment goes; with nothing to carry, the
			// emptying retires the segment through the same call this step
			// would. Withdrawn if the walk ever verifies every payload, so a
			// served duplicate is proven whole before the open reaches here.
			continue
		default:
			// decision: an Active row is never a retirement's candidate,
			// whatever its bytes hold — the retirement path itself refuses
			// the pair, and this step widens it by nothing: a take withheld
			// over a predecessor the open ends stands until that predecessor
			// is unlinked with a synced unlink; a live row holds the records
			// a stream still appends beside. The pool's own spares are not
			// among the ledger's rows at all — the spares the spares phase
			// re-adopted among them, which the pool adopts before it starts
			// — so a spare the retirer recycled during this very step, the
			// reserve's refill, is not taken either.
			continue
		}
		if e.deps.Liveness.Held(row.ID) {
			continue // the tracker's answer is the whole candidate test: no superset beside it
		}
		if len(deferred[row.ID]) > 0 {
			// decision: a segment the deferred-loss registry names holds a
			// record the copy that retires the segment owes the deferral a
			// loss record of, so no retirement may take the record here
			// before that copy exists. The registry is live in this process —
			// the loss pass defers the corrupt drops the open reserve cannot
			// hold, and a running journal's own Lose defers on its headroom
			// — and the guard reads it the way the open reads the failed
			// list: an ended holder travels on to the emptying step, which
			// writes the deferred record into its copy and ends the
			// deferral; a sealed holder simply stands until a later open's
			// copy or the deferral's own resolution retires it.
			continue
		}
		// The doorstep of the retirement's writes: a cancellation that
		// already landed leaves the segment standing and the next open
		// re-reads and re-decides, but no unlink begins past it.
		if err := ctx.Err(); err != nil {
			return err
		}
		ret, err := e.deps.Retire(row.ID, reason)
		if err != nil {
			return err
		}
		if ret.Durable {
			// A retirement the directory sync proved durable frees the takes
			// naming the retired segment, the same relief the emptying's own
			// unlinks give: the emptying step below draws them before the
			// pool's reserve.
			e.freed = append(e.freed, e.freedTakes(row.ID)...)
		}
	}
	return nil
}

// runEmptying is the emptying's second step: each segment the open ended —
// the rows the assembled ledger answers Ended — is copied out and unlinked,
// one at a time. The order the steps run in is the whole contract: the loss
// pass makes its records durable first, so each corrupt record is masked
// before any segment's file is unlinked. A segment may hold the only copy of
// a record; unlinked before its mask, an older copy would serve in its place.
//
// decision: a segment recovery ends is emptied whole at this open, never
// re-read as a live row: an ended segment takes no append and no seal, so a
// journal that kept it would count its cap against every limit until some
// later open re-decided its fate on the same bytes. Copying its survivors
// out and unlinking it here is what makes that one open's word final.
// Withdrawn if an ended segment is ever ruled to stand past the open that
// ended it.
func (e *emptying) runEmptying(ctx context.Context) error {
	var ended []core.SegmentID
	for _, row := range e.st.Assembled.Set.Snapshot() {
		if row.State == segment.Ended {
			ended = append(ended, row.ID) // rows ascend, so ended does too
		}
	}
	// The sealed segments holding a failed record the open reserve could not
	// take join the ended ones: each is emptied the same way, the failed
	// records left out of the copy because the index serves none of them.
	// An ended segment already goes through its own leg, so none is listed
	// twice.
	for _, drop := range e.st.Kinds.PastReserve {
		for _, id := range drop.Segments {
			if state, err := e.st.Assembled.Set.State(id); err == nil && state == segment.Sealed {
				ended = append(ended, id)
			}
		}
	}
	slices.Sort(ended)
	ended = slices.Compact(ended)
	if len(ended) == 0 {
		return nil
	}
	// The ended segments nothing holds go first: the retire step leaves them
	// here only so their served duplicates are read whole, and one with
	// nothing to carry retires and refills the reserve before a held one's
	// copy draws from it, the order the retire step itself keeps.
	slices.SortStableFunc(ended, func(a, b core.SegmentID) int {
		return cmp.Compare(boolRank(e.deps.Liveness.Held(a)), boolRank(e.deps.Liveness.Held(b)))
	})
	if e.deps.DrawReserve == nil {
		return errors.New("recovery: the emptying's deps carry no reserve draw: the open hands the pool's own take of the repack reserve")
	}
	// The deferred-loss registry, read the way the retire step read it: each
	// entry against a segment the open ends becomes the loss record the copy
	// below writes and this step ends. The read is a snapshot, exactly as
	// the retire step's is; nothing defers between the two, for no Lose of
	// the journal the open built runs inside the open.
	deferred, err := durability.DeferredFor(e.deps.Dir, e.identity)
	if err != nil {
		return err
	}

	for _, id := range ended {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := e.oneSegment(ctx, id, deferred[id]); err != nil {
			return err
		}
	}
	return nil
}

// boolRank orders false before true.
func boolRank(b bool) int {
	if b {
		return 1
	}
	return 0
}

// emptying carries the state one run of the emptying step shares across the
// ended segments it walks: the spares the durable unlinks freed — the takes
// naming a predecessor the emptying unlinked, which the spares phase's own
// verdict held back — are drawn before the pool's reserve, but only when the
// take's id is above every segment the copy replaces: a copy below a rotten
// twin it replaces would not outlive the next open.
type emptying struct {
	deps     Deps
	rd       *reader
	st       *State
	identity core.JournalID
	marks    []core.SyncMark

	// freed holds the takes a durable unlink released, in the order they were
	// freed; a copy draws the first one above its id floor (drawAbove). A
	// freed take is the emptying's own spare alone: the pool adopts the
	// spares the spares phase re-adopted, before it starts, but a take freed
	// here comes after the start and never reaches it — an undrawn one
	// stays a ledger row the next open re-adopts.
	freed []core.SegmentID

	// carried keys every aside record this run already copied into a sealed
	// target: the skip counts this step's own new segments, so a duplicate a
	// later segment holds is not carried a second time.
	carried map[asideKey]bool
}

// asideKey is what makes two aside records copies of one another: the
// standing test asideCopyVerifies matches on the same fields.
type asideKey struct {
	kind format.Kind
	seq  core.Seq
	file core.FileID
	ext  core.Extent
}

// carriedCopy is one record travelling to the copy target: the input the
// copier takes, the repoint the record's served pieces need once the copy
// landed, and the registration an aside or loss record carries — its copy
// re-registered at the placement the copier answered, exactly as the
// journal's own carry places one. reg is zero for a content record; file
// is the record's own file across all three shapes.
type carriedCopy struct {
	in      copyInput
	file    core.FileID
	repoint bool
	// from is the placement whose served pieces move to the copy: the
	// record's own, or a rotten twin's the index served in its place.
	from index.RecordAt
	reg  reclaim.Rec
}

// lossWrite is one loss record the emptying of one ended segment owes: the
// loss itself, the record's encoded bytes and its drawn sequence number
// where the copy writes it, durable alone where a record on disk already
// names it, and the registration the copy carries. A deferred entry — one
// the registry named against the segment — ends its deferral at finish; a
// read-found loss is no deferral's to end.
type lossWrite struct {
	l        durability.Loss
	at       []byte
	seq      core.Seq
	rec      reclaim.Rec
	durable  bool
	deferral bool
}

// oneSegment copies the ended segment id out and unlinks it: the records
// repack would carry forward — each payload verified whole before it is
// copied, numbers kept, never merged — through the copier into one sealed
// spare; the loss record each deferred loss against the segment is owed; the
// loss record of each record that fails only on this read with no other
// verifying copy; then the retirement's unlink with the directory sync it
// carries. A record whose copy already verifies in a segment the open
// neither ends nor saw fail is skipped and its served pieces repointed
// there: that segment stays held. A second refusal from the copier, or no
// spare, fails the open with that error, leaving the segment as it stands.
func (e *emptying) oneSegment(ctx context.Context, id core.SegmentID, deferred []durability.Loss) error {
	st, deps := e.st, e.deps
	// The segment's own walk, read once more: the rebuild's scan keeps what
	// its later phases read, and the per-record conversions the feed charged
	// are not among them, so the emptying walks the ended segment once and
	// reads each payload whole — the one read-back this open makes of it,
	// never repeated once the segment is gone.
	sc, serr := scanSegment(e.rd, e.identity, SegmentInfo{ID: id})
	if serr != nil && !isContextErr(serr) {
		// Cannot happen: the same walk read the segment at the rebuild, but
		// a reread the bound did not clear refuses the open on its ranges
		// like any read the pass makes — never judged on bytes it did not
		// return.
		if ranges := e.rd.Unreadable(); len(ranges) > 0 {
			return &core.UnreadableRangesError{Ranges: ranges}
		}
		return serr
	}
	if serr != nil {
		return serr
	}

	// The moves the served pieces need: one per carried payload to the
	// placement the copier answers, and one per skipped record to its
	// standing copy.
	type moveAt struct {
		file core.FileID
		from index.RecordAt
		to   index.RecordAt
	}
	var moves []moveAt
	var losses []lossWrite

	// The proofs a read-found loss is synced by, the classing's own rule for a
	// dropped record: the segment's seal, a successor naming it, a record's
	// synced-through past it, or a sync mark's.
	named := successorSet(st.Attached.Own)
	through := make(map[core.SegmentID]int64, len(e.marks))
	for _, m := range e.marks {
		// The greatest Through proves the most, as the marks raise tests each mark.
		through[core.SegmentID(m.Segment)] = max(through[core.SegmentID(m.Segment)], m.Through)
	}

	var ins []carriedCopy
	// The content records: a payload kind whose placement still serves a
	// piece, read whole and verified; a record whose copy verifies in a
	// standing segment is not copied but its served pieces move there.
	for _, rec := range sc.Records {
		if rec.Kind != index.RecWrite && rec.Kind != index.RecFill {
			continue // header-only: the registrations below carry those, never payloads
		}
		at := index.RecordAt{Segment: id, Offset: rec.At}
		from := at
		if !st.Rebuilt.Index.Names(rec.File, at) {
			// Repack carries nothing the index answers for from this
			// placement — no held piece and no retained one, the limit's
			// own kept extents included — so the record answers droppable by
			// every rule the index keeps, and it goes with the segment. The
			// one exception is a duplicate the index serves in its place
			// whose payload does not verify: a skip stands only on a copy
			// that verifies whole, so this copy is carried and the served
			// pieces move from the rotten twin to it.
			twin, rots, terr := e.rottenTwin(rec, id)
			if terr != nil {
				return terr
			}
			if !rots {
				continue
			}
			// decision: the rot branch always carries, even when another
			// sealed copy verifies.
			//
			// Why: the index serves the greatest copy among the kept
			// segments, so any other verifying copy sits below the rotten
			// twin. Repointing to it holds its segment only for this
			// process; the next open's walk verifies no payload and serves
			// the twin again, and the lower copy, held by nothing for this
			// record, goes once its other records are superseded. A carry
			// lands in a spare drawn above the twin (drawAbove), so the next
			// open serves it and retires the stale duplicate as unheld; the
			// duplicate lasts one open.
			//
			// The pool's reserve holds the spares an open re-adopts, whose
			// old ids can sit below the twin; the draw passes the twin as its
			// floor, so the reserve hands one above it, renaming a low one.
			//
			// Withdrawn when the walk verifies payloads or the open records a
			// rotten placement, so a restart cannot serve the twin again.
			from = twin
		} else {
			other, found, serr := e.standingCopy(rec, id)
			if serr != nil {
				return serr
			}
			if found {
				// The copy already verifies whole in a segment the open
				// neither ends nor saw fail, so the bytes live on there:
				// nothing is copied and the served pieces move to the
				// placement that stays.
				moves = append(moves, moveAt{file: rec.File, from: at, to: other})
				continue
			}
		}
		bytes, refused := e.readWhole(id, int64(rec.At))
		synced := sc.Sealed || named[id] || int64(rec.At) < sc.SyncedThrough || int64(rec.At) < through[id]
		if refused != nil {
			return refused
		}
		if bytes == nil {
			// The record failed only on this read — its header verified at
			// the open's walk, and the whole the copy leg reads does not. A
			// loss record is written only while no other copy verifies, so
			// every other fed copy is read first, an ended segment's this run
			// has not emptied yet included: the first that verifies is
			// carried into this segment's target in the rotten one's place,
			// and the served pieces move to the carry.
			//
			// decision: the verifying copy is carried rather than the served
			// piece repointed to it where it sits. A sealed copy can sit
			// below a rotten twin the index serves, and the next open would
			// serve the twin again over a repoint, its walk verifying no
			// payload; an ended copy carried here is one its own leg then
			// finds served from the carry and skips. The duplicate left
			// behind is the next open's to retire as unheld. Withdrawn when
			// the walk verifies payloads, which makes a repoint survive a
			// restart.
			if other, b, found, derr := e.verifyingDuplicate(rec, id, true); derr != nil {
				return derr
			} else if found {
				ins = append(ins, carriedCopy{in: copyInput{
					Bytes: b, Segment: other.Segment, At: int64(other.Offset),
					Seq: rec.Numbers.Seq, Version: rec.Numbers.Version,
				}, file: rec.File, repoint: true, from: from})
				continue
			}
			// No other copy verifies: the loss record rides the copy, and the
			// pieces the record served read lost, never older bytes and never
			// a placement the unlink takes away.
			//
			// decision: the record's served extents leave the index here,
			// the same cut the data path's corrupt drop makes, for the
			// segment is about to go and a served piece pointing into an
			// unlinked file answers a read with the file's absence, not the
			// loss the mask records. For a record no sync proved the cut
			// errs the strict way: it masks the extent caller-side too,
			// which the reveal of an unsynced read-found loss would need
			// its own rule to unmask. The generation, the counters and the
			// event are the next open's found record's own, published when
			// its kinds pass places it. For a record no sync proved, this
			// open's ring holds no event and the extent reads missing, for
			// nobody could have read it. For a synced one the extent reads
			// Lost and Since reports it in this process, but the ring event
			// and the generation come at the next open, where the placement
			// publishes them. Withdrawn if a ruled loss record of the
			// running window must raise them at the open that found the rot.
			if _, _, lerr := st.Rebuilt.Index.LoseSuperseded(rec.File,
				core.Extent{Off: rec.Off, Len: rec.Len}, rec.Numbers.Version, rec.Numbers.Seq); lerr != nil {
				return lerr
			}
			if synced {
				// A record a seal, a successor, a synced-through or a mark
				// proves synced is a synced loss: its extent reads lost, the
				// marker the next open's own cut leaves for the same record,
				// so no older copy is served over it in this process or the
				// next.
				if _, _, lerr := st.Rebuilt.Index.Lose(rec.File,
					core.Extent{Off: rec.Off, Len: rec.Len}, rec.Numbers.Version); lerr != nil {
					return lerr
				}
			}
			lw, lerr := e.newLossWrite(durability.Loss{
				Share:   rec.Tag,
				File:    rec.File,
				Extent:  core.Extent{Off: rec.Off, Len: rec.Len},
				Version: rec.Numbers.Version,
				Seq:     rec.Numbers.Seq,
				Reason:  core.LossCorrupt,
				Synced:  synced,
				AtOpen:  true,
				// The record's own segment carried the loss's bytes in
				// transport, and the read-found loss names no deferral the
				// holder would have to stand for.
				Seg: id,
			})
			if lerr != nil {
				return lerr
			}
			losses = append(losses, lw)
			continue
		}
		ins = append(ins, carriedCopy{in: copyInput{
			Bytes: bytes, Segment: id, At: int64(rec.At),
			Seq: rec.Numbers.Seq, Version: rec.Numbers.Version,
		}, file: rec.File, repoint: true, from: from})
	}
	// The aside records the tracker registered live: judged by the tracker's
	// own drop test, a record that answers droppable is dead weight the
	// retirement drops, and a copy of the rest registers at the placement
	// the copier answers; the forget snapshot rides with its record.
	for _, rec := range asideRecs(st) {
		if rec.Seg != id {
			continue
		}
		if deps.Liveness.Droppable(rec) {
			continue // a loss, a release or a marker the registry and the index already answer dead
		}
		if e.carried[asideKey{rec.Kind, rec.Seq, rec.File, rec.Ext}] || e.asideCopyVerifies(rec, id) {
			continue // another copy verifies in a standing segment or this run's own target: nothing is carried
		}
		bytes, refused := e.readWhole(rec.Loc.Segment, rec.Loc.Off)
		if refused != nil {
			return refused
		}
		// decision: an aside record whose reread does not verify fails the
		// open rather than trading for a loss record — a loss names a
		// dropped record and never a marker, a forget or a release — and the
		// segment stands for the next open to re-read. Overturned by a ruled
		// loss record for a rotten removal record, which the leg above would
		// then write in place of the refusal.
		if bytes == nil {
			return fmt.Errorf("%w: the record of seq %d at %d of segment %d does not decode or verify on the emptying's read", core.ErrOpenRefused, rec.Seq, rec.Loc.Off, id)
		}
		ins = append(ins, carriedCopy{in: copyInput{
			Bytes: bytes, Segment: id, At: rec.Loc.Off,
			Seq: rec.Seq, Version: rec.Pair.Version,
		}, file: rec.File, reg: rec})
	}
	// The deferred losses the copy leg owes: each becomes the loss record
	// its deferral promised, written into the copy and ending its deferral
	// once durable; an entry a record on disk already names ends its
	// deferral alone.
	for _, l := range deferred {
		if e.lossOnDisk(l) {
			losses = append(losses, lossWrite{l: l, durable: true, deferral: true})
			continue
		}
		lw, lerr := e.newLossWrite(l)
		if lerr != nil {
			return lerr
		}
		lw.deferral = true // the record the deferral owes: it ends once the copy holds it durable
		losses = append(losses, lw)
	}

	// One spare for the whole segment: the copy is one batch or none, so a
	// half-copied segment never stands. The synthesized loss records ride
	// the same target — the copier's spare, never the headroom.
	//
	// ponytail: in a copy a loss record takes its real bytes, 184 — a
	// 96-byte header, a 16-byte payload and a 72-byte footer entry — against
	// at least 168 for the header-only record it replaces, so a segment of
	// nothing but losses can need a tenth more than one spare's cap, and the
	// copy's one-cap fit refuses it as a wedge of the open. Upgrade to the
	// packing loop that seals a full target and draws another when a copy
	// heavy in loss records is seen in the field.
	for i := range losses {
		if losses[i].at == nil {
			continue
		}
		ins = append(ins, carriedCopy{in: copyInput{
			Bytes: losses[i].at, Segment: id, At: 0,
			Seq: losses[i].seq, Version: losses[i].l.Version,
		}, file: losses[i].l.File, reg: losses[i].rec})
	}

	repoint := func() error {
		for _, mv := range moves {
			if _, _, rerr := st.Rebuilt.Index.RepointRecord(mv.file, mv.from, mv.to); rerr != nil {
				return fmt.Errorf("repointing the copy of the record of file %v from segment %d: %w", mv.file, id, rerr)
			}
		}
		return nil
	}
	if len(ins) == 0 {
		// The skipped records' served pieces move to their standing copies
		// even when nothing is carried, or the unlink would take the
		// placement they serve.
		if err := repoint(); err != nil {
			return err
		}
		// Nothing is carried: the segment holds nothing the index and the
		// tracker answer live, so the unlink takes dead weight and the
		// deferrals below end on the records already durable.
		return e.finish(ctx, id, 0, losses)
	}

	// A carry in a twin's place must land above the twin, or the next
	// open serves the twin over it again.
	var floor core.SegmentID
	for i := range ins {
		if ins[i].repoint && ins[i].from.Segment != id {
			floor = max(floor, ins[i].from.Segment)
		}
	}
	c := &copier{deps: deps, identity: e.identity, draw: func() (core.SegmentID, error) { return e.drawAbove(floor) }}
	natural := make([]copyInput, len(ins))
	for i := range ins {
		natural[i] = ins[i].in
	}
	target, at, err := c.copy(ctx, natural)
	if err != nil {
		return err
	}
	// The sealed target, sealed in the ledger the continuation runs: the
	// emptying runs past the open's core build, so the row the pool's take
	// or a freed take drew Active is sealed here, the same shape the open's
	// own continuation gives the re-append's target.
	if err := st.Assembled.Set.Seal(target, true); err != nil {
		// A ledger refusal is the rows' own state, read again the same way
		// by a retry.
		return fmt.Errorf("%w: sealing the copy target %d in the ledger: %w", core.ErrOpenRefused, target, err)
	}
	// A draw that failed before the one that landed left a row the draw
	// took and a file of half-written records: the row ends, for no stream
	// may append into bytes no marker speaks for, and the next open re-reads
	// the file and settles it.
	for _, failed := range c.taken {
		if failed == target {
			continue
		}
		if err := st.Assembled.Set.End(failed); err != nil {
			return fmt.Errorf("%w: ending the failed copy target %d in the ledger: %w", core.ErrOpenRefused, failed, err)
		}
		endSegment(st, failed)
	}
	// The repoints: the carried payloads move to the placements the copier
	// answered, and the skipped ones have moved to their standing copies.
	for i := range ins {
		if !ins[i].repoint {
			continue
		}
		moves = append(moves, moveAt{
			file: ins[i].file,
			from: ins[i].from,
			to:   index.RecordAt{Segment: target, Offset: uint32(at[i])},
		})
	}
	if err := repoint(); err != nil {
		return err
	}
	// The carried registrations, notes and loss records: each copy registers
	// at the placement the copier answered — the journal's own carry's
	// shape, the forget snapshot riding with its record — and the index
	// notes the extents the copies cover, so none is ever on disk unnoted.
	for i := range ins {
		rec := ins[i].reg
		if rec == (reclaim.Rec{}) {
			continue
		}
		cp := rec
		cp.Seg = target
		cp.Loc = segment.Location{Segment: target, Off: at[i], Len: int64(len(ins[i].in.Bytes))}
		if rec.Kind == format.KindForget {
			deps.Liveness.RegisterForget(cp, forgetSnapshot(st, rec))
		} else {
			deps.Liveness.Register(cp)
		}
		if e.carried == nil {
			e.carried = make(map[asideKey]bool)
		}
		e.carried[asideKey{rec.Kind, rec.Seq, rec.File, rec.Ext}] = true
		if rec.File != (core.FileID{}) {
			if nerr := st.Rebuilt.Index.NotePlaced(rec.Tag, rec.File, target, coveredRec(rec), rec.Pair); nerr != nil {
				return nerr
			}
		}
	}
	return e.finish(ctx, id, target, losses)
}

// finish completes one ended segment's emptying: the deferrals end — each one
// whose record rode the copy, sealed and synced where it did, and each one a
// record on disk already named — and the retirement itself unlinks the
// segment, its directory sync making the removal durable. A durable unlink
// frees the takes naming the segment, which the next ended segment's copy
// draws on first. target names the copy that landed, zero when nothing
// was carried.
func (e *emptying) finish(ctx context.Context, id core.SegmentID, target core.SegmentID, losses []lossWrite) error {
	for _, lo := range losses {
		if !lo.deferral {
			continue // a read-found loss is no deferral's to end: the record rides the copy
		}
		if e.deps.EndDeferral == nil {
			if lo.durable {
				// A deferral whose record an earlier open's copy already
				// made durable ends through nothing this open wrote: the
				// entry stands for the next open's own end, itself safe.
				continue
			}
			return errors.New("recovery: the emptying's deps carry no deferral end: the open hands the durability layer's own")
		}
		e.deps.EndDeferral(id, lo.l)
	}
	// The retirement's doorstep: a cancellation that already landed leaves
	// the segment standing with its copy durable — the next open re-reads
	// the same bytes and settles them the same way — but no unlink begins
	// past it.
	if err := ctx.Err(); err != nil {
		return err
	}
	reason := segment.ReasonEnded
	if state, serr := e.st.Assembled.Set.State(id); serr == nil && state == segment.Sealed {
		// decision: a sealed holder retires through the same call whether
		// the retirer unlinks it or, full-size with the pool short, recycles
		// it into a spare. Both paths run only here, after the copy is
		// sealed and synced, and a recycle zeroes the header and the trailer
		// after its durable rename, so the failed records stay behind no
		// header any scan would read. Withdrawn if a recycle ever keeps a
		// header it could be re-scanned under.
		reason = segment.NotHeld // a sealed holder of a failed record, emptied before it is unheld
	}
	ret, err := e.deps.Retire(id, reason)
	if err != nil {
		return err
	}
	if !ret.Durable {
		// decision: a removal the directory sync never proved durable frees
		// no take, for a power cut may resurrect the predecessor and its
		// naming header is the evidence the next open's classing reasons
		// from. The retirement itself is done — the retirer's own word, with
		// its durable flag — and the next open's scan settles what the
		// failed sync left.
		return nil
	}
	e.freed = append(e.freed, e.freedTakes(id)...)
	return nil
}

// drawAbove hands the copier one spare the copy may take whose id is above
// floor: the first take a durable unlink freed that clears it, the others
// left for later copies, else the pool's own repack reserve through the
// closure the open wired, handed the same floor: its ready spares include the
// ones the open re-adopted under their old ids, so when none clears the floor
// it renames one above every id the directory held, needing no new space.
// floor is zero for a copy that carries nothing in a twin's place.
//
// decision: a copy lands above every copy it replaces. The rule: the rebuild
// serves the greatest placement among the kept copies of a record and
// verifies no payload, so a carry in a rotten twin's place must take an id
// above the twin and must stay held until the older copies are gone, or the
// next open serves the twin again. The hazard: a freed take
// below the floor would hold the only verifying copy under the twin, which
// the next open serves rotten; so such a take is skipped, never drawn, and a
// low reserve spare is drawn only under a fresh id above the floor. When
// every freed take is below the floor and the pool's reserve is empty, the
// draw answers the
// pool's spare refusal, which the copier returns as the pool
// gave it: the open fails, the ended segment stands, and the copy is never
// placed low. A freed take at or below the floor is only skipped, not renamed
// above it as the reserve's low spare is, so at the maximum an empty reserve
// still refuses where the same rename could serve the take. Withdrawn when
// the rebuild verifies payloads, or the open records a rotten placement so a
// restart cannot serve it again.
func (e *emptying) drawAbove(floor core.SegmentID) (core.SegmentID, error) {
	for i, id := range e.freed {
		if id > floor {
			e.freed = slices.Delete(e.freed, i, i+1)
			return id, nil
		}
	}
	return e.deps.DrawReserve(floor)
}

// freedTakes answers the takes a durable unlink of predecessor freed, in id
// order: each withheld take — one the spares phase held back over the verdict
// its marks raise computed, its header the one that names the predecessor —
// that still answers the record-less test passes as a spare again, its file
// cap-sized with no verifying record and no seal, and no mark naming it
// (the relief; the verdict the raise computed decides which takes were
// held). The take is adopted as the emptying's own spare, copies overwriting
// the header, the trailer and the record slots its records would take, so
// none of the old life's bytes outlives a copy into it.
func (e *emptying) freedTakes(pred core.SegmentID) []core.SegmentID {
	st := e.st
	marked := make(map[core.SegmentID]bool, len(e.marks))
	for _, m := range e.marks {
		marked[core.SegmentID(m.Segment)] = true
	}
	var out []core.SegmentID
	for _, seg := range st.Attached.Own {
		if seg.Header.Predecessor != pred || seg.Header.Predecessor == seg.ID {
			continue
		}
		id := seg.ID
		if marked[id] {
			continue // a mark names it: the marks steps' own, never a spare's
		}
		if !slices.Contains(st.Spares.Held, id) {
			continue // the spares phase re-adopted it, or never withheld it for this predecessor
		}
		sc, ok := markScan(st, id)
		if !ok || sc.Sealed || sc.RecordsEnd != format.SegmentHeaderSize || sc.Size != e.deps.SegmentCap {
			continue // not the record-less take the spare test answers
		}
		out = append(out, id)
	}
	slices.Sort(out)
	return out
}

// standingCopy answers the placement a verifying copy of rec already holds in
// a segment the open neither ends nor saw fail: its own Ended rows are
// excluded — each is emptied or was, and its records carry through its own
// leg — and so is every unsealed row, whose bytes nothing proved. Such a
// segment stays held. A read no bounded attempt completed refuses the open.
func (e *emptying) standingCopy(rec index.Record, id core.SegmentID) (index.RecordAt, bool, error) {
	at, _, found, err := e.verifyingDuplicate(rec, id, false)
	return at, found, err
}

// verifyingDuplicate answers another fed copy of rec — same file and seq,
// outside segment id — whose whole record verifies, with its bytes, looked
// up in the duplicates the feed collected rather than by walking the
// segments again. A copy counts only in a Sealed row, or with endedToo in an
// Ended row this run has not emptied yet; a row already emptied or retired
// is out of the ledger. A failed-list record never fed, so it is never a
// duplicate. A read no bounded attempt completed refuses the open.
func (e *emptying) verifyingDuplicate(rec index.Record, id core.SegmentID, endedToo bool) (index.RecordAt, []byte, bool, error) {
	for _, at := range e.st.Kinds.Duplicates[copyKey{file: rec.File, seq: rec.Numbers.Seq}] {
		if at.Segment == id {
			continue // the record's own copy is no other copy: it goes with the segment
		}
		// decision: a content duplicate in a sealed row this run also empties
		// is a standing copy all the same: the skip repoints the served piece
		// there, so that row's own emptying finds the record named and carries
		// it, and one emptied first is out of the ledger and leaves the ended
		// segment's copy to carry. Withdrawn if the index stops serving the
		// repointed placement.
		state, err := e.st.Assembled.Set.State(at.Segment)
		if standing := state == segment.Sealed || endedToo && state == segment.Ended; err != nil || !standing {
			// decision: only a sealed row's duplicate stands. An unsealed row's
			// bytes are the cache's word until a sync the open can read proves
			// them, and the copy this skip stands in for goes to a sealed,
			// synced spare. Withdrawn if an active row's synced-through is
			// read here and proves the duplicate's whole extent.
			//
			// decision: no test reaches this narrowing for an active row,
			// for no fixture holds a fed duplicate in an unsealed row the
			// open keeps beside an emptied one; the aside skip's is covered.
			continue
		}
		b, h, rot, rerr := readRecordWhole(e.rd, e.identity, at.Segment, int64(at.Offset))
		if rerr != nil {
			return index.RecordAt{}, nil, false, rerr
		}
		if rot || h.Version != rec.Numbers.Version || h.Extent != (core.Extent{Off: rec.Off, Len: rec.Len}) {
			continue
		}
		return at, b, true, nil
	}
	return index.RecordAt{}, nil, false, nil
}

// rottenTwin answers the placement of a duplicate of rec — same sequence
// number, in a segment other than id — the index serves over rec's extent in
// its place, when that duplicate's payload does not verify on a whole read.
// rots is false when the index serves no such duplicate or it verifies. A read
// no bounded attempt completed refuses the open.
func (e *emptying) rottenTwin(rec index.Record, id core.SegmentID) (index.RecordAt, bool, error) {
	placed, _ := e.st.Rebuilt.Index.Span(rec.File, core.Extent{Off: rec.Off, Len: rec.Len})
	var read []index.RecordAt
	for _, p := range placed {
		if p.Seq != rec.Numbers.Seq || p.Record.Segment == id || slices.Contains(read, p.Record) {
			continue
		}
		read = append(read, p.Record)
		_, _, rot, rerr := readRecordWhole(e.rd, e.identity, p.Record.Segment, int64(p.Record.Offset))
		if rerr != nil {
			return index.RecordAt{}, false, rerr
		}
		if rot {
			return p.Record, true, nil
		}
	}
	return index.RecordAt{}, false, nil
}

// asideCopyVerifies answers whether a copy of the aside record rec verifies
// in a segment the open neither ends nor saw fail, the standing test
// standingCopy runs for content records.
func (e *emptying) asideCopyVerifies(rec reclaim.Rec, id core.SegmentID) bool {
	for i := range e.st.Rebuilt.Scans {
		sc := &e.st.Rebuilt.Scans[i]
		if sc.ID == id {
			continue
		}
		// decision: a sealed row this run also empties is a standing copy
		// all the same. The asides register one record per copy, so each
		// segment owes its own carry, and a segment emptied later finds the
		// earlier one's row gone (State errors, so it is skipped here): it
		// skips when the earlier one carried its copy into this run's own
		// target, and carries otherwise, so the last of any set of
		// duplicates leaves exactly one. Withdrawn if the open ever
		// registers one record for a set of duplicates.
		state, err := e.st.Assembled.Set.State(sc.ID)
		if err != nil || state != segment.Sealed {
			continue // the same trust standingCopy keeps: a sealed row alone
		}
		for _, a := range sc.Aside {
			if a.Header.Kind == rec.Kind && a.Header.Seq == rec.Seq &&
				a.Header.File == rec.File && a.Header.Extent == rec.Ext {
				return true
			}
		}
	}
	return false
}

// readWhole reads the record at off of segment id whole — its header and the
// body its header sizes — through the open's reader, answering the bytes a
// verifying read kept. A read no bounded attempt completed refuses the open
// on the ranges the reader collected. A record whose bytes do not decode or
// verify answers nil with a nil error: the record failed only on this read,
// and the caller's loss leg or refusal owns it.
func (e *emptying) readWhole(id core.SegmentID, off int64) (bytes []byte, refused error) {
	b, _, rot, rerr := readRecordWhole(e.rd, e.identity, id, off)
	if rerr != nil {
		return nil, rerr
	}
	if rot {
		return nil, nil
	}
	return b, nil
}

// readRecordWhole reads the record at off of segment id whole, through the
// open's bound: its header, decoded and judged against the placement, then
// the body its header sizes, verified. rot reports a record whose bytes do
// not decode or verify — a read that returned, bytes nobody may trust. A
// read no bounded attempt completed answers the open's refusal on the ranges
// the reader collected.
func readRecordWhole(rd *reader, identity core.JournalID, id core.SegmentID, off int64) (b []byte, h format.RecordHeader, rot bool, refused error) {
	name := format.SegmentName(id)
	hdr := make([]byte, format.RecordHeaderSize)
	n, rerr := rd.ReadAt(name, hdr, off)
	if rerr != nil && !errors.Is(rerr, io.EOF) {
		return nil, h, false, unreadRefusal(rd, rerr)
	}
	if int64(n) < format.RecordHeaderSize {
		return nil, h, true, nil // the file ends short of a header the walk once verified: a rot on this read
	}
	h, derr := format.DecodeRecordHeader(hdr[:n], identity, id, off)
	if derr != nil {
		return nil, h, true, nil
	}
	full := make([]byte, format.RecordHeaderSize+int64(h.BodySize()))
	fn, rerr := rd.ReadAt(name, full, off)
	if rerr != nil && !errors.Is(rerr, io.EOF) {
		return nil, h, false, unreadRefusal(rd, rerr)
	}
	if int64(fn) < format.RecordHeaderSize+int64(h.BodySize()) {
		return nil, h, true, nil // short of the body the header sizes: the same rot
	}
	if _, verr := format.VerifyRecordBody(full[format.RecordHeaderSize:], identity, h); verr != nil {
		return nil, h, true, nil
	}
	return full, h, false, nil
}

// newLossWrite builds one loss record's encoded bytes for l, drawing its own
// fresh sequence number — a loss record this open never numbered — and the
// registration the copy carries, keyed where the copy lands by the caller.
// The record is transported as bytes through the copier, its header carried
// at the segment it defers against so the copier's placement check verifies
// it.
func (e *emptying) newLossWrite(l durability.Loss) (lossWrite, error) {
	seq, err := e.st.Rebuilt.Counters.NextSeq()
	if err != nil {
		// The counter's exhaustion repeats identically on every retry.
		return lossWrite{}, fmt.Errorf("%w: numbering the loss record of the dropped record %d: %w", core.ErrOpenRefused, l.Seq, err)
	}
	h := format.RecordHeader{
		Kind:    format.KindLoss,
		File:    l.File,
		Extent:  l.Extent,
		Seq:     seq,
		Version: l.Version,
		Tag:     l.Share,
		Segment: l.Seg,
		Offset:  0,
	}
	body := format.RecordBody{Loss: format.Loss{
		Dropped:   l.Seq,
		Synced:    l.Synced,
		Offloaded: l.Offloaded,
		Reason:    format.LossReasonOf(l.Reason),
	}}
	at := format.AppendRecord(nil, e.identity, h, body)
	// The registration the copy carries, mirrors what the kinds pass builds
	// for a loss record it reads back: the copy registers live at the
	// placement it lands.
	reg := reclaim.Rec{
		Kind:       format.KindLoss,
		File:       l.File,
		Seg:        l.Seg,
		Ext:        l.Extent,
		Pair:       index.Numbers{Version: l.Version, Seq: seq},
		Seq:        seq,
		Named:      l.Seq,
		Tag:        l.Share,
		Synced:     l.Synced,
		Superseded: l.Reason == core.LossSuperseded,
		Loc:        segment.Location{Segment: l.Seg, Off: 0, Len: int64(len(at))},
	}
	return lossWrite{l: l, at: at, seq: seq, rec: reg}, nil
}

// lossOnDisk answers whether a loss record naming the loss l already
// verifies on disk: the aside records the walks read keep every attached one
// with its body, so an open that crashed between the copy and the deferral's
// end finds the record the copy already landed and ends the deferral without
// a second record.
func (e *emptying) lossOnDisk(l durability.Loss) bool {
	for i := range e.st.Rebuilt.Scans {
		// The trust asideCopyVerifies keeps: a sealed row's bytes alone are
		// proven, and a deferral ended on an unsealed row's record would
		// leave the loss unwritten if that record never reached a seal.
		if state, err := e.st.Assembled.Set.State(e.st.Rebuilt.Scans[i].ID); err != nil || state != segment.Sealed {
			continue
		}
		for _, a := range e.st.Rebuilt.Scans[i].Aside {
			if a.Header.Kind != format.KindLoss ||
				a.Body.Loss.Dropped != l.Seq || a.Header.Extent != l.Extent {
				continue
			}
			// The read-found case: a record named on disk by reason stands
			// for the same loss over the same extent whatever its reason's
			// number is, for a piece is taken once by (seq, extent).
			return true
		}
	}
	// The records the re-append sealed into its target: the walks never read
	// that target, whose proof is its own seal.
	if e.st.Proof.ReappendTarget != 0 {
		for _, rec := range e.st.Proof.Reappends {
			h := rec.Header
			if h.Kind != format.KindLoss || h.Extent != l.Extent || int64(len(rec.First)) < format.RecordHeaderSize+h.BodySize() {
				continue
			}
			if body, err := format.VerifyRecordBody(rec.First[format.RecordHeaderSize:format.RecordHeaderSize+h.BodySize()], e.identity, h); err == nil && body.Loss.Dropped == l.Seq {
				return true
			}
		}
	}
	return false
}

// asideRecs answers every registered aside record the open rebuilt: the live
// registrations, the unsynced losses' own and the forgets'. Each is the Rec
// the assembly handed the tracker, so the registry's own answer — Droppable
// — judges the record a copy leg carries.
func asideRecs(st *State) []reclaim.Rec {
	var out []reclaim.Rec
	out = append(out, st.Assembled.Live...)
	for _, u := range st.Assembled.Unsynced {
		out = append(out, u.Rec)
	}
	for _, f := range st.Assembled.Forgets {
		out = append(out, f.Rec)
	}
	return out
}

// forgetSnapshot answers the snapshot the forget rec's copy re-enters: the
// segments holding records of its share tag when the journal appended it,
// exactly what its original registration carried.
func forgetSnapshot(st *State, rec reclaim.Rec) []core.SegmentID {
	for _, f := range st.Assembled.Forgets {
		if f.Rec == rec {
			return slices.Clone(f.Segs)
		}
	}
	return nil
}

// coveredRec spells the extent the record rec covers, the way the index's
// drop ranges keep it: a truncate covers everything past the size its header
// names, a delete the whole file, and every other kind the header's own
// extent.
func coveredRec(rec reclaim.Rec) core.Extent {
	switch rec.Kind {
	case format.KindTruncate:
		return core.Extent{Off: rec.Ext.Off, Len: math.MaxInt64 - rec.Ext.Off}
	case format.KindDelete:
		return core.Extent{Off: 0, Len: math.MaxInt64}
	default:
		return rec.Ext
	}
}
