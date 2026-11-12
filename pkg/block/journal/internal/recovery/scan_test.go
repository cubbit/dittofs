package recovery

import (
	"context"
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"
	"syscall"
	"testing"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/format"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/index"
	"github.com/cubbit/dittofs/pkg/block/journal/journaltest/kit"
	"github.com/cubbit/dittofs/pkg/block/journal/journaltest/raw"
)

// rec builds one record of kind k for the segment-building fixtures: a file
// of its own with a share tag, a sequence number and a version, and body
// fields the kind carries set by extra, so each check plants one record
// without repeating the fields every kind constrains on its own.
func rec(k uint8, extra func(r *raw.Record)) raw.Record {
	r := raw.Record{
		Kind:    k,
		FileID:  [16]byte{0x30, 0x31, 0x32, 0x33, 0x34, 0x35, 0x36, byte(k), 0x38, 0x39, 0x3a, 0x3b, 0x3c, 0x3d, 0x3e, 0x3f},
		Offset:  0x4100, // a file offset the kind's rules leave alone
		Length:  16,
		Seq:     100,
		Version: raw.Version{Counter: 200},
		Tag:     7,
	}
	if extra != nil {
		extra(&r)
	}
	return r
}

// buildSeg returns the bytes of segment id holding recs in order, and its
// final record slot offset, as the journal's own writer would have left them:
// the segment header naming this journal, then each record at its slot with
// the own offset the decoder checks.
func buildSeg(id core.SegmentID, recs ...raw.Record) []byte {
	b := raw.SegmentHeader{Identity: journalID, ID: uint64(id)}.Bytes()
	for i := range recs {
		recs[i].Identity = journalID
		recs[i].Segment = uint64(id)
		recs[i].RecordOffset = uint32(len(b))
		b = append(b, recs[i].Bytes()...)
	}
	return b
}

// scan runs scanSegment over the rig's seam for the segment id, the way the
// rebuild phase will: one reader whose collected ranges the caller reads.
func (r *rig) scan(id core.SegmentID, identity core.JournalID) (segmentScan, error) {
	r.t.Helper()
	rd := newReader(context.Background(), r.seam.FS(), r.clk, nil)
	return scanSegment(rd, identity, SegmentInfo{ID: id,
		Header: format.SegmentHeader{Identity: identity, ID: id}})
}

// plantSeg writes the segment id's file from bytes b, as a synced writer
// would have left it.
func (r *rig) plantSeg(id core.SegmentID, b []byte) {
	r.t.Helper()
	r.write(format.SegmentName(id), b)
}

// TestScanSegmentConvertsEveryContentKind feeds one record of each kind 1–9
// and holds the conversion against index.Record's rules per kind: the kind
// maps, the extent carries what the kind's column spells (a truncate's file
// offset is the new size with no length, a delete's extent is 0,0, a clone
// target's is the spec's DstOff and Len), Segment and At name where the
// record sits, the numbers ride along, and a clone target alone carries a
// Clone built from the body the scan went back to read.
func TestScanSegmentConvertsEveryContentKind(t *testing.T) {
	id := core.SegmentID(4)
	seg := buildSeg(id,
		rec(1, func(r *raw.Record) { r.Write = &raw.WriteBody{MTime: 1, Payload: bytes16()} }),
		rec(2, func(r *raw.Record) { r.Fill = &raw.FillBody{Payload: bytes16()} }),
		rec(3, nil), // release: any extent
		rec(4, func(r *raw.Record) { r.Length = 0 }), // truncate: Off is the new size, Len 0
		rec(5, nil), // deallocate: the extent punched
		rec(6, func(r *raw.Record) { r.Offset, r.Length = 0, 0 }), // delete: both 0
		rec(7, func(r *raw.Record) { // clone target: the extent is DstOff, Len
			r.Clone = &raw.CloneBody{Share: srcShare(), File: srcFile(), Off: 0x101, AsOf: raw.Version{Counter: 9}}
		}),
		rec(8, nil), // offloaded
		rec(9, nil), // unmark
	)
	r := newRig(t)
	r.plantSeg(id, seg)

	got, err := r.scan(id, journalID)
	if err != nil {
		t.Fatalf("scanSegment: %v", err)
	}

	wantKind := []index.RecordKind{
		index.RecWrite, index.RecFill, index.RecRelease, index.RecTruncate,
		index.RecDeallocate, index.RecDelete, index.RecCloneTarget,
		index.RecOffloaded, index.RecUnmark,
	}
	if len(got.Records) != len(wantKind) {
		t.Fatalf("scanSegment: attached %d records, want %d", len(got.Records), len(wantKind))
	}
	if got.ID != id {
		t.Errorf("scanSegment: the scan's segment id got %d, want %d", got.ID, id)
	}
	if got.Resynced {
		t.Errorf("scanSegment: a clean segment's scan got Resynced true, want false")
	}
	if got.Sealed || got.End != int64(len(seg)) {
		t.Errorf("scanSegment: a clean segment's scan got Sealed %v, End %d, want false and %d", got.Sealed, got.End, len(seg))
	}
	if got.Size != int64(len(seg)) {
		t.Errorf("scanSegment: the scan's Size got %d, want the segment's %d", got.Size, len(seg))
	}
	for i, w := range got.Records {
		if w.Kind != wantKind[i] {
			t.Errorf("scanSegment: record %d's kind got %v, want %v", i, w.Kind, wantKind[i])
		}
		if w.Segment != id {
			t.Errorf("scanSegment: record %d's segment got %d, want %d", i, w.Segment, id)
		}
		if w.Numbers != (index.Numbers{Version: core.Version{Counter: 200}, Seq: 100}) {
			t.Errorf("scanSegment: record %d's numbers got %+v, want version counter 200 and seq 100", i, w.Numbers)
		}
		if w.Clone != nil && w.Kind != index.RecCloneTarget {
			t.Errorf("scanSegment: a kind %v record carries Clone %+v, want none", w.Kind, w.Clone)
		}
	}
	// Where the record sits: At is the record's own offset in the segment,
	// whatever size each kind's record takes.
	off := int64(format.SegmentHeaderSize)
	for i, w := range got.Records {
		if w.At != uint32(off) {
			t.Errorf("scanSegment: record %d's At got %d, want %d, its own offset in the segment", i, w.At, off)
		}
		off += int64(format.RecordHeader{
			Kind: format.Kind(wantKind[i]), Extent: core.Extent{Len: 16},
		}.Size())
	}
	// Extents per kind, as index.Record's doc spells them.
	wantExtents := []core.Extent{
		{Off: 0x4100, Len: 16}, // write
		{Off: 0x4100, Len: 16}, // fill
		{Off: 0x4100, Len: 16}, // release
		{Off: 0x4100, Len: 0},  // truncate: the new size
		{Off: 0x4100, Len: 16}, // deallocate
		{Off: 0, Len: 0},       // delete
		{Off: 0x4100, Len: 16}, // clone target: DstOff, Len
		{Off: 0x4100, Len: 16}, // offloaded
		{Off: 0x4100, Len: 16}, // unmark
	}
	for i, w := range got.Records {
		if e := (core.Extent{Off: w.Off, Len: w.Len}); e != wantExtents[i] {
			t.Errorf("scanSegment: record %d's extent got %+v, want %+v", i, e, wantExtents[i])
		}
	}
	// The clone target's Clone, from the 56-byte body the scan read back and
	// verified: nothing but the spec it holds.
	wantClone := core.CloneSpec{
		SrcShare: srcShare(), Src: core.NewFileID(srcFile()), SrcOff: 0x101,
		DstOff: 0x4100, Len: 16, AsOf: core.Version{Counter: 9},
	}
	if got.Records[6].Clone == nil || *got.Records[6].Clone != wantClone {
		t.Errorf("scanSegment: the clone target's Clone got %+v, want %+v, the spec its body verified",
			got.Records[6].Clone, wantClone)
	}
}

// TestScanSegmentSetsAsideTheT75Kinds holds the aside rule: a loss, hold,
// unhold, stamp or forget record contributes no index record and lands in
// Aside with its whole header, for the phases that read those bodies to
// take — the scan here read no body byte of any of them.
func TestScanSegmentSetsAsideTheT75Kinds(t *testing.T) {
	id := core.SegmentID(5)
	seg := buildSeg(id,
		rec(10, func(r *raw.Record) { // loss
			r.Loss = &raw.LossBody{Dropped: 99, Flags: 1 | 1<<8}
		}),
		rec(11, func(r *raw.Record) { r.Cut = &raw.CutBody{Cut: 3}; r.Offset, r.Length = 0, 0 }), // hold
		rec(12, func(r *raw.Record) {
			r.Cut = &raw.CutBody{Cut: 4}
			r.FileID = [16]byte{}
			r.Version = raw.Version{}
			r.Offset, r.Length = 0, 0
		}), // unhold
		rec(13, func(r *raw.Record) { r.Cut = &raw.CutBody{Cut: 5}; r.Offset, r.Length = 0, 0 }),                     // stamp
		rec(14, func(r *raw.Record) { r.FileID = [16]byte{}; r.Offset, r.Length = 0, 0; r.Version = raw.Version{} }), // forget
	)
	r := newRig(t)
	r.plantSeg(id, seg)

	got, err := r.scan(id, journalID)
	if err != nil {
		t.Fatalf("scanSegment: %v", err)
	}

	if len(got.Records) != 0 {
		t.Errorf("scanSegment: the aside kinds built index records %v, want none", got.Records)
	}
	if len(got.Aside) != 5 {
		t.Fatalf("scanSegment: Aside holds %d records, want the 5 planted", len(got.Aside))
	}
	wantKinds := []format.Kind{format.KindLoss, format.KindHold, format.KindUnhold, format.KindStamp, format.KindForget}
	for i, a := range got.Aside {
		if a.Header.Kind != wantKinds[i] {
			t.Errorf("scanSegment: aside record %d's kind got %v, want %v", i, a.Header.Kind, wantKinds[i])
		}
		if a.Header.Segment != id {
			t.Errorf("scanSegment: aside record %d's header names segment %d, want %d", i, a.Header.Segment, id)
		}
	}
}

// TestScanSegmentRefusesReconcileKinds holds the reconcile ceiling: a
// verifying reconcile or reconciled record fails the scan with
// ErrNotImplemented, naming the kind and the segment, rather than being
// skipped or set aside — an open that quietly dropped it would serve without
// the reconcile metadata it owes.
func TestScanSegmentRefusesReconcileKinds(t *testing.T) {
	for _, tc := range []struct {
		name string
		rec  raw.Record
		kind string
	}{
		{"reconcile", rec(15, func(r *raw.Record) {
			r.FileID = [16]byte{}
			r.Offset, r.Length = 0, 0
			r.Version = raw.Version{}
			r.Tag = 0
		}), "reconcile"},
		{"reconciled", rec(16, func(r *raw.Record) {
			r.FileID = [16]byte{}
			r.Offset, r.Length = 0, 0
			r.Version = raw.Version{}
			r.Tag = 0
			r.Reconciled = &raw.ReconciledBody{Segment: 3, Start: 1, End: 2}
		}), "reconciled"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			id := core.SegmentID(6)
			r := newRig(t)
			r.plantSeg(id, buildSeg(id, tc.rec))

			_, err := r.scan(id, journalID)
			if !errors.Is(err, core.ErrNotImplemented) {
				t.Fatalf("scanSegment over a %s record got err %v, want ErrNotImplemented", tc.kind, err)
			}
			if got := err.Error(); !contains(got, tc.kind) || !contains(got, "6") {
				t.Errorf("scanSegment: the refusal %q names neither the kind %q nor the segment 6", got, tc.kind)
			}
		})
	}
}

// TestScanSegmentRefusesKind17AndKind128 holds the unknown-kind stop: a
// header of a kind this format does not define, verifying by its checksum,
// fails the scan naming the segment — never ErrNotImplemented, and never
// stepped over by any length.
func TestScanSegmentRefusesKind17AndKind128(t *testing.T) {
	for _, tc := range []struct {
		name string
		kind uint8
	}{
		{"kind 17", 17},
		{"kind 128", 128},
	} {
		t.Run(tc.name, func(t *testing.T) {
			id := core.SegmentID(7)
			r := newRig(t)
			r.plantSeg(id, buildSeg(id, rec(tc.kind, nil)))

			_, err := r.scan(id, journalID)
			if err == nil {
				t.Fatalf("scanSegment over a kind %d record returned no error, want the open to fail", tc.kind)
			}
			if errors.Is(err, core.ErrNotImplemented) {
				t.Errorf("scanSegment: a kind %d record got ErrNotImplemented, want the unknown-kind refusal", tc.kind)
			}
			if !errors.Is(err, core.ErrUnknownFormat) {
				t.Errorf("scanSegment: a kind %d record got err %v, want ErrUnknownFormat", tc.kind, err)
			}
			if got := err.Error(); !contains(got, "7") {
				t.Errorf("scanSegment: the refusal %q does not name the segment 7 it refused", got)
			}
		})
	}
}

// TestScanSegmentRefusesTheWalksEpochStopAndTheLargestCounters holds the number-range rule: a
// record whose version carries a non-zero epoch half is refused by the walk's
// own stop, as an unknown kind is, and a record whose version counter or
// sequence number is the largest 64-bit value is refused by the scan's number
// check — the journal issues neither, so it comes from damage or another
// writer.
func TestScanSegmentRefusesTheWalksEpochStopAndTheLargestCounters(t *testing.T) {
	for _, tc := range []struct {
		name string
		rec  raw.Record
	}{
		{"a non-zero epoch (the walk's stop)", rec(3, func(r *raw.Record) { r.Version = raw.Version{Epoch: 1, Counter: 200} })},
		{"the largest version counter", rec(3, func(r *raw.Record) { r.Version = raw.Version{Counter: math.MaxUint64} })},
		{"the largest sequence number", rec(5, func(r *raw.Record) { r.Seq = math.MaxUint64 })},
	} {
		t.Run(tc.name, func(t *testing.T) {
			id := core.SegmentID(8)
			r := newRig(t)
			r.plantSeg(id, buildSeg(id, tc.rec))

			_, err := r.scan(id, journalID)
			if err == nil {
				t.Fatalf("scanSegment over %s returned no error, want the open to fail", tc.name)
			}
			if !errors.Is(err, core.ErrUnknownFormat) {
				t.Errorf("scanSegment over %s got err %v, want ErrUnknownFormat", tc.name, err)
			}
			if got := err.Error(); !contains(got, "8") {
				t.Errorf("scanSegment over %s: the refusal %q does not name the segment 8 it refused", tc.name, got)
			}
		})
	}
}

// TestScanSegmentRefusesTheLargestNumbersOfEveryAsideKind holds the number-range rule for the
// kinds the scan only sets aside: a loss, hold, unhold, stamp or forget
// whose sequence number is the largest 64-bit value fails the scan as a
// content kind's does, and so does one whose version counter is — for the
// kinds whose rules allow a version at all (an unhold and a forget name
// none).
func TestScanSegmentRefusesTheLargestNumbersOfEveryAsideKind(t *testing.T) {
	zeroed := func(r *raw.Record) { r.FileID = [16]byte{}; r.Version = raw.Version{}; r.Offset, r.Length = 0, 0 }
	kinds := []struct {
		name       string
		build      func() raw.Record
		hasVersion bool
	}{
		{"loss", func() raw.Record {
			return rec(10, func(r *raw.Record) { r.Loss = &raw.LossBody{Dropped: 99, Flags: 1 | 1<<8} })
		}, true},
		{"hold", func() raw.Record {
			return rec(11, func(r *raw.Record) { r.Cut = &raw.CutBody{Cut: 3}; r.Offset, r.Length = 0, 0 })
		}, true},
		{"unhold", func() raw.Record { return rec(12, func(r *raw.Record) { r.Cut = &raw.CutBody{Cut: 4}; zeroed(r) }) }, false},
		{"stamp", func() raw.Record {
			return rec(13, func(r *raw.Record) { r.Cut = &raw.CutBody{Cut: 5}; r.Offset, r.Length = 0, 0 })
		}, true},
		{"forget", func() raw.Record { return rec(14, zeroed) }, false},
	}
	for _, k := range kinds {
		for _, num := range []string{"seq", "version"} {
			if num == "version" && !k.hasVersion {
				continue
			}
			t.Run(k.name+" with the largest "+num, func(t *testing.T) {
				id := core.SegmentID(8)
				rc := k.build()
				if num == "seq" {
					rc.Seq = math.MaxUint64
				} else {
					rc.Version = raw.Version{Counter: math.MaxUint64}
				}
				r := newRig(t)
				r.plantSeg(id, buildSeg(id, rc))

				_, err := r.scan(id, journalID)
				if !errors.Is(err, core.ErrUnknownFormat) {
					t.Fatalf("scanSegment over a %s with the largest %s got err %v, want ErrUnknownFormat", k.name, num, err)
				}
			})
		}
	}
}

// TestScanSegmentCountsOneResyncPerScan holds the resync count: two bad
// ranges crossed in one scan count one Resynced, and a range in each of two
// segments counts one per scan — the count is scans that resynced, not the
// bad ranges they crossed.
func TestScanSegmentCountsOneResyncPerScan(t *testing.T) {
	id := core.SegmentID(9)
	// buildSegAt plants records around single junk bytes, so the walk's
	// examination fails where the journal wrote nothing and the forward
	// search finds each record past it.
	build := func(id core.SegmentID, first, second raw.Record) []byte {
		b := raw.SegmentHeader{Identity: journalID, ID: uint64(id)}.Bytes()
		for _, rc := range []raw.Record{first, second} {
			// A bad slot: one whole slot of bytes, table-sorted to hold a
			// non-zero byte, so the walk's examination fails where the
			// journal wrote nothing and the forward search finds each
			// record past it, at the slot boundary the record header's own
			// offset must name.
			b = append(b, 0, 0, 0, 0, 0, 0, 0, 1)
			rc.Identity = journalID
			rc.Segment = uint64(id)
			rc.RecordOffset = uint32(len(b))
			b = append(b, rc.Bytes()...)
		}
		return b
	}
	seg := build(id,
		rec(3, nil),
		rec(3, func(r *raw.Record) { r.Seq = 101 }))
	r := newRig(t)
	r.plantSeg(id, seg)

	got, err := r.scan(id, journalID)
	if err != nil {
		t.Fatalf("scanSegment: %v", err)
	}
	if !got.Resynced {
		t.Errorf("scanSegment: a scan that crossed two bad ranges got Resynced false, want true")
	}
	if len(got.Records) != 2 {
		t.Fatalf("scanSegment: the scan attached %d records, want the 2 planted past the bad slots", len(got.Records))
	}

	// Two segments, one bad range each: each scan counts its own one.
	other := core.SegmentID(10)
	r.plantSeg(other, build(other,
		rec(3, nil),
		rec(3, func(r *raw.Record) { r.Seq = 102 })))

	again, err := r.scan(other, journalID)
	if err != nil {
		t.Fatalf("scanSegment: %v", err)
	}
	if !again.Resynced {
		t.Errorf("scanSegment: the second segment's scan got Resynced false, want true: one scan, one count")
	}
}

// TestScanSegmentSyncedThroughIsTheHighestNamed holds the synced-through
// rule: the scan answers the highest offset any verifying record names,
// whichever order they were written in.
func TestScanSegmentSyncedThroughIsTheHighestNamed(t *testing.T) {
	id := core.SegmentID(11)
	r := newRig(t)
	r.plantSeg(id, buildSeg(id,
		rec(3, func(r *raw.Record) { r.SyncedThrough = 240; r.Seq = 101 }),
		rec(5, func(r *raw.Record) { r.SyncedThrough = 100; r.Seq = 102 }),
	))

	got, err := r.scan(id, journalID)
	if err != nil {
		t.Fatalf("scanSegment: %v", err)
	}
	if got.SyncedThrough != 240 {
		t.Errorf("scanSegment: SyncedThrough got %d, want 240, the highest any record names", got.SyncedThrough)
	}
}

// TestScanSegmentRetriesAFailedReadThenSucceeds holds both halves of the
// read bound: a scan whose reads fault twice and then succeed answers same
// as one that never faulted, and a scan whose every attempt faults returns
// the device's error as itself — the range the bound collected refuses the
// open, the segment file is left exactly as it stands, and the never-read
// bytes are judged nowhere.
func TestScanSegmentRetriesAFailedReadThenSucceeds(t *testing.T) {
	id := core.SegmentID(12)
	seg := buildSeg(id, rec(3, nil), rec(5, func(r *raw.Record) { r.Seq = 101 }))
	clean := newRig(t)
	clean.plantSeg(id, seg)
	want, err := clean.scan(id, journalID)
	if err != nil {
		t.Fatalf("scanSegment without faults: %v", err)
	}

	r := newRig(t)
	r.plantSeg(id, seg)
	r.failReads(format.SegmentName(id), 2)

	got, err := r.scan(id, journalID)
	if err != nil {
		t.Fatalf("scanSegment with two EIOs then success: %v", err)
	}
	if len(got.Records) != len(want.Records) || got.Records[0] != want.Records[0] {
		t.Errorf("scanSegment: the two-EIO scan got records %+v, want the no-fault %+v", got.Records, want.Records)
	}

	failing := newRig(t)
	failing.plantSeg(id, seg)
	before := failing.fileBytes(format.SegmentName(id))
	failing.failReads(format.SegmentName(id), 0)

	rd := newReader(context.Background(), failing.seam.FS(), failing.clk, nil)
	_, err = scanSegment(rd, journalID, SegmentInfo{ID: id})
	if err == nil {
		t.Fatalf("scanSegment with every attempt failing returned no error, want the fault it never cleared")
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("scanSegment: an EIO every attempt failed came back as the context's error: %v", err)
	}
	ranges := rd.Unreadable()
	if len(ranges) != 1 || ranges[0].Segment != id || ranges[0].Len <= 0 {
		t.Errorf("scanSegment: the unreadable ranges got %v, want one naming segment %d", ranges, id)
	}
	if after := failing.fileBytes(format.SegmentName(id)); !slices.Equal(before, after) {
		t.Errorf("scanSegment: the refused scan changed the segment file: got %x, want the bytes it started with", after)
	}
}

// TestScanSegmentTooShortForASlotIsEmpty holds the empty scan of a segment
// with its header verifying but too short for one record slot contributes
// nothing and fails nothing — the later phases judge it.
func TestScanSegmentTooShortForASlotIsEmpty(t *testing.T) {
	id := core.SegmentID(13)
	r := newRig(t)
	b := raw.SegmentHeader{Identity: journalID, ID: uint64(id)}.Bytes()
	b = append(b, make([]byte, 40)...) // past the header, short of a record slot
	r.plantSeg(id, b)

	got, err := r.scan(id, journalID)
	if err != nil {
		t.Fatalf("scanSegment over a too-short segment: %v, want no error", err)
	}
	if len(got.Records) != 0 || len(got.Aside) != 0 {
		t.Errorf("scanSegment over a too-short segment got records %v and asides %v, want none", got.Records, got.Aside)
	}
	if got.ID != id {
		t.Errorf("scanSegment over a too-short segment: the scan's segment id got %d, want %d", got.ID, id)
	}
	if want := int64(len(b)); got.End != want || got.Size != want {
		t.Errorf("scanSegment over a too-short segment: End %d and Size %d, want both the file's size %d", got.End, got.Size, want)
	}
}

// TestScanSegmentRefusesACloneTargetBodyThatDoesNotVerify holds the one body
// the rebuild reads at the scan: a clone target's 56-byte spec, verified
// whole where it is read. A body that does not verify fails the scan naming
// the segment and the record's offset — never silently dropped, for the
// destination's extent would then read as zeros.
func TestScanSegmentRefusesACloneTargetBodyThatDoesNotVerify(t *testing.T) {
	id := core.SegmentID(14)
	seg := buildSeg(id, rec(7, func(r *raw.Record) {
		r.Clone = &raw.CloneBody{Share: srcShare(), File: srcFile(), Off: 0x101, AsOf: raw.Version{Counter: 9}}
	}))
	seg[format.SegmentHeaderSize+format.RecordHeaderSize] ^= 0xff // the spec's first byte
	r := newRig(t)
	r.plantSeg(id, seg)

	_, err := r.scan(id, journalID)
	if err == nil {
		t.Fatalf("scanSegment over a clone target whose body does not verify returned no error, want the open to fail")
	}
	if !errors.Is(err, core.ErrCorrupt) {
		t.Errorf("scanSegment: a clone target body that does not verify got err %v, want ErrCorrupt", err)
	}
	if got := err.Error(); !contains(got, "14") || !contains(got, fmt.Sprint(format.SegmentHeaderSize)) {
		t.Errorf("scanSegment: the refusal %q names neither the segment 14 nor the record's offset", got)
	}
}

// TestScanSegmentRefusesACloneTargetBodyOfALaterEpochAsUnknownFormat holds
// that a verifying clone target whose as-of version names a non-zero epoch
// is refused as a format this binary does not read, as a header carrying one
// is, and not as damage.
func TestScanSegmentRefusesACloneTargetBodyOfALaterEpochAsUnknownFormat(t *testing.T) {
	id := core.SegmentID(14)
	seg := buildSeg(id, rec(7, func(r *raw.Record) {
		r.Clone = &raw.CloneBody{Share: srcShare(), File: srcFile(), Off: 0x101, AsOf: raw.Version{Epoch: 1, Counter: 9}}
	}))
	r := newRig(t)
	r.plantSeg(id, seg)

	_, err := r.scan(id, journalID)
	if !errors.Is(err, core.ErrUnknownFormat) {
		t.Fatalf("scanSegment over a clone target with an as-of epoch 1 got err %v, want ErrUnknownFormat", err)
	}
	if errors.Is(err, core.ErrCorrupt) {
		t.Errorf("scanSegment: a clone target's epoch refusal %v also answers ErrCorrupt, want the unknown format alone", err)
	}
	if got := err.Error(); !contains(got, "14") || !contains(got, fmt.Sprint(format.SegmentHeaderSize)) {
		t.Errorf("scanSegment: the refusal %q names neither the segment 14 nor the record's offset", got)
	}
}

// TestScanSegmentRefusesASequenceNumberOfZeroOnAnAsideRecord holds that the
// zero sequence number, which the journal's writer never issues, is refused
// whatever the record's kind: an aside record of seq 0 fails the scan, and
// the same record of seq 1 is carried.
func TestScanSegmentRefusesASequenceNumberOfZeroOnAnAsideRecord(t *testing.T) {
	id := core.SegmentID(8)
	build := func(seq uint64) []byte {
		return buildSeg(id, rec(10, func(r *raw.Record) {
			r.Seq = seq
			r.Loss = &raw.LossBody{Dropped: 99, Flags: 1 | 1<<8}
		}))
	}
	t.Run("seq 0", func(t *testing.T) {
		r := newRig(t)
		r.plantSeg(id, build(0))
		_, err := r.scan(id, journalID)
		if !errors.Is(err, core.ErrUnknownFormat) {
			t.Fatalf("scanSegment over a loss record of seq 0 got err %v, want ErrUnknownFormat", err)
		}
	})
	t.Run("seq 1", func(t *testing.T) {
		r := newRig(t)
		r.plantSeg(id, build(1))
		got, err := r.scan(id, journalID)
		if err != nil {
			t.Fatalf("scanSegment over a loss record of seq 1: %v", err)
		}
		if len(got.Aside) != 1 || got.Aside[0].Header.Seq != 1 {
			t.Errorf("scanSegment: Aside got %v, want the one record of seq 1", got.Aside)
		}
	})
}

// bytes16 returns sixteen payload bytes, distinct from every header range.
func bytes16() []byte {
	b := make([]byte, 16)
	for i := range b {
		b[i] = 0x60 + byte(i)
	}
	return b
}

// srcShare and srcFile are a clone's source ids, of their own byte ranges.
func srcShare() [16]byte {
	return [16]byte{0xa1, 0xa2, 0xa3, 0xa4, 0xa5, 0xa6, 0xa7, 0xa8, 0xa9, 0xaa, 0xab, 0xac, 0xad, 0xae, 0xaf, 0xb0}
}
func srcFile() [16]byte {
	return [16]byte{0xc0, 0xc1, 0xc2, 0xc3, 0xc4, 0xc5, 0xc6, 0xc7, 0xc8, 0xc9, 0xca, 0xcb, 0xcc, 0xcd, 0xce, 0xcf}
}

// contains reports whether s holds substr, as a refusal's naming is checked.
func contains(s, substr string) bool { return strings.Contains(s, substr) }

// TestScanSegmentRetriesAFailedSizeThenSucceeds holds the size half of the
// read bound: a segment whose open faults twice — the attach pass's one
// header read before it having passed healthy — and then reports a size
// answers exactly the scan the same segment with no fault gives, and a size
// no bounded attempt answers is collected as the segment's whole range,
// never judged as a length the caller guesses.
func TestScanSegmentRetriesAFailedSizeThenSucceeds(t *testing.T) {
	id := core.SegmentID(4)
	seg := buildSeg(id, rec(3, nil))

	clean := newRig(t)
	clean.plantSeg(id, seg)
	want, err := clean.scan(id, journalID)
	if err != nil {
		t.Fatalf("scanSegment without faults: %v", err)
	}

	scheduled := func(r *rig, rules ...kit.Rule) {
		r.t.Helper()
		if err := r.seam.Schedule(rules...); err != nil {
			r.t.Fatalf("fixture: schedule: %v", err)
		}
	}

	// The size's open is the first call the bound makes of the file: the
	// two EIOs are its first two attempts, and the third answers the size,
	// so the scan proceeds over the same bytes as the clean one.
	r := newRig(t)
	r.plantSeg(id, seg)
	scheduled(r, kit.Failing(kit.OpOpen, format.SegmentName(id), 2, syscall.EIO))

	got, err := r.scan(id, journalID)
	if err != nil {
		t.Fatalf("scanSegment with two EIOs then success on the size's open: %v", err)
	}
	if len(got.Records) != len(want.Records) || got.Records[0] != want.Records[0] {
		t.Errorf("scanSegment: the two-EIO size open got records %+v, want the no-fault %+v", got.Records, want.Records)
	}

	// No bounded attempt answers: the whole file is collected as the one
	// range a size the bound never learned names.
	failing := newRig(t)
	failing.plantSeg(id, seg)
	scheduled(failing, kit.Failing(kit.OpOpen, format.SegmentName(id), 0, syscall.EIO))
	before := failing.fileBytes(format.SegmentName(id))

	rd := newReader(context.Background(), failing.seam.FS(), failing.clk, nil)
	_, err = scanSegment(rd, journalID, SegmentInfo{ID: id})
	if err == nil {
		t.Fatalf("scanSegment with every size open failing returned no error, want the fault it never cleared")
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("scanSegment: an EIO every size open attempt failed came back as the context's error: %v", err)
	}
	ranges := rd.Unreadable()
	if len(ranges) == 0 || ranges[0].Segment != id || ranges[0].Off != 0 || ranges[0].Len <= 0 {
		t.Errorf("scanSegment: the unreadable ranges got %v, want the segment %d whole, a size no bounded attempt answered", ranges, id)
	}
	if after := failing.fileBytes(format.SegmentName(id)); !slices.Equal(before, after) {
		t.Errorf("scanSegment: the refused size changed the segment file: got %x, want the bytes it started with", after)
	}
}
