package engine

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"

	"github.com/cubbit/dittofs/pkg/block/journal"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/metadata/kvstore"
)

// cloneRemovalFunc is called with the share, the file and the record of a
// clone removal not done that start step 4 lists.
type cloneRemovalFunc func(share entity.ShareID, file entity.FileID, r entity.Removal)

// queuedRemoval is a loop item step 4 found, with the binding whose journal's
// loop runs it.
type queuedRemoval struct {
	b  *binding
	it *loopItem
}

// collectRemovals is start step 4 (RFC 8 §2.5): for each served share it lists
// the shard's unfinished-removal index, never the files, and keeps one loop
// item per file with removals pending. Nothing runs here: Start posts the
// items once the table is published, so start time does not grow with the
// removals pending.
//
// decision: the removals of a file are one item, as RFC 8 §6.1 makes them one
// entry: a not-done one runs the file's phase 2 and then the prune at the
// newest version listed, a done one only the prune. A file with a clone not
// done gets no item: the store's Resume takes the oldest removal not done of
// the file, a clone's included, so the clone's own rebuild runs the file's
// removals in version order once it holds the source. Overturned if the
// rebuild is found to leave a plain removal of such a file unfinished.
func (e *Engine) collectRemovals(ctx context.Context, o *opened) error {
	if o.journal == nil {
		return nil
	}

	for _, b := range o.table {
		if b.removed {
			continue
		}

		if err := e.collectShard(ctx, o, b); err != nil {
			return err
		}
	}

	return nil
}

// collectShard lists one share's shard. The entries of one file are adjacent
// in the index, so a file's are held while it is read and no more.
func (e *Engine) collectShard(ctx context.Context, o *opened, b *binding) error {
	var file []kvstore.UnfinishedRemoval

	for u, err := range e.d.Content.UnfinishedRemovals(ctx, b.share.ID, uint64(b.share.Shard)) {
		if err != nil {
			return fmt.Errorf(
				"engine: start: list the unfinished removals of share %v: %w",
				b.share.ID,
				err,
			)
		}

		if len(file) > 0 && file[0].File != u.File {
			if err := e.collectFile(o, b, file); err != nil {
				return err
			}

			file = file[:0]
		}

		file = append(file, u)
	}

	if len(file) == 0 {
		return nil
	}

	return e.collectFile(o, b, file)
}

// collectFile hands the clones among one file's removals to the rebuild, or
// keeps the file's one item.
func (e *Engine) collectFile(o *opened, b *binding, entries []kvstore.UnfinishedRemoval) error {
	f := entries[0].File

	var (
		cloning, pending bool
		newest           entity.JournalVersion
		kind             entity.RemovalKind
	)

	for _, u := range entries {
		if !u.Removal.Done {
			pending = true
		}

		if u.Removal.Kind == entity.RemoveClone && !u.Removal.Done {
			cloning = true
		}

		if entity.MaxVersion(newest, u.Version) == u.Version {
			newest, kind = u.Version, u.Removal.Kind
		}
	}

	if cloning {
		for _, u := range entries {
			if u.Removal.Kind != entity.RemoveClone || u.Removal.Done {
				continue
			}

			if e.cloneRemoval != nil {
				e.cloneRemoval(b.share.ID, f, u.Removal)
				continue
			}

			if err := e.rebuildRemoval(o, b, f, u); err != nil {
				return err
			}
		}

		return nil
	}

	r := &removal{
		b:    b,
		f:    f,
		kind: kind,
		v:    journal.VersionFromBytes(newest),
	}

	it := e.prune(r)
	if pending {
		it = e.phase2(r)
	}

	o.removals = append(o.removals, queuedRemoval{
		b:  b,
		it: it,
	})

	return nil
}

// prune is the item that prunes r's removal records, at r's version, when all
// of them are done: no batch is left to run.
func (e *Engine) prune(r *removal) *loopItem {
	share, v := r.b.share.ID, entity.JournalVersion(r.v.Bytes())

	return &loopItem{
		b:    r.b,
		f:    r.f,
		kind: r.kind,
		run: func(ctx context.Context) answer {
			ctx, cancel := e.bounded(ctx)
			defer cancel()

			err := e.d.Content.PruneRemovals(ctx, share, r.f, v)

			res, a := runError, itemRetry

			switch {
			case err == nil:
				res, a = runDone, itemDone
			case errors.Is(err, kvstore.ErrConflict):
				res = runConflict
			}

			r.b.gc.removals.runs[res].Add(1)

			return a
		},
	}
}

// strandUnclaimed is start step 5 (RFC 8 §2.5, RFC 1 §3): every tag the
// journal names that no share record of the installation names, and no
// attached share holds, is stranded and reported with its held and dirty
// bytes, before any file of the journal is served. It runs after every
// served share is attached, so a tag a share attaches later is not taken for
// unclaimed: the store's records decide, never the attached set alone.
//
// decision: a Strand that fails fails Start, as any step does, since RFC 1 §3
// has the caller strand each unclaimed tag before it serves. Overturned if the
// journal can refuse a tag for a cause that leaves it safe to serve.
func (e *Engine) strandUnclaimed(o *opened) error {
	if o.journal == nil {
		return nil
	}

	claimed := map[journal.ShareTag]bool{}

	for _, r := range o.tags {
		claimed[journal.ShareTag(r.Tag)] = true
	}

	for _, b := range o.table {
		if !b.removed {
			claimed[b.share.Tag] = true
		}
	}

	shares := o.journal.j.Stats().Shares

	for _, tag := range slices.Sorted(maps.Keys(shares)) {
		if claimed[tag] {
			continue
		}

		if err := o.journal.j.Strand(tag); err != nil {
			return fmt.Errorf("engine: start: strand tag %d: %w", tag, err)
		}

		e.d.Logger.Warn(
			"unclaimed tag stranded",
			"tag", tag,
			"held", shares[tag].HeldBytes,
			"dirty", shares[tag].DirtyBytes,
		)
	}

	return nil
}
