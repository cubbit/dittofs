package journal

import (
	"context"
	"iter"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/datapath"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/index"
)

// Inspect returns a way to read out the journal's placement index, per file
// and in bulk, read its per-segment accounting, and verify the index against
// the segments. None of it mutates the journal or stops it.
func (j *Journal) Inspect() Inspector {
	return Inspector{c: j.core}
}

// Inspector reads out a journal's index and segments. Its methods are safe for
// concurrent use with every other operation on the journal. After the journal
// closes, File and Verify fail with ErrClosed, and All and Segments return
// nothing.
//
// decision: All and Segments have no error to return, so after Close they
// return nothing rather than a view of state the journal has released.
type Inspector struct{ c *datapath.Core }

// File returns the index entries of file id, in offset order.
func (in Inspector) File(id FileID) ([]IndexEntry, error) {
	if in.c.Closed() {
		return nil, ErrClosed
	}
	return entriesFrom(in.c.Index().FileEntries(id)), nil
}

// All yields the index entries of every file in the index, one file at a time.
func (in Inspector) All() iter.Seq2[FileID, []IndexEntry] {
	return func(yield func(FileID, []IndexEntry) bool) {
		if in.c.Closed() {
			return
		}
		for id, es := range in.c.Index().InspectAll() {
			if !yield(id, entriesFrom(es)) {
				return
			}
		}
	}
}

// entriesFrom maps the index's read-out of one file to the public shape. The
// copy is fresh, so the caller may keep and change it; a file holding no entry
// answers no slice rather than an empty one.
//
// decision: each entry reports its file's owner tag, so on a rivalled file —
// one the rebuild answered under two tags, whose every call is refused until
// one tag is forgotten — every entry names the owner, including the ones the
// rival's records back, because the index keeps one tag per file and no tag
// per piece. Overturned by the index keeping a tag per piece, or by recovery
// parking a rival's records out of the read-out until one tag is forgotten.
func entriesFrom(es []index.Entry) []IndexEntry {
	if len(es) == 0 {
		return nil
	}
	out := make([]IndexEntry, len(es))
	for i, e := range es {
		out[i] = IndexEntry{
			Kind:      EntryKind(e.Kind),
			Extent:    e.Extent,
			Version:   e.Version,
			Tag:       e.Tag,
			Location:  RecordRef{Segment: e.Record.Segment, Offset: int64(e.Record.Offset)},
			Offloaded: e.Offloaded,
			By:        e.By,
			Removal:   e.Removal,
			Clone:     e.Clone,
		}
	}
	return out
}

// dirtySeed derives the dirty-file seed of a rebuilt index: one became-dirty
// delta per file that holds or retains an extent whose offloaded bit is unset,
// the whole content of the list an open starts its dirty list from, so a file
// nothing seeded answers unlisted until a write of its own lands. Each delta
// carries the file's owner tag and the became-dirty transition alone, at the
// zero write time content a rebuild restores carries; a file whose every
// extent holds the bit, and one a removal marker left holding nothing, is no
// member. It reads the index and mutates nothing.
func dirtySeed(ix *index.Index) []index.HeldDelta {
	var out []index.HeldDelta
	for id, es := range ix.InspectAll() {
		for _, e := range es {
			if (e.Kind == index.EntryHeld || e.Kind == index.EntryRetained) && !e.Offloaded {
				out = append(out, index.HeldDelta{
					Tag:   e.Tag,
					File:  id,
					Dirty: index.BecameDirty,
				})
				break
			}
		}
	}
	return out
}

// Segments returns the accounting of every segment the journal owns. Stats
// gives the totals; this is the breakdown behind them.
//
// decision: the per-segment accounting is not built yet, so Segments answers
// nil until the step that maintains it lands.
func (in Inspector) Segments() []SegmentInfo {
	return nil
}

// Verify checks the index against the segments and reports what does not
// agree. It reports and never repairs: repair is a separate, explicit action.
//
// Verify checks each file against a stable view of that file, so a write in
// flight during the pass is never reported as a disagreement. It reads every
// segment, so it takes a context, honoured as the package doc states.
//
// decision: the index-to-segments check is not built yet, so Verify answers
// ErrNotImplemented until the pass that walks the segments and collects the
// report lands.
func (in Inspector) Verify(ctx context.Context) (VerifyReport, error) {
	return VerifyReport{}, ErrNotImplemented
}

// EntryKind is what an index entry records about its extent.
type EntryKind uint8

// The entry kinds.
const (
	// EntryHeld is an extent the journal holds: a read serves it.
	EntryHeld EntryKind = iota + 1

	// EntryRetained is superseded content kept until its superseding write is
	// synced, or until its successor's existence commits, or while a snapshot
	// hold keeps it. A read does not serve it.
	EntryRetained

	// EntryMarker is a removal not yet settled: a truncate, deallocate or
	// delete the caller has not settled.
	EntryMarker
)

// IndexEntry is one entry of a file's placement index.
type IndexEntry struct {
	Kind    EntryKind
	Extent  Extent
	Version Version

	// Tag is the tag of the share owning the file: the index keeps one owner
	// tag per file, so every entry of a file reports the same tag, and a
	// rivalled file — one rebuilt under two tags, whose every call is refused
	// until one tag is forgotten — reports its owner's on every entry.
	Tag ShareTag

	// Location is the record holding the entry's bytes; zero for a marker.
	//
	// decision: an entry is one contiguous piece of one record, so its
	// location is that record's, the same RecordRef Verify reports.
	Location RecordRef

	// Offloaded is the entry's offloaded bit. It is never an answer about
	// whether the content is offloaded: the journal is not authoritative for
	// that.
	Offloaded bool

	// By is retained: the version of the record that superseded the entry's;
	// zero for every other kind.
	By Version

	// Removal is a marker: the removal that left it; zero for every other
	// kind.
	Removal ChangeKind

	// Clone is a clone target's marker: the copy of the spec the entry keeps,
	// which the caller may change; nil for every other kind.
	Clone *CloneSpec
}

// RecordRef locates a record: the segment holding it and its offset there.
type RecordRef struct {
	Segment SegmentID
	Offset  int64
}

// SegmentInfo is one segment's accounting.
type SegmentInfo struct {
	ID     SegmentID
	Stream uint32
	Sealed bool

	// Failed reports that a sync or an append into the segment failed, so it
	// takes no further append.
	Failed bool

	// Allocated is the storage the segment is accounted at, in bytes.
	Allocated int64

	// HeldBytes is the bytes of held extents in the segment, and
	// RetainedBytes the bytes of retained ones; neither includes the other.
	HeldBytes     int64
	RetainedBytes int64

	// Damaged reports that the segment holds records that do not verify.
	Damaged bool
}

// VerifyReport is what Verify found.
type VerifyReport struct {
	// Unsupported holds, per file, the index entries no record on the
	// segments supports.
	Unsupported map[FileID][]IndexEntry

	// Unindexed are the records that need an index entry, held, retained or a
	// removal marker, but have none. A record nothing needs any more, such as
	// one superseded and past its retention or one released, is not one.
	Unindexed []UnindexedRecord

	// BadFooters are the segments whose footer did not verify.
	BadFooters []SegmentID

	// FootprintMismatch is the accounted footprint minus the sum of the
	// segments' sizes, in bytes; zero when the two agree.
	FootprintMismatch int64
}

// UnindexedRecord is a record that needs an index entry but has none.
type UnindexedRecord struct {
	File    FileID
	Extent  Extent
	Version Version
	Record  RecordRef
}
