package reclaim_test

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/datapath"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/durability"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/format"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/index"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/reclaim"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/segment"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/unread"
	"github.com/cubbit/dittofs/pkg/block/journal/journaltest/kit"
)

const (
	// copySegCap is the segment size the copy fixture's journal accounts.
	copySegCap = int64(64 << 10)
	// copyHeadroomRecords is the headroom the fixture's journal reserves,
	// in records.
	copyHeadroomRecords = 2
	// copyStreams is the number of the fixture's append streams.
	copyStreams = 2
	// copyExtents is the placement bound the fixture's index is built with.
	copyExtents = 1 << 20
	// copyWait is the backstop around the fixture's one settle event.
	copyWait = 10 * time.Second
)

// copyIdentity is the journal identity the fixture seeds every record's
// checksums with.
var copyIdentity = core.JournalID{0xd, 0x9, 0x5}

// copyTag is the share the fixture's records count against.
const copyTag = core.ShareTag(1)

// copyFile returns the file id distinguished by b.
func copyFile(b byte) core.FileID { return core.NewFileID([16]byte{15: b}) }

// copyFixture is one real journal core over a recorded storage seam: the
// index, the counters and the segment ledger are the fixture's own pieces,
// handed to the core as its State, and every record is planted through the
// core's own streams and index the way the writer's path plants them.
type copyFixture struct {
	core     *datapath.Core
	seam     *kit.Seam
	ix       *index.Index
	counters *index.Counters
	set      *segment.Set
	un       *unread.Set
	tracker  *reclaim.Tracker
	clock    core.Clock
}

// newCopyFixture builds the core, waits for its spare pool to settle, and
// closes it when the test ends.
func newCopyFixture(t *testing.T) *copyFixture {
	t.Helper()
	dir := t.TempDir()
	seam, err := kit.NewSeam(dir)
	if err != nil {
		t.Fatalf("the seam could not be made: %v", err)
	}
	clock := kit.NewStopwatchClock()
	ix := index.New(copyExtents)
	counters := &index.Counters{}
	if err := counters.SetVersionLimit(core.Version{Counter: 1 << 40}); err != nil {
		t.Fatalf("SetVersionLimit: %v", err)
	}
	set := segment.NewSet(copySegCap, nil)
	c, err := datapath.New(datapath.Deps{
		FS:              seam.FS(),
		Clock:           clock,
		Log:             slog.New(slog.DiscardHandler),
		Dir:             dir,
		Identity:        copyIdentity,
		Streams:         copyStreams,
		SegmentCap:      copySegCap,
		MaxBytes:        0,
		HeadroomRecords: copyHeadroomRecords,
		Targets:         segment.Targets{Content: 1, Reserve: 1, Headroom: 1},
	}, datapath.State{Index: ix, Counters: counters, Set: set})
	if err != nil {
		t.Fatalf("datapath.New: %v", err)
	}
	t.Cleanup(func() {
		if err := c.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	f := &copyFixture{
		core:     c,
		seam:     seam,
		ix:       ix,
		counters: counters,
		set:      set,
		un:       unread.New(clock),
		tracker:  reclaim.New(ix, nil),
		clock:    clock,
	}
	f.waitSpares(t)
	return f
}

// waitSpares waits for the spare pool's own settle event: every draw a test
// or a pass makes — a plant's content spare, a copy's reserve target — has to
// find one ready, and the refilling beside it is the pool's own work.
func (f *copyFixture) waitSpares(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), copyWait)
	defer cancel()
	if err := f.core.WaitSpares(ctx); err != nil {
		t.Fatalf("WaitSpares: %v", err)
	}
}

// plant appends one write record of payload to file at offset zero, under a
// version drawn fresh, and publishes it the way the writer's path does.
func (f *copyFixture) plant(t *testing.T, tag core.ShareTag, file core.FileID, payload []byte, mtime int64) (index.RecordAt, core.Seq, core.Version) {
	t.Helper()
	v, err := f.counters.Draw()
	if err != nil {
		t.Fatalf("Draw: %v", err)
	}
	rec, seq := f.plantAt(t, tag, file, core.Extent{Len: int64(len(payload))}, payload, mtime, v)
	return rec, seq, v
}

// plantAt appends one write record covering x of file, under the version the
// caller names, and publishes it through the index the way the writer's path
// does: the file's append domain held, a sequence number issued, the record
// appended and its entry retained, the held delta accounted. The set's
// per-tag ledger needs no hand-planting here: the stream's own append
// accounts the record's bytes on the share's row.
func (f *copyFixture) plantAt(t *testing.T, tag core.ShareTag, file core.FileID, x core.Extent, payload []byte, mtime int64, v core.Version) (index.RecordAt, core.Seq) {
	t.Helper()
	unlock := f.core.Domains().Lock(file)
	defer unlock()
	seq, err := f.counters.NextSeq()
	if err != nil {
		t.Fatalf("NextSeq: %v", err)
	}
	p, err := f.core.Streams().For(file).Append(format.RecordHeader{
		Kind: format.KindWrite, File: file, Extent: x, Seq: seq, Version: v, Tag: tag,
	}, format.RecordBody{Payload: payload, MTime: mtime})
	if err != nil {
		t.Fatalf("Append: %v", err)
	}
	rec := index.RecordAt{Segment: p.Loc.Segment, Offset: uint32(p.Loc.Off)}
	hd, err := f.ix.WriteRetain(tag, file, index.Placed{Extent: x, Record: rec, Version: v}, seq)
	if err != nil {
		t.Fatalf("WriteRetain: %v", err)
	}
	f.core.Account(hd)
	return rec, seq
}

// sealSource seals the segment the file's records landed in the way the
// writer's own seal does, so the segment becomes a selection candidate.
func (f *copyFixture) sealSource(t *testing.T, file core.FileID) {
	t.Helper()
	if _, err := f.core.Streams().For(file).Seal(false); err != nil {
		t.Fatalf("Seal: %v", err)
	}
}

// selector returns a selection over the fixture's pieces. It claims the
// candidates it returns, as a pass's does.
func (f *copyFixture) selector() *reclaim.Selector {
	return reclaim.NewSelector(f.set, f.ix, f.un, nil, nil)
}

// copier wires the pass over the fixture's core: its domains and its
// corrupter are the core's own pieces.
func (f *copyFixture) copier() *reclaim.Copier {
	return reclaim.NewCopier(f.ix, f.counters, f.core.Streams(), f.un, f.tracker, f.core.Domains(), f.corrupt, f.clock, slog.New(slog.DiscardHandler))
}

// corrupt drops a record the pass could not verify into the durability
// layer, the record's append domain already held by the caller.
func (f *copyFixture) corrupt(ctx context.Context, bad reclaim.BadSource) error {
	_, err := f.core.Durability().Corrupt(ctx, durability.Corruption{
		Share:      bad.Tag,
		File:       bad.File,
		Record:     bad.Record,
		Seq:        bad.Seq,
		Version:    bad.Version,
		Extent:     bad.Extent,
		DomainHeld: true,
	})
	return err
}

// readBack reads whole the record at off the seam's bytes, verifies it and
// returns it: the copy the seam holds, decoded independently of the pass.
func (f *copyFixture) readBack(t *testing.T, at index.RecordAt) (format.RecordHeader, format.RecordBody) {
	t.Helper()
	name := format.SegmentName(at.Segment)
	file, err := f.seam.Open(name)
	if err != nil {
		t.Fatalf("Open %s: %v", name, err)
	}
	defer func() { _ = file.Close() }()
	head := make([]byte, format.RecordHeaderSize)
	if _, err := file.ReadAt(head, int64(at.Offset)); err != nil {
		t.Fatalf("reading a header in %s: %v", name, err)
	}
	h, err := format.DecodeRecordHeader(head, copyIdentity, at.Segment, int64(at.Offset))
	if err != nil {
		t.Fatalf("DecodeRecordHeader in %s: %v", name, err)
	}
	body := make([]byte, h.BodySize())
	if _, err := file.ReadAt(body, int64(at.Offset)+format.RecordHeaderSize); err != nil {
		t.Fatalf("reading a body in %s: %v", name, err)
	}
	verified, err := format.VerifyRecordBody(body, copyIdentity, h)
	if err != nil {
		t.Fatalf("VerifyRecordBody in %s: %v", name, err)
	}
	return h, verified
}
