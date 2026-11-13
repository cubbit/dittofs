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
// that re-prove its window.
func TestAMarkerWhoseSyncFailsIsTreatedAsAbsent(t *testing.T) {
	seg, markerOff, name := proveSeg()
	marker := slices.Clone(seg[markerOff:])
	rotted := slices.Clone(seg)
	rotted[markerOff+format.SealMarkerSize-1] ^= 0xff // the marker's checksum
	r := newRig(t)
	r.writeFormat()
	r.plantSeg(2, rotted)
	r.writeLiveOnly(name, markerOff, marker)
	if err := r.seam.Schedule(kit.Failing(kit.OpSync, name, 0, syscall.EIO)); err != nil {
		t.Fatalf("fixture: schedule: %v", err)
	}

	st := runTails(t, r) // the failed sync is the marker's absence, never the open's failure

	if calls := r.seam.CallsFor(kit.OpReadDirect, name); calls != 0 {
		t.Errorf("direct reads of the marker got %d, want 0: the marker is never re-read after a failed sync, and neither are the records whose window failed", calls)
	}
	// The segment's records name no sync point either, so the tail flow
	// joined the marker's failed window: both records verified at their
	// first reads and the sync failed again, so both are handed to the
	// re-append from their first copies and the segment ends at the first
	// of them — the unseal's torn tail deepened, never cleared.
	wantReappends(t, st,
		reappendOf(t, 2, format.SegmentHeaderSize, seg[format.SegmentHeaderSize:markerOff-tailLen]),
		reappendOf(t, 2, markerOff-tailLen, seg[markerOff-tailLen:markerOff]))
	wantFate(t, st, 2, fateEnded, format.SegmentHeaderSize)
	if !slices.Equal(st.Proof.FailedWindows, []core.SegmentID{2}) {
		t.Errorf("FailedWindows got %v, want [2]: the marker flow and the tail flow name one window, not two", st.Proof.FailedWindows)
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
// handed to none: the segment ends at the record's start, and A, whose sync
// proved it whole, stays indexed.
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
	// The segment ends at B's own start — never below the synced-through A's
	// record named, which is that same offset: the cut takes B and nothing
	// A's sync proved whole.
	wantFate(t, st, 2, fateEnded, bAt)
	if got := planted(st); got != 2 {
		t.Errorf("the index holds %d entries, want 2: A stays indexed, and the torn B stays for the read that serves it to catch", got)
	}
}

// TestARecordWholeOnlyInTheCacheIsLostByTheDevice holds the device's half: a
// record the cache serves whole but the device does not hold — damaged where
// it reached, or written nowhere at all — is lost by the device. The re-read
// that shows it is never a refusal, for the record below it already verified
// at its first read and its copy is kept: the re-append writes the record
// back, so the open succeeds and nothing is dropped.
func TestARecordWholeOnlyInTheCacheIsLostByTheDevice(t *testing.T) {
	a, b, bAt := tailSeg()
	full := buildSeg(2, a, b)
	name := format.SegmentName(2)
	good := full[bAt:]

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
		wantFate(t, st, 2, fateEnded, bAt)
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
		wantFate(t, st, 2, fateEnded, bAt)
	})
}

// TestASealMarkerThatFailsProofWithUnprovenRecords holds the two halves
// joined on one segment: a marker the cache holds whole over a rotted slot,
// with an unproven record below it, leaves the sealed states — the marker is
// absent — and its tail is proven as an unsealed segment's is, so the record
// the device does not hold joins the re-append beside the marker the device
// never showed. One segment, one fate, both flows.
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
	r.dropSyncs(name)

	st := runTails(t, r)

	f := fateOf(t, st, 2)
	if f.Kind == fateSealToProve || f.Kind == fateSealed {
		t.Errorf("segment 2's fate got %v, want it past the seal: the marker the device holds does not verify", f.Kind)
	}
	// The tail flow took the unsealed queue in hand: B is lost by the
	// device, handed over from the copy the first read verified, and the
	// segment ends at B's own start.
	wantReappends(t, st, reappendOf(t, 2, bAt, full[bAt:markerOff]))
	wantFate(t, st, 2, fateEnded, bAt)
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
	if err := r.seam.Schedule(kit.Failing(kit.OpSync, name, 1, syscall.EIO)); err != nil {
		t.Fatalf("fixture: schedule: %v", err)
	}

	st := runTails(t, r) // a failed window is resolved by the re-append, never by refusing

	if !slices.Equal(st.Proof.FailedWindows, []core.SegmentID{2}) {
		t.Errorf("FailedWindows got %v, want [2]: the segment whose proof sync failed is named for the phases that re-prove it", st.Proof.FailedWindows)
	}
	wantReappends(t, st, reappendOf(t, 2, bAt, full[bAt:]))
	if calls := r.seam.CallsFor(kit.OpReadDirect, name); calls != 0 {
		t.Errorf("direct reads of the segment got %d, want 0: the records are handed over from their first copies alone", calls)
	}
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
