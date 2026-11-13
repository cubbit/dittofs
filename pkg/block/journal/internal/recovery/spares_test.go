package recovery

import (
	"context"
	"slices"
	"testing"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/format"
	"github.com/cubbit/dittofs/pkg/block/journal/journaltest/raw"
)

// testCap is the segment cap every fixture plants at: a spare is a whole
// cap-sized file, and every segment file here is exactly that long.
const testCap = 8 << 10

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

	b := raw.SegmentHeader{Identity: journalID, ID: uint64(x)}.Bytes()
	rec := rec(1, func(rr *raw.Record) {
		rr.Write = &raw.WriteBody{MTime: 1, Payload: bytes16()}
	})
	rec.Identity = journalID
	rec.Segment = uint64(x)
	rec.RecordOffset = uint32(len(b))
	rec.SyncedThrough = uint64(format.SegmentHeaderSize + int64(len(rec.Bytes()))) // the sync covered the record whole
	b = append(b, rec.Bytes()...)
	markerAt := int64(len(b))
	b = append(b, raw.SealMarker{Identity: journalID, Segment: uint64(x), Offset: uint64(markerAt)}.Bytes()...)
	if int64(len(b)) > testCap-raw.TrailerSize {
		t.Fatalf("fixture: header, record and marker take %d bytes, above the cap %d", len(b), testCap)
	}
	b = append(b, make([]byte, testCap-raw.TrailerSize-int64(len(b)))...)
	b = append(b, raw.Trailer(1, uint64(markerAt+format.SealMarkerSize))...)
	r.plantSeg(x, b)

	// The interrupted recycle's one durable step: the rename, and the
	// directory sync that made it survive the crash.
	if err := r.seam.Rename(format.SegmentName(x), format.SegmentName(y)); err != nil {
		t.Fatalf("fixture: rename %s to %s: %v", format.SegmentName(x), format.SegmentName(y), err)
	}
	if err := r.seam.SyncDir(); err != nil {
		t.Fatalf("fixture: sync dir: %v", err)
	}

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
