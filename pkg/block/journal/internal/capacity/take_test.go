package capacity

import (
	"context"
	"errors"
	"log/slog"
	"math"
	"runtime"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/durability"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/format"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/index"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/segment"
	"github.com/cubbit/dittofs/pkg/block/journal/journaltest/kit"
)

// The fixed figures of every rig: a cap small enough that a real pool makes
// its spares quickly in a temporary directory, and a version limit no test
// reaches.
const (
	testCap  = int64(64 << 10)
	limitBig = uint64(1) << 40
)

// tag1 is the share every test request writes as; tagOther is the share a
// rig's own filler records are written as, so they never touch tag1's figures.
const (
	tag1     = core.ShareTag(1)
	tagOther = core.ShareTag(99)
)

// rigOpts shapes a rig's real collaborators. Build one from std.
type rigOpts struct {
	// spares is the content spares left ready once the stream is primed.
	spares int
	// unprimed leaves stream 1 with no active segment, so its room is zero
	// and a write's every piece rolls.
	unprimed bool
	// room, when positive on a primed rig, is the most payload room the
	// stream is left with: a filler record on another tag spends the rest.
	room int64
	// makerMax is the maker's journal maximum; zero bounds it to exactly the
	// segments the rig makes, so the pool never grows past the spares named.
	makerMax int64
	// maxBytes is the admission's journal maximum; zero bounds nothing.
	maxBytes int64
	// indexBound is the index's entry bound; zero is generous.
	indexBound int64
	// noVersions leaves the counters with no version limit, so none is left.
	noVersions bool
	// cfg adjusts the Config before New runs.
	cfg func(*Config)
}

// std is the rig most tests want: three ready spares, a primed stream.
func std() rigOpts { return rigOpts{spares: 3} }

// rig is a Capacity over the real collaborators the datapath wires it to: a
// real segment set, maker and pool over a test directory, real streams, a real
// index and counters, and a real durability layer. Where a test needs a delay,
// a park or a refusal, it sets one of the thin fault wrappers around the real
// call (holdSpy, holdFault, or a wrapper it puts around a Config func).
type rig struct {
	c       *Capacity
	set     *segment.Set
	pool    *segment.Pool
	idx     *index.Index
	ctr     *index.Counters
	dur     *durability.Durability
	streams *segment.Streams

	// holds and checks record the calls that reach the pool's Hold and the
	// index's CheckWrite; both pass the call through.
	holds, checks atomic.Int64

	// holdSpy, when set, runs before each real hold with the capacity mutex
	// released, observing or parking the call.
	holdSpy func(n int)
	// holdFault, when set, runs before each real hold; a non-nil error fails
	// that hold in place of the pool, a nil one lets the pool answer.
	holdFault func(n int) error

	files atomic.Uint64
	seq   atomic.Uint64
}

// domains is the file append domain table the durability layer wants: one
// mutex per file.
type domains struct{ m sync.Map }

func (d *domains) Lock(id core.FileID) func() {
	mu, _ := d.m.LoadOrStore(id, new(sync.Mutex))
	mu.(*sync.Mutex).Lock()
	return mu.(*sync.Mutex).Unlock
}

// newRig builds the rig o names and waits, on exact state, until the pool and
// the set agree the spares are made.
func newRig(t *testing.T, o rigOpts) *rig {
	t.Helper()

	seam, err := kit.NewSeam(t.TempDir())
	if err != nil {
		t.Fatalf("NewSeam: %v", err)
	}
	r := &rig{ctr: &index.Counters{}}
	if !o.noVersions {
		if err := r.ctr.SetVersionLimit(core.Version{Counter: limitBig}); err != nil {
			t.Fatalf("SetVersionLimit: %v", err)
		}
	}
	bound := o.indexBound
	if bound == 0 {
		bound = 1 << 20
	}
	r.idx = index.New(bound)
	d, err := durability.New(durability.Config{
		Clock:    kit.NewStopwatchClock(),
		Domains:  &domains{},
		Index:    r.idx,
		Dir:      t.TempDir(),
		Counters: r.ctr,
		MarkLoss: r.idx.Lose,
	})
	if err != nil {
		t.Fatalf("durability.New: %v", err)
	}
	r.dur = d

	r.set = segment.NewSet(testCap, nil)
	// The pool keeps one spare more than asked while the stream is primed, for
	// the prime takes one; the maker's maximum keeps it from making another.
	target, made := o.spares, o.spares
	if !o.unprimed {
		target, made = o.spares+1, o.spares+1
	}
	makerMax := o.makerMax
	if makerMax == 0 {
		makerMax = max(int64(made), 1) * testCap
	}
	maker := segment.NewMaker(seam.FS(), r.set, makerMax, 0, nil)
	r.pool = segment.NewPool(maker, segment.Targets{Content: target}, d.Admit, nil)
	cache := segment.NewDescriptors(seam.FS(), format.SegmentName, core.NewDescriptorBudget(64))
	r.streams = segment.NewStreams(2, core.JournalID{0xc, 0xa, 0x9}, r.set, r.pool, cache, d.Sink)
	d.Start(r.streams)
	r.pool.Start()
	t.Cleanup(func() {
		d.Stop()
		_ = r.streams.Close()
		r.pool.Stop()
		_ = cache.Shutdown()
	})
	r.awaitSpares(t, target)

	if !o.unprimed {
		r.prime(t, o.room)
		r.awaitSpares(t, o.spares)
	}

	c := Config{
		Admit: d.Admit,
		CheckIndex: func(f core.FileID, e core.Extent) (int64, error) {
			r.checks.Add(1)
			return r.idx.CheckWrite(f, e)
		},
		CheckFillIndex: r.idx.CheckFillIndexBound,
		VersionsLeft:   r.ctr.VersionsLeft,
		DrawVersion:    r.ctr.Draw,
		EntriesLeft:    r.idx.EntriesLeft,
		Footprint:      func() int64 { return r.set.Counters().UsedBytes },
		ShareFigures:   r.set.ShareBytesAndRetired,
		Room: func(stream int) int64 {
			// Take must hold its own section when this runs: a caller
			// snapshotting the room before the section could take the
			// capability mutex here, which is the ordering violation
			// the in-section read exists to close. The TryLock is the
			// test's own assertion, the pattern the hold spy uses.
			if r.c.mu.TryLock() {
				r.c.mu.Unlock()
				t.Errorf("Room read outside the capacity section: a caller snapshot can straddle a landing")
			}
			return r.streams.Stream(stream).Room()
		},
		ReadySpares: r.pool.ReadyContent,
		Hold:        r.hold,
		BelowHalf:   func() bool { return false },
		MaxBytes:    o.maxBytes,
		SegmentCap:  testCap,
	}
	if o.cfg != nil {
		o.cfg(&c)
	}
	r.c = New(c)
	return r
}

// awaitSpares waits, on the pool's and the set's own counts, until exactly n
// content spares are ready and none is being made.
func (r *rig) awaitSpares(t *testing.T, n int) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(time.Millisecond) {
		if p, s := r.pool.Counters(), r.set.Counters(); p.Spares == int64(n) && s.Spares == int64(n) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the pool never readied %d spares: %+v, set %+v", n, r.pool.Counters(), r.set.Counters())
		}
	}
}

// prime gives stream 1 an active segment by appending one filler record on
// tagOther; room > 0 sizes the filler so the stream is left at most that much
// payload room.
func (r *rig) prime(t *testing.T, room int64) {
	t.Helper()
	p := int64(0)
	if room > 0 {
		for ; segment.WriteRoom(testCap, format.SegmentHeaderSize+segment.RecordBytes(p), 1) > room; p += 8 {
			if p > segment.EmptyRoom(testCap) {
				t.Fatalf("no filler leaves a room of %d", room)
			}
		}
	}
	r.append(t, segment.Roll{}, tagOther, p)
}

// room is the payload room stream 1 has now.
func (r *rig) room() int64 { return r.streams.Stream(1).Room() }

// file returns a file id of its own.
func (r *rig) file() core.FileID {
	return core.NewFileID([16]byte{byte(r.files.Add(1)), 0xf})
}

// append appends one real write record of payload bytes under tag to stream 1,
// rolling as roll says, and returns where it landed and how many payload bytes
// it placed (a payload past the room is split at the segment's edge).
func (r *rig) append(t *testing.T, roll segment.Roll, tag core.ShareTag, payload int64) segment.Placed {
	t.Helper()
	seq := r.seq.Add(1)
	p, err := r.streams.Stream(1).AppendRoll(format.RecordHeader{
		Kind: format.KindWrite, File: r.file(), Extent: core.Extent{Len: payload},
		Seq: core.Seq(seq), Version: core.Version{Counter: seq}, Tag: tag,
	}, format.RecordBody{Payload: make([]byte, payload), MTime: 1}, roll)
	if err != nil {
		t.Fatalf("Append(%d): %v", payload, err)
	}
	return p
}

// seed lands a real record of payload bytes under tag1, as a recovered open or
// an earlier write would have, and returns its segment.
func (r *rig) seed(t *testing.T, payload int64) core.SegmentID {
	t.Helper()
	p := r.append(t, segment.Roll{}, tag1, payload)
	if p.N != payload {
		t.Fatalf("the seed record placed %d of %d bytes", p.N, payload)
	}
	return p.Loc.Segment
}

// land appends the whole of g's write as real records, the way the datapath
// does, handing each piece to g.Wrote once it is appended.
func (r *rig) land(t *testing.T, g *Grant, payload int64) {
	t.Helper()
	for done := int64(0); done < payload; {
		p := r.append(t, segment.Roll{Hold: g.Hold()}, tag1, payload-done)
		g.Wrote(segment.RecordBytes(p.N))
		done += p.N
	}
}

// seal seals stream 1's active segment, so the set may retire it.
func (r *rig) seal(t *testing.T) {
	t.Helper()
	if _, err := r.streams.Stream(1).Seal(false); err != nil {
		t.Fatalf("Seal: %v", err)
	}
}

// retire removes the sealed segment seg from the set, the move a retirement
// makes of the set's figures: the settled bytes fall and the cumulative
// retired bytes rise.
func (r *rig) retire(t *testing.T, seg core.SegmentID) {
	t.Helper()
	if _, err := r.set.Retire(seg); err != nil {
		t.Fatalf("Retire(%d): %v", seg, err)
	}
}

// fillIndex publishes one write on a file of its own, which is the whole of a
// one-entry index's bound, so the next write on another file is refused.
func (r *rig) fillIndex(t *testing.T) {
	t.Helper()
	seq := core.Seq(r.seq.Add(1))
	p := index.Placed{Extent: core.Extent{Len: 4096}, Record: index.RecordAt{Segment: 1}, Version: core.Version{Counter: uint64(seq)}}
	if _, err := r.idx.WriteRetain(tag1, r.file(), p, seq); err != nil {
		t.Fatalf("WriteRetain: %v", err)
	}
}

// awaitReady waits, on the pool's own count, until n content spares are ready.
func (r *rig) awaitReady(t *testing.T, n int) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); r.pool.ReadyContent() < n; time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("the pool never readied %d spares: %+v", n, r.pool.Counters())
		}
	}
}

// hold is the capacity's Hold func: a recording wrapper around the pool's
// Hold, with the spy and the fault the test set.
func (r *rig) hold(n int) (*segment.Hold, error) {
	r.holds.Add(1)
	if spy := r.holdSpy; spy != nil {
		spy(n)
	}
	if f := r.holdFault; f != nil {
		if err := f(n); err != nil {
			return nil, err
		}
	}
	return r.pool.Hold(n)
}

// failHolds makes the next times holds fail with err in place of the pool,
// and then lets the pool answer again.
func (r *rig) failHolds(err error, times int64) {
	var left atomic.Int64
	left.Store(times)
	r.holdFault = func(int) error {
		if left.Add(-1) >= 0 {
			return err
		}
		return nil
	}
}

// parkFirstHold makes the first hold signal started and wait for release
// before the pool answers it.
func (r *rig) parkFirstHold() (started <-chan struct{}, release func()) {
	in, gate := make(chan struct{}, 1), make(chan struct{})
	var once sync.Once
	r.holdSpy = func(int) {
		once.Do(func() {
			in <- struct{}{}
			<-gate
		})
	}
	return in, func() { close(gate) }
}

// share is tag1's settled record bytes as the set reports them.
func (r *rig) share() int64 { return r.set.ShareBytes(tag1) }

// req is the request every test starts from: tag1, stream 1, 4096 payload
// bytes that fit the stream's room, no share limit.
func req() Request {
	return Request{
		Tag:     tag1,
		Stream:  1,
		Extent:  core.Extent{Off: 0, Len: 4096},
		Payload: 4096,
	}
}

// grant calls Take and fails the test on any error.
func (r *rig) grant(t *testing.T, rq Request) *Grant {
	t.Helper()
	g, err := r.c.Take(context.Background(), rq)
	if err != nil {
		t.Fatalf("Take(%+v): %v", rq, err)
	}
	return g
}

// refused calls Take and fails the test unless it was refused with the wanted
// limit, returning the refusal's fields for the callers that go on asserting
// them; a caller that checks the naming alone discards the return
// explicitly, for the helper fails the test on a wrong refusal either way.
func (r *rig) refused(t *testing.T, rq Request, want core.Limit) *core.NoSpaceError {
	t.Helper()
	g, err := r.c.Take(context.Background(), rq)
	if err == nil {
		g.Release()
		t.Fatalf("Take(%+v) was granted, want a refusal naming %s", rq, want)
	}
	var ns *core.NoSpaceError
	if !errors.As(err, &ns) {
		t.Fatalf("Take(%+v) = %v, want a *core.NoSpaceError", rq, err)
	}
	if want != 0 && ns.Limit != want {
		t.Fatalf("Take(%+v) refused by %s, want %s", rq, ns.Limit, want)
	}
	if got := r.c.ReservedBytes(); got != 0 {
		t.Fatalf("after the refusal by %s, ReservedBytes = %d, want 0", ns.Limit, got)
	}
	return ns
}

// TestTakeFillIgnoresVersionLimit: with no version left to draw, a write is
// refused naming the version limit and a fill of the same ask is granted, for
// a fill draws no version. The fill's reservation is real, so every other
// limit still sees it.
func TestTakeFillIgnoresVersionLimit(t *testing.T) {
	o := std()
	o.noVersions = true
	r := newRig(t, o)
	r.seed(t, 0)

	_ = r.refused(t, req(), core.LimitVersion)

	q := req()
	q.Fill = true
	g := r.grant(t, q)
	if got := r.c.ReservedBytes(); got <= 0 {
		t.Fatalf("after the granted fill, ReservedBytes = %d, want the fill's ask held", got)
	}
	g.Release()
	if got := r.c.ReservedBytes(); got != 0 {
		t.Fatalf("after Release, ReservedBytes = %d, want 0", got)
	}
}

// TestTakeFillRefusedByOtherLimits: a fill passes the version limit's skip
// alone. The share's limit, the journal's maximum and the spare pool each
// still refuse it, naming themselves, with no version left.
func TestTakeFillRefusedByOtherLimits(t *testing.T) {
	// Share.
	o := std()
	o.noVersions, o.maxBytes = true, 1
	r := newRig(t, o)
	r.seed(t, 0)
	q := req()
	q.Fill, q.ShareLimit = true, 1
	_ = r.refused(t, q, core.LimitShare)

	// Journal: a write beyond its room, the journal at its maximum, and no
	// spare ready.
	o = std()
	o.noVersions, o.unprimed, o.spares, o.maxBytes = true, true, 0, 1
	r = newRig(t, o)
	big := req()
	big.Fill, big.Payload = true, 2*testCap
	_ = r.refused(t, big, core.LimitJournal)

	// Spare pool: three spares needed with two ready.
	o = std()
	o.noVersions, o.unprimed, o.spares = true, true, 2
	r = newRig(t, o)
	big = req()
	big.Fill, big.Payload = true, 2*segment.EmptyRoom(testCap)+50
	_ = r.refused(t, big, core.LimitSpare)
}

// TestTake_ZeroGapFillAtFullIndex checks a fill that adds no index entry is
// admitted at an exactly-full index: the bound gates what a call adds, and a
// fill whose every part is already held adds none.
func TestTake_ZeroGapFillAtFullIndex(t *testing.T) {
	o := std()
	o.indexBound = 1
	r := newRig(t, o)

	// The index's one entry is a 4096-byte piece this fill covers whole, so
	// the fill has no gap and leaves the count where it stands.
	id := core.NewFileID([16]byte{1, 0xf})
	p := index.Placed{Extent: core.Extent{Len: 4096}, Record: index.RecordAt{Segment: 1}, Version: core.Version{Counter: 1}}
	if _, err := r.idx.WriteRetain(tag1, id, p, 1); err != nil {
		t.Fatalf("WriteRetain: %v", err)
	}

	q := req()
	q.File, q.Fill = id, true
	g, err := r.c.Take(context.Background(), q)
	if err != nil {
		t.Fatalf("a fill that adds no entry at an exactly-full index: %v", err)
	}
	g.Release()
}

// TestTake_OrderOfRefusals drives one request every limit refuses at once
// and checks the first still standing names itself: read-only, then version,
// index, share, journal, and the spare pool last. The closing case holds: a
// write over its share's limit with no spare ready names the share, not the
// pool.
func TestTake_OrderOfRefusals(t *testing.T) {
	// everyLimit is a rig whose every limit refuses the request below: no
	// version left, a full index, a share over its limit, a journal maximum
	// of one byte.
	everyLimit := func(t *testing.T) *rig {
		o := std()
		o.noVersions, o.indexBound, o.maxBytes = true, 1, 1
		r := newRig(t, o)
		r.seed(t, 0)
		r.fillIndex(t)
		return r
	}
	q := req()
	q.ShareLimit = 1

	// Read-only first: the sentinel itself, nothing reserved, no hold asked,
	// no other limit consulted. Every other limit refuses this request too,
	// so only the order can name the read-only state.
	r := everyLimit(t)
	r.dur.Enter(errors.New("the directory turned read-only"))
	_, err := r.c.Take(context.Background(), q)
	if !errors.Is(err, core.ErrJournalReadOnly) {
		t.Fatalf("a read-only journal refused with %v, want ErrJournalReadOnly", err)
	}
	if got := r.c.ReservedBytes(); got != 0 {
		t.Fatalf("a read-only refusal reserved %d bytes, want 0", got)
	}
	if got := r.checks.Load(); got != 0 {
		t.Fatalf("a read-only refusal ran the index pre-check %d times, want 0", got)
	}
	if got := r.holds.Load(); got != 0 {
		t.Fatalf("a read-only refusal asked for %d holds, want 0", got)
	}

	// Version before index, share, journal and spare.
	_ = everyLimit(t).refused(t, q, core.LimitVersion)

	// Index before share, journal and spare.
	o := std()
	o.indexBound, o.maxBytes = 1, 1
	r = newRig(t, o)
	r.seed(t, 0)
	r.fillIndex(t)
	_ = r.refused(t, q, core.LimitIndex)

	// Share before journal and spare.
	o = std()
	o.maxBytes = 1
	r = newRig(t, o)
	r.seed(t, 0)
	ns := r.refused(t, q, core.LimitShare)
	if ns.Unit != core.UnitBytes {
		t.Fatalf("the share refusal counts %s, want bytes", ns.Unit)
	}

	// Journal before spare: a write beyond its room, the journal at its
	// maximum, and the pool holding no spare either, is refused by the
	// maximum.
	o = std()
	o.unprimed, o.spares, o.maxBytes = true, 0, 1
	r = newRig(t, o)
	big := req()
	big.Payload = 2 * testCap
	_ = r.refused(t, big, core.LimitJournal)

	// The spare pool last, and only when no other limit refuses: the same
	// write with no maximum, needing three spares with two ready.
	o = std()
	o.unprimed, o.spares = true, 2
	r = newRig(t, o)
	big = req()
	big.Payload = 2*segment.EmptyRoom(testCap) + 50
	ns = r.refused(t, big, core.LimitSpare)
	if ns.Missing != 1 || ns.Unit != core.UnitSegments {
		t.Fatalf("the spare refusal = %d %s, want one segment", ns.Missing, ns.Unit)
	}

	// A write over its share's limit with no spare ready names the share:
	// the pool answers only when every other limit would admit the write.
	o = std()
	o.spares = 0
	r = newRig(t, o)
	r.seed(t, 0)
	over := req()
	over.ShareLimit = 2
	_ = r.refused(t, over, core.LimitShare)
}

// TestTake_NamedRefusalFields checks each limit's refusal fields: the share
// and the journal name bytes; the index entries; the version versions; the
// spare pool segments. The journal case holds MaxBytes half a cap above the
// footprint and a write needing two spares with one ready, so the missing
// bytes are half a cap.
func TestTake_NamedRefusalFields(t *testing.T) {
	// Share: the shortfall in bytes, taken against the record bytes the
	// admission draws for the payload, so the refused figure names the
	// record over the limit.
	r := newRig(t, std())
	seed := segment.RecordBytes(0)
	r.seed(t, 0)
	q := req() // payload 4096 in the stream's room: one record
	q.ShareLimit = 4100
	ns := r.refused(t, q, core.LimitShare)
	if want := seed + segment.RecordBytes(4096) - 4100; ns.Missing != want || ns.Unit != core.UnitBytes {
		t.Fatalf("the share refusal = %d %s, want %d bytes", ns.Missing, ns.Unit, want)
	}

	// Journal: a write needing two spares with one ready; the footprint is
	// the one ready spare's cap and the maximum sits half a cap above it, so
	// the uncovered spare's cap exceeds the maximum's room by half a cap: the
	// missing the refusal names is the difference the formula draws.
	o := std()
	o.unprimed, o.spares, o.maxBytes = true, 1, testCap+testCap/2
	r = newRig(t, o)
	q = req()
	q.Payload = segment.EmptyRoom(testCap) + 50
	ns = r.refused(t, q, core.LimitJournal)
	if ns.Missing != testCap/2 || ns.Unit != core.UnitBytes {
		t.Fatalf("the journal refusal = %d %s, want half a cap in bytes", ns.Missing, ns.Unit)
	}

	// Index: the index's own refusal, forwarded untouched.
	o = std()
	o.indexBound = 1
	r = newRig(t, o)
	r.fillIndex(t)
	_, cerr := r.idx.CheckWrite(req().File, req().Extent)
	want := asNoSpace(cerr)
	if want == nil || want.Limit != core.LimitIndex {
		t.Fatalf("the full index answered %v, want a refusal naming the index", want)
	}
	ns = r.refused(t, req(), core.LimitIndex)
	if ns.Missing != want.Missing || ns.Unit != core.UnitEntries || want.Unit != core.UnitEntries {
		t.Fatalf("the index refusal = %d %s, want the index's own %d %s", ns.Missing, ns.Unit, want.Missing, want.Unit)
	}

	// Version: one version.
	o = std()
	o.noVersions = true
	r = newRig(t, o)
	ns = r.refused(t, req(), core.LimitVersion)
	if ns.Missing != 1 || ns.Unit != core.UnitVersions {
		t.Fatalf("the version refusal = %d %s, want one version", ns.Missing, ns.Unit)
	}

	// Spare: the ready spares short of what the pieces need. One ready,
	// three pieces need spares, so two are missing.
	o = std()
	o.unprimed, o.spares = true, 1
	r = newRig(t, o)
	q = req()
	q.Payload = 2*segment.EmptyRoom(testCap) + 50
	ns = r.refused(t, q, core.LimitSpare)
	if ns.Missing != 2 || ns.Unit != core.UnitSegments {
		t.Fatalf("the spare refusal = %d %s, want two segments", ns.Missing, ns.Unit)
	}
}

// TestTake_FitsRoomTakesNoSpare runs four hundred concurrent takes that all
// fit the stream's room together (each claims a record and its footer entry of
// the real room, and four hundred of them sit inside it), with no ready spare and the journal's maximum
// already reached, and checks every one is granted from the room and the
// pool is never asked: a write that needs no new segment is never refused
// by the maximum, which bounds only whole segments.
func TestTake_FitsRoomTakesNoSpare(t *testing.T) {
	// MaxBytes == the footprint: the primed stream's one segment, a cap, and
	// no spare is ready.
	o := std()
	o.spares, o.maxBytes = 0, testCap
	r := newRig(t, o)
	if used := r.set.Counters().UsedBytes; used != testCap {
		t.Fatalf("the footprint is %d, want the one cap the maximum names", used)
	}

	const takes = 400
	q := req()
	q.Payload = 1
	var wg sync.WaitGroup
	var granted atomic.Int64
	for range takes {
		wg.Add(1)
		go func() {
			defer wg.Done()
			g, err := r.c.Take(context.Background(), q)
			if err != nil {
				t.Errorf("a take that fits the room at the maximum: %v", err)
				return
			}
			granted.Add(1)
			g.Release()
		}()
	}
	wg.Wait()
	if got := granted.Load(); got != takes {
		t.Fatalf("%d of %d takes that fit the room were granted, want all", got, takes)
	}
	if got := r.holds.Load(); got != 0 {
		t.Fatalf("the pool was asked for %d holds, want 0", got)
	}
}

// TestTake_HoldsSparesBeyondRoom drives a write two and a half caps beyond
// its room and checks Take asks the pool for the spares the bytes need —
// three — never zero, and after the critical section ended: the spy asserts
// TryLock on the capacity mutex succeeds while the hold runs.
func TestTake_HoldsSparesBeyondRoom(t *testing.T) {
	o := std()
	o.spares, o.room = 2, testCap/2 // two of the three the write needs
	r := newRig(t, o)

	var asked atomic.Int64
	var underMutex atomic.Bool
	r.holdSpy = func(n int) {
		if n != 3 {
			t.Errorf("the pool was asked for %d spares, want 3", n)
		}
		asked.Add(1)
		if !r.c.mu.TryLock() {
			underMutex.Store(true)
		} else {
			r.c.mu.Unlock()
		}
	}
	q := req()
	// A write 2.5 caps larger than Room: its bytes beyond the room are
	// 2.5 caps, which cross three segments, so three spares are held.
	q.Payload = r.room() + testCap*5/2
	ns := r.refused(t, q, core.LimitSpare)
	if asked.Load() == 0 {
		t.Fatal("Take never asked the pool for the spares a write beyond its room needs")
	}
	if underMutex.Load() {
		t.Error("Hold ran with the capacity mutex held")
	}
	if ns.Missing != 1 {
		t.Fatalf("the spare refusal is %d short, want 1", ns.Missing)
	}
}

// TestTake_RefusedHoldGivesBytesBack checks a hold the pool refuses gives
// the reservation back — ReservedBytes falls to zero, one refusal counts —
// and the same bytes are granted once the pool can hold them. The refusal is
// forced at the one hold, past a ready count that covered the need.
func TestTake_RefusedHoldGivesBytesBack(t *testing.T) {
	o := std()
	o.room = testCap / 2
	r := newRig(t, o)
	r.failHolds(&core.NoSpaceError{Limit: core.LimitSpare, Missing: 2, Unit: core.UnitSegments}, 1)
	q := req()
	q.Payload = 2 * testCap
	before := r.c.RefusedCount()
	_ = r.refused(t, q, core.LimitSpare)
	if got := r.c.RefusedCount(); got != before+1 {
		t.Fatalf("RefusedCount went %d -> %d, want +1", before, got)
	}
	if got := r.c.ReservedBytes(); got != 0 {
		t.Fatalf("after the refused hold, ReservedBytes = %d, want 0", got)
	}

	// The pool can hold the need again: the same bytes are granted.
	g := r.grant(t, q)
	g.Release()
}

// TestTake_MultiSegmentAskHoldsEveryPiece pins the figure the admission
// holds whole segments by against the pieces the stream's own split draws:
// an empty segment takes emptyRoom(cap) payload bytes — the cap less the
// segment header, the seal's reserve and the record's own overhead — so a
// payload of two full segments and a small tail sits in three pieces, and
// the hold must name all three. Bytes divided by the cap name only two, the
// figure a split the stream never draws, and the write it admitted would
// meet the pool's refusal on its third roll, after its first piece landed.
func TestTake_MultiSegmentAskHoldsEveryPiece(t *testing.T) {
	e := segment.EmptyRoom(testCap)
	q := req()
	q.Payload = 2*e + 50
	q.Extent.Len = q.Payload
	o := std()
	o.unprimed, o.spares = true, 0 // a fresh stream: every piece rolls
	r := newRig(t, o)

	ns := r.refused(t, q, core.LimitSpare)
	if ns.Missing != 3 || ns.Unit != core.UnitSegments {
		t.Fatalf("the hold asked for %d segments, want the 3 pieces a payload of %d bytes rolls into", ns.Missing, q.Payload)
	}
}

// TestTake_ExactFit checks a reservation of exactly the remaining space
// succeeds and one byte more is refused, for the share's limit and for the
// journal's maximum.
func TestTake_ExactFit(t *testing.T) {
	// Share: an ask that fits the remaining bytes succeeds and a payload
	// one byte larger — its ask crossing the record's grid — is refused
	// with the one byte of the limit it passes.
	r := newRig(t, std())
	r.seed(t, 896) // a 1000-byte record
	q := req()
	// The record a 2896-byte payload costs is 3000 bytes, against a limit
	// that holds it and the 1000-byte seed exactly: the limit bounds the
	// record bytes, so the exact remainder is granted.
	q.Payload = 2896
	q.ShareLimit = 1000 + 3000
	r.grant(t, q).Release()
	q.Payload = 2897 // one payload byte more: the record crosses the grid to 3008
	ns := r.refused(t, q, core.LimitShare)
	if ns.Missing != 8 {
		t.Fatalf("the share's refusal over the exact ask = %d missing, want 8", ns.Missing)
	}

	// Journal: the maximum bounds whole segments, so the one-byte-over edge
	// runs at the segment's granularity: a write whose tail rolls to one
	// spare is granted when a ready spare covers it, and refused with one
	// byte missing when none is ready and the maximum is one byte short of
	// the cap that spare would need.
	o := std()
	o.spares, o.maxBytes = 1, 2*testCap // the stream's segment and the ready spare
	r = newRig(t, o)
	q = req()
	q.Payload = r.room() + 1 // nearly full: the tail rolls to one spare
	r.grant(t, q).Release()

	o = std()
	o.spares, o.maxBytes = 0, 2*testCap-1 // the stream's segment, and a cap less a byte of room
	r2 := newRig(t, o)
	q = req()
	q.Payload = r2.room() + 1
	ns = r2.refused(t, q, core.LimitJournal)
	if ns.Missing != 1 {
		t.Fatalf("the journal's one-byte-over refusal = %d missing, want 1", ns.Missing)
	}
}

// TestTake_AtomicNoReadThenDecide drives sixteen goroutines at a share with
// room for exactly four of their writes and checks exactly four are
// granted. The hook between the counters' read and the decision lets the
// second caller in: two goroutines against one slot, where a
// read-then-decide design grants both. With the mutex section removed from
// Take this test fails on its assertion — verified by removing the
// section and watching it.
func TestTake_AtomicNoReadThenDecide(t *testing.T) {
	// Sixteen goroutines, four slots: the limit is four asks — an
	// ask being the record a one-byte payload costs — and the grants are held until all sixteen have
	// answered: the decision section decides each take whole, so exactly
	// four pass and twelve are refused, never five.
	r := newRig(t, std())
	q := req()
	q.Payload = 1
	q.ShareLimit = 4 * (segment.RecordBytes(1))
	var wg sync.WaitGroup
	var granted atomic.Int64
	holds := make(chan *Grant, 16)
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			g, err := r.c.Take(context.Background(), q)
			if err == nil {
				granted.Add(1)
				holds <- g
			}
		}()
	}
	wg.Wait()
	close(holds)
	for g := range holds {
		g.Release()
	}
	if got := granted.Load(); got != 4 {
		t.Fatalf("%d of 16 takes were granted, want exactly 4", got)
	}

	// The hook: the first caller parks in it inside the decision section,
	// after the counters' read and before the reservation moves, holding
	// the section. Only then does the second caller start: it blocks on
	// the mutex, so the first's reservation is what its read must see. Let
	// the first finish — the second reads the byte the first reserved and
	// is refused by the limit of one, so exactly one grant stands. A
	// read-then-decide design has both read before either reserves — the
	// hook would hold no section — and both are granted.
	r = newRig(t, std())
	hookIn := make(chan struct{}, 1)
	hookRelease := make(chan struct{})
	var armed atomic.Bool
	armed.Store(true)
	r.c.hook = func() {
		// The armed flag parks one caller only: the second caller's hook
		// finds it disarmed and runs straight through, so the parked first
		// caller is what the test controls, never a second park.
		if !armed.CompareAndSwap(true, false) {
			return
		}
		hookIn <- struct{}{}
		<-hookRelease
	}
	q = req()
	q.Payload = 1
	q.ShareLimit = segment.RecordBytes(1) // room for exactly one ask
	var wg2 sync.WaitGroup
	var granted2 atomic.Int64
	kept := make(chan *Grant, 2)
	take := func() {
		defer wg2.Done()
		g, err := r.c.Take(context.Background(), q)
		if err == nil {
			granted2.Add(1)
			kept <- g
		}
	}
	wg2.Add(1)
	go take()
	<-hookIn
	wg2.Add(1)
	go take()
	// Give the second caller time to block on the mutex the first holds,
	// then let the first finish: its grant is held until both have
	// answered, so the second's read cannot miss its reservation by the
	// first's early release.
	runtime.Gosched()
	hookRelease <- struct{}{}
	wg2.Wait()
	close(kept)
	for g := range kept {
		g.Release()
	}
	if got := granted2.Load(); got != 1 {
		t.Fatalf("%d of 2 takes were granted a share with room for one, want exactly 1", got)
	}
}

// TestTake_NoIONoWait checks a refusal returns without calling Hold, with
// the index pre-check run exactly once, and without blocking: the caller's
// one-second deadline covers the whole refusal.
func TestTake_NoIONoWait(t *testing.T) {
	r := newRig(t, std())
	r.seed(t, 0)
	q := req()
	q.ShareLimit = 2

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := r.c.Take(ctx, q)
		done <- err
	}()
	select {
	case err := <-done:
		var ns *core.NoSpaceError
		if err == nil || !errors.As(err, &ns) || ns.Limit != core.LimitShare {
			t.Fatalf("the refused take = %v, want a share refusal", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the refused take blocked past its caller's deadline")
	}
	if got := r.holds.Load(); got != 0 {
		t.Fatalf("the refusal asked for %d holds, want 0", got)
	}
	if got := r.checks.Load(); got != 1 {
		t.Fatalf("the refusal ran the index pre-check %d times, want 1", got)
	}
}

// TestTake_RefusalCountsAndSpan checks each refusal increments RefusedCount
// by exactly one and opens one reserve_refused span ended with the refusal's
// error, and that an ENOSPC-wrapped error from Hold counts nothing, spans
// nothing and does not match ErrNoSpace, while the reservation it held is
// given back.
func TestTake_RefusalCountsAndSpan(t *testing.T) {
	var mu sync.Mutex
	var spans []*spanRecord
	tracer := func(ctx context.Context, op core.SpanOp, _ ...slog.Attr) (context.Context, func(error)) {
		mu.Lock()
		defer mu.Unlock()
		s := &spanRecord{op: op}
		spans = append(spans, s)
		return ctx, func(err error) {
			mu.Lock()
			defer mu.Unlock()
			s.err, s.ended = err, true
		}
	}
	o := std()
	o.room = testCap / 2
	o.cfg = func(c *Config) { c.Tracer = tracer }
	r := newRig(t, o)

	// One refusal: one count, one ended span with the refusal's error.
	r.seed(t, 0)
	q := req()
	q.ShareLimit = 2
	before := r.c.RefusedCount()
	ns := r.refused(t, q, core.LimitShare)
	if got := r.c.RefusedCount(); got != before+1 {
		t.Fatalf("RefusedCount went %d -> %d, want +1", before, got)
	}
	mu.Lock()
	if len(spans) != 1 || spans[0].op != core.SpanReserveRefused || !spans[0].ended {
		t.Fatalf("after one refusal the spans hold %+v, want one ended reserve_refused span", spans)
	} else if !errors.Is(spans[0].err, core.ErrNoSpace) || !errors.Is(spans[0].err, ns) {
		t.Fatalf("the span ended with %v, want the refusal itself", spans[0].err)
	}
	mu.Unlock()

	// An ENOSPC-wrapped error from Hold: no count, no span, no ErrNoSpace
	// match, and the reservation the failed hold held is returned.
	enospc := &pathError{errno: syscall.ENOSPC}
	r.failHolds(enospc, 1)
	q = req()
	q.Payload = 2 * testCap
	before = r.c.RefusedCount()
	_, err := r.c.Take(context.Background(), q)
	if err == nil {
		t.Fatal("the ENOSPC hold was granted")
	}
	if !errors.Is(err, syscall.ENOSPC) {
		t.Fatalf("the ENOSPC hold's error = %v, want it to match syscall.ENOSPC", err)
	}
	if errors.Is(err, core.ErrNoSpace) {
		t.Fatal("the ENOSPC hold's error matches ErrNoSpace")
	}
	if got := r.c.RefusedCount(); got != before {
		t.Fatalf("RefusedCount moved to %d on an ENOSPC hold, want %d", got, before)
	}
	mu.Lock()
	if len(spans) != 1 {
		t.Fatalf("the ENOSPC hold opened %d spans, want the one refusal's", len(spans))
	}
	mu.Unlock()
	if got := r.c.ReservedBytes(); got != 0 {
		t.Fatalf("after the ENOSPC hold, ReservedBytes = %d, want 0", got)
	}
}

// spanRecord is one span the test tracer opened.
type spanRecord struct {
	op    core.SpanOp
	err   error
	ended bool
}

// pathError stands in for the platform's path error wrapping an errno: it
// matches its errno under errors.Is.
type pathError struct{ errno syscall.Errno }

func (e *pathError) Error() string { return "write seg-0000000000000001: " + e.errno.Error() }
func (e *pathError) Unwrap() error { return e.errno }

// TestGrant_WroteAndRelease checks Wrote moves a piece's bytes out of the
// reservation — by the piece, with the share's already-counted bytes read
// alongside the fallen reservation at the moment the hook runs — and that
// Release after two of three pieces returns the third's bytes and the
// unused spares, closes the hold, and is idempotent. The caller appends the
// record, which counts it in the share, before it calls Wrote, as the append
// path does, so inside Wrote's section the share's bytes hold what the piece
// wrote while the reservation has just fallen by the same amount: the hook
// reads both figures at that moment and the test asserts them together.
func TestGrant_WroteAndRelease(t *testing.T) {
	o := std()
	o.unprimed = true // three spares for a write whose three pieces each roll
	r := newRig(t, o)

	type move struct {
		n, reserved, share int64
	}
	var mu sync.Mutex
	var moves []move
	r.c.wroteHook = func(stream int, n, reserved int64) {
		mu.Lock()
		defer mu.Unlock()
		moves = append(moves, move{n: n, reserved: reserved, share: r.share()})
	}

	e := segment.EmptyRoom(testCap)
	q := req()
	q.Payload = 2*e + 4096
	g := r.grant(t, q)
	asked := r.c.ReservedBytes()
	if asked <= q.Payload {
		t.Fatalf("the ask %d never rose over the payload by its pieces' overhead", asked)
	}

	// Two of three pieces are written: each is appended, which counts it in
	// the share's bytes, before the grant is told, and Wrote lowers the
	// reservation by the piece.
	piece := segment.RecordBytes(e)
	for range 2 {
		p := r.append(t, segment.Roll{Hold: g.Hold()}, tag1, e)
		g.Wrote(segment.RecordBytes(p.N))
	}
	mu.Lock()
	if len(moves) != 2 {
		mu.Unlock()
		t.Fatalf("the wrote hook saw %d moves, want 2", len(moves))
	}
	for i, m := range moves {
		wantShare := int64(i+1) * piece
		if m.n != piece || m.reserved != asked-int64(i+1)*piece {
			mu.Unlock()
			t.Fatalf("move %d = n %d reserved %d, want n %d reserved %d",
				i, m.n, m.reserved, piece, asked-int64(i+1)*piece)
		}
		if m.share != wantShare {
			mu.Unlock()
			t.Fatalf("at move %d the share counts %d beside the fallen reservation, want %d: the record is the share's before the reservation releases",
				i, m.share, wantShare)
		}
	}
	mu.Unlock()
	if got := r.c.ReservedBytes(); got != asked-2*piece {
		t.Fatalf("after two pieces ReservedBytes = %d, want %d", got, asked-2*piece)
	}
	if got := r.share(); got != 2*piece {
		t.Fatalf("the share counts %d after two written pieces, want %d", got, 2*piece)
	}

	// Release returns the third piece's bytes and the unused spares.
	g.Release()
	if got := r.c.ReservedBytes(); got != 0 {
		t.Fatalf("after Release, ReservedBytes = %d, want 0", got)
	}
	if got := r.share(); got != 2*piece {
		t.Fatalf("after Release the share counts %d, want only the two written pieces", got)
	}
	if got := r.holds.Load(); got != 1 {
		t.Fatalf("the pool was asked for %d holds, want 1", got)
	}
	if c := r.pool.Counters(); c.Held != 0 {
		t.Fatalf("after Release the pool still owes %d spares, want 0: the hold is closed", c.Held)
	}
	// Idempotent.
	g.Release()
}

// TestGrant_WroteBeyondReservation checks Wrote past the reservation cannot
// drive ReservedBytes negative: the pieces after Release, or beyond the
// granted bytes, are clamped.
func TestGrant_WroteBeyondReservation(t *testing.T) {
	r := newRig(t, std())
	q := req()
	q.Payload = 100
	g := r.grant(t, q)
	asked := r.c.ReservedBytes()
	g.Wrote(asked)
	g.Wrote(50) // beyond the granted bytes
	if got := r.c.ReservedBytes(); got != 0 {
		t.Fatalf("after writes past the grant, ReservedBytes = %d, want 0", got)
	}
	g.Release()
}

// TestTake_JournalMaximumIsAtomic drives sixteen concurrent takes at a
// journal whose maximum leaves room for exactly one uncovered segment — each
// take needs two spares, and the coverage figure sees one ready — and checks
// the reservation is atomic against the maximum: exactly one take passes
// the journal's check, and every other take is refused by the journal's
// maximum itself, reserving nothing. The uncovered segments a take reserves
// are counted against the maximum inside the decision section until the
// hold answers, so two takes interleaving in the window between the
// reservation and the hold cannot both claim the one segment of room — the
// second is refused by LimitJournal there, whichever way the first's hold
// lands.
func TestTake_JournalMaximumIsAtomic(t *testing.T) {
	// The footprint is the one ready spare's cap, the maximum holds exactly
	// one more, and the first admitted take's hold is parked before the pool
	// answers it, so nothing but the count can mark how many takes have
	// claimed the room while their holds are still pending. The pool's
	// maker may make that second spare when a refused hold asks for it.
	o := std()
	o.unprimed, o.spares = true, 1
	o.makerMax, o.maxBytes = 2*testCap, 2*testCap
	r := newRig(t, o)
	started, release := r.parkFirstHold()

	q := req()
	q.Payload = 3 * (testCap / 2) // no room: the bytes roll into two spares

	var wg sync.WaitGroup
	var granted atomic.Int64
	var byJournal atomic.Int64
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			g, err := r.c.Take(context.Background(), q)
			if err == nil {
				granted.Add(1)
				g.Release()
				return
			}
			var ns *core.NoSpaceError
			if errors.As(err, &ns) {
				if ns.Limit == core.LimitJournal {
					byJournal.Add(1)
				}
			}
		}()
	}
	// Hold the first admitted take inside its hold while the other
	// fifteen run their sections, then let its hold answer. The spin gives
	// the sections ten seconds to answer and gives up on the assertion
	// below, so a reservation that lets them through fails there rather
	// than here.
	<-started
	deadline := time.Now().Add(10 * time.Second)
	for byJournal.Load() < 15 && time.Now().Before(deadline) {
		runtime.Gosched()
	}
	release()
	wg.Wait()
	// The pool holds one ready spare against a hold of two, so it refuses
	// the one admitted take, parked while the others decide: no take can be
	// granted and the storm's observable is which limit refused. Exactly one
	// take — whichever reached the section first — claims the maximum's one
	// segment of room and parks in its hold; the count holds its room
	// against every other take, so all fifteen are refused by the journal's
	// maximum itself, never by the pool.
	if got := granted.Load(); got != 0 {
		t.Fatalf("%d of 16 takes were granted against a pool holding one of the two spares they need, want 0", got)
	}
	if got := byJournal.Load(); got != 15 {
		t.Fatalf("%d of the 15 not-admitted takes were refused by the journal's maximum, want all 15", got)
	}
	if got := r.c.ReservedBytes(); got != 0 {
		t.Fatalf("after the storm, ReservedBytes = %d, want 0", got)
	}

	// The refusals returned every reservation, and the storm left the
	// maximum's room intact: the refused hold asked the pool for the missing
	// spare, and once it is ready a take it can serve is granted.
	r.awaitReady(t, 2)
	g := r.grant(t, q)
	g.Release()
}

// TestTake_NegativeBytesRefused checks a negative Bytes is refused with
// core.ErrInvalidExtent and reserves nothing, before any limit is consulted.
func TestTake_NegativeBytesRefused(t *testing.T) {
	r := newRig(t, std())
	q := req()
	q.Payload = -1
	before := r.c.RefusedCount()
	_, err := r.c.Take(context.Background(), q)
	if !errors.Is(err, core.ErrInvalidExtent) {
		t.Fatalf("a request of negative bytes = %v, want ErrInvalidExtent", err)
	}
	if got := r.c.ReservedBytes(); got != 0 {
		t.Fatalf("a refused invalid request reserved %d bytes, want 0", got)
	}
	if got := r.c.RefusedCount(); got != before {
		t.Fatalf("RefusedCount moved to %d on an invalid request, want %d: an invalid ask is a wiring bug, not a limit's refusal", got, before)
	}
}

// TestTake_BytesOverflowSafe checks a Bytes near MaxInt64 neither wraps the
// spare count negative nor overflows the reservation math: the refusal the
// huge ask meets names a limit, not a wrapped number.
func TestTake_BytesOverflowSafe(t *testing.T) {
	o := std()
	o.unprimed, o.spares = true, 0
	r := newRig(t, o)
	q := req()
	q.Payload = math.MaxInt64 - 1
	ns := r.refused(t, q, 0)
	if ns.Limit != core.LimitJournal && ns.Limit != core.LimitSpare {
		t.Fatalf("a huge ask was refused by %s, want the journal's maximum or the pool's", ns.Limit)
	}
	if ns.Missing < 0 {
		t.Fatalf("the refusal names %d missing, a wrapped negative", ns.Missing)
	}
}

// TestTake_ShareFigureIsExactUnderLandings parks one take between the
// set's read of the share figure and its decision, runs a whole other
// write to completion inside the park (its grant, its landing in the set and
// its Wrote), and checks the parked take is then refused: the figure it
// decides on includes the record that landed after its read. The limit
// leaves room for exactly one ask, so a take that decides on its stale read
// would be granted too.
func TestTake_ShareFigureIsExactUnderLandings(t *testing.T) {
	// The limits: each take asks the 3000 record bytes a 2896-byte payload
	// costs, against a limit holding exactly one ask over the 1000-byte
	// seed, so one take
	// fits and the second finds none.
	r := newRig(t, std())
	const seed = 1000
	r.seed(t, seed-segment.RecordBytes(0))
	ask := recordSplit(2896, r.room(), testCap)
	if ask != 3000 {
		t.Fatalf("the whole ask the admission charges for the payload = %d, want 3000; the test's figures depend on it", ask)
	}

	// The deterministic landing: the victim take parks between the set's
	// read and its decision — the only seam a design that re-reads the
	// set per take has there — while the other writer completes a whole
	// record inside the park: the append that counts it in the set and
	// the Wrote that hands it over. The check that follows must see the
	// landed record whether the take's figure is owned or read. The park is
	// a wrapper around the set's own read: it reads, then waits.
	inPark := make(chan struct{})
	release := make(chan struct{})
	var parked atomic.Bool
	shareRead := func(tag core.ShareTag) (settled, retired int64) {
		v, dn := r.set.ShareBytesAndRetired(tag)
		// The victim's seam: read first, park, then return the stale
		// figure — the read-then-decide order the RFC forbids: the
		// landing below lands inside the park, after this read but
		// before the take it feeds decides. One caller parks; the
		// others pass through.
		if parked.CompareAndSwap(false, true) {
			close(inPark)
			<-release
		}
		return v, dn
	}
	r.c.cfg.ShareFigures = shareRead

	q := req()
	q.Payload = 2896
	q.ShareLimit = seed + ask

	// The victim goes first and parks in the read.
	var wg sync.WaitGroup
	var victim error
	wg.Add(1)
	go func() {
		defer wg.Done()
		g, err := r.c.Take(context.Background(), q)
		if err == nil {
			g.Release()
		}
		victim = err
	}()
	<-inPark

	// The other writers run to completion one after the other inside the
	// park: the first is granted and lands its whole record — the append that
	// counts it in the set and the Wrote that hands it over — before the
	// next takes, and every later take finds the landed record in the figure.
	g := r.grant(t, q)
	r.land(t, g, 2896)
	g.Release()
	_ = r.refused(t, q, core.LimitShare) // the landed record is already in the figure

	// Only now does the victim's decision run, on the figure its stale read
	// seeded: the tally the landing moved, not the read it returns.
	close(release)
	wg.Wait()
	var ns *core.NoSpaceError
	if !errors.As(victim, &ns) || ns.Limit != core.LimitShare {
		t.Fatalf("the parked take = %v, want the share's refusal: the landing inside its window escaped the figure", victim)
	}
}

// TestTake_InterleavedDrawnPieceOversetsTheShareByOneHeader stages the
// same-stream interleaving that draws an extra piece: two takes read the
// same stream room, and the first writer's landing between the second
// take's section and its appends moves the cut, so the larger writer's
// write appends one piece more than the split its take sized. The share's
// limit charges the split's record bytes alone, so both takes are
// admitted against a limit holding exactly the two splits, and the
// settled bytes pass the limit by the drawn piece's header and by no more:
// at most one record header per displaced write, the bound the admission's
// decision states.
func TestTake_InterleavedDrawnPieceOversetsTheShareByOneHeader(t *testing.T) {
	r := newRig(t, std())
	// The stream's room, read by both takes: the larger write fits it whole
	// with a thousand bytes to spare, and the smaller one lands first and
	// spends more than that.
	room := r.room()
	small, large := int64(4096), room-1000
	if segment.RecordBytes(small)+format.CatalogEntrySize <= room-large {
		t.Fatalf("the fixture's figures moved: the small write leaves the large one its room")
	}

	// The limit holds the two writes' record bytes exactly.
	limit := segment.RecordBytes(small) + segment.RecordBytes(large)
	qBig, qSmall := req(), req()
	qBig.Payload, qSmall.Payload = large, small
	qBig.ShareLimit, qSmall.ShareLimit = limit, limit
	gBig, err := r.c.Take(context.Background(), qBig)
	if err != nil {
		t.Fatalf("the first writer's take: %v", err)
	}
	// The same room read, the first writer not yet appended: the smaller
	// split fits the limit's remainder exactly and is granted.
	gSmall, err := r.c.Take(context.Background(), qSmall)
	if err != nil {
		t.Fatalf("the second take at the limit's exact remainder: %v", err)
	}
	r.land(t, gSmall, small)
	gSmall.Release()

	// The larger writer lands with its cut moved: one piece more than its
	// split, whose header is what the share passes the limit by.
	r.land(t, gBig, large)
	gBig.Release()

	hdr := segment.RecordBytes(0)
	if got := r.share(); got != limit+hdr {
		t.Fatalf("the share settled %d, want the limit %d and the one drawn piece's header %d", got, limit, hdr)
	}
	if got := r.c.ReservedBytes(); got != 0 {
		t.Fatalf("after both writers settled, ReservedBytes = %d, want 0", got)
	}
}

// TestTake_ShareTallyNeverUndercountsTheLanded checks the share figure a
// take decides on is never BELOW the bytes the share has settled: with a
// landscape where one write has fully landed (seeded by the set), a take
// after it must still refuse past the limit, and the marker's ceiling — the
// reconciliation with the set — holds for every landed record.
func TestTake_ShareTallyNeverUndercountsTheLanded(t *testing.T) {
	r := newRig(t, std())
	// The share's settled figure the set itself reports (as a recovered
	// open would): the seed every later take stands on.
	r.seed(t, 896) // a 1000-byte record
	q := req()
	q.Payload = 1500
	q.ShareLimit = 4000
	first := segment.RecordBytes(1500)
	g := r.grant(t, q)
	r.land(t, g, 1500)
	g.Release()
	// An unbounded ask the limit never refused: only its own Wrote moved
	// the tally, as every write's does.
	q2 := req()
	q2.Payload = 2000
	q2.ShareLimit = 0
	second := segment.RecordBytes(2000)
	g2 := r.grant(t, q2)
	r.land(t, g2, 2000)
	g2.Release()

	// The next bounded take sees the whole settled figure: it refuses past
	// the limit, never under it.
	q3 := req()
	q3.Payload = 1
	q3.ShareLimit = 4000
	third := segment.RecordBytes(1)
	ns := r.refused(t, q3, core.LimitShare)
	if want := 1000 + first + second + third - 4000; ns.Missing != want {
		t.Fatalf("Missing = %d, want %d: the settled figure plus the whole ask over the limit", ns.Missing, want)
	}
}

// TestTake_RetiredLowersTheTally pins the reconciliation the retirement
// runs: bytes a retirement frees lower the share's figure through the
// capacity's own method, so the retry after a retirement passes and the
// figure is whole.
func TestTake_RetiredLowersTheTally(t *testing.T) {
	r := newRig(t, std())
	seg := r.seed(t, 896) // a 1000-byte record
	q := req()
	// One ask beside the seed: a payload whose record costs 2896 bytes
	// costs 3000 exactly against the 4000 limit.
	q.Payload = 2896
	q.ShareLimit = 4000
	g := r.grant(t, q)
	r.land(t, g, 2896)
	g.Release()

	// The tally holds both records, so the retry finds no room.
	_ = r.refused(t, q, core.LimitShare)

	// A retirement of the segment holding the records frees their bytes: the
	// set's own figure falls, its cumulative retired figure rises, the
	// caller tells the capacity, and the retry fits in the room it freed.
	r.seal(t)
	r.retire(t, seg)
	r.c.Retired(tag1)
	q.ShareLimit = segment.RecordBytes(2896) // fits only if the tally fell to 0
	g2 := r.grant(t, q)
	g2.Release()
}

// TestTake_RoomIsNotGrantedTwice drives two takes on one stream, each
// asking the whole room the stream names, and checks the ledger of
// granted-but-unwritten room convicts the second: the first grant's
// roomPart stands against its own figures, so the second take's bytes
// roll into a spare the pool holds none of and it is refused, never two
// grants of one window.
func TestTake_RoomIsNotGrantedTwice(t *testing.T) {
	o := std()
	o.spares = 0
	r := newRig(t, o)
	room := r.room()
	q := req()
	// The payload that fills the room the stream names, so one grant claims
	// all of it.
	q.Payload = room
	if ask := recordSplit(q.Payload, room, testCap); ask+format.CatalogEntrySize < room {
		t.Fatalf("the fixture asks %d against a room of %d; the figure depends on it", ask, room)
	}

	var wg sync.WaitGroup
	var granted, refused atomic.Int64
	var kept []*Grant
	var mu sync.Mutex
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			g, err := r.c.Take(context.Background(), q)
			if err != nil {
				var ns *core.NoSpaceError
				if errors.As(err, &ns) && ns.Limit == core.LimitSpare {
					refused.Add(1)
					return
				}
				t.Errorf("Take = %v, want the spare's refusal", err)
				return
			}
			granted.Add(1)
			mu.Lock()
			kept = append(kept, g)
			mu.Unlock()
		}()
	}
	wg.Wait()
	if got := granted.Load(); got != 1 {
		t.Fatalf("%d takes granted the one room window, want exactly 1: the second must find its bytes turning into spares the refusal names", got)
	}
	if got := refused.Load(); got != 1 {
		t.Fatalf("%d takes refused, want the other 1", got)
	}
	for _, g := range kept {
		g.Release()
	}
}

// TestTake_TwoWritesInTheRoomAreBothGrantedFromIt pins that a take claims
// from the stream's room only what its landing takes from it: the record's
// bytes and its footer entry. Two writes that together fit the active
// segment, taken one after the other before either lands, are both granted
// from the room with no spare held; the pool holds no spare, so a spare hold
// here is refused and a claim wider than the landing fails the test.
func TestTake_TwoWritesInTheRoomAreBothGrantedFromIt(t *testing.T) {
	const first = 1000
	o := std()
	o.spares, o.room = 0, 5000
	r := newRig(t, o)
	// The room names the second write's payload exactly beyond the first
	// record and its footer entry.
	second := r.room() - segment.RecordBytes(first) - format.CatalogEntrySize
	q := req()
	q.Payload = first
	g1 := r.grant(t, q)
	q.Payload = second
	g2 := r.grant(t, q)
	if g1.Hold() != nil || g2.Hold() != nil {
		t.Fatal("a write that fits the room held spares")
	}
	g1.Release()
	g2.Release()
}

// TestGrant_WroteGivesBackTheEntryRoom pins that a landing releases the
// room it consumed — the record's bytes and its footer entry — so a take
// for another file, made after the landing and before Release, sees the
// room the stream really has and is granted from it, not sent to a spare.
func TestGrant_WroteGivesBackTheEntryRoom(t *testing.T) {
	const first = 1000
	o := std()
	o.spares, o.room = 0, 5000
	r := newRig(t, o)
	q := req()
	q.Payload = first
	g1 := r.grant(t, q)
	// The landing lowers the stream's room by the record and its entry.
	r.land(t, g1, first)
	// The room the stream has now is exactly what the second write needs.
	q.Payload = r.room()
	g2 := r.grant(t, q)
	if g2.Hold() != nil {
		t.Fatal("a write that fits the room held spares while the landed grant was unreleased")
	}
	g1.Release()
	g2.Release()
}

// TestTake_OneReadySpareRefusesNeedOfTwo pins the shape the datapath's
// own spare-refusal test left out: a write needing two spares of a pool
// holding one ready is refused naming the spare, with the pool's holds
// unchanged — the take runs before anything is drawn, so the refused
// hold leaves the holds it found.
func TestTake_OneReadySpareRefusesNeedOfTwo(t *testing.T) {
	o := std()
	o.unprimed, o.spares = true, 1 // the one spare an unreserved take can reach
	r := newRig(t, o)
	q := req()
	q.Payload = 3 * (testCap / 2) // its bytes roll into two spares
	before := r.pool.Counters().Held
	ns := r.refused(t, q, core.LimitSpare)
	if ns.Missing != 1 || ns.Unit != core.UnitSegments {
		t.Fatalf("the spare refusal = %d %s, want one segment missing", ns.Missing, ns.Unit)
	}
	if got := r.pool.Counters().Held; got != before {
		t.Fatalf("the refused take left the pool owing %d holds, want the %d it found", got, before)
	}
}

// TestTake_DrawsTheVersionInsideItsSection checks a plain write's version is
// drawn inside the admission's own section, where its check and draw are one
// step: a reservation's draw parked in that same section cannot pass between a
// plain write's check and its draw and take the version it was admitted on.
func TestTake_DrawsTheVersionInsideItsSection(t *testing.T) {
	r := newRig(t, std())
	if err := r.ctr.SetVersionLimit(core.Version{Counter: 4}); err != nil {
		t.Fatalf("SetVersionLimit: %v", err)
	}
	// A reservation holds one version, leaving one free.
	rs := r.reserve(t, ReserveRequest{Tag: tag1, Bytes: 4304, Writes: 1})
	defer rs.Release()

	// The draw must run under the capacity mutex: a caller that draws outside
	// it can pair its check with another draw's.
	drew := false
	r.c.cfg.DrawVersion = func() (core.Version, error) {
		if r.c.mu.TryLock() {
			r.c.mu.Unlock()
			t.Error("the version was drawn outside the admission's section")
		}
		drew = true
		return core.Version{Counter: 1}, nil
	}
	g, err := r.c.Take(context.Background(), req())
	if err != nil {
		t.Fatalf("Take: %v", err)
	}
	defer g.Release()
	if !drew {
		t.Fatal("Take admitted a write without drawing its version")
	}
	if g.Version().Counter == 0 {
		t.Fatal("the grant carries no version")
	}
}

// TestReserve_ZeroGapFillBesideAReservationIsRefused checks the held-entry
// gate's narrowness: a fill that adds no entry is admitted at an exactly-full
// index
// (TestTake_ZeroGapFillAtFullIndex), but beside an open reservation holding
// entries it is refused naming the index. The refusal is deliberate — the fill
// cannot prove it will not grow a piece its version's arrival splits — and
// fails closed: it is the one write that adds nothing yet is refused while the
// figure holding entries is non-zero.
func TestReserve_ZeroGapFillBesideAReservationIsRefused(t *testing.T) {
	// The bound leaves room for the reservation's pieces beside the one piece
	// the fill covers whole, so the refusal is the held entries' alone.
	pieces, _ := PiecesFor(2*WriteCharge(1, testCap), 2, testCap)
	o := std()
	o.indexBound = int64(pieces) + 2
	r := newRig(t, o)
	id := core.NewFileID([16]byte{1, 0xf})
	p := index.Placed{Extent: core.Extent{Len: 4096}, Record: index.RecordAt{Segment: 1}, Version: core.Version{Counter: 1}}
	if _, err := r.idx.WriteRetain(tag1, id, p, 1); err != nil {
		t.Fatalf("WriteRetain: %v", err)
	}
	rs := r.reserve(t, ReserveRequest{Tag: tag1, Bytes: 2 * WriteCharge(1, testCap), Writes: 2})
	defer rs.Release()

	q := req()
	q.File, q.Fill = id, true
	_, err := r.c.Take(context.Background(), q)
	var ns *core.NoSpaceError
	if !errors.As(err, &ns) || ns.Limit != core.LimitIndex {
		t.Fatalf("a zero-gap fill beside the reservation = %v, want a NoSpaceError naming the index", err)
	}
}

// TestTake_HoldsPendingEntries pins that an admitted plain take holds the
// entries its pieces will publish as pending, so a reservation taken before it
// publishes reads them spent: the datapath tests pin the effect, this pins the
// counter move at its source.
func TestTake_HoldsPendingEntries(t *testing.T) {
	r := newRig(t, std())
	q := req() // a payload the room covers: one piece, one entry
	g := r.grant(t, q)
	if got := r.c.pendingEntries; got != 1 {
		t.Fatalf("after the take, pendingEntries = %d, want the one entry its piece publishes", got)
	}
	// Every piece the index admits hands one entry over.
	g.Claimed(1)
	if got := r.c.pendingEntries; got != 0 {
		t.Fatalf("after the publish claimed the piece, pendingEntries = %d, want 0", got)
	}
	g.Release()
	if got := r.c.pendingEntries; got != 0 {
		t.Fatalf("after release, pendingEntries = %d, want 0", got)
	}
}

// TestTake_HoldsEveryRollingPiecesEntry pins the multi-piece half: a write whose
// split the room only partly covers publishes one entry per piece, and Take
// holds them all as pending, so a reservation taken while it is in flight
// cannot count on any of them.
func TestTake_HoldsEveryRollingPiecesEntry(t *testing.T) {
	r := newRig(t, std())
	// A partly covered write: its head in the room, its tail in the next
	// segment, so two pieces where the pre-check names one.
	room := r.room()
	if room <= 0 || room >= 2*segment.EmptyRoom(testCap) {
		t.Fatalf("precondition: the room is %d, want a partly filled segment", room)
	}
	q := req()
	q.Payload = room + 4096 // head fills the room, tail rolls
	q.Extent = core.Extent{Len: q.Payload}
	g := r.grant(t, q)
	if got := r.c.pendingEntries; got < 2 {
		t.Fatalf("the partly covered take holds %d pending entries, want one per piece (at least 2)", got)
	}
	g.Release()
	if got := r.c.pendingEntries; got != 0 {
		t.Fatalf("after release, pendingEntries = %d, want 0", got)
	}
}
