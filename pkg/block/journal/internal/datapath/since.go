package datapath

import (
	"cmp"
	"context"
	"fmt"
	"iter"
	"slices"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/durability"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/index"
)

// Since yields the changes of file id strictly above from, in version order:
// each held extent with the modification time its record carries and the runs
// it superseded, each unsettled removal marker, and each synced loss. Of one
// version, markers come before extents, then extents in offset order; a loss
// yielded in place of a dropped record can follow a write at the same version,
// though the two never overlap. An
// unsynced loss and a loss of a superseded write leave no marker, so neither
// is yielded.
//
// The iteration copies the file's changes in one step under the file index's
// read lock and holds no lock afterwards: each write's record is then read,
// with no lock held, so a writer to the file never waits on a read of a
// record. A cancelled ctx, a handle whose close began and a file of another
// share are refused first, with ctx.Err(), core.ErrClosed and core.ErrOtherShare,
// as one yield carrying the error and nothing else; ctx is checked again
// between items, ending the iteration with its error. An I/O error ends it too.
//
// A write's record is read and verified as a read verifies it. One that does
// not verify goes to Corrupt under the sequence number the index carries: a
// copy it names that verifies stands in, and one that does not verify either
// leaves the extent yielded with an Mtime of 0; a dropped record's extent is
// not yielded, and when the drop was of a synced record the loss it left is
// yielded in its place. A segment retired since the copy was taken sends the
// read once to the record the index now names for the extent, as a repack
// leaves it; when there is none the record is taken as dropped.
//
// decision: the changes are those of the copy taken at the start. A drop of an
// unsynced record puts what it superseded back in the index, and that write is
// not yielded by this iteration, since it was not held when the copy was
// taken; the next Since yields it. Overturned by a caller that must see a
// consistent view across a drop within one iteration, which would then take a
// second copy after the first drop.
func (c *Core) Since(ctx context.Context, s *Share, id core.FileID, from core.Version) iter.Seq2[core.Change, error] {
	return func(yield func(core.Change, error) bool) {
		if err := ctx.Err(); err != nil {
			yield(core.Change{}, err)
			return
		}
		exit, err := s.enter()
		if err != nil {
			yield(core.Change{}, err)
			return
		}
		// decision: the handle's call count is held across every yield, so a
		// consumer paused inside one makes Share.Close wait for it. Writers and
		// readers are never blocked, which is all the spec asks of Since, and
		// a Close that ran under a live iteration would read records of a
		// closed handle. Overturned by a consumer that parks in a yield for
		// long, which would need the iteration to re-enter per item.
		defer exit()
		if err := c.checkTag(s, id); err != nil {
			yield(core.Change{}, err)
			return
		}
		changes := c.index.Since(id, from)
		sr := sinceRead{reading: reading{c: c, ctx: ctx, id: id, st: c.streams.For(id)}, from: from, dropped: map[index.RecordAt]bool{}, seen: map[lossKey]bool{}}
		for _, ch := range changes {
			if ch.Kind == core.ChangeLoss {
				sr.seen[lossKey{ch.Version, ch.Extent}] = true
			}
		}
		bp := recordBufs.Get().(*[]byte)
		defer recordBufs.Put(bp)
		for _, ch := range changes {
			if err := ctx.Err(); err != nil {
				yield(core.Change{}, err)
				return
			}
			out := core.Change{Kind: ch.Kind, Extent: ch.Extent, Version: ch.Version, Clone: ch.Clone, Over: ch.Over}
			if ch.Kind != core.ChangeWrite {
				if !yield(out, nil) {
					return
				}
				continue
			}
			mtime, replaced, dropped, err := sr.write(ch, bp, false)
			if err != nil {
				yield(core.Change{}, err)
				return
			}
			if dropped {
				for _, l := range replaced {
					if !yield(l, nil) {
						return
					}
				}
				continue
			}
			out.Mtime = mtime
			if !yield(out, nil) {
				return
			}
		}
	}
}

// lossKey names one loss marker.
type lossKey struct {
	v core.Version
	e core.Extent
}

// sinceRead is one Since's progress. It reuses reading for the Corruption a
// record that does not verify is handed to Corrupt as.
type sinceRead struct {
	reading
	from    core.Version
	dropped map[index.RecordAt]bool // records Corrupt dropped during this iteration
	seen    map[lossKey]bool        // loss markers the copy held or this iteration yielded in a write's place
}

// write reads ch's record and returns the modification time to yield with
// its extent. When the record was dropped, dropped is true and replaced holds
// the losses to yield in place of the extent, none for an unsynced drop.
// again allows one re-resolve of a record whose segment was retired.
func (r *sinceRead) write(ch index.Change, bp *[]byte, again bool) (mtime int64, replaced []core.Change, dropped bool, err error) {
	if r.dropped[ch.Record] {
		return 0, nil, true, nil
	}
	pc := index.Placed{Extent: ch.Extent, Record: ch.Record, Skip: ch.Skip, Version: ch.Version, Seq: ch.Seq}
	g := recordPieces{at: ch.Record, pieces: []index.Placed{pc}}
	_, h, mtime, verdict, err := r.c.readRecord(r.ctx, r.st, r.id, ch.Record, g.pieces, bp)
	switch {
	case segmentGone(err):
		return r.resolveAgain(ch, bp, again)
	case err != nil:
		return 0, nil, false, err
	case verdict == recordVerifies:
		return mtime, nil, false, nil
	}
	// decision: an index entry with sequence number 0 is damage the open
	// should have refused or an index bug, and no header can match 0. Since
	// returns an error and drops nothing, as a read does.
	if ch.Seq == 0 {
		return 0, nil, false, fmt.Errorf("journal: index entry of file %v at %+v carries no sequence number", r.id, ch.Record)
	}
	out, err := r.c.dur.Corrupt(r.ctx, r.corruption(g, h, verdict))
	if err != nil {
		return 0, nil, false, err
	}
	if r.c.afterCorrupt != nil {
		r.c.afterCorrupt()
	}
	if out.Kind == durability.CorruptUseCopy {
		at := index.RecordAt{Segment: out.Copy.Segment, Offset: uint32(out.Copy.Off)}
		_, _, mtime, verdict, err = r.c.readRecord(r.ctx, r.st, r.id, at, g.pieces, bp)
		switch {
		case segmentGone(err):
			return r.resolveAgain(ch, bp, again)
		case err == nil && verdict != recordVerifies:
			// decision: a copy that does not verify here is not handed to
			// Corrupt again, as in a read, and its extent is yielded with an
			// Mtime of 0, which verifies nothing; the next read of it finds
			// the copy bad. Overturned if Since must drop what it cannot verify.
			return 0, nil, false, nil
		case err != nil:
			return 0, nil, false, err
		}
		return mtime, nil, false, nil
	}
	r.dropped[ch.Record] = true
	return 0, r.lossesAt(ch.Version), true, nil
}

// resolveAgain handles a record whose segment was retired after the copy was
// taken. When the index names the same record, by sequence number, at another
// place, as a repack leaves it, the write is read from there, once. Otherwise
// the record is taken as dropped: the losses at its version are yielded in its
// place, none when it was superseded or dropped unsynced.
//
// decision: a record whose segment is gone and that the index still names at
// the same place, or names nowhere, is treated as dropped, not yielded with an
// unverified Mtime of 0, which the format reserves for a suspended write.
// Overturned if a retirement can leave the index naming a segment it removed
// for content still held.
func (r *sinceRead) resolveAgain(ch index.Change, bp *[]byte, again bool) (int64, []core.Change, bool, error) {
	if !again {
		if cur, ok := r.c.index.Lookup(r.id, ch.Extent.Off); ok && cur.Seq == ch.Seq && cur.Record != ch.Record && cur.Extent.Off <= ch.Extent.Off {
			ch.Record, ch.Skip = cur.Record, cur.Skip+uint32(ch.Extent.Off-cur.Extent.Off)
			return r.write(ch, bp, true)
		}
	}
	r.dropped[ch.Record] = true
	return 0, r.lossesAt(ch.Version), true, nil
}

// lossesAt returns the loss markers of the file at version v that the copy
// did not hold and this iteration has not yielded, in offset order. The marker
// of a synced loss is visible once Corrupt returns, whoever dropped the
// record, so these are the losses to yield in place of a dropped write.
func (r *sinceRead) lossesAt(v core.Version) []core.Change {
	var out []core.Change
	for _, m := range r.c.index.Since(r.id, r.from) {
		k := lossKey{m.Version, m.Extent}
		if m.Kind == core.ChangeLoss && m.Version == v && !r.seen[k] {
			r.seen[k] = true
			out = append(out, core.Change{Kind: core.ChangeLoss, Extent: m.Extent, Version: m.Version})
		}
	}
	slices.SortFunc(out, func(a, b core.Change) int { return cmp.Compare(a.Extent.Off, b.Extent.Off) })
	return out
}
