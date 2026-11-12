package reclaim_test

import (
	"bytes"
	"context"
	"errors"
	"slices"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/format"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/index"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/reclaim"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/unread"
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

// TestCopyCorruptHeaderNoPieceLeft copies a source segment holding one
// file's two records, A and B, with a byte of A's header rotten — bytes read
// but not decodable, the corrupt path — while every piece of A is lifted
// from the index between the pass's unheld first phase and its domain-held
// second, superseded whole by a record of a newer version. A record the
// index holds no piece of any more has nothing left to drop: it reaches no
// corruption handler, no corruption is reported, nothing is marked unread
// and no loss is recorded, while B, its clean neighbour, copies normally.
func TestCopyCorruptHeaderNoPieceLeft(t *testing.T) {
	f := newCopyFixture(t)
	file := copyFile(1)
	vA, err := f.counters.Draw()
	if err != nil {
		t.Fatalf("Draw: %v", err)
	}
	payA := make([]byte, copyKib)
	for i := range payA {
		payA[i] = byte(int64(i)>>8) ^ 0xa5
	}
	recA, _ := f.plantAt(t, copyTag, file, core.Extent{Len: copyKib}, payA, 12345, vA)
	payB := make([]byte, copyKib)
	for i := range payB {
		payB[i] = byte(int64(i)>>8) ^ 0x3c
	}
	recB, _ := f.plantAt(t, copyTag, file, core.Extent{Off: 2 * copyKib, Len: copyKib}, payB, 999, vA)
	if recA.Segment != recB.Segment {
		t.Fatalf("A and B landed in segments %d and %d, want one shared source segment", recA.Segment, recB.Segment)
	}
	seg := recA.Segment
	f.sealSource(t, file)

	// The superseder of A's whole extent, planted after the seal so its
	// record lands outside the source segment.
	f.waitSpares(t)
	vS, err := f.counters.Draw()
	if err != nil {
		t.Fatalf("Draw: %v", err)
	}
	payS := make([]byte, copyKib)
	for i := range payS {
		payS[i] = byte(int64(i)>>8) ^ 0x5a
	}
	recS, _ := f.plantAt(t, copyTag, file, core.Extent{Len: copyKib}, payS, 777, vS)
	if recS.Segment == seg {
		t.Fatalf("the superseder landed in the sealed source segment %d, want a segment of its own", seg)
	}

	// A's header rots: its bytes will read but not decode.
	f.flipByte(t, recA, int64(recA.Offset)+50)

	cs := f.selector().Select(reclaim.Scope{Segments: []core.SegmentID{seg}}, 1)
	if len(cs) != 1 || cs[0].ID != seg {
		t.Fatalf("Select returned %v, want the one candidate %d", cs, seg)
	}

	// The pass enumerates A with its pieces held and then reads it: its
	// first source read is held at the seam, and the lift of every piece of
	// A lands in the window the hold stages between the two phases.
	h := f.seam.HoldFirst(t, kit.OpRead, format.SegmentName(seg))
	type copyAnswer struct {
		res reclaim.Result
		err error
	}
	done := make(chan copyAnswer, 1)
	go func() {
		res, err := f.copier().Copy(context.Background(), seg)
		done <- copyAnswer{res, err}
	}()
	select {
	case <-h.Reached():
	case <-time.After(copyWait):
		h.Release()
		t.Fatalf("the copy never reached A's source read within %v, want it held", copyWait)
	}
	if _, err := f.ix.LiftSuperseded(file, vS, core.Extent{Len: copyKib}); err != nil {
		h.Release()
		t.Fatalf("LiftSuperseded of A's whole extent: %v", err)
	}
	// The precondition the rule relies on: no piece of A survives the lift.
	if got := f.ix.HeldAt(file, recA); len(got) != 0 {
		h.Release()
		t.Fatalf("A still holds %d pieces after the lift, want none", len(got))
	}
	for _, rt := range f.ix.Retained(file) {
		if rt.Record == recA {
			h.Release()
			t.Fatalf("a retained piece of A survived the lift at extent %v, want none", rt.Extent)
		}
	}
	h.Release()

	// A bounded wait, the assertion backstop: everything the copy waits on
	// here was released, so it has no reason to outstay it.
	var res reclaim.Result
	select {
	case a := <-done:
		if a.err != nil {
			t.Fatalf("Copy over a header-rotten record whose pieces lifted: %v", a.err)
		}
		if len(a.res.Corrupt) != 0 {
			t.Fatalf("Copy reported %d corrupt sources (%+v), want none: the record's pieces raced away and nothing is left to drop", len(a.res.Corrupt), a.res.Corrupt)
		}
		res = a.res
	case <-time.After(copyWait):
		t.Fatalf("Copy did not finish within %v after the lift, want it abandoned", copyWait)
	}
	if len(res.Copies) != 1 {
		t.Fatalf("Copy appended %d copies (%+v), want B alone", len(res.Copies), res.Copies)
	}
	cp := res.Copies[0]
	if cp.From != recB {
		t.Fatalf("the one copy is of record %v, want B at %v", cp.From, recB)
	}
	_, body := f.readBack(t, cp.To)
	if !bytes.Equal(body.Payload, payB) {
		t.Fatalf("B's copy carries %d bytes, want B's bytes whole", len(body.Payload))
	}
	// Bytes read but undecodable are a verify failure, never an unread
	// mark: nothing sits in the unread set.
	if got := f.un.SuspectSegments(); got != 0 {
		t.Fatalf("SuspectSegments = %d after Copy, want 0: no read returned no bytes", got)
	}
	if got := f.core.Durability().Stats().DamagedSegments; got != 0 {
		t.Fatalf("DamagedSegments = %d after Copy, want 0: a record with no piece left reached no corruption handler", got)
	}
	if got := f.countLossRecords(t); got != 0 {
		t.Fatalf("%d loss records on the seam after Copy, want 0: nothing was dropped", got)
	}
}

// TestCopySourceEIOSuspends copies a source segment holding one file's two
// records, B and A, with a schedule failing A's three read attempts with EIO.
// An I/O error is not a checksum failure: the unread source suspends the
// whole unit and reaches no corruption handler — B, read and verified before
// A's reads fail, is appended nowhere either — while the index keeps both
// records' extents and the unread set keeps A's range. The recovery then
// reads A again through the pass's own read — the bound for a record already
// unread is one attempt — and clears the set, and a second Copy over the
// same claimed segment copies both A and B whole.
func TestCopySourceEIOSuspends(t *testing.T) {
	f := newCopyFixture(t)
	file := copyFile(1)
	vA, err := f.counters.Draw()
	if err != nil {
		t.Fatalf("Draw: %v", err)
	}
	// B sits at the head, so it is read and verified before A and the unit
	// holds one good source when A's reads fail. A sits after a gap, so it
	// copies as a record of its own.
	payB := make([]byte, copyKib)
	for i := range payB {
		payB[i] = byte(int64(i)>>8) ^ 0x3c
	}
	recB, _ := f.plantAt(t, copyTag, file, core.Extent{Len: copyKib}, payB, 999, vA)
	payA := make([]byte, copyKib)
	for i := range payA {
		payA[i] = byte(int64(i)>>8) ^ 0xa5
	}
	recA, _ := f.plantAt(t, copyTag, file, core.Extent{Off: 2 * copyKib, Len: copyKib}, payA, 12345, vA)
	if recA.Segment != recB.Segment {
		t.Fatalf("A and B landed in segments %d and %d, want one shared source segment", recA.Segment, recB.Segment)
	}
	seg := recA.Segment
	f.sealSource(t, file)
	f.waitSpares(t)

	cs := f.selector().Select(reclaim.Scope{Segments: []core.SegmentID{seg}}, 1)
	if len(cs) != 1 || cs[0].ID != seg {
		t.Fatalf("Select returned %v, want the one candidate %d", cs, seg)
	}
	// The schedule counts from its own install, so B's header and body read
	// clean as the first two matching calls and A's three attempts are
	// calls three to five.
	if err := f.seam.Schedule(kit.FailingFrom(kit.OpRead, format.SegmentName(seg), 3, 3, syscall.EIO)); err != nil {
		t.Fatalf("Schedule: %v", err)
	}
	writes := f.seam.Calls(kit.OpWrite)

	res, err := f.copier().Copy(context.Background(), seg)
	if !errors.Is(err, core.ErrUnreadable) {
		t.Fatalf("Copy error = %v, want one wrapping %v", err, core.ErrUnreadable)
	}
	if len(res.Copies) != 0 || len(res.Corrupt) != 0 {
		t.Fatalf("Copy result %+v over an unread source, want empty: the unit suspends whole, B's verified copy appended nowhere", res)
	}
	if got := f.seam.Calls(kit.OpWrite); got != writes {
		t.Fatalf("%d writes on the seam after the suspended Copy, want the %d before: the abandoned unit appended no copy at all", got, writes)
	}
	// The reads the suspended Copy spent on the source segment, taken before
	// any later check reads the seam's bytes back.
	reads := f.seam.CallsFor(kit.OpRead, format.SegmentName(seg))
	// The index still holds both records' extents whole.
	for _, look := range []struct {
		name string
		off  int64
		rec  index.RecordAt
	}{
		{"B", 100, recB},
		{"A", 2*copyKib + 100, recA},
	} {
		if p, ok := f.ix.Lookup(file, look.off); !ok || p.Record != look.rec {
			t.Fatalf("%s at file offset %d = %v %v after the suspended Copy, want its own record %v", look.name, look.off, p, ok, look.rec)
		}
	}
	ref := func(rec index.RecordAt) unread.Ref {
		return unread.Ref{
			SegmentRange: core.SegmentRange{Segment: seg, Off: int64(rec.Offset), Len: format.RecordHeaderSize},
			Kind:         unread.Content,
			File:         file,
		}
	}
	if !f.un.Unread(ref(recA)) {
		t.Fatal("A is not marked unread: a read that returned no bytes marks its range")
	}
	if f.un.Unread(ref(recB)) {
		t.Fatal("B is marked unread, want nothing marked of a record that read whole")
	}
	if !f.un.Suspect(seg) {
		t.Fatalf("segment %d is not suspect, want its unread range keeping it so", seg)
	}
	if got := f.un.SuspectSegments(); got != 1 {
		t.Fatalf("SuspectSegments = %d after the suspended Copy, want 1: the one segment holding an unread range", got)
	}
	if got := f.core.Durability().Stats().DamagedSegments; got != 0 {
		t.Fatalf("DamagedSegments = %d after the suspended Copy, want 0: the unread source reached no corruption handler", got)
	}
	if got := f.countLossRecords(t); got != 0 {
		t.Fatalf("%d loss records on the seam after the suspended Copy, want 0: nothing was dropped and no loss recorded", got)
	}

	// The recovery half: the fault clears and A reads again, through the
	// same read the pass's first phase makes of every source, on the claim
	// the test still holds. A's range is unread until this read, so the
	// access is bounded at one attempt of the copier's closure: one header
	// read and one body read, no retry.
	if want := 5; reads != want {
		t.Fatalf("%d reads of the source segment by the suspended Copy, want %d: B's header and body and A's three failed attempts", reads, want)
	}
	f.seam.SetFault(nil)
	pre := f.seam.CallsFor(kit.OpRead, format.SegmentName(seg))
	cp := f.copier()
	got, rerr := cp.ReadSource(context.Background(), seg, file, recA)
	if rerr != nil {
		t.Fatalf("the recovery read of the record marked unread: %v", rerr)
	}
	hA, derr := format.DecodeRecordHeader(got[:format.RecordHeaderSize], copyIdentity, seg, int64(recA.Offset))
	if derr != nil {
		t.Fatalf("the recovery read returned %d bytes whose header does not decode: %v", len(got), derr)
	}
	bodyA, verr := format.VerifyRecordBody(got[format.RecordHeaderSize:], copyIdentity, hA)
	if verr != nil {
		t.Fatalf("the recovery read's body does not verify: %v", verr)
	}
	if !bytes.Equal(bodyA.Payload, payA) {
		t.Fatalf("the recovery read's payload of %d bytes, want A's bytes whole", len(bodyA.Payload))
	}
	after := f.seam.CallsFor(kit.OpRead, format.SegmentName(seg))
	if delta := after - pre; delta != 2 {
		t.Fatalf("the recovery read made %d seam reads, want 2: one attempt, one header and one body, no retry", delta)
	}
	if f.un.Unread(ref(recA)) {
		t.Fatal("A still marked unread after reading whole, want the read to clear it")
	}
	if got := f.un.SuspectSegments(); got != 0 {
		t.Fatalf("SuspectSegments = %d after the recovery read, want 0: the last unread range cleared", got)
	}

	// The second Copy over the same claimed segment: both sources copy.
	f.waitSpares(t)
	res2, err := f.copier().Copy(context.Background(), seg)
	if err != nil {
		t.Fatalf("the second Copy over the recovered segment: %v", err)
	}
	if len(res2.Copies) != 2 {
		t.Fatalf("the second Copy appended %d copies, want A's and B's", len(res2.Copies))
	}
	for i, want := range []struct {
		from index.RecordAt
		x    core.Extent
		pay  []byte
	}{
		{recB, core.Extent{Len: copyKib}, payB},
		{recA, core.Extent{Off: 2 * copyKib, Len: copyKib}, payA},
	} {
		cc := res2.Copies[i]
		if cc.From != want.from {
			t.Fatalf("copy %d is of record %v, want %v", i, cc.From, want.from)
		}
		if cc.Extent != want.x {
			t.Fatalf("copy %d covers extent %v, want %v", i, cc.Extent, want.x)
		}
		if cc.To.Segment == seg {
			t.Fatalf("copy %d landed in its own target segment %d", i, cc.To.Segment)
		}
		_, body := f.readBack(t, cc.To)
		if !bytes.Equal(body.Payload, want.pay) {
			t.Fatalf("copy %d carries %d bytes, want the source's bytes whole", i, len(body.Payload))
		}
	}
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

// forgetRec is one registered forget record for tag: a hand-made Rec of
// kind forget counted against the segment the caller names, its sequence
// number the caller drew — the registry state a pass's forgetting check
// consults.
func forgetRec(tag core.ShareTag, seg core.SegmentID, seq core.Seq) reclaim.Rec {
	return reclaim.Rec{
		Kind: format.KindForget, File: core.FileID{}, Seg: seg,
		Pair: index.Numbers{Seq: seq}, Seq: seq, Tag: tag,
	}
}

// TestCopyLosesToNewerWrite copies a source record whose whole extent a
// newer write overwrites between the two phases. The race point is the
// copier's own domain table, wrapped for the pass: its Lock runs the
// overwrite just before the real lock is taken — the pass's first phase
// (enumerate, read, verify) has finished, its second (the liveness check
// under the domain and the append) is about to run. The overwrite takes the
// file's append domain itself through the real write path: a WriteAt of
// exactly the source's extent through the fixture core's own share, drawing
// its own version and superseding the source's piece.
//
// The synced half settles the overwrite before the check runs — the
// supersede published and lifted the way a synced writer's path does — so
// at the check the source's piece is neither held nor retained and no copy
// appends. The unsynced half stops after the append and publish: the source
// is retained, not held, until the superseder syncs (RFC 1 §7.2), so the
// copy appends anyway — the real record of content a newer write replaced
// but has not yet settled — carrying the source's version and modification
// time under a fresh sequence number, reported for repoint.
func TestCopyLosesToNewerWrite(t *testing.T) {
	for _, tc := range []struct {
		name   string
		synced bool
	}{
		{"a synced overwrite lands first, so no copy appends", true},
		{"an unsynced overwrite lands first, so the source still copies", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newCopyFixture(t)
			file := copyFile(1)
			const mtime = 12345
			payA := make([]byte, copyKib)
			for i := range payA {
				payA[i] = byte(int64(i)>>8) ^ 0xa5
			}
			recA, seqA, vA := f.plant(t, copyTag, file, payA, mtime)
			f.sealSource(t, file)
			f.waitSpares(t)

			cs := f.selector().Select(reclaim.Scope{Segments: []core.SegmentID{recA.Segment}}, 1)
			if len(cs) != 1 || cs[0].ID != recA.Segment {
				t.Fatalf("Select returned %v, want the one candidate %d", cs, recA.Segment)
			}

			share := f.share(t, copyTag)
			payS := make([]byte, copyKib)
			for i := range payS {
				payS[i] = byte(int64(i)>>8) ^ 0x5a
			}
			dom := &countingDomains{inner: f.core.Domains()}
			var vNew core.Version
			dom.hook = func(core.FileID) {
				v, err := f.core.WriteAt(context.Background(), share, file, 0, payS, 777)
				if err != nil {
					t.Fatalf("the overwriting WriteAt over the source's extent: %v", err)
				}
				vNew = v
				if tc.synced {
					// The superseder settled: its publish lift dropped what it
					// superseded, the same lift a synced writer's path takes,
					// so nothing of the source is left at the check.
					if _, err := f.ix.LiftSuperseded(file, v, core.Extent{Len: copyKib}); err != nil {
						t.Fatalf("LiftSuperseded over the source's extent: %v", err)
					}
					if got := f.ix.HeldAt(file, recA); got != nil {
						t.Fatalf("the overwrite synced, held pieces at the source = %v, want none", got)
					}
					for _, rt := range f.ix.Retained(file) {
						if rt.Record == recA {
							t.Fatalf("the overwrite synced, want nothing of the source retained, got extent %v", rt.Extent)
						}
					}
					return
				}
				// The superseder appended and published but has not synced:
				// the source holds nothing and retains everything (RFC 1
				// §7.2), the state the unsynced half's copy must answer for.
				if got := f.ix.HeldAt(file, recA); got != nil {
					t.Fatalf("the overwrite published, held pieces at the source = %v, want none: the superseder holds the extent now", got)
				}
				var retained bool
				for _, rt := range f.ix.Retained(file) {
					if rt.Record == recA {
						retained = true
					}
				}
				if !retained {
					t.Fatal("the unsynced overwrite retains nothing of the source, want its piece retained until the superseder syncs")
				}
			}

			res, err := f.copierOver(dom).Copy(context.Background(), recA.Segment)
			if err != nil {
				t.Fatalf("Copy over the overwritten source: %v", err)
			}
			// The overwrite published under a newer version whatever the
			// half: the fact the two answers differ over.
			if p, ok := f.ix.Lookup(file, copyKib/2); !ok || p.Version != vNew {
				t.Fatalf("the source's extent after the overwrite = %+v %v, want it held at the overwriting version %v", p, ok, vNew)
			}
			if tc.synced {
				if len(res.Copies) != 0 {
					t.Fatalf("Copy appended %d copies over a synced overwrite (%+v), want 0: the source is neither held nor retained", len(res.Copies), res.Copies)
				}
				return
			}
			if len(res.Copies) != 1 {
				t.Fatalf("Copy appended %d copies over an unsynced overwrite (%+v), want 1: the source's piece is still retained", len(res.Copies), res.Copies)
			}
			cp := res.Copies[0]
			if cp.From != recA || cp.Extent != (core.Extent{Len: copyKib}) {
				t.Fatalf("the copy is of record %v covering %v, want the source record %v whole", cp.From, cp.Extent, recA)
			}
			if cp.Version != vA {
				t.Fatalf("copy Version = %v, want the source's %v and not the superseder's %v", cp.Version, vA, vNew)
			}
			if cp.Seq <= seqA {
				t.Fatalf("copy Seq = %d, want a fresh number above the source's %d", cp.Seq, seqA)
			}
			if cp.To.Segment == recA.Segment {
				t.Fatalf("the copy landed in its own target segment %d", cp.To.Segment)
			}
			h, body := f.readBack(t, cp.To)
			if h.Version != vA || body.MTime != mtime {
				t.Fatalf("the copy carries version %v and mtime %d, want the source's %v and %d", h.Version, body.MTime, vA, mtime)
			}
			if !bytes.Equal(body.Payload, payA) {
				t.Fatalf("the copy's payload holds %d bytes, want the source's bytes whole: the retained content is what the pass owes", len(body.Payload))
			}
		})
	}
}

// TestCopyAbandonedUnderForget pins the two orders of a forget record and a
// pass of its tag's segment (RFC 1 §8.4): a mark live before the copy's
// check abandons the tag's files — nothing appended, no sequence number
// drawn for them, the copies of another tag's records unaffected, a
// shared segment's other tag included — while a mark registered after a
// pass draws its own number above every copy the pass appended, and a
// later pass over the same claim copies nothing under it.
func TestCopyAbandonedUnderForget(t *testing.T) {
	pattern := func(xor byte) []byte {
		pay := make([]byte, copyKib)
		for i := range pay {
			pay[i] = byte(int64(i)>>8) ^ xor
		}
		return pay
	}

	// The mark live before the check: the tag's file is abandoned whole and
	// the other tag's records copy as if nothing changed.
	t.Run("a mark before the check abandons the tag's files alone", func(t *testing.T) {
		f := newCopyFixture(t)
		const other = copyTag + 1
		fileT, fileU := copyFile(1), copyFile(2)
		payT, payU := pattern(0xa5), pattern(0x3c)
		recT, _, _ := f.plant(t, copyTag, fileT, payT, 12345)
		// The two files sit on different streams, so the second's record
		// needs the pool's next spare beside the first's.
		f.waitSpares(t)
		recU, _, _ := f.plant(t, other, fileU, payU, 999)
		f.sealSource(t, fileT)
		f.sealSource(t, fileU)

		seqF, err := f.counters.NextSeq()
		if err != nil {
			t.Fatalf("NextSeq for the forget record: %v", err)
		}
		f.tracker.RegisterForget(forgetRec(copyTag, recT.Segment, seqF), []core.SegmentID{recT.Segment})
		if !f.tracker.Forgetting(copyTag) {
			t.Fatal("Forgetting of the marked tag = false, want true: the registry holds the mark")
		}
		if f.tracker.Forgetting(other) {
			t.Fatal("Forgetting of the unmarked tag = true, want false: the mark answers for its own tag alone")
		}

		f.waitSpares(t)
		segs := []core.SegmentID{recT.Segment}
		if recU.Segment != recT.Segment {
			segs = append(segs, recU.Segment)
		}
		seqPre, err := f.counters.NextSeq()
		if err != nil {
			t.Fatalf("NextSeq before the copies: %v", err)
		}
		var res reclaim.Result
		for _, seg := range segs {
			cs := f.selector().Select(reclaim.Scope{Segments: []core.SegmentID{seg}}, 1)
			if len(cs) != 1 || cs[0].ID != seg {
				t.Fatalf("Select over segment %d returned %v, want the one candidate", seg, cs)
			}
			r, err := f.copier().Copy(context.Background(), seg)
			if err != nil {
				t.Fatalf("Copy over segment %d: %v", seg, err)
			}
			res.Copies = append(res.Copies, r.Copies...)
		}

		var forT, forU []reclaim.Copied
		for _, cp := range res.Copies {
			switch cp.File {
			case fileT:
				forT = append(forT, cp)
			case fileU:
				forU = append(forU, cp)
			}
		}
		if len(forT) != 0 {
			t.Fatalf("the pass appended %d copies for the forgotten tag's file, want 0: the mark was live before its check ran (%+v)", len(forT), forT)
		}
		if len(forU) != 1 {
			t.Fatalf("the pass appended %d copies for the other tag's file, want 1: forgetting one tag abandons only its files", len(forU))
		}
		cp := forU[0]
		if cp.From != recU {
			t.Fatalf("the other tag's copy is of record %v, want its own source record %v", cp.From, recU)
		}
		_, body := f.readBack(t, cp.To)
		if !bytes.Equal(body.Payload, payU) {
			t.Fatalf("the other tag's copy holds %d bytes, want its source's bytes byte-exact: an abandoned neighbour changes nothing", len(body.Payload))
		}
		// The forgotten tag's bytes sit in no copy target: nothing was
		// appended for its files at all.
		for _, name := range func() []string {
			names, err := f.seam.List()
			if err != nil {
				t.Fatalf("List: %v", err)
			}
			return names
		}() {
			kind, id := format.ClassifyName(name)
			if kind != format.NameSegment || id == recT.Segment || id == recU.Segment {
				continue
			}
			raw := f.readNameBytes(t, name)
			if at := bytes.Index(raw, payT); at >= 0 {
				t.Fatalf("the forgotten tag's bytes sit in copy target segment %d at %d, want them nowhere but the source", id, at)
			}
		}
		// The seq the pass drew: one per copy it appended, and not one for
		// the abandoned file.
		seqPost, err := f.counters.NextSeq()
		if err != nil {
			t.Fatalf("NextSeq after the copies: %v", err)
		}
		if got, want := int(seqPost-seqPre)-1, len(res.Copies); got != want {
			t.Fatalf("the pass drew %d sequence numbers, want %d: one per copy appended and none for the abandoned file", got, want)
		}
	})

	// The mark registered after a pass: the copies already stand, the forget
	// record's own number answers above every copy's, and a later pass over
	// the same claim copies nothing under it.
	t.Run("a mark after the copies draws above every copy", func(t *testing.T) {
		f := newCopyFixture(t)
		file := copyFile(1)
		pay := pattern(0xa5)
		rec, _, _ := f.plant(t, copyTag, file, pay, 12345)
		f.sealSource(t, file)

		cs := f.selector().Select(reclaim.Scope{Segments: []core.SegmentID{rec.Segment}}, 1)
		if len(cs) != 1 || cs[0].ID != rec.Segment {
			t.Fatalf("Select returned %v, want the one candidate %d", cs, rec.Segment)
		}
		f.waitSpares(t)
		res, err := f.copier().Copy(context.Background(), rec.Segment)
		if err != nil {
			t.Fatalf("the first Copy over the source: %v", err)
		}
		if len(res.Copies) != 1 {
			t.Fatalf("the first Copy appended %d copies, want 1: no mark was live", len(res.Copies))
		}
		cp := res.Copies[0]
		_, body := f.readBack(t, cp.To)
		if !bytes.Equal(body.Payload, pay) || cp.From != rec {
			t.Fatalf("the standing copy is of %v with %d bytes, want the source %v byte-exact", cp.From, len(body.Payload), rec)
		}

		seqF, err := f.counters.NextSeq()
		if err != nil {
			t.Fatalf("NextSeq for the forget record: %v", err)
		}
		f.tracker.RegisterForget(forgetRec(copyTag, rec.Segment, seqF), []core.SegmentID{rec.Segment})
		for _, cp := range res.Copies {
			if seqF <= cp.Seq {
				t.Fatalf("the forget record's seq %d is not above copy (of %v) seq %d, want above: copies drawn under the domain before the mark precede its own draw", seqF, cp.From, cp.Seq)
			}
		}

		// The same claim re-run under the now-live mark: the file is
		// abandoned, so no copy of the tag answers with a sequence number
		// the forget record would leave behind.
		res2, err := f.copier().Copy(context.Background(), rec.Segment)
		if err != nil {
			t.Fatalf("the second Copy under the mark: %v", err)
		}
		if len(res2.Copies) != 0 {
			t.Fatalf("the second Copy appended %d copies under a live forget mark (%+v), want 0: a copy of the tag after the mark would carry a sequence number above the forget record's %d", len(res2.Copies), res2.Copies, seqF)
		}
		// The standing copy still reads byte-exact: the mark abandons the
		// later pass, never the record already appended.
		_, standing := f.readBack(t, cp.To)
		if !bytes.Equal(standing.Payload, pay) {
			t.Fatal("the standing copy's payload changed under the mark, want the source's bytes byte-exact")
		}
	})

	// The abandoned file shares its source segment with another tag's record:
	// the abandon answers per file under its own domain, so one unit holding
	// both tags copies the one and abandons the other.
	t.Run("the abandoned file's segment-mate of another tag still copies", func(t *testing.T) {
		f := newCopyFixture(t)
		const other = copyTag + 1
		fileT := copyFile(1)
		fileU := sameStreamFile(t, f, fileT)
		payT, payU := pattern(0xa5), pattern(0x3c)
		recT, _, _ := f.plant(t, copyTag, fileT, payT, 12345)
		recU, _, _ := f.plant(t, other, fileU, payU, 999)
		if recT.Segment != recU.Segment {
			t.Fatalf("the two records landed in segments %d and %d, want one shared source segment", recT.Segment, recU.Segment)
		}
		seg := recT.Segment
		f.sealSource(t, fileT)

		seqF, err := f.counters.NextSeq()
		if err != nil {
			t.Fatalf("NextSeq for the forget record: %v", err)
		}
		f.tracker.RegisterForget(forgetRec(copyTag, seg, seqF), []core.SegmentID{seg})

		f.waitSpares(t)
		cs := f.selector().Select(reclaim.Scope{Segments: []core.SegmentID{seg}}, 1)
		if len(cs) != 1 || cs[0].ID != seg {
			t.Fatalf("Select returned %v, want the one candidate %d", cs, seg)
		}
		res, err := f.copier().Copy(context.Background(), seg)
		if err != nil {
			t.Fatalf("Copy over the shared segment: %v", err)
		}
		var forT, forU int
		var cpU reclaim.Copied
		for _, cp := range res.Copies {
			switch cp.File {
			case fileT:
				forT++
			case fileU:
				forU++
				cpU = cp
			}
		}
		if forT != 0 {
			t.Fatalf("the pass appended %d copies of the forgotten tag's file inside its shared segment, want 0", forT)
		}
		if forU != 1 {
			t.Fatalf("the pass appended %d copies of the other tag's file inside the shared segment, want 1: the abandon answers per file", forU)
		}
		if cpU.From != recU {
			t.Fatalf("the segment-mate's copy is of record %v, want its own source record %v", cpU.From, recU)
		}
		_, body := f.readBack(t, cpU.To)
		if !bytes.Equal(body.Payload, payU) {
			t.Fatalf("the segment-mate's copy holds %d bytes, want its source's bytes byte-exact beside an abandoned file", len(body.Payload))
		}
	})
}

// sameStreamFile returns the id of another file on the stream of first, so
// records planted for the two land in one stream, one segment when neither
// stream rolls between their appends.
func sameStreamFile(t *testing.T, f *copyFixture, first core.FileID) core.FileID {
	t.Helper()
	want := f.core.Streams().IndexFor(first)
	for b := byte(2); b < 40; b++ {
		id := copyFile(b)
		if f.core.Streams().IndexFor(id) == want {
			return id
		}
	}
	t.Fatalf("no second file id found on file %v's stream", first)
	return core.FileID{}
}

// TestCopyLockOrder runs the pass over a segment holding two files'
// records while concurrent writers write other files around it, through the
// fixture core's real write path. The copier's domain table is wrapped with
// a counter: the pass holds at most one file append domain at any instant,
// and none at all while its source reads run. The reads are pinned to that
// by holding the pass's first source read at the seam: with the read
// parked, the counter answers zero, and a lookup and a span of the copied
// file answer beside it — the index takes no lock the pass holds while it
// reads, and no I/O the pass makes runs under any index lock (RFC 1 §8.4).
func TestCopyLockOrder(t *testing.T) {
	f := newCopyFixture(t)

	// Two source files on one stream, so their pair of records lands in one
	// segment the pass copies under two separate file domains.
	first := copyFile(1)
	second := sameStreamFile(t, f, first)

	type source struct {
		file core.FileID
		rec  index.RecordAt
		pay  []byte
	}
	var srcs []source
	for i, file := range []core.FileID{first, second} {
		pay := make([]byte, copyKib)
		for j := range pay {
			pay[j] = byte(int64(j)>>8) ^ byte(0xa0+i)
		}
		rec, _, _ := f.plant(t, copyTag, file, pay, 12345)
		srcs = append(srcs, source{file: file, rec: rec, pay: pay})
	}
	if srcs[0].rec.Segment != srcs[1].rec.Segment {
		t.Fatalf("the two source files landed in segments %d and %d, want one shared source segment", srcs[0].rec.Segment, srcs[1].rec.Segment)
	}
	seg := srcs[0].rec.Segment
	// The two files share one stream, so the one seal of its active segment
	// closes them both.
	f.sealSource(t, first)

	cs := f.selector().Select(reclaim.Scope{Segments: []core.SegmentID{seg}}, 1)
	if len(cs) != 1 || cs[0].ID != seg {
		t.Fatalf("Select returned %v, want the one candidate %d", cs, seg)
	}

	// Concurrent writers of other files, through the real write path while
	// the pass runs: their writes take their own files' domains on the same
	// real table the copier's wrapper wraps.
	share := f.share(t, copyTag)
	stop := make(chan struct{})
	var wg sync.WaitGroup
	var wrote atomic.Int32
	landed := make(chan struct{}, 2)
	for i := range 2 {
		file := copyFile(byte(9 + i))
		pay := make([]byte, copyKib)
		for j := range pay {
			pay[j] = byte(int64(j)>>8) ^ byte(0x50+i)
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			var signalled bool
			for off := int64(0); ; off += copyKib {
				select {
				case <-stop:
					return
				default:
				}
				if _, err := f.core.WriteAt(context.Background(), share, file, off, pay, 1); err != nil {
					// A spare the pool is refilling beneath two writers
					// refuses a piece; wait for the pool's settle and try
					// the same write again.
					ctx, cancel := context.WithTimeout(context.Background(), copyWait)
					_ = f.core.WaitSpares(ctx)
					cancel()
					continue
				}
				if !signalled {
					signalled = true
					landed <- struct{}{}
				}
				wrote.Add(1)
			}
		}()
	}

	// The pass's first source read is held at the seam: everything the
	// parked-moment assertions read runs while the pass waits inside its
	// read, holding nothing.
	h := f.seam.HoldFirst(t, kit.OpRead, format.SegmentName(seg))
	dom := &countingDomains{inner: f.core.Domains()}
	type copyAnswer struct {
		res reclaim.Result
		err error
	}
	done := make(chan copyAnswer, 1)
	go func() {
		res, err := f.copierOver(dom).Copy(context.Background(), seg)
		done <- copyAnswer{res, err}
	}()
	select {
	case <-h.Reached():
	case <-time.After(copyWait):
		h.Release()
		close(stop)
		wg.Wait()
		t.Fatalf("the copy never reached its first source read within %v, want it held", copyWait)
	}

	if got := dom.heldNow(); got != 0 {
		h.Release()
		close(stop)
		wg.Wait()
		t.Fatalf("the copier held %d file domains while its source read was parked, want 0: the reads run under no lock", got)
	}

	// A lookup and a span of the copied file answer while the read waits:
	// both complete, so the index was free across the parked read.
	probe := make(chan struct{})
	go func() {
		f.ix.Lookup(srcs[0].file, copyKib/2)
		f.ix.Span(srcs[0].file, core.Extent{Len: copyKib})
		close(probe)
	}()
	select {
	case <-probe:
	case <-time.After(copyWait):
		// A bounded wait, the assertion backstop: while the read parks, the
		// index owes the probe an answer; a pass holding an index lock
		// across its reads would park the probe with it.
		h.Release()
		close(stop)
		wg.Wait()
		t.Fatalf("a lookup and a span of the copied file did not answer within %v while the source read was parked, want the index free: no I/O runs under an index lock", copyWait)
	}

	// Both writers place their first write while the read is still parked,
	// so the count below rests on an event rather than on a race with the
	// pass's speed. A bounded wait, the assertion backstop: the parked pass
	// consumes nothing and the pool refills beside it, so a writer that
	// never lands a write inside it is a starvation the pool must answer
	// for.
	for range 2 {
		select {
		case <-landed:
		case <-time.After(copyWait):
			h.Release()
			close(stop)
			wg.Wait()
			t.Fatalf("a concurrent writer placed no write within %v while the source read was parked, want one: the pool refills beside a waiting pass", copyWait)
		}
	}
	h.Release()

	var a copyAnswer
	select {
	case a = <-done:
	case <-time.After(copyWait):
		close(stop)
		wg.Wait()
		t.Fatalf("Copy did not finish within %v after the hold released, want it completed", copyWait)
	}
	close(stop)
	wg.Wait()

	if a.err != nil {
		t.Fatalf("Copy beside concurrent writers: %v", a.err)
	}
	// Beside the concurrent writers a run places whole or split across
	// placements, so the assertion is the invariant, not a count: each source
	// file's copies tile its extent adjacently, cover it exactly, and hold
	// its payload byte-exact in file order.
	for _, s := range srcs {
		var assembled []byte
		var covered int64
		i := 0
		for _, cp := range a.res.Copies {
			if cp.File != s.file {
				continue
			}
			if cp.Extent.Off != covered {
				t.Fatalf("copy %d of file %v covers %v, want it adjacent at file offset %d: the run's placements tile its extent", i, s.file, cp.Extent, covered)
			}
			covered += cp.Extent.Len
			_, body := f.readBack(t, cp.To)
			assembled = append(assembled, body.Payload...)
			i++
		}
		if covered != copyKib {
			t.Fatalf("the copies of file %v cover %d bytes in %d placements, want the source's whole %d: every planned byte lands", s.file, covered, i, copyKib)
		}
		if !bytes.Equal(assembled, s.pay) {
			t.Fatalf("the copies of file %v hold %d bytes, want the source's bytes byte-exact in file order", s.file, len(assembled))
		}
	}
	if got := dom.maxHeld(); got > 1 {
		t.Fatalf("the copier held %d file domains at once, want ≤ 1: only one file's domain is held at any instant", got)
	}
	if got := wrote.Load(); got < 2 {
		t.Fatalf("the concurrent writers landed %d real writes during the pass, want ≥ 2: both wrote beside the pass before it ran", got)
	}
}
