package reclaim_test

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/capacity"
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
	// forgetting is the share-forget mark the copier consults, the seam a
	// test plants its mark through; nil in every test that forgets nothing,
	// which is the production wiring too.
	forgetting func(core.ShareTag) bool
	// deferred, admit and drawn are the deferred-loss wirings the pass hands
	// the copier: the registry the durability layer keeps, mapped field by
	// field, and the headroom's admission with the counting of its draws.
	// nil in every test that defers nothing, which is every wiring but the
	// pass's own.
	deferred func(core.SegmentID) []reclaim.DeferredLoss
	admit    func(capacity.RecordClass) (capacity.Draw, error)
	drawn    func(capacity.Draw)
	crashed  bool // set by a test that crashes the seam; read by the cleanup
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
	t.Cleanup(func() {
		err := c.Close()
		if f.crashed {
			// Only a seam that was crashed closed every descriptor under the
			// core, so only then may a close error be fs.ErrClosed; any other
			// error the join keeps still fails the test.
			err = withoutErrClosed(err)
		}
		if err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	f.waitSpares(t)
	return f
}

// withoutErrClosed returns the leaves of err that are not fs.ErrClosed, joined,
// or nil when there are none.
func withoutErrClosed(err error) error {
	switch e := err.(type) {
	case nil:
		return nil
	case interface{ Unwrap() []error }:
		var rest []error
		for _, c := range e.Unwrap() {
			rest = append(rest, withoutErrClosed(c))
		}
		return errors.Join(rest...)
	default:
		if errors.Is(err, fs.ErrClosed) {
			return nil
		}
		return err
	}
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
// corrupter are the core's own pieces, its forget mark the fixture's
// plant, nil outside a test that forgets something, and its deferred-loss
// wirings the fixture's plants likewise, nil outside a test that defers
// something.
func (f *copyFixture) copier() *reclaim.Copier {
	return reclaim.NewCopier(f.ix, f.counters, f.core.Streams(), f.un, f.tracker, f.core.Domains(), f.forgetting, f.corrupt, f.clock, slog.New(slog.DiscardHandler), f.deferred, f.admit, f.drawn)
}

// copierOver wires the pass over the fixture's core behind dom, the append
// domain table the copier takes instead of the core's own: a recording
// wrapper around the real one, or the plain table.
func (f *copyFixture) copierOver(dom reclaim.Domains) *reclaim.Copier {
	return reclaim.NewCopier(f.ix, f.counters, f.core.Streams(), f.un, f.tracker, dom, f.forgetting, f.corrupt, f.clock, slog.New(slog.DiscardHandler), f.deferred, f.admit, f.drawn)
}

// share returns the fixture core's handle of tag, closed when the test ends.
func (f *copyFixture) share(t *testing.T, tag core.ShareTag) *datapath.Share {
	t.Helper()
	s, err := f.core.Share(tag, 0, core.Version{})
	if err != nil {
		t.Fatalf("Share of tag %d: %v", tag, err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Errorf("Close of the share of tag %d: %v", tag, err)
		}
	})
	return s
}

// countingDomains wraps the real file append domains for the copier alone.
// Lock runs the test hook just before it takes the real lock — the seam
// between the pass's two phases, where the liveness check and the append
// are about to run under the domain — and counts how many domains the
// wrapper's holders keep, so a test can race a write in that gap and assert
// on what the pass holds at any instant.
type countingDomains struct {
	inner reclaim.Domains
	hook  func(core.FileID)

	mu   sync.Mutex
	held int
	max  int
}

// Lock runs the hook, then takes the real domain and counts it held until
// the release it returns is called.
func (d *countingDomains) Lock(id core.FileID) func() {
	if d.hook != nil {
		d.hook(id)
	}
	unlock := d.inner.Lock(id)
	d.mu.Lock()
	d.held++
	if d.held > d.max {
		d.max = d.held
	}
	d.mu.Unlock()
	return func() {
		d.mu.Lock()
		d.held--
		d.mu.Unlock()
		unlock()
	}
}

// heldNow reports how many domains the wrapper's holders keep.
func (d *countingDomains) heldNow() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.held
}

// maxHeld reports the most domains any instant found held.
func (d *countingDomains) maxHeld() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.max
}

// corrupt drops a record the pass could not verify into the durability
// layer, the record's append domain already held by the caller, and maps
// the layer's outcome onto the pass's own: the mapping keeps package reclaim
// from importing the durability layer, which the journal wires.
func (f *copyFixture) corrupt(ctx context.Context, bad reclaim.BadSource) (reclaim.CorruptOutcome, error) {
	out, err := f.core.Durability().Corrupt(ctx, durability.Corruption{
		Share:      bad.Tag,
		File:       bad.File,
		Record:     bad.Record,
		Seq:        bad.Seq,
		Version:    bad.Version,
		Extent:     bad.Extent,
		DomainHeld: true,
	})
	if err != nil {
		return 0, err
	}
	switch out.Kind {
	case durability.CorruptDropped:
		return reclaim.CorruptDropped, nil
	case durability.CorruptUseCopy:
		return reclaim.CorruptUseCopy, nil
	}
	return 0, fmt.Errorf("durability answered corrupt with an outcome %d with no mapping", out.Kind)
}

// plantRemoval appends one header-only removal record of kind of file and
// publishes it the way the writer's own removal path does: the file's append
// domain held, a version and a sequence number drawn, the record appended,
// the removal applied with its marker left over over, the held delta
// accounted. The record registers live with the tracker at the place its
// append reported, and settle names whether metadata's settle covers the
// removal's own version at once, the re-test of its file included, the way
// a settle racing a pass would.
func (f *copyFixture) plantRemoval(t *testing.T, tag core.ShareTag, file core.FileID, kind format.Kind, change core.ChangeKind, header, over core.Extent, settle bool) (reclaim.Rec, index.RecordAt, core.Seq, core.Version) {
	t.Helper()
	unlock := f.core.Domains().Lock(file)
	defer unlock()
	v, err := f.counters.Draw()
	if err != nil {
		t.Fatalf("Draw of the %s's version: %v", kind, err)
	}
	seq, err := f.counters.NextSeq()
	if err != nil {
		t.Fatalf("NextSeq of the %s: %v", kind, err)
	}
	pl, err := f.core.Streams().For(file).Append(format.RecordHeader{
		Kind: kind, File: file, Extent: header, Seq: seq, Version: v, Tag: tag,
	}, format.RecordBody{})
	if err != nil {
		t.Fatalf("Append of the %s record: %v", kind, err)
	}
	at := index.RecordAt{Segment: pl.Loc.Segment, Offset: uint32(pl.Loc.Off)}
	hd, _, err := f.ix.ApplyRemoval(tag, file, change, over, v, seq, at, nil)
	if err != nil {
		t.Fatalf("ApplyRemoval of the %s: %v", kind, err)
	}
	f.core.Account(hd)
	rec := reclaim.Rec{
		Kind: kind, File: file, Seg: pl.Loc.Segment, Ext: header,
		Pair: index.Numbers{Version: v, Seq: seq}, Seq: seq, Tag: tag, Loc: pl.Loc,
	}
	f.tracker.Register(rec)
	if settle {
		f.tracker.MarkerSettled(file, v)
	}
	return rec, at, seq, v
}

// plantLoss appends one header-only loss record of file naming the dropped
// record's sequence number and publishes it the way the durability layer's
// own loss path does: the file's append domain held, a version and a
// sequence number drawn, the record appended with its loss body, the synced
// loss applied over the extent e it covers, its marker left over it. The
// record registers live with the tracker at the place its append reported,
// settle is plantRemoval's.
func (f *copyFixture) plantLoss(t *testing.T, tag core.ShareTag, file core.FileID, dropped core.Seq, why format.LossReason, e core.Extent, settle bool) (reclaim.Rec, index.RecordAt, core.Seq, core.Version) {
	t.Helper()
	unlock := f.core.Domains().Lock(file)
	defer unlock()
	v, err := f.counters.Draw()
	if err != nil {
		t.Fatalf("Draw of the loss record's version: %v", err)
	}
	seq, err := f.counters.NextSeq()
	if err != nil {
		t.Fatalf("NextSeq of the loss record: %v", err)
	}
	pl, err := f.core.Streams().For(file).Append(format.RecordHeader{
		Kind: format.KindLoss, File: file, Extent: e, Seq: seq, Version: v, Tag: tag,
	}, format.RecordBody{Loss: format.Loss{Dropped: dropped, Synced: true, Reason: why}})
	if err != nil {
		t.Fatalf("Append of the loss record: %v", err)
	}
	at := index.RecordAt{Segment: pl.Loc.Segment, Offset: uint32(pl.Loc.Off)}
	hd, _, err := f.ix.Lose(file, e, v)
	if err != nil {
		t.Fatalf("Lose of the loss record: %v", err)
	}
	f.core.Account(hd)
	rec := reclaim.Rec{
		Kind: format.KindLoss, File: file, Seg: pl.Loc.Segment, Ext: e,
		Pair: index.Numbers{Version: v, Seq: seq},
		Seq:  seq, Named: dropped, Tag: tag,
		Synced: true, Superseded: why == format.LossSuperseded, Loc: pl.Loc,
	}
	f.tracker.Register(rec)
	if settle {
		f.tracker.MarkerSettled(file, v)
	}
	return rec, at, seq, v
}

// flipByte flips every bit of the byte at off of the segment the record at
// names: damage on the device, written through the seam's own file.
func (f *copyFixture) flipByte(t *testing.T, at index.RecordAt, off int64) {
	t.Helper()
	name := format.SegmentName(at.Segment)
	file, err := f.seam.Open(name)
	if err != nil {
		t.Fatalf("Open %s: %v", name, err)
	}
	defer func() { _ = file.Close() }()
	b := make([]byte, 1)
	if _, err := file.ReadAt(b, off); err != nil {
		t.Fatalf("reading %s at %d: %v", name, off, err)
	}
	b[0] ^= 0xff
	if _, err := file.WriteAt(b, off); err != nil {
		t.Fatalf("writing %s at %d: %v", name, off, err)
	}
}

// segmentRaw reads a whole segment's bytes through the seam.
func (f *copyFixture) segmentRaw(t *testing.T, seg core.SegmentID) []byte {
	t.Helper()
	return f.readNameBytes(t, format.SegmentName(seg))
}

// readNameBytes reads a whole file of the seam's directory.
func (f *copyFixture) readNameBytes(t *testing.T, name string) []byte {
	t.Helper()
	file, err := f.seam.Open(name)
	if err != nil {
		t.Fatalf("Open %s: %v", name, err)
	}
	defer func() { _ = file.Close() }()
	size, err := file.Size()
	if err != nil {
		t.Fatalf("Size of %s: %v", name, err)
	}
	raw := make([]byte, size)
	if _, err := file.ReadAt(raw, 0); err != nil {
		t.Fatalf("reading %s: %v", name, err)
	}
	return raw
}

// countLossRecords walks every segment file the seam holds and counts the
// loss records decodable in each: the check that no loss was recorded
// anywhere, read from the bytes a loss record would sit in rather than a
// counter beside them.
func (f *copyFixture) countLossRecords(t *testing.T) int {
	t.Helper()
	names, err := f.seam.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	total := 0
	for _, name := range names {
		kind, id := format.ClassifyName(name)
		if kind != format.NameSegment {
			continue
		}
		raw := f.readNameBytes(t, name)
		for off := 0; off+format.RecordHeaderSize <= len(raw); {
			h, err := format.DecodeRecordHeader(raw[off:off+format.RecordHeaderSize], copyIdentity, id, int64(off))
			if err != nil {
				// The walk stops where the records end: the bytes past the
				// last one hold a seal marker or a footer, no record.
				break
			}
			if h.Kind == format.KindLoss {
				total++
			}
			off += int(h.Size())
		}
	}
	return total
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
