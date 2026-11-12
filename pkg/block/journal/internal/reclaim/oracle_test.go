package reclaim

import (
	"fmt"
	"math"
	"math/rand/v2"
	"slices"
	"testing"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/format"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/index"
)

// The oracle property test: over random writes, overwrites,
// removals, settles and retirements, Held of every segment is compared
// after every step with an exact oracle that keeps every record and
// answers the removal-drop rule pair by pair, per extent, per file —
// never by sequence alone, never against every record of the file,
// never journal-wide. Two assertions run:
//
//   - SAFETY, always: a segment the oracle holds is held. The tracker
//     answering not held while the exact rule holds would be data
//     resurrection.
//   - EQUALITY, which this workload earns rather than assumes: every
//     record is drawn over a four-bucket disjoint extent grid per
//     file, and only write records and the rebuild's notes feed the
//     index's drop ranges, so a (file, segment) pair holds at most
//     four disjoint ranges — the ninth-range merge and the coarser
//     span it can leave never run — and a segment the tracker holds
//     that the exact rule does not is a missing re-test event.
//
// The oracle shares the real index with the tracker — bytes are the
// one authority both must read the same — but keeps its own registry
// of records and re-implements the drop query, the settle rule and the
// pair comparison, never asking the index for them.

// oracleFiles are the two files a scenario draws over.
var oracleFiles = [2]core.FileID{fid(11), fid(12)}

// oracleSegs is the number of segments a scenario draws over: the
// fixture's own plus four spares it takes in.
const oracleSegs = 5

// oracleBuckets are the disjoint extents a scenario draws records
// over: bucket 0 over [0, 1 MiB), buckets 1-3 at 1 GiB and 4 MiB
// strides past it. Nothing of one file ever overlaps another file's
// records and no two buckets touch, so nothing the index notes merges
// across buckets and every range it keeps covers exactly the bytes of
// real records.
func oracleBuckets(b byte) core.Extent {
	if b == 0 {
		return core.Extent{Len: 1 << 20}
	}
	return core.Extent{Off: 1<<30 + int64(b-1)<<22, Len: 1 << 20}
}

// orec is one record the oracle keeps: the segment slot it sits in,
// the extent its rule covers, its (version, sequence) pair, and, for
// a removal record, whether the marker term applies to its kind.
type orec struct {
	seg        byte
	e          core.Extent
	v          uint64
	seq        core.Seq
	needMarker bool
}

// omarker is one removal marker the index carries, as the oracle
// keeps it: the version it was left at, and whether a settle covered
// it.
type omarker struct {
	v    uint64
	gone bool
}

// oracle is the exact model of a scenario: per file, the records a
// path noted in the index's drop ranges (each a candidate the drop
// rule compares pairs against), the removal markers, the removal
// records registered live with the tracker (with the segment of
// their registration), and which (file, segment) pairs and which
// segments a retirement has cleared.
type oracle struct {
	noted   [2][]orec
	markers [2][]omarker
	live    [2][]orec
	gone    [2][oracleSegs]bool
	retired [oracleSegs]bool
}

// outranked answers the index's drop query exactly, reimplemented:
// whether some record of file sits in another segment overlapping e
// with a pair strictly below (v, seq). The pair comparison is spelled
// here — version first, then sequence, strictly lower only, an equal
// pair being the record itself or a copy of it — rather than asked of
// the index.
func (o *oracle) outranked(file byte, r orec) bool {
	if r.e.Len == 0 {
		return false
	}
	lo, hi := r.e.Off, r.e.Off+r.e.Len
	for _, x := range o.noted[file] {
		if x.seg == r.seg || o.gone[file][x.seg] || o.retired[x.seg] {
			continue
		}
		if x.v != r.v {
			if x.v > r.v {
				continue
			}
		} else if x.seq >= r.seq {
			continue
		}
		if xlo, xhi := x.e.Off, x.e.Off+x.e.Len; xlo < hi && lo < xhi {
			return true
		}
	}
	return false
}

// settled answers MarkerSettled exactly, reimplemented: whether every
// marker the file still holds sits above v. Settles are kept as a
// flag per marker, so the answer reads no order the markers arrived
// in.
func (o *oracle) settled(file byte, v uint64) bool {
	for _, m := range o.markers[file] {
		if !m.gone && m.v <= v {
			return false
		}
	}
	return true
}

// droppable answers the drop rule for one registered removal record:
// the marker term its kind's rule reads — a release record leaves the
// question to outranking — then the outranking term, compared pair by
// pair over every record a path noted.
func (o *oracle) droppable(file byte, r orec) bool {
	return (!r.needMarker || o.settled(file, r.v)) && !o.outranked(file, r)
}

// ret re-tests the removal records registered live of file, releasing
// each one the drop rule now answers droppable — the event a settle, a
// copies-done or a retirement carries.
func (o *oracle) ret(file byte) {
	o.live[file] = slices.DeleteFunc(o.live[file], o.droppableOf(file))
}

func (o *oracle) droppableOf(file byte) func(orec) bool {
	return func(r orec) bool { return o.droppable(file, r) }
}

// settle covers every marker of file at or below v, then re-tests the
// file's registered records, exactly as the index's Settle and the
// tracker's MarkerSettled run together.
func (o *oracle) settle(file byte, v uint64) {
	for i := range o.markers[file] {
		if o.markers[file][i].v <= v {
			o.markers[file][i].gone = true
		}
	}
	o.ret(file)
}

// retire ends a segment's liveness, exactly as SegmentRetired does:
// every registration of the retired segment is taken whatever the drop
// rule says — the segment is gone — the drop ranges of each file the
// caller's list names leave the index, and each named file is
// re-tested, so a record elsewhere the retirement freed may now drop.
func (o *oracle) retire(seg byte, files []byte) {
	if o.retired[seg] {
		return
	}
	o.retired[seg] = true
	for file := range o.live {
		o.live[file] = slices.DeleteFunc(o.live[file], func(r orec) bool { return r.seg == seg })
	}
	for _, file := range files {
		o.gone[file][seg] = true
		o.ret(file)
	}
}

// held answers the whole question for one segment: what the index's
// bytes say, then any removal record still registered live in it.
func (o *oracle) held(ix *index.Index, segs [oracleSegs]core.SegmentID, i int) bool {
	seg := segs[i]
	if b := ix.SegmentBytes(seg); b.Held > 0 || b.Retained > 0 {
		return true
	}
	for file := range o.live {
		for _, r := range o.live[file] {
			if r.seg == byte(i) {
				return true
			}
		}
	}
	return false
}

// scenario is one run: the fixture with its planted index and real
// segment set, the tracker beside it, the oracle, the segments' ids
// and which still live.
type scenario struct {
	f     *fixture
	tr    *Tracker
	o     *oracle
	segs  [oracleSegs]core.SegmentID
	lives [oracleSegs]bool
}

// drawScenario makes the planted state every run starts from: the
// fixture, five real segments, and per (file, bucket) one content
// record planted through the rebuild — held bytes a later write
// overwrites and a noted record a lower-pair removal judges against —
// with no markers and nothing registered yet.
func drawScenario(t *testing.T) *scenario {
	t.Helper()
	f := newFixture(t)
	p := newPlant(f)
	s := &scenario{f: f, tr: New(f.ix, nil), o: &oracle{}}
	s.segs[0] = f.seg
	s.lives[0] = true
	for i := 1; i < oracleSegs; i++ {
		s.segs[i] = f.takeIn(t)
		s.lives[i] = true
	}
	for file := range 2 {
		for b := range 4 {
			seg := byte((2*file + b) % oracleSegs)
			e := oracleBuckets(byte(b))
			v := uint64(8*file + 2*b + 1)
			seq := core.Seq(4*file + b + 1)
			p.write(t, oracleFiles[file], s.segs[seg], e, uint32(b), v, uint64(seq))
			s.o.noted[file] = append(s.o.noted[file], orec{seg: seg, e: e, v: v, seq: seq})
		}
	}
	p.finish(t)
	return s
}

// check asserts Held agrees for every segment after one step. SAFETY
// names the direction nothing may take — the tracker answering not
// held while the exact rule holds is data resurrection; EQUALITY names
// the other — the tracker holding what no re-test event released.
func (s *scenario) check(t *testing.T, step int, what string) {
	t.Helper()
	for i := range oracleSegs {
		got := s.tr.Held(s.segs[i])
		want := s.o.held(s.f.ix, s.segs, i)
		if got != want {
			if want && !got {
				t.Fatalf("step %d (%s): SAFETY: tracker Held of segment %d = %v, want %v: the exact oracle holds it and the tracker answers not held, data resurrection", step, what, s.segs[i], got, want)
			}
			t.Fatalf("step %d (%s): EQUALITY: tracker Held of segment %d = %v, want %v: the exact rule holds nothing and the tracker does, a missing re-test event", step, what, s.segs[i], got, want)
		}
	}
}

// liveCount returns how many segments still live.
func (s *scenario) liveCount() int {
	n := 0
	for i := range oracleSegs {
		if s.lives[i] {
			n++
		}
	}
	return n
}

// liveSeg draws a segment slot that still lives: a retirement draw
// respawns itself off the segments the run already retired.
func (s *scenario) liveSeg(r *rand.Rand) byte {
	for {
		i := byte(r.IntN(oracleSegs))
		if s.lives[i] {
			return i
		}
	}
}

// opKind is one step of the workload.
type opKind byte

const (
	opWrite   opKind = 0 // a write or overwrite of one bucket
	opRemove  opKind = 1 // a removal record of one bucket, loss-shaped: marker, register, all together
	opSettle  opKind = 2 // a settle of one file at a drawn version
	opRetest  opKind = 3 // a plain re-test of one file, the event a copies-done carries
	opRetire  opKind = 4 // a retirement of one segment
	opNopKind opKind = 5 // decoded padding: consumes its bytes, changes nothing
)

// op is one step, the form both the property draw and the fuzz decode
// hand the step runner.
type op struct {
	kind   opKind
	file   byte
	bucket byte
	seg    byte
	v, seq uint64
}

// run applies one step to the tracker, the index and the oracle
// together: the real calls the write path and the reclamation events
// make on one side, the exact model's own account of the same step on
// the other. A write into a retired segment slot or a retirement of a
// retired one changes nothing: the caller's draws never make them, and
// the fuzz body's bytes may.
func (s *scenario) run(t *testing.T, step int, o op) {
	t.Helper()
	switch o.kind {
	case opWrite:
		if !s.lives[o.seg] {
			return
		}
		e := oracleBuckets(o.bucket)
		at := uint32(step+1) << 16
		if _, err := s.f.ix.WriteRetain(tag, oracleFiles[o.file], index.Placed{
			Extent:  e,
			Record:  index.RecordAt{Segment: s.segs[o.seg], Offset: at},
			Version: ver(o.v),
		}, core.Seq(o.seq)); err != nil {
			t.Fatalf("step %d: WriteRetain of bucket %d of file %d: %v", step, o.bucket, o.file, err)
		}
		s.o.noted[o.file] = append(s.o.noted[o.file],
			orec{seg: o.seg, e: e, v: o.v, seq: core.Seq(o.seq)})
	case opRemove:
		if !s.lives[o.seg] {
			return
		}
		e := oracleBuckets(o.bucket)
		// The removal applies to the index as the loss paths do — the
		// marker left below the removal's version over the bucket —
		// and the tracker registers the record live. Even-bucket draws
		// are release-shaped, an extent of their own and no marker
		// term; odd-bucket ones truncate-shaped, the header-only
		// offset-only Rec the planted tests use, so covered() spells
		// them to the end of the file and the marker term applies —
		// the removal rule of RFC 1 §7.2's planted case: a truncate
		// kept by a record whose sequence is higher and whose version
		// is lower.
		if _, _, err := s.f.ix.Lose(oracleFiles[o.file], e, ver(o.v)); err != nil {
			t.Fatalf("step %d: Lose over bucket %d of file %d: %v", step, o.bucket, o.file, err)
		}
		s.o.markers[o.file] = append(s.o.markers[o.file], omarker{v: o.v})
		var (
			rec        Rec
			needMarker bool
			covered    = e
		)
		if o.bucket%2 == 0 {
			rec = Rec{Kind: format.KindRelease, File: oracleFiles[o.file], Seg: s.segs[o.seg], Ext: e}
		} else {
			rec = Rec{Kind: format.KindTruncate, File: oracleFiles[o.file], Seg: s.segs[o.seg], Ext: core.Extent{Off: e.Off}}
			needMarker = true
			covered = core.Extent{Off: e.Off, Len: math.MaxInt64 - e.Off}
		}
		rec.Pair = index.Numbers{Version: ver(o.v), Seq: core.Seq(o.seq)}
		rec.Seq = core.Seq(o.seq)
		s.tr.Register(rec)
		s.o.live[o.file] = append(s.o.live[o.file],
			orec{seg: o.seg, e: covered, v: o.v, seq: core.Seq(o.seq), needMarker: needMarker})
	case opSettle:
		// The journal's own settle first, then the tracker's relay of
		// it, where the relay's own Settle meets one already run and
		// idempotently passes.
		s.f.ix.Settle(oracleFiles[o.file], ver(o.v))
		s.tr.MarkerSettled(oracleFiles[o.file], ver(o.v))
		s.o.settle(o.file, o.v)
	case opRetest:
		s.tr.Retest(oracleFiles[o.file])
		s.o.ret(o.file)
	case opRetire:
		if !s.lives[o.seg] {
			return
		}
		s.lives[o.seg] = false
		// The complete list a journal's own retirement carries — every
		// file the segment held content of or a registration for —
		// because a caller's list the tracker fills from its own
		// registry names only registered records, and a file whose
		// content sat in the segment unregistered would keep the
		// index's drop ranges of a segment long gone.
		var files []core.FileID
		var ofiles []byte
		for file := range 2 {
			in := slices.ContainsFunc(s.o.noted[file], func(x orec) bool { return x.seg == o.seg }) ||
				slices.ContainsFunc(s.o.live[file], func(x orec) bool { return x.seg == o.seg })
			if in {
				files = append(files, oracleFiles[file])
				ofiles = append(ofiles, byte(file))
			}
		}
		s.tr.SegmentRetired(s.segs[o.seg], files)
		// The durable half of the retirement, the retirer's durable signal:
		// the ranges leave the index only once the unlink is durable, so
		// the scenario runs both halves back to back — the oracle's retire
		// drops the segment's ranges whole, the way the two calls do
		// together.
		s.tr.RemovalDurable(s.segs[o.seg])
		s.o.retire(o.seg, ofiles)
	case opNopKind:
	}
}

// TestHeldOracleProperty: 200 seeded sequences of 30-60 steps over the
// five segments — writes, overwrites, removals, settles, re-tests and
// retirements — with Held of every segment checked against the exact
// oracle after every step. Every record's version is drawn
// independently of its append, so pairs land above and below the
// planted records' and around each other; one draw in six reuses the
// previous step's sequence number, so equal-prefix pairs sit beside
// records only the (version, sequence) comparison can order — the
// planted shape of RFC 1 §7.2 among them: a removal outranked by
// nothing of a higher version, kept by a record whose sequence is
// higher and whose version is lower.
func TestHeldOracleProperty(t *testing.T) {
	for seed := range 200 {
		t.Run(fmt.Sprintf("seed=%d", seed), func(t *testing.T) {
			r := rand.New(rand.NewPCG(uint64(seed), 0x25add1c6))
			s := drawScenario(t)
			steps := 30 + r.IntN(31)
			seq := uint64(100 + seed*64) // above every planted sequence number
			for step := range steps {
				var o op
				switch n := r.IntN(100); {
				case n < 30:
					o.kind = opWrite
				case n < 60:
					o.kind = opRemove
				case n < 75:
					o.kind = opSettle
				case n < 85:
					o.kind = opRetest
				default:
					o.kind = opRetire
				}
				if (o.kind == opWrite || o.kind == opRemove || o.kind == opRetire) && s.liveCount() == 0 {
					o.kind = opSettle
				}
				o.file = byte(r.IntN(2))
				o.bucket = byte(r.IntN(4))
				switch o.kind {
				case opWrite, opRemove, opRetire:
					o.seg = s.liveSeg(r)
				}
				if o.kind == opWrite || o.kind == opRemove {
					o.v = 1 + uint64(r.IntN(16))
					if r.IntN(6) > 0 {
						seq++
					}
					o.seq = seq
				}
				if o.kind == opSettle {
					o.v = 1 + uint64(r.IntN(16))
				}
				s.run(t, step, o)
				s.check(t, step, fmt.Sprintf("%d/%d kind=%d file=%d bucket=%d seg=%d v=%d seq=%d",
					step, steps, o.kind, o.file, o.bucket, o.seg, o.v, o.seq))
			}
		})
	}
}
