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

// TestPastReserveLossRefusesTheOpen holds the reserve's far edge at the
// open that reaches it: corrupt drops past what the open reserve holds
// answer no served journal. The open refuses before it appends anything —
// a deferred corrupt loss hands its record to the copy that retires the
// segment holding the drop, but no retirement gate reads the deferral's
// registry, so the running journal would unlink that segment and let the
// drop's older copies serve over a write the crash had synced — and the
// refusal burns nothing: each refused open leaves the rotted bytes, the
// loss records and the failed list exactly as the last one found them, so
// the open that finally holds a reserve answers whole. The row's
// deferral-serving acceptance returns when the emptying that copies the
// deferred records out exists to withdraw the refusal; this row asserts
// behaviour, not history — retune it with that withdrawal.
func TestPastReserveLossRefusesTheOpen(t *testing.T) {
	f := seededJournal(t)
	file := fileOf(4)
	older := plantRecord(1, ownerTag, file, 10, 10, 0, 32)
	newer := plantRecord(1, ownerTag, file, 11, 11, 32, 32)
	offs := plantSealedAt(t, f.seam, f.id, 944, older, newer)
	f.rotHeader(t, format.SegmentName(944), int64(offs[0]))
	f.rotHeader(t, format.SegmentName(944), int64(offs[1]))
	before, err := f.seam.Durable(format.SegmentName(944))
	if err != nil {
		t.Fatalf("read the planted segment: %v", err)
	}

	// Two headroom records keep an open reserve of one: the two rotted
	// records are two corrupt drops past what it holds. The refusal precedes
	// every append, so no retry burns a record the reserve could have held.
	refuse := func(nth string) {
		_, err := Open(context.Background(), f.dir, Version{}, f.id, nil, f.opts(func(s *Settings) { s.HeadroomRecords = 2 })...)
		if !errors.Is(err, ErrNotImplemented) {
			t.Fatalf("the %s open of two corrupt drops past the reserve returned %v, want the ErrNotImplemented refusal", nth, err)
		}
		if !strings.Contains(err.Error(), "the first against segment 944") {
			t.Fatalf("the %s refusal %q names no segment the losses past the reserve defer against, want it naming segment 944", nth, err)
		}
		if after, aerr := f.seam.Durable(format.SegmentName(944)); aerr != nil || !slices.Equal(before, after) {
			t.Fatalf("the %s refusing open changed the rotted segment: got %x (err %v), want the planted bytes", nth, after, aerr)
		}
		if n := len(f.lossRecords(t)); n != 0 {
			t.Fatalf("%d loss records on disk after the %s refusal, want none", n, nth)
		}
		failed, ferr := durability.FailedFor(f.dir, f.id)
		if ferr != nil || len(failed) != 0 {
			t.Fatalf("the %s refusal left %v on the directory's failed list (err %v), want none: the pass took no piece it owes a record for", nth, failed, ferr)
		}
	}
	refuse("first")
	refuse("second")

	// The refusal is the reserve arithmetic's own, not a wall before the
	// loss pass: a reserve that holds both drops serves them, all losses
	// appended and none deferred.
	j := f.publicOpen(t, func(s *Settings) { s.HeadroomRecords = 50 })
	events := lossEvents(t, j, core.LossCorrupt)
	if len(events) != 2 || events[0].Deferred || events[1].Deferred {
		t.Fatalf("the open over a full reserve published %d corrupt losses %+v, want the two, none deferred", len(events), events)
	}
	if n := len(f.lossRecords(t)); n != 2 {
		t.Fatalf("%d loss records on disk after the open over the full reserve, want the two", n)
	}
	h := shareOf(t, j, ownerTag)
	if _, missing, _, _, err := h.ReadAt(t.Context(), file, 0, make([]byte, 32)); err != nil || len(missing) != 1 || missing[0] != (Extent{Off: 0, Len: 32}) {
		t.Fatalf("reading the first rotted extent after the serving open: missing %+v, err %v, want the whole extent lost", missing, err)
	}
}

// TestDeferredOnlySegmentStandsBehindTheRefusal holds the probe shape the
// refusal answers: two corrupt drops over a one-record reserve, and the
// past-reserve drop the only record of its own segment. Under the deferral
// this open once served, that segment held nothing but the drop whose
// loss deferred, so the running journal's retirement unlinked it and the
// reopen after a close served the older record the dropped write
// superseded, missing nothing over a synced write. The open refuses and
// the deferred-only segment stands; and the served variant of this row —
// a regression back to the deferral, or the emptying's withdrawal of the
// refusal — must read the drop's own extent lost, never the retired
// segment's older bytes, which is the row's negative verification: retune
// the refusal assertion with that withdrawal, keep the read.
func TestDeferredOnlySegmentStandsBehindTheRefusal(t *testing.T) {
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
	// holder is the drop whose loss once deferred against it.
	newer := plantRecord(1, ownerTag, file, 11, 11, 0, 32)
	offsB := plantSealedAt(t, f.seam, f.id, 948, newer)
	f.rotHeader(t, format.SegmentName(948), int64(offsB[0]))

	narrow := func(s *Settings) { s.HeadroomRecords = 2 }
	nameB := format.SegmentName(948)
	open := func() (*Journal, error) {
		return Open(context.Background(), f.dir, Version{}, f.id, nil, f.opts(narrow)...)
	}
	j, err := open()
	if err == nil {
		// The negative verification, kept for the day the refusal is
		// withdrawn: a served open must still never serve the retired
		// segment's older bytes for the dropped write — the loss that
		// masks it answers wherever its record lives.
		t.Cleanup(func() { _ = j.Close() })
		h := shareOf(t, j, ownerTag)
		p := make([]byte, 32)
		n, missing, _, _, rerr := h.ReadAt(t.Context(), file, 0, p)
		if rerr != nil || n != 0 || len(missing) != 1 || missing[0] != (Extent{Off: 0, Len: 32}) {
			t.Fatalf("the served open answered the dropped extent n %d, missing %+v, err %v — the older record served over the synced write, want the whole extent lost", n, missing, rerr)
		}
		t.Fatalf("the open of two corrupt drops past the reserve served, want the ErrNotImplemented refusal")
	}
	if !errors.Is(err, ErrNotImplemented) {
		t.Fatalf("the open of a deferred-only corrupt segment returned %v, want the ErrNotImplemented refusal", err)
	}
	if !strings.Contains(err.Error(), "the first against segment 948") {
		t.Fatalf("the refusal %q names no segment the past-reserve loss defers against, want it naming the deferred-only segment 948", err)
	}
	// The refused open unlinked nothing: the deferred-only segment stands
	// exactly where the planting left it.
	if _, aerr := f.seam.Durable(nameB); aerr != nil {
		t.Fatalf("the refusing open left no readable segment 948, the deferred drop's own holder: %v", aerr)
	}
	if n := len(f.lossRecords(t)); n != 0 {
		t.Fatalf("%d loss records on disk after the refusal, want none", n)
	}
	// The refusal answers every retry the same way.
	if _, err := open(); !errors.Is(err, ErrNotImplemented) {
		t.Fatalf("the second open returned %v, want the same refusal", err)
	}
	if _, aerr := f.seam.Durable(nameB); aerr != nil {
		t.Fatalf("the second refusing open left no readable segment 948: %v", aerr)
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
	if errors.Is(err, ErrNotImplemented) || !strings.Contains(err.Error(), "input/output error") {
		t.Fatalf("the failed open answered %v, want the device's own sync failure of the loss record", err)
	}
	if _, aerr := f.seam.Durable(name); aerr != nil {
		t.Fatalf("the failed open left no readable segment 950, the record the loss was to mask: %v", aerr)
	}

	f.seam.SetFault(nil)
	// The empty spare the failed sync ended leaves every later open
	// refusing — the standing ended-segment seam — and the rotted
	// segment stands under it, unlinked by nothing.
	again, aerr := Open(context.Background(), f.dir, Version{}, f.id, nil, f.opts(nil)...)
	if aerr == nil {
		t.Cleanup(func() { _ = again.Close() })
		t.Fatalf("the open after the failed one served the directory, want the ended-segment refusal")
	}
	if !errors.Is(aerr, ErrNotImplemented) || !strings.Contains(aerr.Error(), "the open ends segment") {
		t.Fatalf("the open after the failed one answered %v, want the refusal naming the ended empty spare", aerr)
	}
	if _, derr := f.seam.Durable(name); derr != nil {
		t.Fatalf("the refusing reopen left no readable segment 950: %v", derr)
	}
}
