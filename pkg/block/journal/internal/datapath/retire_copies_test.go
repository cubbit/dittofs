package datapath

import (
	"context"
	"testing"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/durability"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/format"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/index"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/segment"
)

// A retirement through the executor is the one that owes the copy table and
// the damaged rows their cleanup: the executor's hook fires with the retired
// segment's id only, after the segment left the set, and the wiring that
// hands it to Durability.Retired is what drops the copies the segment held
// and marks its damaged row retired, keeping the row, and what lowers the
// share figures a tagged write waits on. These tests drive a real retirement
// through Core.Retirer's executor over the assembled core.
//
// Both tests seal the stream's segment and then roll the stream onto a
// successor with one more write before retiring: a seal leaves the segment's
// descriptor to the stream until its next roll, and the retirer's Cache
// drain would otherwise wait on a ref nothing but that roll releases.

// TestRetirement_ThroughExecutorLeavesNoCopyEntry covers the executor's
// Retired hook: a sealed segment that held a record a corrupt read dropped,
// and a copy registered in it, is retired through Core.Retirer's executor,
// which drops the copy-table entry for the segment and marks its damaged row
// retired, the row still counted.
func TestRetirement_ThroughExecutorLeavesNoCopyEntry(t *testing.T) {
	c, _ := newTestCore(t)
	s := mustShare(t, c, 1)
	ctx := context.Background()
	f := testFile(1)
	v, err := c.WriteAt(ctx, s, f, 0, []byte("w"), 1)
	if err != nil {
		t.Fatalf("the write: %v", err)
	}
	placed, ok := c.Index().Lookup(f, 0)
	if !ok {
		t.Fatal("the write is not in the index")
	}
	seg := placed.Record.Segment

	// Seal the segment the record lives in, so a retirement can claim it:
	// its records are durable once the seal has synced them. The write below
	// rolls the stream onto a successor, releasing the sealed segment's
	// descriptor — the drain the retirer's Close runs would otherwise wait on
	// the ref the stream keeps until its next roll.
	if _, err := c.Streams().For(f).Seal(false); err != nil {
		t.Fatalf("Seal: %v", err)
	}
	h2 := sameStreamFile(c, f)
	if _, err := c.WriteAt(ctx, s, h2, 0, []byte("w"), 1); err != nil {
		t.Fatalf("the roll onto the successor: %v", err)
	}
	syncFiles(t, c, f)

	// The record's sequence number, read from its own header in the sealed
	// segment through the seam, names it to the copy table and the loss.
	hdr := recordHeaderAt(t, c, seg, int64(placed.Record.Offset))
	if hdr.Seq == 0 {
		t.Fatal("the record carries no sequence number")
	}
	// A copy of the record registered in the segment: the table's entry for
	// it is what the retirement must drop. Its checksum is one no real
	// record's body matches, so a read that looked for the copy would find
	// it bad rather than serve it — the shapes are distinct here, and the
	// retirement, not another write or a retirement of another path, is
	// what the test asks to drop the entry.
	copyLoc := segment.Location{Segment: seg, Off: int64(placed.Record.Offset), Len: int64(hdr.Size())}
	c.Durability().RegisterCopy(hdr.Seq, copyLoc, ^hdr.PayloadCRC)
	if got := c.Durability().CopyCount(seg); got != 1 {
		t.Fatalf("the copy table holds %d entries in segment %d, want 1", got, seg)
	}

	// A corrupt read of the record found it bad and dropped it: the segment
	// is marked damaged as of this call, and the worker records the loss.
	out, err := c.Durability().Corrupt(ctx, durability.Corruption{
		Share:   s.Tag,
		File:    f,
		Record:  index.RecordAt{Segment: seg, Offset: placed.Record.Offset},
		Seq:     hdr.Seq,
		Version: v,
		Extent:  core.Extent{Off: 0, Len: 1},
		AtOpen:  true,
		Synced:  true,
	})
	if err != nil || out.Kind != durability.CorruptDropped {
		t.Fatalf("Corrupt = %+v, %v, want the record dropped", out, err)
	}
	if p, ok := c.Index().Lookup(f, 0); ok {
		t.Fatalf("the dropped record is still served from %+v", p.Record)
	}

	// The retirement runs through the executor the core wires: its hook
	// must leave no copy-table entry for the segment and mark the damaged
	// row retired, keeping it counted.
	if _, err := c.Retirer().Retire(seg, segment.NotHeld); err != nil {
		t.Fatalf("Retire(%d): %v", seg, err)
	}
	if got := c.Durability().CopyCount(seg); got != 0 {
		t.Fatalf("the copy table holds %d entries in the retired segment %d, want 0", got, seg)
	}
	p := c.Durability().Stats()
	var row durability.DamagedRow
	var found bool
	for _, r := range p.Segments {
		if r.Segment == seg {
			row, found = r, true
		}
	}
	if !found || !row.Retired {
		t.Fatalf("the retired segment's damaged row = %+v, found %v, want a retired row", row, found)
	}
	if p.DamagedSegments != 1 {
		t.Fatalf("the damaged count = %d after the retirement, want the retired row still counted as 1", p.DamagedSegments)
	}
}

// TestRetirement_ThroughExecutorLowersTheShareFigure covers the other half
// of the hook: the executor's hook also calls Core.Retired for every
// registered share's tag, so a share whose settled record bytes a retirement
// freed finds the room it gave back and its next write is admitted where the
// same write was refused before.
func TestRetirement_ThroughExecutorLowersTheShareFigure(t *testing.T) {
	c, _ := newTestCore(t)
	// The limit holds exactly two of the write's asks: the first lands in
	// one segment, the second rolls the stream onto a successor — releasing
	// the first segment's descriptor, which the retirer's drain waits on —
	// and every further ask of the same size is refused by the share's limit
	// until the retirement of the first segment gives its bytes back.
	const payload = 4096
	s := mustLimitShare(t, c, 1, 2*(recordBytes(payload)+askOverhead()))
	f := testFile(1)
	ctx := context.Background()
	if _, err := c.WriteAt(ctx, s, f, 0, make([]byte, payload), 1); err != nil {
		t.Fatalf("the first write: %v", err)
	}
	placed, ok := c.Index().Lookup(f, 0)
	if !ok {
		t.Fatal("the first write is not in the index")
	}
	// Seal the first segment and roll the stream onto a successor with the
	// second write, so the retirement can claim a segment no stream holds.
	if _, err := c.Streams().For(f).Seal(false); err != nil {
		t.Fatalf("Seal: %v", err)
	}
	h2 := sameStreamFile(c, f)
	if _, err := c.WriteAt(ctx, s, h2, 0, make([]byte, payload), 1); err != nil {
		t.Fatalf("the rolling write onto the successor: %v", err)
	}
	syncFiles(t, c, f, h2)

	// Before the retirement the share's figure still holds both records'
	// bytes: a third write of the same size is refused naming the limit, its
	// ask more than the slack the limit has left.
	h3 := testFile(3)
	_, err := c.WriteAt(ctx, s, h3, 0, make([]byte, payload), 1)
	if ns := refused(t, err, core.LimitShare); ns.Missing == 0 {
		t.Fatalf("the refusal names no shortfall: %+v", ns)
	}

	// The retirement through the executor: its hook lowers the share's
	// figure by the first segment's record bytes, so the same ask the limit
	// refused now fits.
	if _, err := c.Retirer().Retire(placed.Record.Segment, segment.NotHeld); err != nil {
		t.Fatalf("Retire(%d): %v", placed.Record.Segment, err)
	}
	if _, err := c.WriteAt(ctx, s, h3, 0, make([]byte, payload), 1); err != nil {
		t.Fatalf("the write after the retirement: %v", err)
	}
}

// sameStreamFile returns a file id that lands on f's stream, so a write
// through it rolls the very segment f's stream holds. It never returns f
// itself, and with the two streams of a test core it always finds one
// among the first few bytes.
func sameStreamFile(c *Core, f core.FileID) core.FileID {
	for b := byte(2); b < 40; b++ {
		if g := testFile(b); g != f && streamOf(c, g) == streamOf(c, f) {
			return g
		}
	}
	return f // unreachable with the test cores' two streams
}

// recordHeaderAt reads the record header at off of segment id, through a
// descriptor the core's cache opens, and fails the test when it cannot.
func recordHeaderAt(t *testing.T, c *Core, seg core.SegmentID, off int64) format.RecordHeader {
	t.Helper()
	s := c.Streams()
	var target *segment.Stream
	for i := 0; i < s.Len(); i++ {
		if st := s.Stream(i); st != nil {
			target = st
			break
		}
	}
	if target == nil {
		t.Fatal("the core has no stream to read the record through")
	}
	buf := make([]byte, format.RecordHeaderSize)
	if _, err := target.ReadAt(seg, buf, off); err != nil {
		t.Fatalf("reading the record header of segment %d at %d: %v", seg, off, err)
	}
	h, err := format.DecodeRecordHeader(buf, cID(), seg, off)
	if err != nil {
		t.Fatalf("decoding the record header of segment %d at %d: %v", seg, off, err)
	}
	return h
}

// cID returns the journal identity the test cores are built with, the one
// the record headers verify against.
func cID() core.JournalID { return testIdentity }

// TestRetirement_WhileUnregisteredLowersTheSeedToo covers the hook against a
// share that is closed when its segment retires: the executor's hook must
// lower the figure the admission holds for the tag even then, for the seed
// happens once and a reattached share would otherwise wait on a stale-high
// figure until some unrelated retirement fired while it was registered.
func TestRetirement_WhileUnregisteredLowersTheSeedToo(t *testing.T) {
	c, _ := newTestCore(t)
	const payload = 4096
	// The limit holds exactly two of the write's asks, as the executor
	// test's does: the first two land, every later ask is refused.
	s := mustLimitShare(t, c, 1, 2*(recordBytes(payload)+askOverhead()))
	f := testFile(1)
	ctx := context.Background()
	if _, err := c.WriteAt(ctx, s, f, 0, make([]byte, payload), 1); err != nil {
		t.Fatalf("the first write: %v", err)
	}
	placed, ok := c.Index().Lookup(f, 0)
	if !ok {
		t.Fatal("the first write is not in the index")
	}
	// Seal, roll onto a successor and sync, so the first segment is one
	// the retirer's drain can claim.
	if _, err := c.Streams().For(f).Seal(false); err != nil {
		t.Fatalf("Seal: %v", err)
	}
	h2 := sameStreamFile(c, f)
	if _, err := c.WriteAt(ctx, s, h2, 0, make([]byte, payload), 1); err != nil {
		t.Fatalf("the rolling write: %v", err)
	}
	syncFiles(t, c, f, h2)

	// Close the handle before the retirement: the tag leaves the registry,
	// and the segment retires while no share of it is registered.
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := c.Retirer().Retire(placed.Record.Segment, segment.NotHeld); err != nil {
		t.Fatalf("Retire(%d): %v", placed.Record.Segment, err)
	}

	// A fresh handle on the same tag must find the room the retirement
	// freed: the seed already ran for this tag, so what the reattached
	// share sees is the figure the hook lowers, not the set's pair.
	s2 := mustLimitShare(t, c, 1, 2*(recordBytes(payload)+askOverhead()))
	h3 := testFile(3)
	if _, err := c.WriteAt(ctx, s2, h3, 0, make([]byte, payload), 1); err != nil {
		t.Fatalf("the write after the reattach: %v", err)
	}
}
