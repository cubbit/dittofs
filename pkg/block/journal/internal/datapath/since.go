package datapath

import (
	"cmp"
	"context"
	"errors"
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
// not yielded unless the index holds it at another place by then, where it is
// read from, and when the drop was of a synced record the loss it left is
// yielded in its place. A segment retired since the copy was taken sends the
// read on, a bounded number of times, to the record the index now names for
// each piece of the extent, as a repack leaves it; a piece the index no longer
// holds is left out, and a record that keeps moving or whose retired segment
// the index still names ends the iteration with errRecordMoving.
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
			items, err := sr.write(ch, bp, 0)
			if err != nil {
				yield(core.Change{}, err)
				return
			}
			for _, it := range items {
				if !yield(it, nil) {
					return
				}
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

// maxRetargets bounds how many times one change is sent on to the record the
// index names for it after the record it read was retired.
const maxRetargets = 8

// errRecordMoving ends a Since whose record kept moving or whose retired
// segment the index still names, rather than leave a held extent unreported.
var errRecordMoving = errors.New("journal: a record's segment was retired while the index still names it")

// write reads ch's record and returns what to yield for it: the write with its
// modification time, one write per piece when the record moved and the extent
// is now held in pieces, or the losses of a dropped record, none for an
// unsynced drop. depth counts the retargets so far.
func (r *sinceRead) write(ch index.Change, bp *[]byte, depth int) ([]core.Change, error) {
	if r.dropped[ch.Record] {
		return r.retarget(ch, bp, depth, nil)
	}
	pc := index.Placed{Extent: ch.Extent, Record: ch.Record, Skip: ch.Skip, Version: ch.Version, Seq: ch.Seq}
	g := recordPieces{at: ch.Record, pieces: []index.Placed{pc}}
	_, h, mtime, verdict, err := r.c.readRecord(r.ctx, r.st, r.id, ch.Record, g.pieces, bp)
	switch {
	case segmentGone(err):
		return r.retarget(ch, bp, depth, err)
	case err != nil:
		return nil, err
	case verdict == recordVerifies:
		return []core.Change{r.written(ch, mtime)}, nil
	}
	// decision: an index entry with sequence number 0 is damage the open
	// should have refused or an index bug, and no header can match 0. Since
	// returns an error and drops nothing, as a read does.
	if ch.Seq == 0 {
		return nil, fmt.Errorf("journal: index entry of file %v at %+v carries no sequence number", r.id, ch.Record)
	}
	out, err := r.c.dur.Corrupt(r.ctx, r.corruption(g, h, verdict))
	if err != nil {
		return nil, err
	}
	if r.c.afterCorrupt != nil {
		r.c.afterCorrupt()
	}
	if out.Kind == durability.CorruptUseCopy {
		at := index.RecordAt{Segment: out.Copy.Segment, Offset: uint32(out.Copy.Off)}
		_, _, mtime, verdict, err = r.c.readRecord(r.ctx, r.st, r.id, at, g.pieces, bp)
		switch {
		case segmentGone(err):
			// decision: reached once Corrupt's hold on the copy's segment
			// ends, which it does when the worker has re-appended the record
			// and the new place completed; the segment can then be retired
			// before this re-read, and the read follows the index. While a
			// repack or retirement still holds the claim the index names the
			// copy, and retarget ends the Since with errRecordMoving, which
			// fails safe. Overturned by a retirement that can take the
			// segment while the hold stands.
			return r.retarget(ch, bp, depth, err)
		case err == nil && verdict != recordVerifies:
			// decision: a copy that does not verify here is not handed to
			// Corrupt again, as in a read, and its extent is yielded with an
			// Mtime of 0, which verifies nothing; the next read of it finds
			// the copy bad. Overturned if Since must drop what it cannot verify.
			return []core.Change{r.written(ch, 0)}, nil
		case err != nil:
			return nil, err
		}
		return []core.Change{r.written(ch, mtime)}, nil
	}
	r.dropped[ch.Record] = true
	return r.retarget(ch, bp, depth, nil)
}

// written is the change a verified write yields.
func (r *sinceRead) written(ch index.Change, mtime int64) core.Change {
	return core.Change{Kind: core.ChangeWrite, Extent: ch.Extent, Version: ch.Version, Mtime: mtime, Over: ch.Over}
}

// retarget handles a record whose segment was retired after the copy was
// taken, or that Corrupt answered dropped, which it also does for a record that
// only moved. Each piece of ch's extent that the index still names with ch's
// sequence number, at another record, is read from there as a change of its
// own, as a repack leaves it. A piece the index no longer holds for that record
// was superseded or removed, and is left out, as are the others of the same
// original record, which are looked up on their own. The losses at ch's version
// that the copy did not hold are yielded too, none unless the record was
// dropped, by this Since or another read.
//
// decision: a record Corrupt reports dropped is followed through the index like
// a retired one, because Corrupt answers dropped also for a record that moved
// after the copy was taken; a piece still named at the dropped record is a
// refused drop, which its loss marker stands for, and is left out. For a piece
// the index no longer holds, the losses at that version are yielded.
//
// decision: nothing is marked dropped for a lookup that missed. A record split
// into pieces can lose one to a newer write and have another moved by a repack,
// so a miss says only that this change's extent is gone; only Corrupt dropping
// the record marks it. Each retarget follows an index that only moves records
// forward, so a change settles in a few steps, and maxRetargets caps a ping-pong
// that never does.
//
// decision: a piece the index still names at the very record whose segment is
// gone ends the Since with errRecordMoving instead of being skipped: a held
// extent must never be omitted without an error. So does a change retargeted
// maxRetargets times. Overturned by a retirement that may leave the index
// naming a segment it removed for content that is still held, which would need
// the iteration to wait for the repoint.
func (r *sinceRead) retarget(ch index.Change, bp *[]byte, depth int, gone error) ([]core.Change, error) {
	if depth >= maxRetargets {
		return nil, fmt.Errorf("journal: the record of file %v at %+v moved %d times during one Since: %w", r.id, ch.Record, depth, errRecordMoving)
	}
	var out []core.Change
	placed, _ := r.c.index.Span(r.id, ch.Extent)
	for _, pc := range placed {
		if pc.Seq != ch.Seq || pc.Version != ch.Version {
			continue
		}
		if pc.Record == ch.Record {
			if r.dropped[ch.Record] {
				continue
			}
			return nil, fmt.Errorf("journal: the record of file %v at %+v is gone but still held: %w: %w", r.id, ch.Record, errRecordMoving, gone)
		}
		sub := ch
		sub.Extent, sub.Record, sub.Skip = pc.Extent, pc.Record, pc.Skip
		sub.Over = clipTo(ch.Over, pc.Extent)
		if r.c.afterRetarget != nil {
			r.c.afterRetarget()
		}
		items, err := r.write(sub, bp, depth+1)
		if err != nil {
			return nil, err
		}
		out = append(out, items...)
	}
	return append(out, r.lossesAt(ch.Version)...), nil
}

// clipTo returns the parts of runs inside e.
func clipTo(runs []core.Extent, e core.Extent) []core.Extent {
	var out []core.Extent
	for _, r := range runs {
		lo, hi := max(r.Off, e.Off), min(r.Off+r.Len, e.Off+e.Len)
		if lo < hi {
			out = append(out, core.Extent{Off: lo, Len: hi - lo})
		}
	}
	return out
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
