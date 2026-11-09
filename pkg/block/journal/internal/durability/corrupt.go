package durability

import (
	"context"
	"fmt"
	"slices"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/index"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/segment"
)

// Corruption is a record a reader found not to verify.
type Corruption struct {
	Share core.ShareTag
	File  core.FileID
	// Record is where the record sits.
	Record  index.RecordAt
	Seq     core.Seq
	Version core.Version
	// Extent is the record header's extent, named by the loss when the record
	// holds no piece any more.
	Extent core.Extent
	// AtOpen says the caller proved whether the record was synced, as recovery
	// does, so Corrupt reads no ledger and takes Synced as the answer.
	AtOpen bool
	// Synced is honoured only with AtOpen.
	Synced bool
	// DomainHeld says the caller holds File's append domain, which Corrupt
	// then neither takes nor releases.
	DomainHeld bool
}

// CorruptKind is what Corrupt did with a record.
type CorruptKind uint8

const (
	// CorruptDropped is a record whose extents left the index: the caller
	// reports them missing.
	CorruptDropped CorruptKind = iota + 1
	// CorruptUseCopy is a record served from another copy of it.
	CorruptUseCopy
)

// CorruptOutcome is Corrupt's answer.
type CorruptOutcome struct {
	Kind CorruptKind
	// Copy is the copy to read, set for CorruptUseCopy.
	Copy segment.Location
}

// Corrupt handles a record a reader found not to verify. It first looks for
// another copy of the record with the same sequence number in the copy table
// and reads it, with no lock of this layer held. A copy that verifies is
// served: the index is repointed to it under the file's domain, no loss is
// recorded, and Corrupt returns CorruptUseCopy with its location. A read of
// the copy that fails with an error that shows nothing about it is returned
// as that error, with nothing marked or dropped (readCopy says which). With no
// copy, or one that does not verify, it drops the extents
// the record backs from the index, raises the loss generation for a dirty
// one, and hands the loss record to the stream's worker. It returns
// CorruptDropped, after which the caller reports the extents missing. The
// read appends nothing, takes no segment append domain and waits on no sync:
// a loss record that cannot be appended reaches only the worker, where the
// drop stands, so the error is nil whatever the append does.
//
// Whether the record was synced is decided under the stream's mutex in the
// same step that takes it out of the ledger or out of a pending window, so a
// sync or a failed window the worker handles meanwhile cannot make the answer
// stale. A record still unsynced has its entry taken out and what it
// superseded held again. A record a sync covered is synced: its entry stays
// for the worker, and what it superseded is lifted here, as the worker would
// on completion, so the loss masks it. A record taken out of a pending window
// is one the worker will neither fail nor report, so the file is marked owed
// the window's report in that same step. A record the worker already failed,
// or one the index no longer names, is reported dropped with nothing else.
//
// decision: Corrupt is called only for a checksum or a header-against-catalog
// mismatch, never for an I/O error. A read that fails with EIO has not shown
// the record bad, and the device may succeed on a retry, so the caller returns
// that error and drops nothing. Overturned if an EIO read is ruled to drop the
// extent, which would then call Corrupt for it too.
//
// decision: the generation is raised here, by the reader, once per call that
// drops a piece whose offloaded bit is unset, before the loss is handed to the
// worker, and the loss says so with Raised. It lives in memory only, like
// every raise. A reader that gets the extent missing then finds the generation
// up, though the loss record and the event follow on the worker. Overturned if
// a raise must wait for the loss record to be durable.
func (d *Durability) Corrupt(ctx context.Context, c Corruption) (CorruptOutcome, error) {
	if err := ctx.Err(); err != nil {
		return CorruptOutcome{}, err
	}
	st := d.streamOf(c.File)
	if st == nil {
		return CorruptOutcome{}, fmt.Errorf("durability: the layer is not started: %w", core.ErrClosed)
	}
	// Another copy of the record is read before any lock of this layer is
	// taken, and a read that shows nothing about it returns here, before
	// anything is marked or dropped.
	cp, haveCopy := d.copyOf(c.Seq)
	if haveCopy {
		verdict, err := d.readCopy(st, cp)
		if err != nil {
			return CorruptOutcome{}, err
		}
		haveCopy = verdict == copyVerifies
	}

	unlock := func() {}
	if !c.DomainHeld {
		unlock = d.domains.Lock(c.File)
	}
	if haveCopy {
		if out, ok := d.useCopy(st, c, cp); ok {
			unlock()
			return out, nil
		}
	}
	return d.dropCorrupt(st, c, unlock), nil
}

// dropCorrupt is Corrupt's drop of a record with no copy to serve: it runs
// under c.File's append domain, which unlock releases before it returns, and
// hands the loss to the stream's worker. The worker calls it too, for a held
// record whose copy's segment was retired.
func (d *Durability) dropCorrupt(st *stream, c Corruption, unlock func()) CorruptOutcome {
	// The segment is counted damaged here, on the caller's goroutine and before
	// any return below, so the count reads 1 from the moment Corrupt returns
	// whatever the index or the worker had already done with the record; the
	// worker, which only records the loss, would leave a window where it still
	// read 0.
	d.markDamaged(c.Record.Segment)
	dropped := CorruptOutcome{Kind: CorruptDropped}

	synced, known := c.Synced, c.AtOpen
	if !known {
		var lost bool
		synced, lost = st.takeCorrupt(c)
		if lost {
			unlock()
			return dropped
		}
	}

	// A synced record lifted what it superseded when it completed, or the
	// worker is about to; lifting now first keeps the drop below from holding
	// it again, and leaves the worker's own lift nothing to do.
	if synced {
		d.lift(c.File, c.Version, c.Extent)
	}
	fw, hd, err := d.index.FailWindow(c.File, []index.WindowRecord{{Record: c.Record, Version: c.Version, Extent: c.Extent}})
	if err != nil {
		d.log.Error("journal could not drop a corrupt record from the index", "file", c.File, "seq", c.Seq, "err", err)
	}
	d.accountDelta(hd)
	for _, cv := range fw.Covers {
		if !d.hasEntry(st, c.File, cv) {
			d.lift(c.File, cv.Version, cv.Extent)
		}
	}
	if d.afterCorruptDrop != nil {
		d.afterCorruptDrop(&fw)
	}

	job := corruptJob{record: c.Record, synced: synced}
	dirty := false
	base := Loss{
		Share:   c.Share,
		File:    c.File,
		Version: c.Version,
		Seq:     c.Seq,
		Reason:  core.LossCorrupt,
		Synced:  synced,
		AtOpen:  c.AtOpen,
		Raised:  true,
	}
	for _, p := range fw.Dropped {
		if p.Record != c.Record {
			continue
		}
		l := base
		l.Extent, l.Offloaded = p.Extent, p.Offloaded
		job.losses = append(job.losses, l)
		dirty = dirty || !p.Offloaded
	}
	if len(job.losses) == 0 && err != nil {
		// The index refused the drop, so it may still name the record: the
		// loss is named by the header extent, as dirty, whatever it holds.
		l := base
		l.Extent = c.Extent
		job.losses = append(job.losses, l)
		dirty = true
	}
	if len(job.losses) == 0 {
		// The index no longer names the record: an overwrite or an earlier
		// drop took it, and nothing it held is lost here.
		unlock()
		return dropped
	}
	// Raised before the domain is let go, so a read of the file that finds the
	// extent missing finds the generation up.
	if dirty {
		d.dir.raise()
	}
	unlock()

	d.queueCorrupt(st, job)
	return dropped
}

// streamOf returns the stream state of the stream file is appended to, or nil
// before Start.
func (d *Durability) streamOf(file core.FileID) *stream {
	ss := d.streams.Load()
	if ss == nil {
		return nil
	}
	s := ss.For(file)
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, st := range d.made {
		if st.s == s {
			return st
		}
	}
	return nil
}

// takeCorrupt decides, in one step under mu, whether the record c names
// was synced, and takes it out of the ledger or a pending window when it was
// not. lost reports a record the worker already failed, which has nothing
// left to drop.
//
// decision: a record in neither the ledger, a pending window nor the lost set
// is synced. Every record Track saw that left the ledger otherwise was
// completed, which is after its sync, and a record taken out by an earlier
// Corrupt is no longer in the index, so the answer reaches nothing for it. It
// is the answer Durable gives for such a location. The segment's synced value
// cannot decide it: the worker forgets it once the segment holds no entry.
// Overturned by a record that can leave the ledger unsynced some other way.
//
// ponytail: the record is found by a linear scan of the ledger, the pending
// windows and the lost set, O(ledger + window + lost) per call, and the lost
// set is never pruned. Upgrade to an index by record when a read that finds
// many corrupt records shows the scan in a profile.
func (st *stream) takeCorrupt(c Corruption) (synced, lost bool) {
	st.mu.Lock()
	defer st.mu.Unlock()
	at := func(w segment.Written) bool { return recordAt(w) == c.Record }
	for i := range st.ledger {
		e := &st.ledger[i]
		if !at(e.w) {
			continue
		}
		if st.covered(e.w.Loc) {
			// Synced: the worker completes the entry and lifts what the
			// record superseded, whether or not it has yet.
			return true, false
		}
		file, seg := e.w.File, e.w.Loc.Segment
		st.removeLocked(func(e *entry) bool { return at(e.w) }, nil)
		// The segment may have failed already, with its window not yet taken:
		// the window will not hold the record, so the file is owed its report
		// now, and takeWindow turns that into the window's mark.
		if st.segmentFailedLocked(seg) && !slices.Contains(st.owed[seg], file) {
			if st.owed == nil {
				st.owed = map[core.SegmentID][]core.FileID{}
			}
			st.owed[seg] = append(st.owed[seg], file)
		}
		st.d.notifyAll()
		st.wake()
		return false, false
	}
	for _, w := range st.windows {
		for i := range w.entries {
			if !at(w.entries[i].w) {
				continue
			}
			file := w.entries[i].w.File
			w.entries = slices.Delete(w.entries, i, i+1)
			st.markLocked(w, file)
			st.d.notifyAll()
			// A window left empty is resolved on the worker's next pass.
			st.wake()
			return false, false
		}
	}
	for loc := range st.lost {
		if loc.Segment == c.Record.Segment && loc.Off == int64(c.Record.Offset) {
			return false, true
		}
	}
	return true, false
}

// segmentFailedLocked reports whether seg has failed and its end event, which
// takeWindow turns into the window, is still to be taken: the worker counted
// its failed sync and ended it, a sync found it ended, or the event is queued.
// The caller holds mu. An event stays queued until takeWindow pops it in the
// step that makes its window, so no gap opens between the two.
//
// decision: a failure counts from when this layer records it: once syncStep
// has recorded the worker's failed sync, or once the end event is queued, not
// from the failed fsync or from the stream's own end of the segment. A
// Corrupt before that point is read as before the failure: the record leaves
// the ledger, the window is later taken without it, and its file is told
// nothing of the window. Overturned if the stream reports a failure to this
// layer before it ends the segment.
//
// decision: a failure recorded in st.failed alone is not enough. A sync the
// stream refused without an fsync records one with no end event to follow,
// and an owed report no window takes would hold every Sync of the file for
// good. Such a segment's record is dropped with no mark, as one of a segment
// that never failed. Overturned if a refusal without an fsync ever ends the
// segment.
func (st *stream) segmentFailedLocked(seg core.SegmentID) bool {
	if _, counted := st.counted[seg]; counted {
		return true
	}
	if _, ended := st.ending[seg]; ended {
		return true
	}
	return slices.ContainsFunc(st.ends, func(ev segment.EndEvent) bool { return ev.Segment == seg })
}

// owesLocked reports whether a file of ids is owed the report of a failed
// segment whose window is not taken yet. The caller holds mu.
func (st *stream) owesLocked(ids []core.FileID) bool {
	for _, files := range st.owed {
		for _, id := range ids {
			if slices.Contains(files, id) {
				return true
			}
		}
	}
	return false
}
