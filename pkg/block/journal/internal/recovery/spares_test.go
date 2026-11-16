package recovery

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/format"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/index"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/segment"
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

// markerRottedSealed returns segment id's sealed bytes with one synced record
// and its seal marker rotted: the interrupted shape whose marker is the one
// thing wrong with its end — the predecessor a held take's open reads as
// end-unproven, its whole life verifiable past the marker.
func markerRottedSealed(id core.SegmentID) []byte {
	seg, ents := buildSealed(id, tailRec(200, 0x4100))
	markerOff := len(seg) - raw.TrailerSize - raw.CatalogEntrySize*len(ents) - format.SealMarkerSize
	seg[markerOff+31] ^= 0xff
	return seg
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
// segments of the classes an open under this journal's identity may re-adopt,
// with no sync mark naming any of them.
func candidates(st State) []core.SegmentID {
	// The held takes are the state's own answer, Spares.Held; the row here
	// reads the candidate list alone, its proofs the state's own.
	cands, _ := spareCandidates(st.Attached, st.Rebuilt.Scans, successorSet(st.Attached.Own), nil, sealProvenOf(st))
	return cands
}

// sealProvenOf mirrors the phase's own answer over a state: each id the
// classing gave the sealed fate — the fate a named predecessor takes whether
// its marker verified on the scan or its trailer named a footer just past the
// rotted marker.
func sealProvenOf(st State) map[core.SegmentID]bool {
	proven := make(map[core.SegmentID]bool, len(st.Tails.Fates))
	for _, f := range st.Tails.Fates {
		proven[f.ID] = f.Kind == fateSealed
	}
	return proven
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
// again — an untaken spare, its zero-header slot proving it. The open pays
// nothing for the re-adoption: no file is created and no byte is allocated,
// for the spare the crash left whole is the pool's already.
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
	// The headroom and the repack reserve stay whole with no new allocation:
	// what the run spent making files.
	made := r.seam.Calls(kit.OpCreate) + r.seam.Calls(kit.OpAllocate)
	st := r.runCandidates()

	if want := []core.SegmentID{id}; !slices.Equal(st.Attached.ZeroHeader, want) {
		t.Errorf("attach: the take the crash undid was classed ZeroHeader %v, want %v", st.Attached.ZeroHeader, want)
	}
	if got, want := candidates(st), []core.SegmentID{id}; !slices.Equal(got, want) {
		t.Errorf("spareCandidates: a take the crash never made durable got candidates %v, want %v", got, want)
	}
	if got := r.seam.Calls(kit.OpCreate) + r.seam.Calls(kit.OpAllocate); got != made {
		t.Errorf("spares: the re-open made something, got %d creates and allocations, want %d, the count before the run: the spare the crash left whole is the pool's already", got, made)
	}
}

// TestSpareCandidatesNeverHasAForeignSegment plants a record-less segment
// whose header verifies naming another journal: no candidate test may reach
// it, for the identity is checked first — a foreign segment is never
// re-adopted and never zeroed, whatever its records hold, because adopting it
// could destroy the only copy of another journal's content. The row asks
// spareCandidates directly with a hand-made record-less scan for it, so the
// identity's exclusion is the only thing keeping it out: the rebuild scans
// no foreign segment, so through the open alone its missing scan would drop
// it before the identity answered. Its bytes are unchanged and Rejected
// counts it.
func TestSpareCandidatesNeverHasAForeignSegment(t *testing.T) {
	other := core.JournalID{'a', 'n', 'o', 't', 'h', 'e', 'r'}
	r := newRig(t)
	r.writeFormat()
	id := core.SegmentID(5)
	b := make([]byte, testCap)
	copy(b, raw.SegmentHeader{Identity: other, ID: uint64(id)}.Bytes())
	r.write(format.SegmentName(id), b)

	st := r.runCandidates()

	// The record-less scan a foreign segment would show, had the rebuild
	// scanned it: with it the identity check is the only thing excluding
	// the id.
	scans := append(slices.Clone(st.Rebuilt.Scans), segmentScan{ID: id, Size: testCap, RecordsEnd: format.SegmentHeaderSize})
	if cands, _ := spareCandidates(st.Attached, scans, nil, nil, nil); slices.Contains(cands, id) {
		t.Errorf("spareCandidates: the foreign segment %d with a record-less scan was a candidate, got %v: the identity is checked before the record-less test", id, cands)
	}
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

// TestSpareCandidatesNeverHasASyncedCloneTargetWithAFailedBody plants an own
// segment, named by no successor, whose only record is a clone target its own
// sync proof covers — so the tail's classing judges it, corrupt — and whose
// body failed while its header did verify. A record header verifies in the
// segment, so it is no spare whatever the body did: re-adopting it would zero
// the header evidence of a synced record. It is not a candidate and Run
// leaves its bytes exactly as planted, on disk and reported as the classing's
// corrupt drop.
func TestSpareCandidatesNeverHasASyncedCloneTargetWithAFailedBody(t *testing.T) {
	r := newRig(t)
	r.writeFormat()
	id := core.SegmentID(4)
	seg, _ := appendClone(raw.SegmentHeader{Identity: journalID, ID: uint64(id)}.Bytes(), id,
		func(at int64) int64 { return at + format.RecordHeaderSize })
	seg = append(seg, make([]byte, testCap-int64(len(seg)))...)
	r.plantSeg(id, seg)

	st := r.runCandidates()

	if got := candidates(st); slices.Contains(got, id) {
		t.Errorf("spareCandidates: segment %d, whose one record's header verified, was a candidate, got %v, want it absent: no record header may verify anywhere in a spare", id, got)
	}
	if len(st.Spares.Adopted) != 0 || len(st.Spares.Unlinked) != 0 {
		t.Errorf("spares: the segment whose record header verified got adopted %v and unlinked %v, want neither", st.Spares.Adopted, st.Spares.Unlinked)
	}
	if after := r.fileBytes(format.SegmentName(id)); !slices.Equal(after, seg) {
		t.Errorf("spares: the segment whose record header verified was touched, got %x, want the bytes it arrived with", after[:format.SegmentHeaderSize])
	}
	if len(st.Tails.Corrupt) != 1 || st.Tails.Corrupt[0].Segment != id {
		t.Errorf("spares: the clone target was not classed the corrupt drop it is, got %+v, want exactly one in segment %d", st.Tails.Corrupt, id)
	}
}

// TestSparesPhaseWithholdsACandidateASyncMarkNames plants two record-less
// whole spares and hands Run a sync mark naming one: a mark is the caller's
// durable record that its stream synced through the segment it names, so the
// marked candidate is never re-adopted, zeroed or unlinked — its bytes are
// evidence the marks phases still read — while its unmarked neighbour beside
// it is adopted.
func TestSparesPhaseWithholdsACandidateASyncMarkNames(t *testing.T) {
	r := newRig(t)
	r.writeFormat()
	marked, plain := core.SegmentID(5), core.SegmentID(7)
	markName := format.SegmentName(marked)
	r.write(markName, make([]byte, testCap))
	r.write(format.SegmentName(plain), make([]byte, testCap))
	writes := r.seam.CallsFor(kit.OpWrite, markName)
	removes := r.seam.Calls(kit.OpRemove)

	st, err := Run(context.Background(), r.runDeps(), core.Version{}, journalID,
		[]core.SyncMark{{Stream: 1, Segment: uint64(marked), Through: testCap, Offset: testCap, Sequence: 1}})
	if err != nil {
		t.Fatalf("spares: the open under a sync mark: %v", err)
	}

	if got, want := st.Spares.Adopted, []AdoptedSpare{{ID: plain, Class: segment.Content}}; !slices.Equal(got, want) {
		t.Errorf("spares: under a mark naming %d the adoptions got %v, want %v: a candidate a mark names is withheld", marked, got, want)
	}
	if len(st.Spares.Unlinked) != 0 {
		t.Errorf("spares: the candidate the mark names got unlinked %v, want nothing: its bytes stay for the marks phases", st.Spares.Unlinked)
	}
	if !slices.Contains(r.listing(), markName) {
		t.Errorf("spares: the marked candidate %s is gone from the directory %v, want it kept", markName, r.listing())
	}
	if got, want := r.seam.CallsFor(kit.OpWrite, markName), writes; got != want {
		t.Errorf("spares: the marked candidate was written to, got %d writes, want %d, the count before the run", got, want)
	}
	if got, want := r.seam.Calls(kit.OpRemove), removes; got != want {
		t.Errorf("spares: the marked candidate was removed, got %d removes, want %d, the count before the run", got, want)
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
// adopted. The unlinks are recorded ascending by id, and no fate is left
// naming an unlinked id: the later cutting phases would truncate a file that
// no longer exists.
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
	if got, want := st.Spares.Adopted, []AdoptedSpare{{ID: whole, Class: segment.Content}}; !slices.Equal(got, want) {
		t.Errorf("spares: the whole spare beside the short ones got adoptions %v, want %v", got, want)
	}
	for _, id := range []core.SegmentID{empty, half} {
		for _, f := range st.Tails.Fates {
			if f.ID == id {
				t.Errorf("spares: the unlinked spare %d kept its fate %s, want none: the cutting phases would truncate a file that no longer exists", id, f.Kind)
			}
		}
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

// TestSparesPhaseRefusesADepsWithoutSegmentCap plants one empty record-less
// .seg — the file whose size 0 equals the zero cap — and runs the open under
// a zero SegmentCap, the Deps literal that forgot the cap: no cap right-sizes
// any spare, so the phase refuses the open naming the unset cap rather than
// unlinking each candidate under it, and the directory keeps every file the
// body planted.
func TestSparesPhaseRefusesADepsWithoutSegmentCap(t *testing.T) {
	r := newRig(t)
	r.writeFormat()
	empty := core.SegmentID(4)
	r.write(format.SegmentName(empty), nil)
	dirSyncs := r.seam.Calls(kit.OpSyncDir)
	removes := r.seam.Calls(kit.OpRemove)

	deps := r.runDeps()
	deps.SegmentCap = 0
	_, err := Run(context.Background(), deps, core.Version{}, journalID, nil)

	if err == nil {
		t.Fatalf("spares: the open under a zero SegmentCap succeeded, want the refusal naming the unset cap")
	}
	if !strings.Contains(err.Error(), "SegmentCap") {
		t.Errorf("spares: the zero-cap refusal reads %q, want it to name the unset SegmentCap", err.Error())
	}
	if !slices.Contains(r.listing(), format.SegmentName(empty)) {
		t.Errorf("spares: the refused zero-cap open unlinked %s; the directory holds %v", format.SegmentName(empty), r.listing())
	}
	if got, want := r.seam.Calls(kit.OpRemove), removes; got != want {
		t.Errorf("spares: the refused zero-cap open removed something, got %d removes, want %d, the count before the run", got, want)
	}
	if got, want := r.seam.Calls(kit.OpSyncDir), dirSyncs; got != want {
		t.Errorf("spares: the refused zero-cap open synced the directory, got %d syncs, want %d, the count before the run", got, want)
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
			if got, want := st.Spares.Adopted, []AdoptedSpare{{ID: whole, Class: segment.Content}}; !slices.Equal(got, want) {
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

// TestSparesPhaseUnlinksWithNoAdoptionAndNoDirectorySync plants a single
// short spare, so the phase adopts nothing and removes exactly it: the
// phase's one directory sync proves the names of the spares to be taken, and
// a removal is not among what it proves — the sync runs only where a spare is
// staged to be taken, so the unlink-only open spends none, and the removal is
// left unproved by design: a power cut may resurrect the short spare, which
// the next open re-reads and unlinks again.
func TestSparesPhaseUnlinksWithNoAdoptionAndNoDirectorySync(t *testing.T) {
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
	if got, want := r.seam.Calls(kit.OpSyncDir), dirSyncs; got != want {
		t.Errorf("spares: the unlink-only open spent %d directory syncs, want exactly %d, the count before the run: the one sync proves only the names of spares to be taken, never a removal", got, want)
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
	if got, want := st.Spares.Adopted, []AdoptedSpare{{ID: y, Class: segment.Content}}; !slices.Equal(got, want) {
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
	if got, want := st.Spares.Adopted, []AdoptedSpare{{ID: id, Class: segment.Content}}; !slices.Equal(got, want) {
		t.Errorf("spares: the all-zero spare was not adopted, got %v, want %v", got, want)
	}
	for _, f := range st.Tails.Fates {
		if f.ID == id {
			t.Errorf("spares: the re-adopted spare %d kept its fate %s, want none: a spare's tail is nobody's to cut", id, f.Kind)
		}
	}
}

// TestSparesPhaseFailsTheOpenWhenTheDirectorySyncFails plants several whole
// untaken spares beside one short one, so adoptions and an unlink are
// pending, and fails the one directory sync the phase owes: the open fails
// with the sync's own error and takes no spare — every .seg is still in the
// directory, the short one included, and nothing is unlinked, for the
// removals are staged and run only past the sync, and a spare taken before
// its name is durable could lose that name, and the records a later open
// re-appends into it, to a power cut. The refusal spent exactly the phase's
// one sync; run again with the fault spent, the open adopts every whole
// candidate and unlinks the short one.
func TestSparesPhaseFailsTheOpenWhenTheDirectorySyncFails(t *testing.T) {
	r := newRig(t)
	r.writeFormat()
	short := core.SegmentID(4)
	r.write(format.SegmentName(short), make([]byte, testCap/2))
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
	for _, id := range slices.Concat(ids, []core.SegmentID{short}) {
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

	want := []AdoptedSpare{{ID: ids[0], Class: segment.Content}, {ID: ids[1], Class: segment.Content}, {ID: ids[2], Class: segment.Content}}
	if got := st.Spares.Adopted; !slices.Equal(got, want) {
		t.Errorf("spares: the re-run's adoptions got %v, want %v, every whole candidate under a passing sync", got, want)
	}
	if got, want := st.Spares.Unlinked, []core.SegmentID{short}; !slices.Equal(got, want) {
		t.Errorf("spares: the re-run's unlinking got %v, want %v, the short candidate under a passing sync", got, want)
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

// TestSparesPhaseClassesAdoptedSparesIntoTheThreeClasses plants whole
// record-less spares and runs the open under the pool's own targets — the
// content class keeping two spares, the headroom one, the repack reserve one
// — with a segment its successor names standing among them, never a
// candidate: the adoptions ascend by id and class into the targets' fill
// order, the headroom first, then the reserve, then the content class, and
// every spare past every target still takes the content class — adopted, not
// orphaned, never unlinked.
func TestSparesPhaseClassesAdoptedSparesIntoTheThreeClasses(t *testing.T) {
	for _, row := range []struct {
		name  string
		ids   []core.SegmentID
		named bool
	}{
		{"four candidates fill the pool's three classes", []core.SegmentID{5, 7, 9, 11}, true},
		{"extras past every target take the content class", []core.SegmentID{20, 21, 22, 23, 24, 25, 26}, false},
	} {
		t.Run(row.name, func(t *testing.T) {
			r := newRig(t)
			r.writeFormat()
			if row.named {
				// Z: record-less, its trailer naming a footer just past the
				// marker slot at the first record slot's end, so the open
				// reads its missing marker as the marker alone rotted.
				z, w := core.SegmentID(2), core.SegmentID(12)
				b := raw.SegmentHeader{Identity: journalID, ID: uint64(z)}.Bytes()
				b = append(b, make([]byte, testCap-raw.TrailerSize-int64(len(b)))...)
				b = append(b, raw.Trailer(1, uint64(format.SegmentHeaderSize+format.SealMarkerSize))...)
				r.plantSeg(z, b)

				// W: the successor whose header names Z, holding one
				// verifying record so it is no spare itself.
				s := raw.SegmentHeader{Identity: journalID, ID: uint64(w), Predecessor: uint64(z)}.Bytes()
				rr := rec(1, func(r *raw.Record) {
					r.Write = &raw.WriteBody{MTime: 1, Payload: bytes16()}
				})
				rr.Identity = journalID
				rr.Segment = uint64(w)
				rr.RecordOffset = uint32(len(s))
				s = append(s, rr.Bytes()...)
				s = append(s, make([]byte, testCap-int64(len(s)))...)
				r.plantSeg(w, s)
			}
			for _, id := range row.ids {
				r.write(format.SegmentName(id), make([]byte, testCap))
			}

			deps := r.runDeps()
			deps.SpareTargets = segment.Targets{Content: 2, Headroom: 1, Reserve: 1}
			st, err := Run(context.Background(), deps, core.Version{}, journalID, nil)
			if err != nil {
				t.Fatalf("spares: the classing open: %v", err)
			}

			// The fill order: the lowest id fills the headroom, the next the
			// reserve, and every one after takes the content class — the
			// content target's two and every extra beside.
			want := make([]AdoptedSpare, len(row.ids))
			if len(row.ids) > 0 {
				want[0] = AdoptedSpare{ID: row.ids[0], Class: segment.Headroom}
			}
			if len(row.ids) > 1 {
				want[1] = AdoptedSpare{ID: row.ids[1], Class: segment.Reserve}
			}
			for i := 2; i < len(row.ids); i++ {
				want[i] = AdoptedSpare{ID: row.ids[i], Class: segment.Content}
			}
			if got := st.Spares.Adopted; !slices.Equal(got, want) {
				t.Errorf("spares: the adoptions' classes got %v, want %v, the fill order headroom, reserve, content over ids ascending", got, want)
			}
			if len(st.Spares.Unlinked) != 0 {
				t.Errorf("spares: the classing open unlinked %v, want nothing: no spare is ever orphaned", st.Spares.Unlinked)
			}
			for _, id := range row.ids {
				if !slices.Contains(r.listing(), format.SegmentName(id)) {
					t.Errorf("spares: the adopted spare %s is gone from the directory %v, want it kept", format.SegmentName(id), r.listing())
				}
			}
		})
	}
}

// TestSparesPhaseFailsTheOpenWhenTheSlotSyncFails plants the interrupted
// recycle and fails the one file sync its zeroing alone owes. The phase's
// directory sync runs first — the candidate is staged to be taken, so its
// name is proved — and the zeroing starts past it: the slot sync's own error
// answers the open, no adoption is recorded — the failed sync rolled the two
// slot writes back, so the candidate's durable bytes are the bytes it arrived
// with — and the removals never run. The spent directory sync is what the
// refusal leaves behind: it proved a name whose file never became a spare,
// and the next open re-reads the candidate and re-stages it the same way.
func TestSparesPhaseFailsTheOpenWhenTheSlotSyncFails(t *testing.T) {
	r := newRig(t)
	r.writeFormat()
	x, y := core.SegmentID(3), core.SegmentID(9)
	plantInterruptedRecycle(r, x, y)
	before := r.fileBytes(format.SegmentName(y))
	dirSyncs := r.seam.Calls(kit.OpSyncDir)
	removes := r.seam.Calls(kit.OpRemove)
	if err := r.seam.Schedule(kit.Failing(kit.OpSync, format.SegmentName(y), 1, syscall.EIO)); err != nil {
		t.Fatalf("fixture: schedule the failed file sync: %v", err)
	}

	st, err := Run(context.Background(), r.runDeps(), core.Version{}, journalID, nil)

	if !errors.Is(err, syscall.EIO) {
		t.Fatalf("spares: the failed slot sync answered err %v, want %v, the sync's own error", err, syscall.EIO)
	}
	if len(st.Spares.Adopted) != 0 || len(st.Spares.Unlinked) != 0 {
		t.Errorf("spares: the open over a failed slot sync got adopted %v and unlinked %v, want neither", st.Spares.Adopted, st.Spares.Unlinked)
	}
	if after := r.fileBytes(format.SegmentName(y)); !slices.Equal(after, before) {
		t.Errorf("spares: the failed sync's slot writes became durable, got %x..., want the bytes the file arrived with", after[:format.SegmentHeaderSize])
	}
	if got, want := r.seam.Calls(kit.OpSyncDir), dirSyncs+1; got != want {
		t.Errorf("spares: the open over a failed slot sync spent %d directory syncs, want exactly %d: the one before the zeroing, for the candidate staged to be taken", got, want)
	}
	if got := r.seam.Calls(kit.OpRemove); got != removes {
		t.Errorf("spares: the failed slot sync unlinked something, got %d removes, want %d, the count before the run", got, removes)
	}
	if !slices.Contains(r.listing(), format.SegmentName(y)) {
		t.Errorf("spares: the failed slot sync removed %s; the directory holds %v", format.SegmentName(y), r.listing())
	}
}

// TestSparesPhaseRefusesWhenItsOwnSlotReadNeverCompletes plants the
// interrupted recycle and makes the three attempts of the phase's own
// header-slot read fail — every read of the file before it, its header at
// attach and the rebuild's three, answered — so the bound refuses the open
// on the range it collected rather than zeroing a slot no read returned:
// the error is the unreadable-ranges refusal naming the candidate, the
// file's durable bytes are the bytes it arrived with, nothing is written,
// adopted or unlinked, and the phase's directory sync — already spent for
// the candidate staged to be taken ahead of its zeroing — is all the
// refusal leaves behind.
func TestSparesPhaseRefusesWhenItsOwnSlotReadNeverCompletes(t *testing.T) {
	r := newRig(t)
	r.writeFormat()
	x, y := core.SegmentID(3), core.SegmentID(9)
	plantInterruptedRecycle(r, x, y)
	before := r.fileBytes(format.SegmentName(y))
	dirSyncs := r.seam.Calls(kit.OpSyncDir)
	// The fourth read of the file the open makes is the last before the
	// phase; its header-slot read is the fifth, and the bound pays three
	// attempts on the ones that fail.
	if err := r.seam.Schedule(kit.FailingFrom(kit.OpRead, format.SegmentName(y), 5, 3, syscall.EIO)); err != nil {
		t.Fatalf("fixture: schedule the refused slot read: %v", err)
	}
	writesBefore := r.seam.CallsFor(kit.OpWrite, format.SegmentName(y))

	_, err := Run(context.Background(), r.runDeps(), core.Version{}, journalID, nil)

	var ure *core.UnreadableRangesError
	if !errors.As(err, &ure) {
		t.Fatalf("spares: the slot read no attempt completed answered err %v, want an UnreadableRangesError", err)
	}
	if len(ure.Ranges) != 1 || ure.Ranges[0].Segment != y || ure.Ranges[0].Off != 0 || ure.Ranges[0].Len != format.SegmentHeaderSize {
		t.Errorf("spares: the refusal's ranges got %v, want exactly the candidate %d's header slot [0,%d)", ure.Ranges, y, format.SegmentHeaderSize)
	}
	if after := r.fileBytes(format.SegmentName(y)); !slices.Equal(after, before) {
		t.Errorf("spares: the refused slot read changed the candidate's bytes, got %x..., want the bytes the file arrived with", after[:format.SegmentHeaderSize])
	}
	if got, want := r.seam.CallsFor(kit.OpWrite, format.SegmentName(y)), writesBefore; got != want {
		t.Errorf("spares: the refused slot read preceded %d writes, want %d, the count before the run: no slot is written over an unread range", got, want)
	}
	if got, want := r.seam.Calls(kit.OpSyncDir), dirSyncs+1; got != want {
		t.Errorf("spares: the refused slot read answered before the directory sync, got %d syncs, want exactly %d, the one before the zeroing for the candidate staged to be taken", got, want)
	}
	if !slices.Contains(r.listing(), format.SegmentName(y)) {
		t.Errorf("spares: the refused slot read removed %s; the directory holds %v", format.SegmentName(y), r.listing())
	}
}

// TestSparesPhaseAFailingDirectorySyncChangesNoCandidatesBytes plants the
// interrupted recycle — the candidate whose dirty slots the phase would
// zero — and fails the one directory sync its taking owes: the sync runs
// ahead of the zeroing, so its failure answers the open before any slot is
// written, and the candidate's durable bytes are the bytes it arrived with,
// the dirty header and trailer slots included — nothing is adopted, nothing
// is unlinked, and the file keeps its name over the sync that proved none.
func TestSparesPhaseAFailingDirectorySyncChangesNoCandidatesBytes(t *testing.T) {
	r := newRig(t)
	r.writeFormat()
	x, y := core.SegmentID(3), core.SegmentID(9)
	plantInterruptedRecycle(r, x, y)
	before := r.fileBytes(format.SegmentName(y))
	writes := r.seam.CallsFor(kit.OpWrite, format.SegmentName(y))
	removes := r.seam.Calls(kit.OpRemove)
	dirSyncs := r.seam.Calls(kit.OpSyncDir)

	if err := r.seam.Schedule(kit.Failing(kit.OpSyncDir, "*", 1, syscall.EIO)); err != nil {
		t.Fatalf("fixture: schedule the failed sync: %v", err)
	}
	_, err := Run(context.Background(), r.runDeps(), core.Version{}, journalID, nil)

	if !errors.Is(err, syscall.EIO) {
		t.Fatalf("spares: the failed directory sync answered err %v, want %v, the sync's own error", err, syscall.EIO)
	}
	if after := r.fileBytes(format.SegmentName(y)); !slices.Equal(after, before) {
		t.Errorf("spares: the failed directory sync left the candidate's bytes changed, got %x..., want the dirty bytes the file arrived with: the sync runs ahead of the zeroing", after[:format.SegmentHeaderSize])
	}
	if got := r.seam.CallsFor(kit.OpWrite, format.SegmentName(y)); got != writes {
		t.Errorf("spares: the failed directory sync wrote to the candidate, got %d writes, want %d, the count before the run", got, writes)
	}
	if got := r.seam.Calls(kit.OpRemove); got != removes {
		t.Errorf("spares: the failed directory sync unlinked something, got %d removes, want %d, the count before the run", got, removes)
	}
	if got, want := r.seam.Calls(kit.OpSyncDir), dirSyncs+1; got != want {
		t.Errorf("spares: the failed directory sync spent %d directory syncs, want exactly %d: its one refusal, so a second sync cannot mask the first's failure", got, want)
	}
	if !slices.Contains(r.listing(), format.SegmentName(y)) {
		t.Errorf("spares: the failed directory sync removed %s; the directory holds %v", format.SegmentName(y), r.listing())
	}
}

// TestSparesPhaseACancelledOpenUnlinksNothing plants a short record-less
// spare alone — the candidate whose fate the phase writes nothing to decide,
// so no read of the run would carry the cancellation — and cancels the open
// under the spares phase's own span, whose context check runs before any
// sync, zeroing or removal: the cancelled open answers the context's error
// there, the short spare keeps its name, and the phase spends no directory
// sync and no removal over a cancellation.
func TestSparesPhaseACancelledOpenUnlinksNothing(t *testing.T) {
	r := newRig(t)
	r.writeFormat()
	short := core.SegmentID(4)
	r.write(format.SegmentName(short), make([]byte, testCap/2))
	removes := r.seam.Calls(kit.OpRemove)
	dirSyncs := r.seam.Calls(kit.OpSyncDir)

	ctx, cancel := context.WithCancel(context.Background())
	tracer := func(tctx context.Context, op core.SpanOp, _ ...slog.Attr) (context.Context, func(error)) {
		if op == core.SpanOpenSpares {
			cancel()
		}
		return tctx, func(error) {}
	}
	_, err := Run(ctx, r.runDepsWithTracer(tracer), core.Version{}, journalID, nil)

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("spares: the open cancelled under the phase's own span answered err %v, want context.Canceled at the phase's own check", err)
	}
	if !slices.Contains(r.listing(), format.SegmentName(short)) {
		t.Errorf("spares: the cancelled open unlinked the short spare %s; the directory holds %v, want it kept: the phase checks its context before the removals", format.SegmentName(short), r.listing())
	}
	if got := r.seam.Calls(kit.OpRemove); got != removes {
		t.Errorf("spares: the cancelled open removed something, got %d removes, want %d, the count before the run", got, removes)
	}
	if got := r.seam.Calls(kit.OpSyncDir); got != dirSyncs {
		t.Errorf("spares: the cancelled open synced the directory, got %d syncs, want %d, the count before the run", got, dirSyncs)
	}
}

// TestSparesPhaseFailsTheOpenWhenTheAllocationQueryFails plants the
// interrupted recycle and fails the allocation query of its file once: the
// query is asked in the phase's write-free staging, so the open fails with
// the query's own error, nothing is adopted or unlinked, the phase's
// directory sync is never spent, and the candidate's durable bytes are the
// dirty bytes it arrived with — taking nothing is the only answer over a
// device that would not say whether the file is whole.
func TestSparesPhaseFailsTheOpenWhenTheAllocationQueryFails(t *testing.T) {
	r := newRig(t)
	r.writeFormat()
	x, y := core.SegmentID(3), core.SegmentID(9)
	plantInterruptedRecycle(r, x, y)
	before := r.fileBytes(format.SegmentName(y))
	removes := r.seam.Calls(kit.OpRemove)
	dirSyncs := r.seam.Calls(kit.OpSyncDir)

	if err := r.seam.Schedule(kit.Failing(kit.OpAllocated, format.SegmentName(y), 1, syscall.EIO)); err != nil {
		t.Fatalf("fixture: schedule the failed query: %v", err)
	}
	st, err := Run(context.Background(), r.runDeps(), core.Version{}, journalID, nil)

	if !errors.Is(err, syscall.EIO) {
		t.Fatalf("spares: the failed allocation query answered err %v, want %v, the query's own error", err, syscall.EIO)
	}
	if len(st.Spares.Adopted) != 0 || len(st.Spares.Unlinked) != 0 {
		t.Errorf("spares: the open over a failed query got adopted %v and unlinked %v, want neither", st.Spares.Adopted, st.Spares.Unlinked)
	}
	if after := r.fileBytes(format.SegmentName(y)); !slices.Equal(after, before) {
		t.Errorf("spares: the failed query changed the candidate's bytes, got %x..., want the dirty bytes the file arrived with", after[:format.SegmentHeaderSize])
	}
	if got := r.seam.Calls(kit.OpRemove); got != removes {
		t.Errorf("spares: the failed query unlinked something, got %d removes, want %d, the count before the run", got, removes)
	}
	if got := r.seam.Calls(kit.OpSyncDir); got != dirSyncs {
		t.Errorf("spares: the failed query synced the directory, got %d syncs, want %d, the count before the run: the staging refuses before the sync", got, dirSyncs)
	}
	if !slices.Contains(r.listing(), format.SegmentName(y)) {
		t.Errorf("spares: the failed query removed %s; the directory holds %v", format.SegmentName(y), r.listing())
	}
}

// TestSparesPhaseUnlinksACandidateTheQueryAnswersNotWhole plants a whole
// record-less spare whose file the seam answers not whole — the shape of a
// cap-sized file whose middle a punch holed, whatever its bytes hold — and
// runs the open: the candidate is unlinked and made again rather than
// adopted, recorded alone, and the phase spends no directory sync, for
// nothing was staged to be taken.
func TestSparesPhaseUnlinksACandidateTheQueryAnswersNotWhole(t *testing.T) {
	r := newRig(t)
	r.writeFormat()
	holed, whole := core.SegmentID(4), core.SegmentID(7)
	r.write(format.SegmentName(holed), make([]byte, testCap))
	r.write(format.SegmentName(whole), make([]byte, testCap))
	holedWrites := r.seam.CallsFor(kit.OpWrite, format.SegmentName(holed))
	dirSyncs := r.seam.Calls(kit.OpSyncDir)

	if err := r.seam.Schedule(kit.NotWhole(format.SegmentName(holed), 1)); err != nil {
		t.Fatalf("fixture: schedule the not-whole answer: %v", err)
	}
	st := r.runCandidates()

	if got, want := st.Spares.Unlinked, []core.SegmentID{holed}; !slices.Equal(got, want) {
		t.Errorf("spares: the not-whole candidate's unlinking got %v, want %v: the query, not the bytes or the size alone, is what finds it", got, want)
	}
	if got, want := st.Spares.Adopted, []AdoptedSpare{{ID: whole, Class: segment.Content}}; !slices.Equal(got, want) {
		t.Errorf("spares: the whole spare beside it got adoptions %v, want %v", got, want)
	}
	if slices.Contains(r.listing(), format.SegmentName(holed)) {
		t.Errorf("spares: the not-whole spare %s is still in the directory %v, want it unlinked", format.SegmentName(holed), r.listing())
	}
	if got := r.seam.CallsFor(kit.OpWrite, format.SegmentName(holed)); got != holedWrites {
		t.Errorf("spares: the not-whole spare was written to, got %d writes, want %d, the count before the run: no byte of a file about to be removed is written", got, holedWrites)
	}
	if got := r.seam.Calls(kit.OpSyncDir); got != dirSyncs+1 {
		t.Errorf("spares: the open spent %d directory syncs, want exactly %d beside the fixture's %d: the one for the spare staged beside the not-whole one", got, dirSyncs+1, dirSyncs)
	}
}

// TestSparesPhaseACancelledPastItsStagingWithNoTakeUnlinksNothing plants a
// single cap-sized record-less candidate whose allocation query the seam
// answers not whole, and cancels the open while that query is held: the
// staging then reaches its end having staged nothing to take — the phase's
// context check past it is the last thing standing before the unlink loop,
// and a cancellation delivered under the query is what it answers. The
// cancelled open takes the candidate's un-unlinked presence and answers the
// context's error: nothing zeroed, nothing synced, nothing unlinked — the
// shape a phase whose past-staging check guarded only its adoptions would
// unlink, running the removals of a cancelled open.
func TestSparesPhaseACancelledPastItsStagingWithNoTakeUnlinksNothing(t *testing.T) {
	r := newRig(t)
	r.writeFormat()
	holed := core.SegmentID(6)
	r.write(format.SegmentName(holed), make([]byte, testCap))
	removes := r.seam.Calls(kit.OpRemove)
	dirSyncs := r.seam.Calls(kit.OpSyncDir)
	if err := r.seam.Schedule(kit.NotWhole(format.SegmentName(holed), 1)); err != nil {
		t.Fatalf("fixture: schedule the not-whole answer: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	held := r.seam.HoldFirst(t, kit.OpAllocated, format.SegmentName(holed))
	done := make(chan error, 1)
	go func() {
		_, err := Run(ctx, r.runDeps(), core.Version{}, journalID, nil)
		done <- err
	}()

	<-held.Reached()
	cancel()
	held.Release()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("spares: the open cancelled past the phase's staging, staging nothing, answered err %v, want context.Canceled before the removals", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("spares: the cancelled open never answered; the open is parked somewhere")
	}
	if !slices.Contains(r.listing(), format.SegmentName(holed)) {
		t.Errorf("spares: the cancelled open unlinked the not-whole candidate %s; the directory holds %v, want it kept: the phase's context check guards the removals too", format.SegmentName(holed), r.listing())
	}
	if got := r.seam.Calls(kit.OpRemove); got != removes {
		t.Errorf("spares: the cancelled open removed something, got %d removes, want %d, the count before the run", got, removes)
	}
	if got := r.seam.Calls(kit.OpSyncDir); got != dirSyncs {
		t.Errorf("spares: the cancelled open synced the directory, got %d syncs, want %d, the count before the run", got, dirSyncs)
	}
}

// cancelledUnder runs the open under a context that is cancelled while the
// hold's first matching call is parked at the seam, and asserts the open
// answers the context's error: the row arms h, runs the open, waits for the
// hold to be reached, cancels and releases, so the write under way finishes
// and every later write is the doorstep check's to refuse.
func cancelledUnder(t *testing.T, r *rig, h *kit.Hold) error {
	t.Helper()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := Run(ctx, r.runDeps(), core.Version{}, journalID, nil)
		done <- err
	}()

	select {
	case <-h.Reached():
	case <-time.After(10 * time.Second):
		cancel()
		t.Fatal("spares: the cancelled open never reached the call the row parks at; the open is stuck before it")
	}
	cancel()
	h.Release()

	select {
	case err := <-done:
		return err
	case <-time.After(10 * time.Second):
		t.Fatal("spares: the cancelled open never answered; it is parked somewhere past the hold")
	}
	return nil
}

// TestSparesPhaseACancelledDuringTheDirectorySyncZeroesNothingPlants two
// interrupted recycles — two staged spares whose dirty slots the phase would
// zero — and a short spare beside them, and cancels the open while the
// phase's directory sync is under way: the sync in flight finishes, but no
// write begins past the cancellation — neither staged spare is zeroed, its
// durable bytes the dirty bytes the fixture planted, and the short spare
// keeps its name.
func TestSparesPhaseACancelledDuringTheDirectorySyncZeroesNothing(t *testing.T) {
	r := newRig(t)
	r.writeFormat()
	x1, y1 := core.SegmentID(3), core.SegmentID(9)
	x2, y2 := core.SegmentID(4), core.SegmentID(10)
	short := core.SegmentID(7)
	plantInterruptedRecycle(r, x1, y1)
	plantInterruptedRecycle(r, x2, y2)
	r.write(format.SegmentName(short), make([]byte, testCap/2))
	before1, before2 := r.fileBytes(format.SegmentName(y1)), r.fileBytes(format.SegmentName(y2))
	removes := r.seam.Calls(kit.OpRemove)

	held := r.seam.HoldFirst(t, kit.OpSyncDir, "*")
	err := cancelledUnder(t, r, held)

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("spares: the open cancelled during the phase's directory sync answered err %v, want context.Canceled before the zeroings began", err)
	}
	if after := r.fileBytes(format.SegmentName(y1)); !slices.Equal(after, before1) {
		t.Errorf("spares: the cancelled open zeroed the staged spare %d, got %x..., want the dirty bytes it arrived with: no write begins past the cancellation", y1, after[:format.SegmentHeaderSize])
	}
	if after := r.fileBytes(format.SegmentName(y2)); !slices.Equal(after, before2) {
		t.Errorf("spares: the cancelled open zeroed the staged spare %d, got %x..., want the dirty bytes it arrived with: no write begins past the cancellation", y2, after[:format.SegmentHeaderSize])
	}
	if !slices.Contains(r.listing(), format.SegmentName(short)) {
		t.Errorf("spares: the cancelled open unlinked the short spare %s; the directory holds %v, want it kept: no write begins past the cancellation", format.SegmentName(short), r.listing())
	}
	if got := r.seam.Calls(kit.OpRemove); got != removes {
		t.Errorf("spares: the cancelled open removed something, got %d removes, want %d, the count before the run", got, removes)
	}
}

// TestSparesPhaseACancelledDuringTheFirstZeroingStopsTheRest plants two
// interrupted recycles and cancels the open while the first staged spare's
// zeroing is under way: the write in flight finishes — its whole self is
// zeroed and synced — but the second staged spare keeps the dirty bytes it
// arrived with, no removal runs, and the open answers the context's error.
func TestSparesPhaseACancelledDuringTheFirstZeroingStopsTheRest(t *testing.T) {
	r := newRig(t)
	r.writeFormat()
	x1, y1 := core.SegmentID(3), core.SegmentID(9)
	x2, y2 := core.SegmentID(4), core.SegmentID(10)
	plantInterruptedRecycle(r, x1, y1)
	plantInterruptedRecycle(r, x2, y2)
	before2 := r.fileBytes(format.SegmentName(y2))
	removes := r.seam.Calls(kit.OpRemove)

	held := r.seam.HoldFirst(t, kit.OpWrite, format.SegmentName(y1))
	err := cancelledUnder(t, r, held)

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("spares: the open cancelled during the first zeroing answered err %v, want context.Canceled before the second staged spare's zeroing began", err)
	}
	if after1 := r.fileBytes(format.SegmentName(y1)); anyHeaderByte(after1[:format.SegmentHeaderSize]) || anyHeaderByte(after1[testCap-format.TrailerSize:]) {
		t.Errorf("spares: the write the cancellation landed on did not finish, got header %x and trailer %x, want the in-flight zeroing completed",
			after1[:format.SegmentHeaderSize], after1[testCap-format.TrailerSize:])
	}
	if after := r.fileBytes(format.SegmentName(y2)); !slices.Equal(after, before2) {
		t.Errorf("spares: the cancelled open zeroed the second staged spare %d, got %x..., want the dirty bytes it arrived with: no write begins past the cancellation", y2, after[:format.SegmentHeaderSize])
	}
	if got := r.seam.Calls(kit.OpRemove); got != removes {
		t.Errorf("spares: the cancelled open removed something, got %d removes, want %d, the count before the run", got, removes)
	}
}

// TestSparesPhaseACancelledDuringTheUnlinksStopsTheRest plants two short
// record-less candidates — the phase stages nothing to take, so the removals
// are all it does — and cancels the open while the first candidate's removal
// is under way: that removal finishes, but the second candidate keeps its
// name, for no write begins past the cancellation.
func TestSparesPhaseACancelledDuringTheUnlinksStopsTheRest(t *testing.T) {
	r := newRig(t)
	r.writeFormat()
	first, second := core.SegmentID(4), core.SegmentID(9)
	r.write(format.SegmentName(first), make([]byte, testCap/2))
	r.write(format.SegmentName(second), make([]byte, testCap/2))

	held := r.seam.HoldFirst(t, kit.OpRemove, format.SegmentName(first))
	err := cancelledUnder(t, r, held)

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("spares: the open cancelled during the first removal answered err %v, want context.Canceled before the second removal began", err)
	}
	if slices.Contains(r.listing(), format.SegmentName(first)) {
		t.Errorf("spares: the removal the cancellation landed on did not finish, %s is still in the directory %v, want the in-flight removal completed", format.SegmentName(first), r.listing())
	}
	if !slices.Contains(r.listing(), format.SegmentName(second)) {
		t.Errorf("spares: the cancelled open unlinked the second short spare %s; the directory holds %v, want it kept: no write begins past the cancellation", format.SegmentName(second), r.listing())
	}
}

// TestSpareCandidatesHoldsATakeOnlyOverAnUnprovenEnd pins the naming hold's
// whole release surface at the function that owns it: a take naming a
// predecessor on disk is held only while neither of the predecessor's own
// proofs seals its end. The marker's proof releases on the scan itself; the
// trailer's — the marker-alone exemption's, surfacing as the sealed fate —
// releases on the classing's answer; the leg with no proof held, its inputs
// the hand-fed ones this row supplies, is the shape today's open refuses
// before the phase (a named predecessor whose end neither proof seals), which
// the later verdict-carrying phases will let reach it and the hold must
// still meet there.
func TestSpareCandidatesHoldsATakeOnlyOverAnUnprovenEnd(t *testing.T) {
	const w = core.SegmentID(2)
	sealed, _ := buildSealed(1, tailRec(200, 0x4100))
	rotted := func() []byte {
		b, ents := buildSealed(1, tailRec(200, 0x4100))
		markerOff := len(b) - raw.TrailerSize - raw.CatalogEntrySize*len(ents) - format.SealMarkerSize
		b[markerOff+31] ^= 0xff
		return b
	}

	for _, leg := range []struct {
		name     string
		plant    []byte
		proven   map[core.SegmentID]bool // the classing's sealed-end answer, hand-fed for the arm the open cannot run
		wantHeld bool
	}{
		{"the marker releases it, its seal read on the scan", sealed,
			map[core.SegmentID]bool{1: false}, false},
		{"the trailer releases it, its proof the classing's sealed fate", rotted(),
			map[core.SegmentID]bool{1: true}, false},
		{"an end no proof seals holds it", rotted(),
			map[core.SegmentID]bool{1: false}, true},
	} {
		t.Run(leg.name, func(t *testing.T) {
			r := newRig(t)
			r.writeFormat()
			r.plantSeg(1, leg.plant)
			plantTake(r, w, 1)
			st := r.runCandidates()

			cands, heldTakes := spareCandidates(st.Attached, st.Rebuilt.Scans, successorSet(st.Attached.Own), nil, leg.proven)

			gotHeld := slices.Contains(heldTakes, w)
			if gotHeld != leg.wantHeld {
				t.Errorf("spareCandidates: the take %d naming predecessor 1 got held=%v, want %v over %s", w, gotHeld, leg.wantHeld, leg.name)
			}
			gotCand := slices.Contains(cands, w)
			if gotCand != !leg.wantHeld {
				t.Errorf("spareCandidates: the take %d got candidate=%v, want %v over %s: held and candidate are mutually exclusive", w, gotCand, !leg.wantHeld, leg.name)
			}
		})
	}
}

// TestTheHeldTakesCountAheadOfTheAdoptedClasses runs the phase itself over
// the state a real open builds and the one hand-stamp the open cannot: the
// predecessor's proof pressed away, the missing-end verdict the refused
// opens of today keep from reaching the phase and the later phases will
// carry — so the take is held, and held takes stand in the headroom ahead of
// the classes the adopted spares draw. Over one held take and a headroom
// target of one, the first adopted spare — his worked example — takes the
// reserve, never the headroom slot the held take occupies, and the spare
// past it takes content.
func TestTheHeldTakesCountAheadOfTheAdoptedClasses(t *testing.T) {
	r := newRig(t)
	r.writeFormat()
	r.plantSeg(1, markerRottedSealed(1))
	plantTake(r, 2, 1) // W: the held take, its predecessor's proof pressed away below
	r.write(format.SegmentName(5), make([]byte, testCap))
	r.write(format.SegmentName(6), make([]byte, testCap))

	deps := r.runDeps()
	deps.SpareTargets = segment.Targets{Content: 2, Headroom: 1, Reserve: 1}
	ctx := context.Background()
	rd := newReader(ctx, r.seam.FS(), r.clk, nil)
	st := State{}
	if err := runAttach(ctx, deps, rd, &st, journalID, core.Version{}, nil); err != nil {
		t.Fatalf("attach: %v", err)
	}
	if err := runRebuild(ctx, deps, rd, &st, journalID, core.Version{}, nil); err != nil {
		t.Fatalf("rebuild: %v", err)
	}
	// The stamp the open cannot reach: the classing proved P's end (its
	// trailer), the phase holds W only where it did not — the state the
	// refused missing-end verdicts will hand the phase once the later phases
	// defer them instead of refusing.
	for i := range st.Tails.Fates {
		if st.Tails.Fates[i].ID == 1 {
			st.Tails.Fates[i].Kind = fateTornTail
		}
	}

	if err := runSpares(ctx, deps, rd, &st, journalID, core.Version{}, nil); err != nil {
		t.Fatalf("spares: the phase over a held take: %v", err)
	}
	if got, want := st.Spares.Held, []core.SegmentID{2}; !slices.Equal(got, want) {
		t.Fatalf("spares: the take over the pressed proof got held %v, want %v", got, want)
	}
	wantAdopted := []AdoptedSpare{{ID: 5, Class: segment.Reserve}, {ID: 6, Class: segment.Content}}
	if got := st.Spares.Adopted; !slices.Equal(got, wantAdopted) {
		t.Errorf("spares: with one held take standing in the headroom the adoptions got %v, want %v: the held take occupies the headroom's slot, the first adopted spare takes the reserve, the next content", got, wantAdopted)
	}
}
