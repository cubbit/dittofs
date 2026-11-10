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
	"github.com/cubbit/dittofs/pkg/block/journal/internal/segment"
	"github.com/cubbit/dittofs/pkg/block/journal/journaltest/kit"
)

// The fixed figures of every rig: a cap small enough that a real pool makes
// its spares quickly in a temporary directory, a room most requests fit, and
// the maximums that mean "none".
const (
	testCap  = int64(64 << 10)
	roomBig  = int64(8 << 20)
	maxNone  = int64(1) << 62
	limitBig = uint64(1) << 40
)

// tag1 is the share every test request writes as.
const tag1 = core.ShareTag(1)

// fake is the injected facts of a Capacity: the plain funcs a rig's Config is
// built from, each with the counts a test watches. Its zero value answers
// every check as "no limit": writable, versions left, no index bound.
type fake struct {
	admitErr atomic.Value // error, nil until a test stores one
	holdErr  atomic.Value // *holdOutcome, nil until a test stores one
	left     atomic.Uint64
	checks   atomic.Int64
	check    func(core.FileID, core.Extent) error

	mu      sync.Mutex
	pending map[core.ShareTag]int64
	retired map[core.ShareTag]int64

	ready    atomic.Int64
	holdGate atomic.Bool
	holds    atomic.Int64
	room     atomic.Int64

	footprint atomic.Int64
}

func (f *fake) admit() error {
	if v, ok := f.admitErr.Load().(error); ok {
		return v
	}
	return nil
}

func (f *fake) checkIndex(id core.FileID, e core.Extent) error {
	f.checks.Add(1)
	if f.check != nil {
		return f.check(id, e)
	}
	return nil
}

func (f *fake) versionsLeft() uint64 { return f.left.Load() }

func (f *fake) used() int64 { return f.footprint.Load() }

func (f *fake) shareBytes(tag core.ShareTag) int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.pending[tag]
}

// shareFigures reads the pair the way the set's own accessor does: the
// settled bytes beside the cumulative retired bytes, under the fake's one
// lock, so a seed or a Retired call that reads it sees every modelled
// retirement exactly once.
func (f *fake) shareFigures(tag core.ShareTag) (settled, retired int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.pending[tag], f.retired[tag]
}

// setRetired sets the tag's cumulative retired record bytes — the figure
// a modelled retirement leaves behind.
func (f *fake) setRetired(tag core.ShareTag, n int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.retired == nil {
		f.retired = map[core.ShareTag]int64{}
	}
	f.retired[tag] = n
}

func (f *fake) setShare(tag core.ShareTag, n int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.pending == nil {
		f.pending = map[core.ShareTag]int64{}
	}
	f.pending[tag] = n
}

// incrShare raises the share's record bytes the way the stream's own ledger
// does when a record is written: the test calls it before Grant.Wrote, as
// the datapath's append does.
func (f *fake) incrShare(tag core.ShareTag, n int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.pending == nil {
		f.pending = map[core.ShareTag]int64{}
	}
	f.pending[tag] += n
}

func (f *fake) readySpares() int { return int(f.ready.Load()) }

// roomOf is the rig's Room answer: the payload room the stream names,
// defaulted generous so most tests' payloads sit whole in one record.
func (f *fake) roomOf(int) int64 { return f.room.Load() }

// holdOutcome is what the rig's hold answers with when the fake overrides
// the pool: an error stands in for the pool's own answer, nil grants. A
// park set makes the first hold wait on it before answering with the
// error — the overlap window a real pool's I/O opens, deterministically —
// and started carries one signal when that hold arrives.
type holdOutcome struct {
	err     error
	park    chan struct{}
	started chan struct{}
	once    sync.Once
}

// setHoldErr makes the rig's hold answer with err; a real nil grants
// through the pool again.
func (f *fake) setHoldErr(err error) {
	f.holdErr.Store(&holdOutcome{err: err})
}

// setHoldPark makes the rig's hold answer with err, the first caller kept
// waiting on the returned park channel before its answer, so a take's
// reservation stays held across other takes' sections exactly as it does
// while a real hold reaches the pool's storage.
func (f *fake) setHoldPark(err error) (park chan struct{}, started chan struct{}) {
	park = make(chan struct{})
	started = make(chan struct{}, 1)
	f.holdErr.Store(&holdOutcome{err: err, park: park, started: started})
	return park, started
}

// rig is a Capacity over a fake's facts and a real segment pool, whose Hold
// the capacity calls through; the pool readies three content spares and
// closes when the test ends.
type rig struct {
	f    *fake
	c    *Capacity
	pool *segment.Pool
	set  *segment.Set

	// holdSpy, when set, runs inside the hold func with the capacity mutex
	// released, observing each call the capacity makes.
	holdSpy func(n int)
}

// newRig builds a Capacity over fake facts and a real pool with three
// content spares, and adjusts the Config through cfg before New runs. The
// fake's versions counter starts generous and its ready count mirrors the
// pool's, so a test overrides either only where it means to.
func newRig(t *testing.T, cfg func(*Config)) *rig {
	return newRigMax(t, maxNone, cfg)
}

// newRigMax is newRig with the Maker's own journal maximum set: the set
// refuses a spare its reservation would cross that maximum, the same wall
// an open journal's makes meet.
func newRigMax(t *testing.T, makerMax int64, cfg func(*Config)) *rig {
	t.Helper()

	seam, err := kit.NewSeam(t.TempDir())
	if err != nil {
		t.Fatalf("NewSeam: %v", err)
	}
	set := segment.NewSet(testCap, nil)
	maker := segment.NewMaker(seam.FS(), set, makerMax, 0, nil)
	pool := segment.NewPool(maker, segment.Targets{Content: 3}, nil, nil)
	pool.Start()
	t.Cleanup(pool.Stop)

	// The pool's three content spares are real makes, so wait for them.
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(time.Millisecond) {
		if c := pool.Counters(); c.Spares >= 3 {
			break
		}
	}
	if c := pool.Counters(); c.Spares < 3 {
		t.Fatalf("the pool never readied its spares: %+v", c)
	}

	f := &fake{}
	f.left.Store(limitBig)
	f.room.Store(roomBig)
	f.ready.Store(int64(pool.ReadyContent()))
	f.holdGate.Store(true)
	r := &rig{f: f, pool: pool, set: set}
	c := Config{
		Admit:        f.admit,
		CheckIndex:   f.checkIndex,
		VersionsLeft: f.versionsLeft,
		Footprint:    f.used,
		ShareFigures: f.shareFigures,
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
			return f.roomOf(stream)
		},
		ReadySpares: f.readySpares,
		Hold:        r.hold,
		MaxBytes:    maxNone,
		SegmentCap:  testCap,
	}
	if cfg != nil {
		cfg(&c)
	}
	r.c = New(c)
	return r
}

// hold is the capacity's Hold func. The fake's ready count is the pool's
// own answer here too — the rig's fake overrides how many content spares
// exist, not only what Take reads — so a hold over what the fake says is
// ready is refused by the pool's own refusal, exactly as the real pool
// refuses it, and a hold that fits is granted from the real pool's spares.
// A test that names the fake's ready for the journal's coverage figure
// alone clears holdGate, which lets the real pool answer the hold: its
// three spares exist whatever the journal's covered need reads. The fake's
// hold error, when one is set, stands in for the pool's answer before
// either path runs. The spy runs first, so it observes every call with
// the capacity mutex released no matter the outcome.
func (r *rig) hold(n int) (*segment.Hold, error) {
	r.f.holds.Add(1)
	if spy := r.holdSpy; spy != nil {
		spy(n)
	}
	if v, ok := r.f.holdErr.Load().(*holdOutcome); ok && v != nil {
		if v.park != nil {
			v.once.Do(func() {
				v.started <- struct{}{}
				<-v.park
			})
		}
		return nil, v.err
	}
	if r.f.holdGate.Load() {
		if ready := r.f.readySpares(); ready < n {
			return nil, &core.NoSpaceError{Limit: core.LimitSpare, Missing: int64(n - ready), Unit: core.UnitSegments}
		}
	}
	return r.pool.Hold(n)
}

// req is the request every test starts from: tag1, stream 1, 4096 record
// bytes that fit an 8 MiB room, no share limit.
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

// TestTake_OrderOfRefusals drives one request every limit refuses at once
// and checks the first still standing names itself: read-only, then version,
// index, share, journal, and the spare pool last. The closing case holds: a
// write over its share's limit with no spare ready names the share, not the
// pool.
func TestTake_OrderOfRefusals(t *testing.T) {
	// Read-only first: the sentinel itself, nothing reserved, no hold asked,
	// no other limit consulted. Every other limit refuses this request too,
	// so only the order can name the read-only state.
	r := newRig(t, nil)
	r.f.admitErr.Store(core.ErrJournalReadOnly)
	r.f.left.Store(0)
	r.f.setShare(tag1, 1)
	r.f.check = func(core.FileID, core.Extent) error {
		return &core.NoSpaceError{Limit: core.LimitIndex, Missing: 1, Unit: core.UnitEntries}
	}
	q := req()
	q.ShareLimit = 1
	_, err := r.c.Take(context.Background(), q)
	if !errors.Is(err, core.ErrJournalReadOnly) {
		t.Fatalf("a read-only journal refused with %v, want ErrJournalReadOnly", err)
	}
	if got := r.c.ReservedBytes(); got != 0 {
		t.Fatalf("a read-only refusal reserved %d bytes, want 0", got)
	}
	if got := r.f.checks.Load(); got != 0 {
		t.Fatalf("a read-only refusal ran the index pre-check %d times, want 0", got)
	}
	if got := r.f.holds.Load(); got != 0 {
		t.Fatalf("a read-only refusal asked for %d holds, want 0", got)
	}

	// Version before index, share, journal and spare.
	r = newRig(t, func(c *Config) { c.MaxBytes = 1 })
	r.f.left.Store(0)
	r.f.setShare(tag1, 1)
	r.f.footprint.Store(1)
	r.f.check = func(core.FileID, core.Extent) error {
		return &core.NoSpaceError{Limit: core.LimitIndex, Missing: 1, Unit: core.UnitEntries}
	}
	_ = r.refused(t, q, core.LimitVersion)

	// Index before share, journal and spare.
	r = newRig(t, func(c *Config) { c.MaxBytes = 1 })
	r.f.setShare(tag1, 1)
	r.f.footprint.Store(1)
	r.f.check = func(core.FileID, core.Extent) error {
		return &core.NoSpaceError{Limit: core.LimitIndex, Missing: 1, Unit: core.UnitEntries}
	}
	_ = r.refused(t, q, core.LimitIndex)

	// Share before journal and spare.
	r = newRig(t, func(c *Config) { c.MaxBytes = 1 })
	r.f.setShare(tag1, 1)
	r.f.footprint.Store(1)
	ns := r.refused(t, q, core.LimitShare)
	if ns.Unit != core.UnitBytes {
		t.Fatalf("the share refusal counts %s, want bytes", ns.Unit)
	}

	// Journal before spare: a write beyond its room, the footprint at the
	// maximum, and the pool refusing too, is refused by the maximum.
	r = newRig(t, func(c *Config) { c.MaxBytes = 1 })
	r.f.footprint.Store(1)
	r.f.ready.Store(0)
	big := req()
	big.Payload = 2 * testCap
	r.f.room.Store(testCap / 2)
	_ = r.refused(t, big, core.LimitJournal)

	// The spare pool last, and only when no other limit refuses: the same
	// write with no maximum, needing three spares with two ready.
	r = newRig(t, nil)
	r.f.ready.Store(2)
	big = req()
	big.Payload = 3*testCap + 1
	r.f.room.Store(testCap / 2)
	ns = r.refused(t, big, core.LimitSpare)
	if ns.Missing != 1 || ns.Unit != core.UnitSegments {
		t.Fatalf("the spare refusal = %d %s, want one segment", ns.Missing, ns.Unit)
	}

	// A write over its share's limit with no spare ready names the share:
	// the pool answers only when every other limit would admit the write.
	r = newRig(t, nil)
	r.f.ready.Store(0)
	r.f.setShare(tag1, 1)
	over := req()
	over.ShareLimit = 2
	_ = r.refused(t, over, core.LimitShare)
}

// TestTake_NamedRefusalFields checks each limit's refusal fields: the share
// and the journal name bytes; the index entries; the version versions; the
// spare pool segments. The journal case holds MaxBytes half a cap above the
// footprint and a write needing two spares with one ready, so the missing
// bytes are one cap.
func TestTake_NamedRefusalFields(t *testing.T) {
	// Share: the shortfall in bytes, taken against the ask the admission
	// draws for the payload — the record's own bytes and the one extra
	// record's charge — so the refused figure names the whole ask over
	// the limit.
	r := newRig(t, nil)
	r.f.setShare(tag1, 100)
	q := req() // payload 4096 at the rig's generous room: one record
	q.ShareLimit = 4100
	ns := r.refused(t, q, core.LimitShare)
	// The figure the share's limit sees carries the whole ask — the
	// record and the one extra record's charge — so the refusal names the
	// whole charge over the limit.
	if ns.Missing != 100+recordBytes(4096)+slackOverhead()-4100 || ns.Unit != core.UnitBytes {
		t.Fatalf("the share refusal = %d %s, want %d bytes", ns.Missing, ns.Unit, 100+recordBytes(4096)+slackOverhead()-4100)
	}

	// Journal: a write two caps and a byte beyond its room needs two
	// spares; one is ready and covers one, so the uncovered need is one
	// cap; the maximum sits half a cap above the footprint, so what the
	// need exceeds the maximum by is half a cap: the missing the refusal
	// names is the difference the formula draws — the footprint plus the
	// uncovered need, less the maximum.
	used := testCap
	r = newRig(t, func(c *Config) { c.MaxBytes = used + testCap/2 })
	r.f.footprint.Store(used)
	r.f.ready.Store(1)
	q = req()
	q.Payload = 2*testCap + 1
	r.f.room.Store(testCap / 2)
	ns = r.refused(t, q, core.LimitJournal)
	if ns.Missing != testCap/2 || ns.Unit != core.UnitBytes {
		t.Fatalf("the journal refusal = %d %s, want half a cap in bytes", ns.Missing, ns.Unit)
	}

	// Index: the injected check's refusal, forwarded untouched.
	r = newRig(t, nil)
	r.f.check = func(core.FileID, core.Extent) error {
		return &core.NoSpaceError{Limit: core.LimitIndex, Missing: 3, Unit: core.UnitEntries}
	}
	ns = r.refused(t, req(), core.LimitIndex)
	if ns.Missing != 3 || ns.Unit != core.UnitEntries {
		t.Fatalf("the index refusal = %d %s, want 3 entries", ns.Missing, ns.Unit)
	}

	// Version: one version.
	r = newRig(t, nil)
	r.f.left.Store(0)
	ns = r.refused(t, req(), core.LimitVersion)
	if ns.Missing != 1 || ns.Unit != core.UnitVersions {
		t.Fatalf("the version refusal = %d %s, want one version", ns.Missing, ns.Unit)
	}

	// Spare: the ready spares short of what the pieces need. One ready,
	// three pieces need spares, so two are missing.
	r = newRig(t, nil)
	r.f.ready.Store(1)
	q = req()
	q.Payload = 3*testCap + 1
	r.f.room.Store(testCap / 3)
	ns = r.refused(t, q, core.LimitSpare)
	if ns.Missing != 2 || ns.Unit != core.UnitSegments {
		t.Fatalf("the spare refusal = %d %s, want two segments", ns.Missing, ns.Unit)
	}
}

// TestTake_FitsRoomTakesNoSpare runs a thousand concurrent takes that all
// fit the stream's room, with no ready spare and the journal's maximum
// already reached, and checks every one is granted from the room and the
// pool is never asked: a write that needs no new segment is never refused
// by the maximum, which bounds only whole segments.
func TestTake_FitsRoomTakesNoSpare(t *testing.T) {
	// MaxBytes == UsedBytes: the footprint sits at the maximum, one cap,
	// and no spare is ready.
	r := newRig(t, func(c *Config) { c.MaxBytes = testCap })
	r.f.footprint.Store(testCap)
	r.f.ready.Store(0)

	var wg sync.WaitGroup
	var granted atomic.Int64
	for range 1000 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			g, err := r.c.Take(context.Background(), req())
			if err != nil {
				t.Errorf("a take that fits the room at the maximum: %v", err)
				return
			}
			granted.Add(1)
			g.Release()
		}()
	}
	wg.Wait()
	if got := granted.Load(); got != 1000 {
		t.Fatalf("%d of 1000 takes that fit the room were granted, want all", got)
	}
	if got := r.f.holds.Load(); got != 0 {
		t.Fatalf("the pool was asked for %d holds, want 0", got)
	}
}

// TestTake_HoldsSparesBeyondRoom drives a write two and a half caps beyond
// its room and checks Take asks the pool for the spares the bytes need —
// three — never zero, and after the critical section ended: the spy asserts
// TryLock on the capacity mutex succeeds while the hold runs.
func TestTake_HoldsSparesBeyondRoom(t *testing.T) {
	r := newRig(t, nil)
	r.f.ready.Store(2) // two of the three the write needs

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
	q.Payload = testCap/2 + testCap*5/2
	r.f.room.Store(testCap / 2)
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
// and the same bytes are granted once the pool can hold them.
func TestTake_RefusedHoldGivesBytesBack(t *testing.T) {
	r := newRig(t, nil)
	r.f.ready.Store(0)
	r.f.setHoldErr(&core.NoSpaceError{Limit: core.LimitSpare, Missing: 2, Unit: core.UnitSegments})
	q := req()
	q.Payload = 2 * testCap
	r.f.room.Store(testCap / 2)
	before := r.c.RefusedCount()
	_ = r.refused(t, q, core.LimitSpare)
	if got := r.c.RefusedCount(); got != before+1 {
		t.Fatalf("RefusedCount went %d -> %d, want +1", before, got)
	}
	if got := r.c.ReservedBytes(); got != 0 {
		t.Fatalf("after the refused hold, ReservedBytes = %d, want 0", got)
	}

	// The pool can hold the need again: the same bytes are granted.
	r.f.setHoldErr(nil)
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
	e := emptyRoom(testCap)
	q := req()
	q.Payload = 2*e + 50
	q.Extent.Len = q.Payload
	r := newRig(t, nil)
	r.f.room.Store(0) // a fresh stream: every piece rolls
	r.f.ready.Store(0)

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
	r := newRig(t, nil)
	r.f.setShare(tag1, 1000)
	q := req()
	// The whole ask: the record a 2792-byte payload costs plus the one
	// extra record's charge — 2896 + 104 = 3000 — against the limit that
	// holds it and the 1000-byte seed exactly.
	q.Payload = 2792
	q.ShareLimit = 1000 + 3000 + 7
	r.grant(t, q).Release()
	q.Payload = 2793 // one payload byte more: the whole ask crosses to 3008
	ns := r.refused(t, q, core.LimitShare)
	if ns.Missing != 1 {
		t.Fatalf("the share's refusal over the exact ask = %d missing, want 1", ns.Missing)
	}

	// Journal: the maximum bounds whole segments, so the one-byte-over edge
	// runs at the segment's granularity — a write on a stream holding no
	// room whose pieces need exactly one spare is granted when the
	// maximum's remaining headroom covers its cap, and refused with one
	// byte missing when the maximum is one byte short of it.
	// One byte over: the same ask — one cap-plus-a-record write against a
	// near-full active segment — at a maximum one byte short of the cap
	// its one uncovered spare needs is refused with that one byte missing.
	r = newRig(t, func(c *Config) { c.MaxBytes = testCap })
	r.f.footprint.Store(0)
	r.f.ready.Store(1)
	q = req()
	q.Payload = emptyRoom(testCap) + 1
	r.f.room.Store(testCap - 300) // nearly full: the tail rolls to one spare
	r.grant(t, q).Release()

	r2 := newRig(t, func(c *Config) { c.MaxBytes = testCap - 1 })
	r2.f.footprint.Store(0)
	r2.f.ready.Store(0)
	q = req()
	q.Payload = emptyRoom(testCap) + 1
	r2.f.room.Store(testCap - 300)
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
	// Sixteen goroutines, four slots: the limit is four whole asks — an
	// ask being the record a one-byte payload costs plus the extra
	// record's charge — and the grants are held until all sixteen have
	// answered: the decision section decides each take whole, so exactly
	// four pass and twelve are refused, never five.
	r := newRig(t, nil)
	q := req()
	q.Payload = 1
	q.ShareLimit = 4 * (recordBytes(1) + slackOverhead())
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
	r = newRig(t, nil)
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
	q.ShareLimit = recordBytes(1) + slackOverhead() // room for exactly one ask
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
	r := newRig(t, nil)
	r.f.setShare(tag1, 1)
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
	if got := r.f.holds.Load(); got != 0 {
		t.Fatalf("the refusal asked for %d holds, want 0", got)
	}
	if got := r.f.checks.Load(); got != 1 {
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
	r := newRig(t, func(c *Config) { c.Tracer = tracer })

	// One refusal: one count, one ended span with the refusal's error.
	r.f.setShare(tag1, 1)
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
	r.f.setHoldErr(enospc)
	q = req()
	q.Payload = 2 * testCap
	r.f.room.Store(testCap / 2)
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
// unused spares, closes the hold, and is idempotent. The caller counts the
// record in the share before it calls Wrote, as the append path does, so
// inside Wrote's section the share's bytes hold what the piece wrote
// while the reservation has just fallen by the same amount: the hook
// reads both figures at that moment and the test asserts them together.
func TestGrant_WroteAndRelease(t *testing.T) {
	r := newRig(t, nil)
	r.f.ready.Store(2) // two spares for a write that needs three

	type move struct {
		n, reserved, share int64
	}
	var mu sync.Mutex
	var moves []move
	r.c.wroteHook = func(stream int, n, reserved int64) {
		mu.Lock()
		defer mu.Unlock()
		moves = append(moves, move{n: n, reserved: reserved, share: r.f.shareBytes(tag1)})
	}

	q := req()
	q.Payload = 3 * (testCap / 2)
	r.f.room.Store(1)
	g := r.grant(t, q)
	asked := r.c.ReservedBytes()
	if asked <= 3*(testCap/2) {
		t.Fatalf("the ask %d never rose over the payload by its pieces' overhead", asked)
	}

	// Two of three pieces are written: the stream's ledger raises the
	// share's bytes before the grant is told, as the datapath's append
	// does, and Wrote lowers the reservation by the piece.
	piece := testCap / 2
	r.f.incrShare(tag1, piece)
	g.Wrote(piece)
	r.f.incrShare(tag1, piece)
	g.Wrote(piece)
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
	if before, after := r.f.shareBytes(tag1), int64(2*piece); before != after {
		t.Fatalf("the share counts %d after two written pieces, want %d", before, after)
	}

	// Release returns the third piece's bytes and the unused spares.
	g.Release()
	if got := r.c.ReservedBytes(); got != 0 {
		t.Fatalf("after Release, ReservedBytes = %d, want 0", got)
	}
	if got := r.f.shareBytes(tag1); got != 2*piece {
		t.Fatalf("after Release the share counts %d, want only the two written pieces", got)
	}
	if got := r.f.holds.Load(); got != 1 {
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
	r := newRig(t, nil)
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
	// The footprint is a fact the fake holds constant, so only the count
	// can bound concurrent admissions: used sits at one cap, the maximum
	// holds exactly one more, and every hold is refused by the stub with
	// the pool's own spare refusal, so nothing but the count can mark how
	// many takes have claimed the room while their holds are still pending.
	used := testCap
	r := newRig(t, func(c *Config) { c.MaxBytes = 2 * testCap })
	r.f.footprint.Store(used)
	r.f.ready.Store(1)
	park, started := r.f.setHoldPark(&core.NoSpaceError{Limit: core.LimitSpare, Missing: 2, Unit: core.UnitSegments})

	q := req()
	q.Payload = 3 * (testCap / 2)
	r.f.room.Store(1) // one byte of room: the bytes roll into two spares

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
	close(park)
	wg.Wait()
	// The stub refuses every hold, parking the first caller while the
	// others decide, so no take can be granted and the storm's observable
	// is which limit refused. Exactly one take — whichever reached the
	// section first — claims the maximum's one segment of room and parks in
	// its hold; the count holds its room against every other take, so all
	// fifteen are refused by the journal's maximum itself, never by the
	// stub.
	if got := granted.Load(); got != 0 {
		t.Fatalf("%d of 16 takes were granted against a hold that refuses every ask, want 0", got)
	}
	if got := byJournal.Load(); got != 15 {
		t.Fatalf("%d of the 15 not-admitted takes were refused by the journal's maximum, want all 15", got)
	}
	if got := r.c.ReservedBytes(); got != 0 {
		t.Fatalf("after the storm, ReservedBytes = %d, want 0", got)
	}

	// The stub's refusals returned every reservation, and the storm left
	// the maximum's room intact: a take the pool can serve is granted.
	r.f.setHoldErr(nil)
	r.f.holdGate.Store(false)
	g := r.grant(t, q)
	g.Release()
}

// TestTake_NegativeBytesRefused checks a negative Bytes is refused with
// core.ErrInvalidExtent and reserves nothing, before any limit is consulted.
func TestTake_NegativeBytesRefused(t *testing.T) {
	r := newRig(t, nil)
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
	r := newRig(t, nil)
	r.f.ready.Store(0)
	q := req()
	q.Payload = math.MaxInt64 - 1
	r.f.room.Store(1)
	ns := r.refused(t, q, 0)
	if ns.Limit != core.LimitJournal && ns.Limit != core.LimitSpare {
		t.Fatalf("a huge ask was refused by %s, want the journal's maximum or the pool's", ns.Limit)
	}
	if ns.Missing < 0 {
		t.Fatalf("the refusal names %d missing, a wrapped negative", ns.Missing)
	}
}

// TestTake_ShareFigureIsExactUnderLandings drives concurrent takes at a
// share whose limit leaves room for exactly one of them, where every
// earlier grant also completes its Wrote in flight — the landing that once
// escaped a snapshot pair — and checks the figure decides exactly: of two
// racing takes at a one-slot limit with landings interleaved, exactly one
// is granted, never one more than the limit holds, and never one less.
func TestTake_ShareFigureIsExactUnderLandings(t *testing.T) {
	// The limits: each take asks the record bytes a 2792-byte payload
	// costs beside the extra record's charge — 3000 whole — against a
	// limit holding exactly one ask over the 1000-byte seed, so one take
	// fits and the second finds none.
	r := newRig(t, nil)
	const seed = 1000
	r.f.setShare(tag1, seed)
	ask := recordSplit(2792, roomBig, testCap) + slackOverhead()
	if ask != 3000 {
		t.Fatalf("the whole ask the admission charges for the payload = %d, want 3000; the test's figures depend on it", ask)
	}

	// The deterministic landing: the victim take parks between the set's
	// read and its decision — the only seam a design that re-reads the
	// set per take has there — while the other writer completes a whole
	// record inside the park: the append that counts it in the set and
	// the Wrote that hands it over. The check that follows must see the
	// landed record whether the take's figure is owned or read.
	inPark := make(chan struct{})
	release := make(chan struct{})
	var parked atomic.Bool
	shareRead := func(tag core.ShareTag) (settled, retired int64) {
		v, dn := r.f.shareFigures(tag)
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

	const writers = 4
	start := make(chan struct{})
	var wg sync.WaitGroup
	var granted, refused atomic.Int64
	for i := range writers {
		wg.Add(1)
		go func(victim bool) {
			defer wg.Done()
			<-start
			q := req()
			q.Payload = 2792
			q.ShareLimit = seed + ask
			g, err := r.c.Take(context.Background(), q)
			if err != nil {
				if ns, ok := err.(*core.NoSpaceError); ok && ns.Limit == core.LimitShare {
					refused.Add(1)
					return
				}
				t.Errorf("Take = %v, want the share's limit", err)
				return
			}
			granted.Add(1)
			// The record lands right where the other takes may be
			// deciding: the sequence that escapes a read-then-decide.
			r.f.incrShare(tag1, recordBytes(2792))
			g.Wrote(recordBytes(2792))
			g.Release()
		}(i == 0)
	}
	// The victim goes first and parks; the others run to completion,
	// landing whole records inside its park; only then does the victim's
	// decision run.
	close(start)
	<-inPark
	deadline := time.Now().Add(5 * time.Second)
	for (granted.Load()+refused.Load()) < writers-1 && time.Now().Before(deadline) {
		runtime.Gosched()
	}
	if got := granted.Load(); got != 1 {
		close(release)
		wg.Wait()
		t.Fatalf("%d takes granted before the parked one decided, want the one ask the limit holds", got)
	}
	close(release)
	wg.Wait()
	if got := granted.Load() + refused.Load(); got != writers {
		t.Fatalf("%d grants + %d refusals, want all %d takes answered", granted.Load(), refused.Load(), writers)
	}
	if got := granted.Load(); got != 1 {
		t.Fatalf("%d takes granted at a one-slot limit, want exactly 1: the landing inside the parked take's window escaped the figure", got)
	}
}

// TestTake_InterleavedDrawnPieceNeverOversetsTheShare stages the
// same-stream interleaving that draws an extra piece: two takes read the
// same stream room — the second reads it before the first's pieces land,
// for the pair's sections run one after another and neither has appended
// in between — and the first writer's landing between the second take's
// section and its appends moves the cut, so the second writer's write
// appends one piece more than the split its take sized. Against a limit
// holding the two writes' record bytes exactly — the figure the old
// admission charged, the split without the extra record's overhead — both
// takes were admitted and the settled bytes overran the limit by the drawn
// piece's overhead. Charging each ask whole — the split plus the one extra
// record's charge — the admission refuses the second take and the settled
// figure never passes the limit.
//
// The figures, drawn in the record arithmetic the rig's own functions
// spell (rb(n) the record bytes of a payload of n): rb(4096) = 4200 and
// rb(61049) = 61160. Both takes read the room R = 65248 payload bytes the
// segment names, and each write fits it whole, so each take sizes its
// split at one record. Their whole asks under the whole-charge admission
// are 4200+104 = 4304 and 61160+104 = 61264; their record bytes without
// the extra charge — the old figure — are 4200 and 61160, whose sum
// 65360 is the limit. The old figure admitted both. The larger writer's
// landing then draws one piece more than its take sized — the smaller
// writer's 4200 landed in between — charging the share the split plus one
// record's overhead: 61264. Settled, the pair stood at 4200+61264 =
// 65464, 104 past the limit the old figure had promised to hold.
func TestTake_InterleavedDrawnPieceNeverOversetsTheShare(t *testing.T) {
	// The figures the staging depends on, checked the arithmetic stands:
	const (
		small = int64(4096)
		large = int64(61049)
	)
	room := emptyRoom(testCap) // 65248 payload bytes the segment names
	if room != 65248 || recordBytes(small) != 4200 || recordBytes(large) != 61160 {
		t.Fatalf("the fixture's figures moved: room %d, rb(%d) %d, rb(%d) %d; the test's arithmetic depends on them",
			room, small, recordBytes(small), large, recordBytes(large))
	}
	r := newRig(t, nil)
	r.f.room.Store(room)

	// The limit holds the two writes' record bytes exactly — the bound the
	// old figure, charging each split without the extra record's overhead,
	// promised: rb(4096) + rb(61049) = 65360.
	limit := recordBytes(small) + recordBytes(large)

	// The larger writer takes first: its section reads the fresh room and
	// its whole ask — the split plus the extra record's charge — fits.
	qBig := req()
	qBig.Payload = large
	qBig.ShareLimit = limit
	gBig, err := r.c.Take(context.Background(), qBig)
	if err != nil {
		t.Fatalf("the first writer's take: %v", err)
	}

	// The smaller writer's take reads the same room — the larger writer
	// has reserved but not appended, so the room its section reads still
	// holds the whole segment. Under the whole-charge ask its figure
	// overruns the limit the first writer's charge filled: the take is
	// refused naming the share. The old figure charged the split alone,
	// admitted this take, and its landing below overran the limit.
	qSmall := req()
	qSmall.Payload = small
	qSmall.ShareLimit = limit
	gSmall, terr := r.c.Take(context.Background(), qSmall)
	if terr != nil {
		// A refusal moves nothing, so the neighbours' landings below owe it
		// nothing either; the settled figure stands alone.
		var ns *core.NoSpaceError
		if !errors.As(terr, &ns) || ns.Limit != core.LimitShare {
			t.Fatalf("the interleaved second take = %v, want the share's refusal", terr)
		}
	}
	// If the admission granted the second take — the old design's figure,
	// charging each split without the extra record's overhead, admitted it
	// here — its landing settles beside the first writer's drawn piece,
	// and the settled-bytes assert below convicts the overrun.
	if terr == nil {
		r.f.incrShare(tag1, recordBytes(small))
		gSmall.Wrote(recordBytes(small))
		gSmall.Release()
	}

	// The first writer lands: the same-stream neighbour's landing moved its
	// cut, so its appends drew one piece more than its take sized — the
	// drawn piece's own overhead beside the split — and the settled figure
	// never passes the limit, for the charge its reservation held covered
	// the piece it drew.
	r.f.incrShare(tag1, recordBytes(large)+slackOverhead())
	gBig.Wrote(recordBytes(large) + slackOverhead())
	gBig.Release()

	if got := r.f.shareBytes(tag1); got > limit {
		t.Fatalf("the share's settled bytes %d passed the limit %d under same-stream interleaving that drew an extra piece", got, limit)
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
	r := newRig(t, nil)
	// The share's settled figure the set itself reports (as a recovered
	// open would): the seed every later take stands on.
	r.f.setShare(tag1, 1000)
	q := req()
	q.Payload = 1500
	q.ShareLimit = 4000
	first := recordBytes(1500)
	g := r.grant(t, q)
	g.Wrote(first)
	g.Release()
	// An unbounded ask the limit never refused: only its own Wrote moved
	// the tally, as every write's does.
	q2 := req()
	q2.Payload = 2000
	q2.ShareLimit = 0
	second := recordBytes(2000)
	g2 := r.grant(t, q2)
	r.f.incrShare(tag1, second)
	g2.Wrote(second)
	// The extra record's charge of the second ask returns to the
	// reservation when the record needed no spare piece.
	g2.Release()

	// The next bounded take sees the whole settled figure: it refuses past
	// the limit, never under it.
	q3 := req()
	q3.Payload = 1
	q3.ShareLimit = 4000
	third := recordBytes(1) + slackOverhead()
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
	r := newRig(t, nil)
	r.f.setShare(tag1, 1000)
	q := req()
	// One ask beside the seed: a payload whose record costs 2896 bytes
	// over the slack charges 3000 exactly against the 4000 limit.
	q.Payload = 2792
	q.ShareLimit = 4000
	g := r.grant(t, q)
	r.f.incrShare(tag1, recordBytes(2792))
	g.Wrote(recordBytes(2792))
	g.Release()

	// A retirement of the segment holding the record frees its bytes: the
	// set's own figure falls, its cumulative retired figure rises, the
	// caller tells the capacity, and the retry fits in the room it freed.
	r.f.setShare(tag1, 1000)
	r.f.setRetired(tag1, recordBytes(2792))
	r.c.Retired(tag1)
	g2 := r.grant(t, q)
	g2.Release()
}

// TestTake_RoomIsNotGrantedTwice drives two takes on one stream, each
// asking the whole room the stream names, and checks the ledger of
// granted-but-unwritten room convicts the second: the first grant's
// roomPart stands against its own figures, so the second take's bytes
// roll into a spare the pool's stub refuses and it is refused, never two
// grants of one window.
func TestTake_RoomIsNotGrantedTwice(t *testing.T) {
	var room atomic.Int64
	room.Store(3 * (testCap / 2))
	r := newRig(t, func(c *Config) {
		c.Room = func(int) int64 { return room.Load() }
		c.ReadySpares = func() int { return 0 }
		c.Hold = func(n int) (*segment.Hold, error) {
			return nil, &core.NoSpaceError{Limit: core.LimitSpare, Missing: int64(n), Unit: core.UnitSegments}
		}
		c.MaxBytes = 0
	})
	q := req()
	// The payload whose whole ask sits exactly in the room the stream
	// names: its record on the record grid plus the extra record's
	// charge. The grid arithmetic is drawn from the same functions the
	// admission charges, so the ask equals the room to the byte.
	q.Payload = room.Load() - slackOverhead()
	for recordSplit(q.Payload, room.Load(), testCap)+slackOverhead() != room.Load() {
		q.Payload -= 1
	}
	ask := recordSplit(q.Payload, room.Load(), testCap) + slackOverhead()
	if ask != room.Load() {
		t.Fatalf("the fixture asks %d against a room of %d; the figure depends on it", ask, room.Load())
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

// TestTake_OneReadySpareRefusesNeedOfTwo pins the shape the datapath's
// own spare-refusal test left out: a write needing two spares of a pool
// holding one ready is refused naming the spare, with the pool's holds
// unchanged — the take runs before anything is drawn, so the refused
// hold leaves the holds it found.
func TestTake_OneReadySpareRefusesNeedOfTwo(t *testing.T) {
	r := newRig(t, nil)
	r.f.setShare(tag1, 0)
	r.f.ready.Store(1) // the one spare an unreserved take can reach
	q := req()
	q.Payload = 3 * (testCap / 2) // its bytes roll into two spares
	r.f.room.Store(0)
	before := r.pool.Counters().Held
	ns := r.refused(t, q, core.LimitSpare)
	if ns.Missing != 1 || ns.Unit != core.UnitSegments {
		t.Fatalf("the spare refusal = %d %s, want one segment missing", ns.Missing, ns.Unit)
	}
	if got := r.pool.Counters().Held; got != before {
		t.Fatalf("the refused take left the pool owing %d holds, want the %d it found", got, before)
	}
}
