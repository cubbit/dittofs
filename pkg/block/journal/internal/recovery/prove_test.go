package recovery

import (
	"context"
	"errors"
	"io/fs"
	"reflect"
	"slices"
	"syscall"
	"testing"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/format"
	"github.com/cubbit/dittofs/pkg/block/journal/journaltest/kit"
	"github.com/cubbit/dittofs/pkg/block/journal/journaltest/raw"
)

// writeLiveOnly writes b at off of the file name without a sync, as a
// process whose pages never reached the device left them: the seam serves
// the bytes through its live view and no sync makes them durable, so a
// direct read answers what the file held before. The file is created only
// when absent, never synced and its directory never synced: live bytes only.
func (r *rig) writeLiveOnly(name string, off int64, b []byte) {
	r.t.Helper()
	f, err := r.seam.Open(name)
	if errors.Is(err, fs.ErrNotExist) {
		f, err = r.seam.Create(name)
	}
	if err != nil {
		r.t.Fatalf("fixture: open %s: %v", name, err)
	}
	if _, err := f.WriteAt(b, off); err != nil {
		r.t.Fatalf("fixture: write %s: %v", name, err)
	}
	if err := f.Close(); err != nil {
		r.t.Fatalf("fixture: close %s: %v", name, err)
	}
}

// dropSyncs schedules the file's syncs dropped: every sync reports success
// and makes nothing durable, keeping the rules the rig's fault helpers
// scheduled before it.
func (r *rig) dropSyncs(name string) {
	r.t.Helper()
	r.faults = append(r.faults, kit.Dropping(kit.OpSync, name, 0))
	r.scheduleFaults()
}

// failDirect schedules direct reads of name failing count times with EIO
// then passing, or every direct read when count is zero, keeping the rules
// the rig's fault helpers scheduled before it.
func (r *rig) failDirect(name string, count int) {
	r.t.Helper()
	r.faults = append(r.faults, kit.Failing(kit.OpReadDirect, name, count, syscall.EIO))
	r.scheduleFaults()
}

// failSyncs schedules the file's syncs failing count times with EIO then
// passing, or every sync when count is zero, keeping the rules the rig's
// fault helpers scheduled before it.
func (r *rig) failSyncs(name string, count int) {
	r.t.Helper()
	r.faults = append(r.faults, kit.Failing(kit.OpSync, name, count, syscall.EIO))
	r.scheduleFaults()
}

// failReadsFrom schedules reads of name failing with EIO from the from'th
// matching call on — every call from it, so the bound's own attempts each
// fail — keeping the rules the rig's fault helpers scheduled before it.
func (r *rig) failReadsFrom(name string, from int) {
	r.t.Helper()
	r.faults = append(r.faults, kit.FailingFrom(kit.OpRead, name, from, 0, syscall.EIO))
	r.scheduleFaults()
}

// scheduleFaults schedules every rule the rig's fault helpers collected as
// the seam's one fault: each rule fires on its own op, and a leg that both
// drops a file's syncs and fails its direct reads needs both in the one
// schedule, since scheduling replaces any fault set before it.
func (r *rig) scheduleFaults() {
	r.t.Helper()
	if err := r.seam.Schedule(r.faults...); err != nil {
		r.t.Fatalf("fixture: schedule: %v", err)
	}
}

// proveSeg plants the bytes of segment 2 holding two records and a verifying
// seal marker naming its own offset, and returns the bytes, the marker's
// offset and the segment's file name.
func proveSeg() (seg []byte, markerOff int64, name string) {
	seg = buildSeg(2, tailRec(200, 0x4100), tailRec(201, 0x8100))
	markerOff = int64(len(seg))
	seg = append(seg, raw.SealMarker{Identity: journalID, Segment: 2, Offset: uint64(markerOff)}.Bytes()...)
	return seg, markerOff, format.SegmentName(2)
}

// TestAnUnnamedSealMarkerIsProvenOnlyOnTheDevice holds the proof's own rule:
// a marker the walk saw names a seal only when the device holds it. Verifying
// past the reader's live view, the fate seal-to-prove becomes sealed; left
// short of the device — the good bytes in pages a dropped sync never wrote —
// the marker is treated as absent: the segment keeps the torn tail the
// classing gives an unsealed one, and no phase reports it damaged.
func TestAnUnnamedSealMarkerIsProvenOnlyOnTheDevice(t *testing.T) {
	seg, markerOff, name := proveSeg()
	t.Run("verifying on the device, it seals", func(t *testing.T) {
		r := newRig(t)
		r.writeFormat()
		r.plantSeg(2, seg)
		synced := r.seam.CallsFor(kit.OpSync, name)

		st := runTails(t, r)

		wantFate(t, st, 2, fateSealed, 0)
		if calls := r.seam.CallsFor(kit.OpReadDirect, name); calls < 1 {
			t.Errorf("direct reads of the marker got %d, want at least 1: the proof reads the marker on the device", calls)
		}
		if calls := r.seam.CallsFor(kit.OpSync, name) - synced; calls != 1 {
			t.Errorf("the open's syncs of the segment got %d, want 1: the proof syncs the file before it reads the marker", calls)
		}
	})
	t.Run("disagreeing on the device, it is absent", func(t *testing.T) {
		marker := slices.Clone(seg[markerOff:])
		rotted := slices.Clone(seg)
		rotted[markerOff+format.SealMarkerSize-1] ^= 0xff // the marker's checksum
		r := newRig(t)
		r.writeFormat()
		r.plantSeg(2, rotted)
		r.writeLiveOnly(name, markerOff, marker)
		r.dropSyncs(name)

		st := runTails(t, r)

		f := fateOf(t, st, 2)
		if f.Kind == fateSealToProve || f.Kind == fateSealed {
			t.Errorf("segment 2's fate got %v, want it past the seal: the marker the live view serves never reached the device", f.Kind)
		}
		// The marker is part of the torn tail now: the segment stays a
		// torn tail cut at the marker's own offset, its records proven.
		wantFate(t, st, 2, fateTornTail, markerOff)
		if calls := r.seam.CallsFor(kit.OpReadDirect, name); calls < 1 {
			t.Errorf("direct reads of the marker got %d, want at least 1: the proof read the marker before treating it as absent", calls)
		}
		if len(st.Proof.Reappends) != 0 {
			t.Errorf("Reappends got %v, want none: an absent marker hands no record to the re-append", st.Proof.Reappends)
		}
		if len(st.Tails.Damaged) != 0 {
			t.Errorf("Damaged got %v, want none: a marker treated as absent is never damage", st.Tails.Damaged)
		}
	})
}

// TestASealMarkerASuccessorNamesIsNotReRead holds the proof's reach: a
// predecessor a successor names is sealed by that name alone, so the proof
// never opens it — no sync, no direct read, its fate unchanged.
func TestASealMarkerASuccessorNamesIsNotReRead(t *testing.T) {
	r := newRig(t)
	r.writeFormat()
	pred, _ := buildSealed(1, tailRec(200, 0x4100))
	r.plantSeg(1, pred)
	r.plantSeg(2, raw.SegmentHeader{Identity: journalID, ID: 2, Predecessor: 1}.Bytes())
	name := format.SegmentName(1)
	synced, direct := r.seam.CallsFor(kit.OpSync, name), r.seam.CallsFor(kit.OpReadDirect, name)

	st := runTails(t, r)

	if calls := r.seam.CallsFor(kit.OpSync, name) - synced; calls != 0 {
		t.Errorf("the open's syncs of the named predecessor got %d, want 0: a successor's name proves the marker", calls)
	}
	if calls := r.seam.CallsFor(kit.OpReadDirect, name) - direct; calls != 0 {
		t.Errorf("direct reads of the named predecessor's marker got %d, want 0: a marker a successor names is never re-read", calls)
	}
	wantFate(t, st, 1, fateSealed, 0)
}

// TestASealMarkerDirectReadRefusedRefusesTheOpen holds the refusal: a marker
// no bounded direct read completes fails the open on the marker's own range,
// never judged on bytes no read returned. The refusal writes nothing — the
// bytes the segment held, the last sync included, are the bytes it keeps —
// and answers with no state at all.
func TestASealMarkerDirectReadRefusedRefusesTheOpen(t *testing.T) {
	seg, markerOff, name := proveSeg()
	marker := slices.Clone(seg[markerOff:])
	rotted := slices.Clone(seg)
	rotted[markerOff+format.SealMarkerSize-1] ^= 0xff // the marker's checksum
	r := newRig(t)
	r.writeFormat()
	r.plantSeg(2, rotted)
	r.writeLiveOnly(name, markerOff, marker)
	r.dropSyncs(name)
	before := r.fileBytes(name)
	r.failDirect(name, 0)

	st, err := Run(context.Background(), r.runDeps(), core.Version{}, journalID, nil)

	var ure *core.UnreadableRangesError
	if !errors.As(err, &ure) {
		t.Fatalf("Run over a marker no direct read completed got err %v, want an UnreadableRangesError", err)
	}
	want := []core.SegmentRange{{Segment: 2, Off: markerOff, Len: format.SealMarkerSize}}
	if !slices.Equal(ure.Ranges, want) {
		t.Errorf("the refusal names ranges %v, want the one marker range %v: the read refused names the marker alone", ure.Ranges, want)
	}
	if after := r.fileBytes(name); !slices.Equal(before, after) {
		t.Errorf("the refused open changed the segment: got %x, want the bytes it held, %x", after, before)
	}
	if !reflect.DeepEqual(st, State{}) {
		t.Errorf("the refused open returned state %+v, want the zero State: a refusal rebuilds nothing", st)
	}
}

// TestAMarkerWhoseSyncFailsIsTreatedAsAbsent holds the failed sync's rule:
// the marker is never re-read after one, so the phase treats it as absent at
// once and the open still succeeds — the segment keeps the torn tail the
// classing gives an unsealed one, which the tail flow then deepens by the
// same failed window — and the failed segment's id is kept for the phases
// that re-prove its window. The sync is made once and once only: a later
// success stands in for nothing, so the tail flow attempts no second one.
func TestAMarkerWhoseSyncFailsIsTreatedAsAbsent(t *testing.T) {
	seg, markerOff, name := proveSeg()
	marker := slices.Clone(seg[markerOff:])
	rotted := slices.Clone(seg)
	rotted[markerOff+format.SealMarkerSize-1] ^= 0xff // the marker's checksum
	r := newRig(t)
	r.writeFormat()
	r.plantSeg(2, rotted)
	r.writeLiveOnly(name, markerOff, marker)
	synced := r.seam.CallsFor(kit.OpSync, name)
	r.failSyncs(name, 0) // every sync fails

	st := runTails(t, r) // the failed sync is the marker's absence, never the open's failure

	if calls := r.seam.CallsFor(kit.OpSync, name) - synced; calls != 1 {
		t.Errorf("the open's syncs of the segment got %d, want 1: the marker flow's sync is the segment's one sync, its failure leaving none for the tail flow to make", calls)
	}
	if calls := r.seam.CallsFor(kit.OpReadDirect, name); calls != 0 {
		t.Errorf("direct reads of the marker got %d, want 0: the marker is never re-read after a failed sync, and neither are the records whose window failed", calls)
	}
	// The segment's records name no sync point either, so the tail flow
	// joined the marker's failed window: both records verified at their
	// first reads, so both are handed to the re-append from their first
	// copies — the cut keeps the extent of each, handed to the re-append
	// as it is — the unseal's torn tail deepened, never cleared, and the
	// segment ends past the last record it handed over.
	wantReappends(t, st,
		reappendOf(t, 2, format.SegmentHeaderSize, seg[format.SegmentHeaderSize:markerOff-tailLen]),
		reappendOf(t, 2, markerOff-tailLen, seg[markerOff-tailLen:markerOff]))
	if !slices.Equal(st.Proof.FailedWindows, []core.SegmentID{2}) {
		t.Errorf("FailedWindows got %v, want [2]: the marker flow and the tail flow name one window, not two", st.Proof.FailedWindows)
	}
	wantFate(t, st, 2, fateEnded, markerOff)
}

// TestAMarkerWhoseSyncFailsOnceNeverSyncsAgain is the leg the other one
// cannot hold: the first sync fails and every one after it would succeed,
// so only refusing to sync again tells the two apart. The tail flow re-reads
// nothing on the device — the window is the one that failed — and both
// records join the re-append from their first copies, the segment ended past
// them: one sync made in the whole open.
func TestAMarkerWhoseSyncFailsOnceNeverSyncsAgain(t *testing.T) {
	seg, markerOff, name := proveSeg()
	marker := slices.Clone(seg[markerOff:])
	rotted := slices.Clone(seg)
	rotted[markerOff+format.SealMarkerSize-1] ^= 0xff // the marker's checksum
	r := newRig(t)
	r.writeFormat()
	r.plantSeg(2, rotted)
	r.writeLiveOnly(name, markerOff, marker)
	synced := r.seam.CallsFor(kit.OpSync, name)
	r.failSyncs(name, 1) // the first sync fails, every one after it passes

	st := runTails(t, r)

	if calls := r.seam.CallsFor(kit.OpSync, name) - synced; calls != 1 {
		t.Errorf("the open's syncs of the segment got %d, want 1: a sync that succeeded after the failed one would stand in for nothing, and the flow makes none", calls)
	}
	if calls := r.seam.CallsFor(kit.OpReadDirect, name); calls != 0 {
		t.Errorf("direct reads of the segment got %d, want 0: a window whose sync failed is never asked of the device", calls)
	}
	wantReappends(t, st,
		reappendOf(t, 2, format.SegmentHeaderSize, seg[format.SegmentHeaderSize:markerOff-tailLen]),
		reappendOf(t, 2, markerOff-tailLen, seg[markerOff-tailLen:markerOff]))
	wantFate(t, st, 2, fateEnded, markerOff)
	if !slices.Equal(st.Proof.FailedWindows, []core.SegmentID{2}) {
		t.Errorf("FailedWindows got %v, want [2]: the one failed sync names the one window", st.Proof.FailedWindows)
	}
}

// tailLen is the whole size of one tailRec, the record a tail fixture holds
// past the segment header.
var tailLen = endAfter(tailRec(200, 0x4100)) - format.SegmentHeaderSize

// tailSeg returns the synced records of the two-record tail the proof's legs
// build on, and the offset B starts at: A names the point its own sync
// completed at — A's own end — and B, naming the same point, ends past it,
// so B is the one record the proof owns.
func tailSeg() (a, b raw.Record, bAt int64) {
	endA := endAfter(tailRec(200, 0x4100))
	return syncedRec(200, 0x4100, endA), syncedRec(201, 0x8100, endA), endA
}

// reappendOf builds the record the re-append phase is handed for the bytes b
// holds at at of segment id: the header the bytes carry and the bytes
// themselves, exactly as a first read would have served them.
func reappendOf(t *testing.T, id core.SegmentID, at int64, b []byte) tailRecord {
	t.Helper()
	h, err := format.DecodeRecordHeader(b[:format.RecordHeaderSize], journalID, id, at)
	if err != nil {
		t.Fatalf("fixture: decode the record the leg planted: %v", err)
	}
	return tailRecord{Segment: id, At: at, Header: h, First: b}
}

// wantReappends checks the records the proof handed to the re-append phase,
// in order, each with the copy its first read verified.
func wantReappends(t *testing.T, st State, want ...tailRecord) {
	t.Helper()
	if len(st.Proof.Reappends) != len(want) {
		t.Fatalf("Reappends got %+v, want the %d record(s) %+v", st.Proof.Reappends, len(want), want)
	}
	for i, w := range want {
		got := st.Proof.Reappends[i]
		if got.Segment != w.Segment || got.At != w.At || got.Header != w.Header || !slices.Equal(got.First, w.First) {
			t.Errorf("Reappends[%d] got segment %d at %d header %+v first %x, want segment %d at %d header %+v first %x",
				i, got.Segment, got.At, got.Header, got.First, w.Segment, w.At, w.Header, w.First)
		}
	}
}

// TestUnprovenRecordsAreVerifiedInPlace holds the tail's own rule: the
// records past the last sync point live in the pages a dead process held
// them in, so the proof reads each through the cache, syncs the segment
// once and re-reads both on the device, where a record that verifies stays
// where it is — its index entry keeps its numbers, and nothing is
// re-appended. A is covered by its own synced-through and never re-read.
func TestUnprovenRecordsAreVerifiedInPlace(t *testing.T) {
	endA := endAfter(tailRec(200, 0x4100))
	a := syncedRec(200, 0x4100, endA)
	b := syncedRec(201, 0x8100, endA)
	c := syncedRec(202, 0xc100, endA)
	full := buildSeg(2, a, b, c)
	name := format.SegmentName(2)
	r := newRig(t)
	r.writeFormat()
	r.plantSeg(2, full[:endA]) // the durable file: the header and sync-proven A alone
	r.writeLiveOnly(name, endA, full[endA:endA+tailLen])
	r.writeLiveOnly(name, endA+tailLen, full[endA+tailLen:])
	synced := r.seam.CallsFor(kit.OpSync, name)

	st := runTails(t, r)

	if calls := r.seam.CallsFor(kit.OpSync, name) - synced; calls != 1 {
		t.Errorf("the open's syncs of the segment got %d, want 1: the proof syncs the file once, before it re-reads the tail", calls)
	}
	if calls := r.seam.CallsFor(kit.OpReadDirect, name); calls != 2 {
		t.Errorf("direct reads of the segment got %d, want 2: B and C are re-read on the device, A is covered by its own synced-through", calls)
	}
	if len(st.Proof.Reappends) != 0 {
		t.Errorf("Reappends got %+v, want none: both records verified on the device", st.Proof.Reappends)
	}
	if len(st.Proof.FailedWindows) != 0 {
		t.Errorf("FailedWindows got %v, want none: the window's sync succeeded", st.Proof.FailedWindows)
	}
	wantFate(t, st, 2, fateIntact, 0)
	// The records read back with their numbers, not re-appended: the index
	// holds the three entries, each with the version its record carried.
	got := []uint64{}
	for _, e := range st.Rebuilt.Index.FileEntries(tailFile()) {
		got = append(got, e.Version.Counter)
	}
	slices.Sort(got)
	if !slices.Equal(got, []uint64{200, 201, 202}) {
		t.Errorf("the index entries' versions got %v, want the records' own 200, 201 and 202", got)
	}
}

// TestABoundaryRecordStraddlingTheSyncPointIsVerified holds the boundary the
// proof reads: a sync whose reach stopped inside a record proves the bytes
// below the point, but the record that straddles it — starting below the
// point, ending past it — is one of the tail's, and skipping it would
// silently lose an acknowledged write. A sits wholly below the point, so
// the point's own word proves it; B is the boundary, C the one after it.
func TestABoundaryRecordStraddlingTheSyncPointIsVerified(t *testing.T) {
	endA := endAfter(tailRec(200, 0x4100))
	insideB := endA + 8 // a sync that stopped inside B itself
	a := syncedRec(200, 0x4100, endA)
	b := syncedRec(201, 0x8100, insideB)
	c := syncedRec(202, 0xc100, insideB)
	seg := buildSeg(2, a, b, c)
	name := format.SegmentName(2)
	r := newRig(t)
	r.writeFormat()
	r.plantSeg(2, seg)

	st := runTails(t, r)

	if calls := r.seam.CallsFor(kit.OpReadDirect, name); calls != 2 {
		t.Errorf("direct reads of the segment got %d, want 2: the straddling B and the C after it are proven, A is wholly below the point", calls)
	}
	if len(st.Proof.Reappends) != 0 {
		t.Errorf("Reappends got %+v, want none: all three records read back with their numbers", st.Proof.Reappends)
	}
	if len(st.Proof.FailedWindows) != 0 {
		t.Errorf("FailedWindows got %v, want none: the window's sync succeeded", st.Proof.FailedWindows)
	}
	wantFate(t, st, 2, fateIntact, 0)
	if got := planted(st); got != 3 {
		t.Errorf("the index holds %d entries, want 3: A, B and C all prove", got)
	}
}

// TestATornTailRecordIsTornNotReportedAndNotReappended holds the first
// read's verdict: a record whose payload rotted durably — the cache serves
// the same rotted bytes — does not verify at the first read and is torn.
// Torn is neither a refusal of the open nor damage the open reports, and
// the record has no copy a re-append could keep its numbers from, so it is
// handed to none — and, treated as never written, it keeps no index entry
// either: the journal never claims to hold a byte the device never took. A,
// whose sync proved it whole, stays indexed, and the segment ends at B's
// own start — the end of the last record that held — never below it.
func TestATornTailRecordIsTornNotReportedAndNotReappended(t *testing.T) {
	a, b, bAt := tailSeg()
	seg := buildSeg(2, a, b)
	seg[bAt+format.RecordHeaderSize+8+2] ^= 0xff // B's payload, where the cache reads the same bytes

	r := newRig(t)
	r.writeFormat()
	r.plantSeg(2, seg)

	st := runTails(t, r) // a torn record never refuses the open

	if len(st.Proof.Reappends) != 0 {
		t.Errorf("Reappends got %+v, want none: a torn record has no copy to re-append", st.Proof.Reappends)
	}
	if len(st.Tails.Damaged) != 0 {
		t.Errorf("Damaged got %v, want none: a record the proof finds torn is never damage", st.Tails.Damaged)
	}
	wantFate(t, st, 2, fateEnded, bAt)
	if got := planted(st); got != 1 {
		t.Errorf("the index holds %d entries, want 1: A stays indexed and the torn B — treated as never written — is dropped, so no hold claims B's extent", got)
	}
}

// TestARecordWholeOnlyInTheCacheIsLostByTheDevice holds the device's half: a
// record the cache serves whole but the device does not hold — damaged where
// it reached, or written nowhere at all — is lost by the device. The re-read
// that shows it is never a refusal, for the record below it already verified
// at its first read and its copy is kept: the re-append writes the record
// back, so the open succeeds and nothing is dropped — and the cut keeps the
// record's own extent, which the re-append claims, rather than ending the
// segment at the record's start.
func TestARecordWholeOnlyInTheCacheIsLostByTheDevice(t *testing.T) {
	a, b, bAt := tailSeg()
	full := buildSeg(2, a, b)
	name := format.SegmentName(2)
	good := full[bAt:]
	endB := int64(len(full)) // where the lost record ends: the cut stays above it

	t.Run("damaged where it reached", func(t *testing.T) {
		damaged := slices.Clone(full)
		damaged[bAt+format.RecordHeaderSize+8+2] ^= 0xff // B's payload on the device
		r := newRig(t)
		r.writeFormat()
		r.plantSeg(2, damaged)
		r.writeLiveOnly(name, bAt, good) // the good B, whole in the cache alone
		r.dropSyncs(name)                // the open's sync makes nothing durable

		st := runTails(t, r)

		wantReappends(t, st, reappendOf(t, 2, bAt, good))
		if len(st.Proof.FailedWindows) != 0 {
			t.Errorf("FailedWindows got %v, want none: a sync that reports success made no window fail", st.Proof.FailedWindows)
		}
		wantFate(t, st, 2, fateEnded, endB)
	})
	t.Run("past the device's end", func(t *testing.T) {
		r := newRig(t)
		r.writeFormat()
		r.plantSeg(2, full[:bAt]) // B lies past the durable size: the re-read short-reads
		r.writeLiveOnly(name, bAt, good)
		r.dropSyncs(name)

		st := runTails(t, r) // bytes that read are lost, never refused

		wantReappends(t, st, reappendOf(t, 2, bAt, good))
		if len(st.Proof.FailedWindows) != 0 {
			t.Errorf("FailedWindows got %v, want none: a sync that reports success made no window fail", st.Proof.FailedWindows)
		}
		wantFate(t, st, 2, fateEnded, endB)
	})
}

// TestASealMarkerThatFailsProofWithUnprovenRecords holds the two halves
// joined on one segment: a marker the cache holds whole over a rotted slot,
// with an unproven record below it, leaves the sealed states — the marker is
// absent — and its tail is proven as an unsealed segment's is, on the sync
// the marker flow already made — its one success spent, the flow makes no
// second — so the record the device does not hold joins the re-append beside
// the marker the device never showed, and the segment ends past the record
// the re-append claims. One segment, one fate, both flows.
func TestASealMarkerThatFailsProofWithUnprovenRecords(t *testing.T) {
	a, b, bAt := tailSeg()
	base := buildSeg(2, a, b)
	markerOff := int64(len(base))
	marker := raw.SealMarker{Identity: journalID, Segment: 2, Offset: uint64(markerOff)}.Bytes()
	full := append(slices.Clone(base), marker...)
	name := format.SegmentName(2)
	durable := slices.Clone(full)
	durable[bAt+format.RecordHeaderSize+8+2] ^= 0xff   // B's durable slot: garbage
	durable[markerOff+format.SealMarkerSize-1] ^= 0xff // the marker's checksum
	r := newRig(t)
	r.writeFormat()
	r.plantSeg(2, durable)
	r.writeLiveOnly(name, bAt, full[bAt:markerOff]) // the good B, cache only
	r.writeLiveOnly(name, markerOff, marker)        // the good marker, cache only
	synced := r.seam.CallsFor(kit.OpSync, name)
	r.dropSyncs(name)

	st := runTails(t, r)

	f := fateOf(t, st, 2)
	if f.Kind == fateSealToProve || f.Kind == fateSealed {
		t.Errorf("segment 2's fate got %v, want it past the seal: the marker the device holds does not verify", f.Kind)
	}
	if calls := r.seam.CallsFor(kit.OpSync, name) - synced; calls != 1 {
		t.Errorf("the open's syncs of the segment got %d, want 1: the marker flow's sync is the segment's sync, spent there", calls)
	}
	// The tail flow took the unsealed queue in hand: B is lost by the
	// device, handed over from the copy the first read verified, and the
	// segment ends past B's own extent, which the re-append claims.
	wantReappends(t, st, reappendOf(t, 2, bAt, full[bAt:markerOff]))
	wantFate(t, st, 2, fateEnded, markerOff)
	if len(st.Tails.Damaged) != 0 {
		t.Errorf("Damaged got %v, want none: a marker treated as absent and a record handed to the re-append are never damage", st.Tails.Damaged)
	}
}

// TestATailWhoseSyncFailsIsAFailedWindow holds the failed window: a tail
// whose one sync fails is not a refusal — the re-append resolves it — but a
// window the open names for the phases that re-prove it. The records that
// verified at the first read are handed to the re-append from their first
// copies, and the device is never asked: the sync that would have carried
// them there is the thing that failed.
func TestATailWhoseSyncFailsIsAFailedWindow(t *testing.T) {
	a, b, bAt := tailSeg()
	full := buildSeg(2, a, b)
	name := format.SegmentName(2)
	r := newRig(t)
	r.writeFormat()
	r.plantSeg(2, full[:bAt])
	r.writeLiveOnly(name, bAt, full[bAt:])
	r.failSyncs(name, 1)

	st := runTails(t, r) // a failed window is resolved by the re-append, never by refusing

	if !slices.Equal(st.Proof.FailedWindows, []core.SegmentID{2}) {
		t.Errorf("FailedWindows got %v, want [2]: the segment whose proof sync failed is named for the phases that re-prove it", st.Proof.FailedWindows)
	}
	wantReappends(t, st, reappendOf(t, 2, bAt, full[bAt:]))
	if calls := r.seam.CallsFor(kit.OpReadDirect, name); calls != 0 {
		t.Errorf("direct reads of the segment got %d, want 0: the records are handed over from their first copies alone", calls)
	}
	wantFate(t, st, 2, fateEnded, int64(len(full)))
}

// TestEveryDirectReadRefusedHandsTheTailOverAndRefusesAnUnnamedMarker holds
// the one split the carve-out names: on a filesystem refusing every direct
// read, an unnamed seal marker's refused re-read refuses the open on the
// marker's range — a marker the device never showed may seal nothing — while
// a record whose first read verified is handed to the re-append from its
// first copy and the open succeeds, the refusal being no proof and no
// verdict. Nothing is written either way.
func TestEveryDirectReadRefusedHandsTheTailOverAndRefusesAnUnnamedMarker(t *testing.T) {
	seg, markerOff, markerName := proveSeg()
	marker := slices.Clone(seg[markerOff:])
	rotted := slices.Clone(seg)
	rotted[markerOff+format.SealMarkerSize-1] ^= 0xff // the marker's checksum
	a, b, bAt := tailSeg()
	recSeg := buildSeg(3, a, b)
	recName := format.SegmentName(3)
	good := recSeg[bAt:]

	t.Run("a marker to prove refuses on its own range", func(t *testing.T) {
		r := newRig(t)
		r.writeFormat()
		r.plantSeg(3, recSeg[:bAt])
		r.writeLiveOnly(recName, bAt, good)
		r.plantSeg(2, rotted)
		r.writeLiveOnly(markerName, markerOff, marker)
		r.dropSyncs(markerName)
		r.failDirect(markerName, 0)
		r.failDirect(recName, 0)
		beforeMarker, beforeTail := r.fileBytes(markerName), r.fileBytes(recName)

		st, err := Run(context.Background(), r.runDeps(), core.Version{}, journalID, nil)

		var ure *core.UnreadableRangesError
		if !errors.As(err, &ure) {
			t.Fatalf("Run over a device refusing every direct read got err %v, want an UnreadableRangesError", err)
		}
		want := []core.SegmentRange{{Segment: 2, Off: markerOff, Len: format.SealMarkerSize}}
		if !slices.Equal(ure.Ranges, want) {
			t.Errorf("the refusal names ranges %v, want the one marker range %v: the refused read names the marker alone", ure.Ranges, want)
		}
		if after := r.fileBytes(markerName); !slices.Equal(beforeMarker, after) {
			t.Errorf("the refused open changed segment 2: got %x, want the bytes it held, %x", after, beforeMarker)
		}
		if after := r.fileBytes(recName); !slices.Equal(beforeTail, after) {
			t.Errorf("the refused open changed segment 3: got %x, want the bytes it held, %x", after, beforeTail)
		}
		if !reflect.DeepEqual(st, State{}) {
			t.Errorf("the refused open returned state %+v, want the zero State: a refusal rebuilds nothing", st)
		}
	})
	t.Run("the unproven records alone are handed over", func(t *testing.T) {
		r := newRig(t)
		r.writeFormat()
		r.plantSeg(3, recSeg[:bAt])
		r.writeLiveOnly(recName, bAt, good)
		r.failDirect(recName, 0)

		st := runTails(t, r) // the refused re-read is no proof and no refusal

		wantReappends(t, st, reappendOf(t, 3, bAt, good))
		if len(st.Proof.FailedWindows) != 0 {
			t.Errorf("FailedWindows got %v, want none: a device that refuses reads can still take the sync", st.Proof.FailedWindows)
		}
	})
}

// tailTriSeg is the three-record tail the cut rules build on: A names the
// point its own sync completed at — A's own end — and B and C, each ending
// past the point, are the two records the proof owns, C following the B the
// leg puts at fault.
func tailTriSeg() (a, b, c raw.Record, bAt, endC int64) {
	endA := endAfter(tailRec(200, 0x4100))
	a = syncedRec(200, 0x4100, endA)
	b = syncedRec(201, 0x8100, endA)
	c = syncedRec(202, 0xc100, endA)
	return a, b, c, endA, endA + 2*tailLen
}

// TestALostRecordSteppedBackwardFromDoesNotCutTheHoldingRecords holds the
// cut's own rule: a segment the flow ends is ended at the END of the last
// record that held, not at the start of the first one that did not — a lost
// record is re-appended and so its extent is claimed, and the records after
// it that verify on the device prove past it, so cutting at the lost
// record's start would discard records the journal keeps. The cut names C's
// end here, never below it, whatever order the flow found B's loss in.
func TestALostRecordSteppedBackwardFromDoesNotCutTheHoldingRecords(t *testing.T) {
	a, b, c, bAt, endC := tailTriSeg()
	full := buildSeg(2, a, b, c)
	name := format.SegmentName(2)
	rotted := slices.Clone(full)
	rotted[bAt+format.RecordHeaderSize+8+2] ^= 0xff // B's payload: garbage on the device alone
	r := newRig(t)
	r.writeFormat()
	r.plantSeg(2, rotted)                             // A and C durable whole, B rotted there
	r.writeLiveOnly(name, bAt, full[bAt:bAt+tailLen]) // the good B, whole in the cache alone
	r.dropSyncs(name)                                 // the open's sync makes nothing durable

	st := runTails(t, r)

	// B is lost by the device and handed to the re-append; C proves whole on
	// the device, and A its sync proved: three held records, one of them
	// re-appended, and the cut is C's end.
	wantReappends(t, st, reappendOf(t, 2, bAt, full[bAt:bAt+tailLen]))
	if f := fateOf(t, st, 2); f.Kind != fateEnded || f.Offset < endC {
		t.Errorf("segment 2's fate got %v at %d, want ended not below %d: the cut is the end of the last record that held", f.Kind, f.Offset, endC)
	}
}

// TestATornRecordBeforeAHoldingRecordIsSteppedOver holds the torn half of
// the same rule: a torn record — never written, dropped from the index — is
// stepped over exactly as a torn slot is, so a holding record after it
// proves past it and the cut keeps the holding record's extent. B keeps no
// index entry, so nothing claims its bytes, and A and C keep theirs.
func TestATornRecordBeforeAHoldingRecordIsSteppedOver(t *testing.T) {
	a, b, c, bAt, endC := tailTriSeg()
	full := buildSeg(2, a, b, c)
	full[bAt+format.RecordHeaderSize+8+2] ^= 0xff // B's payload, in the cache and on the device both

	r := newRig(t)
	r.writeFormat()
	r.plantSeg(2, full)

	st := runTails(t, r)

	if len(st.Proof.Reappends) != 0 {
		t.Errorf("Reappends got %+v, want none: the torn B is never written and holds no copy, C its own device holds", st.Proof.Reappends)
	}
	if got := planted(st); got != 2 {
		t.Errorf("the index holds %d entries, want 2: A and C alone, the torn B dropped as never written", got)
	}
	got := []uint64{}
	for _, e := range st.Rebuilt.Index.FileEntries(tailFile()) {
		got = append(got, e.Version.Counter)
	}
	slices.Sort(got)
	if !slices.Equal(got, []uint64{200, 202}) {
		t.Errorf("the index entries' versions got %v, want A's 200 and C's 202, B's 201 dropped", got)
	}
	if f := fateOf(t, st, 2); f.Kind != fateEnded || f.Offset < endC {
		t.Errorf("segment 2's fate got %v at %d, want ended not below %d: the cut steps over the torn B to C's end", f.Kind, f.Offset, endC)
	}
}

// lossRec is a loss record of its own version, an aside record the walk set
// aside, at a file offset the kind's rules leave alone.
func lossRec(counter uint64) raw.Record {
	return rec(10, func(r *raw.Record) {
		r.Version = raw.Version{Counter: counter}
		r.Loss = &raw.LossBody{Dropped: 99, Flags: 1 | 1<<8}
	})
}

// TestAnUnprovenAsideRecordIsProvenLikeAContentRecord holds the kinds rule:
// the tail gives aside records the same proof it gives content ones — the
// re-append's copier copies records whole and kind-blind, so a kind the flow
// excepts would leave its bytes unproven and its loss unreported. Lost by
// the device, the aside joins the re-append with the whole first-read copy.
func TestAnUnprovenAsideRecordIsProvenLikeAContentRecord(t *testing.T) {
	endA := endAfter(tailRec(200, 0x4100))
	l := lossRec(201)
	a := syncedRec(200, 0x4100, endA)
	full := buildSeg(2, a, l)
	lAt := endA
	name := format.SegmentName(2)
	durable := slices.Clone(full)
	durable[lAt+format.RecordHeaderSize+3] ^= 0xff // the loss record's body, rotted on the device alone
	r := newRig(t)
	r.writeFormat()
	r.plantSeg(2, durable)
	r.writeLiveOnly(name, lAt, full[lAt:]) // the good loss record, whole in the cache alone
	r.dropSyncs(name)

	st := runTails(t, r)

	// The aside is lost by the device and handed over whole: its first copy
	// is the record's own bytes, header and body both, and its fate's cut
	// keeps it, as a lost content record's does.
	wantReappends(t, st, reappendOf(t, 2, lAt, full[lAt:]))
	if len(st.Proof.FailedWindows) != 0 {
		t.Errorf("FailedWindows got %v, want none: a sync that reports success made no window fail", st.Proof.FailedWindows)
	}
	wantFate(t, st, 2, fateEnded, int64(len(full)))
}

// TestATornAsideRecordIsDroppedFromTheScansAsideList holds the torn half:
// an aside record whose bytes read and do not verify is treated as never
// written, so nothing of it survives the open — the scan's aside list, the
// only place the rebuilt state holds it, drops it, for the same reason the
// index drops a torn content record: the journal never claims a record the
// device never took.
func TestATornAsideRecordIsDroppedFromTheScansAsideList(t *testing.T) {
	endA := endAfter(tailRec(200, 0x4100))
	l := lossRec(201)
	a := syncedRec(200, 0x4100, endA)
	full := buildSeg(2, a, l)
	full[endA+format.RecordHeaderSize+3] ^= 0xff // the loss record's body, in the cache and on the device both

	r := newRig(t)
	r.writeFormat()
	r.plantSeg(2, full)

	st := runTails(t, r)

	if len(st.Proof.Reappends) != 0 {
		t.Errorf("Reappends got %+v, want none: a torn aside record has no copy to re-append", st.Proof.Reappends)
	}
	sc, ok := scanOf(st, 2)
	if !ok {
		t.Fatalf("Rebuilt.Scans holds no scan of segment 2, want one")
	}
	if len(sc.Aside) != 0 {
		t.Errorf("the scan's aside list holds %+v, want it empty: the torn loss record is never written, and no later phase answers for it", sc.Aside)
	}
	wantFate(t, st, 2, fateEnded, endA)
}

// TestAnUnprovenTailRecordsFirstReadRefusedRefusesTheOpen holds the one
// refusal of the tail's own reads, D15: a candidate whose FIRST read — the
// one through the reader, payload included — no bounded attempt completes
// refuses the open on the record's body range, the bytes the read was asked
// for. Never a torn verdict, never a dropped record: a read that fails says
// nothing about the bytes it never returned, and the open writes nothing it
// refuses to rebuild. The fault arms at the body's read alone: reads one to
// ten — the attach header, the rebuild walk's four window slices, the
// proof's own four, and the record's header — all pass, so the bound spends
// its three attempts on the body and the refusal names it and nothing else.
// The from is the count of those reads over this fixture's two records,
// not a measured number anywhere else.
func TestAnUnprovenTailRecordsFirstReadRefusedRefusesTheOpen(t *testing.T) {
	a, b, bAt := tailSeg()
	full := buildSeg(2, a, b)
	name := format.SegmentName(2)
	bodyOff, bodyLen := bAt+format.RecordHeaderSize, int64(len(full))-bAt-format.RecordHeaderSize
	r := newRig(t)
	r.writeFormat()
	r.plantSeg(2, full)
	before := r.fileBytes(name)
	r.failReadsFrom(name, 11)

	st, err := Run(context.Background(), r.runDeps(), core.Version{}, journalID, nil)

	var ure *core.UnreadableRangesError
	if !errors.As(err, &ure) {
		t.Fatalf("Run over a body read no attempt completed got err %v, want an UnreadableRangesError", err)
	}
	want := []core.SegmentRange{{Segment: 2, Off: bodyOff, Len: bodyLen}}
	if !slices.Equal(ure.Ranges, want) {
		t.Errorf("the refusal names ranges %v, want the one body range %v: the refused read names the record's body alone", ure.Ranges, want)
	}
	if calls := r.seam.CallsFor(kit.OpRead, name); calls != 13 {
		t.Errorf("reads of the segment got %d, want 13: ten passed, and the bound spent three on the refused body", calls)
	}
	if after := r.fileBytes(name); !slices.Equal(before, after) {
		t.Errorf("the refused open changed the segment: got %x, want the bytes it held, %x", after, before)
	}
	if !reflect.DeepEqual(st, State{}) {
		t.Errorf("the refused open returned state %+v, want the zero State: a refusal rebuilds nothing", st)
	}
}
