package recovery

import (
	"context"
	"math"
	"slices"
	"testing"

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
	h := newEmptyingHarness(t, r, &st)
	if err := Emptying(context.Background(), h.deps, &st, journalID, core.Version{}, nil); err != nil {
		t.Fatalf("Emptying: %v", err)
	}

	if slices.Contains(r.listing(), format.SegmentName(1)) {
		t.Errorf("the dead sealed segment's file is still on disk, want it retired at the emptying's first step")
	}
	if _, err := st.Assembled.Set.State(1); err == nil {
		t.Errorf("the assembled set still holds the dead segment's row, want it gone with the retirement")
	}
	if !slices.Contains(r.listing(), format.SegmentName(2)) {
		t.Errorf("the ended segment's file is gone from the directory, want it standing for the emptying that follows the retire step")
	}
	if got, err := st.Assembled.Set.State(2); err != nil || got != segment.Ended {
		t.Errorf("the ended segment's row got %v (%v), want Ended and standing", got, err)
	}
	if _, ok := st.Rebuilt.Index.Lookup(file, 0x4100); !ok {
		t.Errorf("the index answers no record at 0x4100, want the newer version the ended segment holds serving it")
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
	h := newEmptyingHarness(t, r, &st, segment.Targets{Reserve: 1})
	if err := Emptying(context.Background(), h.deps, &st, journalID, core.Version{}, nil); err != nil {
		t.Fatalf("Emptying: %v", err)
	}

	// The dead full seal retired by the retirement path's own rule for a
	// full word: a recycle, its fresh spare refilling the reserve the
	// emptying draws on — the old name gone, the fresh one on disk, the
	// pool's ready repack reserve at the cap it holds.
	if slices.Contains(r.listing(), format.SegmentName(1)) {
		t.Errorf("the dead full seal's old name is still on disk, want it recycled by the retire step")
	}
	fresh := format.SegmentName(st.Attached.MaxID + 1)
	if !slices.Contains(r.listing(), fresh) {
		t.Errorf("the recycle's fresh spare %s is not on disk, want it refilled into the reserve the emptying draws on", fresh)
	}
	if got, want := h.pool.Counters().RepackReserveBytes, d.SegmentCap; got != want {
		t.Errorf("the pool's ready repack reserve after the retire step got %d bytes, want the %d-byte cap: each retirement refills the reserve before the next emptying", got, want)
	}
	// The spare and the deferred loss's segment both stand.
	if !slices.Contains(r.listing(), format.SegmentName(3)) {
		t.Errorf("the re-adopted spare's file is gone from the directory, want a spare never a retirement's")
	}
	if got, err := st.Assembled.Set.State(3); err != nil || got != segment.Active {
		t.Errorf("the re-adopted spare's row got %v (%v), want Active and standing", got, err)
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
	return &emptyingHarness{deps: d, pool: pool}
}

// flatDomains is a file append domain table that never blocks: the deferred
// registry's plant takes no domain.
type flatDomains struct{}

func (flatDomains) Lock(core.FileID) func() { return func() {} }

// deferLossAgainst registers one deferred loss against seg in the
// per-directory registry of the rig's directory under the journal id, the
// way a running journal's own deferral does: a real Durability instance,
// started over throwaway streams that write nothing, whose deferred Lose
// takes the piece and the entry and appends no record — so the retire
// step's reader answers the entry the same registry's failed-list reader
// answers its own.
func (r *rig) deferLossAgainst(t *testing.T, id core.JournalID, seg core.SegmentID, l durability.Loss) {
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
		t.Fatalf("the registry plant's durability: %v", err)
	}
	// The cleanups run last in, first out: the layer stops, then its
	// throwaway streams close, then their cache shuts down.
	t.Cleanup(func() {
		if err := cache.Shutdown(); err != nil {
			t.Errorf("the registry plant's cache Shutdown: %v", err)
		}
	})
	streams := segment.NewStreams(1, id, set, pool, cache, d.Sink)
	t.Cleanup(func() {
		if err := streams.Close(); err != nil {
			t.Errorf("the registry plant's streams Close: %v", err)
		}
	})
	t.Cleanup(d.Stop)
	d.Start(streams)
	l.Deferred, l.Seg = true, seg
	if _, err := d.Lose(context.Background(), l); err != nil {
		t.Fatalf("the registry plant's deferred Lose: %v", err)
	}
}
