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
// classing gives an unsealed one — and the failed segment's id is kept for
// the phases that re-prove its window.
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
		t.Errorf("direct reads of the marker got %d, want 0: the marker is never re-read after a failed sync", calls)
	}
	wantFate(t, st, 2, fateTornTail, markerOff)
	if !slices.Equal(st.Proof.FailedWindows, []core.SegmentID{2}) {
		t.Errorf("FailedWindows got %v, want [2]: the segment whose proof sync failed is named for the phases that re-prove it", st.Proof.FailedWindows)
	}
}
