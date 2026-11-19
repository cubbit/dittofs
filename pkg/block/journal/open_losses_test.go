package journal

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/durability"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/format"
	"github.com/cubbit/dittofs/pkg/block/journal/journaltest/kit"
	"github.com/cubbit/dittofs/pkg/block/journal/journaltest/raw"
)

// The tests in this file drive the open turned on over a directory holding
// segments: the public Open runs the recovery itself, refuses the
// directory whose open ends a segment and the one whose corrupt drops
// would run past the open reserve, and appends its own loss records — the
// failed-list records first on the reserve, then the corrupt drops —
// before it returns. Every check reads its answers through
// the journal's public surface, so a recovery that drops what it rebuilt or
// a loss pass that publishes what no ring holds fails at the caller that
// consumes it.

// publicOpen opens the fixture's directory through the production Open, with
// the settings the caller narrows, and closes what it answered when the test
// ends. It is the row the reopen seam has stood in for: the open runs the
// recovery itself now.
func (f *assembledJournal) publicOpen(t *testing.T, set func(*Settings)) *Journal {
	t.Helper()
	j, err := Open(context.Background(), f.dir, Version{}, f.id, nil, f.opts(set)...)
	if err != nil {
		t.Fatalf("the public Open over %s: %v", f.dir, err)
	}
	t.Cleanup(func() {
		if err := j.Close(); err != nil && !errors.Is(err, ErrClosed) {
			t.Errorf("close the reopened journal: %v", err)
		}
	})
	return j
}

// rotHeader flips a byte of the record header's checksum at off in the file
// the seam serves under name, so the record's header decodes nowhere while
// every column of the entry that names it stands: the corruption the
// catalogue's own read confines as a corrupt drop.
func (f *assembledJournal) rotHeader(t *testing.T, name string, off int64) {
	t.Helper()
	file, err := f.seam.Open(name)
	if err != nil {
		t.Fatalf("open %s to rot: %v", name, err)
	}
	b := make([]byte, 1)
	if _, err := file.ReadAt(b, off+92); err != nil {
		t.Fatalf("read the header checksum of the record at %d: %v", off, err)
	}
	b[0] ^= 0xff
	if _, err := file.WriteAt(b, off+92); err != nil {
		t.Fatalf("rot the header checksum of the record at %d: %v", off, err)
	}
	if err := errors.Join(file.Sync(), file.Close()); err != nil {
		t.Fatalf("sync the rotted %s: %v", name, err)
	}
}

// lossRecords answers the dropped record each loss record on disk names, in
// the order the scan read them: the order the streams appended them in.
//
// The holds park the two ways a live journal's retirement worker makes a
// listed segment file vanish mid-read — a recycle renames its name away, a
// retirement unlinks it — so the listings and the reads see one directory.
// Both releases are deferred, and the kit's own test-end backstop re-releases
// after t.Fatalf, before the cleanup closing a still-open journal.
//
// Durable answers the crash view of the last SyncDir, not the live directory
// the listing walked, so a listed name can name a segment whose publishing
// SyncDir the live journal has not run yet — a fresh spare or the open's own
// new segment. The open syncs every loss record it appends before it returns,
// so such a name holds no loss record, and the skip below leaves the answer
// unchanged; with the holds in place ErrNotExist is never a removal.
func (f *assembledJournal) lossRecords(t *testing.T) []core.Seq {
	t.Helper()
	defer f.seam.Hold(t, kit.OpRename, "*.seg").Release()
	defer f.seam.Hold(t, kit.OpRemove, "*.seg").Release()
	var out []core.Seq
	for _, name := range f.segmentFileNames(t) {
		kind, id := format.ClassifyName(name)
		if kind != format.NameSegment {
			continue
		}
		b, err := f.seam.Durable(name)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		scan, err := format.ScanSegmentHeaders(bytes.NewReader(b), int64(len(b)), f.id, id)
		if err != nil {
			t.Fatalf("scan %s for loss records: %v", name, err)
		}
		for _, r := range scan.Records {
			if r.Header.Kind != format.KindLoss {
				continue
			}
			off := int(r.Header.Offset)
			end := off + int(r.Header.Size())
			body, berr := format.VerifyRecordBody(b[off+format.RecordHeaderSize:end], f.id, r.Header)
			if berr != nil {
				t.Fatalf("verify the loss record at %d of %s: %v", r.Header.Offset, name, berr)
			}
			out = append(out, body.Loss.Dropped)
		}
	}
	return out
}

// lossEvents answers the ring's events of one reason, the corrupt and the
// failed-window legs' own rows.
func lossEvents(t *testing.T, j *Journal, reason core.LossReason) []core.LossEvent {
	t.Helper()
	var out []core.LossEvent
	for _, ev := range j.Stats().Losses {
		if ev.Reason == reason {
			out = append(out, ev)
		}
	}
	return out
}

// TestCleanExitOpens holds C1's clean half at the journal's own caller: the
// public Open of a journal closed cleanly returns a journal that serves —
// the recovery read the closed segments back, and the phases this branch
// built are what it serves: the content synced before the close reads back
// whole, a later write draws a fresh version, and its Sync returns a mark.
func TestCleanExitOpens(t *testing.T) {
	f := newAssembledJournal(t)
	j := f.open(t)
	withLimit(t, j)
	h := shareOf(t, j, 1)
	file := fileOf(1)
	first := writeReady(t, h, file, "written before the close")
	if _, err := h.Sync(t.Context(), file); err != nil {
		t.Fatalf("Sync before the close: %v", err)
	}
	if err := errors.Join(h.Close(), j.Close()); err != nil {
		t.Fatalf("the clean close: %v", err)
	}

	j2 := f.publicOpen(t, nil)
	withLimit(t, j2)
	h2 := shareOf(t, j2, 1)
	p := []byte("written before the close")
	n, missing, _, _, err := h2.ReadAt(t.Context(), file, 0, p)
	if err != nil || n != len(p) || len(missing) != 0 {
		t.Fatalf("reading the synced content back after the clean close: n %d, missing %+v, err %v", n, missing, err)
	}
	if string(p) != "written before the close" {
		t.Fatalf("the content after the reopen = %q, want what the first journal synced", p)
	}
	next := writeReady(t, h2, file, "written after the reopen")
	if next.Counter <= first.Counter {
		t.Fatalf("the first write after the reopen drew version %d, want above the closed journal's %d", next.Counter, first.Counter)
	}
	marks, err := h2.Sync(t.Context(), file)
	if err != nil || len(marks) == 0 {
		t.Fatalf("Sync after the reopen: marks %+v, err %v, want its stream's mark", marks, err)
	}
}

// TestTornTailOnlyOpens holds C1 over a directory killed with torn tails
// alone: the public Open serves it, the cut-and-seal phase it runs keeps the
// bytes the crash synced, and the address the crash tore past reads lost,
// never zeros.
func TestTornTailOnlyOpens(t *testing.T) {
	f := newAssembledJournal(t)
	j := f.open(t)
	withLimit(t, j)
	h := shareOf(t, j, 1)
	file := fileOf(1)
	writeReady(t, h, file, "the synced half of the segment")
	if _, err := h.Sync(t.Context(), file); err != nil {
		t.Fatalf("Sync the content the crash keeps: %v", err)
	}
	// A write past the sync that the crash tears: its bytes never complete,
	// so the tail the classing cuts starts at the record that verifies.
	if _, err := h.WriteAt(t.Context(), file, 16<<10, payload(16), 1); err != nil {
		t.Fatalf("the write the crash tears: %v", err)
	}
	f.crash(t, j)

	j2 := f.publicOpen(t, nil)
	withLimit(t, j2)
	h2 := shareOf(t, j2, 1)
	p := []byte("the synced half of the segment")
	n, missing, _, _, err := h2.ReadAt(t.Context(), file, 0, p)
	if err != nil || n != len(p) || len(missing) != 0 {
		t.Fatalf("reading the synced content back: n %d, missing %+v, err %v", n, missing, err)
	}
	if string(p) != "the synced half of the segment" {
		t.Fatalf("the synced content after the open = %q, want what the crash kept", p)
	}
}

// TestOpenEndingASegmentEmptiesIt holds the withdrawn checkpoint at its own
// caller: an open whose recovery ends a segment — here over a torn slot
// below a record that verifies, the ending whole with no cut — serves, the
// ended segment's record copied out whole with its numbers kept into a
// sealed spare of the repack reserve, and the segment's file gone with a
// synced unlink: the emptying that unlinks such a segment is what the open
// now runs.
func TestOpenEndingASegmentEmptiesIt(t *testing.T) {
	f := seededJournal(t)
	seg := core.SegmentID(941)
	plantEndedSegment(t, f, seg)

	j := f.publicOpen(t, nil)
	// The ended segment is gone, its whole survived record read back once
	// from the sealed copy the open made, numbers kept.
	if slices.Contains(f.segmentFileNames(t), format.SegmentName(seg)) {
		t.Fatalf("the ended segment %d is still on disk after the open, want it unlinked by the emptying", seg)
	}
	h := shareOf(t, j, ownerTag)
	p := make([]byte, 32)
	n, missing, _, _, err := h.ReadAt(t.Context(), fileOf(2), 32, p)
	if err != nil || n != 32 || len(missing) != 0 {
		t.Fatalf("reading the record the ended segment held: n %d, missing %+v, err %v, want its copy serving it whole", n, missing, err)
	}
	for _, name := range f.segmentFileNames(t) {
		kind, id := format.ClassifyName(name)
		if kind != format.NameSegment {
			continue
		}
		bs, derr := f.seam.Durable(name)
		if derr != nil {
			t.Fatalf("read %s: %v", name, derr)
		}
		walk, werr := format.ScanSegmentHeaders(bytes.NewReader(bs), int64(len(bs)), f.id, id)
		if werr != nil {
			t.Fatalf("walking %s: %v", name, werr)
		}
		for _, rec := range walk.Records {
			if rec.Header.Seq == 31 && rec.Header.Version.Counter != 31 {
				t.Errorf("the copy of the record of seq 31 kept version %d, want its own 31, never merged, numbers kept", rec.Header.Version.Counter)
			}
		}
	}
}

// plantEndedSegment plants segment seg holding two live records of fileOf(2)
// — seq 20 at offset 0 and seq 21 at offset 32, 32 bytes each — with a torn
// slot between them: a zeroed gap no proof covers, below a record that
// verifies whole, the classing's ending whole, an ended fate with no cut, the
// segment the emptying empties.
func plantEndedSegment(t *testing.T, f *assembledJournal, seg core.SegmentID) {
	t.Helper()
	a := plantRecord(1, ownerTag, fileOf(2), 20, 30, 0, 32)
	rr := plantRecord(1, ownerTag, fileOf(2), 21, 31, 32, 32)
	b := raw.SegmentHeader{Identity: f.id, ID: uint64(seg)}.Bytes()
	a.Identity, a.Segment, a.RecordOffset = f.id, uint64(seg), uint32(len(b))
	b = append(b, a.Bytes()...)
	b = append(b, make([]byte, 16)...)
	rr.Identity, rr.Segment, rr.RecordOffset = f.id, uint64(seg), uint32(len(b))
	b = append(b, rr.Bytes()...)
	plantSealedBytes(t, f, format.SegmentName(seg), b)
}

// TestOpenAtItsMaximumEndingASegmentOpens holds the emptying at a journal at
// its maximum: a journal whose pool settled is closed, a segment the open
// ends is planted beside its files, and the reopen under a maximum of
// exactly what the directory holds still opens — the copy draws the repack
// reserve's spare the open re-adopted, so it needs no byte past the files
// already there — empties the ended segment and serves its records whole,
// its footprint never past the maximum.
func TestOpenAtItsMaximumEndingASegmentOpens(t *testing.T) {
	f := newAssembledJournal(t)
	j := f.open(t)
	mustWaitSpares(t, j)
	if err := j.Close(); err != nil {
		t.Fatalf("close the settled journal: %v", err)
	}
	seg := core.SegmentID(941)
	plantEndedSegment(t, f, seg)
	maxBytes := int64(len(f.segmentFileNames(t))) * openSegmentCap

	j2, err := Open(t.Context(), f.dir, Version{}, f.id, nil, f.opts(func(s *Settings) { s.MaxBytes = maxBytes })...)
	if err != nil {
		t.Fatalf("the reopen at the maximum %d, the bytes the directory holds, answered %v, want it open: a journal at its maximum whose open ends a segment still opens", maxBytes, err)
	}
	t.Cleanup(func() {
		if err := j2.Close(); err != nil && !errors.Is(err, ErrClosed) {
			t.Errorf("close the reopened journal: %v", err)
		}
	})
	if slices.Contains(f.segmentFileNames(t), format.SegmentName(seg)) {
		t.Fatalf("the ended segment %d is still on disk after the open, want it unlinked by the emptying", seg)
	}
	h := shareOf(t, j2, ownerTag)
	p := make([]byte, 64)
	n, missing, _, _, err := h.ReadAt(t.Context(), fileOf(2), 0, p)
	if err != nil || n != 64 || len(missing) != 0 {
		t.Fatalf("reading the records the ended segment held: n %d, missing %+v, err %v, want both served whole from the copy", n, missing, err)
	}
	if got := j2.Stats().UsedBytes; got > maxBytes {
		t.Fatalf("the reopened journal's UsedBytes = %d, want at most the maximum %d", got, maxBytes)
	}
	// The ledger counts only what it holds: a re-adopted spare dropped from
	// it would stay on disk beside the refill's own, so the files count too.
	if got := int64(len(f.segmentFileNames(t))) * openSegmentCap; got > maxBytes {
		t.Fatalf("the reopened journal's files hold %d bytes, want at most the maximum %d", got, maxBytes)
	}
}

// TestOpenWhoseReserveIsEmptyAtTheCopyWaitsForTheReserveAlone holds the
// open's draw at a maximum that leaves the content class short: the
// directory holds no spare when the reopen starts, so the copy draws before
// the refill has readied the repack reserve, and the maximum has room for the
// headroom and the reserve spares but never the content ones. The draw waits
// for the reserve class alone, so the open serves; a wait for the pool's whole
// goal would never end.
func TestOpenWhoseReserveIsEmptyAtTheCopyWaitsForTheReserveAlone(t *testing.T) {
	f := newAssembledJournal(t)
	j := f.open(t)
	mustWaitSpares(t, j)
	if err := j.Close(); err != nil {
		t.Fatalf("close the settled journal: %v", err)
	}
	for _, name := range f.segmentFileNames(t) {
		if err := f.seam.Remove(name); err != nil {
			t.Fatalf("remove the spare %s: %v", name, err)
		}
	}
	plantEndedSegment(t, f, 941)
	maxBytes := int64(6 * openSegmentCap) // the ended segment, five spares: the headroom's and the reserve's, no content spare

	// The backstop only: the wait the open takes is the pool's reserve event.
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	j2, err := Open(ctx, f.dir, Version{}, f.id, nil, f.opts(func(s *Settings) { s.MaxBytes = maxBytes })...)
	if err != nil {
		t.Fatalf("the reopen with no spare on disk under the maximum %d answered %v, want it open: the copy waits for the reserve spare alone", maxBytes, err)
	}
	t.Cleanup(func() {
		if err := j2.Close(); err != nil && !errors.Is(err, ErrClosed) {
			t.Errorf("close the reopened journal: %v", err)
		}
	})
	h := shareOf(t, j2, ownerTag)
	p := make([]byte, 64)
	n, missing, _, _, err := h.ReadAt(t.Context(), fileOf(2), 0, p)
	if err != nil || n != 64 || len(missing) != 0 {
		t.Fatalf("reading the records the ended segment held: n %d, missing %+v, err %v, want both served whole from the copy", n, missing, err)
	}
}

// TestOpenAtItsMaximumCarryingOverARottenTwinOpens holds the draw's floor at
// the maximum: beside the ended segment, a sealed twin above it holds a copy
// of its first record whose payload rots, so the carry must land above the
// twin, while the reserve's one ready spare is the one the open re-adopted
// under its old id, below it. The reopen under a maximum of exactly what the
// directory holds still opens — the low spare takes a fresh id in place,
// no byte past the files already there — and the record reads back whole
// from the carry above the twin, the footprint never past the maximum.
func TestOpenAtItsMaximumCarryingOverARottenTwinOpens(t *testing.T) {
	f := newAssembledJournal(t)
	j := f.open(t)
	mustWaitSpares(t, j)
	if err := j.Close(); err != nil {
		t.Fatalf("close the settled journal: %v", err)
	}
	seg, twin := core.SegmentID(941), core.SegmentID(942)
	plantEndedSegment(t, f, seg)
	offs := plantSealedAt(t, f.seam, f.id, twin, plantRecord(1, ownerTag, fileOf(2), 20, 30, 0, 32))
	rotPayload(t, f, format.SegmentName(twin), int64(offs[0])+format.RecordHeaderSize)
	maxBytes := int64(len(f.segmentFileNames(t))) * openSegmentCap

	j2, err := Open(t.Context(), f.dir, Version{}, f.id, nil, f.opts(func(s *Settings) { s.MaxBytes = maxBytes })...)
	if err != nil {
		t.Fatalf("the reopen at the maximum %d, the bytes the directory holds, answered %v, want it open: the carry above the rotten twin needs no new space", maxBytes, err)
	}
	t.Cleanup(func() {
		if err := j2.Close(); err != nil && !errors.Is(err, ErrClosed) {
			t.Errorf("close the reopened journal: %v", err)
		}
	})
	h := shareOf(t, j2, ownerTag)
	p := make([]byte, 64)
	n, missing, _, _, err := h.ReadAt(t.Context(), fileOf(2), 0, p)
	if err != nil || n != 64 || len(missing) != 0 {
		t.Fatalf("reading the records the ended segment held: n %d, missing %+v, err %v, want both served whole", n, missing, err)
	}
	if got := j2.Stats().UsedBytes; got > maxBytes {
		t.Fatalf("the reopened journal's UsedBytes = %d, want at most the maximum %d", got, maxBytes)
	}
	// The ledger counts only what it holds: a re-adopted spare dropped from
	// it would stay on disk beside the refill's own, so the files count too.
	if got := int64(len(f.segmentFileNames(t))) * openSegmentCap; got > maxBytes {
		t.Fatalf("the reopened journal's files hold %d bytes, want at most the maximum %d", got, maxBytes)
	}

	// The next open serves the greatest placement and verifies no payload:
	// the carry outranks the twin, so the record still reads whole.
	if err := errors.Join(h.Close(), j2.Close()); err != nil {
		t.Fatalf("close the reopened journal: %v", err)
	}
	j3 := f.publicOpen(t, func(s *Settings) { s.MaxBytes = maxBytes })
	h3 := shareOf(t, j3, ownerTag)
	n, missing, _, _, err = h3.ReadAt(t.Context(), fileOf(2), 0, p)
	if err != nil || n != 64 || len(missing) != 0 {
		t.Fatalf("reading the records after the second reopen: n %d, missing %+v, err %v, want both served whole from the carry above the twin", n, missing, err)
	}
}

// rotPayload flips one byte at off in the file the seam serves under name.
func rotPayload(t *testing.T, f *assembledJournal, name string, off int64) {
	t.Helper()
	file, err := f.seam.Open(name)
	if err != nil {
		t.Fatalf("open %s to rot: %v", name, err)
	}
	b := make([]byte, 1)
	if _, err := file.ReadAt(b, off); err != nil {
		t.Fatalf("read the byte at %d of %s: %v", off, name, err)
	}
	b[0] ^= 0xff
	if _, err := file.WriteAt(b, off); err != nil {
		t.Fatalf("rot the byte at %d of %s: %v", off, name, err)
	}
	if err := errors.Join(file.Sync(), file.Close()); err != nil {
		t.Fatalf("sync the rotted %s: %v", name, err)
	}
}

// plantSealedBytes writes one planted file whole under the seam, synced as
// a writer would have left it.
func plantSealedBytes(t *testing.T, f *assembledJournal, name string, b []byte) {
	t.Helper()
	file, err := f.seam.Create(name)
	if err != nil {
		t.Fatalf("create %s: %v", name, err)
	}
	if _, err := file.WriteAt(b, 0); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	if err := errors.Join(file.Sync(), file.Close(), f.seam.SyncDir()); err != nil {
		t.Fatalf("sync %s whole: %v", name, err)
	}
}

// TestCorruptDirtRecordRaisesGenerationDurably holds the open's first loss
// record at the caller that needs it: a corrupt dirty record found at open
// publishes its loss as found at open, raises the generation, and its loss
// record is durable the moment the open returns — crash right after and the
// reopen answers nothing new: the found loss record places back, the
// generation does not move and the rotted extent still reads lost.
func TestCorruptDirtRecordRaisesGenerationDurably(t *testing.T) {
	f := seededJournal(t)
	file := fileOf(2)
	older := plantRecord(1, ownerTag, file, 10, 10, 0, 32)
	newer := plantRecord(1, ownerTag, file, 11, 11, 0, 32)
	offs := plantSealedAt(t, f.seam, f.id, 942, older, newer)
	f.rotHeader(t, format.SegmentName(942), int64(offs[1]))

	j := f.publicOpen(t, nil)
	if got := j.Stats().LossGeneration; got != 1 {
		t.Fatalf("the generation after the open = %d, want 1: raised once by the corrupt record's loss", got)
	}
	events := lossEvents(t, j, core.LossCorrupt)
	if len(events) != 1 || !events[0].AtOpen || events[0].Deferred || !events[0].Synced {
		t.Fatalf("the open's loss events = %+v, want the one corrupt loss marked found at open, synced and not deferred", events)
	}
	if n := len(f.lossRecords(t)); n != 1 {
		t.Fatalf("%d loss records on disk after the open, want the one durable record", n)
	}
	h := shareOf(t, j, ownerTag)
	p := make([]byte, 32)
	n, missing, _, _, err := h.ReadAt(t.Context(), file, 0, p)
	if err != nil || n != 0 || len(missing) != 1 || missing[0] != (Extent{Off: 0, Len: 32}) {
		t.Fatalf("reading the corrupted extent: n %d, missing %+v, err %v, want the whole extent lost", n, missing, err)
	}

	// The crash right after: everything synced stands, so the reopen finds
	// the loss record the open appended and answers nothing new with it.
	f.seam.SetFault(nil)
	f.crash(t, j)
	j2 := f.publicOpen(t, nil)
	if got := j2.Stats().LossGeneration; got != 1 {
		t.Fatalf("the generation after the crash and reopen = %d, want the opened journal's 1: the found loss record raises nothing", got)
	}
	if n := len(f.lossRecords(t)); n != 1 {
		t.Fatalf("%d loss records on disk after the reopen, want still the one: a rotted record whose loss was already recorded appends no second", n)
	}
	h2 := shareOf(t, j2, ownerTag)
	if _, missing, _, _, err := h2.ReadAt(t.Context(), file, 0, p); err != nil || len(missing) != 1 {
		t.Fatalf("reading the rotted extent after the reopen: n 0, missing %+v, err %v, want it lost still", missing, err)
	}
}

// TestExhaustedHeadroomSyncedLossOnReserve holds the reserve's own rule at
// an exhausted headroom: one corrupt synced record in a sealed segment gets
// exactly one synced loss record on the open reserve, the older content it
// superseded is never served — masked by the record the loss left — and a
// later open of the same directory appends no second loss record, for the
// first one is on disk and the registry answers it named.
func TestExhaustedHeadroomSyncedLossOnReserve(t *testing.T) {
	f := seededJournal(t)
	file := fileOf(3)
	older := plantRecord(1, ownerTag, file, 10, 10, 0, 32)
	newer := plantRecord(1, ownerTag, file, 11, 11, 0, 32)
	offs := plantSealedAt(t, f.seam, f.id, 943, older, newer)
	f.rotHeader(t, format.SegmentName(943), int64(offs[1]))

	// Two headroom records keep an open reserve of one: the single corrupt
	// drop takes it, nothing defers.
	j := f.publicOpen(t, func(s *Settings) { s.HeadroomRecords = 2 })
	events := lossEvents(t, j, core.LossCorrupt)
	if len(events) != 1 || !events[0].Synced || events[0].Deferred {
		t.Fatalf("the corrupt loss at the exhausted headroom = %+v, want the one synced loss on the open reserve", events)
	}
	if n := len(f.lossRecords(t)); n != 1 {
		t.Fatalf("%d loss records on disk, want the one the open reserve held", n)
	}
	h := shareOf(t, j, ownerTag)
	if _, missing, _, _, err := h.ReadAt(t.Context(), file, 0, make([]byte, 32)); err != nil || len(missing) != 1 {
		t.Fatalf("reading the extent the corrupted record superseded: missing %+v, err %v, want the older content masked and the whole extent lost", missing, err)
	}

	// The later open finds the loss record on disk and appends no second
	// one: the registry answers the piece named.
	if err := h.Close(); err != nil {
		t.Fatalf("close the handle: %v", err)
	}
	if err := j.Close(); err != nil && !errors.Is(err, ErrClosed) {
		t.Fatalf("close the journal: %v", err)
	}
	j2 := f.publicOpen(t, func(s *Settings) { s.HeadroomRecords = 2 })
	if n := len(f.lossRecords(t)); n != 1 {
		t.Fatalf("%d loss records on disk after the second open, want still the one", n)
	}
	if got := j2.Stats().LossGeneration; got != 1 {
		t.Fatalf("the second open's generation = %d, want 1: the found loss record raises nothing", got)
	}
}

// TestPastReserveLossDefersPastTheOpen holds the withdrawn refusal at the
// open that reaches the reserve's far edge: corrupt drops past what the
// open reserve holds defer — the loss record the reserve held appends and
// syncs, and each drop past it defers against the segment holding it — and
// the open serves. The deferred holder stands: no retirement gate takes it
// before the copy that owes the deferral its record exists, so the drop's
// older content never serves over a write the crash had synced — the row's
// negative verification, live now — and each retry over the same process
// burns no record: the pieces the first open took answer named.
func TestPastReserveLossDefersPastTheOpen(t *testing.T) {
	f := seededJournal(t)
	file := fileOf(4)
	older := plantRecord(1, ownerTag, file, 10, 10, 0, 32)
	newer := plantRecord(1, ownerTag, file, 11, 11, 32, 32)
	offs := plantSealedAt(t, f.seam, f.id, 944, older, newer)
	f.rotHeader(t, format.SegmentName(944), int64(offs[0]))
	f.rotHeader(t, format.SegmentName(944), int64(offs[1]))
	name := format.SegmentName(944)

	// Two headroom records keep an open reserve of one: the first drop's
	// loss record takes it, the second defers against the segment it sits
	// in, and the open serves both masked.
	narrow := func(s *Settings) { s.HeadroomRecords = 2 }
	j := f.publicOpen(t, narrow)
	events := lossEvents(t, j, core.LossCorrupt)
	var appended, deferred int
	for _, ev := range events {
		switch {
		case ev.Deferred:
			deferred++
			if ev.Extent != (core.Extent{Off: 32, Len: 32}) {
				t.Errorf("the deferred corrupt loss covers %+v, want the second rotted extent [32,64)", ev.Extent)
			}
		default:
			appended++
			if ev.Extent != (core.Extent{Off: 0, Len: 32}) {
				t.Errorf("the appended corrupt loss covers %+v, want the first rotted extent [0,32)", ev.Extent)
			}
		}
	}
	if appended != 1 || deferred != 1 {
		t.Fatalf("the open over a one-record reserve published %+v, want the one appended loss and the one deferred", events)
	}
	if n := len(f.lossRecords(t)); n != 1 {
		t.Fatalf("%d loss records on disk, want the one the open reserve held: a deferred loss appends none", n)
	}
	// The registry answers the deferral against the drop's holder, and the
	// holder stands: nothing may retire it before the copy that owes the
	// record exists.
	defss, derr := durability.DeferredFor(f.dir, f.id)
	if derr != nil || len(defss) != 1 || len(defss[944]) != 1 {
		t.Fatalf("the deferred registry after the open answers %+v (err %v), want the one loss against segment 944", defss, derr)
	}
	if !slices.Contains(f.segmentFileNames(t), name) {
		t.Fatalf("the deferred drop's holder %s is gone from the directory, want it standing on the deferral's account", name)
	}
	// The negative verification, kept live from the refusal's day: the
	// rotted extents read lost — never the content the corrupted records
	// superseded, whichever record names them.
	h := shareOf(t, j, ownerTag)
	for _, off := range []int64{0, 32} {
		if _, missing, _, _, err := h.ReadAt(t.Context(), file, off, make([]byte, 32)); err != nil || len(missing) != 1 || missing[0] != (Extent{Off: off, Len: 32}) {
			t.Fatalf("reading the rotted extent at %d of the serving open: missing %+v, err %v, want the whole extent lost", off, missing, err)
		}
	}

	// The retry burns nothing: a second open over the same narrow reserve
	// answers the pieces the first one took as named, defers nothing new
	// and appends nothing new.
	if err := errors.Join(h.Close(), j.Close()); err != nil {
		t.Fatalf("the serving journal's close: %v", err)
	}
	j2 := f.publicOpen(t, narrow)
	for _, ev := range lossEvents(t, j2, core.LossCorrupt) {
		t.Errorf("the second open published %+v, want nothing: the pieces the first open took answer named", ev)
	}
	if n := len(f.lossRecords(t)); n != 1 {
		t.Fatalf("%d loss records on disk after the second open, want still the one", n)
	}
	if defss, derr := durability.DeferredFor(f.dir, f.id); derr != nil || len(defss) != 1 || len(defss[944]) != 1 {
		t.Fatalf("the deferred registry after the second open answers %+v (err %v), want the same one entry", defss, derr)
	}
	if !slices.Contains(f.segmentFileNames(t), name) {
		t.Fatalf("the deferred drop's holder %s is gone after the second open, want it standing on the deferral's account", name)
	}
}

// TestDeferredOnlySegmentStandsOnItsDeferral holds the probe shape the
// withdrawal answers: two corrupt drops over a one-record reserve, and the
// past-reserve drop the only record of its own segment. The open serves —
// the drop's loss defers against the deferred-only segment, exactly as it
// once did before the refusal — and the segment stands where it sits, held
// by nothing but the deferral whose record the copy that retires it owes.
// The negative verification stays the row's own: the drop's extent reads
// lost, never the holder's older bytes, which is what a retirement that
// ignored the deferral would serve over the synced write.
func TestDeferredOnlySegmentStandsOnItsDeferral(t *testing.T) {
	f := seededJournal(t)
	file := fileOf(5)
	// The older write of the extent, whole in its own segment — the bytes a
	// retired deferred-only segment would serve — beside the one corrupt
	// drop that takes the reserve.
	older := plantRecord(1, ownerTag, file, 10, 10, 0, 32)
	reserveDrop := plantRecord(1, ownerTag, file, 13, 13, 32, 32)
	offsA := plantSealedAt(t, f.seam, f.id, 947, older, reserveDrop)
	f.rotHeader(t, format.SegmentName(947), int64(offsA[1]))
	// The past-reserve drop, alone in its own segment: the whole of its
	// holder is the drop whose loss defers against it.
	newer := plantRecord(1, ownerTag, file, 11, 11, 0, 32)
	offsB := plantSealedAt(t, f.seam, f.id, 948, newer)
	f.rotHeader(t, format.SegmentName(948), int64(offsB[0]))

	narrow := func(s *Settings) { s.HeadroomRecords = 2 }
	nameB := format.SegmentName(948)
	j := f.publicOpen(t, narrow)

	// The negative verification: the dropped extent reads lost, never the
	// older record the retired segment would have served over the synced
	// write.
	h := shareOf(t, j, ownerTag)
	p := make([]byte, 32)
	n, missing, _, _, rerr := h.ReadAt(t.Context(), file, 0, p)
	if rerr != nil || n != 0 || len(missing) != 1 || missing[0] != (Extent{Off: 0, Len: 32}) {
		t.Fatalf("the served open answered the dropped extent n %d, missing %+v, err %v — the older record served over the synced write, want the whole extent lost", n, missing, rerr)
	}
	// The one loss record the reserve held names the reserved drop, and the
	// past-reserve one defers against its own holder, which stands.
	if n := len(f.lossRecords(t)); n != 1 {
		t.Fatalf("%d loss records on disk, want the one the open reserve held: the past-reserve drop deferred", n)
	}
	defss, derr := durability.DeferredFor(f.dir, f.id)
	if derr != nil || len(defss) != 1 || len(defss[948]) != 1 {
		t.Fatalf("the deferred registry answers %+v (err %v), want the one loss against the deferred-only segment 948", defss, derr)
	}
	if _, aerr := f.seam.Durable(nameB); aerr != nil {
		t.Fatalf("the served open left no readable segment 948, the deferred drop's own holder: %v", aerr)
	}
	if !slices.Contains(f.segmentFileNames(t), nameB) {
		t.Fatalf("the deferred-only segment 948 is gone from the directory, want it standing on its deferral's account")
	}

	// The staying answer is the retirement's own gate, not the open's
	// refuse: a close and a reopen over the same process keep the segment
	// standing, its deferral read by the next open's retire step.
	if err := errors.Join(h.Close(), j.Close()); err != nil && !errors.Is(err, ErrClosed) {
		t.Fatalf("the served journal's close: %v", err)
	}
	f.publicOpen(t, narrow)
	if !slices.Contains(f.segmentFileNames(t), nameB) {
		t.Fatalf("the deferred-only segment 948 is gone after the reopen, want the deferral's registry read holding it")
	}
}

// TestDeferredHolderStaysHeldForTheRunningWorker holds the gate the running
// journal's retirement worker decides on: the liveness tracker's Held answer.
// The holder of a deferred loss has no live record of its own — it is the
// deferral alone that keeps it — so the tracker must count the deferral, or
// the first tick of the worker unlinks the holder and a reopen serves the
// older bytes over the synced write the crash corrupted.
func TestDeferredHolderStaysHeldForTheRunningWorker(t *testing.T) {
	f := seededJournal(t)
	file := fileOf(7)
	older := plantRecord(1, ownerTag, file, 10, 10, 0, 32)
	reserveDrop := plantRecord(1, ownerTag, file, 13, 13, 32, 32)
	offsA := plantSealedAt(t, f.seam, f.id, 947, older, reserveDrop)
	f.rotHeader(t, format.SegmentName(947), int64(offsA[1]))
	newer := plantRecord(1, ownerTag, file, 11, 11, 0, 32)
	offsB := plantSealedAt(t, f.seam, f.id, 948, newer)
	f.rotHeader(t, format.SegmentName(948), int64(offsB[0]))

	ticks := make(chan struct{}, 8)
	j := f.publicOpen(t, func(s *Settings) { s.HeadroomRecords = 2 })
	j.core.AfterRetirementTick(func() {
		select {
		case ticks <- struct{}{}:
		default:
		}
	})
	if defss, derr := durability.DeferredFor(f.dir, f.id); derr != nil || len(defss[948]) != 1 {
		t.Fatalf("the deferred registry answers %+v (err %v), want the one loss against segment 948", defss, derr)
	}
	// The worker's retirement decision reads this answer and nothing beside
	// it: a holder it answers not held is unlinked within a tick.
	if !j.core.Liveness().Held(948) {
		t.Fatalf("the tracker answers segment 948 not held while its deferred loss stands, want held: the running worker would unlink it and a reopen would serve the older bytes over the synced write")
	}
	// The worker's own decision: its first tick offers a not-held segment and
	// the next tick, a full tick later, retires it. The test waits on the
	// worker's tick events for three ticks, then closes the journal, which
	// joins the worker and any retirement it handed over, and asserts the
	// holder is still listed: a holder the worker would take is gone by then.
	for range 3 {
		select {
		case <-ticks:
		case <-time.After(30 * time.Second): // backstop only: a tick comes each second
			t.Fatalf("the retirement worker completed no tick")
		}
	}
	if err := j.Close(); err != nil {
		t.Fatalf("close the journal: %v", err)
	}
	if !slices.Contains(f.segmentFileNames(t), format.SegmentName(948)) {
		t.Fatalf("the running worker unlinked the deferred holder 948, want it standing while its deferral does")
	}
}

// TestAlreadyNamedCorruptDropDrawsNoReserve holds the reserve arithmetic of
// the open's dedup: a corrupt drop a loss record on disk already names draws
// none of the open reserve — the found record's placement takes its piece —
// so the unnamed drop behind it takes the one record the reserve holds
// instead of deferring behind a budget the named drop burned.
func TestAlreadyNamedCorruptDropDrawsNoReserve(t *testing.T) {
	f := seededJournal(t)
	file := fileOf(6)
	older := plantRecord(1, ownerTag, file, 10, 10, 0, 32)
	second := plantRecord(1, ownerTag, file, 12, 12, 32, 32)
	// The loss record the disk already holds for the newer write, over its
	// own extent: unsynced, corrupt-reasoned, naming the dropped record 11.
	named := raw.Record{
		Kind: 10, FileID: file.Bytes(), Seq: 21, Version: raw.Version{Counter: 21},
		Tag: uint32(ownerTag), Offset: 0, Length: 32,
		Loss: &raw.LossBody{Dropped: 11, Flags: 1 << 8},
	}
	// The newer write itself: its header rotted whole, the corrupt drop the
	// planted record above already names.
	newer := plantRecord(1, ownerTag, file, 11, 11, 0, 32)
	offs := plantSealedAt(t, f.seam, f.id, 946, older, newer, second, named)
	f.rotHeader(t, format.SegmentName(946), int64(offs[1]))
	f.rotHeader(t, format.SegmentName(946), int64(offs[2]))

	// Two headroom records leave an open reserve of one. The named drop must
	// leave it whole for the unnamed one, whose loss appends rather than
	// deferring behind a budget the named drop burned.
	j := f.publicOpen(t, func(s *Settings) { s.HeadroomRecords = 2 })
	events := lossEvents(t, j, core.LossCorrupt)
	if len(events) != 2 {
		t.Fatalf("the open's corrupt losses = %+v, want the found record placed back and the one unnamed drop's appended", events)
	}
	for _, ev := range events {
		if ev.Deferred {
			t.Fatalf("a corrupt loss deferred (%+v): the named drop left the reserve whole, so the unnamed one appended", ev)
		}
		switch ev.Extent.Off {
		case 0:
			// The found record placed back, carrying the planted record's
			// own version: the named drop's answer, no new record.
			if ev.Version.Counter != 21 {
				t.Fatalf("the loss placed over the named drop = %+v, want the found record's own version 21", ev)
			}
		case 32:
			// The unnamed drop's own loss, appended on the reserve.
		default:
			t.Fatalf("a corrupt loss over %+v, want one over each of the two named extents", ev.Extent)
		}
	}
	if n := len(f.lossRecords(t)); n != 2 {
		t.Fatalf("%d loss records on disk, want the planted one and the unnamed drop's: a drop the disk names appends no second record and defers against no segment", n)
	}
}

// TestANamedOnlyOpenWaitsOnNoSpareRefill holds the dedup-aware gate: an open
// whose every append answers to a record the disk already names waits on no
// refill of the spare pool, so it serves where the device refuses to make
// one more spare — a wait for a settle would fail the open there.
func TestANamedOnlyOpenWaitsOnNoSpareRefill(t *testing.T) {
	f := seededJournal(t)
	file := fileOf(6)
	older := plantRecord(1, ownerTag, file, 10, 10, 0, 32)
	named := raw.Record{
		Kind: 10, FileID: file.Bytes(), Seq: 21, Version: raw.Version{Counter: 21},
		Tag: uint32(ownerTag), Offset: 0, Length: 32,
		Loss: &raw.LossBody{Dropped: 11, Flags: 1 << 8},
	}
	newer := plantRecord(1, ownerTag, file, 11, 11, 0, 32)
	offs := plantSealedAt(t, f.seam, f.id, 946, older, newer, named)
	f.rotHeader(t, format.SegmentName(946), int64(offs[1]))

	// The device makes no more spares: every spare-target the pool falls
	// short of cannot be made, so any wait for the pool to settle fails.
	if err := f.seam.Schedule(kit.Failing(kit.OpCreate, "*", 0, syscall.EIO)); err != nil {
		t.Fatalf("schedule the spare-less device: %v", err)
	}
	j := f.publicOpen(t, func(s *Settings) { s.HeadroomRecords = 2 })
	events := lossEvents(t, j, core.LossCorrupt)
	if len(events) != 1 || events[0].Version.Counter != 21 || events[0].Deferred {
		t.Fatalf("the named-only open's corrupt losses = %+v, want the found record placed back alone, carrying its own version and deferring nothing", events)
	}
	if n := len(f.lossRecords(t)); n != 1 {
		t.Fatalf("%d loss records on disk, want the planted one alone: the drop it names appends no second record, so nothing waited on the spare pool", n)
	}
}

// TestAnOpenOwingOneRecordServesOnOneSpare holds the loss pass's wait at a
// device near full: an open owing one loss record waits for one spare a
// header-only record can roll onto, not for the pool's whole goal, so once
// the device has made one spare and refuses every other, the open serves
// with the record durable rather than refusing for a goal the record never
// needed.
func TestAnOpenOwingOneRecordServesOnOneSpare(t *testing.T) {
	f := seededJournal(t)
	file := fileOf(7)
	older := plantRecord(1, ownerTag, file, 10, 10, 0, 32)
	newer := plantRecord(1, ownerTag, file, 11, 11, 0, 32)
	offs := plantSealedAt(t, f.seam, f.id, 947, older, newer)
	f.rotHeader(t, format.SegmentName(947), int64(offs[1]))

	// The device makes one spare and then runs out of space for any other.
	if err := f.seam.Schedule(kit.FailingFrom(kit.OpCreate, "*.spare.tmp", 2, 0, syscall.ENOSPC)); err != nil {
		t.Fatalf("schedule the one-spare device: %v", err)
	}
	j := f.publicOpen(t, nil)
	events := lossEvents(t, j, core.LossCorrupt)
	if len(events) != 1 || !events[0].Synced || events[0].Deferred {
		t.Fatalf("the open's corrupt losses = %+v, want the one loss synced and not deferred", events)
	}
	if n := len(f.lossRecords(t)); n != 1 {
		t.Fatalf("%d loss records on disk after the open, want the one durable record", n)
	}
}

// TestFailedRecordsTakeTheReserveFirst holds the ordering of the open's loss
// records: the records a read-only journal failed earlier in this process —
// found on disk whole, in a segment whose sync failed — take the open
// reserve before any corrupt drop's, none of them deferred; the corrupt drop
// draws on what the reserve has left.
func TestFailedRecordsTakeTheReserveFirst(t *testing.T) {
	f := newAssembledJournal(t)
	corruptSeg := core.SegmentID(945)
	older := plantRecord(1, ownerTag, fileOf(5), 10, 10, 0, 32)
	newer := plantRecord(1, ownerTag, fileOf(5), 11, 11, 0, 32)
	offs := plantSealedAt(t, f.seam, f.id, corruptSeg, older, newer)

	j := f.open(t)
	withLimit(t, j)
	h := shareOf(t, j, 1)
	fa := fileOf(1)
	writeReady(t, h, fa, "the synced record the read-only journal keeps")
	if _, err := h.Sync(t.Context(), fa); err != nil {
		t.Fatalf("Sync the record the crash keeps: %v", err)
	}
	writeReady(t, h, fa, "the record the read-only journal fails: B")
	writeReady(t, h, fa, "the record the read-only journal fails: C")
	writeReady(t, h, fa, "the record the read-only journal fails: D")

	writingOps := []string{kit.OpOpen, kit.OpWrite, kit.OpSync, kit.OpCreate,
		kit.OpAllocate, kit.OpRename, kit.OpRemove, kit.OpTruncate, kit.OpSyncDir}
	rules := make([]kit.Rule, 0, len(writingOps))
	for _, op := range writingOps {
		rules = append(rules, kit.Failing(op, "*", 0, syscall.EROFS))
	}
	if err := f.seam.Schedule(rules...); err != nil {
		t.Fatalf("schedule the read-only device: %v", err)
	}
	if _, err := h.Sync(t.Context(), fa); !errors.Is(err, ErrJournalReadOnly) {
		t.Fatalf("the Sync on the read-only device returned %v, want ErrJournalReadOnly", err)
	}
	var failed []core.Seq
	waitFor(t, "the failed list to hold the failed records", 30*time.Second, func() bool {
		seqs, err := durability.FailedFor(f.dir, f.id)
		if err != nil {
			t.Fatalf("read the directory's failed list: %v", err)
		}
		failed = seqs
		return len(failed) == 3
	})
	f.seam.SetFault(nil)
	if err := j.Close(); err != nil {
		t.Fatalf("close the read-only journal: %v", err)
	}
	// The corrupt drop the reopen must also answer: the planted sealed
	// segment's newer record, rotted after the close like a rot the crash
	// left behind, is a synced corrupt drop beside the failed records.
	f.rotHeader(t, format.SegmentName(corruptSeg), int64(offs[1]))

	j2 := f.publicOpen(t, func(s *Settings) { s.HeadroomRecords = 8 })
	// The failure was already published when the read-only window failed each
	// record, so the loss records the open now appends publish nothing: the
	// ring holds none of the failed-window reason (RFC 1 section 5.4).
	failedEvents := lossEvents(t, j2, core.LossFailedWindow)
	if len(failedEvents) != 0 {
		t.Fatalf("the open published %+v, want no failed-window event: the records' failure was reported when the window failed", failedEvents)
	}
	corruptEvents := lossEvents(t, j2, core.LossCorrupt)
	if len(corruptEvents) != 1 || corruptEvents[0].Deferred {
		t.Fatalf("the corrupt loss = %+v, want the one corrupt drop drawing on what the failed records left of the reserve, not deferred", corruptEvents)
	}

	// The records on disk carry only the failed records' and the corrupt
	// drop's losses — none deferred — and the failed three of them are the
	// first three the open drew on the reserve, in the failed list's own
	// order: each loss record names its dropped seq, and the corrupt drop's
	// record names its own.
	//
	// Eight headroom records keep an open reserve of four — exactly the
	// three failed records plus the one the corrupt drop takes — so an
	// open that drew the corrupt drop first would starve a failed record
	// of the reserve, and one that appended the record out of the failed
	// list's order shows here as the wrong order on disk.
	records := f.lossRecords(t)
	if len(records) != 4 {
		t.Fatalf("%d loss records on disk, want the three failed records' and the corrupt drop's", len(records))
	}
	failedSorted := slices.Clone(failed)
	slices.Sort(failedSorted)
	if first := records[:len(failedSorted)]; !slices.Equal(first, failedSorted) {
		t.Fatalf("the loss records on disk = %v, want the failed list's %v first and in its order, then the corrupt drop's", records, failedSorted)
	}
	if dropped := records[len(failedSorted)]; dropped != 11 || slices.Contains(failedSorted, dropped) {
		t.Fatalf("the loss record after the failed list's names the seq %d, want the corrupt drop's own 11: the failed records took the reserve first", dropped)
	}
}

// TestASharedPieceIsCountedOnceAcrossTheLegs holds the loss pass's accounting
// where its two legs share one input: a piece — the failed record's seq over
// one extent — reaches the failed-list leg's inputs through the whole record
// the feed found on disk, and the corrupt leg's through a rotted twin of the
// same seq carrying the same columns. The pass consumes it once: the walk's
// piece key skips what the failed leg's own count already took, so a reserve
// the shared piece exhausts answers the corrupt drops past it alone — the
// refusal names the next drop's own segment, never the shared piece's — while
// the legs append one record for the shared piece and one for the corrupt
// drop beside it, never a second for the piece twice named. The second shape
// rides the same walk: the corrupt drop's extent shares a range with the
// shared piece's, yet is its own piece by its own seq, so neither leglet
// dedup swallows it — the walk counts it and a tight serving reserve holds
// exactly its record.
func TestASharedPieceIsCountedOnceAcrossTheLegs(t *testing.T) {
	f := newAssembledJournal(t)
	j := f.open(t)
	withLimit(t, j)
	h := shareOf(t, j, 1)
	fa := fileOf(1)
	writeReady(t, h, fa, "the synced record the crash keeps")
	if _, err := h.Sync(t.Context(), fa); err != nil {
		t.Fatalf("Sync the record the crash keeps: %v", err)
	}
	writeReady(t, h, fa, "the record the read-only journal fails")
	writingOps := []string{kit.OpOpen, kit.OpWrite, kit.OpSync, kit.OpCreate,
		kit.OpAllocate, kit.OpRename, kit.OpRemove, kit.OpTruncate, kit.OpSyncDir}
	rules := make([]kit.Rule, 0, len(writingOps))
	for _, op := range writingOps {
		rules = append(rules, kit.Failing(op, "*", 0, syscall.EROFS))
	}
	if err := f.seam.Schedule(rules...); err != nil {
		t.Fatalf("schedule the read-only device: %v", err)
	}
	if _, err := h.Sync(t.Context(), fa); !errors.Is(err, ErrJournalReadOnly) {
		t.Fatalf("the Sync on the read-only device returned %v, want ErrJournalReadOnly", err)
	}
	var failed []core.Seq
	waitFor(t, "the failed list to hold the failed record", 30*time.Second, func() bool {
		seqs, err := durability.FailedFor(f.dir, f.id)
		if err != nil {
			t.Fatalf("read the directory's failed list: %v", err)
		}
		failed = seqs
		return len(failed) == 1
	})
	f.seam.SetFault(nil)
	// The crash rids the directory of the failed record's own bytes — its
	// window's sync failed, so they never made durability, and the seam drops
	// them — leaving the planted whole twin below the only record the feed
	// finds for the failed seq.
	f.crash(t, j)
	if len(failed) != 1 {
		t.Fatalf("the failed list holds %v, want the one failed record's seq", failed)
	}
	shared := failed[0]

	// The shared piece's two legs: a whole record of the failed seq the feed
	// finds, and a rotted twin of the same seq over the same extent, whose
	// footer entry carries the same columns to the corrupt leg. The
	// overlapping drop beside them: its own seq over an extent sharing the
	// range [32,64) with the shared piece's, alone in its own segment.
	file := fileOf(8)
	whole := plantRecord(1, ownerTag, file, uint64(shared), uint64(shared), 0, 64)
	sharedDrop := plantRecord(1, ownerTag, file, uint64(shared), uint64(shared), 0, 64)
	offs := plantSealedAt(t, f.seam, f.id, 951, whole, sharedDrop)
	f.rotHeader(t, format.SegmentName(951), int64(offs[1]))
	over := plantRecord(1, ownerTag, file, uint64(shared)+100, uint64(shared)+100, 32, 64)
	offsOver := plantSealedAt(t, f.seam, f.id, 952, over)
	f.rotHeader(t, format.SegmentName(952), int64(offsOver[0]))
	beforeOver, err := f.seam.Durable(format.SegmentName(952))
	if err != nil {
		t.Fatalf("read the overlapping drop's planted segment: %v", err)
	}

	// Two headroom records keep an open reserve of one: the shared piece's
	// failed leg takes it whole, the corrupt leg skips it as the piece the
	// failed leg counted, and the overlapping drop alone runs past the
	// reserve — deferring against its own segment 952, never the shared
	// piece's 951, and burning no record the reserve could have held.
	narrowJ := f.publicOpen(t, func(s *Settings) { s.HeadroomRecords = 2 })
	if narrowFailed := lossEvents(t, narrowJ, core.LossFailedWindow); len(narrowFailed) != 0 {
		t.Fatalf("the open published %+v, want no failed-window event: the record's failure was reported when the window failed", narrowFailed)
	}
	narrowCorrupt := lossEvents(t, narrowJ, core.LossCorrupt)
	if len(narrowCorrupt) != 1 || !narrowCorrupt[0].Deferred || !narrowCorrupt[0].Synced ||
		narrowCorrupt[0].Extent != (core.Extent{Off: 32, Len: 64}) {
		t.Fatalf("the corrupt losses = %+v, want the one overlapping drop's loss over [32,96), synced and deferred against its own segment", narrowCorrupt)
	}
	if n := len(f.lossRecords(t)); n != 1 {
		t.Fatalf("%d loss records on disk, want the shared piece's one: the failed leg drew the whole reserve and the overlapping drop deferred", n)
	}
	if l, lerr := durability.FailedFor(f.dir, f.id); lerr != nil || len(l) != 1 || l[0] != shared {
		t.Fatalf("the serving open left %v on the directory's failed list (err %v), want the same one failed seq", l, lerr)
	}
	if defss, derr := durability.DeferredFor(f.dir, f.id); derr != nil || len(defss) != 1 {
		t.Fatalf("the deferred registry answers %+v (err %v), want the one overlapping drop alone — never the shared piece's segment", defss, derr)
	}
	// The shared piece's own segment holds nothing the open serves — its
	// whole twin is the record the read-only journal failed and the rotted
	// one a named corrupt drop — so the retire step unlinks it; the
	// deferred drop's holder stands on its deferral's account.
	if slices.Contains(f.segmentFileNames(t), format.SegmentName(951)) {
		t.Fatalf("the shared piece's segment 951 is still on disk, want the retire step unlinking the holder neither the index nor the registry answers live")
	}
	if after, aerr := f.seam.Durable(format.SegmentName(952)); aerr != nil || !slices.Equal(beforeOver, after) {
		t.Fatalf("the serving open changed the deferred drop's segment: got %x (err %v), want the planted bytes", after, aerr)
	}
	if err := narrowJ.Close(); err != nil && !errors.Is(err, ErrClosed) {
		t.Fatalf("the narrow journal's close: %v", err)
	}

	// The serving reserve answers the true demand exactly — four headroom
	// records keep two — but the pieces the serving open took answer named
	// on every retry: nothing appends a second time and no third record
	// exists for a piece twice taken.
	js := f.publicOpen(t, func(s *Settings) { s.HeadroomRecords = 4 })
	fullFailed := lossEvents(t, js, core.LossFailedWindow)
	if len(fullFailed) != 0 {
		t.Fatalf("the open published %+v, want no failed-window event: the record's failure was reported when the window failed", fullFailed)
	}
	fullCorrupt := lossEvents(t, js, core.LossCorrupt)
	if len(fullCorrupt) != 0 {
		t.Fatalf("the open's corrupt losses = %+v, want none: the shared piece and the deferred drop's pieces answer named already", fullCorrupt)
	}
	records := f.lossRecords(t)
	if len(records) > 1 {
		t.Fatalf("%d loss records on disk, want at most the one the narrow open appended: each piece taken once answers named, and no second record exists for a piece twice taken", len(records))
	}
}

// TestAPastReserveFailedPieceItsCorruptTwinRepeats holds the pass's one
// accounting of the open reserve: a failed record that runs past the
// reserve — kept by this process's failed list for the next open to answer —
// whose corrupt twin reaches the corrupt leg with the same (seq, extent)
// piece. The plan spends the piece once, by the failed leg's own rule that
// nothing past the reserve appends, and the corrupt leg answers it as the
// failed leg's own piece, never as a refusal the reserve could have refused
// before it appended: the open behind a reserve of one serves on the one
// failed record that fits it, and the corrupt twin of the one past it
// strands no record behind a refusal.
func TestAPastReserveFailedPieceItsCorruptTwinRepeats(t *testing.T) {
	f := newAssembledJournal(t)
	j := f.open(t)
	withLimit(t, j)
	h := shareOf(t, j, 1)
	fa := fileOf(1)
	writeReady(t, h, fa, "the synced record the crash keeps")
	if _, err := h.Sync(t.Context(), fa); err != nil {
		t.Fatalf("Sync the record the crash keeps: %v", err)
	}
	writeAtReady(t, h, fa, 0, payload(20)) // the failed record A over [0,20)
	writeAtReady(t, h, fa, 0, payload(34)) // the failed record B over [0,34)

	writingOps := []string{kit.OpOpen, kit.OpWrite, kit.OpSync, kit.OpCreate,
		kit.OpAllocate, kit.OpRename, kit.OpRemove, kit.OpTruncate, kit.OpSyncDir}
	rules := make([]kit.Rule, 0, len(writingOps))
	for _, op := range writingOps {
		rules = append(rules, kit.Failing(op, "*", 0, syscall.EROFS))
	}
	if err := f.seam.Schedule(rules...); err != nil {
		t.Fatalf("schedule the read-only device: %v", err)
	}
	if _, err := h.Sync(t.Context(), fa); !errors.Is(err, ErrJournalReadOnly) {
		t.Fatalf("the Sync on the read-only device returned %v, want ErrJournalReadOnly", err)
	}
	var failed []core.Seq
	waitFor(t, "the failed list to hold the two failed records", 30*time.Second, func() bool {
		seqs, err := durability.FailedFor(f.dir, f.id)
		if err != nil {
			t.Fatalf("read the directory's failed list: %v", err)
		}
		failed = seqs
		return len(failed) == 2
	})
	f.seam.SetFault(nil)
	// The crash rids the directory of the failed records' own bytes, their
	// window's sync never having made them durable.
	f.crash(t, j)
	a, b := failed[0], failed[1] // the failed list answers sorted: A before B

	// The failed records' whole twins, found on disk, feed the failed leg;
	// the second one's rotted twin feeds the corrupt leg with the same
	// (seq, extent) piece the failed leg's whole record named.
	file := fileOf(9)
	wholeA := plantRecord(1, ownerTag, file, uint64(a), uint64(a), 0, 20)
	wholeB := plantRecord(1, ownerTag, file, uint64(b), uint64(b), 0, 34)
	twin := plantRecord(1, ownerTag, file, uint64(b), uint64(b), 0, 34)
	offs := plantSealedAt(t, f.seam, f.id, 951, wholeA, wholeB, twin)
	f.rotHeader(t, format.SegmentName(951), int64(offs[2]))

	// Two headroom records keep an open reserve of one: the first failed
	// record takes it whole, the second runs past it, and the corrupt twin
	// of the second — the failed leg's own piece — answers from the plan
	// alone, appended by no leg and refused by none.
	j2, err := Open(context.Background(), f.dir, Version{}, f.id, nil, f.opts(func(s *Settings) { s.HeadroomRecords = 2 })...)
	if err != nil {
		t.Fatalf("the open returned %v, want it to serve: the corrupt twin of the failed record past the reserve is the failed leg's own piece, a refusal no record landed in front of", err)
	}
	t.Cleanup(func() { _ = j2.Close() })
	records := f.lossRecords(t)
	if len(records) != 1 || records[0] != a {
		t.Fatalf("the loss records on disk = %v, want the one the reserve held, the failed seq %d alone: the record past it appends nothing and its corrupt twin defers no further", records, a)
	}
	if events := lossEvents(t, j2, core.LossCorrupt); len(events) != 0 {
		t.Fatalf("the open published the corrupt losses %+v, want none: the twin's piece is the failed leg's own, appended by no leg", events)
	}
}

// TestRetiringHoldsUntilTheOpenLossRecordLands pins the order the open
// starts its retirement worker: only once the loss pass has landed. A
// segment whose whole content is one corrupt drop holds nothing the
// tracker pins once the open registered, so its retirement is owed to the
// loss record alone — a worker started before the pass could not see that
// debt, and stopping the record's own append at the device holds the open
// inside its loss pass with the record unborn, no location in any pending
// window and nothing durable owed: the tick would retire the segment and
// serve the older copies the crash superseded. The worker therefore waits
// for the whole pass; the hold here is the seam's own choreography — the
// first write a "*.seg" file of this directory sees is the loss record's
// own append, for the planted segment is sealed and no earlier phase
// writes one — and one full tick plus the poll's steps past it is the
// bounded wait that proves the hold, for the worker's timer is real time
// no injected clock advances.
func TestRetiringHoldsUntilTheOpenLossRecordLands(t *testing.T) {
	f := seededJournal(t)
	file := fileOf(7)
	drop := plantRecord(1, ownerTag, file, 20, 20, 0, 32)
	offs := plantSealedAt(t, f.seam, f.id, 949, drop)
	f.rotHeader(t, format.SegmentName(949), int64(offs[0]))
	name := format.SegmentName(949)

	hold := f.seam.Hold(t, kit.OpWrite, "*.seg")
	opened := make(chan error, 1)
	go func() {
		_, err := Open(context.Background(), f.dir, Version{}, f.id, nil, f.opts(nil)...)
		opened <- err
	}()
	select {
	case <-hold.Reached():
	case <-time.After(30 * time.Second):
		t.Fatalf("the open reached no segment write to hold: the loss record's own append never came")
	}

	// The bounded wait is the assertion: the worker's prompt-only candidate
	// waits one full tick from its offer, so a worker running before the
	// pass hands the segment over inside this window. The poll watches the
	// segment's own file, never a clock alone.
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		if !slices.Contains(f.segmentFileNames(t), name) {
			t.Fatalf("the corrupt-drop segment 949 was unlinked while its loss record's append was held: the retirement ran ahead of the open's loss record, want it held until the pass lands the mask")
		}
		time.Sleep(100 * time.Millisecond)
	}
	hold.Release()
	if err := <-opened; err != nil {
		t.Fatalf("the open whose loss record's append was held: %v", err)
	}
	if n := len(f.lossRecords(t)); n != 1 {
		t.Fatalf("%d loss records on disk after the held open, want the one naming the dropped record", n)
	}
}

// TestAFailedOpenUnlinksNothing holds the debris of the loss pass's own
// failure: an open whose loss record could not be made durable — a device
// refusing the record's sync — answers no journal, and the segment the
// record was to mask still stands when it is gone, file whole and
// readable. Nothing a refused or failed open started may unlink, for the
// unlink of the masked record's holder is the one write the recovery
// cannot take back. The directory it leaves answers every later open
// with the ended-segment refusal over the empty spare the failed sync
// ended — the honest closed word until the emptying exists, for a mask
// the device cannot make durable must not be traded for a served journal.
func TestAFailedOpenUnlinksNothing(t *testing.T) {
	f := seededJournal(t)
	file := fileOf(7)
	drop := plantRecord(1, ownerTag, file, 20, 20, 0, 32)
	offs := plantSealedAt(t, f.seam, f.id, 950, drop)
	f.rotHeader(t, format.SegmentName(950), int64(offs[0]))
	name := format.SegmentName(950)

	// The device refuses the loss record's own sync — the second "*.seg"
	// sync the open makes, for the tail proof's sync of the rotted segment
	// came first: the pass's Lose fails, the open is refused, and the
	// directory keeps the rotted segment.
	if err := f.seam.Schedule(kit.FailingFrom(kit.OpSync, "*.seg", 2, 1, syscall.EIO)); err != nil {
		t.Fatalf("schedule the sync-refusing device: %v", err)
	}
	_, err := Open(context.Background(), f.dir, Version{}, f.id, nil, f.opts(nil)...)
	if err == nil {
		t.Fatalf("the open over a device refusing the loss record's sync served, want the refusal")
	}
	if errors.Is(err, ErrOpenRefused) || !strings.Contains(err.Error(), "input/output error") {
		t.Fatalf("the failed open answered %v, want the device's own sync failure of the loss record", err)
	}
	if _, aerr := f.seam.Durable(name); aerr != nil {
		t.Fatalf("the failed open left no readable segment 950, the record the loss was to mask: %v", aerr)
	}

	f.seam.SetFault(nil)
	// The later open answers the failed one's debris: the loss record the
	// failed sync never landed is appended whole on the failed list's
	// account, the rotted segment retires — sealed, nothing holding the
	// masked drop — and the empty spare the failed sync ended is emptied
	// and unlinked, its retirement durable with the directory sync it
	// carries. The served journal's read of the rotted extent answers
	// lost, never the older content the corrupted record superseded. The
	// failed sync's record still sits in the live file of the spare it
	// ended, placed but unsynced, so the pass appends the loss again and
	// the emptying carries that found copy too: two durable records may
	// name the one piece, which the next open's placement answers once.
	again, aerr := Open(context.Background(), f.dir, Version{}, f.id, nil, f.opts(nil)...)
	if aerr != nil {
		t.Fatalf("the open after the failed one returned %v, want it serving: the failed list answers the record the failed open could not mask", aerr)
	}
	t.Cleanup(func() { _ = again.Close() })
	losses := f.lossRecords(t)
	if len(losses) != 2 {
		t.Fatalf("%d loss records on disk after the serving open, want two, both naming the dropped record: the pass's own and the emptying's carried copy", len(losses))
	}
	for _, seq := range losses {
		if seq != 20 {
			t.Fatalf("a loss record on disk names seq %d, want only the dropped record 20", seq)
		}
	}
	if slices.Contains(f.segmentFileNames(t), name) {
		t.Fatalf("the rotted segment 950 is still on disk after the serving open, want the retire step masking its drop and unlinking it")
	}
	h := shareOf(t, again, ownerTag)
	if _, missing, _, _, rerr := h.ReadAt(t.Context(), file, 0, make([]byte, 32)); rerr != nil || len(missing) != 1 || missing[0] != (Extent{Off: 0, Len: 32}) {
		t.Fatalf("reading the rotted extent after the serving open: missing %+v, err %v, want the whole extent lost", missing, rerr)
	}
}
