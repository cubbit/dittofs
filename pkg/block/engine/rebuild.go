package engine

import (
	"context"
	"fmt"

	"github.com/cubbit/dittofs/pkg/block/journal"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/metadata/kvstore"
	"github.com/cubbit/dittofs/pkg/vfs/vfserr"
)

// A clone a crash cut short is rebuilt at start from what the journal and
// the store kept of it: before its phase 1 committed, from the journal's
// clone marker (step 3); after, from the clone removal the store holds not
// done (step 4). Both of its registrations are installed before Start
// publishes the share table, so no call reaches either extent before the
// clone is done, and its work is posted to the destination journal's loop
// once Start has published it.
//
// decision: a rebuilt clone runs in a served engine although nothing joins a
// running clone at Unbind or Close yet, which keeps a live Clone behind a
// test's stand-in: refusing Start instead would leave every file of the share
// unserved, and a rebuilt clone ends at its loop's end, leaving the marker or
// the removal record for the next start, as a live clone's retry does.
// Withdraw it with that stand-in, once the running clones are joined.

// errSourceNotServed fails a Start that finds a clone to rebuild whose
// source share it does not serve: the destination may not be served before
// the clone is done, and the clone cannot run without its source.
var errSourceNotServed = fmt.Errorf("engine: start: a clone's source share is not served: %w", vfserr.ErrUnavailable)

// rebuildRun is the clone run of a rebuilt clone of spec into file dst of
// share db, its registrations installed on both files. The source share must
// be one this start serves.
func (e *Engine) rebuildRun(o *opened, db *binding, dst entity.FileID, spec journal.CloneSpec) (*cloneRun, error) {
	sb := o.table[entity.ShareID(spec.SrcShare)]
	if sb == nil || sb.removed {
		return nil, fmt.Errorf("engine: start: rebuild the clone into file %v from share %v: %w",
			dst, entity.ShareID(spec.SrcShare), errSourceNotServed)
	}

	c := &cloneRun{
		sb:   sb,
		db:   db,
		src:  entity.FileID(spec.Src.Bytes()),
		dst:  dst,
		rs:   &reg{Extent: entity.Extent{Offset: spec.SrcOff, Length: spec.Len}},
		rd:   &reg{Extent: entity.Extent{Offset: spec.DstOff, Length: spec.Len}, dst: true},
		spec: spec,
		done: make(chan struct{}),
	}

	// Nothing is served yet, so no write is admitted to wait out and no
	// registration of a live clone can overlap.
	for _, x := range []struct {
		f entity.FileID
		r *reg
	}{{c.src, c.rs}, {c.dst, c.rd}} {
		fe := e.files.lockFor(x.f)
		fe.regs = append(fe.regs, x.r)
		e.files.unlock(x.f, fe)
	}
	o.undoRebuilds = append(o.undoRebuilds, func() {
		e.unregister(c.dst, c.rd)
		e.unregister(c.src, c.rs)
	})

	return c, nil
}

// rebuildMarker rebuilds the clone whose marker at v start step 3 found in
// file f of share b, above every change it applied, above the changes the
// journal holds of the file past the marker. The destination's guard is
// taken and its entry given the clone's operation at v, each copy the journal
// holds inside the destination extent, which the clone keeps, and each write
// outside it, which phase 1 commits with the clone; then the clone's copies,
// sync and phase 1 run from the destination journal's loop, as a live
// clone's retry does, making and reserving only the copies not kept.
//
// ponytail: a write the journal holds above the marker, outside the
// destination extent, takes the restart's time as its modification time, not
// the one its record carries, as the entry keeps one time per operation.
// Carry the record's time in the entry if a client is seen reading a write's
// time moved by a restart.
func (e *Engine) rebuildMarker(ctx context.Context, o *opened, b *binding, f entity.FileID, spec journal.CloneSpec, v journal.Version, above []replayStep) error {
	c, err := e.rebuildRun(o, b, f, spec)
	if err != nil {
		return err
	}

	r, err := e.guard(ctx, b, f, entity.RemoveClone, c.rd.Extent)
	if err != nil {
		return fmt.Errorf("engine: start: rebuild the clone into file %v: %w", f, err)
	}
	o.undoRebuilds = append(o.undoRebuilds, func() {
		fe := e.files.lockFor(f)
		// The replay commits through the store and leaves nothing on an
		// entry, so every operation here is the rebuild's.
		fe.ops = nil
		fe.unguard()
		e.files.unlock(f, fe)
	})

	r.v = v
	dx := journal.Extent{Off: spec.DstOff, Len: spec.Len}

	fe := e.files.lockFor(f)
	fe.insert(pendingOp{version: v, kind: opClone, extent: dx, at: r.at})

	for _, s := range above {
		// A destination with a clone running takes no removal and no write
		// into the extent: a change above the marker is a copy or a write
		// outside the extent, nothing else.
		if s.kind != journal.ChangeWrite {
			e.files.unlock(f, fe)
			return fmt.Errorf("engine: start: rebuild the clone into file %v: a %v at %v above its marker",
				f, s.kind, s.version)
		}

		copied := s.extent.Off >= dx.Off && opEnd(s.extent) <= opEnd(dx)
		fe.insert(pendingOp{version: s.version, kind: opWrite, extent: s.extent, at: r.at,
			setsMtime: true, lost: s.lost, copy: copied})

		if copied {
			c.kept = append(c.kept, keptRuns(s, spec)...)
			c.newest = newer(c.newest, s.version)
		}
	}
	e.files.unlock(f, fe)

	o.removals = append(o.removals, queuedRemoval{b: b, it: e.cloneItem(c, r)})
	e.d.Logger.Info("clone rebuilt at start from its journal marker",
		"src", c.src, "dst", f, "kept", len(c.kept))

	return nil
}

// keptRuns is the source extents a copy s the journal holds covers: the
// copy's extent but its lost runs, moved to the source's offsets.
func keptRuns(s replayStep, spec journal.CloneSpec) []journal.Extent {
	held := []journal.Held{{Extent: s.extent}}
	for _, l := range s.lost {
		held = uncovered(held, []journal.Extent{{Off: l.Offset, Len: l.Length}})
	}

	out := make([]journal.Extent, len(held))
	for i, h := range held {
		out[i] = journal.Extent{Off: h.Extent.Off - spec.DstOff + spec.SrcOff, Len: h.Extent.Len}
	}

	return out
}

// uncovered is runs with every byte kept covers cut out.
func uncovered(runs []journal.Held, kept []journal.Extent) []journal.Held {
	for _, k := range kept {
		var next []journal.Held

		for _, h := range runs {
			x := h.Extent
			if k.Off >= opEnd(x) || opEnd(k) <= x.Off {
				next = append(next, h)
				continue
			}

			if x.Off < k.Off {
				next = append(next, journal.Held{Extent: journal.Extent{Off: x.Off, Len: k.Off - x.Off}, Version: h.Version})
			}

			if opEnd(k) < opEnd(x) {
				next = append(next, journal.Held{Extent: journal.Extent{Off: opEnd(k), Len: opEnd(x) - opEnd(k)}, Version: h.Version})
			}
		}

		runs = next
	}

	return runs
}

// rebuildRemoval rebuilds the clone whose removal u start step 4 listed not
// done for file f of share b: its phase 1 committed, so its batches are left,
// which run once open state serves the source's shard, its hold then finding
// the clone-kind open open state's recovery installed.
//
// decision: on one node the source's registration is rebuilt from the clone
// removal the destination's shard lists, whose spec names the source extent,
// not from the source's clone-kind open record, which nothing the engine
// receives lists. Withdraw it when a source's shard can have another primary
// than its destination's, when open state's recovery must hand the engine the
// record.
func (e *Engine) rebuildRemoval(o *opened, b *binding, f entity.FileID, u kvstore.UnfinishedRemoval) error {
	s := u.Removal.Clone
	if s == nil {
		return fmt.Errorf("engine: start: rebuild the clone into file %v: its removal at %v names no clone", f, u.Version)
	}

	spec := journal.CloneSpec{
		SrcShare: journal.ShareID(s.SrcShare),
		Src:      fileID(s.Src),
		SrcOff:   s.SrcOff,
		DstOff:   s.DstOff,
		Len:      s.Len,
		AsOf:     journal.VersionFromBytes(s.AsOf),
	}

	c, err := e.rebuildRun(o, b, f, spec)
	if err != nil {
		return err
	}

	r := &removal{b: b, f: f, x: c.rd.Extent, kind: entity.RemoveClone,
		v: journal.VersionFromBytes(u.Version), at: e.d.Clock.Now()}
	it := e.clonePhase2(c, r)

	o.rebuilt = append(o.rebuilt, func() { go e.awaitServed(c, it) })
	e.d.Logger.Info("clone rebuilt at start from its removal record", "src", c.src, "dst", f)

	return nil
}

// awaitServed waits, on its own goroutine, until open state serves the
// rebuilt clone's source shard, then posts it, its phase 2, to the
// destination journal's loop, so the loop never holds the wait. The wait runs
// in rounds of the loop's backoff; after five it is removal_stalled for the
// destination, cleared once the shard is served. The loop's end ends the
// clone, leaving its removal for the next start.
func (e *Engine) awaitServed(c *cloneRun, it *loopItem) {
	l := c.db.gc.removals

	for rounds := 1; ; rounds++ {
		err := e.servedRound(l.ctx, c.sb.share.Shard)
		if err == nil {
			break
		}

		if l.ctx.Err() != nil {
			l.waiting(it, false)
			it.end()

			return
		}

		if rounds == stallAfter {
			l.waiting(it, true)
		}
	}

	l.waiting(it, false)
	l.enqueue(it)
}

// servedRound is one round of awaitServed: Holds.Served for shard s, cut
// at the loop's backoff on the engine's clock or at ctx's end. A refusal
// before the round's end waits the round out, so a Served that answers at
// once is not asked again in a busy loop.
func (e *Engine) servedRound(ctx context.Context, s entity.ShardID) error {
	rctx, cancel := context.WithCancel(ctx)
	defer cancel()

	tick := e.d.Clock.After(loopBackoff)

	go func() {
		select {
		case <-tick:
			cancel()
		case <-rctx.Done():
		}
	}()

	err := e.d.Holds.Served(rctx, s)
	if err != nil {
		<-rctx.Done() // the round's end, or ctx's
	}

	return err
}
