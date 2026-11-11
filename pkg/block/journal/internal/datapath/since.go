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
// version, markers come before extents, then extents in offset order. An
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
// yielded in its place, from the extents Corrupt says it lost. A segment retired since the copy was taken yields the
// extent with an Mtime of 0.
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
		defer exit()
		if err := c.checkTag(s, id); err != nil {
			yield(core.Change{}, err)
			return
		}
		changes := c.index.Since(id, from)
		sr := sinceRead{reading: reading{c: c, ctx: ctx, id: id, st: c.streams.For(id)}, dropped: map[index.RecordAt]bool{}}
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
			mtime, replaced, err := sr.write(ch, bp)
			if err != nil {
				yield(core.Change{}, err)
				return
			}
			if replaced != nil {
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

// sinceRead is one Since's progress. It reuses reading for the Corruption a
// record that does not verify is handed to Corrupt as.
type sinceRead struct {
	reading
	dropped map[index.RecordAt]bool // records Corrupt dropped during this iteration
}

// write reads ch's record and returns the modification time to yield with
// its extent. When the record was dropped it returns replaced instead, the
// losses to yield in place of the extent, which is empty for an unsynced drop
// and non-nil only to say "the extent is not yielded".
func (r *sinceRead) write(ch index.Change, bp *[]byte) (mtime int64, replaced []core.Change, err error) {
	if r.dropped[ch.Record] {
		return 0, []core.Change{}, nil
	}
	pc := index.Placed{Extent: ch.Extent, Record: ch.Record, Skip: ch.Skip, Version: ch.Version, Seq: ch.Seq}
	g := recordPieces{at: ch.Record, pieces: []index.Placed{pc}}
	_, h, mtime, verdict, err := r.c.readRecord(r.ctx, r.st, r.id, ch.Record, g.pieces, bp)
	switch {
	case segmentGone(err):
		return 0, nil, nil
	case err != nil:
		return 0, nil, err
	case verdict == recordVerifies:
		return mtime, nil, nil
	}
	// decision: an index entry with sequence number 0 is damage the open
	// should have refused or an index bug, and no header can match 0. Since
	// returns an error and drops nothing, as a read does.
	if ch.Seq == 0 {
		return 0, nil, fmt.Errorf("journal: index entry of file %v at %+v carries no sequence number", r.id, ch.Record)
	}
	out, err := r.c.dur.Corrupt(r.ctx, r.corruption(g, h, verdict))
	if err != nil {
		return 0, nil, err
	}
	if out.Kind == durability.CorruptUseCopy {
		at := index.RecordAt{Segment: out.Copy.Segment, Offset: uint32(out.Copy.Off)}
		_, _, mtime, verdict, err = r.c.readRecord(r.ctx, r.st, r.id, at, g.pieces, bp)
		switch {
		case segmentGone(err), err == nil && verdict != recordVerifies:
			// decision: a copy that does not verify here is not handed to
			// Corrupt again, as in a read, and its extent is yielded with an
			// Mtime of 0, which verifies nothing; the next read of it finds
			// the copy bad. Overturned if Since must drop what it cannot verify.
			return 0, nil, nil
		case err != nil:
			return 0, nil, err
		}
		return mtime, nil, nil
	}
	r.dropped[ch.Record] = true
	replaced = []core.Change{}
	for _, e := range out.Lost {
		replaced = append(replaced, core.Change{Kind: core.ChangeLoss, Extent: e, Version: ch.Version})
	}
	slices.SortFunc(replaced, func(a, b core.Change) int { return cmp.Compare(a.Extent.Off, b.Extent.Off) })
	return 0, replaced, nil
}
