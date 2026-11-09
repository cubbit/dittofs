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

// forgeHeader rewrites the header of the record at r as h, re-encoded with
// both checksums valid over body, and checks that the forged record decodes
// and verifies on its own at r's place.
func forgeHeader(t *testing.T, seam *kit.Seam, r index.RecordAt, h format.RecordHeader, body format.RecordBody) {
	t.Helper()
	h.Segment, h.Offset = r.Segment, int64(r.Offset)
	forged := format.AppendRecord(nil, testIdentity, h, body)
	overwrite(t, seam, r.Segment, int64(r.Offset), forged[:format.RecordHeaderSize])
	whole := segmentBytes(t, seam, r.Segment, int64(r.Offset), int(h.Size()))
	fh, err := format.DecodeRecordHeader(whole[:format.RecordHeaderSize], testIdentity, r.Segment, int64(r.Offset))
	if err != nil {
		t.Fatalf("the forged header does not decode: %v", err)
	}
	if _, err := format.VerifyRecordBody(whole[format.RecordHeaderSize:format.RecordHeaderSize+fh.BodySize()], testIdentity, fh); err != nil {
		t.Fatalf("the forged record does not verify: %v", err)
	}
}

// headerAt decodes the header of the record at r.
func headerAt(t *testing.T, seam *kit.Seam, r index.RecordAt) format.RecordHeader {
	t.Helper()
	raw := segmentBytes(t, seam, r.Segment, int64(r.Offset), format.RecordHeaderSize)
	h, err := format.DecodeRecordHeader(raw, testIdentity, r.Segment, int64(r.Offset))
	if err != nil {
		t.Fatalf("DecodeRecordHeader: %v", err)
	}
	return h
}

func TestReadHeaderForAnotherExtentIsCorruption(t *testing.T) {
	c, seam := newTestCore(t)
	s := mustShare(t, c, 1)
	f := testFile(1)
	data := pattern(4<<10, 6)
	if _, err := c.WriteAt(context.Background(), s, f, 0, data, 1); err != nil {
		t.Fatal(err)
	}
	syncFiles(t, c, f)
	r := recordOf(t, c, f, 0).Record

	// The header is rewritten to name another extent of the same file and
	// version, with both checksums valid: the record verifies on its own, and
	// only the index entry that sent the read there tells it is wrong. It is
	// still f's record, so its sequence number is trusted and Corrupt drops it.
	h := headerAt(t, seam, r)
	h.Extent.Off += 8 << 10
	forgeHeader(t, seam, r, h, format.RecordBody{Payload: data, MTime: 1})

	readUntouched(t, c, s, f, 0, 4<<10, []core.Extent{{Off: 0, Len: 4 << 10}})
	if _, ok := c.Index().Lookup(f, 0); ok {
		t.Fatal("the record whose header names another extent is still held")
	}
}

func TestReadHeaderOfAnotherFilesRecordIsRefusedNotRepointed(t *testing.T) {
	c, seam := newTestCore(t)
	s := mustShare(t, c, 1)
	f, g := testFile(1), testFile(2)
	gdata := pattern(4<<10, 12)
	// g's record has a registered copy, so a Corrupt that took g's sequence
	// number would find that copy and serve or repoint f's entries to it.
	gp, _ := writeWithCopy(t, c, seam, s, g, gdata)
	// f holds the same bytes, so g's header verifies over f's body too.
	if _, err := c.WriteAt(context.Background(), s, f, 0, gdata, 1); err != nil {
		t.Fatal(err)
	}
	syncFiles(t, c, f)
	before := recordOf(t, c, f, 0)

	// f's record now carries g's header, valid at f's place.
	forgeHeader(t, seam, before.Record, headerAt(t, seam, gp.Record), format.RecordBody{Payload: gdata, MTime: 1})

	p := sentinels(4 << 10)
	n, missing, held, _, err := c.ReadAt(context.Background(), s, f, 0, p)
	if !errors.Is(err, core.ErrCorrupt) {
		t.Fatalf("err = %v, want core.ErrCorrupt", err)
	}
	if n != 0 || len(held) != 0 || len(missing) != 0 || !bytes.Equal(p, sentinels(4<<10)) {
		t.Fatalf("the refused read reports %d bytes, held %v, missing %v", n, held, missing)
	}
	if after := recordOf(t, c, f, 0); after != before || !c.Index().Names(f, before.Record) {
		t.Fatalf("f's entry changed: %+v, was %+v", after, before)
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

// corruptPayload flips a payload byte of the record holding off of f.
func corruptPayload(t *testing.T, c *Core, seam *kit.Seam, f core.FileID, off int64) {
	t.Helper()
	r := recordOf(t, c, f, off).Record
	flip(t, seam, r.Segment, payloadStart(r)+10)
}

func TestReadDroppedPiecesJoinNeighbouringGaps(t *testing.T) {
	c, seam := newTestCore(t)
	s := mustShare(t, c, 1)
	f := testFile(1)
	for i, off := range []int64{100, 200} {
		if _, err := c.WriteAt(context.Background(), s, f, off, pattern(100, byte(i)), 1); err != nil {
			t.Fatal(err)
		}
	}
	syncFiles(t, c, f)
	if recordOf(t, c, f, 100).Record == recordOf(t, c, f, 200).Record {
		t.Fatal("the two writes share a record")
	}
	corruptPayload(t, c, seam, f, 100)
	corruptPayload(t, c, seam, f, 200)

	// A gap, two dropped records, a gap: one exact extent, no wider.
	readUntouched(t, c, s, f, 50, 300, []core.Extent{{Off: 50, Len: 300}})
}

func TestReadDroppedRecordBetweenHeldPieces(t *testing.T) {
	c, seam := newTestCore(t)
	s := mustShare(t, c, 1)
	f := testFile(1)
	var vs []core.Version
	for i, off := range []int64{0, 100, 200} {
		v, err := c.WriteAt(context.Background(), s, f, off, pattern(100, byte(i)), 1)
		if err != nil {
			t.Fatal(err)
		}
		vs = append(vs, v)
	}
	syncFiles(t, c, f)
	corruptPayload(t, c, seam, f, 100)

	p := sentinels(300)
	n, missing, held, _, err := c.ReadAt(context.Background(), s, f, 0, p)
	if err != nil {
		t.Fatalf("ReadAt: %v", err)
	}
	if want := []core.Extent{{Off: 100, Len: 100}}; !slices.Equal(missing, want) {
		t.Fatalf("missing = %v, want %v", missing, want)
	}
	wantHeld := []core.Held{
		{Extent: core.Extent{Off: 0, Len: 100}, Version: vs[0]},
		{Extent: core.Extent{Off: 200, Len: 100}, Version: vs[2]},
	}
	if n != 200 || !slices.Equal(held, wantHeld) {
		t.Fatalf("n = %d held %v, want 200 and %v", n, held, wantHeld)
	}
	if !bytes.Equal(p[:100], pattern(100, 0)) || !bytes.Equal(p[200:], pattern(100, 2)) || !bytes.Equal(p[100:200], sentinels(100)) {
		t.Fatal("p does not hold the two held pieces around the untouched dropped one")
	}
}

// forgeHeaderOnly rewrites the header of the record at r as h, re-encoded with
// a valid header checksum over a body of the right size, and checks that the
// header decodes at r's place. The record's body on disk is left as it was.
func forgeHeaderOnly(t *testing.T, seam *kit.Seam, r index.RecordAt, h format.RecordHeader, body format.RecordBody) {
	t.Helper()
	h.Segment, h.Offset = r.Segment, int64(r.Offset)
	forged := format.AppendRecord(nil, testIdentity, h, body)
	overwrite(t, seam, r.Segment, int64(r.Offset), forged[:format.RecordHeaderSize])
	if _, err := format.DecodeRecordHeader(segmentBytes(t, seam, r.Segment, int64(r.Offset), format.RecordHeaderSize), testIdentity, r.Segment, int64(r.Offset)); err != nil {
		t.Fatalf("the forged header does not decode: %v", err)
	}
}

func TestReadHeaderOverrunningItsSegmentIsDamaged(t *testing.T) {
	for _, synced := range []bool{true, false} {
		name := "unsynced"
		if synced {
			name = "synced"
		}
		t.Run(name, func(t *testing.T) {
			c, seam := newTestCore(t)
			s := mustShare(t, c, 1)
			f := testFile(1)
			if !synced {
				// The ledger keeps the record, so its sequence number is known.
				seam.Hold(t, kit.OpSync, "*")
			}
			if _, err := c.WriteAt(context.Background(), s, f, 0, pattern(4<<10, 14), 1); err != nil {
				t.Fatal(err)
			}
			if synced {
				syncFiles(t, c, f)
			}
			before := recordOf(t, c, f, 0)

			// A header of this file and version, valid under its checksum, whose
			// length runs the record past the end of its segment.
			h := headerAt(t, seam, before.Record)
			h.Extent.Len = testSegmentCap
			forgeHeaderOnly(t, seam, before.Record, h, format.RecordBody{Payload: make([]byte, testSegmentCap), MTime: 1})

			p := sentinels(4 << 10)
			n, missing, held, _, err := c.ReadAt(context.Background(), s, f, 0, p)
			if n != 0 || len(held) != 0 || !bytes.Equal(p, sentinels(4<<10)) {
				t.Fatalf("the read served %d bytes, held %v, of a header that overruns its segment", n, held)
			}
			if synced {
				if !errors.Is(err, core.ErrCorrupt) || len(missing) != 0 {
					t.Fatalf("err = %v, missing %v: want the refusal of a damaged header with no known sequence number", err, missing)
				}
				if after := recordOf(t, c, f, 0); after != before {
					t.Fatalf("the index changed: %+v, was %+v", after, before)
				}
				return
			}
			if err != nil {
				t.Fatalf("ReadAt: %v", err)
			}
			if want := []core.Extent{{Off: 0, Len: 4 << 10}}; !slices.Equal(missing, want) {
				t.Fatalf("missing = %v, want %v", missing, want)
			}
			if _, ok := c.Index().Lookup(f, 0); ok {
				t.Fatal("the record whose header overruns its segment is still held")
			}
		})
	}
}
