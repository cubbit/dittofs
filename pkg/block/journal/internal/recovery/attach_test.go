package recovery

import (
	"context"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"slices"
	"syscall"
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/format"
	"github.com/cubbit/dittofs/pkg/block/journal/journaltest/kit"
	"github.com/cubbit/dittofs/pkg/block/journal/journaltest/raw"
)

// journalID is the identity of the journal every own segment names.
var journalID = core.JournalID{'o', 'w', 'n', '-', 'i', 'd', 'e', 'n', 't', 'i', 't', 'y'}

// install is the installation ID of the format file the attach pass checks
// versions against. Its value is never read by attach, only its version is.
var install = core.InstallationID{'k', 'i', 't'}

// rig is one journal directory on the real filesystem, reached through a
// crash-simulating seam whose schedule injects the read faults.
type rig struct {
	t    *testing.T
	seam *kit.Seam

	clk kit.DrivenClock
}

// newRig makes the directory and the seam over it, and registers the cleanup.
func newRig(t *testing.T) *rig {
	t.Helper()
	dir := t.TempDir()
	seam, err := kit.NewSeam(dir)
	if err != nil {
		t.Fatalf("fixture: new seam: %v", err)
	}
	t.Cleanup(seam.Abandon)
	return &rig{t: t, seam: seam, clk: kit.NewStopwatchClock()}
}

// write makes the file name from whole bytes, as a synced writer would have
// left it: created and written through the seam, then the directory synced,
// so the seam serves the file and takes it as durable.
func (r *rig) write(name string, b []byte) {
	r.t.Helper()
	f, err := r.seam.Create(name)
	if err != nil {
		r.t.Fatalf("fixture: create %s: %v", name, err)
	}
	if len(b) > 0 {
		if _, err := f.WriteAt(b, 0); err != nil {
			r.t.Fatalf("fixture: write %s: %v", name, err)
		}
		if err := f.Sync(); err != nil {
			r.t.Fatalf("fixture: sync %s: %v", name, err)
		}
	}
	if err := f.Close(); err != nil {
		r.t.Fatalf("fixture: close %s: %v", name, err)
	}
	if err := r.seam.SyncDir(); err != nil {
		r.t.Fatalf("fixture: sync dir: %v", err)
	}
}

// writeFormat makes the journal's format file, version 1, under its final
// name; the attach pass reads only its version.
func (r *rig) writeFormat() {
	r.t.Helper()
	r.write(format.FormatFileName, format.AppendFormatFile(nil, format.FormatFile{
		Identity:     journalID,
		Installation: install,
	}))
}

// writeSeg makes segment id's file, header identity this journal's unless
// name gives another.
func (r *rig) writeSeg(id core.SegmentID, name core.JournalID) {
	r.t.Helper()
	h := raw.SegmentHeader{ID: uint64(id)}
	copy(h.Identity[:], name[:])
	r.write(format.SegmentName(id), h.Bytes())
}

// listing returns the directory's names as os sorts them, straight from the
// filesystem, so a check reads what a later Open finds, not the seam's view.
func (r *rig) listing() []string {
	r.t.Helper()
	names, err := r.seam.List()
	if err != nil {
		r.t.Fatalf("fixture: list: %v", err)
	}
	return names
}

// run makes the reader and runs the attach pass over the seam's view, the
// way recovery.Run will: one reader whose collected ranges the caller reads
// and the caller's logger, here discarded as the journal's own default is.
func (r *rig) run(ctx context.Context) (Attached, []core.SegmentRange) {
	r.t.Helper()
	rd := newReader(ctx, r.seam.FS(), r.clk)
	att, err := attach(ctx, r.seam.FS(), rd, journalID, nil)
	if err != nil {
		r.t.Fatalf("attach: %v", err)
	}
	ranges, unreadFormat := rd.Unreadable()
	if unreadFormat {
		r.t.Fatalf("attach: the format file was collected as unreadable: %v", ranges)
	}
	return att, ranges
}

// failReads makes reads of name fail count times with EIO, then pass; zero
// fails every read.
func (r *rig) failReads(name string, count int) {
	r.t.Helper()
	if err := r.seam.Schedule(kit.Failing(kit.OpRead, name, count, syscall.EIO)); err != nil {
		r.t.Fatalf("fixture: schedule: %v", err)
	}
}

func TestAttachOwnSegment(t *testing.T) {
	r := newRig(t)
	r.writeFormat()
	r.writeSeg(7, journalID)

	att, ranges := r.run(context.Background())

	if want := []core.SegmentID{7}; !slices.Equal(ids(att.Own), want) {
		t.Errorf("attach: own segment ids got %v, want %v", ids(att.Own), want)
	}
	if len(ranges) != 0 {
		t.Errorf("attach: unreadable ranges got %v, want none", ranges)
	}
	if att.MaxID != 7 {
		t.Errorf("attach: MaxID from segment ids got %d, want 7", att.MaxID)
	}
}

// The pass leaves a foreign segment: its bytes are unchanged, it contributes
// nothing, and Rejected counts it.
func TestAttachForeignSegment(t *testing.T) {
	other := core.JournalID{'a', 'n', 'o', 't', 'h', 'e', 'r'}
	r := newRig(t)
	r.writeFormat()
	r.writeSeg(5, other)
	before := r.fileBytes(format.SegmentName(5))

	att, _ := r.run(context.Background())

	if len(att.Own) != 0 {
		t.Errorf("attach: a foreign segment was attached as own: got %v, want none", att.Own)
	}
	if len(att.Foreign) != 1 || att.Foreign[0].ID != 5 || att.Foreign[0].Header.Identity != other {
		t.Errorf("attach: foreign segments got %+v, want one segment 5 of identity %x", att.Foreign, other)
	}
	if att.Rejected != 1 {
		t.Errorf("attach: Rejected got %d, want 1", att.Rejected)
	}
	if after := r.fileBytes(format.SegmentName(5)); !slices.Equal(before, after) {
		t.Errorf("attach: a foreign segment's bytes changed: got %x, want the bytes it arrived with", after)
	}
	if !slices.Contains(r.listing(), format.SegmentName(5)) {
		t.Errorf("attach: the foreign segment was unlinked; directory holds %v", r.listing())
	}
}

// A spare's temporary name is unlinked, and its id counts into MaxID above
// every .seg id, so the allocator never reuses it.
func TestAttachSpareUnlinkedAndSeedAboveItsID(t *testing.T) {
	r := newRig(t)
	r.writeFormat()
	r.writeSeg(3, journalID)
	r.write(format.SpareName(9), nil)

	att, _ := r.run(context.Background())

	if slices.Contains(r.listing(), format.SpareName(9)) {
		t.Errorf("attach: the .spare.tmp was not unlinked; directory holds %v", r.listing())
	}
	if att.MaxID != 9 {
		t.Errorf("attach: MaxID got %d, want 9, above the .spare.tmp's id", att.MaxID)
	}
}

// format.tmp is ignored: neither unlinked nor counted.
func TestAttachIgnoresFormatTmp(t *testing.T) {
	r := newRig(t)
	r.writeFormat()
	r.write(format.FormatTmpName, []byte("half a format file"))

	r.run(context.Background())

	if !slices.Contains(r.listing(), format.FormatTmpName) {
		t.Errorf("attach: format.tmp was unlinked; directory holds %v", r.listing())
	}
}

// Files the classifier does not recognise — a segment-named backup, a notes
// file, a spare-named one whose id is 0, which no spare the journal makes
// carries — are left alone whatever their bytes, reported in Unrecognised,
// and none of their ids counts into MaxID.
func TestAttachUnrecognisedNamesLeftAlone(t *testing.T) {
	names := []string{
		"00000000000000000042.seg.bak",
		"notes.txt",
		"0000000000000000000.spare.tmp", // id 0: a name the journal could not have produced
	}
	r := newRig(t)
	r.writeFormat()
	r.writeSeg(2, journalID)
	for _, name := range names {
		r.write(name, []byte("bytes of a file the journal did not write"))
	}

	att, _ := r.run(context.Background())

	got := r.listing()
	for _, name := range names {
		if !slices.Contains(got, name) {
			t.Errorf("attach: unrecognised file %s was removed; directory holds %v", name, got)
		}
	}
	// Every unrecognised name is reported, in the listing's order; the
	// comparison sorts both, so it names the set, not the order the
	// directory happened to hand over.
	gotNames := slices.Clone(att.Unrecognised)
	wantNames := slices.Clone(names)
	slices.Sort(gotNames)
	slices.Sort(wantNames)
	if !slices.Equal(gotNames, wantNames) {
		t.Errorf("attach: Unrecognised got %v, want %v, every unrecognised name reported", att.Unrecognised, names)
	}
	if att.MaxID != 2 {
		t.Errorf("attach: MaxID got %d, want 2, from the .seg file alone: no unrecognised name counts its id", att.MaxID)
	}
}

// A segment header of a version this binary does not read fails the pass
// with ErrUnknownFormat; its bytes are left as they are.
func TestAttachUnknownHeaderVersion(t *testing.T) {
	r := newRig(t)
	r.writeFormat()
	r.write(format.SegmentName(4), versionTwoSegmentHeader())

	rd := newReader(context.Background(), r.seam.FS(), r.clk)
	_, err := attach(context.Background(), r.seam.FS(), rd, journalID, nil)
	if !errors.Is(err, core.ErrUnknownFormat) {
		t.Errorf("attach: a segment of an unknown version got err %v, want ErrUnknownFormat", err)
	}
	if !slices.Contains(r.listing(), format.SegmentName(4)) {
		t.Errorf("attach: the segment of the unknown version was unlinked; directory holds %v", r.listing())
	}
}

// A zero-header slot — an untaken spare, a short or zero-length segment file —
// is never a failure of the pass: the id counts into MaxID and the slot goes
// to ZeroHeader for the later phases to decide.
func TestAttachZeroHeaderSlot(t *testing.T) {
	testCases := []struct {
		name string
		b    []byte
	}{
		{"all-zero slot", make([]byte, format.SegmentHeaderSize)},
		{"short file", make([]byte, 10)},
		{"zero-length file", nil},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			r := newRig(t)
			r.writeFormat()
			r.write(format.SegmentName(6), tc.b)

			att, _ := r.run(context.Background())

			if want := []core.SegmentID{6}; !slices.Equal(att.ZeroHeader, want) {
				t.Errorf("attach: zero-header segments got %v, want %v", att.ZeroHeader, want)
			}
			if len(att.Own) != 0 || len(att.Unverified) != 0 {
				t.Errorf("attach: a zero header slot was classed: own %v, unverified %v, want none",
					att.Own, att.Unverified)
			}
			if att.MaxID != 6 {
				t.Errorf("attach: MaxID got %d, want 6, the zero-slot segment's id", att.MaxID)
			}
		})
	}
}

// A damaged header — a checksum the segment never wrote — is an unverified
// header, not a failure and not zero: the later phases decide it. The header
// is grown in place: the seed the classifier needs is planted first, then the
// same file is written again, so no directory window serves an unfinished
// header.
func TestAttachDamagedHeaderUnverified(t *testing.T) {
	r := newRig(t)
	r.writeFormat()
	r.writeSeg(8, journalID)
	b := r.fileBytes(format.SegmentName(8))
	b[44] ^= 0xff // the header's checksum, last field
	f, err := r.seam.Open(format.SegmentName(8))
	if err != nil {
		t.Fatalf("fixture: open %s: %v", format.SegmentName(8), err)
	}
	if _, err := f.WriteAt(b, 0); err != nil {
		t.Fatalf("fixture: write %s: %v", format.SegmentName(8), err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("fixture: close %s: %v", format.SegmentName(8), err)
	}

	att, _ := r.run(context.Background())

	if want := []core.SegmentID{8}; !slices.Equal(att.Unverified, want) {
		t.Errorf("attach: unverified segments got %v, want %v", att.Unverified, want)
	}
	if len(att.Own) != 0 || len(att.ZeroHeader) != 0 {
		t.Errorf("attach: a damaged header was classed: own %v, zero %v, want none",
			att.Own, att.ZeroHeader)
	}
}

// A header read that EIOs twice and then succeeds is attached: the failed
// attempts read no bytes, so they are never judged as damage nor counted as
// an unreadable range.
func TestAttachHeaderReadFailsTwiceThenSucceeds(t *testing.T) {
	r := newRig(t)
	r.writeFormat()
	r.writeSeg(11, journalID)
	r.failReads(format.SegmentName(11), 2)

	att, ranges := r.run(context.Background())

	if want := []core.SegmentID{11}; !slices.Equal(ids(att.Own), want) {
		t.Errorf("attach: a segment whose header read on the third attempt got own ids %v, want %v", ids(att.Own), want)
	}
	if len(ranges) != 0 {
		t.Errorf("attach: a header read that succeeded was collected as unreadable: %v", ranges)
	}
	if calls := r.seam.CallsFor(kit.OpRead, format.SegmentName(11)); calls != 3 {
		t.Errorf("attach: header reads got %d, want 3 (two failed, the third attached it)", calls)
	}
}

// A header read that fails on every attempt — three of them — is collected
// as an unreadable range; the segment is never classed zero, damaged or
// spare, its file stays, and the pass still reports the segments it could
// read.
func TestAttachHeaderReadFailsAllAttempts(t *testing.T) {
	r := newRig(t)
	r.writeFormat()
	r.writeSeg(11, journalID)
	r.writeSeg(12, journalID)
	r.writeSeg(13, journalID)
	r.failReads(format.SegmentName(12), 0) // every read

	rd := newReader(context.Background(), r.seam.FS(), r.clk)
	att, err := attach(context.Background(), r.seam.FS(), rd, journalID, nil)
	if err != nil {
		t.Fatalf("attach: a failed header read failed the pass itself: %v", err)
	}
	ranges, _ := rd.Unreadable()

	want := []core.SegmentRange{{Segment: 12, Off: 0, Len: format.SegmentHeaderSize}}
	if !slices.Equal(ranges, want) {
		t.Errorf("attach: unreadable ranges got %v, want %v", ranges, want)
	}
	if want := []core.SegmentID{11, 13}; !slices.Equal(ids(att.Own), want) {
		t.Errorf("attach: own segment ids got %v, want %v", ids(att.Own), want)
	}
	if len(att.Unverified) != 0 || len(att.ZeroHeader) != 0 {
		t.Errorf("attach: a segment whose header never read was classed: unverified %v, zero %v, want none",
			att.Unverified, att.ZeroHeader)
	}
	if !slices.Contains(r.listing(), format.SegmentName(12)) {
		t.Errorf("attach: the segment whose read failed was unlinked; directory holds %v", r.listing())
	}
	if calls := r.seam.CallsFor(kit.OpRead, format.SegmentName(12)); calls != 3 {
		t.Errorf("attach: header reads got %d, want 3, the readretry bound", calls)
	}
}

// A cancelled context fails the pass with the context's error before the
// directory is touched at all. Nothing is changed — not the .spare.tmp
// unlinks the pass would have made.
func TestAttachCancelledContext(t *testing.T) {
	r := newRig(t)
	r.writeFormat()
	r.writeSeg(3, journalID)
	r.write(format.SpareName(9), nil)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	rd := newReader(ctx, r.seam.FS(), r.clk)
	_, err := attach(ctx, r.seam.FS(), rd, journalID, nil)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("attach: a cancelled context got err %v, want context.Canceled", err)
	}
	got := r.listing()
	for _, name := range []string{format.SegmentName(3), format.SpareName(9)} {
		if !slices.Contains(got, name) {
			t.Errorf("attach: a cancelled pass changed the directory: %s is gone; directory holds %v",
				name, got)
		}
	}
}

// A context cancelled mid-pass — after the first segment's header is read,
// with the second's open held at the seam — fails the pass with the
// context's error rather than answering without the second segment. The
// cancellation is not a device fault, so nothing of it lands in the
// collected ranges either: the pass answers all-or-error, never a partial
// Attached.
func TestAttachCancelledMidPass(t *testing.T) {
	r := newRig(t)
	r.writeFormat()
	r.writeSeg(11, journalID)
	r.writeSeg(13, journalID)

	ctx, cancel := context.WithCancel(context.Background())

	held := r.seam.HoldFirst(t, kit.OpOpen, format.SegmentName(13))
	done := make(chan error, 1)
	rd := newReader(ctx, r.seam.FS(), r.clk)
	go func() {
		_, err := attach(ctx, r.seam.FS(), rd, journalID, nil)
		done <- err
	}()

	<-held.Reached() // the pass is between segment 11's header and 13's
	cancel()
	held.Release()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("attach: a pass cancelled mid-listing got err %v, want context.Canceled", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("attach: a pass cancelled mid-listing did not return")
	}
	ranges, unreadFormat := rd.Unreadable()
	if len(ranges) != 0 || unreadFormat {
		t.Errorf("reader: the cancellation was collected as a device fault: ranges %v, format %t, want none",
			ranges, unreadFormat)
	}
	if !slices.Contains(r.listing(), format.SegmentName(13)) {
		t.Errorf("attach: a cancelled pass changed the directory: segment 13 is gone; directory holds %v",
			r.listing())
	}
}

// A read of the format file that no attempt completes — three of them — is
// collected with the format flag set, as the one no segment id names; the
// attach pass leaves the flag to the open to refuse on.
func TestReaderFormatUnreadable(t *testing.T) {
	r := newRig(t)
	r.writeFormat()
	r.failReads(format.FormatFileName, 0)

	rd := newReader(context.Background(), r.seam.FS(), r.clk)
	b := make([]byte, format.FormatFileSize)
	if _, err := rd.ReadAt(format.FormatFileName, b, 0); err == nil {
		t.Fatalf("reader: a format file no read completes returned nil, want the storage's error")
	}
	ranges, formatUnread := rd.Unreadable()
	if !formatUnread {
		t.Errorf("reader: the format file's failed reads were not collected: got formatUnread false, want true")
	}
	if len(ranges) != 0 {
		t.Errorf("reader: the format file's failure was wrongly collected as a segment range: %v", ranges)
	}
	if calls := r.seam.CallsFor(kit.OpRead, format.FormatFileName); calls != 3 {
		t.Errorf("reader: format reads got %d, want 3, the readretry bound", calls)
	}
}

// versionTwoSegmentHeader returns a segment header of format version 2: it
// verifies by its checksum and names this journal, so the decoder reads it
// whole and refuses it on the version alone — the one refusal the attach
// pass must fail the open with rather than classing the segment damaged.
func versionTwoSegmentHeader() []byte {
	b := make([]byte, format.SegmentHeaderSize)
	copy(b, "DJH1")
	binary.LittleEndian.PutUint16(b[4:6], 2)
	copy(b[8:24], journalID[:])
	binary.LittleEndian.PutUint64(b[24:32], 4)
	binary.LittleEndian.PutUint32(b[44:48],
		crc32.Checksum(b[:44], crc32.MakeTable(crc32.Castagnoli)))
	return b
}

// ids returns the segment ids of the infos, in their order.
func ids(infos []SegmentInfo) []core.SegmentID {
	out := make([]core.SegmentID, len(infos))
	for i, si := range infos {
		out[i] = si.ID
	}
	return out
}

// fileBytes reads the file name's bytes from the rig's directory, as a
// later Open would find them.
func (r *rig) fileBytes(name string) []byte {
	r.t.Helper()
	b, err := r.seam.Durable(name)
	if err != nil {
		r.t.Fatalf("fixture: read %s: %v", name, err)
	}
	return b
}

// A header read the open's own cancellation cut while the device also
// faulted — the first attempt failed with EIO, the second open is held at
// the seam and the context is cancelled under it — fails the pass with the
// context's error, and neither the format flag nor a segment range is
// collected, so the answer is the cancellation's alone: an open that
// reported a device fault here would leave a supervisor retrying a device
// that never failed.
//
// decision: no format file is written for this rig, so the pass holding
// nothing but the one segment proves the read left the propagate branch of
// attachSegment and not a later iteration's ctx check; a cancelled read
// between two segments is TestAttachCancelledMidPass's.
func TestAttachCancelPropagatesThroughAFailingSegmentRead(t *testing.T) {
	r := newRig(t)
	r.writeSeg(12, journalID)
	r.failReads(format.SegmentName(12), 1) // the first read attempt faults

	ctx, cancel := context.WithCancel(context.Background())
	held := r.seam.HoldFirst(t, kit.OpRead, format.SegmentName(12))
	done := make(chan error, 1)
	rd := newReader(ctx, r.seam.FS(), r.clk)
	go func() {
		_, err := attach(ctx, r.seam.FS(), rd, journalID, nil)
		done <- err
	}()

	<-held.Reached() // the second read attempt is open and held under it
	cancel()
	held.Release()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("attach: a read the cancellation cut got err %v, want context.Canceled", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("attach: a read the cancellation cut did not return")
	}
	ranges, unreadFormat := rd.Unreadable()
	if len(ranges) != 0 || unreadFormat {
		t.Errorf("reader: the cancellation was collected as a device fault: ranges %v, format %t, want none",
			ranges, unreadFormat)
	}
	if !slices.Contains(r.listing(), format.SegmentName(12)) {
		t.Errorf("attach: a cancelled pass changed the directory: segment 12 is gone; directory holds %v",
			r.listing())
	}
}
