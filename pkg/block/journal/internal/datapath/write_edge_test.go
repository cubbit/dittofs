package datapath

import (
	"bytes"
	"context"
	"errors"
	"math"
	"slices"
	"testing"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/format"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/index"
	"github.com/cubbit/dittofs/pkg/block/journal/journaltest"
)

// fileWrites returns the write records of f on disk, in segment and offset
// order.
func fileWrites(t *testing.T, seam *journaltest.Seam, f core.FileID) []onDisk {
	t.Helper()
	var out []onDisk
	for _, r := range readRecords(t, seam) {
		if r.Header.Kind == format.KindWrite && r.Header.File == f {
			out = append(out, r)
		}
	}
	return out
}

// heldPieces walks the held pieces of f that tile [lo, hi) through Lookup. A
// gap in the range fails the test.
func heldPieces(t *testing.T, c *Core, f core.FileID, lo, hi int64) []index.Placed {
	t.Helper()
	var out []index.Placed
	for off := lo; off < hi; {
		p, ok := c.Index().Lookup(f, off)
		if !ok {
			t.Fatalf("no held piece at %d", off)
		}
		out = append(out, p)
		off = p.Extent.Off + p.Extent.Len
	}
	return out
}

func mustWrite(t *testing.T, c *Core, s *Share, f core.FileID, off int64, p []byte) core.Version {
	t.Helper()
	v, err := c.WriteAt(context.Background(), s, f, off, p, 1)
	if err != nil {
		t.Fatalf("WriteAt(%d, %d bytes): %v", off, len(p), err)
	}
	return v
}

// segmentRoom is the payload bytes the first record of a fresh stream places
// in one segment, found by writing more than a segment holds.
func segmentRoom(t *testing.T) int64 {
	t.Helper()
	c, seam := newTestCore(t)
	f := testFile(1)
	mustWrite(t, c, c.AddShare(1), f, 0, make([]byte, 2*testSegmentCap))
	recs := fileWrites(t, seam, f)
	if len(recs) < 2 {
		t.Fatalf("a write of two segments made %d records", len(recs))
	}
	return recs[0].Header.Extent.Len
}

func TestEdge_WriteCrossesSegment(t *testing.T) {
	c, seam := newTestCore(t)
	s := c.AddShare(1)
	f := testFile(1)
	mustWrite(t, c, s, f, 0, bytes.Repeat([]byte{1}, 40<<10))
	const off = 1 << 20
	payload := make([]byte, 40<<10)
	for i := range payload {
		payload[i] = byte(i*7 + 3)
	}
	v := mustWrite(t, c, s, f, off, payload)

	// The records are read as WriteAt returns: both pieces are written.
	var got []onDisk
	for _, r := range fileWrites(t, seam, f) {
		if r.Header.Version == v {
			got = append(got, r)
		}
	}
	if len(got) != 2 {
		t.Fatalf("%d records carry the write's version, want 2", len(got))
	}
	n := got[0].Header.Extent.Len
	if got[0].Header.Extent.Off != off || got[1].Header.Extent.Off != off+n || n+got[1].Header.Extent.Len != int64(len(payload)) {
		t.Fatalf("pieces are %v and %v, want a split of [%d,+%d) at its own offsets", got[0].Header.Extent, got[1].Header.Extent, int64(off), len(payload))
	}
	if got[0].Header.Segment == got[1].Header.Segment {
		t.Fatal("both pieces are in one segment")
	}
	if got[0].Header.Seq >= got[1].Header.Seq {
		t.Fatalf("sequence numbers %d, %d do not grow", got[0].Header.Seq, got[1].Header.Seq)
	}
	if !bytes.Equal(append(slices.Clone(got[0].Body.Payload), got[1].Body.Payload...), payload) {
		t.Fatal("the two pieces do not carry the write's bytes")
	}
	pieces := heldPieces(t, c, f, off, off+int64(len(payload)))
	if len(pieces) != 2 || pieces[0].Version != v || pieces[1].Version != v {
		t.Fatalf("held pieces = %+v, want two at version %v", pieces, v)
	}
}

func TestEdge_RecordExactlyFillsSegment(t *testing.T) {
	room := segmentRoom(t)
	c, seam := newTestCore(t)
	f := testFile(2)
	v := mustWrite(t, c, c.AddShare(1), f, 0, make([]byte, room))
	recs := fileWrites(t, seam, f)
	if len(recs) != 1 || recs[0].Header.Version != v || recs[0].Header.Extent.Len != room {
		t.Fatalf("records = %d, want one of %d bytes", len(recs), room)
	}
	if got := heldPieces(t, c, f, 0, room); len(got) != 1 {
		t.Fatalf("%d held pieces, want 1", len(got))
	}
	// One byte more no longer fits whole: it splits.
	c2, seam2 := newTestCore(t)
	g := testFile(3)
	mustWrite(t, c2, c2.AddShare(1), g, 0, make([]byte, room+1))
	if n := len(fileWrites(t, seam2, g)); n != 2 {
		t.Fatalf("a write one byte over the room made %d records, want 2", n)
	}
}

func TestEdge_ThreeWaySplit(t *testing.T) {
	c, _ := newTestCore(t)
	s := c.AddShare(1)
	f := testFile(4)
	v1 := mustWrite(t, c, s, f, 0, make([]byte, 3000))
	v2 := mustWrite(t, c, s, f, 1000, make([]byte, 1000))
	got := heldPieces(t, c, f, 0, 3000)
	want := []struct {
		e core.Extent
		v core.Version
	}{{core.Extent{Off: 0, Len: 1000}, v1}, {core.Extent{Off: 1000, Len: 1000}, v2}, {core.Extent{Off: 2000, Len: 1000}, v1}}
	if len(got) != 3 {
		t.Fatalf("held pieces = %+v, want three", got)
	}
	for i, w := range want {
		if got[i].Extent != w.e || got[i].Version != w.v {
			t.Fatalf("piece %d = %+v, want %v at %v", i, got[i], w.e, w.v)
		}
	}
	if h, _, _ := c.heldBytes(); h != 3000 {
		t.Fatalf("held = %d, want 3000", h)
	}
}

func TestEdge_OverwriteCoversSeveralExtents(t *testing.T) {
	c, _ := newTestCore(t)
	s := c.AddShare(1)
	f := testFile(5)
	var vs []core.Version
	for i := range 5 { // five extents of 100: Z, A, B, C, D
		vs = append(vs, mustWrite(t, c, s, f, int64(i)*100, make([]byte, 100)))
	}
	v := mustWrite(t, c, s, f, 50, make([]byte, 400)) // [50,450): part of Z, A B C whole, part of D
	got := heldPieces(t, c, f, 0, 500)
	want := []struct {
		e core.Extent
		v core.Version
	}{{core.Extent{Off: 0, Len: 50}, vs[0]}, {core.Extent{Off: 50, Len: 400}, v}, {core.Extent{Off: 450, Len: 50}, vs[4]}}
	if len(got) != len(want) {
		t.Fatalf("held pieces = %+v, want %d", got, len(want))
	}
	for i, w := range want {
		if got[i].Extent != w.e || got[i].Version != w.v {
			t.Fatalf("piece %d = %+v, want %v at %v", i, got[i], w.e, w.v)
		}
	}
	if h, _, _ := c.heldBytes(); h != 500 {
		t.Fatalf("held = %d, want 500", h)
	}
}

func TestEdge_SameExtentOverwrittenManyTimes(t *testing.T) {
	c, seam := newTestCore(t)
	s := c.AddShare(1)
	f := testFile(6)
	var vs []core.Version
	for i := range 200 {
		vs = append(vs, mustWrite(t, c, s, f, 4096, bytes.Repeat([]byte{byte(i)}, 512)))
		if h, _, _ := c.heldBytes(); h != 512 {
			t.Fatalf("after %d writes held = %d, want 512", i+1, h)
		}
		if n := len(heldPieces(t, c, f, 4096, 4096+512)); n != 1 {
			t.Fatalf("after %d writes the extent has %d index entries, want 1", i+1, n)
		}
	}
	seqOf := map[core.Version]core.Seq{}
	for _, r := range fileWrites(t, seam, f) {
		seqOf[r.Header.Version] = r.Header.Seq
	}
	if len(seqOf) != 200 {
		t.Fatalf("%d records on disk, want 200", len(seqOf))
	}
	for i := 1; i < len(vs); i++ {
		if seqOf[vs[i]] <= seqOf[vs[i-1]] {
			t.Fatalf("write %d has sequence %d, not above %d", i, seqOf[vs[i]], seqOf[vs[i-1]])
		}
	}
	if p, _ := c.Index().Lookup(f, 4096); p.Version != vs[199] {
		t.Fatalf("held version %v, want the last, %v", p.Version, vs[199])
	}
}

func TestEdge_FirstHeldByteFarFromZero(t *testing.T) {
	c, _ := newTestCore(t)
	f := testFile(7)
	const off = int64(1) << 40
	v := mustWrite(t, c, c.AddShare(1), f, off, make([]byte, 4096))
	for _, at := range []int64{off, off + 4095} {
		if p, ok := c.Index().Lookup(f, at); !ok || p.Version != v || p.Extent != (core.Extent{Off: off, Len: 4096}) {
			t.Fatalf("Lookup(%d) = %+v, %v", at, p, ok)
		}
	}
	if _, ok := c.Index().Lookup(f, 0); ok {
		t.Fatal("a byte below the write is held")
	}
	if h, _, _ := c.heldBytes(); h != 4096 {
		t.Fatalf("held = %d, want 4096", h)
	}
}

// A zero-length write changes nothing whatever the file already holds.
// WriteAt's early return already covers it, so this pins the behaviour
// without guarding a line of its own.
func TestEdge_ZeroLengthWrite(t *testing.T) {
	c, _ := newTestCore(t)
	s := c.AddShare(1)
	f := testFile(8)
	v := mustWrite(t, c, s, f, 0, make([]byte, 100))
	if z, err := c.WriteAt(context.Background(), s, f, 50, nil, 1); z != (core.Version{}) || err != nil {
		t.Fatalf("WriteAt of nothing = %v, %v", z, err)
	}
	got := heldPieces(t, c, f, 0, 100)
	if len(got) != 1 || got[0].Version != v {
		t.Fatalf("held pieces = %+v, want the one write", got)
	}
}

func TestEdge_WriteNearLargestOffset(t *testing.T) {
	c, _ := newTestCore(t)
	s := c.AddShare(1)
	f := testFile(9)
	p := make([]byte, 16)
	off := int64(math.MaxInt64) - int64(len(p))
	v := mustWrite(t, c, s, f, off, p)
	if got, ok := c.Index().Lookup(f, off+int64(len(p))-1); !ok || got.Version != v {
		t.Fatalf("Lookup of the last byte = %+v, %v", got, ok)
	}
	if h, _, _ := c.heldBytes(); h != 16 {
		t.Fatalf("held = %d, want 16", h)
	}
	g := testFile(10)
	z, err := c.WriteAt(context.Background(), s, g, off+1, p, 1)
	if !errors.Is(err, core.ErrInvalidExtent) || z != (core.Version{}) {
		t.Fatalf("a write one byte too far = %v, %v; want ErrInvalidExtent", z, err)
	}
	if _, ok := c.Index().Lookup(g, off+1); ok {
		t.Fatal("a refused write is held")
	}
}

func TestWriteAt_ThreePiecesAcrossTwoRolls(t *testing.T) {
	c, seam := newTestCore(t)
	f := testFile(11)
	payload := make([]byte, 5*testSegmentCap/2)
	for i := range payload {
		payload[i] = byte(i*31 + i>>8)
	}
	const off = 12345
	v, err := c.WriteAt(context.Background(), c.AddShare(1), f, off, payload, 1)
	if err != nil {
		t.Fatalf("WriteAt: %v", err)
	}
	recs := fileWrites(t, seam, f)
	segments := map[core.SegmentID]bool{}
	var joined []byte
	next := int64(off)
	for _, r := range recs {
		if r.Header.Version != v || r.Header.Extent.Off != next {
			t.Fatalf("record %+v: want version %v at offset %d", r.Header, v, next)
		}
		next += r.Header.Extent.Len
		segments[r.Header.Segment] = true
		joined = append(joined, r.Body.Payload...)
	}
	if len(recs) != 3 || len(segments) < 3 {
		t.Fatalf("%d records in %d segments, want 3 in at least 3 (two rolls)", len(recs), len(segments))
	}
	if !bytes.Equal(joined, payload) {
		t.Fatal("the pieces do not carry the payload")
	}
	if h, _, _ := c.heldBytes(); h != int64(len(payload)) {
		t.Fatalf("held = %d, want %d", h, len(payload))
	}
	if c.WrittenBytes() != uint64(len(payload)) {
		t.Fatalf("WrittenBytes = %d, want %d", c.WrittenBytes(), len(payload))
	}
	for _, p := range heldPieces(t, c, f, off, off+int64(len(payload))) {
		if p.Version != v {
			t.Fatalf("held piece %+v is not at version %v", p, v)
		}
	}
}

// A later piece that fails returns no version; the pieces already published
// stay held at the write's version and the failed one leaves nothing the
// index names.
func TestWriteAt_LaterPieceFails(t *testing.T) {
	var armed bool
	var c *Core
	var seam *journaltest.Seam
	boom := errors.New("injected write failure")
	f := testFile(12)
	c, seam = newTestCore(t, func(o *coreOptions) {
		o.accountSpy = func(index.HeldDelta) {
			// Piece 1 is published: every segment after its own fails to
			// take a write, so the next piece's record cannot land.
			if !armed {
				return
			}
			armed = false
			p, ok := c.Index().Lookup(f, 0)
			if !ok {
				return
			}
			var rules []journaltest.Rule
			for id := p.Record.Segment + 1; id <= p.Record.Segment+8; id++ {
				rules = append(rules, journaltest.Failing(journaltest.OpWrite, format.SegmentName(id), 0, boom))
			}
			if err := seam.Schedule(rules...); err != nil {
				panic(err)
			}
		}
	})
	s := c.AddShare(1)
	armed = true
	payload := make([]byte, 2*testSegmentCap)
	v, err := c.WriteAt(context.Background(), s, f, 0, payload, 1)
	if !errors.Is(err, boom) {
		t.Fatalf("WriteAt = %v, want the injected failure", err)
	}
	if v != (core.Version{}) {
		t.Fatalf("a failed write returned version %v", v)
	}
	p1, ok := c.Index().Lookup(f, 0)
	if !ok || p1.Version == (core.Version{}) {
		t.Fatalf("piece 1 is not held: %+v, %v", p1, ok)
	}
	end := p1.Extent.Off + p1.Extent.Len
	if end >= int64(len(payload)) {
		t.Fatal("piece 1 covers the whole write")
	}
	if _, ok := c.Index().Lookup(f, end); ok {
		t.Fatal("the index holds a piece past piece 1")
	}
	if h, _, _ := c.heldBytes(); h != p1.Extent.Len {
		t.Fatalf("held = %d, want piece 1's %d", h, p1.Extent.Len)
	}
	if c.WrittenBytes() != uint64(p1.Extent.Len) {
		t.Fatalf("WrittenBytes = %d, want %d", c.WrittenBytes(), p1.Extent.Len)
	}
	for _, r := range fileWrites(t, seam, f) {
		if r.Header.Extent.Off != 0 || r.Header.Version != p1.Version {
			t.Fatalf("a record besides piece 1 reached disk: %+v", r.Header)
		}
	}
}
