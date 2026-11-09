package engine

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/cubbit/dittofs/pkg/block/journal"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/metadata/kvstore"
)

// replay is start step 3 (RFC 8 §2.5): for every file each served share's
// journal lists, it applies to existence the operations above the file's
// applied version, in version order, before any file is served. Files are
// processed one at a time, so memory holds the changes of one file, never of
// a share.
//
// decision: any error a journal's Since yields fails Start, as any failing
// step does. RFC 8 §2.5 step 3 and step 8 skip a file whose Since yields
// ErrUnreadable, which takes writes and serves no read, but the journal
// declares no such error yet, so an unreadable record cannot be told from a
// failing device. Overturned once the journal declares ErrUnreadable, when
// that file is skipped and step 3 runs for it again once its record reads.
func (e *Engine) replay(ctx context.Context, o *opened) error {
	now := e.d.Clock.Now()

	for _, b := range o.table {
		if b.removed {
			continue
		}

		for id := range b.h.Files() {
			if err := e.replayFile(ctx, b, id, now); err != nil {
				return err
			}
		}
	}

	return nil
}

// replayFile applies one file's changes above its applied version, settling
// the journal after each commit.
func (e *Engine) replayFile(
	ctx context.Context,
	b *binding,
	id journal.FileID,
	now time.Time,
) error {
	f := entity.FileID(id.Bytes())

	applied, err := e.d.Existence.Applied(ctx, b.share.ID, f)
	if errors.Is(err, entity.ErrStale) {
		return e.settleGone(ctx, b, id)
	}

	if err != nil {
		return fmt.Errorf("engine: start: read the applied version of file %v: %w", f, err)
	}

	size, err := e.d.Existence.Size(ctx, b.share.ID, f)
	if err != nil {
		return fmt.Errorf("engine: start: read the size of file %v: %w", f, err)
	}

	changes, err := changesSince(ctx, b.h, id, journal.VersionFromBytes(applied))
	if err != nil {
		return fmt.Errorf("engine: start: read the journal's changes of file %v: %w", f, err)
	}

	r := &replayer{
		e:    e,
		b:    b,
		id:   id,
		f:    f,
		size: size,
		now:  now,
	}

	return r.run(ctx, stepsOf(changes))
}

// settleGone settles the journal through the newest change of a file the
// store no longer holds: its release committed and the crash came before the
// settle, so there is nothing to apply and nothing lost (RFC 8 §2.5 step 3).
func (e *Engine) settleGone(ctx context.Context, b *binding, id journal.FileID) error {
	f := entity.FileID(id.Bytes())

	changes, err := changesSince(ctx, b.h, id, journal.Version{})
	if err != nil {
		return fmt.Errorf("engine: start: read the journal's changes of file %v: %w", f, err)
	}

	var newest journal.Version

	for _, c := range changes {
		newest = newer(newest, c.Version)
	}

	if newest != (journal.Version{}) {
		b.h.Settle(id, newest)
	}

	return nil
}

// changesSince is every change the handle yields for the file above from.
func changesSince(
	ctx context.Context,
	h Handle,
	id journal.FileID,
	from journal.Version,
) ([]journal.Change, error) {
	var out []journal.Change

	for c, err := range h.Since(ctx, id, from) {
		if err != nil {
			return nil, err
		}

		out = append(out, c)
	}

	return out, nil
}

// cloneMarkerFunc is called with the share, the file, the spec and the version
// of a clone marker the journal yields above a file's applied version.
type cloneMarkerFunc func(
	share entity.ShareID,
	file entity.FileID,
	spec journal.CloneSpec,
	v journal.Version,
)

// replayStep is what step 3 applies for one version of a file: the held
// pieces and the losses of one write, merged into one write, or one removal
// marker.
type replayStep struct {
	version journal.Version
	kind    journal.ChangeKind // ChangeWrite for a merged write, else the marker's kind
	extent  journal.Extent
	mtime   int64           // the write's modification time in nanoseconds, 0 when none is known
	lost    []entity.Extent // the runs of the write the journal lost after its sync
	clone   *journal.CloneSpec
}

// stepsOf merges the changes of one version into one step each, in version
// order.
//
// decision: the held pieces of one version, and a loss of that version, are
// one write over the range from the first piece's start to the last piece's
// end, with the lost runs inside it. The store commits operations in strictly
// increasing version, and a write overwritten in its middle, or lost in its
// middle, comes back from Since as several changes of one version (RFC 1
// §3.10). The range may cover bytes the journal no longer holds, and each such
// gap is covered by an operation of a higher version, a write, a removal
// marker or a loss, which Since also yields because it is above applied, and
// which is applied afterwards. Overturned if Since yields a version's pieces
// with the gaps between them named.
func stepsOf(changes []journal.Change) []replayStep {
	var steps []replayStep

	for _, c := range changes {
		switch c.Kind {
		case journal.ChangeWrite, journal.ChangeLoss:
			n := len(steps) - 1

			if n < 0 || steps[n].kind != journal.ChangeWrite || steps[n].version != c.Version {
				steps = append(steps, replayStep{
					version: c.Version,
					kind:    journal.ChangeWrite,
					extent:  c.Extent,
				})
			} else {
				steps[n].extent = hull(steps[n].extent, c.Extent)
			}

			s := &steps[len(steps)-1]

			if c.Kind == journal.ChangeLoss {
				s.lost = append(s.lost, entity.Extent{
					Offset: c.Extent.Off,
					Length: c.Extent.Len,
				})
			} else if s.mtime == 0 {
				s.mtime = c.Mtime
			}
		default:
			steps = append(steps, replayStep{
				version: c.Version,
				kind:    c.Kind,
				extent:  c.Extent,
				clone:   c.Clone,
			})
		}
	}

	for i := range steps {
		steps[i].lost = entity.MergeExtents(steps[i].lost)
	}

	return steps
}

// hull is the smallest extent holding a and b.
func hull(a, b journal.Extent) journal.Extent {
	start := min(a.Off, b.Off)

	return journal.Extent{
		Off: start,
		Len: max(opEnd(a), opEnd(b)) - start,
	}
}

// replayer applies one file's steps. Its writes wait in batch until a
// removal, a clone marker or the end of the file commits them.
type replayer struct {
	e      *Engine
	b      *binding
	id     journal.FileID
	f      entity.FileID
	size   int64 // the size the file has once everything applied so far is committed
	now    time.Time
	batch  []pendingOp
	mtimes []time.Time // the modification time each operation of batch sets, zero for none
}

// run applies steps in order and commits what is left.
func (r *replayer) run(ctx context.Context, steps []replayStep) error {
	for _, s := range steps {
		stop, err := r.apply(ctx, s)
		if err != nil {
			return err
		}

		if stop {
			break
		}
	}

	return r.commit(ctx)
}

// apply applies step s. It answers stop for a clone marker, above which
// nothing is applied.
func (r *replayer) apply(ctx context.Context, s replayStep) (stop bool, err error) {
	switch s.kind {
	case journal.ChangeWrite:
		r.stageWrite(s)

		return false, nil
	case journal.ChangeTruncate:
		// A truncate at or above the size the earlier operations leave masks
		// nothing, so it is a grow with no phase 1, as on the live path.
		if s.extent.Off >= r.size {
			r.stageGrow(s)

			return false, nil
		}

		r.size = s.extent.Off
		x := entity.ExtentOf(s.extent.Off, entity.EndOfFile)

		return false, r.remove(ctx, s, entity.RemoveTruncate, x)
	case journal.ChangeDeallocate:
		x := entity.Extent{
			Offset: s.extent.Off,
			Length: s.extent.Len,
		}

		return false, r.remove(ctx, s, entity.RemoveDeallocate, x)
	case journal.ChangeDelete:
		x := entity.ExtentOf(0, entity.EndOfFile)

		return false, r.remove(ctx, s, entity.RemoveRelease, x)
	case journal.ChangeCloneTarget:
		if err := r.commit(ctx); err != nil {
			return true, err
		}

		if hook := r.e.cloneMarker; hook != nil && s.clone != nil {
			hook(r.b.share.ID, r.f, *s.clone, s.version)
		}

		return true, nil
	}

	return false, fmt.Errorf("engine: start: change kind %d of file %v is not known", s.kind, r.f)
}

// stageWrite adds the write to the batch, with the change time of the
// restart and the modification time of the record.
//
// decision: a replayed operation's change time is the clock's time at the
// restart. A write lost whole, and a write whose record carries no
// modification time, leave the file's modification time as it was, since the
// journal records one time per write, zero when suspended (RFC 28 §2.3), and
// a loss keeps none of it. A truncate up sets the modification time to the
// restart's, as a replayed truncate down and the live path do. Overturned by
// RFC 28 recording the times a write and a loss set.
func (r *replayer) stageWrite(s replayStep) {
	r.batch = append(r.batch, pendingOp{
		version: s.version,
		kind:    opWrite,
		extent:  s.extent,
		at:      r.now,
		lost:    s.lost,
	})

	var mtime time.Time

	if s.mtime != 0 {
		mtime = time.Unix(0, s.mtime)
	}

	r.mtimes = append(r.mtimes, mtime)
	r.size = max(r.size, opEnd(s.extent))
}

// stageGrow adds a truncate at or above the file's size, which masks nothing
// and commits as a write does, setting the modification time to the
// restart's.
func (r *replayer) stageGrow(s replayStep) {
	r.batch = append(r.batch, pendingOp{
		version: s.version,
		kind:    opTruncate,
		extent:  journal.Extent{Off: s.extent.Off},
		at:      r.now,
		grow:    true,
	})
	r.mtimes = append(r.mtimes, r.now)
	r.size = s.extent.Off
}

// commit syncs the file in the journal and commits the batch's existence in
// one call, which splits by the store's key budget itself, then settles the
// journal through the version of the batch's newest operation, never above it,
// and empties the batch (RFC 8 §2.5 step 3, E19).
//
// decision: a write's overwritten runs are computed from the replay's own
// writes, as the group commit computes them, not taken from the journal. A
// write that another write above applied superseded, and that already left
// the journal's index, is not seen, though RFC 1 §3.10 and RFC 8 §2.5 step 3
// and §5.1 have Since yield the runs as Change.Over. Overturned once the
// journal's Change carries Over.
func (r *replayer) commit(ctx context.Context) error {
	if len(r.batch) == 0 {
		return nil
	}

	ops := make([]kvstore.PendingOp, len(r.batch))

	for i, op := range r.batch {
		ops[i] = storeOp(op, r.mtimes[i])

		if op.kind == opWrite {
			ops[i].Over = superseded(r.batch, i)
		}
	}

	if err := r.sync(ctx); err != nil {
		return err
	}

	pending := []kvstore.PendingExistence{
		{
			Share: r.b.share.ID,
			File:  r.f,
			Ops:   ops,
		},
	}

	failed, err := r.e.d.Existence.Commit(ctx, pending, nil)
	if err != nil {
		return fmt.Errorf("engine: start: commit the existence of file %v: %w", r.f, err)
	}

	if err := failed[r.f]; err != nil {
		return fmt.Errorf("engine: start: commit the existence of file %v: %w", r.f, err)
	}

	r.b.h.Settle(r.id, r.batch[len(r.batch)-1].version)

	r.batch = nil
	r.mtimes = nil

	return nil
}

// sync makes the file's records durable before an existence commit.
//
// decision: the replay calls the handle's Sync itself, not the engine's sync
// that drains the journal's loss ring after it, which needs the journal's
// group commit, and that is not built before step 3 ends. A SyncError at
// start fails Start, which a later Start tries again, where the engine's sync
// syncs again. Skipping the drain before the commit is a conclusion from three
// passages, none of which says it: a loss record still on disk is put back in
// the ring at open (RFC 1 §3.8); Since also yields a synced loss record above
// applied, and its existence is committed from it (RFC 8 §2.5 step 3); and
// the first Commit drains the ring past the last event the engine applied
// (RFC 8 §5.1). Overturned if the journal stops yielding a synced loss through
// Since, or if RFC 8 states what a replay does with the ring.
func (r *replayer) sync(ctx context.Context) error {
	if _, err := r.b.h.Sync(ctx, r.id); err != nil {
		return fmt.Errorf("engine: start: sync file %v: %w", r.f, err)
	}

	return nil
}

// remove runs the first transaction of a removal the journal left a marker
// for, after the writes below it commit. A release a holder refuses, and a
// removal the store refuses as invalid, end unapplied: the marker is settled
// without the removal and the loss is reported naming the file (RFC 8 §2.5
// step 3). A file found gone ends it quietly.
//
// The loss sequence of the file is not raised: the aborted release happened
// before the restart, and no RFC 8 rule for the sequence names it.
func (r *replayer) remove(
	ctx context.Context,
	s replayStep,
	kind entity.RemovalKind,
	x entity.Extent,
) error {
	if err := r.commit(ctx); err != nil {
		return err
	}

	if err := r.sync(ctx); err != nil {
		return err
	}

	op := kvstore.RemoveOp{
		Version: entity.JournalVersion(s.version.Bytes()),
		Kind:    kind,
		Extent:  x,
		Time:    r.now,
	}

	err := r.e.d.Existence.Remove(ctx, r.b.share.ID, r.f, 0, op, nil)

	// decision: a store answer that the file is gone is settled quietly, as a
	// file found gone is (RFC 8 §2.5 step 3), but it is not expected here: the
	// applied version was just read for this file, and no call reaches a file
	// until step 3 has run for it (RFC 8 §2.5 step 8). No RFC text says it
	// cannot happen, so it is kept as a defense. Overturned by a store that
	// answers it for a file it still holds.
	switch {
	case err == nil, errors.Is(err, entity.ErrStale):
	case kind == entity.RemoveRelease && errors.Is(err, kvstore.ErrHeld):
		r.e.d.Logger.Error(
			"a release aborted by a holder before the restart: content not offloaded reads lost",
			"file", r.f,
		)
	case kvstore.Refused(err):
		r.e.d.Logger.Error(
			"removal ended unapplied, its journal marker settled",
			"file", r.f,
			"kind", kind.String(),
			"err", err,
		)
	default:
		return fmt.Errorf("engine: start: apply the %s of file %v: %w", kind.String(), r.f, err)
	}

	r.b.h.Settle(r.id, s.version)

	return nil
}
