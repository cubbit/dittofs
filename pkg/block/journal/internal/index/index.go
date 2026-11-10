package index

import "github.com/cubbit/dittofs/pkg/block/journal/internal/core"

// Index is the placement index of one journal: the exported handle over the
// file table. Every method takes only the lock of the file it names, and only
// for the one change or read it makes, so no call waits on another file and
// none does I/O.
type Index struct{ fs files }

// New returns an empty index that refuses a change past extentLimit entries.
func New(extentLimit int64) *Index {
	ix := &Index{}
	ix.fs.setExtentLimit(extentLimit)
	return ix
}

// WriteRetain publishes a client write of p to id through tag: it checks the
// file's owner, admits the write against the extent bound, notes the record
// with p's version and seq, and holds p under seq, retaining what it
// supersedes as superseded by p's version. p.Seq is ignored: the record's
// sequence number is seq. It takes the file's lock exclusively. A refusal
// holds nothing.
func (ix *Index) WriteRetain(tag core.ShareTag, id core.FileID, p Placed, seq core.Seq) (HeldDelta, error) {
	var err error
	if ix.fs.view(id, func(f *file) { err = f.check(tag, id) }) && err != nil {
		return HeldDelta{}, err
	}
	p.Seq = seq
	adm, err := ix.fs.admitWrite(id, p.Extent, true)
	if err != nil {
		return HeldDelta{}, err
	}
	return ix.fs.commit(id, adm, func(f *file) error {
		if err := f.recorded(p.Record.Segment, p.Extent, Numbers{Version: p.Version, Seq: seq}, tag, id); err != nil {
			return err
		}
		f.writeRetain(p)
		return nil
	})
}

// Lose applies a synced loss of e at v to id under the file's lock, with no
// admission, and returns the change to the file's held state. A zero-length e
// is refused with core.ErrZeroLength and changes nothing.
func (ix *Index) Lose(id core.FileID, e core.Extent, v core.Version) (HeldDelta, error) {
	return ix.fs.lose(id, e, v)
}

// Lookup returns the held piece of id that covers off, and false when none
// does or the file has no entry. It takes the file's lock shared and creates
// no entry.
func (ix *Index) Lookup(id core.FileID, off int64) (Placed, bool) {
	var p Placed
	var ok bool
	ix.fs.view(id, func(f *file) { p, ok = f.held.lookup(off) })
	return p, ok
}

// Span returns the pieces of id held inside e, clipped to it, and the exact
// gaps between them, both in offset order. A file with no entry has nothing
// held, so a non-empty e comes back wholly missing. Neighbouring pieces of one
// record that differ only in their offloaded bit are returned as one. It takes
// the file's lock shared, does no I/O and creates no entry.
func (ix *Index) Span(id core.FileID, e core.Extent) (placed []Placed, missing []core.Extent) {
	if !ix.fs.view(id, func(f *file) { placed, missing = f.held.span(e, nil, nil) }) && e.Len > 0 {
		missing = []core.Extent{e}
	}
	return placed, missing
}

// Retained returns a copy of id's retained pieces, in offset, version and
// superseder order. It takes the file's lock shared and creates no entry.
func (ix *Index) Retained(id core.FileID) []Retained {
	var out []Retained
	ix.fs.view(id, func(f *file) { out = f.retained.list(nil) })
	return out
}

// SegmentBytes returns the bytes seg backs as held and as retained extents. It
// takes no file's lock.
func (ix *Index) SegmentBytes(seg core.SegmentID) SegmentBytes { return ix.fs.segmentBytes(seg) }

// FileEntries returns a copy of id's placement index entries in read-out
// order, held and retained pieces and removal markers alike; nil when the
// file holds none. It takes the file's lock shared and creates no entry.
//
// decision: exported so the datapath tests can see a file's entries, and a
// later inspection read-out is expected to take it over. It is withdrawn if
// the inspection path does not use it.
func (ix *Index) FileEntries(id core.FileID) []Entry { return ix.fs.inspect(id) }

// AsOf returns the change version a read of id reports.
//
// It takes the file's lock shared and creates no entry. A caller that needs
// the version to never name a change its span did not see reads it with the
// file's append domain held.
func (ix *Index) AsOf(id core.FileID) core.Version { return ix.fs.asOf(id) }

// RaiseFloor raises every floor bucket to floor's counter, clamped to the
// greatest version as the counters clamp it, so a file with no entry reports
// at least the floor and a fill read below it is refused. It never lowers a
// bucket and is safe beside readers. The floor's epoch must already be
// validated: the counters refuse a non-zero one first.
func (ix *Index) RaiseFloor(floor core.Version) {
	ix.fs.raiseBuckets(min(floor.Counter, maxVersion))
}
