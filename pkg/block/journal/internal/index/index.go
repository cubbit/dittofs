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

// CheckWrite predicts a content write of e on id against the index's bound
// without reserving: it returns the refusal the write's admission would —
// a NoSpaceError naming the index, the shortfall in entries, counted by the
// index's refusals counter — or nil when the bound would admit the write. It
// reserves nothing and changes no entry, so a caller that checked and then
// admits finds the index where the check left it. It creates no entry and
// takes the named file's lock shared, exactly as the admission's own pre-check
// does.
func (ix *Index) CheckWrite(id core.FileID, e core.Extent) error {
	return ix.fs.checkWrite(id, e, false)
}

// FillGaps returns the sub-extents of e where a fill at v would hold
// content: where nothing is held and no removal marker above v masks — a
// marker at v masks nothing, since only content below a marker's version is
// excluded — in offset order. It takes the file's lock shared, as a read, and
// creates no entry: a file with no entry holds nothing, so a positive e comes
// back whole, and a zero-length e has no gaps.
func (ix *Index) FillGaps(id core.FileID, e core.Extent, v core.Version) []core.Extent {
	var gaps []core.Extent
	var placed []Placed
	if e.Len <= 0 {
		return gaps
	}
	if !ix.fs.view(id, func(f *file) {
		f.fillParts(Placed{Extent: e, Version: v}, func(part Placed) {
			placed, gaps = f.held.span(part.Extent, placed, gaps)
		})
	}) {
		gaps = []core.Extent{e}
	}
	return gaps
}

// CheckFill predicts a fill on id against the file's tag and staleness
// without reserving: it returns the refusal the fill itself would —
// ErrOtherShare when the file is another share's, then ErrStaleFill when the
// version id is checked against stands above asOf, the comparison files.fill
// makes. It checks no bound: CheckFillIndexBound is the read-only one, and Fill
// admits the real one. It claims nothing, creates no entry and takes the named file's
// lock shared.
func (ix *Index) CheckFill(tag core.ShareTag, id core.FileID, asOf core.Version) error {
	return ix.fs.checkFill(tag, id, asOf)
}

// Fill holds the parts of p a fill at p's version holds — where nothing is
// held, outside every marker above that version — as offloaded, under seq
// rather than p.Seq, claiming id for tag when both checks pass, and returns
// the change the fill made to the file's held state. It is refused with
// the claim's OtherShareError when another share owns the file, and with
// core.ErrStaleFill — matched with errors.Is — when the version id is
// checked against stands above asOf. A fill changes no change version, so a
// read's AsOf is unmoved. A fill refused by the tag check changes
// nothing; one refused as stale or by the bound notes its record in the file's
// drop ranges and claims the file, the record being in its segment already.
// It admits the entries it adds against the extent bound, as a write does, and
// is refused with a NoSpaceError naming the index when they would raise the
// count above it; a refusal reserves nothing. It takes the file's lock
// exclusively.
func (ix *Index) Fill(tag core.ShareTag, id core.FileID, p Placed, seq core.Seq, asOf core.Version) (HeldDelta, error) {
	return ix.fs.fill(tag, id, p, seq, asOf)
}

// Refused returns the index's refusals counter: how many refusals naming
// the index the bound has made — through an admission and a pre-check
// alike — since the index was built.
func (ix *Index) Refused() uint64 { return ix.fs.indexRefusals() }

// Lose applies a synced loss of e at v to id under the file's lock, with no
// admission, and returns the change to the file's held state. A zero-length e
// is refused with core.ErrZeroLength and changes nothing.
func (ix *Index) Lose(id core.FileID, e core.Extent, v core.Version) (HeldDelta, error) {
	return ix.fs.lose(id, e, v)
}

// Truncate stops holding every extent of id at or beyond size, narrowing a
// piece straddling it, notes the record at at under seq, claims id for tag and
// leaves a removal marker from size to the end of the file at v, and returns
// the change it made to the file's held state. The marker outlives the call
// until the caller settles it, and a rebuild makes it again from the record.
//
// It is refused with the claim's OtherShareError when another share owns the
// file, and with a NoSpaceError naming the index when the truncate would split
// a held piece and the count already stands an eighth of the limit above the
// bound. A refusal notes no record, claims nothing and changes nothing. It
// takes the file's lock exclusively and creates the entry of a file it has
// never seen, that the record's segment may outrank an older one.
//
// decision: at names the record so its segment is noted in the file's drop
// ranges, as a write's is. The rebuild does not need it — it learns the same
// segment from the record it replays — but the running journal must keep a
// segment's ranges while a removal in it outranks a record of another segment,
// and that is what the note is for. Overturned by a removal whose record never
// outranks anything, which the drop query could then answer without its
// segment.
func (ix *Index) Truncate(tag core.ShareTag, id core.FileID, size int64, v core.Version, seq core.Seq, at RecordAt) (HeldDelta, error) {
	return ix.fs.removal(tag, id, core.ChangeTruncate, toEnd(size), v, seq, at, nil)
}

// Deallocate stops holding e of id whatever its offloaded bit, notes the record
// at at under seq, claims id for tag and leaves a removal marker over e at v,
// and returns the change it made to the file's held state. A zero-length e is
// refused with core.ErrZeroLength and changes nothing. Its refusals are
// Truncate's; a deallocate splits a held piece more often than a truncate does.
func (ix *Index) Deallocate(tag core.ShareTag, id core.FileID, e core.Extent, v core.Version, seq core.Seq, at RecordAt) (HeldDelta, error) {
	return ix.fs.removal(tag, id, core.ChangeDeallocate, e, v, seq, at, nil)
}

// Delete stops holding every extent of id, notes the record at at under seq,
// claims id for tag and leaves a removal marker over the whole file at v, and
// returns the change it made to the file's held state. Its refusals are
// Truncate's.
func (ix *Index) Delete(tag core.ShareTag, id core.FileID, v core.Version, seq core.Seq, at RecordAt) (HeldDelta, error) {
	return ix.fs.removal(tag, id, core.ChangeDelete, toEnd(0), v, seq, at, nil)
}

// CloneTarget stops holding (spec.DstOff, spec.Len) of id as Deallocate does,
// notes the record at at under seq, claims id for tag, and leaves a removal
// marker keeping a copy of spec at v, so a restart resumes the clone rather
// than applying a plain removal. A zero-length spec is refused with
// core.ErrZeroLength and changes nothing. Its refusals are Deallocate's.
func (ix *Index) CloneTarget(tag core.ShareTag, id core.FileID, spec core.CloneSpec, v core.Version, seq core.Seq, at RecordAt) (HeldDelta, error) {
	return ix.fs.removal(tag, id, core.ChangeCloneTarget, core.Extent{Off: spec.DstOff, Len: spec.Len}, v, seq, at, &spec)
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
