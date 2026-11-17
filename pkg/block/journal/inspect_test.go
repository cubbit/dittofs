package journal

import (
	"bytes"
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/datapath"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/format"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/index"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/recovery"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/segment"
	"github.com/cubbit/dittofs/pkg/block/journal/journaltest/kit"
	"github.com/cubbit/dittofs/pkg/block/journal/journaltest/raw"
)

// The planted rows drive the reopen seam — recovery.Run, then a core over
// what it assembled, then the unexported constructor — so the inspector reads
// the index a journal that crashed and reopened would have served: the
// entries the rebuild rebuilt from the planted records alone.

// inspectIdentity names the planted journal, and inspectInstall the
// installation that created it.
var (
	inspectIdentity = core.JournalID{0x69, 0x6e, 0x73, 0x70}
	inspectInstall  = core.InstallationID{'t', '7', '6'}
)

// The planted segments sit high, above every id the core's spare maker draws
// from zero, so a fresh spare never meets a set the rebuilt ones own.
const (
	segPieces = core.SegmentID(910) // tag 11: a write and the one that supersedes it
	segCuts   = core.SegmentID(911) // tag 12: a truncate and a clone target beside a write
	segMarks  = core.SegmentID(912) // tag 13: the over-planted and below-version offloaded marks
	segUnmark = core.SegmentID(913) // tag 13: the offloaded extent an unmark above clears
	segOwner  = core.SegmentID(914) // the rivalled file's owner's write
	segRival  = core.SegmentID(915) // the rivalled file's rival's write
)

// The tags of the planted records, one per shape.
const (
	piecesTag = core.ShareTag(11)
	cutsTag   = core.ShareTag(12)
	marksTag  = core.ShareTag(13)
	ownerTag  = core.ShareTag(21)
	rivalTag  = core.ShareTag(22)
)

// v is a planted record's version: the counter it drew.
func v(counter uint64) Version { return Version{Counter: counter} }

// plantRecord builds one planted record of kind k under tag over [off, off+n)
// of f, at the sequence number seq and the version counter counter named,
// with a write's payload when the kind carries one.
func plantRecord(k uint8, tag core.ShareTag, f FileID, seq, counter uint64, off, n int64) raw.Record {
	r := raw.Record{
		Kind:    k,
		FileID:  f.Bytes(),
		Seq:     seq,
		Version: raw.Version{Counter: counter},
		Tag:     uint32(tag),
		Offset:  uint64(off),
		Length:  uint64(n),
	}
	if k == 1 {
		r.Write = &raw.WriteBody{MTime: 1, Payload: make([]byte, n)}
	}
	return r
}

// plantSegment writes one planted segment as a synced writer would have left
// it: the records in their order at their own slots, then the directory
// synced. It returns each record's offset in the segment, in the order the
// records were given.
func plantSegment(t *testing.T, seam *kit.Seam, id core.SegmentID, recs ...raw.Record) []uint32 {
	t.Helper()
	b := raw.SegmentHeader{Identity: inspectIdentity, ID: uint64(id)}.Bytes()
	offs := make([]uint32, len(recs))
	for i := range recs {
		recs[i].Identity = inspectIdentity
		recs[i].Segment = uint64(id)
		recs[i].RecordOffset = uint32(len(b))
		offs[i] = recs[i].RecordOffset
		b = append(b, recs[i].Bytes()...)
	}
	f, err := seam.Create(format.SegmentName(id))
	if err != nil {
		t.Fatalf("create %s: %v", format.SegmentName(id), err)
	}
	if _, err := f.WriteAt(b, 0); err != nil {
		t.Fatalf("write %s: %v", format.SegmentName(id), err)
	}
	if err := f.Sync(); err != nil {
		t.Fatalf("sync %s: %v", format.SegmentName(id), err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close %s: %v", format.SegmentName(id), err)
	}
	if err := seam.SyncDir(); err != nil {
		t.Fatalf("sync the directory after %s: %v", format.SegmentName(id), err)
	}
	return offs
}

// plantFormatFile writes the planted journal's format file as an Init would
// have left it.
func plantFormatFile(t *testing.T, seam *kit.Seam) {
	t.Helper()
	f, err := seam.Create(format.FormatFileName)
	if err != nil {
		t.Fatalf("create the format file: %v", err)
	}
	if _, err := f.WriteAt(format.AppendFormatFile(nil, format.FormatFile{
		Identity: inspectIdentity, Installation: inspectInstall,
	}), 0); err != nil {
		t.Fatalf("write the format file: %v", err)
	}
	if err := f.Sync(); err != nil {
		t.Fatalf("sync the format file: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close the format file: %v", err)
	}
	if err := seam.SyncDir(); err != nil {
		t.Fatalf("sync the directory after the format file: %v", err)
	}
}

// reopenedJournal replays one open over the planted directory, closing the
// journal when the test ends. The planted records give each read-out shape a
// file of its own:
//
//   - fileOf(1), a write over [0,64) superseded over [0,32) by a newer write,
//     so its read-out holds the newer write's [0,32) and the older one's
//     remainder [32,64);
//   - fileOf(2), a write truncated at 40 beside a clone target over [200,216),
//     so its read-out holds the truncated remainder and two removal markers;
//   - fileOf(3), a write an offloaded record above covers, so its entry
//     reports the bit set;
//   - fileOf(4), a write planted beside an offloaded record below its
//     version, so its entry reports the bit unset;
//   - fileOf(5), a write planted between an offloaded record and an unmark
//     above both, so its entry reports the bit unset;
//   - fileOf(6), written under two tags, so the rebuild answers a rivalled
//     file whose owner keeps every entry's tag.
//
// The returned offsets are each planted record's placement, keyed by its
// segment: the read-out's Location names them.
func reopenedJournal(t *testing.T) (*Journal, map[core.SegmentID][]uint32) {
	t.Helper()
	dir := t.TempDir()
	seam, err := kit.NewSeam(dir)
	if err != nil {
		t.Fatalf("the seam could not be made: %v", err)
	}
	t.Cleanup(seam.Abandon)
	plantFormatFile(t, seam)

	offs := map[core.SegmentID][]uint32{}
	offs[segPieces] = plantSegment(t, seam, segPieces,
		plantRecord(1, piecesTag, fileOf(1), 10, 10, 0, 64),
		plantRecord(1, piecesTag, fileOf(1), 11, 20, 0, 32))
	offs[segCuts] = plantSegment(t, seam, segCuts,
		plantRecord(1, cutsTag, fileOf(2), 20, 30, 0, 64),
		plantRecord(4, cutsTag, fileOf(2), 21, 31, 40, 0),
		raw.Record{
			Kind: 7, FileID: fileOf(2).Bytes(), Seq: 22,
			Version: raw.Version{Counter: 32}, Tag: uint32(cutsTag),
			Offset: 200, Length: 16,
			Clone: &raw.CloneBody{
				Share: core.ShareID{0x5}, File: fileOf(9).Bytes(), Off: 48,
				AsOf: raw.Version{Counter: 30},
			},
		})
	offs[segMarks] = plantSegment(t, seam, segMarks,
		plantRecord(1, marksTag, fileOf(3), 30, 40, 0, 64),
		plantRecord(8, marksTag, fileOf(3), 31, 41, 0, 64),
		plantRecord(1, marksTag, fileOf(4), 32, 50, 0, 64),
		plantRecord(8, marksTag, fileOf(4), 33, 49, 0, 64))
	offs[segUnmark] = plantSegment(t, seam, segUnmark,
		plantRecord(1, marksTag, fileOf(5), 40, 60, 0, 64),
		plantRecord(8, marksTag, fileOf(5), 41, 61, 0, 64),
		plantRecord(9, marksTag, fileOf(5), 42, 62, 0, 64))
	offs[segOwner] = plantSegment(t, seam, segOwner,
		plantRecord(1, ownerTag, fileOf(6), 50, 70, 0, 32))
	offs[segRival] = plantSegment(t, seam, segRival,
		plantRecord(1, rivalTag, fileOf(6), 51, 75, 32, 64))

	st, err := recovery.Run(context.Background(), recovery.Deps{
		FS:         seam.FS(),
		Clock:      kit.NewStopwatchClock(),
		SegmentCap: 64 << 10,
	}, core.Version{}, inspectIdentity, nil)
	if err != nil {
		t.Fatalf("recovery.Run over the planted directory: %v", err)
	}
	if st.Assembled.Set == nil || st.Rebuilt.Index == nil || st.Rebuilt.Counters == nil {
		t.Fatal("reopen fixture: the rebuild left the assembled state half built")
	}
	deps, state := continueFrom(st, datapath.Deps{
		FS:          seam.FS(),
		Clock:       kit.NewStopwatchClock(),
		Dir:         dir,
		Identity:    inspectIdentity,
		Streams:     2,
		SegmentCap:  64 << 10,
		ExtentLimit: 1 << 20,
		Descriptors: core.NewDescriptorBudget(64),
		Targets:     segment.Targets{Content: 4},
	})
	c, err := datapath.New(deps, state)
	if err != nil {
		t.Fatalf("the core over the rebuilt state: %v", err)
	}
	// The public order the seam stands in for: the worker starts once the
	// open's loss pass would be done.
	if err := c.StartRetiring(); err != nil {
		t.Fatalf("starting the retirement worker over the reopened set: %v", err)
	}
	j := newJournal(c)
	t.Cleanup(func() {
		if err := j.Close(); err != nil && !errors.Is(err, ErrClosed) {
			t.Errorf("Close: %v", err)
		}
	})
	return j, offs
}

// TestInspectFileMapsEachRebuiltEntry reads the read-out of the two files
// whose entries exercise the held, retained and marker shapes, and checks
// every field the mapping carries: kind, extent, version, tag, location and
// offloaded bit.
func TestInspectFileMapsEachRebuiltEntry(t *testing.T) {
	j, offs := reopenedJournal(t)

	cases := []struct {
		file FileID
		want []IndexEntry
	}{
		{
			file: fileOf(1),
			want: []IndexEntry{
				{
					Kind: EntryHeld, Extent: Extent{Off: 0, Len: 32}, Version: v(20), Tag: piecesTag,
					Location: RecordRef{Segment: segPieces, Offset: int64(offs[segPieces][1])},
				},
				{
					Kind: EntryHeld, Extent: Extent{Off: 32, Len: 32}, Version: v(10), Tag: piecesTag,
					Location: RecordRef{Segment: segPieces, Offset: int64(offs[segPieces][0])},
				},
			},
		},
	}
	in := j.Inspect()
	for _, c := range cases {
		got, err := in.File(c.file)
		if err != nil {
			t.Fatalf("File of file %v: %v", c.file, err)
		}
		if len(got) != len(c.want) {
			t.Fatalf("File of file %v got %d entries, want %d: %+v", c.file, len(got), len(c.want), got)
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Errorf("File of file %v entry %d = %+v, want %+v", c.file, i, got[i], c.want[i])
			}
		}
	}

	// The marker shapes carry what the read-out maps beside kind and extent:
	// a removal marker names the removal that left it, and a clone target's
	// keeps a copy of its spec, while every marker's location stays zero.
	es, err := in.File(fileOf(2))
	if err != nil {
		t.Fatalf("File of the truncated file: %v", err)
	}
	if len(es) != 3 {
		t.Fatalf("File of the truncated file got %d entries, want the held remainder and the two markers: %+v", len(es), es)
	}
	if got, want := es[1].Removal, ChangeTruncate; got != want {
		t.Errorf("the truncate marker's Removal = %v, want %v", got, want)
	}
	if got := es[1].Location; got != (RecordRef{}) {
		t.Errorf("the truncate marker's Location = %+v, want the zero RecordRef a marker carries", got)
	}
	if got, want := es[2].Removal, ChangeCloneTarget; got != want {
		t.Errorf("the clone marker's Removal = %v, want %v", got, want)
	}
	wantSpec := CloneSpec{
		SrcShare: ShareID{0x5}, Src: fileOf(9), SrcOff: 48,
		DstOff: 200, Len: 16, AsOf: v(30),
	}
	if es[2].Clone == nil {
		t.Fatalf("the clone marker carries no spec, want the copy of %v", wantSpec)
	}
	if *es[2].Clone != wantSpec {
		t.Errorf("the clone marker's spec = %+v, want %+v", *es[2].Clone, wantSpec)
	}
	if got, want := es[0].Removal, ChangeKind(0); got != want {
		t.Errorf("the held remainder's Removal = %v, want zero on a non-marker entry", got)
	}
	if got := es[0].By; got != (Version{}) {
		t.Errorf("the held remainder's By = %v, want zero on a non-retained entry", got)
	}
	if es[0].Clone != nil {
		t.Errorf("the held remainder carries a spec %+v, want none on a non-marker entry", *es[0].Clone)
	}
}

// TestInspectFileDecidesOffloadedBitsByThePlantedRecords checks the three
// offloaded-bit cases a rebuild answers from records alone: a mark planted
// above a write sets its bit, one planted below leaves it unset, and an unmark
// above a planted mark clears it again.
func TestInspectFileDecidesOffloadedBitsByThePlantedRecords(t *testing.T) {
	j, _ := reopenedJournal(t)
	in := j.Inspect()

	cases := []struct {
		file      FileID
		offloaded bool
		what      string
	}{
		{fileOf(3), true, "an offloaded record above the write set the bit"},
		{fileOf(4), false, "an offloaded record below the write's version set nothing"},
		{fileOf(5), false, "an unmark above the planted offloaded record cleared the bit"},
	}
	for _, c := range cases {
		es, err := in.File(c.file)
		if err != nil {
			t.Fatalf("File of file %v: %v", c.file, err)
		}
		if len(es) != 1 {
			t.Fatalf("File of file %v got %d entries, want the one write: %+v", c.file, len(es), es)
		}
		if es[0].Offloaded != c.offloaded {
			t.Errorf("the offloaded bit of file %v = %v, want %v: %s", c.file, es[0].Offloaded, c.offloaded, c.what)
		}
	}
}

// TestInspectAllYieldsEachRebuiltFileOnce checks the bulk read-out over the
// whole plant: every file the rebuild rebuilt appears exactly once, and each
// file's slice the pass reads matches File of the same file.
func TestInspectAllYieldsEachRebuiltFileOnce(t *testing.T) {
	j, _ := reopenedJournal(t)

	want := []FileID{fileOf(1), fileOf(2), fileOf(3), fileOf(4), fileOf(5), fileOf(6)}
	slices.SortFunc(want, func(a, b FileID) int {
		x, y := a.Bytes(), b.Bytes()
		return bytes.Compare(x[:], y[:])
	})

	in := j.Inspect()
	var got []FileID
	for id, es := range in.All() {
		got = append(got, id)
		file, err := in.File(id)
		if err != nil {
			t.Fatalf("File of file %v: %v", id, err)
		}
		if !sameEntries(es, file) {
			t.Errorf("All's slice of file %v = %+v, want File's read-out %+v", id, es, file)
		}
	}
	slices.SortFunc(got, func(a, b FileID) int {
		x, y := a.Bytes(), b.Bytes()
		return bytes.Compare(x[:], y[:])
	})
	if !slices.Equal(got, want) {
		t.Errorf("All yielded the files %v, want %v", got, want)
	}
}

// TestInspectOfARivalledFileReportsTheOwnersTagOnEveryEntry checks the tag
// answer the read-out gives of a file the rebuild rebuilt under two tags: the
// index keeps one owner per file, so every entry reports the owner's tag,
// including the one whose record the rival planted.
func TestInspectOfARivalledFileReportsTheOwnersTagOnEveryEntry(t *testing.T) {
	j, _ := reopenedJournal(t)

	es, err := j.Inspect().File(fileOf(6))
	if err != nil {
		t.Fatalf("File of the rivalled file: %v", err)
	}
	if len(es) != 2 {
		t.Fatalf("File of the rivalled file got %d entries, want the two writes: %+v", len(es), es)
	}
	for i, e := range es {
		if e.Tag != ownerTag {
			t.Errorf("the rivalled file's entry %d reports tag %d, want the owner's %d and never the rival's %d", i, e.Tag, ownerTag, rivalTag)
		}
	}
}

// TestInspectAfterClose holds the answer the type doc promises once the
// journal has closed: File fails with ErrClosed, and All yields nothing.
func TestInspectAfterClose(t *testing.T) {
	j, _ := reopenedJournal(t)
	in := j.Inspect()
	if err := j.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := in.File(fileOf(1)); !errors.Is(err, ErrClosed) {
		t.Errorf("File after Close = %v, want ErrClosed", err)
	}
	yielded := 0
	in.All()(func(FileID, []IndexEntry) bool {
		yielded++
		return true
	})
	if yielded != 0 {
		t.Errorf("All after Close yielded %d files, want none", yielded)
	}
}

// TestInspectOfAnEmptyIndexYieldsNothing checks an index that holds no entry:
// File answers nothing rather than an error, and All yields no file.
func TestInspectOfAnEmptyIndexYieldsNothing(t *testing.T) {
	j := newTestJournal(t)
	in := j.Inspect()

	es, err := in.File(fileOf(1))
	if err != nil {
		t.Fatalf("File of a file no write covers = %v, want no error", err)
	}
	if len(es) != 0 {
		t.Errorf("File of a file no write covers got %+v, want no entry", es)
	}
	in.All()(func(FileID, []IndexEntry) bool {
		t.Error("All of an empty index yielded a file, want none")
		return false
	})
}

// TestDirtySeedListsTheFilesTheRebuildRestoredDirty holds the seed the
// journal side derives from a rebuilt index: one delta per file that holds or
// retains an extent — the became-dirty transition when the file holds or
// retains content whose offloaded bit is unset, Unchanged when an offloaded
// record covers everything it holds — at the zero write time restored content
// carries, carrying each file's whole held, dirty and retained byte figures,
// the counts the open's journal-wide axes and per-share figures open at. An
// index holding no entry seeds nothing.
func TestDirtySeedListsTheFilesTheRebuildRestoredDirty(t *testing.T) {
	j, _ := reopenedJournal(t)

	seed := dirtySeed(j.core.Index())
	if len(seed) != 6 {
		t.Fatalf("dirtySeed over the plant got %d deltas, want one per file that holds or retains an extent: %+v", len(seed), seed)
	}
	for _, d := range seed {
		if !d.OldestDirty.IsZero() {
			t.Errorf("the seed of file %v names the oldest dirty time %v, want the zero time restored content carries", d.File, d.OldestDirty)
		}
		if d.RetainedBytes != 0 {
			t.Errorf("the seed of file %v carries %d retained bytes, want none: the index a rebuild builds retains nothing", d.File, d.RetainedBytes)
		}
		switch d.File {
		case fileOf(1), fileOf(4), fileOf(5):
			if d.Dirty != index.BecameDirty {
				t.Errorf("the seed of file %v carries the transition %v, want BecameDirty", d.File, d.Dirty)
			}
			if d.HeldBytes != d.DirtyBytes {
				t.Errorf("the seed of file %v carries %d held and %d dirty bytes, want equal figures: no planted record marks it offloaded", d.File, d.HeldBytes, d.DirtyBytes)
			}
			if d.Tag != marksTag && d.Tag != piecesTag {
				t.Errorf("the seed of file %v carries tag %d, want the tag of the record that holds it", d.File, d.Tag)
			}
		case fileOf(2):
			if d.Dirty != index.BecameDirty {
				t.Errorf("the seed of the truncated file carries the transition %v, want BecameDirty: its held remainder is dirty", d.Dirty)
			}
			if d.HeldBytes != 40 {
				t.Errorf("the seed of the truncated file carries %d held bytes, want 40: its held remainder [0,40)", d.HeldBytes)
			}
			if d.Tag != cutsTag {
				t.Errorf("the seed of the truncated file carries tag %d, want %d", d.Tag, cutsTag)
			}
		case fileOf(3):
			if d.Dirty != index.Unchanged {
				t.Errorf("the seed of the over-marked file carries the transition %v, want Unchanged: its offloaded record covers everything it holds", d.Dirty)
			}
			if d.DirtyBytes != 0 {
				t.Errorf("the seed of the over-marked file carries %d dirty bytes, want none: its offloaded record covers everything it holds", d.DirtyBytes)
			}
			if d.HeldBytes != 64 {
				t.Errorf("the seed of the over-marked file carries %d held bytes, want 64: its offloaded record covers it and holds remain held", d.HeldBytes)
			}
		case fileOf(6):
			if d.Dirty != index.BecameDirty {
				t.Errorf("the seed of the rivalled file carries the transition %v, want BecameDirty", d.Dirty)
			}
			if d.Tag != ownerTag {
				t.Errorf("the seed of the rivalled file carries tag %d, want its owner's %d", d.File, d.Tag)
			}
		default:
			t.Errorf("the seed lists the unknown file %v, want only the plant's files", d.File)
		}
	}
	if got := dirtySeed(index.New(1 << 20)); got != nil {
		t.Errorf("dirtySeed of an empty index = %+v, want no delta", got)
	}
}

// sameEntries decides whether two read-outs of the same file hold the same
// entries, clone specs by value: the read-out copies a spec per entry, so its
// copies never share a pointer with the caller's.
func sameEntries(a, b []IndexEntry) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		x, y := a[i], b[i]
		xc, yc := x.Clone, y.Clone
		x.Clone, y.Clone = nil, nil
		if x != y || (xc == nil) != (yc == nil) || (xc != nil && *xc != *yc) {
			return false
		}
	}
	return true
}

// TestInspectFileMapsARetainedPiece holds the retained mapping over the
// index's own superseding write, which the reopen plant cannot rebuild: a
// write the index retains reports its superseder's version in By, and its
// location is its own record's. A rebuild from records alone never retains —
// its supersede is a cut — so the check drives the index's own publish, the
// one call that buffers the piece before its successor is settled.
func TestInspectFileMapsARetainedPiece(t *testing.T) {
	j := newTestJournal(t)
	ix := j.core.Index()

	const tag = core.ShareTag(3)
	f := fileOf(7)
	at := func(off uint32) index.RecordAt { return index.RecordAt{Segment: 5, Offset: off} }
	if _, err := ix.WriteRetain(tag, f, index.Placed{
		Extent: Extent{Off: 0, Len: 64}, Record: at(48), Version: v(1),
	}, 1); err != nil {
		t.Fatalf("the first WriteRetain: %v", err)
	}
	if _, err := ix.WriteRetain(tag, f, index.Placed{
		Extent: Extent{Off: 0, Len: 32}, Record: at(216), Version: v(2),
	}, 2); err != nil {
		t.Fatalf("the superseding WriteRetain: %v", err)
	}

	got, err := j.Inspect().File(f)
	if err != nil {
		t.Fatalf("File of the superseding write's file: %v", err)
	}
	want := []IndexEntry{
		{
			Kind: EntryHeld, Extent: Extent{Off: 0, Len: 32}, Version: v(2), Tag: tag,
			Location: RecordRef{Segment: 5, Offset: 216},
		},
		{
			Kind: EntryRetained, Extent: Extent{Off: 0, Len: 32}, Version: v(1), Tag: tag,
			Location: RecordRef{Segment: 5, Offset: 48}, By: v(2),
		},
		{
			Kind: EntryHeld, Extent: Extent{Off: 32, Len: 32}, Version: v(1), Tag: tag,
			Location: RecordRef{Segment: 5, Offset: 48},
		},
	}
	if !sameEntries(got, want) {
		t.Errorf("File of the superseding write's file = %+v, want %+v", got, want)
	}
}
