package recovery

import (
	"context"
	"errors"
	"slices"
	"sync"
	"syscall"
	"testing"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/format"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/index"
	"github.com/cubbit/dittofs/pkg/block/journal/journaltest/kit"
	"github.com/cubbit/dittofs/pkg/block/journal/journaltest/raw"
)

// testCap is the segment cap every fixture plants at: a spare is a whole
// cap-sized file, and every segment file here is exactly that long.
const testCap = 8 << 10

// plantTake plants segment id's record-less take at the cap: a header naming
// pred and nothing after it, the whole-cap file a take on a spare leaves —
// the shape the spares phase re-adopts under its size check, where a
// header-only file would be a short spare the phase unlinks instead.
func plantTake(r *rig, id, pred core.SegmentID) {
	r.t.Helper()
	b := raw.SegmentHeader{Identity: journalID, ID: uint64(id), Predecessor: uint64(pred)}.Bytes()
	b = append(b, make([]byte, testCap-int64(len(b)))...)
	r.plantSeg(id, b)
}

// runCandidates runs the open's recovery over the rig's directory and returns
// its state, the answer every spare-candidate check reads.
func (r *rig) runCandidates() State {
	r.t.Helper()
	st, err := Run(context.Background(), r.runDeps(), core.Version{}, journalID, nil)
	if err != nil {
		r.t.Fatalf("Run: %v", err)
	}
	return st
}

// candidates returns the spare candidates the state names: the record-less
// segments of the classes an open under this journal's identity may re-adopt.
func candidates(st State) []core.SegmentID {
	return spareCandidates(st.Attached, st.Rebuilt.Scans, namedBySuccessor(st.Attached))
}

// TestSpareCandidatesHasARecordlessUntakenSpare plants one all-zero segment
// at the cap — a spare no stream took — and reads it as the one candidate: a
// segment whose header slot is zeros and in which no record and no marker
// verifies holds nothing that was ever durable.
func TestSpareCandidatesHasARecordlessUntakenSpare(t *testing.T) {
	r := newRig(t)
	r.writeFormat()
	id := core.SegmentID(6)
	r.write(format.SegmentName(id), make([]byte, testCap))

	st := r.runCandidates()

	got, want := candidates(st), []core.SegmentID{id}
	if !slices.Equal(got, want) {
		t.Errorf("spareCandidates: an all-zero record-less segment's candidates got %v, want %v", got, want)
	}
}

// TestSpareCandidatesHasAnInterruptedRecycleSpared plants a segment sealed
// full — header, one synced record, a seal marker and the old trailer at the
// cap's last 32 bytes — then renames it to a fresh id, the rename synced and
// nothing zeroed: a recycle a crash interrupted after the rename. The old
// life's records and marker name the segment the file used to be, so none of
// them verifies under the new name and the file is re-adoptable; the old id,
// whose name no longer exists, is not.
func TestSpareCandidatesHasAnInterruptedRecycleSpared(t *testing.T) {
	r := newRig(t)
	r.writeFormat()
	x, y := core.SegmentID(3), core.SegmentID(9)
	plantInterruptedRecycle(r, x, y)

	st := r.runCandidates()

	got := candidates(st)
	if !slices.Contains(got, y) {
		t.Errorf("spareCandidates: the interrupted recycle's new id %d is not a candidate, got %v: the old life's records name the old id and verify nowhere under the new name", y, got)
	}
	if slices.Contains(got, x) {
		t.Errorf("spareCandidates: the interrupted recycle's old id %d is a candidate, got %v: its name is gone and the ledger goes by the file's name", x, got)
	}
}

// TestSpareCandidatesHasAZeroHeaderTakeACrashNeverSynced plants a synced
// all-zero segment at the cap, then a take's header written into its slot
// without a sync, and crashes: the crash puts the file back to its all-zero
// durable image, so the take never happened and the segment is a candidate
// again — an untaken spare, its zero-header slot proving it.
func TestSpareCandidatesHasAZeroHeaderTakeACrashNeverSynced(t *testing.T) {
	r := newRig(t)
	r.writeFormat()
	id := core.SegmentID(6)
	r.write(format.SegmentName(id), make([]byte, testCap))

	// The take: a header into the slot, never synced.
	f, err := r.seam.Open(format.SegmentName(id))
	if err != nil {
		t.Fatalf("fixture: open %s: %v", format.SegmentName(id), err)
	}
	if _, err := f.WriteAt(raw.SegmentHeader{Identity: journalID, ID: uint64(id)}.Bytes(), 0); err != nil {
		t.Fatalf("fixture: write the take's header: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("fixture: close %s: %v", format.SegmentName(id), err)
	}
	if err := r.seam.Crash(); err != nil {
		t.Fatalf("fixture: crash: %v", err)
	}

	// The crash ended the process that took the spare: recovery runs on the
	// durable image alone.
	if b := r.fileBytes(format.SegmentName(id)); !slices.Equal(b, make([]byte, testCap)) {
		t.Errorf("fixture: the unsynced take's header survived the crash: got %x, want the all-zero image", b[:raw.SegmentHeaderSize])
	}
	st := r.runCandidates()

	if want := []core.SegmentID{id}; !slices.Equal(st.Attached.ZeroHeader, want) {
		t.Errorf("attach: the take the crash undid was classed ZeroHeader %v, want %v", st.Attached.ZeroHeader, want)
	}
	if got, want := candidates(st), []core.SegmentID{id}; !slices.Equal(got, want) {
		t.Errorf("spareCandidates: a take the crash never made durable got candidates %v, want %v", got, want)
	}
}

// TestSpareCandidatesNeverHasAForeignSegment plants a record-less segment
// whose header verifies naming another journal: no candidate test may reach
// it, for the identity is checked first — a foreign segment is never
// re-adopted and never zeroed, whatever its records hold, because adopting it
// could destroy the only copy of another journal's content. Its bytes are
// unchanged and Rejected counts it.
func TestSpareCandidatesNeverHasAForeignSegment(t *testing.T) {
	other := core.JournalID{'a', 'n', 'o', 't', 'h', 'e', 'r'}
	r := newRig(t)
	r.writeFormat()
	id := core.SegmentID(5)
	b := make([]byte, testCap)
	copy(b, raw.SegmentHeader{Identity: other, ID: uint64(id)}.Bytes())
	r.write(format.SegmentName(id), b)

	st := r.runCandidates()

	if got := candidates(st); slices.Contains(got, id) {
		t.Errorf("spareCandidates: the foreign segment %d was a candidate, got %v: the identity is checked before any record-less test", id, got)
	}
	if len(st.Attached.Foreign) != 1 || st.Attached.Foreign[0].ID != id || st.Attached.Foreign[0].Header.Identity != other {
		t.Errorf("attach: the foreign segment was not classed foreign, got %+v, want one id %d of identity %x",
			st.Attached.Foreign, id, other)
	}
	if st.Attached.Rejected != 1 {
		t.Errorf("attach: Rejected got %d, want 1, the foreign segment", st.Attached.Rejected)
	}
	if after := r.fileBytes(format.SegmentName(id)); !slices.Equal(after, b) {
		t.Errorf("spares: the foreign segment's bytes changed, got %x, want the bytes it arrived with", after[:raw.SegmentHeaderSize])
	}
}

// TestSpareCandidatesNeverHasASegmentWithARecord plants an own segment with
// one verifying record, and another with a verifying seal marker alone: a
// segment that holds anything a sync could have made durable is never
// re-adopted, for zeroing it would drop the record, and a sealed one is a
// sealed predecessor, never a spare.
func TestSpareCandidatesNeverHasASegmentWithARecord(t *testing.T) {
	// One verifying record: the scan's VerifiedEnd passes the first record
	// slot, so the segment is not record-less.
	r := newRig(t)
	r.writeFormat()
	id := core.SegmentID(4)
	b := buildSeg(id, rec(1, func(rr *raw.Record) {
		rr.Write = &raw.WriteBody{MTime: 1, Payload: bytes16()}
	}))
	b = append(b, make([]byte, testCap-int64(len(b)))...)
	r.plantSeg(id, b)

	st := r.runCandidates()

	if got := candidates(st); slices.Contains(got, id) {
		t.Errorf("spareCandidates: the segment %d holding one verifying record was a candidate, got %v, want it absent", id, got)
	}

	// A verifying seal marker alone: the scan is sealed, so the segment is
	// never a spare.
	q := newRig(t)
	q.writeFormat()
	marked := core.SegmentID(8)
	m := raw.SegmentHeader{Identity: journalID, ID: uint64(marked)}.Bytes()
	m = append(m, raw.SealMarker{Identity: journalID, Segment: uint64(marked), Offset: uint64(len(m))}.Bytes()...)
	m = append(m, make([]byte, testCap-int64(len(m)))...)
	q.plantSeg(marked, m)

	qst := q.runCandidates()

	if got := candidates(qst); slices.Contains(got, marked) {
		t.Errorf("spareCandidates: the segment %d a verifying seal marker seals was a candidate, got %v, want it absent", marked, got)
	}
}

// TestSpareCandidatesNeverHasASuccessorsPredecessor plants a record-less own
// segment and a successor whose header names it as predecessor: a named
// segment was sealed before its successor was made, so re-adopting it would
// drop a sealed segment's whole life. Its trailer names a footer exactly past
// the seal marker slot its end lacks, so the open reads its missing marker as
// the marker alone rotted rather than refusing on a named predecessor's
// missing end.
func TestSpareCandidatesNeverHasASuccessorsPredecessor(t *testing.T) {
	r := newRig(t)
	r.writeFormat()
	z, w := core.SegmentID(2), core.SegmentID(7)

	// Z: record-less, its trailer naming a footer just past the marker slot
	// at the first record slot's end.
	b := raw.SegmentHeader{Identity: journalID, ID: uint64(z)}.Bytes()
	b = append(b, make([]byte, testCap-raw.TrailerSize-int64(len(b)))...)
	b = append(b, raw.Trailer(1, uint64(format.SegmentHeaderSize+format.SealMarkerSize))...)
	r.plantSeg(z, b)

	// W: the successor whose header names Z.
	s := raw.SegmentHeader{Identity: journalID, ID: uint64(w), Predecessor: uint64(z)}.Bytes()
	s = append(s, make([]byte, testCap-int64(len(s)))...)
	r.plantSeg(w, s)

	st := r.runCandidates()

	if got := candidates(st); slices.Contains(got, z) {
		t.Errorf("spareCandidates: segment %d, named as its successor %d's predecessor, was a candidate, got %v, want it absent", z, w, got)
	}
}

// sealedFull returns the bytes of segment id sealed full — its header, one
// synced record, a seal marker and the cap's trailer — as the journal's
// writer would have left a segment it sealed for recycle.
func sealedFull(id core.SegmentID) []byte {
	b := raw.SegmentHeader{Identity: journalID, ID: uint64(id)}.Bytes()
	rec := rec(1, func(rr *raw.Record) {
		rr.Write = &raw.WriteBody{MTime: 1, Payload: bytes16()}
	})
	rec.Identity = journalID
	rec.Segment = uint64(id)
	rec.RecordOffset = uint32(len(b))
	rec.SyncedThrough = uint64(format.SegmentHeaderSize + int64(len(rec.Bytes()))) // the sync covered the record whole
	b = append(b, rec.Bytes()...)
	markerAt := int64(len(b))
	b = append(b, raw.SealMarker{Identity: journalID, Segment: uint64(id), Offset: uint64(markerAt)}.Bytes()...)
	b = append(b, make([]byte, testCap-raw.TrailerSize-int64(len(b)))...)
	b = append(b, raw.Trailer(1, uint64(markerAt+format.SealMarkerSize))...)
	return b
}

// plantInterruptedRecycle plants a sealed full segment under its old id and
// renames it to its fresh y, syncing the directory between: the durable
// state of a recycle a crash interrupted after its rename, the old header
// and the old trailer still in the file the new name reaches.
func plantInterruptedRecycle(r *rig, x, y core.SegmentID) {
	r.t.Helper()
	r.plantSeg(x, sealedFull(x))
	if err := r.seam.Rename(format.SegmentName(x), format.SegmentName(y)); err != nil {
		r.t.Fatalf("fixture: rename %s to %s: %v", format.SegmentName(x), format.SegmentName(y), err)
	}
	if err := r.seam.SyncDir(); err != nil {
		r.t.Fatalf("fixture: sync dir: %v", err)
	}
}

// TestSparesPhaseUnlinksAShortSpare plants two record-less spares under the
// cap — a half cap file and an empty one, the crash-shapes a half made
// spare and a crash's leftover — beside one whole spare, and runs the open:
// a spare the pool could take must hold exactly the cap, so each short one
// is unlinked and left for the pool to make again — never zeroed, no byte of
// a file about to be removed is written — while the whole one beside them is
// adopted. The unlinks are recorded ascending by id.
func TestSparesPhaseUnlinksAShortSpare(t *testing.T) {
	r := newRig(t)
	r.writeFormat()
	empty, half, whole := core.SegmentID(9), core.SegmentID(4), core.SegmentID(6)
	r.write(format.SegmentName(empty), nil)
	r.write(format.SegmentName(half), make([]byte, testCap/2))
	r.write(format.SegmentName(whole), make([]byte, testCap))
	emptyWrites := r.seam.CallsFor(kit.OpWrite, format.SegmentName(empty))
	halfWrites := r.seam.CallsFor(kit.OpWrite, format.SegmentName(half))

	st := r.runCandidates()
	if got, want := st.Spares.Unlinked, []core.SegmentID{half, empty}; !slices.Equal(got, want) {
		t.Errorf("spares: the short spares' unlinking got %v, want %v, ascending by id", got, want)
	}
	if got, want := st.Spares.Adopted, []AdoptedSpare{{ID: whole}}; !slices.Equal(got, want) {
		t.Errorf("spares: the whole spare beside the short ones got adoptions %v, want %v", got, want)
	}
	for _, id := range []core.SegmentID{empty, half} {
		if slices.Contains(r.listing(), format.SegmentName(id)) {
			t.Errorf("spares: the short spare %s is still in the directory %v, want it unlinked", format.SegmentName(id), r.listing())
		}
	}
	if got := r.seam.CallsFor(kit.OpWrite, format.SegmentName(empty)); got != emptyWrites {
		t.Errorf("spares: the empty spare was written to, got %d writes, want %d, the count before the run: a file about to be removed is never zeroed", got, emptyWrites)
	}
	if got := r.seam.CallsFor(kit.OpWrite, format.SegmentName(half)); got != halfWrites {
		t.Errorf("spares: the half-cap spare was written to, got %d writes, want %d, the count before the run: a file about to be removed is never zeroed", got, halfWrites)
	}
}

// TestSparesPhaseUnlinksAnOverCapSpare plants a record-less .seg over the
// cap beside one whole spare for each size: a file over the cap answers the
// size check the same way one under it does — the pool takes whole
// cap-sized segments only — so it is unlinked, recorded, never zeroed and
// never adopted, and the file is gone from the directory.
func TestSparesPhaseUnlinksAnOverCapSpare(t *testing.T) {
	for _, row := range []struct {
		name string
		size int64
	}{
		{"one byte over the cap", testCap + 1},
		{"half a cap over", testCap + testCap/2},
	} {
		t.Run(row.name, func(t *testing.T) {
			r := newRig(t)
			r.writeFormat()
			over, whole := core.SegmentID(5), core.SegmentID(6)
			r.write(format.SegmentName(over), make([]byte, row.size))
			r.write(format.SegmentName(whole), make([]byte, testCap))
			overWrites := r.seam.CallsFor(kit.OpWrite, format.SegmentName(over))

			st := r.runCandidates()
			if got, want := st.Spares.Unlinked, []core.SegmentID{over}; !slices.Equal(got, want) {
				t.Errorf("spares: the over-cap spare's unlinking got %v, want %v", got, want)
			}
			if got, want := st.Spares.Adopted, []AdoptedSpare{{ID: whole}}; !slices.Equal(got, want) {
				t.Errorf("spares: the whole spare beside the over-cap one got adoptions %v, want %v", got, want)
			}
			if slices.Contains(r.listing(), format.SegmentName(over)) {
				t.Errorf("spares: the over-cap spare %s is still in the directory %v, want it unlinked", format.SegmentName(over), r.listing())
			}
			if got := r.seam.CallsFor(kit.OpWrite, format.SegmentName(over)); got != overWrites {
				t.Errorf("spares: the over-cap spare was written to, got %d writes, want %d, the count before the run: a file about to be removed is never zeroed", got, overWrites)
			}
		})
	}
}

// TestSparesPhaseSyncsItsUnlinkWhenNothingIsAdopted plants a single short
// spare, so the phase adopts nothing and removes exactly it: the removal is
// the open's own change, so the phase's one directory sync still runs,
// making the unlink durable — a short spare's name its sync never proved
// could come back after a power cut — exactly once, the same once an open
// that adopted something spends.
func TestSparesPhaseSyncsItsUnlinkWhenNothingIsAdopted(t *testing.T) {
	r := newRig(t)
	r.writeFormat()
	short := core.SegmentID(4)
	r.write(format.SegmentName(short), make([]byte, testCap/2))
	dirSyncs := r.seam.Calls(kit.OpSyncDir)

	st := r.runCandidates()
	if got, want := st.Spares.Unlinked, []core.SegmentID{short}; !slices.Equal(got, want) {
		t.Errorf("spares: the single short spare's unlinking got %v, want %v", got, want)
	}
	if len(st.Spares.Adopted) != 0 {
		t.Errorf("spares: an unlink-only open adopted %v, want nothing", st.Spares.Adopted)
	}
	if got, want := r.seam.Calls(kit.OpSyncDir), dirSyncs+1; got != want {
		t.Errorf("spares: the unlink-only open spent %d directory syncs, want exactly %d: the phase's one sync beside the fixture's %d, making the short spare's removal durable", got, want, dirSyncs)
	}
}

// TestSparesPhaseFailsTheOpenWhenAnUnlinkFails plants a short spare beside a
// whole one and fails the short one's Remove once: the size check unlinks,
// and a removal the device refused fails the open with its own error — the
// open adopted nothing and removed nothing else, both files still in the
// directory, for a phase whose unlink failed may not carry on taking spares
// over a device that did not answer.
func TestSparesPhaseFailsTheOpenWhenAnUnlinkFails(t *testing.T) {
	r := newRig(t)
	r.writeFormat()
	half, whole := core.SegmentID(4), core.SegmentID(6)
	r.write(format.SegmentName(half), make([]byte, testCap/2))
	r.write(format.SegmentName(whole), make([]byte, testCap))
	if err := r.seam.Schedule(kit.Failing(kit.OpRemove, format.SegmentName(half), 1, syscall.EIO)); err != nil {
		t.Fatalf("fixture: schedule the failed remove: %v", err)
	}

	_, err := Run(context.Background(), r.runDeps(), core.Version{}, journalID, nil)

	if !errors.Is(err, syscall.EIO) {
		t.Fatalf("spares: the failed unlink answered err %v, want %v, the removal's own error", err, syscall.EIO)
	}
	for _, id := range []core.SegmentID{half, whole} {
		if !slices.Contains(r.listing(), format.SegmentName(id)) {
			t.Errorf("spares: the failed unlink removed %s anyway; the directory holds %v", format.SegmentName(id), r.listing())
		}
	}
}

// TestSparesPhaseRefusesOnUnreadableSegmentsBeforeWriting plants a sealed
// full segment whose every read fails, beside an all-zero spare and an
// interrupted recycle — the two other shapes the phase would otherwise
// re-adopt, one with clean slots and one with dirty ones: the phase is the
// open's first writing one, and the range the attach pass collected refuses
// the open there, before any candidate is touched — so the durable bytes of
// every file are the bytes the fixture planted, the dirty slots included,
// nothing is unlinked and no spare is written to — and no phase after the
// spares one opens: the refusal is its own answer, and the whole open ends
// with it.
func TestSparesPhaseRefusesOnUnreadableSegmentsBeforeWriting(t *testing.T) {
	r := newRig(t)
	r.writeFormat()
	x, y, spare := core.SegmentID(3), core.SegmentID(9), core.SegmentID(6)
	r.plantSeg(x, sealedFull(x))
	plantInterruptedRecycle(r, 10, y)
	r.write(format.SegmentName(spare), make([]byte, testCap))
	r.failReads(format.SegmentName(x), 0)

	before := map[string][]byte{
		format.SegmentName(x):     r.fileBytes(format.SegmentName(x)),
		format.SegmentName(y):     r.fileBytes(format.SegmentName(y)),
		format.SegmentName(spare): r.fileBytes(format.SegmentName(spare)),
	}
	removes := r.seam.Calls(kit.OpRemove)
	writes := r.seam.CallsFor(kit.OpWrite, format.SegmentName(spare))

	var mu sync.Mutex
	tracer, spansOf := recordingTracer(&mu)

	_, err := Run(context.Background(), r.runDepsWithTracer(tracer), core.Version{}, journalID, nil)

	var ure *core.UnreadableRangesError
	if !errors.As(err, &ure) {
		t.Fatalf("spares: a segment whose every read failed answered err %v, want an UnreadableRangesError", err)
	}
	wantRanges := []core.SegmentRange{{Segment: x, Off: 0, Len: format.SegmentHeaderSize}}
	if !slices.Equal(ure.Ranges, wantRanges) {
		t.Errorf("spares: the refusal's ranges got %v, want %v, the header slot no read completed", ure.Ranges, wantRanges)
	}
	for name, was := range before {
		if got := r.fileBytes(name); !slices.Equal(got, was) {
			t.Errorf("spares: the refusal changed %s's durable bytes, got %x, want the bytes it held", name, got[:format.SegmentHeaderSize])
		}
	}
	for _, name := range []string{format.SegmentName(x), format.SegmentName(y), format.SegmentName(spare)} {
		if !slices.Contains(r.listing(), name) {
			t.Errorf("spares: the refusal removed %s; the directory holds %v", name, r.listing())
		}
	}
	if got := r.seam.Calls(kit.OpRemove); got != removes {
		t.Errorf("spares: the refusal unlinked something, got %d removes, want %d, the count before the run", got, removes)
	}
	if got := r.seam.CallsFor(kit.OpWrite, format.SegmentName(spare)); got != writes {
		t.Errorf("spares: the refusal wrote to the spare, got %d writes, want %d, the count before the run", got, writes)
	}
	spans := spansOf()
	if last := spans[len(spans)-1].op; last != core.SpanOpenSpares {
		t.Errorf("spares: the refusal was answered after %s, want the spares phase's own span: no phase past the first writing one opens", last)
	}
	if last := spans[len(spans)-1]; last.err != err {
		t.Errorf("spares: the spares span ended with %v, want %v, the refusal the run answered with", last.err, err)
	}
}

// TestSparesPhaseZeroesBothSlotsOfAnInterruptedRecycle runs the open over
// the interrupted recycle: the old header slot and the cap's trailer slot —
// the two places a take writes before its records — are zeroed on the
// device through one handle and one sync, without touching the dead bytes
// between that no open reads again, and the rebuilt index holds nothing
// from the new id, whose old life's records verify nowhere under it.
func TestSparesPhaseZeroesBothSlotsOfAnInterruptedRecycle(t *testing.T) {
	r := newRig(t)
	r.writeFormat()
	x, y := core.SegmentID(3), core.SegmentID(9)
	plantInterruptedRecycle(r, x, y)
	before := r.fileBytes(format.SegmentName(y))
	syncs := r.seam.CallsFor(kit.OpSync, format.SegmentName(y))

	st := r.runCandidates()

	after := r.fileBytes(format.SegmentName(y))
	if hdr := after[:format.SegmentHeaderSize]; anyHeaderByte(hdr) {
		t.Errorf("spares: the interrupted recycle's header slot zeroing got %x, want zeros", hdr)
	}
	if tail := after[testCap-format.SealMarkerSize:]; anyHeaderByte(tail) {
		t.Errorf("spares: the interrupted recycle's trailer slot zeroing got %x, want zeros", tail)
	}
	if mid := after[format.SegmentHeaderSize : testCap-format.SealMarkerSize]; !slices.Equal(mid, before[format.SegmentHeaderSize:testCap-format.SealMarkerSize]) {
		t.Errorf("spares: the phase zeroed past the two slots, got %x, want the dead bytes it held before the run", mid[:16])
	}
	if got := r.seam.CallsFor(kit.OpSync, format.SegmentName(y)); got <= syncs {
		t.Errorf("spares: the slot writes were not synced, got %d file syncs, want more than %d, the count before the run", got, syncs)
	}
	if got, want := st.Rebuilt.Index.SegmentBytes(y), (index.SegmentBytes{}); got != want {
		t.Errorf("spares: the rebuilt index holds bytes of the candidate %d, got %+v, want none: its old life verifies nowhere under the new id", y, got)
	}
	if got, want := st.Spares.Adopted, []AdoptedSpare{{ID: y}}; !slices.Equal(got, want) {
		t.Errorf("spares: the re-adopted spares got %v, want %v", got, want)
	}
}

// TestSparesPhaseLeavesCleanSlotsUnwritten plants a fresh all-zero spare and
// runs the open over it: the phase reads both slots, finds them clean and
// writes nothing — zeroing a clean slot would spend a sync on bytes already
// zeros — while the adoption still lands, and no fate names the id: a
// spare's tail is nobody's to cut.
func TestSparesPhaseLeavesCleanSlotsUnwritten(t *testing.T) {
	r := newRig(t)
	r.writeFormat()
	id := core.SegmentID(6)
	name := format.SegmentName(id)
	r.write(name, make([]byte, testCap))
	writes := r.seam.CallsFor(kit.OpWrite, name)
	syncs := r.seam.CallsFor(kit.OpSync, name)

	st := r.runCandidates()

	if got := r.seam.CallsFor(kit.OpWrite, name); got != writes {
		t.Errorf("spares: a clean spare was written to, got %d writes, want %d, the count before the run", got, writes)
	}
	if got := r.seam.CallsFor(kit.OpSync, name); got != syncs {
		t.Errorf("spares: a clean spare was synced, got %d syncs, want %d, the count before the run", got, syncs)
	}
	if got, want := st.Spares.Adopted, []AdoptedSpare{{ID: id}}; !slices.Equal(got, want) {
		t.Errorf("spares: the all-zero spare was not adopted, got %v, want %v", got, want)
	}
	for _, f := range st.Tails.Fates {
		if f.ID == id {
			t.Errorf("spares: the re-adopted spare %d kept its fate %s, want none: a spare's tail is nobody's to cut", id, f.Kind)
		}
	}
}

// TestSparesPhaseFailsTheOpenWhenTheDirectorySyncFails plants several whole
// untaken spares, so adoptions are pending, and fails the one directory sync
// the phase owes: the open fails with the sync's own error and takes no spare
// — every .seg is still in the directory and nothing is unlinked, for a spare
// taken before its name is durable could lose that name, and the records a
// later open re-appends into it, to a power cut. The refusal spent exactly
// the phase's one sync; run again with the fault spent, the open adopts every
// candidate: all of them in Spares.Adopted, nothing unlinked, nothing
// rejected.
func TestSparesPhaseFailsTheOpenWhenTheDirectorySyncFails(t *testing.T) {
	r := newRig(t)
	r.writeFormat()
	ids := []core.SegmentID{5, 6, 9}
	for _, id := range ids {
		r.write(format.SegmentName(id), make([]byte, testCap))
	}

	// The fault is scheduled after the planting: the rig's write syncs the
	// directory per file, and the fixture must not spend the failure.
	dirSyncs := r.seam.Calls(kit.OpSyncDir)
	removes := r.seam.Calls(kit.OpRemove)
	// Directory calls carry the empty key, and "*" is the pattern that
	// matches it.
	if err := r.seam.Schedule(kit.Failing(kit.OpSyncDir, "*", 1, syscall.EIO)); err != nil {
		t.Fatalf("fixture: schedule the failed sync: %v", err)
	}

	_, err := Run(context.Background(), r.runDeps(), core.Version{}, journalID, nil)

	if !errors.Is(err, syscall.EIO) {
		t.Fatalf("spares: the failed directory sync answered err %v, want %v, the sync's own error", err, syscall.EIO)
	}
	for _, id := range ids {
		if !slices.Contains(r.listing(), format.SegmentName(id)) {
			t.Errorf("spares: the failed sync removed %s; the directory holds %v", format.SegmentName(id), r.listing())
		}
	}
	if got, want := r.seam.Calls(kit.OpSyncDir), dirSyncs+1; got != want {
		t.Errorf("spares: the phase spent %d directory syncs, want exactly %d: its one sync beside the fixture's %d, so a second sync cannot mask the first's failure", got, want, dirSyncs)
	}
	if got := r.seam.Calls(kit.OpRemove); got != removes {
		t.Errorf("spares: the failed sync unlinked something, got %d removes, want %d, the count before the run", got, removes)
	}

	// The fault is spent: the run takes the spares under a passing sync.
	st := r.runCandidates()

	want := []AdoptedSpare{{ID: ids[0]}, {ID: ids[1]}, {ID: ids[2]}}
	if got := st.Spares.Adopted; !slices.Equal(got, want) {
		t.Errorf("spares: the re-run's adoptions got %v, want %v, every candidate under a passing sync", got, want)
	}
	if len(st.Spares.Unlinked) != 0 {
		t.Errorf("spares: the re-run unlinked %v, want nothing", st.Spares.Unlinked)
	}
	if st.Attached.Rejected != 0 {
		t.Errorf("spares: the re-run rejected %d segments, want none: whole untaken spares are candidates, not rejects", st.Attached.Rejected)
	}
}

// TestSparesPhaseDropsTheAdoptedSegmentsFate runs the open over the
// interrupted recycle and checks the adoption's bookkeeping alone: no fate
// names the re-adopted id — a spare's tail is nobody's to cut, and the later
// cutting phases find none for it — and nothing is reported as damage, a
// rejection or an unrecognised name: an unprovoked orphan or damage report
// over a spare's adoption is what this fails.
func TestSparesPhaseDropsTheAdoptedSegmentsFate(t *testing.T) {
	r := newRig(t)
	r.writeFormat()
	y := core.SegmentID(9)
	plantInterruptedRecycle(r, core.SegmentID(3), y)

	st := r.runCandidates()

	for _, f := range st.Tails.Fates {
		if f.ID == y {
			t.Errorf("spares: the re-adopted spare %d kept its fate %s, want none: a spare's tail is nobody's to cut", y, f.Kind)
		}
	}
	if len(st.Tails.Damaged) != 0 {
		t.Errorf("spares: the re-adopted spare was reported damaged, got %v, want none", st.Tails.Damaged)
	}
	if st.Attached.Rejected != 0 || len(st.Attached.Unrecognised) != 0 {
		t.Errorf("spares: the adoption reported a rejected or an unrecognised segment, got rejected %d, unrecognised %v, want none",
			st.Attached.Rejected, st.Attached.Unrecognised)
	}
}
