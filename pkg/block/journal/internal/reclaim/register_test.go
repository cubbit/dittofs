package reclaim

import (
	"fmt"
	"math/rand/v2"
	"slices"
	"sync"
	"testing"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/format"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/index"
)

// lossSlot is the extent offset every unsynced loss record covers, past
// every slot the planted records use: the drop test's outranking term for
// the loss then comes from what the plant happened to build elsewhere, and
// the reach term is the tracker's own registries alone, so the loss's
// answer rests on the copy registrations exactly.
const lossSlot = int64(1) << 30

// openCase is one rebuild's registration inputs beside the on-disk state
// they were rebuilt from: the planted index every tracker answers over,
// the segments the records sit in, and the three lists the open-time hook
// takes.
type openCase struct {
	f        *fixture
	segs     []core.SegmentID
	located  []Rec
	forgets  []ForgetSet
	unsynced []UnsyncedLoss
}

// namedCopy is one planted write an unsynced loss record can name: the
// number the loss will carry, and where the copy sits.
type namedCopy struct {
	seq  core.Seq
	file core.FileID
	seg  core.SegmentID
}

// drawCase plants one seeded scenario of a rebuild's records: planted
// writes give the index held bytes and drop ranges (content records are
// registered nowhere at open; they only shape the answers) and leave named
// copies behind, the header-only truncates and releases become the located
// records, one forget record comes with a snapshot of segments, one
// unsynced loss record names a planted copy, and one record of a kind no
// drop rule covers rides the located list, to check the open registers
// what it cannot answer about.
func drawCase(t *testing.T, r *rand.Rand, scale int) *openCase {
	t.Helper()
	c := &openCase{f: newFixture(t)}
	c.segs = []core.SegmentID{c.f.seg}
	for range 1 + r.IntN(3) {
		c.segs = append(c.segs, c.f.takeIn(t))
	}
	files := []core.FileID{fid(1), fid(2), fid(3)}
	p := newPlant(c.f)
	var at uint32
	var seq uint64
	var named []namedCopy

	register := func(rec Rec) {
		c.located = append(c.located, rec)
	}
	for i := range max(2, scale) {
		file := files[r.IntN(len(files))]
		seg := c.segs[r.IntN(len(c.segs))]
		seq++
		at += 4096
		switch r.IntN(3) {
		case 0: // a planted write: held bytes and a drop range, no registration
			p.write(t, file, seg, core.Extent{Off: int64(i%32) * 4096, Len: 4096}, at, 2+uint64(i%2), seq)
			if r.IntN(2) == 0 {
				named = append(named, namedCopy{seq: core.Seq(seq), file: file, seg: seg})
			}
		case 1: // a truncate the hook registers
			off := int64(i%4) << 20
			p.truncate(t, file, seg, off, at, 4, seq)
			register(Rec{
				Kind: format.KindTruncate, File: file, Seg: seg,
				Ext:  core.Extent{Off: off},
				Pair: index.Numbers{Version: ver(4), Seq: core.Seq(seq)}, Seq: core.Seq(seq),
			})
		default: // a release the hook registers
			e := core.Extent{Off: int64(16+i%16) * 4096, Len: 4096}
			p.release(t, file, seg, e, at, 3, seq)
			register(Rec{
				Kind: format.KindRelease, File: file, Seg: seg, Ext: e,
				Pair: index.Numbers{Version: ver(3), Seq: core.Seq(seq)}, Seq: core.Seq(seq),
			})
		}
	}

	// A stamp record, a kind no drop rule of the tracker covers: the open
	// registers what it cannot answer about, and the record counts its
	// segment held.
	stamp := c.f.takeIn(t)
	c.segs = append(c.segs, stamp)
	seq++
	register(Rec{
		Kind: format.KindStamp, File: fid(9), Seg: stamp,
		Ext:  core.Extent{Off: 0, Len: 4096},
		Pair: index.Numbers{Version: ver(8), Seq: core.Seq(seq)}, Seq: core.Seq(seq),
	})

	// One forget record of the tag, with a snapshot the rebuild collected:
	// never empty, so its droppable answer is the snapshot's, not a plain
	// register's.
	seq++
	c.forgets = append(c.forgets, ForgetSet{Rec: Rec{
		Kind: format.KindForget, Seg: c.segs[r.IntN(len(c.segs))],
		Pair: index.Numbers{Version: ver(9), Seq: core.Seq(seq)}, Seq: core.Seq(seq),
	}, Segs: sampleSegs(r, c.segs)})

	// One unsynced loss record over the far slot, naming a planted copy,
	// with the copy's segment as the one holding the record it names.
	if len(named) > 0 {
		n := named[r.IntN(len(named))]
		seq++
		var sit core.SegmentID
		for {
			sit = c.segs[r.IntN(len(c.segs))]
			if sit != n.seg {
				break
			}
		}
		c.unsynced = append(c.unsynced, UnsyncedLoss{Rec: Rec{
			Kind: format.KindLoss, File: n.file, Seg: sit,
			Ext:  core.Extent{Off: lossSlot, Len: 4096},
			Pair: index.Numbers{Version: ver(9), Seq: core.Seq(seq)},
			Seq:  core.Seq(seq), Named: n.seq,
		}, HeldBy: []core.SegmentID{n.seg}})
	}
	p.finish(t)
	return c
}

// sampleSegs returns every segment of ss the draw keeps, always at least
// one: a forget record's snapshot of the segments holding records of its
// tag.
func sampleSegs(r *rand.Rand, ss []core.SegmentID) []core.SegmentID {
	out := make([]core.SegmentID, 0, len(ss))
	for _, s := range ss {
		if r.IntN(2) == 0 {
			out = append(out, s)
		}
	}
	if len(out) == 0 {
		out = append(out, ss[r.IntN(len(ss))])
	}
	return out
}

// The three live-updates a record's own append path makes, the oracle the
// open-time hook must agree with: a plain record through Register, a
// forget record through RegisterForget, an unsynced loss through Register
// of its record beside a presence mark in each segment holding the record
// it names — the mark makes the loss live without holding its holder,
// exactly the open registration's own rule.
func liveLocated(tr *Tracker, r Rec)      { tr.Register(r) }
func liveForget(tr *Tracker, f ForgetSet) { tr.RegisterForget(f.Rec, f.Segs) }
func liveUnsynced(tr *Tracker, u UnsyncedLoss) {
	tr.Register(u.Rec)
	for _, seg := range u.HeldBy {
		tr.registerMark(u.Rec, seg)
	}
}

// live returns a tracker fed the case's records the way their own append
// paths would have fed it as they ran.
func (c *openCase) live() *Tracker {
	tr := New(c.f.ix, nil)
	for _, l := range c.located {
		liveLocated(tr, l)
	}
	for _, f := range c.forgets {
		liveForget(tr, f)
	}
	for _, u := range c.unsynced {
		liveUnsynced(tr, u)
	}
	return tr
}

// checkAgainstLive asserts that tr — opened through RegisterAtOpen — holds
// and answers droppable exactly as want, the tracker fed the same records
// live, over every record and every segment of the case:
func (c *openCase) checkAgainstLive(t *testing.T, tr, want *Tracker) {
	t.Helper()
	for _, l := range c.located {
		if got, oracle := tr.Droppable(l), want.Droppable(l); got != oracle {
			t.Fatalf("Droppable of a located %s record = %v, want %v: the live feed of the same record's answer", l.Kind, got, oracle)
		}
		if got, oracle := tr.Held(l.Seg), want.Held(l.Seg); got != oracle {
			t.Fatalf("Held of the segment of a located %s record = %v, want %v: the live feed of the same record's answer", l.Kind, got, oracle)
		}
	}
	for _, f := range c.forgets {
		if got, oracle := tr.Droppable(f.Rec), want.Droppable(f.Rec); got != oracle {
			t.Fatalf("Droppable of a forget record = %v, want %v: the live feed of the same record's answer", got, oracle)
		}
	}
	for _, u := range c.unsynced {
		if got, oracle := tr.Droppable(u.Rec), want.Droppable(u.Rec); got != oracle {
			t.Fatalf("Droppable of an unsynced loss record = %v, want %v: the live feed of the same record's answer", got, oracle)
		}
		for _, seg := range u.HeldBy {
			if got, oracle := tr.Held(seg), want.Held(seg); got != oracle {
				t.Fatalf("Held of %d, a segment holding the named record = %v, want %v: the live feed of the same copy's answer", seg, got, oracle)
			}
		}
	}
	for _, s := range c.segs {
		if got, oracle := tr.Held(s), want.Held(s); got != oracle {
			t.Fatalf("Held of segment %d = %v, want %v: the live feed's answer", s, got, oracle)
		}
	}
}

// TestRegisterAtOpen_AnyOrder: over 200 seeded draws of a rebuild's
// records, RegisterAtOpen builds the same liveness the append paths' own
// registrations would have — in any order of the three lists and of each
// list's records, which a shuffle draws per case — and holds by the same
// answers the semantic rules fix: an unsynced loss is droppable never while
// a segment holding its named record is attached, and a record of a kind
// no drop rule covers is never droppable and counts its segment held. The
// open's close completes the picture: one OpenDone after the registration
// equals the live feed followed by a re-test of every file, because both
// re-tests walk the same whole registry.
func TestRegisterAtOpen_AnyOrder(t *testing.T) {
	for seed := range 200 {
		t.Run(fmt.Sprintf("seed=%d", seed), func(t *testing.T) {
			r := rand.New(rand.NewPCG(uint64(seed), 3375))
			c := drawCase(t, r, 2+r.IntN(9))
			want := c.live()
			want.retestAll()

			sh := rand.New(rand.NewPCG(uint64(seed), 11))
			open := New(c.f.ix, nil)
			open.RegisterAtOpen(shuffled(sh, c.located), shuffled(sh, c.forgets), shuffled(sh, c.unsynced))
			open.OpenDone()
			c.checkAgainstLive(t, open, want)

			// The rules the open registration itself must satisfy,
			// whatever the shuffles drew: the unsynced loss is kept by
			// every copy registration it was given, and the record no
			// drop rule covers counts live.
			for _, u := range c.unsynced {
				if got := open.Droppable(u.Rec); got {
					t.Fatalf("Droppable of an unsynced loss record, its named record held by %d attached segment(s) = %v, want false", len(u.HeldBy), got)
				}
			}
			for _, l := range c.located {
				if l.Kind != format.KindStamp {
					continue
				}
				if got := open.Droppable(l); got {
					t.Fatalf("Droppable of a stamp record, a kind no drop rule covers = %v, want false: the open fails closed", got)
				}
				if got := open.Held(l.Seg); !got {
					t.Fatalf("Held of the stamp record's segment = %v, want true: the open registers what it cannot answer about", got)
				}
			}
		})
	}
}

// TestRegisterAtOpen_ConcurrentFeeders: the same records registered by
// several feeders at once, each holding a shard of the three lists — an
// open whose segment scans run concurrently — give the same liveness the
// append paths' own registrations would have, whatever interleaving ran,
// with the open's close after the join: one OpenDone once every feeder is
// done equals the live feed followed by a re-test of every file.
func TestRegisterAtOpen_ConcurrentFeeders(t *testing.T) {
	for seed := range 20 {
		t.Run(fmt.Sprintf("seed=%d", seed), func(t *testing.T) {
			r := rand.New(rand.NewPCG(uint64(seed), 3375))
			c := drawCase(t, r, 2+r.IntN(9))
			want := c.live()
			want.retestAll()

			conc := New(c.f.ix, nil)
			registerConcurrent(conc, shardAll(r, c.located, c.forgets, c.unsynced))
			conc.OpenDone()
			c.checkAgainstLive(t, conc, want)
		})
	}
}

// shuffled returns a shuffled copy of ss, leaving the caller's slice
// alone.
func shuffled[T any](r *rand.Rand, ss []T) []T {
	out := slices.Clone(ss)
	r.Shuffle(len(out), func(i, j int) { out[i], out[j] = out[j], out[i] })
	return out
}

// feederShards is one feeder's shard of a concurrent registration: its
// slices of the three lists.
type feederShards struct {
	located  []Rec
	forgets  []ForgetSet
	unsynced []UnsyncedLoss
}

// shardAll splits the three lists between 2-4 feeders, every record to
// exactly one.
func shardAll(r *rand.Rand, located []Rec, forgets []ForgetSet, unsynced []UnsyncedLoss) []feederShards {
	n := 2 + r.IntN(3)
	shards := make([]feederShards, n)
	for _, l := range located {
		k := r.IntN(n)
		shards[k].located = append(shards[k].located, l)
	}
	for _, f := range forgets {
		k := r.IntN(n)
		shards[k].forgets = append(shards[k].forgets, f)
	}
	for _, u := range unsynced {
		k := r.IntN(n)
		shards[k].unsynced = append(shards[k].unsynced, u)
	}
	return shards
}

// registerConcurrent registers every shard from its own goroutine, all
// released at once: the feeders of a concurrent rebuild have no order
// between them, so the result must not depend on one.
func registerConcurrent(tr *Tracker, shards []feederShards) {
	start := make(chan struct{})
	var wg sync.WaitGroup
	for _, s := range shards {
		wg.Add(1)
		go func(s feederShards) {
			defer wg.Done()
			<-start
			tr.RegisterAtOpen(s.located, s.forgets, s.unsynced)
		}(s)
	}
	close(start)
	wg.Wait()
}

// TestRegisterAtOpen_PresenceMarksDoNotPin: an unsynced loss's copy
// registrations are presence marks in the registry, not live records: a
// holder segment holding nothing else answers not held the moment the open
// leaves it — the mark pins nothing — while the loss itself stays
// undroppable until the last holder retires, and the loss's file's re-test
// the last retirement carries turns it droppable at once.
func TestRegisterAtOpen_PresenceMarksDoNotPin(t *testing.T) {
	f := newFixture(t)
	id := fid(60)
	p := newPlant(f)
	// Two holder segments beside the one the loss record sits in, each
	// holding nothing but the copy it holds for the loss.
	a := f.takeIn(t)
	b := f.takeIn(t)
	sit := f.takeIn(t)
	p.finish(t)
	tr := New(f.ix, nil)
	loss := Rec{
		Kind: format.KindLoss, File: id, Seg: sit,
		Ext:  core.Extent{Off: lossSlot, Len: 4096},
		Pair: index.Numbers{Version: ver(9), Seq: 9}, Seq: 9, Named: 3,
	}
	un := UnsyncedLoss{Rec: loss, HeldBy: []core.SegmentID{a, b}}
	tr.RegisterAtOpen(nil, nil, []UnsyncedLoss{un})

	// The marks pin nothing: both holders answer not held before any
	// retirement, exactly as segments holding nothing of their own.
	if got := tr.Held(a); got {
		t.Fatalf("Held of a holder segment with nothing but the loss's presence mark = %v, want false: the mark is not a live record", got)
	}
	if got := tr.Held(b); got {
		t.Fatalf("Held of a second holder segment with nothing but the loss's presence mark = %v, want false: the mark is not a live record", got)
	}
	// The loss is kept by both marks.
	if got := tr.Droppable(loss); got {
		t.Fatalf("Droppable of the unsynced loss with two attached holders = %v, want false: the named record is still held", got)
	}

	// One holder's retirement releases its mark only, registry-side: the
	// loss finds the other holder's mark still.
	tr.SegmentRetired(a, []core.FileID{id})
	if got := tr.Droppable(loss); got {
		t.Fatalf("Droppable after the first holder retired = %v, want false: a holder of the named record remains", got)
	}

	// The last holder's retirement runs its re-test of the loss's file,
	// and the loss crosses to droppable at once, with no count to lower —
	// the marks raised none.
	tr.SegmentRetired(b, []core.FileID{id})
	if got := tr.Droppable(loss); !got {
		t.Fatalf("Droppable after the last holder retired = %v, want true: nothing holds the named record, and the retirement's re-test re-asks the loss", got)
	}
	// The loss record itself left with its marks: the retirement takes it
	// from the registry exactly as it takes a forget record whose snapshot
	// empties there, releasing its own registration — the count of its
	// own segment — once, through the registry's take gate.
	if _, ok := tr.recs[id][loss]; ok {
		t.Fatalf("the unsynced loss record is still registered, want the last retirement to have taken its now-droppable registration")
	}
}

// TestRegisterAtOpen_MarkBesideHoldersOwnReason: a holder held by its own
// bytes — a planted write of its own — stays held over the presence mark
// exactly as without it, and the loss still waits for that holder's
// retirement: the mark is invisible to both answers, and only the
// registry's take-gated rule sees it.
func TestRegisterAtOpen_MarkBesideHoldersOwnReason(t *testing.T) {
	f := newFixture(t)
	id := fid(61)
	p := newPlant(f)
	// The holder carries its own content, so it is held by its bytes
	// alone.
	holder := f.takeIn(t)
	sit := f.takeIn(t)
	p.write(t, id, holder, core.Extent{Off: 0, Len: 4096}, 0, 3, 3)
	p.finish(t)
	tr := New(f.ix, nil)
	loss := Rec{
		Kind: format.KindLoss, File: id, Seg: sit,
		Ext:  core.Extent{Off: lossSlot, Len: 4096},
		Pair: index.Numbers{Version: ver(9), Seq: 9}, Seq: 9, Named: 3,
	}
	tr.RegisterAtOpen(nil, nil, []UnsyncedLoss{{Rec: loss, HeldBy: []core.SegmentID{holder}}})

	if got := tr.Held(holder); !got {
		t.Fatalf("Held of the holder over its own bytes = %v, want true: its own reason holds it, not the mark", got)
	}
	if got := tr.Droppable(loss); got {
		t.Fatalf("Droppable of the unsynced loss while its holder is attached = %v, want false", got)
	}
	// The retirement is the crossing for the loss, not for the holder's
	// answer — nothing was lowered below zero — and the re-test it carries
	// drops the loss's registration.
	tr.SegmentRetired(holder, []core.FileID{id})
	if got := tr.Droppable(loss); !got {
		t.Fatalf("Droppable after the holder's retirement = %v, want true: nothing holds the named record any more", got)
	}
}

// TestRetireHolder_LossKeptByNamedRecordElsewhere: the last holder's
// retirement does not by itself end an unsynced loss — the loss leaves
// through the same drop test every release reaches, so a live registration
// of the same file still carrying the named sequence number keeps it: the
// mark-registration and the real record's can both name one number, and
// the registry walk, not the marks alone, decides.
func TestRetireHolder_LossKeptByNamedRecordElsewhere(t *testing.T) {
	f := newFixture(t)
	id := fid(70)
	p := newPlant(f)
	// The named record's copy sits in a segment of its own, one the
	// holder's retirement does not touch, still registered; the holder
	// segment carries nothing but the loss's last mark.
	holder := f.takeIn(t)
	home := f.takeIn(t)
	sit := f.takeIn(t)
	p.finish(t)
	tr := New(f.ix, nil)
	loss := Rec{
		Kind: format.KindLoss, File: id, Seg: sit,
		Ext:  core.Extent{Off: lossSlot, Len: 4096},
		Pair: index.Numbers{Version: ver(9), Seq: 9}, Seq: 9, Named: 3,
	}
	// The named record's own registration, live in the tracker as its
	// own path feeds it, over a disjoint extent so the outranking guard
	// alone would not keep the loss: the registry walk is the only term
	// that can.
	named := Rec{
		Kind: format.KindWrite, File: id, Seg: home,
		Ext:  core.Extent{Off: 1 << 24, Len: 4096},
		Pair: index.Numbers{Version: ver(3), Seq: 3}, Seq: 3,
	}
	tr.RegisterAtOpen(nil, nil, []UnsyncedLoss{{Rec: loss, HeldBy: []core.SegmentID{holder}}})
	tr.Register(named)

	tr.SegmentRetired(holder, []core.FileID{id})
	if got := tr.Droppable(loss); got {
		t.Fatalf("Droppable after the last mark holder retired, with the named record registered elsewhere = %v, want false: the drop test's registry walk still finds the named number", got)
	}
	if _, ok := tr.recs[id][loss]; !ok {
		t.Fatalf("the unsynced loss record left the registry at the holder's retirement, want it kept by the named record's live registration")
	}
	if got := tr.Held(sit); !got {
		t.Fatalf("Held of the loss record's own segment after the retirement = %v, want true: the loss is still registered live", got)
	}

	// The named record's release leaves the registry walk clear, and the
	// re-test it would carry drops the loss — the caller's own pairing,
	// not the retirement, is what ends it here.
	tr.Unregister(named)
	tr.Retest(id)
	if got := tr.Droppable(loss); !got {
		t.Fatalf("Droppable after the named record's release and a re-test = %v, want true: no marks, no live word of the named number, nothing to outrank", got)
	}
}

// TestRetireHolder_LossKeptByOutrankingGuard: the loss's own extent must
// also decide through the drop test at the last holder's retirement: a
// lower-pair record of the same file overlapping what the loss covers,
// planted into another segment through the rebuild, keeps the loss live
// whatever the marks are.
func TestRetireHolder_LossKeptByOutrankingGuard(t *testing.T) {
	f := newFixture(t)
	id := fid(71)
	p := newPlant(f)
	holder := f.takeIn(t)
	sitA := f.takeIn(t)
	// A lower-pair record of the loss's file over the loss's own extent,
	// in a segment no mark names, so only the guard can keep the loss.
	p.write(t, id, sitA, core.Extent{Off: lossSlot, Len: 4096}, 0, 2, 1)
	p.finish(t)
	tr := New(f.ix, nil)
	loss := Rec{
		Kind: format.KindLoss, File: id, Seg: f.seg,
		Ext:  core.Extent{Off: lossSlot, Len: 4096},
		Pair: index.Numbers{Version: ver(9), Seq: 9}, Seq: 9, Named: 3,
	}
	tr.RegisterAtOpen(nil, nil, []UnsyncedLoss{{Rec: loss, HeldBy: []core.SegmentID{holder}}})

	// The last holder's retirement empties the marks; the drop test the
	// appended file entry reaches must answer the loss kept: the planted
	// record overlaps what it covers at a lower pair.
	tr.SegmentRetired(holder, []core.FileID{id})
	if got := tr.Droppable(loss); got {
		t.Fatalf("Droppable after the marks emptied, beside a lower-pair record overlapping what the loss covers = %v, want false: the outranking guard holds it", got)
	}
	if _, ok := tr.recs[id][loss]; !ok {
		t.Fatalf("the unsynced loss record left the registry at the holder's retirement, want the outranking guard to keep it")
	}

	// The guard's end: retire the overlapping record's segment too, and
	// the same appended re-test drops the loss at last.
	tr.SegmentRetired(sitA, []core.FileID{id})
	if got := tr.Droppable(loss); !got {
		t.Fatalf("Droppable after the overlapping record's segment retired too = %v, want true: nothing keeps the loss any more", got)
	}
	if _, ok := tr.recs[id][loss]; ok {
		t.Fatalf("the unsynced loss record is still registered after nothing keeps it, want the re-test to have taken it")
	}
}

// TestRetireHolder_NilFilesListStillDropsLoss: a retirement whose caller
// carries no file list still ends an unsynced loss whose marks the retired
// segment emptied: the loss's own file, appended to the re-test list from
// its registry entry, is re-tested, and with nothing else keeping the loss
// it drops there.
func TestRetireHolder_NilFilesListStillDropsLoss(t *testing.T) {
	f := newFixture(t)
	id := fid(72)
	p := newPlant(f)
	holder := f.takeIn(t)
	p.finish(t)
	tr := New(f.ix, nil)
	loss := Rec{
		Kind: format.KindLoss, File: id, Seg: f.seg,
		Ext:  core.Extent{Off: lossSlot, Len: 4096},
		Pair: index.Numbers{Version: ver(9), Seq: 9}, Seq: 9, Named: 3,
	}
	tr.RegisterAtOpen(nil, nil, []UnsyncedLoss{{Rec: loss, HeldBy: []core.SegmentID{holder}}})

	// nil files: nothing but the loss's own registry entry names its file.
	tr.SegmentRetired(holder, nil)
	if got := tr.Droppable(loss); !got {
		t.Fatalf("Droppable after the last holder's retirement under a nil files list = %v, want true: the loss's file was appended to the re-test and nothing keeps it", got)
	}
	if _, ok := tr.recs[id][loss]; ok {
		t.Fatalf("the unsynced loss record is still registered, want the nil-files retirement's re-test to have taken it")
	}
	// The loss record's own segment: its registration left with the
	// re-test's release, so it answers held only by what the index
	// counts, which is nothing here.
	if got := tr.Held(f.seg); got {
		t.Fatalf("Held of the loss record's own segment after its registration dropped = %v, want false", got)
	}
}

// TestOpenDone_DropsAlreadyDroppableRecords: a record the drop test answers
// droppable the moment the open registers it — its marker settled before the
// open read it, nothing below its pair anywhere — counts live until the
// open's own close, then drops: Held answers true before OpenDone and false
// after, with one candidacy for the crossing, and the record answers
// Droppable throughout, because Register never re-tests.
func TestOpenDone_DropsAlreadyDroppableRecords(t *testing.T) {
	f := newFixture(t)
	var fired []core.SegmentID
	tr := New(f.ix, func(seg core.SegmentID) { fired = append(fired, seg) })
	id := fid(80)
	seg := f.takeIn(t)
	// A settled truncate nowhere outranked: the planted write over [0,1MiB)
	// at (3,2) sits below its size.
	p := newPlant(f)
	p.write(t, id, f.seg, core.Extent{Off: 0, Len: 1 << 20}, 0, 3, 2)
	p.truncate(t, id, f.seg, 1<<20, 4096, 5, 6)
	p.finish(t)
	f.ix.Settle(id, ver(9))
	trunc := Rec{
		Kind: format.KindTruncate, File: id, Seg: seg,
		Ext:  core.Extent{Off: 1 << 20},
		Pair: index.Numbers{Version: ver(5), Seq: 6}, Seq: 6,
	}
	tr.RegisterAtOpen([]Rec{trunc}, nil, nil)
	if got := tr.Droppable(trunc); !got {
		t.Fatalf("Droppable of the settled truncate before OpenDone = %v, want true: nothing below its pair overlaps what it covers", got)
	}
	if got := tr.Held(seg); !got {
		t.Fatalf("Held before OpenDone = %v, want true: Register counts the already-droppable record live", got)
	}
	if got := len(fired); got != 0 {
		t.Fatalf("the candidate fired %d times before OpenDone, want 0", got)
	}
	tr.OpenDone()
	if got := tr.Held(seg); got {
		t.Fatalf("Held after OpenDone = %v, want false: the open's own re-test drops the already-droppable record", got)
	}
	if len(fired) != 1 || fired[0] != seg {
		t.Fatalf("the candidate fired %v, want [%d]: OpenDone is the one crossing", fired, seg)
	}
	if _, ok := tr.recs[id][trunc]; ok {
		t.Fatalf("the settled truncate is still registered after OpenDone, want the re-test to have taken it")
	}
}

// TestOpenDone_KeepsLiveRecordsOpenBytes: OpenDone re-tests, it does not
// sweep: a record the drop test keeps — its marker unsettled, a lower-pair
// record of its file overlapping what it covers — survives OpenDone exactly
// as it survives every other re-test.
func TestOpenDone_KeepsLiveRecordsOpenBytes(t *testing.T) {
	f := newFixture(t)
	tr := New(f.ix, nil)
	id := fid(81)
	seg := f.takeIn(t)
	// An unsettled truncate beside a lower-pair write of its file it
	// covers, in another segment.
	p := newPlant(f)
	p.write(t, id, f.seg, core.Extent{Off: 1 << 20, Len: 4096}, 0, 3, 2)
	p.finish(t)
	trunc := Rec{
		Kind: format.KindTruncate, File: id, Seg: seg,
		Ext:  core.Extent{Off: 1 << 20},
		Pair: index.Numbers{Version: ver(5), Seq: 6}, Seq: 6,
	}
	tr.RegisterAtOpen([]Rec{trunc}, nil, nil)
	tr.OpenDone()
	if got := tr.Droppable(trunc); got {
		t.Fatalf("Droppable of the unsettled truncate after OpenDone = %v, want false: the marker term keeps it", got)
	}
	if got := tr.Held(seg); !got {
		t.Fatalf("Held after OpenDone = %v, want true: the re-test keeps the live record", got)
	}
}

// retestAll is the oracle's OpenDone: a live-fed tracker followed by one
// Retest of every file that ended with registered records answers exactly
// as an open-registered tracker followed by one OpenDone, because both
// re-tests walk the same whole registry.
func (tr *Tracker) retestAll() {
	tr.rm.Lock()
	files := make([]core.FileID, 0, len(tr.recs))
	for f := range tr.recs {
		files = append(files, f)
	}
	tr.rm.Unlock()
	if len(files) == 0 {
		return
	}
	tr.retest(files...)
}
