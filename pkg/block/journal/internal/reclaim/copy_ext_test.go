package reclaim_test

import (
	"bytes"
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/format"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/index"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/reclaim"
	"github.com/cubbit/dittofs/pkg/block/journal/journaltest/kit"
)

// copyKib is the 4 KiB region the corrupt-source tests plant content in.
const copyKib = int64(4 << 10)

// copyPayloadStart is the segment offset of the first payload byte of the
// write record at at: after its header and its modification time.
func copyPayloadStart(at index.RecordAt) int64 {
	return int64(at.Offset) + format.RecordHeaderSize + 8
}

// corruptPlant is the layout the corrupt-source tests copy from: one sealed
// source segment holding one file's A, a 12 KiB write whose middle 4 KiB a
// newer write in a second segment supersedes and the index lifts — so A is
// held at its two ends alone — and B, a clean 4 KiB write after a gap, one
// record that shares A's segment.
type corruptPlant struct {
	file             core.FileID
	seg              core.SegmentID
	recA, recB, recS index.RecordAt
	seqA             core.Seq
	vA               core.Version
	payA, payB       []byte
}

// plantCorruptSource plants and seals the corrupt-source layout and returns
// it with A's identity: the record, the sequence number and the version its
// planting issued, which the corrupt answers are named by.
func (f *copyFixture) plantCorruptSource(t *testing.T) corruptPlant {
	t.Helper()
	p := corruptPlant{file: copyFile(1)}
	p.payA = make([]byte, 3*copyKib)
	for i := range p.payA {
		p.payA[i] = byte(int64(i)>>8) ^ 0xa5
	}
	vA, err := f.counters.Draw()
	if err != nil {
		t.Fatalf("Draw: %v", err)
	}
	p.vA = vA
	p.recA, p.seqA = f.plantAt(t, copyTag, p.file, core.Extent{Len: 3 * copyKib}, p.payA, 12345, vA)
	// B sits after a 4 KiB gap, so it copies as a record of its own whatever
	// A's answer is.
	p.payB = make([]byte, copyKib)
	for i := range p.payB {
		p.payB[i] = byte(int64(i)>>8) ^ 0x3c
	}
	p.recB, _ = f.plantAt(t, copyTag, p.file, core.Extent{Off: 4 * copyKib, Len: copyKib}, p.payB, 999, vA)
	if p.recA.Segment != p.recB.Segment {
		t.Fatalf("A and B landed in segments %d and %d, want one shared source segment", p.recA.Segment, p.recB.Segment)
	}
	p.seg = p.recA.Segment
	f.sealSource(t, p.file)

	// The superseder of A's middle, planted after the seal so its record
	// lands outside the source segment.
	f.waitSpares(t)
	vS, err := f.counters.Draw()
	if err != nil {
		t.Fatalf("Draw: %v", err)
	}
	payS := make([]byte, copyKib)
	for i := range payS {
		payS[i] = byte(int64(i)>>8) ^ 0x5a
	}
	middle := core.Extent{Off: copyKib, Len: copyKib}
	p.recS, _ = f.plantAt(t, copyTag, p.file, middle, payS, 777, vS)
	if p.recS.Segment == p.seg {
		t.Fatalf("the superseder landed in the sealed source segment %d, want a segment of its own", p.seg)
	}
	if _, err := f.ix.LiftSuperseded(p.file, vS, middle); err != nil {
		t.Fatalf("LiftSuperseded: %v", err)
	}
	// The precondition the pass relies on: A is held at its two ends, the
	// middle names only the superseder, and no piece of A is retained.
	var held []core.Extent
	for _, piece := range f.ix.HeldAt(p.file, p.recA) {
		held = append(held, piece.Extent)
	}
	wantEnds := []core.Extent{{Off: 0, Len: copyKib}, {Off: 2 * copyKib, Len: copyKib}}
	if !slices.Equal(held, wantEnds) {
		t.Fatalf("the middle lifted, held extents at A = %v, want the two ends %v", held, wantEnds)
	}
	for _, rt := range f.ix.Retained(p.file) {
		if rt.Record == p.recA {
			t.Fatalf("the middle lifted, want no retained piece of A, got extent %v", rt.Extent)
		}
	}
	return p
}

// assertDroppedWhole checks the answer of one Copy over p's source segment
// with A rotten: A alone is reported corrupt, carrying the identity its
// planting issued, copied nowhere, its extents dropped from the index by the
// corruption handler, while B copied as one byte-exact record published
// nowhere, and the superseder of A's middle stays where its own write put it.
func (f *copyFixture) assertDroppedWhole(t *testing.T, p corruptPlant, res reclaim.Result) {
	t.Helper()
	if len(res.Corrupt) != 1 {
		t.Fatalf("Copy reported %d corrupt sources (%+v), want A alone", len(res.Corrupt), res.Corrupt)
	}
	bad := res.Corrupt[0]
	if bad.File != p.file || bad.Record != p.recA {
		t.Fatalf("the corrupt source is file %v record %v, want A of file %v at %v", bad.File, bad.Record, p.file, p.recA)
	}
	if bad.Seq != p.seqA || bad.Version != p.vA || bad.Extent != (core.Extent{Len: 3 * copyKib}) {
		t.Fatalf("the corrupt source's identity = seq %d version %v extent %v, want A's seq %d version %v extent [0,%d)", bad.Seq, bad.Version, bad.Extent, p.seqA, p.vA, 3*copyKib)
	}
	if len(res.Copies) != 1 {
		t.Fatalf("Copy appended %d copies (%+v), want B alone", len(res.Copies), res.Copies)
	}
	cp := res.Copies[0]
	if cp.From != p.recB {
		t.Fatalf("the one copy is of record %v, want B at %v", cp.From, p.recB)
	}
	if cp.Extent != (core.Extent{Off: 4 * copyKib, Len: copyKib}) {
		t.Fatalf("B's copy covers extent %v, want [16384,20480)", cp.Extent)
	}
	h, body := f.readBack(t, cp.To)
	if h.Version != p.vA || !bytes.Equal(body.Payload, p.payB) {
		t.Fatalf("B's copy carries version %v and %d bytes, want version %v and B's bytes whole", h.Version, len(body.Payload), p.vA)
	}
	// A's two ends no longer read held: the corruption handler dropped them.
	if _, ok := f.ix.Lookup(p.file, 0); ok {
		t.Fatal("A's first end is still held at 0: the corruption handler dropped nothing")
	}
	if _, ok := f.ix.Lookup(p.file, 2*copyKib+100); ok {
		t.Fatalf("A's second end is still held at %d: the corruption handler dropped nothing", 2*copyKib+100)
	}
	// The neighbours keep their own content where its own write put it: the
	// pass publishes nothing.
	if s, ok := f.ix.Lookup(p.file, copyKib+100); !ok || s.Record != p.recS {
		t.Fatalf("the middle after Copy = %v %v, want the superseder held at %v", s, ok, p.recS)
	}
	if b, ok := f.ix.Lookup(p.file, 4*copyKib+100); !ok || b.Record != p.recB {
		t.Fatalf("B after Copy = %v %v, want B still at its own record %v: the pass publishes nothing", b, ok, p.recB)
	}
	// A's held bytes sit in no copy target.
	raw := f.segmentRaw(t, cp.To.Segment)
	if at := bytes.Index(raw, p.payA[:copyKib]); at >= 0 {
		t.Fatalf("A's first end sits in copy target segment %d at %d, want A copied nowhere", cp.To.Segment, at)
	}
	if at := bytes.Index(raw, p.payA[2*copyKib:]); at >= 0 {
		t.Fatalf("A's second end sits in copy target segment %d at %d, want A copied nowhere", cp.To.Segment, at)
	}
}

// TestCopyCorruptWholeSource copies a source segment holding one file's two
// records, A and B, with a byte of A's payload rotten. The whole-payload
// verify finds the byte wherever it sits — inside a held end, and in the
// third case strictly inside the middle the superseder released, bytes no
// copy of A would owe at all (RFC 28) — so A drops whole: reported as the one
// corrupt source, copied nowhere, its extents dropped from the index, while
// B, its clean neighbour in the same segment, copies normally.
func TestCopyCorruptWholeSource(t *testing.T) {
	for _, tc := range []struct {
		name string
		at   func(p corruptPlant) int64
	}{
		{name: "a byte of a held end", at: func(p corruptPlant) int64 {
			return copyPayloadStart(p.recA) + 100
		}},
		{name: "a byte strictly inside the released middle", at: func(p corruptPlant) int64 {
			return copyPayloadStart(p.recA) + copyKib + copyKib/2
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newCopyFixture(t)
			p := f.plantCorruptSource(t)
			f.flipByte(t, p.recA, tc.at(p))
			f.waitSpares(t)
			cs := f.selector().Select(reclaim.Scope{Segments: []core.SegmentID{p.seg}}, 1)
			if len(cs) != 1 || cs[0].ID != p.seg {
				t.Fatalf("Select returned %v, want the one candidate %d", cs, p.seg)
			}
			res, err := f.copier().Copy(context.Background(), p.seg)
			if err != nil {
				t.Fatalf("Copy over a corrupt source: %v", err)
			}
			f.assertDroppedWhole(t, p, res)
		})
	}
}

// TestCopyCorruptUndecodableHeader copies the same layout with a byte of A's
// header rotten instead. The header's bytes were read and do not decode,
// which is a verify failure and not an unread read, so the unit does not
// abandon: A drops whole like a source whose body checksum fails, named — its
// header giving no identity — by the pieces the index holds at its record,
// and B, its clean neighbour, copies normally.
func TestCopyCorruptUndecodableHeader(t *testing.T) {
	f := newCopyFixture(t)
	p := f.plantCorruptSource(t)
	f.flipByte(t, p.recA, int64(p.recA.Offset)+50)
	f.waitSpares(t)
	cs := f.selector().Select(reclaim.Scope{Segments: []core.SegmentID{p.seg}}, 1)
	if len(cs) != 1 || cs[0].ID != p.seg {
		t.Fatalf("Select returned %v, want the one candidate %d", cs, p.seg)
	}
	res, err := f.copier().Copy(context.Background(), p.seg)
	if err != nil {
		t.Fatalf("Copy over a source whose header does not decode: %v", err)
	}
	f.assertDroppedWhole(t, p, res)
}

// TestCopyWholeRecord copies a segment holding one held write record and
// checks the copy the seam ends up holding: a fresh record, never in its own
// target segment, carrying the source's version, mtime and bytes whole.
func TestCopyWholeRecord(t *testing.T) {
	f := newCopyFixture(t)
	file := copyFile(1)
	payload := make([]byte, 4<<10)
	for i := range payload {
		payload[i] = byte(i)
	}
	const mtime = 12345
	rec, seq, v := f.plant(t, copyTag, file, payload, mtime)
	f.sealSource(t, file)

	cs := f.selector().Select(reclaim.Scope{}, 1)
	if len(cs) != 1 {
		t.Fatalf("Select returned %d candidates, want 1", len(cs))
	}
	if cs[0].ID != rec.Segment {
		t.Fatalf("candidate = segment %d, want the source %d", cs[0].ID, rec.Segment)
	}

	source := format.SegmentName(rec.Segment)
	writes := f.seam.CallsFor(kit.OpWrite, source)
	res, err := f.copier().Copy(context.Background(), rec.Segment)
	if err != nil {
		t.Fatalf("Copy: %v", err)
	}
	if len(res.Copies) != 1 {
		t.Fatalf("Copy appended %d copies, want 1", len(res.Copies))
	}
	cp := res.Copies[0]
	if cp.Seq <= seq {
		t.Fatalf("copy Seq = %d, want a fresh number above the source's %d", cp.Seq, seq)
	}
	if cp.Version != v {
		t.Fatalf("copy Version = %v, want the source's %v", cp.Version, v)
	}
	if cp.To.Segment == rec.Segment {
		t.Fatalf("copy landed in its own target segment %d", cp.To.Segment)
	}
	if cp.From != rec {
		t.Fatalf("copy From = %v, want the source record %v", cp.From, rec)
	}
	if cp.Loc.Segment != cp.To.Segment || cp.Loc.Off != int64(cp.To.Offset) {
		t.Fatalf("copy Loc = %v, want the placement its To names (segment %d offset %d)", cp.Loc, cp.To.Segment, cp.To.Offset)
	}
	h, body := f.readBack(t, cp.To)
	if cp.Loc.Len != h.Size() {
		t.Fatalf("copy Loc.Len = %d, want the whole record's size %d", cp.Loc.Len, h.Size())
	}
	if h.Kind != format.KindWrite {
		t.Fatalf("copy kind = %s, want %s", h.Kind, format.KindWrite)
	}
	if body.MTime != mtime {
		t.Fatalf("copy MTime = %d, want the source's %d", body.MTime, mtime)
	}
	if !bytes.Equal(body.Payload, payload) {
		t.Fatalf("copy payload of %d bytes, want the source's bytes whole", len(body.Payload))
	}
	if h.File != file {
		t.Fatalf("copy File = %v, want the source's %v", h.File, file)
	}
	if h.Tag != copyTag {
		t.Fatalf("copy Tag = %d, want the file's owner %d", h.Tag, copyTag)
	}
	if h.Version != v {
		t.Fatalf("record Version = %v, want the source's %v", h.Version, v)
	}
	if h.Seq != cp.Seq {
		t.Fatalf("record Seq = %d, want the fresh %d the pass drew", h.Seq, cp.Seq)
	}
	if f.ix.Names(file, cp.To) {
		t.Fatal("the index names the copy: a pass publishes nothing")
	}
	if after := f.seam.CallsFor(kit.OpWrite, source); after != writes {
		t.Fatalf("%d writes to the source segment after Copy, want the %d before: the original stays whole", after, writes)
	}
}

// TestCopyCancelledBeforeStart holds a cancelled context before the pass
// begins and checks the answer: the context's error, nothing appended.
func TestCopyCancelledBeforeStart(t *testing.T) {
	f := newCopyFixture(t)
	file := copyFile(1)
	rec, _, _ := f.plant(t, copyTag, file, []byte("payload bytes"), 1)
	f.sealSource(t, file)

	cs := f.selector().Select(reclaim.Scope{}, 1)
	if len(cs) != 1 {
		t.Fatalf("Select returned %d candidates, want 1", len(cs))
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	res, err := f.copier().Copy(ctx, rec.Segment)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Copy error = %v, want %v", err, context.Canceled)
	}
	if len(res.Copies) != 0 {
		t.Fatalf("Copy appended %d copies on a cancelled context, want none", len(res.Copies))
	}
}

// TestCopySupersededMiddle copies a source segment holding two files' live
// content. One file holds a 12 KiB write whose middle 4 KiB a newer write in
// another segment supersedes — and, its superseder settled, the middle names
// nothing: the middle's bytes must never be written to any copy target, and
// the two ends copy as two records with the gap between. The other file
// holds two adjacent 4 KiB writes of one version in two records of the
// segment, which copy as one record carrying the payloads in offset order.
func TestCopySupersededMiddle(t *testing.T) {
	f := newCopyFixture(t)
	const kib = int64(4 << 10)

	// One 12 KiB write, patterned so every 4 KiB region is its own bytes.
	file1 := copyFile(1)
	const mtime1 = 12345
	pay1 := make([]byte, 3*kib)
	for i := range pay1 {
		pay1[i] = byte(int64(i)>>8) ^ 0xa5
	}
	rec1, _, v1 := f.plant(t, copyTag, file1, pay1, mtime1)
	seg1 := rec1.Segment
	f.sealSource(t, file1)
	f.waitSpares(t)

	// The superseding middle write, planted after the seal so its record
	// lands in a second segment the pass does not name.
	vS, err := f.counters.Draw()
	if err != nil {
		t.Fatalf("Draw: %v", err)
	}
	const mtimeS = 777
	payS := make([]byte, kib)
	for i := range payS {
		payS[i] = byte(int64(i)>>8) ^ 0x5a
	}
	middle := core.Extent{Off: kib, Len: kib}
	f.plantAt(t, copyTag, file1, middle, payS, mtimeS, vS)

	// The superseder is synced, so the middle it superseded names nothing.
	if _, err := f.ix.LiftSuperseded(file1, vS, middle); err != nil {
		t.Fatalf("LiftSuperseded: %v", err)
	}
	// The precondition the pass relies on: the middle backs neither a held
	// nor a retained piece of the source record, so no copy owes it.
	var held []core.Extent
	for _, p := range f.ix.HeldAt(file1, rec1) {
		held = append(held, p.Extent)
	}
	wantEnds := []core.Extent{{Off: 0, Len: kib}, {Off: 2 * kib, Len: kib}}
	if !slices.Equal(held, wantEnds) {
		t.Fatalf("the middle lifted, held extents at the source = %v, want the two ends %v", held, wantEnds)
	}
	for _, rt := range f.ix.Retained(file1) {
		if rt.Record == rec1 {
			t.Fatalf("the middle lifted, want no retained piece of the source record, got extent %v", rt.Extent)
		}
	}

	// Two adjacent 4 KiB writes of one version, one per record, held as a
	// second file's content — the copy must coalesce them across the record
	// seam into one record, payloads in file order.
	file2 := copyFile(2)
	const mtime2 = 999
	pay2 := make([]byte, 2*kib)
	for i := range pay2 {
		pay2[i] = byte(int64(i)>>8) ^ 0x3c
	}
	v2, err := f.counters.Draw()
	if err != nil {
		t.Fatalf("Draw: %v", err)
	}
	f.waitSpares(t)
	rec2a, _ := f.plantAt(t, copyTag, file2, core.Extent{Len: kib}, pay2[:kib], mtime2, v2)
	rec2b, _ := f.plantAt(t, copyTag, file2, core.Extent{Off: kib, Len: kib}, pay2[kib:], mtime2, v2)
	if rec2a.Segment != rec2b.Segment {
		t.Fatalf("the two records of one version landed in segments %d and %d, want a shared segment", rec2a.Segment, rec2b.Segment)
	}
	f.sealSource(t, file2)

	// The ends' segment: two copies, the middle written nowhere.
	f.waitSpares(t)
	source1 := format.SegmentName(seg1)
	cover1 := f.seam.CallsFor(kit.OpWrite, source1)
	cs := f.selector().Select(reclaim.Scope{Segments: []core.SegmentID{seg1}}, 1)
	if len(cs) != 1 || cs[0].ID != seg1 {
		t.Fatalf("Select over the ends' segment returned %v, want the one candidate %d", cs, seg1)
	}
	res1, err := f.copier().Copy(context.Background(), seg1)
	if err != nil {
		t.Fatalf("Copy of the ends' segment: %v", err)
	}
	if len(res1.Copies) != 2 {
		t.Fatalf("Copy appended %d copies for the two live ends, want 2", len(res1.Copies))
	}
	for _, cp := range res1.Copies {
		if cp.File != file1 || cp.From != rec1 {
			t.Fatalf("copy %+v of the ends' segment, want a copy of the source record %v of file %v", cp, rec1, file1)
		}
		if cp.Extent.Len != kib {
			t.Fatalf("copy extent %v of file %v, want one 4 KiB end", cp.Extent, file1)
		}
		if cp.To.Segment == seg1 {
			t.Fatalf("copy landed in its own target segment %d", cp.To.Segment)
		}
		want := pay1[cp.Extent.Off : cp.Extent.Off+cp.Extent.Len]
		h, body := f.readBack(t, cp.To)
		if h.Kind != format.KindWrite || h.Version != v1 || body.MTime != mtime1 {
			t.Fatalf("copy of extent %v: kind %s version %v mtime %d, want %s %v %d", cp.Extent, h.Kind, h.Version, body.MTime, format.KindWrite, v1, mtime1)
		}
		if !bytes.Equal(body.Payload, want) {
			t.Fatalf("copy of extent %v: payload of the end, want the source's bytes byte-exact", cp.Extent)
		}
	}
	if got := f.seam.CallsFor(kit.OpWrite, source1); got != cover1 {
		t.Fatalf("%d writes to the ends' segment after its Copy, want the %d before: the original stays whole", got, cover1)
	}

	// The two-record segment: one coalesced copy, both payloads in order.
	f.waitSpares(t)
	seg2 := rec2a.Segment
	cover2 := f.seam.CallsFor(kit.OpWrite, format.SegmentName(seg2))
	cs2 := f.selector().Select(reclaim.Scope{Segments: []core.SegmentID{seg2}}, 1)
	if len(cs2) != 1 || cs2[0].ID != seg2 {
		t.Fatalf("Select over the two-record segment returned %v, want the one candidate %d", cs2, seg2)
	}
	res2, err := f.copier().Copy(context.Background(), seg2)
	if err != nil {
		t.Fatalf("Copy of the two-record segment: %v", err)
	}
	var merged []reclaim.Copied
	for _, cp := range res2.Copies {
		if cp.File == file2 {
			merged = append(merged, cp)
		}
	}
	if len(merged) != 1 {
		t.Fatalf("Copy of the two-record segment made %d copies of the coalescing file, want 1", len(merged))
	}
	cp := merged[0]
	if cp.Extent != (core.Extent{Len: 2 * kib}) || cp.From != rec2a {
		t.Fatalf("the coalesced copy = extent %v from %v, want the whole [%d %d] from the first record %v", cp.Extent, cp.From, 0, 2*kib, rec2a)
	}
	h, body := f.readBack(t, cp.To)
	if h.Kind != format.KindWrite || h.Version != v2 || body.MTime != mtime2 {
		t.Fatalf("the coalesced copy: kind %s version %v mtime %d, want %s %v %d", h.Kind, h.Version, body.MTime, format.KindWrite, v2, mtime2)
	}
	if !bytes.Equal(body.Payload, pay2) {
		t.Fatalf("the coalesced copy's payload of %d bytes, want both records' payloads in offset order", len(pay2))
	}
	if got := f.seam.CallsFor(kit.OpWrite, format.SegmentName(seg2)); got != cover2 {
		t.Fatalf("%d writes to the two-record segment after its Copy, want the %d before: the original stays whole", got, cover2)
	}

	// The superseded middle, byte-exact, was written to no copy target.
	targets := map[core.SegmentID]struct{}{}
	for _, cp := range res1.Copies {
		targets[cp.To.Segment] = struct{}{}
	}
	for _, cp := range res2.Copies {
		targets[cp.To.Segment] = struct{}{}
	}
	oldMiddle := pay1[middle.Off : middle.Off+middle.Len]
	for seg := range targets {
		file, err := f.seam.Open(format.SegmentName(seg))
		if err != nil {
			t.Fatalf("Open %s: %v", format.SegmentName(seg), err)
		}
		size, err := file.Size()
		if err != nil {
			t.Fatalf("Size of %s: %v", format.SegmentName(seg), err)
		}
		raw := make([]byte, size)
		if _, err := file.ReadAt(raw, 0); err != nil {
			t.Fatalf("reading %s: %v", format.SegmentName(seg), err)
		}
		if err := file.Close(); err != nil {
			t.Fatalf("Close %s: %v", format.SegmentName(seg), err)
		}
		if at := bytes.Index(raw, oldMiddle); at >= 0 {
			t.Fatalf("the superseded middle's bytes sit in copy target segment %v at %d, want them nowhere", seg, at)
		}
	}
}
