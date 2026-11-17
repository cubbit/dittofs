package journal

import (
	"bytes"
	"context"
	"errors"
	"fmt"
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
// segments: the public Open runs the recovery itself, refuses only the
// directory whose open ends a segment, and appends its own loss records — the
// failed-list records first on the open reserve, then the corrupt drops,
// deferred past it — before it returns. Every check reads its answers through
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
func (f *assembledJournal) lossRecords(t *testing.T) []core.Seq {
	t.Helper()
	var out []core.Seq
	for _, name := range f.segmentFileNames(t) {
		kind, id := format.ClassifyName(name)
		if kind != format.NameSegment {
			continue
		}
		b, err := f.seam.Durable(name)
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

// TestOpenEndingASegmentIsRefusedNamingIt holds the checkpoint's refusal at
// its own caller: an open whose recovery ends a segment — here over a torn
// slot below a record that verifies, the ending whole with no cut — answers
// ErrNotImplemented naming the segment, serves nothing and changes nothing
// on disk, until the emptying that unlinks such a segment is built.
func TestOpenEndingASegmentIsRefusedNamingIt(t *testing.T) {
	f := seededJournal(t)
	seg := core.SegmentID(941)
	a := plantRecord(1, ownerTag, fileOf(2), 20, 30, 0, 32)
	rr := plantRecord(1, ownerTag, fileOf(2), 21, 31, 32, 32)

	b := raw.SegmentHeader{Identity: f.id, ID: uint64(seg)}.Bytes()
	a.Identity, a.Segment, a.RecordOffset = f.id, uint64(seg), uint32(len(b))
	b = append(b, a.Bytes()...)
	// The torn slot: a zeroed gap no proof covers, below a record that
	// verifies whole — the classing's ending whole, an ended fate with no
	// cut, left standing for the emptying that unlinks it.
	b = append(b, make([]byte, 16)...)
	rr.Identity, rr.Segment, rr.RecordOffset = f.id, uint64(seg), uint32(len(b))
	b = append(b, rr.Bytes()...)
	plantSealedBytes(t, f, format.SegmentName(seg), b)

	before, err := f.seam.Durable(format.SegmentName(seg))
	if err != nil {
		t.Fatalf("read the planted segment: %v", err)
	}
	_, err = Open(context.Background(), f.dir, Version{}, f.id, nil, f.opts(nil)...)
	if !errors.Is(err, ErrNotImplemented) {
		t.Fatalf("the open of a directory whose recovery ends a segment returned %v, want ErrNotImplemented", err)
	}
	if !strings.Contains(err.Error(), fmt.Sprintf("segment %d", seg)) {
		t.Fatalf("the refusal %q names no ended segment, want it naming segment %d", err, seg)
	}
	if after, aerr := f.seam.Durable(format.SegmentName(seg)); aerr != nil || !slices.Equal(before, after) {
		t.Fatalf("the refusing open changed the ended segment: got %x (err %v), want the planted bytes", after, aerr)
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

// TestPastReserveDeferralServes holds the deferral past the open reserve at
// the callers it reaches: more corrupt records than the reserve holds still
// open, each loss past the reserve is published marked deferred, its extent
// reads lost, and a synced one is yielded by Since until the caller settles
// it.
func TestPastReserveDeferralServes(t *testing.T) {
	f := seededJournal(t)
	file := fileOf(4)
	older := plantRecord(1, ownerTag, file, 10, 10, 0, 32)
	newer := plantRecord(1, ownerTag, file, 11, 11, 32, 32)
	offs := plantSealedAt(t, f.seam, f.id, 944, older, newer)
	f.rotHeader(t, format.SegmentName(944), int64(offs[0]))
	f.rotHeader(t, format.SegmentName(944), int64(offs[1]))

	j := f.publicOpen(t, func(s *Settings) { s.HeadroomRecords = 2 })
	events := lossEvents(t, j, core.LossCorrupt)
	if len(events) != 2 {
		t.Fatalf("the open published %d corrupt losses, want the one event of each rotted drop: %+v", len(events), events)
	}
	nonDeferred, deferred := 0, 0
	for _, ev := range events {
		if ev.Deferred {
			if !ev.Synced {
				t.Fatalf("the deferred loss %+v is not synced, want the classing's own flag", ev)
			}
			deferred++
			continue
		}
		nonDeferred++
	}
	if nonDeferred != 1 || deferred != 1 {
		t.Fatalf("the open appended %d of the two corrupt losses and deferred %d, want 1 on the reserve and 1 past it", nonDeferred, deferred)
	}
	if n := len(f.lossRecords(t)); n != 1 {
		t.Fatalf("%d loss records on disk, want the one the open reserve held: a deferred loss appends none", n)
	}

	h := shareOf(t, j, ownerTag)
	for _, ext := range []Extent{{Off: 0, Len: 32}, {Off: 32, Len: 32}} {
		p := make([]byte, ext.Len)
		if _, missing, _, _, err := h.ReadAt(t.Context(), file, ext.Off, p); err != nil || len(missing) != 1 || missing[0] != ext {
			t.Fatalf("reading the deferred loss's extent %+v: n 0, missing %+v, err %v, want the whole extent lost", ext, missing, err)
		}
	}
	// The synced deferred loss yields its marker through Since until the
	// caller settles it: the loss tells the metadata it lost a synced write.
	var yielded int
	for ch, err := range h.Since(t.Context(), file, Version{}) {
		if err != nil {
			t.Fatalf("Since after the deferred loss: %v", err)
		}
		if ch.Kind == ChangeLoss {
			yielded++
		}
	}
	if yielded != 2 {
		t.Fatalf("Since yielded %d lost-write changes, want one per corrupt extent, the deferred included", yielded)
	}
	h.Settle(file, v(12))
	for ch, err := range h.Since(t.Context(), file, Version{}) {
		if err != nil {
			t.Fatalf("Since after the settle: %v", err)
		}
		if ch.Kind == ChangeLoss {
			t.Fatalf("Since yielded the settled lost-write change %+v, want it settled away", ch)
		}
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

	j2 := f.publicOpen(t, func(s *Settings) { s.HeadroomRecords = 10 })
	failedEvents := lossEvents(t, j2, core.LossFailedWindow)
	if len(failedEvents) != 3 {
		t.Fatalf("the open published %d failed-window losses, want one per failed record: %+v", len(failedEvents), failedEvents)
	}
	for _, ev := range failedEvents {
		if ev.Deferred {
			t.Fatalf("a failed record's loss %+v is deferred, want none: the failed leg never defers", ev)
		}
	}
	corruptEvents := lossEvents(t, j2, core.LossCorrupt)
	if len(corruptEvents) != 1 || corruptEvents[0].Deferred {
		t.Fatalf("the corrupt loss = %+v, want the one corrupt drop drawing on what the failed records left of the reserve, not deferred", corruptEvents)
	}

	// The records on disk carry only the failed records' and the corrupt
	// drop's losses — none deferred — and the failed three of them are the
	// first three the open drew on the reserve: each loss record names its
	// dropped seq, and the corrupt drop's record names its own.
	records := f.lossRecords(t)
	if len(records) != 4 {
		t.Fatalf("%d loss records on disk, want the three failed records' and the corrupt drop's", len(records))
	}
	failedSorted := slices.Clone(failed)
	slices.Sort(failedSorted)
	var got []core.Seq
	for _, dropped := range records {
		if slices.Contains(failedSorted, dropped) {
			got = append(got, dropped)
		}
	}
	slices.Sort(got)
	if !slices.Equal(got, failedSorted) {
		t.Fatalf("the loss records on disk name the failed list's %v among them, got %v of %v", failedSorted, got, records)
	}
}
