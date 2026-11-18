package recovery

import (
	"bytes"
	"context"
	"errors"
	"math"
	"slices"
	"syscall"
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/durability"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/format"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/index"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/reclaim"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/segment"
	"github.com/cubbit/dittofs/pkg/block/journal/journaltest/kit"
	"github.com/cubbit/dittofs/pkg/block/journal/journaltest/raw"
)

// TestNotHeldSegmentsRetireBeforeEmptying holds the emptying's first step
// and its candidate class: a sealed segment nothing holds — its one record
// superseded by a newer version in the ended segment beside it — retires at
// that step, its file leaving the directory, while the ended segment, whose
// record still serves, stands for the emptying that follows: an ended
// segment holding what serves is never retired ahead of its own emptying,
// and a candidate class narrower or wider than the tracker's own liveness
// answer — a superset — leaves the emptying drawing its spares behind what
// the retire step should have refilled.
func TestNotHeldSegmentsRetireBeforeEmptying(t *testing.T) {
	file := tailFile()
	r := newRig(t)
	r.writeFormat()
	dead, _ := buildSealed(1, collectWrite(7, file, 1, 1, 0x4100, 16))
	r.plantSeg(1, dead)
	// The ended segment beside it: its damaged segment header ends it
	// whole, and the newer version of the record the dead segment holds
	// keeps it held — an ended segment still serving, the emptying's own.
	r.plantSeg(2, damageHeader(buildSeg(2, collectWrite(7, file, 2, 2, 0x4100, 16))))

	st, err := Run(context.Background(), r.runDeps(), core.Version{}, journalID, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	h := newEmptyingHarness(t, r, &st, segment.Targets{Reserve: 1})

	// The retire step alone: the dead sealed segment goes, the ended one —
	// held by the record that still serves — stands for the emptying that
	// follows.
	rd := newReader(context.Background(), r.seam.FS(), r.clk, nil)
	empty := &emptying{deps: h.deps, rd: rd, st: &st, identity: journalID}
	if err := runRetire(context.Background(), h.deps, rd, &st, journalID, core.Version{}, nil, empty); err != nil {
		t.Fatalf("the retire step: %v", err)
	}
	if slices.Contains(r.listing(), format.SegmentName(1)) {
		t.Errorf("the dead sealed segment's file is still on disk, want it retired at the emptying's first step")
	}
	if _, err := st.Assembled.Set.State(1); err == nil {
		t.Errorf("the assembled set still holds the dead segment's row, want it gone with the retirement")
	}
	if !slices.Contains(r.listing(), format.SegmentName(2)) {
		t.Errorf("the ended segment's file is gone from the directory at the retire step, want it standing for the emptying that follows")
	}
	if got, err := st.Assembled.Set.State(2); err != nil || got != segment.Ended {
		t.Errorf("the ended segment's row got %v (%v), want Ended and standing", got, err)
	}
	if _, ok := st.Rebuilt.Index.Lookup(file, 0x4100); !ok {
		t.Errorf("the index answers no record at 0x4100, want the newer version the ended segment holds serving it")
	}

	// The emptying step: the ended segment's survivor is copied whole into
	// a sealed spare of the reserve the retire step refilled, its numbers
	// kept, and the segment's file leaves the directory with a synced
	// unlink.
	if err := runEmptying(context.Background(), h.deps, rd, &st, journalID, core.Version{}, nil, empty); err != nil {
		t.Fatalf("the emptying step: %v", err)
	}
	if slices.Contains(r.listing(), format.SegmentName(2)) {
		t.Errorf("the ended segment's file is still on disk, want it unlinked by the emptying")
	}
	if _, err := st.Assembled.Set.State(2); err == nil {
		t.Errorf("the assembled set still holds the emptied segment's row, want it gone with the retirement")
	}
	placed, ok := st.Rebuilt.Index.Lookup(file, 0x4100)
	if !ok {
		t.Fatalf("the index answers no record at 0x4100 after the emptying, want the copy of the record the ended segment held")
	}
	if placed.Record.Segment == 2 {
		t.Errorf("the record of the emptied segment still serves from segment 2, want it repointed to the copy")
	}
	if got, err := st.Assembled.Set.State(placed.Record.Segment); err != nil || got != segment.Sealed {
		t.Errorf("the copy's row got %v (%v), want Sealed", got, err)
	}
	b, err := r.seam.Durable(format.SegmentName(placed.Record.Segment))
	if err != nil {
		t.Fatalf("read the copy's file: %v", err)
	}
	walk, werr := format.ScanSegmentHeaders(bytes.NewReader(b), int64(len(b)), journalID, placed.Record.Segment)
	if werr != nil || len(walk.Records) != 1 {
		t.Fatalf("walking the sealed copy found %d records (err %v), want the one record kept whole", len(walk.Records), werr)
	}
	if h := walk.Records[0].Header; h.Seq != 2 || h.Version.Counter != 2 {
		t.Errorf("the copy's record kept seq %d version %d, want the source's 2 and 2", h.Seq, h.Version.Counter)
	}
}

// TestRetirementNeverTakesSparesOrDeferred holds the retire step's two
// exemptions on real state: a spare the open re-adopted — the pool's own,
// never a retirement's — and a segment the per-directory deferred-loss
// registry answers as holding a deferred loss's dropped record both stand,
// while the dead full seal beside them retires by the retirement path's own
// rule for a full word, a recycle refilling the repack reserve before the
// next ended segment's emptying draws a spare from it.
func TestRetirementNeverTakesSparesOrDeferred(t *testing.T) {
	file := tailFile()
	r := newRig(t)
	r.writeFormat()
	// The dead full seal, its trailer in the cap's last bytes, catalog-named
	// by its successor, which holds the newer version that leaves the seal's
	// one record superseded and the newer version of the deferred segment's
	// record beside it.
	dead, _ := buildSealed(1, collectWrite(7, file, 1, 1, 2<<10, 16))
	r.plantSeg(1, dead)
	sup := [2]raw.Record{
		collectWrite(7, file, 2, 2, 2<<10, 16),
		collectWrite(7, file, 4, 4, 3<<10, 16),
	}
	hb := raw.SegmentHeader{Identity: journalID, ID: 2, Predecessor: 1}.Bytes()
	for i := range sup {
		sup[i].Identity, sup[i].Segment, sup[i].RecordOffset = journalID, 2, uint32(len(hb))
		hb = append(hb, sup[i].Bytes()...)
	}
	r.plantSeg(2, hb)
	deferred, _ := buildSealed(4, collectWrite(7, file, 3, 3, 3<<10, 16))
	r.plantSeg(4, deferred)

	d := r.runDeps()
	d.SegmentCap = int64(len(dead))
	// A spare the spares phase re-adopts at the same cap the seal holds.
	r.write(format.SegmentName(3), make([]byte, d.SegmentCap))
	r.deferLossAgainst(t, journalID, 4, durability.Loss{
		Share:   7,
		File:    file,
		Extent:  core.Extent{Off: 3 << 10, Len: 16},
		Version: core.Version{Counter: 3},
		Seq:     3,
		Reason:  core.LossCorrupt,
		Synced:  true,
	})

	st, err := Run(context.Background(), d, core.Version{}, journalID, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	preRun := slices.Clone(r.listing())
	h := newEmptyingHarness(t, r, &st, segment.Targets{Reserve: 1})
	if err := Emptying(context.Background(), h.deps, &st, journalID, core.Version{}, nil); err != nil {
		t.Fatalf("Emptying: %v", err)
	}
	// The pool refills in the background on the same id sequence as the
	// retirer's recycle, so the reserve is read once the pool has settled.
	if err := h.pool.WaitSpares(context.Background()); err != nil {
		t.Fatalf("the pool's settle: %v", err)
	}

	// The dead full seal retired by the retirement path's own rule for a
	// full word: a recycle, its fresh spare refilling the reserve the
	// emptying draws on — the old name gone, the fresh one on disk, the
	// pool's ready repack reserve at the cap it holds.
	if slices.Contains(r.listing(), format.SegmentName(1)) {
		t.Errorf("the dead full seal's old name is still on disk, want it recycled by the retire step")
	}
	// A spare is new when its name was not in the directory before the run;
	// which of the recycle and the pool's refill made it is not the row's.
	var added int
	for _, name := range r.listing() {
		if kind, _ := format.ClassifyName(name); kind == format.NameSegment && !slices.Contains(preRun, name) {
			added++
		}
	}
	if added < 1 {
		t.Errorf("no new spare is on disk after the retire step, want the recycle's refill in the reserve the emptying draws on")
	}
	if got, want := h.pool.Counters().RepackReserveBytes, d.SegmentCap; got < want {
		t.Errorf("the pool's ready repack reserve after the retire step got %d bytes, want at least the %d-byte cap: each retirement refills the reserve before the next emptying, the refill's own spare beside it", got, want)
	}
	// The spare and the deferred loss's segment both stand.
	if !slices.Contains(r.listing(), format.SegmentName(3)) {
		t.Errorf("the re-adopted spare's file is gone from the directory, want a spare never a retirement's")
	}
	if _, err := st.Assembled.Set.State(3); !errors.Is(err, segment.ErrNotInSet) {
		t.Errorf("the re-adopted spare's row answered %v, want ErrNotInSet: a re-adopted spare is the pool's, never a ledger row a retirement reads", err)
	}
	if !slices.Contains(r.listing(), format.SegmentName(4)) {
		t.Errorf("the segment holding the deferred loss's dropped record is gone from the directory, want it standing until the copy that retires it lands")
	}
	if got, err := st.Assembled.Set.State(4); err != nil || got != segment.Sealed {
		t.Errorf("the deferred loss's segment's row got %v (%v), want Sealed and standing", got, err)
	}
}

// emptyingHarness is the wiring the open hands the emptying over a rebuilt
// state, built here from the same pieces the assembled journal runs: the
// liveness tracker filled from the state's own registrations — SetMember
// over the assembled set, RegisterAtOpen of its lists, the one OpenDone
// closing the window, the registration the journal's open itself makes —
// and the retirement executor over the assembled ledger with a pool of the
// given targets, unstarted, for the executor's own Retire runs its recycles
// with no worker behind it. The maker draws its ids above every id in the
// directory, the seed a running journal's own maker takes.
type emptyingHarness struct {
	deps Deps
	pool *segment.Pool
}

func newEmptyingHarness(t *testing.T, r *rig, st *State, targets ...segment.Targets) *emptyingHarness {
	t.Helper()
	poolTargets := segment.Targets{}
	if len(targets) > 0 {
		poolTargets = targets[0]
	}
	set := st.Assembled.Set
	tr := reclaim.New(st.Rebuilt.Index, nil)
	tr.SetMember(func(id core.SegmentID) bool {
		_, err := set.State(id)
		return err == nil
	})
	tr.RegisterAtOpen(st.Assembled.Live, st.Assembled.Forgets, st.Assembled.Unsynced)
	tr.OpenDone()
	maker := segment.NewMaker(r.seam.FS(), set, math.MaxInt64, st.Attached.MaxID, nil)
	pool := segment.NewPool(maker, poolTargets, nil, nil)
	// The spares the spares phase re-adopted reach the pool before it
	// starts, in their classes, as the open's own core hands them over.
	for _, a := range st.Spares.Adopted {
		if err := pool.Adopt(a.ID, a.Class); err != nil {
			t.Fatalf("the harness pool's adoption of spare %d: %v", a.ID, err)
		}
	}
	pool.Start()
	t.Cleanup(pool.Stop)
	cache := segment.NewDescriptors(r.seam.FS(), format.SegmentName, core.NewDescriptorBudget(8))
	t.Cleanup(func() {
		if err := cache.Shutdown(); err != nil {
			t.Errorf("the harness cache's Shutdown: %v", err)
		}
	})
	retirer := segment.NewRetirer(segment.RetirerConfig{
		Set:   set,
		Cache: cache,
		Pool:  pool,
		Maker: maker,
		FS:    r.seam.FS(),
	})
	d := r.runDeps()
	d.Liveness = tr
	d.Retire = retirer.Retire
	d.Dir = r.dir
	// The pool's own take of the repack reserve, the shape the open hands
	// the emptying's copies: the take, and when the class is short a wait
	// for one reserve spare and the take again. The pool was started above,
	// so the take finds the adopted spares and those the retire step's
	// recycles and the refill made.
	d.DrawReserve = func(floor core.SegmentID) (core.SegmentID, error) {
		id, err := pool.TakeReserveAbove(floor)
		if err == nil || !errors.Is(err, core.ErrNoSpace) {
			return id, err
		}
		if err := pool.WaitReserveSpare(t.Context()); err != nil {
			return 0, err
		}
		return pool.TakeReserveAbove(floor)
	}
	return &emptyingHarness{deps: d, pool: pool}
}

// flatDomains is a file append domain table that never blocks: the deferred
// registry's plant takes no domain.
type flatDomains struct{}

func (flatDomains) Lock(core.FileID) func() { return func() {} }

// registryDurability builds the real durability layer over the rig's
// directory, under the journal id the rig names, started over throwaway
// streams that write nothing: the instance whose deferred Lose registers
// against a segment the way a running journal's own does, and whose
// EndDeferral the harness hands the emptying for the same registry.
func (r *rig) registryDurability(t *testing.T, id core.JournalID) *durability.Durability {
	t.Helper()
	set := segment.NewSet(testSegCap, nil)
	fs := r.seam.FS()
	maker := segment.NewMaker(fs, set, math.MaxInt64, 0, nil)
	pool := segment.NewPool(maker, segment.Targets{}, nil, nil)
	cache := segment.NewDescriptors(fs, format.SegmentName, core.NewDescriptorBudget(1))
	d, err := durability.New(durability.Config{
		Clock:    kit.NewStopwatchClock(),
		Domains:  flatDomains{},
		Index:    index.New(1 << 26),
		Dir:      r.dir,
		Identity: id,
		Counters: &index.Counters{},
		MarkLoss: func(core.FileID, core.Extent, core.Version) (index.HeldDelta, []core.SegmentID, error) {
			return index.HeldDelta{}, nil, nil
		},
	})
	if err != nil {
		t.Fatalf("the registry durability: %v", err)
	}
	// The cleanups run last in, first out: the layer stops, then its
	// throwaway streams close, then their cache shuts down.
	t.Cleanup(func() {
		if err := cache.Shutdown(); err != nil {
			t.Errorf("the registry durability's cache Shutdown: %v", err)
		}
	})
	streams := segment.NewStreams(1, id, set, pool, cache, d.Sink)
	t.Cleanup(func() {
		if err := streams.Close(); err != nil {
			t.Errorf("the registry durability's streams Close: %v", err)
		}
	})
	t.Cleanup(d.Stop)
	d.Start(streams)
	return d
}

// RegistryPlant defers l through dur the way the layer's own deferring Lose
// does: the piece is taken and the registry entry against l.Seg is stored,
// and no record appends.
func RegistryPlant(t *testing.T, dur *durability.Durability, l durability.Loss) {
	t.Helper()
	l.Deferred = true
	if _, err := dur.Lose(context.Background(), l); err != nil {
		t.Fatalf("the registry plant's deferred Lose: %v", err)
	}
}

// deferLossAgainst registers one deferred loss against seg in the
// per-directory registry of the rig's directory under the journal id, the
// way a running journal's own deferral does: a real Durability instance,
// started over throwaway streams that write nothing, whose deferred Lose
// takes the piece and the entry and appends no record — so the retire
// step's reader answers the entry the same registry's failed-list reader
// answers its own.
func (r *rig) deferLossAgainst(t *testing.T, id core.JournalID, seg core.SegmentID, l durability.Loss) {
	t.Helper()
	l.Deferred, l.Seg = true, seg
	RegistryPlant(t, r.registryDurability(t, id), l)
}

// TestEndedSegmentEmptiedOnce holds the row at the emptying's own caller:
// every record an ended segment held reads back after the open exactly once,
// with its numbers kept, the segment's file gone with a synced unlink, and
// the second run of the recovery plus the emptying over the same directory —
// the shape a reopen takes — draws no spare, writes no target and unlinks
// nothing, for the copy the first open sealed is what the second one serves.
// The row catches two ended segments skipping each other's copies — each
// would go on copying the other's record forever — and a copy trusted from
// the page cache — a second open that re-read nothing would serve it.
func TestEndedSegmentEmptiedOnce(t *testing.T) {
	file := tailFile()
	r := newRig(t)
	r.writeFormat()
	// Two ended segments, each holding a record of its own file and the
	// same record of the shared file: the shared record is copied once —
	// the first-ended segment's copy is the other's standing copy — and
	// each of the own records is copied once, never merged, numbers kept.
	a := collectWrite(7, file, 10, 10, 0x4100, 16)
	b := collectWrite(7, tailFile(), 11, 11, 0, 16)
	shared := collectWrite(7, file, 12, 12, 0x8100, 16)
	r.plantSeg(5, damageHeader(buildSeg(5, a, shared)))
	r.plantSeg(6, damageHeader(buildSeg(6, b, shared)))

	d := r.runDeps()
	st, err := Run(context.Background(), d, core.Version{}, journalID, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	h := newEmptyingHarness(t, r, &st, segment.Targets{Reserve: 2})
	if err := Emptying(context.Background(), h.deps, &st, journalID, core.Version{}, nil); err != nil {
		t.Fatalf("Emptying: %v", err)
	}

	for _, id := range []core.SegmentID{5, 6} {
		if slices.Contains(r.listing(), format.SegmentName(id)) {
			t.Errorf("the ended segment %d's file is still on disk, want it unlinked by the emptying", id)
		}
		if _, err := st.Assembled.Set.State(id); err == nil {
			t.Errorf("the assembled set still holds segment %d's row, want it gone with the retirement", id)
		}
	}
	// The shared record copied once: exactly one copy of sequence 12
	// verifies anywhere on disk, over the shared extent, its numbers kept.
	countShared, placedShared := 0, core.FileID{}
	for _, name := range r.listing() {
		kind, id := format.ClassifyName(name)
		if kind != format.NameSegment {
			continue
		}
		bs, err := r.seam.Durable(name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		walk, werr := format.ScanSegmentHeaders(bytes.NewReader(bs), int64(len(bs)), journalID, id)
		if werr != nil {
			t.Fatalf("walking %s: %v", name, werr)
		}
		for _, rec := range walk.Records {
			if rec.Header.Seq != 12 {
				continue
			}
			countShared++
			placedShared = rec.Header.File
			if rec.Header.Version.Counter != 12 {
				t.Errorf("the shared record's copy kept version %d, want its own 12", rec.Header.Version.Counter)
			}
		}
	}
	if countShared != 1 {
		t.Fatalf("the shared record verifies %d times on disk after the emptying, want the one copy: two ended segments must not copy each other's records", countShared)
	}
	if placedShared != file {
		t.Errorf("the shared record's copy names file %v, want the shared record's own %v", placedShared, file)
	}
	// Everything the segments held that serves reads back from the copies.
	for _, look := range []struct {
		file core.FileID
		off  int64
		want core.Seq
	}{{file, 0x4100, 10}, {tailFile(), 0, 11}, {file, 0x8100, 12}} {
		placed, ok := st.Rebuilt.Index.Lookup(look.file, look.off)
		if !ok {
			t.Fatalf("the index answers no record at %d of the file ending %v, want the copy of the record the ended segment held", look.off, look.file)
		}
		if placed.Record.Segment == 5 || placed.Record.Segment == 4 {
			t.Errorf("the record at %d still serves from the unlinked segment %d, want it repointed to the copy", look.off, placed.Record.Segment)
		}
		if placed.Seq != look.want {
			t.Errorf("the record at %d serves seq %d, want the record the ended segment held, seq %d, numbers kept", look.off, placed.Seq, look.want)
		}
	}

	// The second open: the emptying runs again over the same directory and
	// finds no ended segment — the copies the first open sealed are what it
	// reads and serves, and no new copy or unlink happens.
	st2, err := Run(context.Background(), d, core.Version{}, journalID, nil)
	if err != nil {
		t.Fatalf("the second Run: %v", err)
	}
	for _, row := range st2.Assembled.Set.Snapshot() {
		if row.State == segment.Ended {
			t.Fatalf("the second open ends segment %d again, want every ended segment emptied once and gone", row.ID)
		}
	}
	h2 := newEmptyingHarness(t, r, &st2, segment.Targets{Reserve: 2})
	if err := Emptying(context.Background(), h2.deps, &st2, journalID, core.Version{}, nil); err != nil {
		t.Fatalf("the second Emptying: %v", err)
	}
	placed2, ok := st2.Rebuilt.Index.Lookup(file, 0x8100)
	if !ok || placed2.Record.Segment == 5 || placed2.Record.Segment == 4 {
		t.Fatalf("the second open does not serve the shared record from the sealed copy: %+v (%v)", placed2, ok)
	}
	if got, err := st2.Assembled.Set.State(placed2.Record.Segment); err != nil || got != segment.Sealed {
		t.Errorf("the copy the first open sealed reads back as %v (%v), want Sealed", got, err)
	}
}

// TestAnUnsealedRowsAsideDuplicateIsNoStandingCopy holds the narrowing of the
// aside skip to sealed rows: an Active segment with no seal holds a duplicate
// of a live loss record in an ended segment, and the emptying still carries
// the loss into a sealed spare, for an unsealed row's bytes are no proven
// word. The row catches a skip that trusted the Active row: the ended
// source would be unlinked with its only sealed copy of the loss gone.
//
// The content-record skip has no row of its own: the index serves a record's
// verified duplicate over the ended segment's copy, so the ended record is
// never named and no skip is asked.
func TestAnUnsealedRowsAsideDuplicateIsNoStandingCopy(t *testing.T) {
	const tag = core.ShareTag(7)
	file := tailFile()
	r := newRig(t)
	r.writeFormat()
	loss := collectLoss(tag, file, 40, 41, 941, 0x200, 64, 1<<8)
	// The record the unsynced loss names, held by a sealed segment, keeps
	// the loss from answering droppable.
	older, _ := buildSealed(6, collectWrite(tag, file, 40, 40, 0x200, 64))
	r.plantSeg(6, older)
	r.plantSeg(5, damageHeader(buildSeg(5, loss)))
	r.plantSeg(4, buildSeg(4, loss))

	st, err := Run(context.Background(), r.runDeps(), core.Version{}, journalID, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got, err := st.Assembled.Set.State(4); err != nil || got != segment.Active {
		t.Fatalf("the unsealed duplicate's row got %v (%v), want Active: the fixture no longer plants the case", got, err)
	}
	h := newEmptyingHarness(t, r, &st, segment.Targets{Reserve: 2})
	if err := Emptying(context.Background(), h.deps, &st, journalID, core.Version{}, nil); err != nil {
		t.Fatalf("Emptying: %v", err)
	}

	if slices.Contains(r.listing(), format.SegmentName(5)) {
		t.Errorf("the ended source is still on disk, want it unlinked once its loss record is carried")
	}
	var sealedCopies int
	for _, name := range r.listing() {
		kind, id := format.ClassifyName(name)
		if kind != format.NameSegment || id == 4 || id == 6 {
			continue
		}
		bs, err := r.seam.Durable(name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		walk, werr := format.ScanSegmentHeaders(bytes.NewReader(bs), int64(len(bs)), journalID, id)
		if werr != nil {
			continue
		}
		for _, rec := range walk.Records {
			if rec.Header.Kind == format.KindLoss && rec.Header.Seq == 41 {
				sealedCopies++
			}
		}
	}
	if sealedCopies != 1 {
		t.Errorf("%d copies of the loss record of seq 41 sit in the emptying's spare, want the one it carries: an unsealed row's duplicate is no standing copy", sealedCopies)
	}
}

// TestDeferredLossWrittenIntoTheCopy holds the deferred-loss leg:
// a deferred loss whose dropped record sits in an ended segment is written
// into the copy the emptying makes, the deferral ends — the registry gives
// the entry up on the record's durability — and the next open finds the
// record on disk, places it and defers nothing. The row catches a segment
// retired without its deferred record: the deferral would strand with the
// record nobody could ever write again.
func TestDeferredLossWrittenIntoTheCopy(t *testing.T) {
	file := tailFile()
	r := newRig(t)
	r.writeFormat()
	// The ended segment: its rotted header ends it whole, and its one
	// surviving record still serves — the emptying copies it out beside the
	// deferred loss's record. The dropped record itself is not on disk: a
	// journal of this process deferred the loss against the segment, the
	// registry entry that survives it, before the crash reopened here.
	write := collectWrite(7, file, 29, 29, 0, 32)
	r.plantSeg(4, damageHeader(buildSeg(4, write)))
	l := durability.Loss{
		Share:    7,
		File:     file,
		Extent:   core.Extent{Off: 0, Len: 32},
		Version:  core.Version{Counter: 30},
		Seq:      30,
		Reason:   core.LossCorrupt,
		Synced:   true,
		Deferred: true,
		Seg:      4,
	}
	dur := r.registryDurability(t, journalID)
	if _, err := durability.DeferredFor(r.dir, journalID); err != nil {
		t.Fatalf("the empty registry read: %v", err)
	}
	RegistryPlant(t, dur, l)

	d := r.runDeps()
	st, err := Run(context.Background(), d, core.Version{}, journalID, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	h := newEmptyingHarness(t, r, &st, segment.Targets{Reserve: 1})
	h.deps.EndDeferral = dur.EndDeferral
	if err := Emptying(context.Background(), h.deps, &st, journalID, core.Version{}, nil); err != nil {
		t.Fatalf("Emptying: %v", err)
	}

	if slices.Contains(r.listing(), format.SegmentName(4)) {
		t.Errorf("the ended segment holding the deferred drop is still on disk, want it unlinked by the emptying")
	}
	// The loss record rode the copy: the sealed target the emptying made
	// holds exactly the one loss record, naming the dropped seq 30 over the
	// dropped extent.
	found := false
	for _, name := range r.listing() {
		kind, id := format.ClassifyName(name)
		if kind != format.NameSegment || id == 4 {
			continue
		}
		bs, err := r.seam.Durable(name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		walk, werr := format.ScanSegmentHeaders(bytes.NewReader(bs), int64(len(bs)), journalID, id)
		if werr != nil {
			t.Fatalf("walking %s: %v", name, werr)
		}
		for _, rec := range walk.Records {
			if rec.Header.Kind != format.KindLoss {
				continue
			}
			body, berr := format.VerifyRecordBody(bs[rec.Header.Offset+format.RecordHeaderSize:rec.Header.Offset+rec.Header.Size()], journalID, rec.Header)
			if berr != nil {
				t.Fatalf("verify the loss record the copy wrote: %v", berr)
			}
			if body.Loss.Dropped != 30 {
				t.Errorf("the loss record the copy wrote names the dropped record %d, want the deferred drop's 30", body.Loss.Dropped)
			}
			found = true
		}
	}
	if !found {
		t.Fatalf("no loss record on disk after the emptying, want the deferred record written into the copy")
	}
	// The deferral ended with the copy: the registry answers nothing.
	after, err := durability.DeferredFor(r.dir, journalID)
	if err != nil {
		t.Fatalf("the registry read after the emptying: %v", err)
	}
	if len(after) != 0 {
		t.Errorf("the registry still answers %+v after the copy landed, want the deferral ended", after)
	}

	// The next open finds the record on disk and defers nothing: the kinds
	// pass places the found record and no leg asks the deferred arm.
	st2, err := Run(context.Background(), d, core.Version{}, journalID, nil)
	if err != nil {
		t.Fatalf("the second Run: %v", err)
	}
	if deferred, derr := durability.DeferredFor(r.dir, journalID); derr != nil || len(deferred) != 0 {
		t.Errorf("the second open found deferred losses %+v (err %v), want none: the record on disk stands for the deferral", deferred, derr)
	}
	if len(st2.Kinds.Losses) != 1 {
		t.Errorf("the second open collected %d loss events, want the one found record", len(st2.Kinds.Losses))
	}
}

// TestEmptyingWithoutASpareLeavesTheSegment holds the no-spare half of the
// emptying's refusal: an ended segment whose survivors need a copy draws a
// spare of the pool's repack reserve — the closure the open wires, never a
// list recovery keeps — and a draw that answers the pool's shortage fails
// the open with that error, leaving the ended segment exactly as it
// stands, its bytes the next open's to copy.
func TestEmptyingWithoutASpareLeavesTheSegment(t *testing.T) {
	file := tailFile()
	r := newRig(t)
	r.writeFormat()
	r.plantSeg(2, damageHeader(buildSeg(2, collectWrite(7, file, 2, 2, 0x4100, 16))))

	st, err := Run(context.Background(), r.runDeps(), core.Version{}, journalID, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	// A real pool whose repack reserve targets no spare: its take answers
	// its own refusal.
	h := newEmptyingHarness(t, r, &st, segment.Targets{})
	before, err := r.seam.Durable(format.SegmentName(2))
	if err != nil {
		t.Fatalf("read the ended segment: %v", err)
	}
	err = Emptying(context.Background(), h.deps, &st, journalID, core.Version{}, nil)
	if err == nil {
		t.Fatalf("the emptying over a reserve with no spare served, want the refusal")
	}
	var noSpace *core.NoSpaceError
	if !errors.As(err, &noSpace) {
		t.Fatalf("the emptying's refusal over a reserve with no spare answered %v, want the pool's own no-space word", err)
	}
	if after, aerr := r.seam.Durable(format.SegmentName(2)); aerr != nil || !slices.Equal(before, after) {
		t.Fatalf("the refusing emptying changed the ended segment: got %x (err %v), want the planted bytes", after, aerr)
	}
	if got, serr := st.Assembled.Set.State(2); serr != nil || got != segment.Ended {
		t.Errorf("the refusing emptying left the ended segment's row %v (%v), want Ended and standing", got, serr)
	}
}

// TestFreedTakeAfterDurableUnlink holds the relief: a record-less take
// withheld by the spares phase over a predecessor the open ends is freed
// once that predecessor's unlink is durable, and it is the first spare the
// next ended segment's copy draws. The crash leg of the relief is the
// ordering itself: the unlink reaches durability before the take is
// adopted, so a crash between the two leaves the next open reporting the
// predecessor damaged against its own evidence, never torn into a spare
// a crash resurrected.
func TestFreedTakeAfterDurableUnlink(t *testing.T) {
	r := newRig(t)
	r.writeFormat()
	// The predecessor: a sealed, record-less segment the caller's sync mark
	// names, so the marks raise ends it — the one ended shape a naming take
	// survives, its claim sealed and proven. The take that names it:
	// cap-sized, record-less, withheld by the spares phase's verdict until
	// the predecessor is gone. A second ended segment whose copy needs a
	// spare: the freed take is drawn before the pool's reserve.
	pred, _ := buildSealed(2)
	r.write(format.SegmentName(2), append(pred, make([]byte, testSegCap-int64(len(pred)))...))
	take := raw.SegmentHeader{Identity: journalID, ID: 3, Predecessor: 2}.Bytes()
	r.write(format.SegmentName(3), append(take, make([]byte, testSegCap-int64(len(take)))...))
	r.plantSeg(4, damageHeader(buildSeg(4, collectWrite(7, tailFile(), 5, 5, 0, 16))))

	d := r.runDeps()
	d.SegmentCap = testSegCap
	mark := core.SyncMark{Segment: 2, Sequence: 9, Offset: format.SegmentHeaderSize, Through: format.SegmentHeaderSize}
	st, err := Run(context.Background(), d, core.Version{}, journalID, []core.SyncMark{mark})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got, _ := st.Assembled.Set.State(2); got != segment.Ended {
		t.Fatalf("the marked record-less predecessor's row got %v, want Ended by the raise", got)
	}
	if !slices.Contains(st.Spares.Held, core.SegmentID(3)) {
		t.Fatalf("the spares phase did not withhold the take 3 naming the ended predecessor, want it held on the verdict")
	}
	h := newEmptyingHarness(t, r, &st, segment.Targets{Reserve: 4})
	if err := Emptying(context.Background(), h.deps, &st, journalID, core.Version{}, []core.SyncMark{mark}); err != nil {
		t.Fatalf("Emptying: %v", err)
	}
	// The one copy the open makes drew the freed take, not the pool's
	// reserve: the retire step's durable unlink of the predecessor freed it
	// before the emptying step ran, so the take's file is the sealed copy
	// now, its old header gone and the copy's records in its place.
	b, err := r.seam.Durable(format.SegmentName(3))
	if err != nil {
		t.Fatalf("read the reused take: %v", err)
	}
	walk, werr := format.ScanSegmentHeaders(bytes.NewReader(b), int64(len(b)), journalID, 3)
	if werr != nil {
		t.Fatalf("walking the reused take: %v", werr)
	}
	if len(walk.Records) != 1 || walk.Records[0].Header.Seq != 5 {
		t.Fatalf("the reused take holds %d records (want the one of seq 5 the emptied segment held), err %v", len(walk.Records), werr)
	}
	for _, id := range []core.SegmentID{2, 4} {
		if slices.Contains(r.listing(), format.SegmentName(id)) {
			t.Errorf("the ended segment %d is still on disk, want both unlinked by the emptying", id)
		}
	}
}

// TestReadFoundRotRidesTheLossRecord holds the read-found leg: a payload
// whose header verified at the open's walk, with no seal, no successor and
// no synced-through covering it, and whose body fails the copy's whole read,
// the rot landing between the two, leaves the copy with no record of it and
// an unsynced loss record naming its sequence number over its extent, and
// the running index answers the extent missing, never the bytes of a
// placement the unlink takes away.
func TestReadFoundRotRidesTheLossRecord(t *testing.T) {
	file := tailFile()
	r := newRig(t)
	r.writeFormat()
	// The ended shape: a torn slot below a verifying record, from the
	// planted bytes the classing reads; the recorded payload after the
	// record the slot tore — the seq 41 record whose body the rot below
	// lands on.
	first := collectWrite(7, file, 40, 40, 0, 16)
	hb := raw.SegmentHeader{Identity: journalID, ID: 2}.Bytes()
	first.Identity, first.Segment, first.RecordOffset = journalID, 2, uint32(len(hb))
	hb = append(hb, first.Bytes()...)
	hb = append(hb, make([]byte, 16)...) // the torn slot
	second := collectWrite(7, file, 41, 41, 0x4100, 16)
	second.Identity, second.Segment, second.RecordOffset = journalID, 2, uint32(len(hb))
	hb = append(hb, second.Bytes()...)
	rotAt := len(hb) - 1 // the seq 41 record's last payload byte
	r.plantSeg(2, hb)

	d := r.runDeps()
	st, err := Run(context.Background(), d, core.Version{}, journalID, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if _, ok := st.Rebuilt.Index.Lookup(file, 0x4100); !ok {
		t.Fatalf("the index answers no record at 0x4100 at the open, want the walked record placed: its rot is the copy leg's to find")
	}
	if _, ok := st.Rebuilt.Index.Lookup(file, 0); !ok {
		t.Fatalf("the index answers no record at 0 at the open, want the walked record placed below the torn slot")
	}
	// The rot lands between the open's reads and the emptying's: the device
	// failing under the open itself, the body byte the emptying verifies
	// whole before copying.
	seg, err := r.seam.Durable(format.SegmentName(2))
	if err != nil {
		t.Fatalf("read the ended segment to rot: %v", err)
	}
	b := make([]byte, 1)
	b[0] = seg[rotAt] ^ 0xff
	fo, err := r.seam.Open(format.SegmentName(2))
	if err != nil {
		t.Fatalf("open the ended segment to rot: %v", err)
	}
	if _, err := fo.WriteAt(b, int64(rotAt)); err != nil {
		t.Fatalf("rot the ended segment's payload: %v", err)
	}
	if err := errors.Join(fo.Sync(), fo.Close()); err != nil {
		t.Fatalf("sync the rot: %v", err)
	}
	h := newEmptyingHarness(t, r, &st, segment.Targets{Reserve: 1})
	if err := Emptying(context.Background(), h.deps, &st, journalID, core.Version{}, nil); err != nil {
		t.Fatalf("Emptying: %v", err)
	}
	if slices.Contains(r.listing(), format.SegmentName(2)) {
		t.Errorf("the ended segment 2 is still on disk, want it unlinked by the emptying")
	}
	// The rot's record left no copy: no record of seq 41 verifies anywhere,
	// and the loss record naming it rides the sealed target.
	nameLoss := false
	for _, name := range r.listing() {
		kind, id := format.ClassifyName(name)
		if kind != format.NameSegment {
			continue
		}
		bs, derr := r.seam.Durable(name)
		if derr != nil {
			t.Fatalf("read %s: %v", name, derr)
		}
		walk, werr := format.ScanSegmentHeaders(bytes.NewReader(bs), int64(len(bs)), journalID, id)
		if werr != nil {
			t.Fatalf("walking %s: %v", name, werr)
		}
		for _, rec := range walk.Records {
			if rec.Header.Seq == 41 {
				t.Errorf("a copy of the rotted record of seq 41 verifies in %s, want the rot left out of the copy", name)
			}
			if rec.Header.Kind != format.KindLoss {
				continue
			}
			body, berr := format.VerifyRecordBody(bs[rec.Header.Offset+format.RecordHeaderSize:rec.Header.Offset+rec.Header.Size()], journalID, rec.Header)
			if berr != nil {
				t.Fatalf("verify the loss record the copy wrote: %v", berr)
			}
			if body.Loss.Dropped != 41 {
				t.Errorf("the loss record names the dropped record %d, want the rotted record's 41", body.Loss.Dropped)
			}
			if body.Loss.Synced {
				t.Errorf("the loss record carries a synced bit, want none: no sync proved the record its header named")
			}
			nameLoss = true
		}
	}
	if !nameLoss {
		t.Fatalf("no loss record names the rotted record, want the copy carrying its mask")
	}
	if _, ok := st.Rebuilt.Index.Lookup(file, 0x4100); ok {
		t.Errorf("the index still serves a record at 0x4100 from an unlinked segment, want the rot's cut leaving the extent missing")
	}
}

// TestReadFoundRotInASealedHolderReadsLost holds the read-found leg over a
// record its seal proves synced: a sealed holder emptied for its failed
// records, whose surviving write's body rots between the open's walk and the
// copy's whole read, leaves a loss record carrying the synced bit, and the
// index answers the extent lost, a loss marker, never a hole an older copy
// could fill at the next open.
func TestReadFoundRotInASealedHolderReadsLost(t *testing.T) {
	f := newFailedFixture(t)
	st, h := f.open(t)
	placed, ok := st.Rebuilt.Index.Lookup(f.file, f.survOff)
	if !ok || placed.Record.Segment != 5 {
		t.Fatalf("the survivor serves %+v (%v) at the open, want it placed in the sealed holder 5", placed, ok)
	}
	rotByte(t, f.r, format.SegmentName(5), int64(placed.Record.Offset)+format.RecordHeaderSize)

	if err := Emptying(context.Background(), h.deps, st, journalID, core.Version{}, nil); err != nil {
		t.Fatalf("Emptying: %v", err)
	}
	if slices.Contains(f.r.listing(), format.SegmentName(5)) {
		t.Errorf("the sealed holder 5 is still on disk, want it unlinked by the emptying")
	}
	found := false
	for _, name := range f.r.listing() {
		kind, id := format.ClassifyName(name)
		if kind != format.NameSegment {
			continue
		}
		bs, err := f.r.seam.Durable(name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		walk, werr := format.ScanSegmentHeaders(bytes.NewReader(bs), int64(len(bs)), journalID, id)
		if werr != nil {
			t.Fatalf("walking %s: %v", name, werr)
		}
		for _, rec := range walk.Records {
			if rec.Header.Kind != format.KindLoss {
				continue
			}
			body, berr := format.VerifyRecordBody(bs[rec.Header.Offset+format.RecordHeaderSize:rec.Header.Offset+rec.Header.Size()], journalID, rec.Header)
			if berr != nil || body.Loss.Dropped != f.survSeq {
				continue
			}
			found = true
			if !body.Loss.Synced {
				t.Errorf("the loss record of the rotted seq %d carries no synced bit, want it: the holder's seal proves the record synced", f.survSeq)
			}
		}
	}
	if !found {
		t.Fatalf("no loss record names the rotted record of seq %d, want the copy carrying it", f.survSeq)
	}
	lost := false
	for _, ch := range st.Rebuilt.Index.Since(f.file, core.Version{}) {
		if ch.Kind == core.ChangeLoss && ch.Extent.Off <= f.survOff && f.survOff < ch.Extent.Off+ch.Extent.Len {
			lost = true
		}
	}
	if !lost {
		t.Errorf("the index yields no loss marker over the rotted extent at %d, want the synced loss reading lost", f.survOff)
	}
}

// rotByte flips the byte at off of the file name and syncs it: the device
// failing under the open, between its walk and the emptying's reads.
func rotByte(t *testing.T, r *rig, name string, off int64) {
	t.Helper()
	seg, err := r.seam.Durable(name)
	if err != nil {
		t.Fatalf("read %s to rot: %v", name, err)
	}
	fo, err := r.seam.Open(name)
	if err != nil {
		t.Fatalf("open %s to rot: %v", name, err)
	}
	if _, err := fo.WriteAt([]byte{seg[off] ^ 0xff}, off); err != nil {
		t.Fatalf("rot %s at %d: %v", name, off, err)
	}
	if err := errors.Join(fo.Sync(), fo.Close()); err != nil {
		t.Fatalf("sync the rot of %s: %v", name, err)
	}
}

// TestCrashAtEveryEmptyingStep holds the crash matrix over the emptying's
// own seam points: the copy target's append and its seal's syncs, the
// deferred loss record's ride on the same target, and the source's own
// unlink. A power cut lands at each held step mid-flight — the seam's crash
// discards the in-flight call and everything no sync proved — and the crash
// shape runs the reopen twice: the process-killed open (the crash, then the
// state settled), then a second power loss right after that open's own
// unlink has gone durable. At each full settle the three invariants: no
// record held twice — the survivor's seq verifies in exactly one placement
// on disk, the equal-numbers collapse answering a copy the crash raced —
// each deferred loss named by exactly one loss record, and the superseded
// content never served (the survivor reads back whole wherever it sits,
// never a hole an unmasked older piece would fill).
//
// The cuts are the target's append, both syncs of its seal (the records',
// then the marker's, footer's and trailer's), the append into the retry's
// own spare after the first spare's syncs failed, the source's unlink, and
// the directory sync that follows the unlink, held with the removal applied
// and not yet durable, so the cut resurrects the source.
//
// The directory-sync hold is made while the unlink is still parked and
// matches every sync_dir, so no directory sync of any caller, the retirer's
// or the pool refill's, completes between the unlink and the cut: whichever
// reaches the hold first, the removal is applied and not durable, and the row
// asserts that the crash brings the source back.
//
// ponytail: the retire step's own recycles are out of this matrix — the
// fixture holds no dead sealed segments, so each run's retire step retires
// nothing — and the pool's spares are prewarmed before the hold engages, so
// a held write can only be the copy's own. Upgrade the fixture with a dead
// full seal when the retire step's crash points owe their own rows.
func TestCrashAtEveryEmptyingStep(t *testing.T) {
	file := tailFile()
	droppedFile := srcFileFileID()
	// The survivor's numbers, the deferred loss's named record, and the
	// deferred loss's own extent.
	const (
		survSeq   = core.Seq(2)
		dropSeq   = core.Seq(22)
		survOff   = 0x4100
		survBytes = 32
	)

	// plantOne makes the directory the crash runs over: the ended source
	// segment 2 holding the live survivor, and a deferred loss against the
	// segment whose dropped record the copy leg owes the loss record of.
	plantOne := func(t *testing.T) (*rig, Deps) {
		r := newRig(t)
		r.writeFormat()
		r.plantSeg(2, damageHeader(buildSeg(2, collectWrite(7, file, uint64(survSeq), uint64(survSeq), survOff, survBytes))))
		d := r.runDeps()
		return r, d
	}
	// settle runs the recovery and the emptying over the crashed directory —
	// the process-killed reopen — wiring the harness with the registry's
	// own EndDeferral, and asserts the three invariants. Its answers name
	// the state it settled for the caller that reads more from it.
	settle := func(t *testing.T, r *rig, d Deps, dur *durability.Durability) *State {
		t.Helper()
		// The view the deps reach the directory through is bound to the
		// crashes it had seen when it was made, so each reopen past a
		// power cut takes a fresh one: the pinning is the seam's epoch rule.
		d.FS, d.Dir = r.seam.FS(), r.dir
		st, err := Run(context.Background(), d, core.Version{}, journalID, nil)
		if err != nil {
			t.Fatalf("the reopen's Run: %v", err)
		}
		h := newEmptyingHarness(t, r, &st, segment.Targets{Reserve: 2})
		if dur != nil {
			h.deps.EndDeferral = dur.EndDeferral
		}
		if err := Emptying(context.Background(), h.deps, &st, journalID, core.Version{}, nil); err != nil {
			t.Fatalf("the reopen's Emptying: %v", err)
		}
		assertInvariants(t, r, &st, file, survSeq, survOff, dropSeq)
		return &st
	}

	for _, tc := range []struct {
		name string
		op   string
		// nth is the call of op the crash lands in: the seal's second sync
		// is the one that carries the marker, the footer and the trailer.
		nth int
		// retry makes the first spare's syncs fail, so the crash lands in
		// the retry's own spare.
		retry bool
		// afterRemove lands the crash in the directory sync that follows
		// the source's unlink instead, the removal not yet durable.
		afterRemove bool
	}{
		{name: "target_append", op: "write", nth: 1},
		{name: "target_seal_first_sync", op: "sync", nth: 1},
		{name: "target_seal_second_sync", op: "sync", nth: 2},
		{name: "retry_spare_append", op: "write", nth: 1, retry: true},
		{name: "source_unlink", op: "remove", nth: 1},
		{name: "directory_sync_after_unlink", op: "remove", nth: 1, afterRemove: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, d := plantOne(t)

			// The deferred loss against the ended source, planted the way a
			// running journal's own deferral does.
			dur := r.registryDurability(t, journalID)
			RegistryPlant(t, dur, durability.Loss{
				Share:    7,
				File:     droppedFile,
				Extent:   core.Extent{Off: 0, Len: 16},
				Version:  core.Version{Counter: 30},
				Seq:      dropSeq,
				Reason:   core.LossCorrupt,
				Synced:   false,
				Deferred: true,
				Seg:      2,
			})
			st, err := Run(context.Background(), d, core.Version{}, journalID, nil)
			if err != nil {
				t.Fatalf("the first Run: %v", err)
			}
			h := newEmptyingHarness(t, r, &st, segment.Targets{Reserve: 2})
			h.deps.EndDeferral = dur.EndDeferral
			// Prewarm the pool's spares so a held write is the copy's own:
			// the settle the draw waits on already happened.
			if err := h.pool.WaitSpares(context.Background()); err != nil {
				t.Fatalf("the pool's prewarm: %v", err)
			}
			// The crash keys: the spares the copy draws are the first ids
			// above every id the directory holds, in order, and the source's
			// own name for the unlink leg.
			spare1 := format.SegmentName(st.Attached.MaxID + 1)
			spare2 := format.SegmentName(st.Attached.MaxID + 2)
			key := spare1
			if tc.retry {
				r.failSyncs(spare1, 0) // the first target never syncs: the copy retries in the next spare
				key = spare2
			}
			if tc.op == "remove" {
				key = format.SegmentName(2)
			}

			crashed := make(chan error, 1)
			reach := func(hold *kit.Hold, what string) {
				t.Helper()
				select {
				case <-hold.Reached():
				case err := <-crashed:
					t.Fatalf("the emptying returned (%v) before the held step %s", err, what)
				}
			}
			hold := r.seam.HoldFirst(t, kitOp(tc.op), key)
			wantResurrect := false
			go func() {
				crashed <- Emptying(context.Background(), h.deps, &st, journalID, core.Version{}, nil)
			}()
			reach(hold, tc.op+" on "+key)
			switch {
			case tc.nth == 2:
				// The first call is parked and already matched its hold, so
				// a hold made now catches the call after it.
				next := r.seam.HoldFirst(t, kitOp(tc.op), key)
				hold.Release()
				reach(next, "the second "+tc.op+" on "+key)
				hold = next
			case tc.afterRemove:
				next := r.seam.Hold(t, kit.OpSyncDir, "*")
				hold.Release()
				reach(next, "the directory sync after the unlink")
				hold = next
				// Another caller's directory sync, the pool refill's, may reach
				// the hold before the released unlink applies; every directory
				// sync stays parked, so the test waits on the unlink itself
				// with nothing made durable meanwhile.
				deadline := time.Now().Add(30 * time.Second)
				for slices.Contains(r.listing(), format.SegmentName(2)) {
					if time.Now().After(deadline) {
						t.Fatalf("the source is still listed with the directory syncs held, want the unlink applied before the cut")
					}
					time.Sleep(time.Millisecond)
				}
				wantResurrect = true
			}
			if err := r.seam.Crash(); err != nil {
				t.Fatalf("the power cut at the held step: %v", err)
			}
			if wantResurrect && !slices.Contains(r.listing(), format.SegmentName(2)) {
				t.Errorf("the source is gone after a crash at the directory sync, want it resurrected: the unlink was not yet durable")
			}
			hold.Release()
			select {
			case err := <-crashed:
				_ = err
			case <-time.After(30 * time.Second):
				t.Fatalf("the crashed emptying never returned: a hold at the %s step wedged the phase past the crash", tc.name)
			}
			if tc.retry {
				r.faults = nil
				r.scheduleFaults() // the reopen runs on a device that syncs again
			}

			// The process-killed reopen: settled state and invariants.
			settle(t, r, d, dur)

			// The second power loss, right after that open's own unlink
			// went durable: the removal survives exactly and the third open
			// settles the same invariants.
			if err := r.seam.Crash(); err != nil {
				t.Fatalf("the second power loss: %v", err)
			}
			settle(t, r, d, dur)
		})
	}
}

// assertInvariants holds the crash matrix's three answers over a settled
// state: the survivor verifies in exactly one whole placement (no copy the
// crash raced sits a second time), the deferred loss is named by exactly one
// loss record on disk, and the survivor still serves its own extent whole
// — never the hole an unmasked superseded piece would read as.
func assertInvariants(t *testing.T, r *rig, st *State, file core.FileID, survSeq core.Seq, off int64, dropSeq core.Seq) {
	t.Helper()
	var copies, losses int
	for _, name := range r.listing() {
		kind, id := format.ClassifyName(name)
		if kind != format.NameSegment {
			continue
		}
		bs, err := r.seam.Durable(name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		walk, werr := format.ScanSegmentHeaders(bytes.NewReader(bs), int64(len(bs)), journalID, id)
		if werr != nil {
			continue // a crashed target the next open re-reads and settles
		}
		for _, rec := range walk.Records {
			if rec.Header.Seq == survSeq && rec.Header.Kind == format.KindWrite {
				copies++
			}
			if rec.Header.Kind != format.KindLoss {
				continue
			}
			if rec.Header.Offset+rec.Header.Size() > int64(len(bs)) {
				continue
			}
			body, berr := format.VerifyRecordBody(bs[rec.Header.Offset+format.RecordHeaderSize:rec.Header.Offset+rec.Header.Size()], journalID, rec.Header)
			if berr != nil {
				continue
			}
			if body.Loss.Dropped == dropSeq {
				losses++
			}
		}
	}
	if copies != 1 {
		t.Errorf("the survivor of seq %d verifies %d times on disk, want the one copy the emptying settles: a crash raced a copy it must not leave twice", survSeq, copies)
	}
	if losses != 1 {
		t.Errorf("%d loss records on disk name the dropped record %d, want exactly the one the copy leg owes", losses, dropSeq)
	}
	placed, ok := st.Rebuilt.Index.Lookup(file, off)
	if !ok {
		t.Fatalf("the index answers no record at %d of the survivor's file, want the settled copy serving it", off)
	}
	if placed.Seq != survSeq {
		t.Errorf("the extent at %d serves the record of seq %d, want the survivor's %d: the superseded content must never serve", off, placed.Seq, survSeq)
	}
	if slices.Contains(r.listing(), format.SegmentName(2)) {
		t.Errorf("the ended source's file is still on disk at the settle, want the unlink having taken it")
	}
}

// srcFileFileID answers the clone-source fixture's file id as its own, the
// one file the survivor's never shares.
func srcFileFileID() core.FileID {
	return core.NewFileID(srcFile())
}

// kitOp maps the crash-point table's kind word onto the seam's op string.
func kitOp(op string) string {
	switch op {
	case "write":
		return kit.OpWrite // the copy's own append into the target spare
	case "sync":
		return kit.OpSync // the seal writer's syncs over the target
	case "remove":
		return kit.OpRemove // the source's own unlink
	}
	t := op
	panic("the crash table named an op the seam does not hold: " + t)
}

// TestCopyPastOneCapIsAnOpenRefusal holds the class of a refusal that recurs
// on every retry: a batch needing more than one spare's cap cannot be copied
// whatever the pool holds, so the engine must see the open's own refusal and
// not a generic open failure.
func TestCopyPastOneCapIsAnOpenRefusal(t *testing.T) {
	h := format.RecordHeader{Kind: format.KindLoss, File: tailFile(), Extent: core.Extent{Off: 0, Len: 16}, Seq: 1, Version: core.Version{Counter: 1}, Segment: 2}
	body := format.RecordBody{Loss: format.Loss{Dropped: 1, Reason: format.LossCorrupt}}
	rec := format.AppendRecord(nil, journalID, h, body)
	c := &copier{deps: Deps{SegmentCap: 1}, identity: journalID}
	_, _, err := c.copy(context.Background(), []copyInput{{Bytes: rec, Segment: 2, At: 0, Seq: 1, Version: core.Version{Counter: 1}}})
	if !errors.Is(err, core.ErrOpenRefused) {
		t.Fatalf("a copy needing more than one cap answered %v, want an error matching core.ErrOpenRefused", err)
	}
}

// TestEmptyingTargetSyncFailure holds the copy target's failure at the
// emptying: a first spare whose syncs fail is a failed segment, ended and
// never sealed, and the copy lands whole in the next spare; a second failing
// spare in a row fails the emptying with the device's own error, leaving the
// ended source standing with its bytes.
func TestEmptyingTargetSyncFailure(t *testing.T) {
	plant := func(t *testing.T) (*rig, *State, *emptyingHarness) {
		r := newRig(t)
		r.writeFormat()
		r.plantSeg(2, damageHeader(buildSeg(2, collectWrite(7, tailFile(), 2, 2, 0x4100, 16))))
		st, err := Run(context.Background(), r.runDeps(), core.Version{}, journalID, nil)
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		h := newEmptyingHarness(t, r, &st, segment.Targets{Reserve: 2})
		if err := h.pool.WaitSpares(context.Background()); err != nil {
			t.Fatalf("the pool's prewarm: %v", err)
		}
		return r, &st, h
	}
	t.Run("the retry lands in the next spare", func(t *testing.T) {
		r, st, h := plant(t)
		first := core.SegmentID(st.Attached.MaxID + 1)
		r.failSyncs(format.SegmentName(first), 0)
		if err := Emptying(context.Background(), h.deps, st, journalID, core.Version{}, nil); err != nil {
			t.Fatalf("Emptying over a first target whose syncs fail: %v", err)
		}
		if got, err := st.Assembled.Set.State(first); err != nil || got != segment.Ended {
			t.Errorf("the failed target's row is %v (%v), want Ended: nothing of it was proved", got, err)
		}
		if slices.Contains(r.listing(), format.SegmentName(2)) {
			t.Errorf("the source is still on disk, want it unlinked once the retry's copy held")
		}
		placed, ok := st.Rebuilt.Index.Lookup(tailFile(), 0x4100)
		if !ok {
			t.Fatalf("the index answers no record at 0x4100, want the retry's copy")
		}
		if placed.Record.Segment == first {
			t.Errorf("the record serves from the failed target %d, want the retry's spare", first)
		}
		if got, err := st.Assembled.Set.State(placed.Record.Segment); err != nil || got != segment.Sealed {
			t.Errorf("the copy's segment row is %v (%v), want Sealed", got, err)
		}
	})
	t.Run("a second failing spare fails the emptying and keeps the source", func(t *testing.T) {
		r, st, h := plant(t)
		first := core.SegmentID(st.Attached.MaxID + 1)
		r.failSyncs(format.SegmentName(first), 0)
		r.failSyncs(format.SegmentName(first+1), 0)
		before, err := r.seam.Durable(format.SegmentName(2))
		if err != nil {
			t.Fatalf("read the source: %v", err)
		}
		err = Emptying(context.Background(), h.deps, st, journalID, core.Version{}, nil)
		if !errors.Is(err, syscall.EIO) {
			t.Fatalf("Emptying over two failing targets answered %v, want the sync's own EIO", err)
		}
		after, aerr := r.seam.Durable(format.SegmentName(2))
		if aerr != nil || !slices.Equal(before, after) {
			t.Errorf("the refused emptying changed the source: got %x (err %v), want the planted bytes", after, aerr)
		}
		if got, serr := st.Assembled.Set.State(2); serr != nil || got != segment.Ended {
			t.Errorf("the source's row is %v (%v), want Ended and standing", got, serr)
		}
	})
}

// TestAGoodEndedCopyOutlivesARottenStandingTwin holds the skip's payload rule:
// an ended segment holds a write whose duplicate, same numbers and same
// extent, sits in a sealed segment the open keeps, and the duplicate's body
// rots before the emptying reads it. The standing copy does not verify, so
// the ended segment's good copy is carried into a sealed spare rather than
// unlinked in its favour, and the extent serves a copy that verifies whole.
func TestAGoodEndedCopyOutlivesARottenStandingTwin(t *testing.T) {
	file := tailFile()
	const off, n = 0x4100, 16
	r := newRig(t)
	r.writeFormat()
	r.plantSeg(2, damageHeader(buildSeg(2, collectWrite(7, file, 5, 5, off, n))))
	twin, _ := buildSealed(3, collectWrite(7, file, 5, 5, off, n))
	r.plantSeg(3, twin)

	st, err := Run(context.Background(), r.runDeps(), core.Version{}, journalID, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got, err := st.Assembled.Set.State(2); err != nil || got != segment.Ended {
		t.Fatalf("segment 2's row got %v (%v), want Ended by its damaged header", got, err)
	}
	if got, err := st.Assembled.Set.State(3); err != nil || got != segment.Sealed {
		t.Fatalf("segment 3's row got %v (%v), want Sealed and standing", got, err)
	}
	// The twin's body rots between the open's walk and the emptying's read.
	rotByte(t, r, format.SegmentName(3), format.SegmentHeaderSize+format.RecordHeaderSize)

	h := newEmptyingHarness(t, r, &st, segment.Targets{Reserve: 1})
	if err := Emptying(context.Background(), h.deps, &st, journalID, core.Version{}, nil); err != nil {
		t.Fatalf("Emptying: %v", err)
	}
	placed, ok := st.Rebuilt.Index.Lookup(file, off)
	if !ok || placed.Seq != 5 {
		t.Fatalf("the extent at %#x serves %+v (%v), want the write of seq 5", off, placed, ok)
	}
	if placed.Record.Segment == 2 || placed.Record.Segment == 3 {
		t.Fatalf("the extent serves from segment %d, want the sealed copy of the ended segment's good record", placed.Record.Segment)
	}
	rd := newReader(context.Background(), r.seam.FS(), r.clk, nil)
	if _, _, rot, rerr := readRecordWhole(rd, journalID, placed.Record.Segment, int64(placed.Record.Offset)); rerr != nil || rot {
		t.Errorf("the served copy in segment %d does not verify whole (rot %v, err %v), want the good copy carried", placed.Record.Segment, rot, rerr)
	}
}

// TestUnheldEndedSegmentRunsBeforeAHeldOneAtALowerID holds the run's order
// against the ids: the held ended segment sits below the unheld predecessor
// whose durable unlink frees the only spare, so ascending ids alone would
// draw before the take is free and refuse the open.
func TestUnheldEndedSegmentRunsBeforeAHeldOneAtALowerID(t *testing.T) {
	r := newRig(t)
	r.writeFormat()
	r.plantSeg(2, damageHeader(buildSeg(2, collectWrite(7, tailFile(), 5, 5, 0, 16))))
	pred, _ := buildSealed(4)
	r.write(format.SegmentName(4), append(pred, make([]byte, testSegCap-int64(len(pred)))...))
	take := raw.SegmentHeader{Identity: journalID, ID: 5, Predecessor: 4}.Bytes()
	r.write(format.SegmentName(5), append(take, make([]byte, testSegCap-int64(len(take)))...))

	d := r.runDeps()
	d.SegmentCap = testSegCap
	mark := core.SyncMark{Segment: 4, Sequence: 9, Offset: format.SegmentHeaderSize, Through: format.SegmentHeaderSize}
	st, err := Run(context.Background(), d, core.Version{}, journalID, []core.SyncMark{mark})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !slices.Contains(st.Spares.Held, core.SegmentID(5)) {
		t.Fatalf("the spares phase did not withhold the take 5 naming the ended predecessor 4")
	}
	h := newEmptyingHarness(t, r, &st, segment.Targets{})
	if err := Emptying(context.Background(), h.deps, &st, journalID, core.Version{}, []core.SyncMark{mark}); err != nil {
		t.Fatalf("Emptying with the freed take as the only spare: %v", err)
	}
	placed, ok := st.Rebuilt.Index.Lookup(tailFile(), 0)
	if !ok || placed.Record.Segment != 5 {
		t.Errorf("the extent serves %+v (%v), want the copy in the freed take 5", placed, ok)
	}
}

// twinRig plants the shape the standing-copy tests share: one record of seq 5
// in the three segments of ids, the ended one first, each sealed one beside a
// record of its own so that it stays held, with the body of the sealed copy
// named by rotted rotted after the open's walk.
func twinRig(t *testing.T, ended core.SegmentID, sealed []core.SegmentID, rotted core.SegmentID) (*rig, State, core.FileID) {
	t.Helper()
	file := tailFile()
	r := newRig(t)
	r.writeFormat()
	r.plantSeg(ended, damageHeader(buildSeg(ended, collectWrite(7, file, 5, 5, 0x4100, 16))))
	for i, id := range sealed {
		off := int64(0x5100 + 0x1000*i)
		seg, _ := buildSealed(id, collectWrite(7, file, 5, 5, 0x4100, 16), collectWrite(7, file, uint64(6+i), uint64(6+i), off, 16))
		r.plantSeg(id, seg)
	}
	st, err := Run(context.Background(), r.runDeps(), core.Version{}, journalID, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	rotByte(t, r, format.SegmentName(rotted), format.SegmentHeaderSize+format.RecordHeaderSize)
	return r, st, file
}

// verifyingCopies counts the records of seq in the segments on disk whose
// whole body verifies.
func verifyingCopies(t *testing.T, r *rig, seq uint64) int {
	t.Helper()
	n := 0
	rd := newReader(context.Background(), r.seam.FS(), r.clk, nil)
	for _, name := range r.listing() {
		kind, id := format.ClassifyName(name)
		if kind != format.NameSegment {
			continue
		}
		bs, err := r.seam.Durable(name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		walk, werr := format.ScanSegmentHeaders(bytes.NewReader(bs), int64(len(bs)), journalID, id)
		if werr != nil {
			t.Fatalf("walking %s: %v", name, werr)
		}
		for _, rec := range walk.Records {
			if uint64(rec.Header.Seq) != seq {
				continue
			}
			if _, _, rot, rerr := readRecordWhole(rd, journalID, id, int64(rec.Header.Offset)); rerr == nil && !rot {
				n++
			}
		}
	}
	return n
}

// TestACopyBelowARottenServedTwinDoesNotOutliveTheNextOpen holds the rot
// branch across a restart: the ended segment 3, a sealed copy in 2 that
// verifies and a greater sealed twin in 4 the index serves whose body rots.
// A copy below the twin loses to it at the next open, whose walk verifies
// no payload, so the ended copy is carried above it. Then a sealed segment
// supersedes 2's other record, a second open retires 2 as unheld, and a
// copy of the record must still verify.
func TestACopyBelowARottenServedTwinDoesNotOutliveTheNextOpen(t *testing.T) {
	r, st, file := twinRig(t, 3, []core.SegmentID{2, 4}, 4)
	if placed, ok := st.Rebuilt.Index.Lookup(file, 0x4100); !ok || placed.Record.Segment != 4 {
		t.Fatalf("the index serves %+v (%v) at the open, want the greater twin in 4", placed, ok)
	}
	h := newEmptyingHarness(t, r, &st, segment.Targets{Reserve: 1})
	if err := Emptying(context.Background(), h.deps, &st, journalID, core.Version{}, nil); err != nil {
		t.Fatalf("Emptying: %v", err)
	}
	over, _ := buildSealed(20, collectWrite(7, file, 30, 30, 0x5100, 16))
	r.plantSeg(20, over)
	st2, err := Run(context.Background(), r.runDeps(), core.Version{}, journalID, nil)
	if err != nil {
		t.Fatalf("the second Run: %v", err)
	}
	h2 := newEmptyingHarness(t, r, &st2, segment.Targets{Reserve: 1})
	if err := Emptying(context.Background(), h2.deps, &st2, journalID, core.Version{}, nil); err != nil {
		t.Fatalf("the second Emptying: %v", err)
	}
	if n := verifyingCopies(t, r, 5); n < 1 {
		placed, ok := st2.Rebuilt.Index.Lookup(file, 0x4100)
		t.Errorf("seq 5 verifies in %d placements after the second open (serving %+v, %v), want at least 1", n, placed, ok)
	}
}

// TestTheGreatestMarkOfASegmentClassesTheRotAsSynced hands the emptying two
// marks for one segment, in both orders: the record's read-found rot is
// below the higher Through and above the lower, and its loss record carries
// the synced bit either way.
func TestTheGreatestMarkOfASegmentClassesTheRotAsSynced(t *testing.T) {
	for _, tc := range []struct {
		name  string
		first bool // the higher Through comes first
	}{{"higher first", true}, {"lower first", false}} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRig(t)
			r.writeFormat()
			seg := damageHeader(buildSeg(2, collectWrite(7, tailFile(), 41, 41, 0x4100, 16)))
			r.plantSeg(2, seg)
			hi := core.SyncMark{Stream: 1, Segment: 2, Sequence: 41, Offset: int64(len(seg)), Through: int64(len(seg))}
			lo := core.SyncMark{Stream: 2, Segment: 2, Sequence: 41, Offset: format.SegmentHeaderSize, Through: format.SegmentHeaderSize}
			marks := []core.SyncMark{lo, hi}
			if tc.first {
				marks = []core.SyncMark{hi, lo}
			}
			st, err := Run(context.Background(), r.runDeps(), core.Version{}, journalID, marks)
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			rotByte(t, r, format.SegmentName(2), int64(len(seg))-1)
			h := newEmptyingHarness(t, r, &st, segment.Targets{Reserve: 1})
			if err := Emptying(context.Background(), h.deps, &st, journalID, core.Version{}, marks); err != nil {
				t.Fatalf("Emptying: %v", err)
			}
			found := false
			for _, name := range r.listing() {
				kind, id := format.ClassifyName(name)
				if kind != format.NameSegment {
					continue
				}
				bs, _ := r.seam.Durable(name)
				walk, werr := format.ScanSegmentHeaders(bytes.NewReader(bs), int64(len(bs)), journalID, id)
				if werr != nil {
					t.Fatalf("walking %s: %v", name, werr)
				}
				for _, rec := range walk.Records {
					if rec.Header.Kind != format.KindLoss {
						continue
					}
					body, berr := format.VerifyRecordBody(bs[rec.Header.Offset+format.RecordHeaderSize:rec.Header.Offset+rec.Header.Size()], journalID, rec.Header)
					if berr == nil && body.Loss.Dropped == 41 {
						found = true
						if !body.Loss.Synced {
							t.Errorf("the loss record of seq 41 carries no synced bit, want it: the greatest mark's Through covers the record")
						}
					}
				}
			}
			if !found {
				t.Fatalf("no loss record names the rotted record of seq 41")
			}
		})
	}
}

// TestACarryOverARottenTwinSkipsAFreedTakeBelowIt holds the draw's floor: the
// unheld segment 2 is emptied first and its durable unlink frees the take 3,
// then the ended segment 4's record is carried in the place of its rotten
// twin in the sealed segment 7 — through the rot branch, or, when 4's own
// copy rots too, through the loss leg carrying a third copy from the sealed
// segment 5. A carry into the take below the twin would lose to the twin at
// the next open, whose walk verifies no payload, and the take would go as
// unheld; the carry draws above the twin, and a copy still verifies after
// the second open, segment 5's other record superseded before it.
func TestACarryOverARottenTwinSkipsAFreedTakeBelowIt(t *testing.T) {
	for _, tc := range []struct {
		name  string
		third bool
	}{
		{name: "rot_branch"},
		{name: "loss_leg", third: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			file := tailFile()
			r := newRig(t)
			r.writeFormat()
			r.plantSeg(4, damageHeader(buildSeg(4, collectWrite(7, file, 5, 5, 0, 16))))
			pred, _ := buildSealed(2)
			r.write(format.SegmentName(2), append(pred, make([]byte, testSegCap-int64(len(pred)))...))
			take := raw.SegmentHeader{Identity: journalID, ID: 3, Predecessor: 2}.Bytes()
			r.write(format.SegmentName(3), append(take, make([]byte, testSegCap-int64(len(take)))...))
			if tc.third {
				third, _ := buildSealed(5, collectWrite(7, file, 5, 5, 0, 16), collectWrite(7, file, 8, 8, 0x6100, 16))
				r.plantSeg(5, third)
			}
			twin, _ := buildSealed(7, collectWrite(7, file, 5, 5, 0, 16), collectWrite(7, file, 6, 6, 0x5100, 16))
			r.plantSeg(7, twin)

			d := r.runDeps()
			d.SegmentCap = testSegCap
			mark := core.SyncMark{Segment: 2, Sequence: 9, Offset: format.SegmentHeaderSize, Through: format.SegmentHeaderSize}
			st, err := Run(context.Background(), d, core.Version{}, journalID, []core.SyncMark{mark})
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if !slices.Contains(st.Spares.Held, core.SegmentID(3)) {
				t.Fatalf("the spares phase did not withhold the take 3 naming the ended predecessor 2")
			}
			if p, ok := st.Rebuilt.Index.Lookup(file, 0); !ok || p.Record.Segment != 7 {
				t.Fatalf("the open serves %+v (%v), want the twin in 7", p, ok)
			}
			rotByte(t, r, format.SegmentName(7), format.SegmentHeaderSize+format.RecordHeaderSize)
			if tc.third {
				rotByte(t, r, format.SegmentName(4), format.SegmentHeaderSize+format.RecordHeaderSize)
			}
			h := newEmptyingHarness(t, r, &st, segment.Targets{Reserve: 1})
			if err := Emptying(context.Background(), h.deps, &st, journalID, core.Version{}, []core.SyncMark{mark}); err != nil {
				t.Fatalf("Emptying: %v", err)
			}
			if tc.third {
				over, _ := buildSealed(20, collectWrite(7, file, 30, 30, 0x6100, 16))
				r.plantSeg(20, over)
			}
			d2 := r.runDeps()
			d2.SegmentCap = testSegCap
			st2, err := Run(context.Background(), d2, core.Version{}, journalID, nil)
			if err != nil {
				t.Fatalf("the second Run: %v", err)
			}
			h2 := newEmptyingHarness(t, r, &st2, segment.Targets{Reserve: 1})
			if err := Emptying(context.Background(), h2.deps, &st2, journalID, core.Version{}, nil); err != nil {
				t.Fatalf("the second Emptying: %v", err)
			}
			if n := verifyingCopies(t, r, 5); n < 1 {
				p, ok := st2.Rebuilt.Index.Lookup(file, 0)
				t.Errorf("seq 5 verifies in %d placements after the second open (serving %+v, %v; listing %v), want at least 1", n, p, ok, r.listing())
			}
		})
	}
}

// TestTheCopyOutranksARottenTwinAcrossTwoOpens permutes the ids of the
// emptied predecessor, the take its durable unlink frees, the ended segment
// and the sealed twin whose copy of the ended record rots before the
// emptying reads it. The rebuild serves the greatest placement and verifies
// no payload, so after a second open the record the index serves must still
// verify, wherever the ids fall. With every freed take below the twin and the
// reserve empty, the open fails on the pool's spare refusal rather than place
// the copy low, and the ended segment stands.
func TestTheCopyOutranksARottenTwinAcrossTwoOpens(t *testing.T) {
	for _, tc := range []struct {
		name                    string
		pred, take, ended, twin core.SegmentID
		reserve                 int
		refused                 bool
	}{
		{name: "take_below_twin", pred: 2, take: 3, ended: 4, twin: 7, reserve: 1},
		{name: "take_above_twin", pred: 2, take: 9, ended: 4, twin: 7, reserve: 1},
		{name: "take_above_twin_reserve_empty", pred: 2, take: 9, ended: 4, twin: 7},
		{name: "take_below_twin_reserve_empty", pred: 2, take: 3, ended: 4, twin: 7, refused: true},
		{name: "twin_below_ended_take_below_twin", pred: 1, take: 2, ended: 4, twin: 3, reserve: 1},
		{name: "twin_below_ended_take_above_all", pred: 1, take: 9, ended: 4, twin: 3, reserve: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			file := tailFile()
			r := newRig(t)
			r.writeFormat()
			r.plantSeg(tc.ended, damageHeader(buildSeg(tc.ended, collectWrite(7, file, 5, 5, 0, 16))))
			pred, _ := buildSealed(tc.pred)
			r.write(format.SegmentName(tc.pred), append(pred, make([]byte, testSegCap-int64(len(pred)))...))
			take := raw.SegmentHeader{Identity: journalID, ID: uint64(tc.take), Predecessor: uint64(tc.pred)}.Bytes()
			r.write(format.SegmentName(tc.take), append(take, make([]byte, testSegCap-int64(len(take)))...))
			twin, _ := buildSealed(tc.twin, collectWrite(7, file, 5, 5, 0, 16), collectWrite(7, file, 6, 6, 0x5100, 16))
			r.plantSeg(tc.twin, twin)

			d := r.runDeps()
			d.SegmentCap = testSegCap
			mark := core.SyncMark{Segment: uint64(tc.pred), Sequence: 9, Offset: format.SegmentHeaderSize, Through: format.SegmentHeaderSize}
			st, err := Run(context.Background(), d, core.Version{}, journalID, []core.SyncMark{mark})
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if !slices.Contains(st.Spares.Held, tc.take) {
				t.Fatalf("the spares phase did not withhold the take %d naming the predecessor %d", tc.take, tc.pred)
			}
			rotByte(t, r, format.SegmentName(tc.twin), format.SegmentHeaderSize+format.RecordHeaderSize)
			h := newEmptyingHarness(t, r, &st, segment.Targets{Reserve: tc.reserve})
			err = Emptying(context.Background(), h.deps, &st, journalID, core.Version{}, []core.SyncMark{mark})
			if tc.refused {
				if !errors.Is(err, core.ErrNoSpace) {
					t.Fatalf("Emptying with every freed take below the twin and no reserve answered %v, want the pool's spare refusal", err)
				}
				if !slices.Contains(r.listing(), format.SegmentName(tc.ended)) {
					t.Fatalf("the refused emptying unlinked the ended segment %d, want it standing for the next open", tc.ended)
				}
				return
			}
			if err != nil {
				t.Fatalf("Emptying: %v", err)
			}

			d2 := r.runDeps()
			d2.SegmentCap = testSegCap
			st2, err := Run(context.Background(), d2, core.Version{}, journalID, nil)
			if err != nil {
				t.Fatalf("the second Run: %v", err)
			}
			if n := verifyingCopies(t, r, 5); n < 1 {
				t.Fatalf("seq 5 verifies in %d placements after the second open (listing %v), want at least 1", n, r.listing())
			}
			p, ok := st2.Rebuilt.Index.Lookup(file, 0)
			if !ok {
				t.Fatalf("the second open serves nothing at seq 5's extent, want a verifying copy")
			}
			rd := newReader(context.Background(), r.seam.FS(), r.clk, nil)
			if _, _, rot, rerr := readRecordWhole(rd, journalID, p.Record.Segment, int64(p.Record.Offset)); rerr != nil || rot {
				t.Errorf("the second open serves seq %d from segment %d at %d, which does not verify (rot %v, err %v), want the copy above the rotten twin %d", p.Seq, p.Record.Segment, p.Record.Offset, rot, rerr, tc.twin)
			}
		})
	}
}

// TestACarryOverARottenTwinSkipsAnAdoptedReserveSpareBelowIt holds the draw's
// floor over the pool's own reserve: the spare 1 the spares phase re-adopts
// into the repack reserve keeps its old id, below the twin 7 whose copy of
// the ended segment 4's record rots before the emptying reads it. A carry
// into that spare would lose to the twin at the next open, whose walk
// verifies no payload; the carry lands in that spare renamed above the twin.
func TestACarryOverARottenTwinSkipsAnAdoptedReserveSpareBelowIt(t *testing.T) {
	file := tailFile()
	r := newRig(t)
	r.writeFormat()
	r.write(format.SegmentName(1), make([]byte, testSegCap))
	r.plantSeg(4, damageHeader(buildSeg(4, collectWrite(7, file, 5, 5, 0, 16))))
	twin, _ := buildSealed(7, collectWrite(7, file, 5, 5, 0, 16), collectWrite(7, file, 6, 6, 0x5100, 16))
	r.plantSeg(7, twin)

	d := r.runDeps()
	d.SpareTargets = segment.Targets{Reserve: 1}
	st, err := Run(context.Background(), d, core.Version{}, journalID, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got, want := st.Spares.Adopted, []AdoptedSpare{{ID: 1, Class: segment.Reserve}}; !slices.Equal(got, want) {
		t.Fatalf("the spares phase adopted %v, want %v", got, want)
	}
	rotByte(t, r, format.SegmentName(7), format.SegmentHeaderSize+format.RecordHeaderSize)
	h := newEmptyingHarness(t, r, &st, segment.Targets{Reserve: 1})
	if err := Emptying(context.Background(), h.deps, &st, journalID, core.Version{}, nil); err != nil {
		t.Fatalf("Emptying: %v", err)
	}
	if slices.Contains(r.listing(), format.SegmentName(1)) {
		t.Errorf("the adopted reserve spare 1 still stands under its old name, want it renamed above the twin for the carry")
	}

	st2, err := Run(context.Background(), r.runDeps(), core.Version{}, journalID, nil)
	if err != nil {
		t.Fatalf("the second Run: %v", err)
	}
	p, ok := st2.Rebuilt.Index.Lookup(file, 0)
	if !ok {
		t.Fatalf("the second open serves nothing at seq 5's extent, want a verifying copy")
	}
	rd := newReader(context.Background(), r.seam.FS(), r.clk, nil)
	if _, _, rot, rerr := readRecordWhole(rd, journalID, p.Record.Segment, int64(p.Record.Offset)); rerr != nil || rot {
		t.Errorf("the second open serves seq %d from segment %d at %d, which does not verify (rot %v, err %v), want the copy above the rotten twin 7", p.Seq, p.Record.Segment, p.Record.Offset, rot, rerr)
	}
}
