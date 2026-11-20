package datapath

import (
	"bytes"
	"context"
	"errors"
	"math"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/capacity"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/format"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/index"
	"github.com/cubbit/dittofs/pkg/block/journal/journaltest/kit"
)

// fileWrites returns the write records of f on disk, in segment and offset
// order.
func fileWrites(t *testing.T, seam *kit.Seam, f core.FileID) []onDisk {
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

// retryRefused runs op until it stops being refused by one of limits or a
// generous deadline passes, and returns its last error. A roll that finds no
// spare ready, or an append that finds the headroom drawn, is refused
// retryably by design and the pool refills on its own; no clock can be
// injected into that refill, so the bounded wait is the condition itself.
func retryRefused(op func() error, limits ...core.Limit) error {
	deadline := time.Now().Add(10 * time.Second)
	for {
		err := op()
		var ns *core.NoSpaceError
		if !errors.As(err, &ns) || !slices.Contains(limits, ns.Limit) || time.Now().After(deadline) {
			return err
		}
		time.Sleep(time.Millisecond)
	}
}

func mustWrite(t *testing.T, c *Core, s *Share, f core.FileID, off int64, p []byte) core.Version {
	t.Helper()
	var v core.Version
	err := retryRefused(func() (err error) {
		v, err = c.WriteAt(context.Background(), s, f, off, p, 1)
		return err
	}, core.LimitSpare)
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
	mustWrite(t, c, mustShare(t, c, 1), f, 0, make([]byte, 2*testSegmentCap))
	recs := fileWrites(t, seam, f)
	if len(recs) < 2 {
		t.Fatalf("a write of two segments made %d records", len(recs))
	}
	return recs[0].Header.Extent.Len
}

func TestEdge_WriteCrossesSegment(t *testing.T) {
	c, seam := newTestCore(t)
	s := mustShare(t, c, 1)
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
	v := mustWrite(t, c, mustShare(t, c, 1), f, 0, make([]byte, room))
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
	mustWrite(t, c2, mustShare(t, c2, 1), g, 0, make([]byte, room+1))
	if n := len(fileWrites(t, seam2, g)); n != 2 {
		t.Fatalf("a write one byte over the room made %d records, want 2", n)
	}
}

func TestEdge_ThreeWaySplit(t *testing.T) {
	c, _ := newTestCore(t)
	s := mustShare(t, c, 1)
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
	s := mustShare(t, c, 1)
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
	s := mustShare(t, c, 1)
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
	// Each write leaves a record whose sequence tops the one before it, and
	// the held write's record is on disk. The records the check runs over are
	// the survivors alone: a segment whose every record a seal's sync lifted —
	// superseded, settled, dropped from the index — is retired by the liveness
	// wiring and unlinked, so the oldest writes' records may be gone from the
	// directory by the time this counts, and their count is pinned by nothing.
	seqOf := map[core.Version]core.Seq{}
	for _, r := range fileWrites(t, seam, f) {
		seqOf[r.Header.Version] = r.Header.Seq
	}
	var last core.Seq
	survived := 0
	for _, v := range vs {
		seq, ok := seqOf[v]
		if !ok {
			continue
		}
		survived++
		if seq <= last {
			t.Fatalf("the write with sequence %d is not above the survived %d", seq, last)
		}
		last = seq
	}
	if _, ok := seqOf[vs[199]]; !ok {
		t.Fatalf("the held write's record is not on disk: %d of %d survived", survived, len(vs))
	}
	if p, _ := c.Index().Lookup(f, 4096); p.Version != vs[199] {
		t.Fatalf("held version %v, want the last, %v", p.Version, vs[199])
	}
}

func TestEdge_FirstHeldByteFarFromZero(t *testing.T) {
	c, _ := newTestCore(t)
	f := testFile(7)
	const off = int64(1) << 40
	v := mustWrite(t, c, mustShare(t, c, 1), f, off, make([]byte, 4096))
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
	s := mustShare(t, c, 1)
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
	s := mustShare(t, c, 1)
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
	v, err := c.WriteAt(context.Background(), mustShare(t, c, 1), f, off, payload, 1)
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

// A later piece that fails to append returns no version and publishes
// nothing: the first piece's record stays on disk, counted against the share
// and unnamed by the index, so no read serves bytes of a write that was refused.
func TestWriteAt_LaterPieceFails(t *testing.T) {
	var armed atomic.Bool
	var c *Core
	var seam *kit.Seam
	boom := errors.New("injected write failure")
	f := testFile(12)
	c, seam = newTestCore(t, func(o *coreOptions) {
		o.afterAppend = func() {
			// Piece 1 is in its segment and not yet published: every segment
			// after its own fails to take a write, so the next piece's record
			// cannot land.
			if !armed.Load() {
				return
			}
			armed.Store(false)
			seg, ok := c.Streams().For(f).OpenSegment()
			if !ok {
				panic("the stream has no open segment after an append")
			}
			var rules []kit.Rule
			for id := seg + 1; id <= seg+8; id++ {
				rules = append(rules, kit.Failing(kit.OpWrite, format.SegmentName(id), 0, boom))
			}
			if err := seam.Schedule(rules...); err != nil {
				panic(err)
			}
		}
	})
	s := mustShare(t, c, 1)
	armed.Store(true)
	payload := make([]byte, 2*testSegmentCap)
	v, err := c.WriteAt(context.Background(), s, f, 0, payload, 1)
	if !errors.Is(err, boom) {
		t.Fatalf("WriteAt = %v, want the injected failure", err)
	}
	if v != (core.Version{}) {
		t.Fatalf("a failed write returned version %v", v)
	}
	if p, ok := c.Index().Lookup(f, 0); ok {
		t.Fatalf("the index names %+v of a write that failed on its later piece, want nothing", p)
	}
	if h, _, _ := c.heldBytes(); h != 0 {
		t.Fatalf("held = %d after a refused write, want 0", h)
	}
	if got := c.WrittenBytes(); got != 0 {
		t.Fatalf("WrittenBytes = %d after a refused write, want 0: none of it was published", got)
	}
	recs := fileWrites(t, seam, f)
	if len(recs) != 1 || recs[0].Header.Extent.Off != 0 {
		t.Fatalf("records of the file on disk = %d, want piece 1's alone at offset 0", len(recs))
	}
	if c.liveness.Held(recs[0].Segment) {
		t.Fatalf("segment %d still reads as held after the write was refused, want the dead record's hold released", recs[0].Segment)
	}
	if err := seam.Schedule(); err != nil {
		t.Fatalf("the seam refused to clear its faults: %v", err)
	}
	if _, err := c.WriteAt(context.Background(), s, f, 0, []byte("retry"), 2); err != nil {
		t.Fatalf("the retry after the refused write: %v", err)
	}
}

// A reserved write whose later piece fails to append keeps the pieces it
// already published and returns no version, so its holder reserves again for
// what they do not cover; an unreserved write in the same shape publishes
// nothing (TestWriteAt_LaterPieceFails).
func TestWriteReserved_LaterPieceFailsKeepsThePublishedPieces(t *testing.T) {
	var armed atomic.Bool
	var c *Core
	var seam *kit.Seam
	boom := errors.New("injected write failure")
	f := testFile(13)
	c, seam = newTestCore(t, func(o *coreOptions) {
		o.afterAppend = func() {
			if !armed.Load() {
				return
			}
			armed.Store(false)
			seg, ok := c.Streams().For(f).OpenSegment()
			if !ok {
				panic("the stream has no open segment after an append")
			}
			var rules []kit.Rule
			for id := seg + 1; id <= seg+8; id++ {
				rules = append(rules, kit.Failing(kit.OpWrite, format.SegmentName(id), 0, boom))
			}
			if err := seam.Schedule(rules...); err != nil {
				panic(err)
			}
		}
	})
	s := mustShare(t, c, 1)
	payload := make([]byte, 2*testSegmentCap)
	r := reserve(t, c, s, capacity.WriteCharge(int64(len(payload)), testSegmentCap), 1)
	defer r.Release()
	armed.Store(true)
	v, err := c.WriteReserved(context.Background(), s, r, f, 0, payload, 1)
	if !errors.Is(err, boom) {
		t.Fatalf("WriteReserved = %v, want the injected failure", err)
	}
	if v != (core.Version{}) {
		t.Fatalf("a failed write returned version %v", v)
	}
	p1, ok := c.Index().Lookup(f, 0)
	if !ok {
		t.Fatal("the index names nothing of a reserved write whose first piece was published, want that piece held")
	}
	if end := p1.Extent.Off + p1.Extent.Len; end >= int64(len(payload)) {
		t.Fatalf("piece 1 covers %d of %d bytes, want the write cut short", end, len(payload))
	}
}

// A segment holding a piece of an unreserved write that is appended and not yet
// published stays held: the roll that sealed it left no index entry naming the
// piece, so without a hold on the piece's segment a retirement pass that ran
// then would unlink it under the entry the publish adds. The hold ends with the
// publish, and with a refusal of the write.
func TestWriteAt_UnpublishedPieceKeepsItsSealedSegment(t *testing.T) {
	var c *Core
	var seam *kit.Seam
	f := testFile(14)
	var appended atomic.Int64
	atRoll := make(chan struct{})
	resume := make(chan struct{})
	var resumeOnce sync.Once
	release := func() { resumeOnce.Do(func() { close(resume) }) }
	c, seam = newTestCore(t, func(o *coreOptions) {
		o.afterAppend = func() {
			if appended.Add(1) != 2 {
				return
			}
			// The second piece's roll sealed the first piece's segment, and
			// nothing is published.
			close(atRoll)
			<-resume
		}
	})
	// A failed check must not leave the write parked: the core's close, which
	// runs after this, waits on it.
	t.Cleanup(release)
	s := mustShare(t, c, 1)

	wrote := make(chan error, 1)
	go func() {
		_, err := c.WriteAt(context.Background(), s, f, 0, make([]byte, 2*testSegmentCap), 1)
		wrote <- err
	}()
	waitIn(t, 30*time.Second, "the write's second piece to append", func() bool {
		select {
		case <-atRoll:
			return true
		default:
			return false
		}
	})

	recs := fileWrites(t, seam, f)
	if len(recs) == 0 {
		t.Fatal("no record of the write is on disk after its roll")
	}
	first := recs[0].Segment
	if _, ok := c.Index().Lookup(f, 0); ok {
		t.Fatal("the index names the write before its last piece is appended, want nothing published")
	}
	if !c.liveness.Held(first) {
		t.Fatalf("segment %d reads as holding nothing while a piece of an unpublished write sits in it, want it held", first)
	}

	release()
	if err := <-wrote; err != nil {
		t.Fatalf("WriteAt: %v", err)
	}
	if !c.liveness.Held(first) {
		t.Fatalf("segment %d reads as holding nothing after its piece was published, want its bytes held", first)
	}
}

// An unreserved write's piece pins its segment inside the append, before the
// write counts the record or does anything else: the segment cannot be sealed,
// and so retired, in the gap between the append and the pin.
func TestWriteAt_PieceIsPinnedBeforeTheWriteGoesOn(t *testing.T) {
	var c *Core
	f := testFile(15)
	var pinned atomic.Bool
	var checked atomic.Bool
	c, _ = newTestCore(t, func(o *coreOptions) {
		o.beforeWrote = func() {
			seg, ok := c.Streams().For(f).OpenSegment()
			if ok && !checked.Swap(true) {
				pinned.Store(c.liveness.Held(seg))
			}
		}
	})
	s := mustShare(t, c, 1)
	if _, err := c.WriteAt(context.Background(), s, f, 0, make([]byte, 4096), 1); err != nil {
		t.Fatalf("WriteAt: %v", err)
	}
	if !checked.Load() {
		t.Fatal("the write's append never reached the hook")
	}
	if !pinned.Load() {
		t.Fatal("the piece's segment reads as holding nothing when the append returns, want it pinned inside the append")
	}
}
