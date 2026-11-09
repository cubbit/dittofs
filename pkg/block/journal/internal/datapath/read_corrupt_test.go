package datapath

import (
	"bytes"
	"context"
	"errors"
	"slices"
	"syscall"
	"testing"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/format"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/index"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/segment"
	"github.com/cubbit/dittofs/pkg/block/journal/journaltest/kit"
)

// segmentBytes reads n bytes of seg at off through the seam.
func segmentBytes(t *testing.T, seam *kit.Seam, seg core.SegmentID, off int64, n int) []byte {
	t.Helper()
	f, err := seam.Open(format.SegmentName(seg))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = f.Close() }()
	b := make([]byte, n)
	if _, err := f.ReadAt(b, off); err != nil {
		t.Fatalf("ReadAt: %v", err)
	}
	return b
}

// overwrite writes b at off of seg through the seam, as damage on the device.
func overwrite(t *testing.T, seam *kit.Seam, seg core.SegmentID, off int64, b []byte) {
	t.Helper()
	f, err := seam.Open(format.SegmentName(seg))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = f.Close() }()
	if _, err := f.WriteAt(b, off); err != nil {
		t.Fatalf("WriteAt: %v", err)
	}
}

// flip flips every bit of the byte at off of seg.
func flip(t *testing.T, seam *kit.Seam, seg core.SegmentID, off int64) {
	t.Helper()
	b := segmentBytes(t, seam, seg, off, 1)
	b[0] ^= 0xff
	overwrite(t, seam, seg, off, b)
}

// recordOf returns the index's piece of f at off, failing when none is held.
func recordOf(t *testing.T, c *Core, f core.FileID, off int64) index.Placed {
	t.Helper()
	p, ok := c.Index().Lookup(f, off)
	if !ok {
		t.Fatalf("nothing is held at %d of %v", off, f)
	}
	return p
}

// payloadStart is the segment offset of the first payload byte of the write
// record at r: after its header and its modification time.
func payloadStart(r index.RecordAt) int64 {
	return int64(r.Offset) + format.RecordHeaderSize + 8
}

// readUntouched reads len(p) bytes at off of f and checks that the read served
// nothing, reported want missing and left p as it was.
func readUntouched(t *testing.T, c *Core, s *Share, f core.FileID, off int64, size int, want []core.Extent) {
	t.Helper()
	p := sentinels(size)
	n, missing, held, _, err := c.ReadAt(context.Background(), s, f, off, p)
	if err != nil {
		t.Fatalf("ReadAt: %v", err)
	}
	if n != 0 || len(held) != 0 {
		t.Fatalf("the read served %d bytes, held %v, of a record that does not verify", n, held)
	}
	if !slices.Equal(missing, want) {
		t.Fatalf("missing = %v, want %v", missing, want)
	}
	if !bytes.Equal(p, sentinels(size)) {
		t.Fatal("p was touched by a read of a record that does not verify")
	}
}

func TestReadByteFlippedAtRecordEndServesNothing(t *testing.T) {
	c, seam := newTestCore(t, func(o *coreOptions) { o.segmentCap = 4 << 20 })
	s := mustShare(t, c, 1)
	f := testFile(1)
	if _, err := c.WriteAt(context.Background(), s, f, 0, pattern(1<<20, 5), 1); err != nil {
		t.Fatal(err)
	}
	syncFiles(t, c, f)
	r := recordOf(t, c, f, 0).Record
	if last := recordOf(t, c, f, 1<<20-1).Record; last != r {
		t.Fatalf("the write landed in more than one record: %+v %+v", r, last)
	}
	// The last payload byte: far from the 4 KiB the read asks for, so only a
	// read that verifies the whole payload sees it.
	flip(t, seam, r.Segment, payloadStart(r)+1<<20-1)

	readUntouched(t, c, s, f, 0, 4<<10, []core.Extent{{Off: 0, Len: 4 << 10}})
	if _, ok := c.Index().Lookup(f, 0); ok {
		t.Fatal("the record that does not verify is still held: Corrupt did not drop it")
	}
}

func TestReadHeaderForAnotherFileIsCorruption(t *testing.T) {
	c, seam := newTestCore(t)
	s := mustShare(t, c, 1)
	f, g := testFile(1), testFile(2)
	data := pattern(4<<10, 6)
	if _, err := c.WriteAt(context.Background(), s, f, 0, data, 1); err != nil {
		t.Fatal(err)
	}
	syncFiles(t, c, f)
	r := recordOf(t, c, f, 0).Record

	// The header is rewritten to name g, with both checksums valid: the
	// record verifies on its own, and only the index entry that sent the read
	// there tells it is not f's.
	raw := segmentBytes(t, seam, r.Segment, int64(r.Offset), format.RecordHeaderSize)
	h, err := format.DecodeRecordHeader(raw, testIdentity, r.Segment, int64(r.Offset))
	if err != nil {
		t.Fatalf("DecodeRecordHeader: %v", err)
	}
	h.File = g
	forged := format.AppendRecord(nil, testIdentity, h, format.RecordBody{Payload: data, MTime: 1})
	overwrite(t, seam, r.Segment, int64(r.Offset), forged[:format.RecordHeaderSize])
	whole := segmentBytes(t, seam, r.Segment, int64(r.Offset), int(h.Size()))
	fh, err := format.DecodeRecordHeader(whole[:format.RecordHeaderSize], testIdentity, r.Segment, int64(r.Offset))
	if err != nil {
		t.Fatalf("the forged header does not decode: %v", err)
	}
	if _, err := format.VerifyRecordBody(whole[format.RecordHeaderSize:format.RecordHeaderSize+fh.BodySize()], testIdentity, fh); err != nil {
		t.Fatalf("the forged record does not verify: %v", err)
	}

	readUntouched(t, c, s, f, 0, 4<<10, []core.Extent{{Off: 0, Len: 4 << 10}})
	if _, ok := c.Index().Lookup(f, 0); ok {
		t.Fatal("the record whose header names another file is still held")
	}
}

func TestReadDamagedHeaderWithNoKnownSeqIsRefused(t *testing.T) {
	c, seam := newTestCore(t)
	s := mustShare(t, c, 1)
	f := testFile(1)
	if _, err := c.WriteAt(context.Background(), s, f, 0, pattern(4<<10, 7), 1); err != nil {
		t.Fatal(err)
	}
	// Synced and completed: the ledger no longer holds the record, so nothing
	// but its damaged header could name its sequence number.
	syncFiles(t, c, f)
	before := recordOf(t, c, f, 0)
	flip(t, seam, before.Record.Segment, int64(before.Record.Offset)+50)

	p := sentinels(4 << 10)
	n, missing, held, _, err := c.ReadAt(context.Background(), s, f, 0, p)
	if !errors.Is(err, core.ErrCorrupt) {
		t.Fatalf("err = %v, want core.ErrCorrupt", err)
	}
	if n != 0 || len(held) != 0 || len(missing) != 0 {
		t.Fatalf("the refused read reports %d bytes, held %v, missing %v", n, held, missing)
	}
	if !bytes.Equal(p, sentinels(4<<10)) {
		t.Fatal("p was touched by a refused read")
	}
	if after := recordOf(t, c, f, 0); after != before {
		t.Fatalf("the index changed: %+v, was %+v", after, before)
	}
}

func TestReadDamagedHeaderOfUnsyncedRecordIsDropped(t *testing.T) {
	c, seam := newTestCore(t)
	s := mustShare(t, c, 1)
	f := testFile(1)
	// No sync completes, so the ledger still holds the record and names its
	// sequence number.
	seam.Hold(t, kit.OpSync, "*")
	if _, err := c.WriteAt(context.Background(), s, f, 0, pattern(4<<10, 8), 1); err != nil {
		t.Fatal(err)
	}
	r := recordOf(t, c, f, 0).Record
	flip(t, seam, r.Segment, int64(r.Offset)+50)

	readUntouched(t, c, s, f, 0, 4<<10, []core.Extent{{Off: 0, Len: 4 << 10}})
	if _, ok := c.Index().Lookup(f, 0); ok {
		t.Fatal("the record whose header does not decode is still held")
	}
}

func TestReadIOErrorIsReturnedAndChangesNothing(t *testing.T) {
	c, seam := newTestCore(t)
	s := mustShare(t, c, 1)
	f := testFile(1)
	if _, err := c.WriteAt(context.Background(), s, f, 0, pattern(4<<10, 9), 1); err != nil {
		t.Fatal(err)
	}
	syncFiles(t, c, f)
	before := recordOf(t, c, f, 0)
	if err := seam.Schedule(kit.Failing(kit.OpRead, format.SegmentName(before.Record.Segment), 0, syscall.EIO)); err != nil {
		t.Fatal(err)
	}

	p := sentinels(4 << 10)
	n, _, held, _, err := c.ReadAt(context.Background(), s, f, 0, p)
	if !errors.Is(err, syscall.EIO) {
		t.Fatalf("err = %v, want EIO", err)
	}
	if n != 0 || len(held) != 0 || !bytes.Equal(p, sentinels(4<<10)) {
		t.Fatalf("the failed read served %d bytes, held %v", n, held)
	}
	if after := recordOf(t, c, f, 0); after != before {
		t.Fatalf("the index changed on an I/O error: %+v, was %+v", after, before)
	}
}

// writeWithCopy writes data at 0 of f, appends a second copy of the same
// record and registers it as the record's copy, syncs both, and returns the
// index's piece and the copy's location.
func writeWithCopy(t *testing.T, c *Core, seam *kit.Seam, s *Share, f core.FileID, data []byte) (index.Placed, segment.Location) {
	t.Helper()
	if _, err := c.WriteAt(context.Background(), s, f, 0, data, 1); err != nil {
		t.Fatal(err)
	}
	orig := recordOf(t, c, f, 0)
	raw := segmentBytes(t, seam, orig.Record.Segment, int64(orig.Record.Offset), format.RecordHeaderSize)
	h, err := format.DecodeRecordHeader(raw, testIdentity, orig.Record.Segment, int64(orig.Record.Offset))
	if err != nil {
		t.Fatal(err)
	}
	unlock := c.domains.Lock(f)
	pl, err := c.Streams().For(f).Append(format.RecordHeader{
		Kind: format.KindWrite, File: f, Extent: h.Extent, Seq: h.Seq, Version: h.Version, Tag: h.Tag,
	}, format.RecordBody{Payload: data, MTime: 1})
	unlock()
	if err != nil || pl.N != int64(len(data)) {
		t.Fatalf("Append of the copy: %+v %v", pl, err)
	}
	c.Durability().RegisterCopy(h.Seq, pl.Loc, h.PayloadCRC)
	syncFiles(t, c, f)
	return orig, pl.Loc
}

func TestReadContinuesFromAVerifiedCopy(t *testing.T) {
	c, seam := newTestCore(t)
	s := mustShare(t, c, 1)
	f := testFile(1)
	data := pattern(4<<10, 10)
	orig, _ := writeWithCopy(t, c, seam, s, f, data)
	flip(t, seam, orig.Record.Segment, payloadStart(orig.Record)+100)

	p := sentinels(4 << 10)
	n, missing, held, _, err := c.ReadAt(context.Background(), s, f, 0, p)
	if err != nil {
		t.Fatalf("ReadAt: %v", err)
	}
	if n != 4<<10 || len(missing) != 0 || !bytes.Equal(p, data) {
		t.Fatalf("n = %d missing %v, bytes equal %v: the copy was not served", n, missing, bytes.Equal(p, data))
	}
	if want := []core.Held{{Extent: core.Extent{Off: 0, Len: 4 << 10}, Version: orig.Version}}; !slices.Equal(held, want) {
		t.Fatalf("held = %v, want %v", held, want)
	}
}

func TestReadCopyThatFailsAfterCorruptIsMissing(t *testing.T) {
	var cp segment.Location
	var seam *kit.Seam
	var c *Core
	c, seam = newTestCore(t, func(o *coreOptions) {
		o.afterCorrupt = func() { flip(t, seam, cp.Segment, cp.Off+format.RecordHeaderSize+8+100) }
	})
	s := mustShare(t, c, 1)
	f := testFile(1)
	var orig index.Placed
	orig, cp = writeWithCopy(t, c, seam, s, f, pattern(4<<10, 11))
	flip(t, seam, orig.Record.Segment, payloadStart(orig.Record)+100)

	readUntouched(t, c, s, f, 0, 4<<10, []core.Extent{{Off: 0, Len: 4 << 10}})
	// Corrupt answered with the copy, so it dropped nothing: the extent is
	// still held, though this read could not verify what it named.
	if _, ok := c.Index().Lookup(f, 0); !ok {
		t.Fatal("the extent was dropped: Corrupt did not answer with the copy")
	}
}
