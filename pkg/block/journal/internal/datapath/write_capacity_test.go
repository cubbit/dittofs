package datapath

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/capacity"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/format"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/index"
	"github.com/cubbit/dittofs/pkg/block/journal/journaltest/kit"
)

// errMakeSpares is the seam fault that gates the spare pool's refill in
// these tests: making a spare fails with it while it is scheduled.
var errMakeSpares = errors.New("spare making gated by the test")

// A write's WriteAt goes through one admission, capacity.Take, before it
// draws a version or writes a byte. These tests drive the limits over the
// real directory through the seam — the share's limit, the journal's
// maximum, the index's bound, the stream's room and the pool's spares — and
// the refusals that name them.
//
// The record arithmetic is drawn the same way the stream draws its own fit:
// a write record of n payload bytes costs
//
//	format.RecordHeader{Kind: KindWrite, Extent: core.Extent{Len: n}}.Size()
//
// — header, fixed body bytes, payload and padding, and the payload bytes a
// segment can still take after recordsBytes of record bytes is the cap less
// the 48-byte segment header and the records, less the seal's reserve for
// one more record — marker, footer entries, trailer — floored to the 8-byte
// record grid, less one more record's own header and fixed body bytes.

// roomAfter returns the payload Room() of a stream whose active segment
// already holds recordsBytes of record bytes, at the test's segment cap. It
// is the same closed form Stream.Room draws over its own state, reduced to
// the two numbers a test controls: the segment cap and the record bytes.
func roomAfter(recordsBytes int64) int64 {
	room := ((testSegmentCap - format.SegmentHeaderSize - recordsBytes - sealOver(1)) &^ 7) -
		(int64(format.RecordHeaderSize) + format.RecordHeader{Kind: format.KindWrite}.BodySize())
	return max(room, 0)
}

// capacityCoreOption sets the journal's maximum the core builds its
// admission over, or leaves the maximum unbounded when zero — the behaviour
// of a core opened before any maximum was wired, which existing tests
// everywhere keep.
func capacityCoreOption(max int64) func(*coreOptions) {
	return func(o *coreOptions) { o.maxBytes = max }
}

// mustLimitShare attaches a handle for tag at the record-byte limit, or
// fails the test when the core refuses it.
func mustLimitShare(t *testing.T, c *Core, tag core.ShareTag, limit int64) *Share {
	t.Helper()
	s, err := c.Share(tag, limit, core.Version{})
	if err != nil {
		t.Fatalf("Share(%d, %d): %v", tag, limit, err)
	}
	return s
}

// refused checks err carries a NoSpaceError naming want and returns it, for
// the callers that go on asserting its fields; a caller that checks the
// naming alone discards the return explicitly, for the helper fails the
// test on a wrong refusal either way.
func refused(t *testing.T, err error, want core.Limit) *core.NoSpaceError {
	t.Helper()
	var ns *core.NoSpaceError
	if !errors.As(err, &ns) || !errors.Is(err, core.ErrNoSpace) || ns.Limit != want {
		t.Fatalf("the refusal = %v, want a NoSpaceError naming %s", err, want)
	}
	return ns
}

// awaitSettledSpares waits until the pool holds exactly n ready content
// spares, so a census taken after it sees the pool settled.
func awaitSettledSpares(t *testing.T, c *Core, n int64) {
	t.Helper()
	waitIn(t, 5*time.Second, "the pool to settle at "+fmt.Sprint(n)+" ready spares", func() bool {
		return c.pool.Counters().Spares == n
	})
}

// warmedBytes is the record bytes a warm-up write of one byte lands: the
// price warm() pays per file, which tests reading a share's footprint
// subtract so the limit answers for the writes under test alone.
func warmedBytes() int64 {
	return recordBytes(1)
}

// waitIn runs cond until it holds or d passes, failing the test on the
// timeout.
func waitIn(t *testing.T, d time.Duration, what string, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(d); !cond(); time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
	}
}

// headroomFixture is the headroom a content-throttle core reserves, in
// records without bytes: eight, whose open reserve is half — four — so its
// drawing side is four records and half falls at two drawn. Three
// admissions past two put it below half, refusing every content write
// while the journal holds its limit — the throttle keeping the half the
// records that return space draw on — and admitting them again once a
// retirement takes the journal off the limit, which the three tests below
// drive to its edges.
const headroomFixture = 8

// drawHeadroom drives n release admissions through the core's headroom and
// hands each to Drawn, as the removal paths that append the records will:
// the journal's limit-gated drawings move the figure the half rule reads,
// Drawn the counter each appended record raises.
func drawHeadroom(t *testing.T, c *Core, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		d, err := c.headroom.Admit(capacity.Release)
		if err != nil {
			t.Fatalf("headroom admission %d: %v", i+1, err)
		}
		if !d.Append {
			t.Fatalf("headroom admission %d was told not to append", i+1)
		}
		c.headroom.Drawn(d)
	}
}

// seamCensus counts the storage-side calls a write would have to make on
// its own behalf — creating, allocating, opening, renaming and syncing a
// directory — and leaves the record write and the file sync out: the
// durability layer's background windows issue those on their own schedule,
// and a refusal's census must not see the noise a refused write never
// made.
func seamCensus(seam *kit.Seam) int {
	return seam.Calls(kit.OpCreate) + seam.Calls(kit.OpAllocate) +
		seam.Calls(kit.OpOpen) + seam.Calls(kit.OpRename) +
		seam.Calls(kit.OpTruncate) + seam.Calls(kit.OpRemove) +
		seam.Calls(kit.OpList) + seam.Calls(kit.OpSyncDir)
}

// TestRFC1B_Reservation drives sixteen writers at a share limited for
// exactly eight of their writes and checks exactly eight pass, the rest are
// refused naming the share's limit, and the share's footprint — its record
// bytes on disk plus what open reservations hold — never passes the limit.
func TestRFC1B_Reservation(t *testing.T) {
	c, _ := newTestCore(t)
	const each = 4096
	files := make([]core.FileID, 16)
	for i := range files {
		files[i] = testFile(byte(1 + i))
	}
	// The two test streams hold active segments once warmed, so the eight
	// writes the limit admits never reach for a spare and the share's
	// limit alone decides. The warm-ups' bytes count against the limit, so
	// it is raised by their price.
	share, err := c.Share(1, 0, core.Version{})
	if err != nil {
		t.Fatalf("Share: %v", err)
	}
	for i := range files {
		if _, err := c.WriteAt(context.Background(), share, files[i], 1<<20, []byte("w"), 1); err != nil {
			t.Fatalf("warm-up WriteAt: %v", err)
		}
	}
	waitIn(t, 5*time.Second, "the pool to settle", func() bool {
		return c.pool.Counters().Spares == 2*testStreams
	})
	baseline := int64(len(files)) * warmedBytes()
	// The limit holds exactly eight of the writers' records beside the
	// warm-ups' own, so of the sixteen concurrent writers exactly eight
	// pass and eight are refused naming the share. (rb(4096)=4200, so
	// eight are 33600; each warm-up's one-byte record costs rb(1)=112,
	// sixteen of them 1792 beside.)
	limit := 8*(recordBytes(each)) + baseline
	s, err := c.Share(1, limit, core.Version{})
	if err != nil {
		t.Fatalf("Share(1, %d): %v", limit, err)
	}
	var passed, refusedN atomic.Int64
	var wg sync.WaitGroup
	// The poller watches the settled footprint the way the RFC's stats row
	// reads it: a record mid-charge sits in the share's bytes before the
	// reservation hands it over — the one in-flight operation §3.7's
	// inequalities allow — so the settled read is taken with no write past
	// its admission: once the writers answer, the footprint must sit at or
	// under the limit.
	stop := make(chan struct{})
	go func() {
		for {
			select {
			case <-stop:
				return
			default:
			}
			if r := c.ReservedBytes(); r < 0 {
				t.Errorf("ReservedBytes = %d, want it never negative", r)
			}
		}
	}()
	for i := range files {
		wg.Add(1)
		go func(f core.FileID) {
			defer wg.Done()
			_, err := c.WriteAt(context.Background(), s, f, 1<<20, make([]byte, each), 1)
			if err == nil {
				passed.Add(1)
				return
			}
			var ns *core.NoSpaceError
			if errors.As(err, &ns) && ns.Limit == core.LimitShare && errors.Is(err, core.ErrNoSpace) {
				refusedN.Add(1)
				return
			}
			t.Errorf("WriteAt(%v) = %v, want ErrNoSpace naming the share's limit", f, err)
		}(files[i])
	}
	wg.Wait()
	close(stop)
	// The admission owns the share's figure: every counter it decides on
	// moves under its one mutex, so sixteen writers at a share holding
	// exactly eight asks settle exactly eight passes — one per ask, in
	// the order their sections took the mutex — and eight refusals
	// naming the share. The share's settled bytes never pass the limit.
	if got := passed.Load(); got != 8 {
		t.Fatalf("%d writes passed, want exactly 8", got)
	}
	if got := passed.Load() + refusedN.Load(); got != 16 {
		t.Fatalf("%d passed + %d refused, want every writer answered 16", passed.Load(), refusedN.Load())
	}
	// The settled footprint never passes the limit: every record the
	// writers landed is the share's and every reservation returned.
	if got := c.set.ShareBytes(1); got > limit {
		t.Fatalf("the share's settled bytes %d passed the limit %d", got, limit)
	}
}

// TestRFC1B_NamedRefusal checks a share-limit refusal answers at once: it
// names the share's limit and the bytes missing, draws no version, holds
// the file's append domain nowhere the next writer can see and leaves no
// reservation or spare taken.
func TestRFC1B_NamedRefusal(t *testing.T) {
	c, seam := newTestCore(t)
	const each = 4096
	ask := recordBytes(each)
	s := mustLimitShare(t, c, 1, ask)
	ctx := context.Background()
	if _, err := c.WriteAt(ctx, s, testFile(1), 0, make([]byte, each), 1); err != nil {
		t.Fatalf("the first write: %v", err)
	}
	awaitSettledSpares(t, c, 2*testStreams)

	beforeV := c.counters.NextVersion()
	beforeWrites := seamCensus(seam)

	ns := refused(t, func() error {
		_, err := c.WriteAt(ctx, s, testFile(2), 0, make([]byte, 2*each), 1)
		return err
	}(), core.LimitShare)
	// The share's settled bytes sit at the limit, so what the refused
	// write misses is its record less the room the limit has left.
	want := c.set.ShareBytes(1) + recordBytes(2*each) - (recordBytes(each))
	if ns.Missing != want {
		t.Fatalf("Missing = %d, want %d", ns.Missing, want)
	}
	afterC := seamCensus(seam)
	if afterC != beforeWrites {
		t.Fatalf("the refusal made %d seam calls, want none", afterC-beforeWrites)
	}
	if afterV := c.counters.NextVersion(); afterV != beforeV {
		t.Fatalf("the refusal drew a version: next %v was %v", afterV, beforeV)
	}
	if got := c.ReservedBytes(); got != 0 {
		t.Fatalf("ReservedBytes = %d after the refusal, want 0", got)
	}
	if got := c.ReservationRefused(); got != 1 {
		t.Fatalf("ReservationRefused = %d after the share's refusal, want 1", got)
	}
	if got := c.pool.Counters().Spares; got != 2*testStreams {
		t.Fatalf("the refusal took a spare: %d ready, want %d", got, 2*testStreams)
	}
}

// TestRFC1B_ShareLimit fills share A to its limit and checks its next write
// is refused naming the share while share B still writes, and that a
// simulated retirement of A's segment — releasing its record bytes in the
// ledger, as a retirement that unlinked it would — lets A's retry through.
func TestRFC1B_ShareLimit(t *testing.T) {
	c, _ := newTestCore(t)
	const each = 4096
	// A's first write is one byte over the whole room: the stream fills
	// the first segment, the roll seals it full, and the last byte lands
	// in the fresh active successor — so the first piece sits in a sealed,
	// non-active segment a retirement can take while the stream keeps
	// writing in its successor.
	a := mustLimitShare(t, c, 1, 0)
	b := mustShare(t, c, 2)
	fa, fb := testFile(1), testFile(2)
	ctx := context.Background()
	if _, err := c.WriteAt(ctx, a, fa, 0, make([]byte, roomAfter(0)+1), 1); err != nil {
		t.Fatalf("A's room-crossing write: %v", err)
	}
	// The share's limit is the settled bytes of that write, so every
	// later write of A's is refused until the segment retires. Share on an
	// attached tag replaces the handle's limit, so the same handle the
	// writes run through carries it.
	settled := recordBytes(roomAfter(0)) + recordBytes(1)
	if _, err := c.Share(1, settled, core.Version{}); err != nil {
		t.Fatalf("Share(1, %d): %v", settled, err)
	}
	if _, err := c.WriteAt(ctx, b, fb, 0, make([]byte, each), 1); err != nil {
		t.Fatalf("B's write: %v", err)
	}
	_ = refused(t, func() error {
		_, err := c.WriteAt(ctx, a, fa, 1<<20, make([]byte, each), 1)
		return err
	}(), core.LimitShare)
	// B still writes.
	if _, err := c.WriteAt(ctx, b, fb, each, make([]byte, each), 1); err != nil {
		t.Fatalf("B's second write: %v", err)
	}
	if got := c.set.ShareBytes(1); got != settled {
		t.Fatalf("A's settled bytes = %d, want %d", got, settled)
	}

	// A simulated retirement of A's first segment: the record bytes it
	// returns to the share are the filling piece's, as an unlink would
	// give them back, and the stream's active segment is its successor,
	// so A's retry has a segment to write in.
	p, ok := c.Index().Lookup(fa, 0)
	if !ok {
		t.Fatal("A's piece is not held")
	}
	seg := p.Record.Segment
	if _, err := c.set.Retire(seg); err != nil {
		t.Fatalf("retiring A's rolled-off segment: %v", err)
	}
	// The retirement frees the record bytes the segment held of the
	// share; the capacity's own figure stands until told — the
	// reconciliation a real retirement executor owes it, simulated here.
	c.Retired(1)
	if _, err := c.WriteAt(ctx, a, fa, 1<<20, make([]byte, each), 1); err != nil {
		t.Fatalf("A's retry after the retirement: %v", err)
	}
}

// TestEdge_ReservationExactFitShare checks a write of exactly the share's
// remaining record bytes passes and one byte more is refused, one byte
// missing.
func TestEdge_ReservationExactFitShare(t *testing.T) {
	c, _ := newTestCore(t)
	const first, second = 4096, 64
	// The limit holds the first write's record exactly plus one more
	// record: a write of exactly the remaining bytes passes, and one
	// payload byte more is refused by the record that extra byte crosses
	// to.
	ask := recordBytes(second)
	s := mustLimitShare(t, c, 1, recordBytes(first)+ask)
	ctx := context.Background()
	if _, err := c.WriteAt(ctx, s, testFile(1), 0, make([]byte, first), 1); err != nil {
		t.Fatalf("the first write: %v", err)
	}
	if _, err := c.WriteAt(ctx, s, testFile(2), 0, make([]byte, second), 1); err != nil {
		t.Fatalf("the exact remaining-ask write: %v", err)
	}
	ns := refused(t, func() error {
		_, err := c.WriteAt(ctx, s, testFile(3), 0, make([]byte, second+1), 1)
		return err
	}(), core.LimitShare)
	// No byte of the write fits: the one payload byte over crossed its
	// record — the 65 bytes cost — past the limit the settled bytes
	// already fill, so the refusal names that record.
	want := c.set.ShareBytes(1) + recordBytes(second+1) - (recordBytes(first) + recordBytes(second))
	if ns.Missing != want {
		t.Fatalf("Missing = %d, want %d: the whole ask's shortfall over the limit", ns.Missing, want)
	}
}

// journalSettled warms one file's stream with a write that crosses its
// segment's cap and waits for the pool to reach the exact state the
// journal's maximum max leaves it, so a later write on the OTHER stream —
// fresh, holding no active segment — finds the pool at that figure: the
// whole segment caps the maximum admits are all made or spent and nothing
// is held. The state is a target, not a window.
func journalSettled(t *testing.T, c *Core, s *Share, f core.FileID, max int64) {
	t.Helper()
	if _, err := c.WriteAt(context.Background(), s, f, 0, make([]byte, roomAfter(0)+1), 1); err != nil {
		t.Fatalf("the segment-crossing write: %v", err)
	}
	// The caps the ceiling admits, whole: the warm write's hold demands
	// spares above the pool's target, so the refill makes every whole cap
	// the ceiling holds — floor of the maximum over the cap — and the
	// warm write spends two of them (one it fills and seals, one its last
	// byte rolls onto). What stands ready is the rest, and nothing holds:
	// the warm's own grant returned.
	want := max/testSegmentCap - 2
	waitIn(t, 5*time.Second, "the pool to settle at the ceiling's whole spares", func() bool {
		pc := c.pool.Counters()
		return pc.Spares == want && pc.Held == 0
	})
}

// TestEdge_ReservationExactFitJournal checks the maximum's byte-granular
// edge at quiescence: the warm write above spends two spares (one it fills
// and seals, one its last byte rolls onto), so a 2.5-cap write on the
// journal's other stream needs three whole segments against the two the
// pool readies. At a maximum whose headroom covers the fifth cap exactly,
// the refill serves the third spare and the write passes; at a maximum one
// byte short, the refill is refused by the maker, the write's third segment
// is uncovered, and the refusal names the one byte the cap falls short of
// the headroom.
func TestEdge_ReservationExactFitJournal(t *testing.T) {
	// The exact fit: a fifth cap exactly. The maker serves the refill
	// (its AddSpare compares against the maximum with headroom for one
	// whole cap), the pool readies three, and the write's three segments
	// are covered whole.
	c, _ := newTestCore(t, capacityCoreOption(5*testSegmentCap))
	s := mustShare(t, c, 1)
	ctx := context.Background()
	warm := testFile(1)
	journalSettled(t, c, s, warm, 5*testSegmentCap)
	exact := testFile(2)
	if streamOf(c, exact) == streamOf(c, warm) {
		t.Fatal("the two files share a stream; the fresh-stream leg cannot run")
	}
	if _, err := c.WriteAt(ctx, s, exact, 0, make([]byte, testSegmentCap*5/2), 1); err != nil {
		t.Fatalf("the exact remaining-capacity write: %v", err)
	}

	// One byte more than the maximum: the maker refuses the refill at the
	// whole cap (creating a segment reserves its cap against the maximum),
	// the pool stays two, and the write is refused with the uncovered cap
	// less whichever byte of headroom the maximum still holds.
	c2, _ := newTestCore(t, capacityCoreOption(5*testSegmentCap-1))
	s2 := mustShare(t, c2, 1)
	warm2 := testFile(3)
	journalSettled(t, c2, s2, warm2, 5*testSegmentCap-1)
	ns := refused(t, func() error {
		_, err := c2.WriteAt(ctx, s2, testFile(4), 0, make([]byte, testSegmentCap*5/2), 1)
		return err
	}(), core.LimitJournal)
	if ns.Missing != 1 {
		t.Fatalf("Missing = %d, want 1: the one byte the maximum's headroom falls short of the cap the write's third segment needs", ns.Missing)
	}
}

// TestWriteAt_2p5CapsOneSpareRefusedNoIO checks a write of two and a half
// caps against a fresh stream — its room covers none of the write — whose
// three whole segments the pool's two ready spares fall one short of
// covering, with the maker's ceiling leaving the maximum no headroom for the
// uncovered one: it is refused naming the journal's maximum — ahead of the
// spare the hold would have taken — with no seam call and no version drawn.
func TestWriteAt_2p5CapsOneSpareRefusedNoIO(t *testing.T) {
	// Three caps at the maker's ceiling: the pool readies three spares and
	// the refill cannot make a fourth, so the footprint settles at three
	// caps with no headroom.
	c, seam := newTestCore(t, capacityCoreOption(3*testSegmentCap))
	s := mustShare(t, c, 1)
	warm := testFile(1)
	ctx := context.Background()
	journalSettled(t, c, s, warm, 3*testSegmentCap)

	beforeV := c.counters.NextVersion()
	beforeC := seamCensus(seam)

	// The warm write above spends two of the three spares the maker's
	// ceiling readies (one it fills and seals, one its last byte rolls
	// onto), so the pool holds one. 2.5 caps of payload on the OTHER
	// stream — fresh, holding no active segment — needs three whole
	// segments; the one ready spare covers one, two are uncovered, and
	// two caps against no headroom is what the maximum misses. The journal
	// refuses before the spare the hold would have taken is named.
	f := testFile(2)
	if streamOf(c, f) == streamOf(c, warm) {
		t.Fatal("the two files share a stream; the fresh-stream leg cannot run")
	}
	ns := refused(t, func() error {
		_, err := c.WriteAt(ctx, s, f, 0, make([]byte, testSegmentCap*5/2), 1)
		return err
	}(), core.LimitJournal)
	if ns.Missing != 2*testSegmentCap {
		t.Fatalf("Missing = %d, want two uncovered caps %d", ns.Missing, 2*testSegmentCap)
	}

	afterC := seamCensus(seam)
	if afterC != beforeC {
		t.Fatalf("the refusal made %d seam calls, want none", afterC-beforeC)
	}
	if afterV := c.counters.NextVersion(); afterV != beforeV {
		t.Fatalf("the refusal drew a version: next %v was %v", afterV, beforeV)
	}
	if got := c.ReservedBytes(); got != 0 {
		t.Fatalf("ReservedBytes = %d after the refusal, want 0", got)
	}
}

// TestWriteAt_ManySmallWritesUseRoomNotSpares drives sixty-four 4 KiB writes
// at one stream with a journal maximum at the settled footprint the writes
// roll themselves into, and checks every write passes on its segment's own
// room: no hold is taken, no spare is consumed beyond the rolls' own and the
// storage drawn never passes the ceiling — a reservation charging
// room-covered bytes against the maximum would fail the sixty-fourth write
// if not the fiftieth.
func TestWriteAt_ManySmallWritesUseRoomNotSpares(t *testing.T) {
	// The footprint the writes settle at: the pool's four caps and the
	// four its rolls spend, side by side — an 8-cap ceiling covers
	// exactly that and nothing more.
	c, _ := newTestCore(t, capacityCoreOption(8*testSegmentCap))
	s := mustShare(t, c, 1)
	f := testFile(1)
	ctx := context.Background()
	// Warm the stream: its active segment is the pool's first spare, and
	// the pool settles back at its target under the ceiling.
	if _, err := c.WriteAt(ctx, s, f, 0, make([]byte, 4096), 1); err != nil {
		t.Fatalf("the warm-up write: %v", err)
	}
	waitIn(t, 5*time.Second, "the pool to settle", func() bool {
		return c.pool.Counters().Spares == 2*testStreams
	})
	before := c.pool.Counters()

	for i := range 64 {
		if _, err := c.WriteAt(ctx, s, f, 4096+int64(i)*4096, make([]byte, 4096), 1); err != nil {
			t.Fatalf("write %d: %v", i+1, err)
		}
	}
	// The refill serves each roll the ceiling lets it make, then settles:
	// the sixty-five records (the warm-up's among them) roll the stream
	// through five segments — four of the ceiling's caps beside the warm
	// one — so of the eight caps the ceiling holds three stand ready,
	// and none holds, the writes having needed no spare of their own.
	waitIn(t, 5*time.Second, "the pool to settle at its last three spares", func() bool {
		pc := c.pool.Counters()
		return pc.Spares == 3 && pc.Held == before.Held
	})
	after := c.pool.Counters()
	// The rolls spent four of the eight the ceiling covered — three
	// ready stand beside the five live segments — and none of that was
	// held: the room alone carried every write. Storage never passed the
	// ceiling, so no room-covered byte was charged against it.
	if after.Held != before.Held {
		t.Fatalf("the writes took a hold: %d, want the %d they found", after.Held, before.Held)
	}
	if got := c.set.Counters().UsedBytes; got > 8*testSegmentCap {
		t.Fatalf("the writes drew %d bytes of storage, past the %d ceiling", got, 8*testSegmentCap)
	}
	if got := c.set.Counters().UsedBytes; got < 7*testSegmentCap {
		t.Fatalf("the writes drew only %d bytes of storage, want the rolls to have spent spares", got)
	}
	if got := c.ReservedBytes(); got != 0 {
		t.Fatalf("ReservedBytes = %d after the last write returned, want 0", got)
	}
}

// TestWriteAt_JournalMaximumTwoWrites puts the journal at its maximum with
// half a cap of room and one spare ready and checks two 1.5-cap writes to
// two files on two streams: the first is written whole, reads back whole and
// holds no bytes back; the second is refused naming the journal, the whole
// segments its pieces need that no ready spare covers as its Missing, with
// nothing published of it, no version drawn and no seam call.
func TestWriteAt_JournalMaximumTwoWrites(t *testing.T) {
	t.Run("two streams", func(t *testing.T) {
		testJournalMaximumTwoWrites(t, false)
	})
	t.Run("one stream", func(t *testing.T) {
		testJournalMaximumTwoWrites(t, true)
	})
}

func testJournalMaximumTwoWrites(t *testing.T, oneStream bool) {
	// Three caps at the maker's ceiling: the pool readies three spares
	// and no fourth can be made. The first 1.5-cap write takes two of them
	// (the segment it fills and the one its last piece rolls onto), so the
	// pool settles at one; the footprint sits at the maximum with no
	// headroom. The second 1.5-cap write needs two whole segments, the
	// one ready spare covers one, and the uncovered one is refused by the
	// maximum before the spare the hold would run for is named.
	c, _ := newTestCore(t, capacityCoreOption(3*testSegmentCap))
	s := mustShare(t, c, 1)
	// Odd file ids hash to one stream and even ones to the other, so the
	// two-stream leg takes one of each and the one-stream leg two odd.
	var f1, f2 core.FileID
	if oneStream {
		f1, f2 = testFile(1), testFile(3)
	} else {
		f1, f2 = testFile(1), testFile(2)
	}
	if oneStream && streamOf(c, f1) != streamOf(c, f2) {
		t.Fatal("the two files do not share a stream; the one-stream case cannot run")
	}
	ctx := context.Background()

	if _, err := c.WriteAt(ctx, s, f1, 0, make([]byte, testSegmentCap*3/2), 1); err != nil {
		t.Fatalf("the first write: %v", err)
	}
	// The pool's refill is refused at the fourth cap (the maker reserves
	// it against the maximum), so the ready figure settles at one.
	waitIn(t, 5*time.Second, "the pool to settle at its one spare", func() bool {
		return c.pool.Counters().Spares == 1
	})

	beforeV := c.counters.NextVersion()
	ns := refused(t, func() error {
		_, err := c.WriteAt(ctx, s, f2, 0, make([]byte, testSegmentCap*3/2), 1)
		return err
	}(), core.LimitJournal)
	// The refused write needs two whole segments; the one ready spare
	// covers one, so one cap against no headroom is what it misses.
	if ns.Missing != testSegmentCap {
		t.Fatalf("Missing = %d, want one segment cap %d", ns.Missing, testSegmentCap)
	}
	if afterV := c.counters.NextVersion(); afterV != beforeV {
		t.Fatalf("the refusal drew a version: next %v was %v", afterV, beforeV)
	}
	if _, ok := c.Index().Lookup(f2, 0); ok {
		t.Fatal("something of the refused write was published")
	}
	// The first write is held whole still.
	readBack(t, c, f1, 0, testSegmentCap*3/2)
}

// readBack checks the held pieces of f tile [off, off+n) whole, walking
// them through Lookup the way a reader would.
func readBack(t *testing.T, c *Core, f core.FileID, off, n int64) {
	t.Helper()
	got := heldPieces(t, c, f, off, off+n)
	var end int64
	for _, p := range got {
		if p.Extent.Off != (off + end) {
			t.Fatalf("a piece begins at %d where %d is wanted: %+v", p.Extent.Off, off+end, p)
		}
		end += p.Extent.Len
	}
	if end != n {
		t.Fatalf("the held pieces cover %d bytes, want %d", end, n)
	}
}

// streamOf returns which of the core's streams file f routes to, from the
// streams' own accessor, so a test picks two files that share a stream or
// fall on two exactly as the writes do.
func streamOf(c *Core, f core.FileID) int {
	return c.Streams().IndexFor(f)
}

// TestWriteAt_SpareThenRetrySucceeds checks a write whose pieces need more
// spares than the pool has ready — with no other limit refusing — is
// refused naming the spare, the last limit in the order, moves no state,
// and the retry succeeds once the pool can serve it again; and a write
// needing two spares with one ready is refused with the pool's holds
// unchanged.
func TestWriteAt_SpareThenRetrySucceeds(t *testing.T) {
	// The journal is unbounded, so the spare pool is the only limit in
	// the order that can refuse these writes. The pool's refill is gated
	// at the seam, so its ready figure holds still while the writes run.
	c, seam := newTestCore(t)
	s := mustShare(t, c, 1)
	f := testFile(1)
	ctx := context.Background()
	gate := func(on bool) {
		t.Helper()
		var rules []kit.Rule
		if on {
			rules = append(rules, kit.Failing(kit.OpAllocate, "*", 0, errMakeSpares))
		}
		if err := seam.Schedule(rules...); err != nil {
			t.Fatalf("Schedule: %v", err)
		}
	}

	// Gate the maker and hold the whole target, so no unreserved take
	// reaches a spare and none is made to replace it.
	waitIn(t, 5*time.Second, "the pool to reach its target", func() bool {
		return c.pool.Counters().Spares >= 2*testStreams
	})
	gate(true)
	hold, err := c.pool.Hold(2 * testStreams)
	if err != nil {
		t.Fatalf("Hold: %v", err)
	}
	defer hold.Close()
	waitIn(t, 5*time.Second, "the holds to take the whole target", func() bool {
		return c.pool.ReadyContent() == 0
	})

	// The write: 2.5 caps on a fresh stream needs three fresh segments,
	// none covered by the room and none ready for its hold: refused
	// naming the spare, the last limit, with nothing moved.
	beforeHeld := c.pool.Counters().Held
	ns := refused(t, func() error {
		_, err := c.WriteAt(ctx, s, f, 0, make([]byte, testSegmentCap*5/2), 1)
		return err
	}(), core.LimitSpare)
	if ns.Missing < 1 || ns.Unit != core.UnitSegments {
		t.Fatalf("the spare refusal = %d %s, want segments missing", ns.Missing, ns.Unit)
	}
	if got := c.ReservedBytes(); got != 0 {
		t.Fatalf("ReservedBytes = %d after the refused hold, want 0", got)
	}
	if got := c.pool.Counters().Held; got != beforeHeld {
		t.Fatalf("the refused write left the pool owing %d holds, want the %d it had", got, beforeHeld)
	}

	// The retry succeeds once the refill serves again: open the gate,
	// release the hold, and let the refill make what the write needs. The
	// pool serves a refused take's recorded demand above its target, so
	// the wait asks only for the three the write needs; whatever it
	// settles at, the retry takes them whole.
	gate(false)
	hold.Close()
	waitIn(t, 5*time.Second, "the pool to serve unreserved takes again", func() bool {
		return c.pool.ReadyContent() >= 3
	})
	if _, err := c.WriteAt(ctx, s, f, 0, make([]byte, testSegmentCap*5/2), 1); err != nil {
		t.Fatalf("the retry: %v", err)
	}
	if got := c.ReservedBytes(); got != 0 {
		t.Fatalf("ReservedBytes = %d after the retry returned, want 0", got)
	}
}

// TestWriteAt_ThirdPieceFailsReturnsGrant faults the record write of the
// third piece of a large write at the seam and checks the failed write
// returns its grant: ReservedBytes returns to zero, the share's settled
// bytes count only the pieces that landed, and the pool holds none of the
// spares the take reserved for the write's remaining pieces.
func TestWriteAt_ThirdPieceFailsReturnsGrant(t *testing.T) {
	var armed atomic.Bool
	var c *Core
	var seam *kit.Seam
	boom := errors.New("injected record-write failure")
	f := testFile(1)
	var flightHeld atomic.Int64
	var pieces atomic.Int64
	c, seam = newTestCore(t, func(o *coreOptions) {
		o.afterAppend = func() {
			// Pieces past the first only: once the second piece is in,
			// fault the record writes of every segment its own and
			// later, so the third piece's record write — in the
			// segment the second one rolled onto — fails at the seam.
			if !armed.Load() {
				return
			}
			pieces.Add(1)
			if pieces.Load() < 2 {
				return
			}
			// Mid-flight, the write's grant holds the spares its pieces
			// have not yet taken: the pool owes them to this write, taken
			// ahead of every unreserved one.
			flightHeld.Store(c.pool.Counters().Held)
			p, ok := c.Index().Lookup(f, 4096)
			if !ok {
				// The second piece's own publish has not run yet: look
				// up the first piece and fault everything above its
				// segment — the second and third pieces'.
				p, ok = c.Index().Lookup(f, 0)
				if !ok {
					return
				}
			}
			armed.Store(false)
			var rules []kit.Rule
			// The second piece sits in the segment the first rolled to;
			// the third rolls to the next one. Fault from the segment
			// after the piece we faulted on: the third piece's write.
			for id := p.Record.Segment + 1; id <= p.Record.Segment+8; id++ {
				rules = append(rules, kit.Failing(kit.OpWrite, format.SegmentName(id), 0, boom))
			}
			if err := seam.Schedule(rules...); err != nil {
				panic(err)
			}
		}
	})
	s := mustShare(t, c, 1)
	before := c.pool.Counters()

	armed.Store(true)
	_, err := c.WriteAt(context.Background(), s, f, 0, make([]byte, 3*testSegmentCap), 1)
	if !errors.Is(err, boom) {
		t.Fatalf("WriteAt = %v, want the injected failure", err)
	}
	// Mid-flight the grant held the write's spares ahead of every unreserved
	// take; once the write failed, the hold returned them and the pool owes
	// no more than it owed before the write.
	if got := flightHeld.Load(); got < before.Held+1 {
		t.Fatalf("the write held %d spares mid-flight on top of the %d before it, want its grant's", got, before.Held)
	}
	if got := c.ReservedBytes(); got != 0 {
		t.Fatalf("ReservedBytes = %d after the failed write, want 0", got)
	}
	// The pieces that landed are the share's; the failed piece is nowhere.
	// The share's bytes count record bytes, the index's held bytes the
	// payload of the pieces it holds — the landed part of the write, and
	// nothing of the failed piece.
	if got := c.set.ShareBytes(1); got <= 0 || got >= 3*testSegmentCap {
		t.Fatalf("the share's settled bytes = %d, want only the pieces that landed", got)
	}
	var published int64
	for off := int64(0); published < 3*testSegmentCap; {
		p, ok := c.Index().Lookup(f, off)
		if !ok {
			break
		}
		published += p.Extent.Len
		off += p.Extent.Len
	}
	if h, _, _ := c.heldBytes(); h != published {
		t.Fatalf("the index holds %d bytes, want the %d of the pieces that landed", h, published)
	}
	if published >= 3*testSegmentCap {
		t.Fatal("the failed write was published whole")
	}
	// The fault bit on the third piece: exactly two pieces landed, both
	// whole segments' worth or less.
	if got := pieces.Load(); got != 2 {
		t.Fatalf("%d pieces landed before the failure, want exactly 2", got)
	}
	after := c.pool.Counters()
	if after.Held < before.Held {
		t.Fatalf("the failed write lowered the pool's holds to %d from %d", after.Held, before.Held)
	}
	if after.Held > before.Held {
		t.Fatalf("the failed write left the pool owing %d holds, want the %d it had", after.Held, before.Held)
	}
	waitIn(t, 5*time.Second, "the pool to return to its target", func() bool {
		return c.pool.Counters().Spares >= 2*testStreams
	})
}

// TestEdge_FilesystemENOSPCIsNotErrNoSpace fails the device under a write
// that the admission granted — its reservation taken, its pieces landing
// one segment past a full one — and checks the write answers with the
// device's own error: never a NoSpaceError, never ErrNoSpace, and no
// reservation refusal counted for it, for the take was granted and what
// failed is the medium, not a limit.
//
// decision: this drives the ENOSPC where the record write itself meets
// it. The shape where making the segment fails no longer reaches WriteAt
// through the one admission: a write that would need a spare the pool
// cannot serve is refused by the pool before any byte moves, and a write
// the pool served writes into segments the maker already created whole.
func TestEdge_FilesystemENOSPCIsNotErrNoSpace(t *testing.T) {
	c, seam := newTestCore(t)
	s := mustShare(t, c, 1)
	f := testFile(1)
	ctx := context.Background()
	if _, err := c.WriteAt(ctx, s, f, 0, []byte("w"), 1); err != nil {
		t.Fatalf("the warm-up write: %v", err)
	}

	// Every later write of every file meets ENOSPC at the seam: the next
	// write's roll into a fresh segment fails its successor's header write.
	if err := seam.Schedule(kit.Failing(kit.OpWrite, "*", 0, syscall.ENOSPC)); err != nil {
		t.Fatalf("Schedule: %v", err)
	}
	g := testFile(2)
	if streamOf(c, g) == streamOf(c, f) {
		g = testFile(3)
	}
	beforeRefused := c.ReservationRefused()
	_, err := c.WriteAt(ctx, s, g, 0, make([]byte, roomAfter(0)+2), 1)
	if !errors.Is(err, syscall.ENOSPC) {
		t.Fatalf("the write = %v, want the device's ENOSPC", err)
	}
	if errors.Is(err, core.ErrNoSpace) {
		t.Fatalf("the device's ENOSPC matches ErrNoSpace: %v", err)
	}
	var ns *core.NoSpaceError
	if errors.As(err, &ns) {
		t.Fatalf("the device's ENOSPC carries a NoSpaceError: %v", err)
	}
	if got := c.ReservedBytes(); got != 0 {
		t.Fatalf("ReservedBytes = %d after the failed write, want 0", got)
	}
	if got := c.ReservationRefused(); got != beforeRefused {
		t.Fatalf("ReservationRefused rose to %d, want the %d it was", got, beforeRefused)
	}
}

// TestWriteAt_RefusalHoldsNoLock checks that after a refused write the
// file's append domain is free — a second write on the same file from
// another goroutine runs — and the index is unlocked: FileEntries answers.
func TestWriteAt_RefusalHoldsNoLock(t *testing.T) {
	c, _ := newTestCore(t)
	const each = 4096
	s := mustLimitShare(t, c, 1, recordBytes(each))
	f := testFile(1)
	ctx := context.Background()
	if _, err := c.WriteAt(ctx, s, f, 0, make([]byte, each), 1); err != nil {
		t.Fatalf("the first write: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		_, err := c.WriteAt(ctx, s, f, each, make([]byte, each), 1)
		done <- err
	}()
	select {
	case err := <-done:
		_ = refused(t, err, core.LimitShare)
	case <-time.After(5 * time.Second):
		t.Fatal("the refused write never returned")
	}

	// The same file's append domain is free — a second write on it from
	// another goroutine runs, refused by the limit and not by any lock the
	// refusal left — and the index answers.
	second := make(chan error, 1)
	go func() {
		_, err := c.WriteAt(ctx, s, f, 2*each, make([]byte, 4), 1)
		second <- err
	}()
	select {
	case err := <-second:
		_ = refused(t, err, core.LimitShare)
	case <-time.After(5 * time.Second):
		t.Fatal("the file's append domain was still held after the refusal")
	}
	locked := make(chan struct{})
	go func() {
		_ = c.Index().FileEntries(f)
		close(locked)
	}()
	select {
	case <-locked:
	case <-time.After(5 * time.Second):
		t.Fatal("the index was still locked after the refusal")
	}
}

// TestWriteAt_ReservedBytesFallsAfterRecordWritten holds a write between
// its append and its publish and checks the two gauges never double-count a
// record: by the time the hook runs — the record appended, its bytes the
// share's, the grant told — ReservedBytes holds none of it and the share
// counts it exactly once. A reservation that never hands a written record
// over leaves the record in both gauges; one that hands it over before the
// share counts it leaves it in neither.
//
// stageSnapshot is one read of the two gauges the hook takes: what the
// reservations hold and what the share has settled.
type stageSnapshot struct {
	reserved, share int64
}

// TestWriteAt_ReservedBytesFallsAfterRecordWritten holds a write between
// its append and its publish and checks the two gauges never double-count a
// record: by the time the hook runs — the record appended, its bytes the
// share's, the grant told — ReservedBytes holds none of it and the share
// counts it exactly once. A reservation that never hands a written record
// over leaves the record in both gauges; one that hands it over before the
// share counts it leaves it in neither.
func TestWriteAt_ReservedBytesFallsAfterRecordWritten(t *testing.T) {
	const each = 4096
	var mu sync.Mutex
	var stages []stageSnapshot
	var c *Core
	c, _ = newTestCore(t, func(o *coreOptions) {
		o.afterAppend = func() {
			// Between the append and the publish, with the record
			// already counted in its share's bytes: the reservation
			// has handed the record over — ReservedBytes holds the
			// write's record less it — and the two gauges together
			// cover the record exactly once.
			mu.Lock()
			stages = append(stages, stageSnapshot{c.ReservedBytes(), c.set.ShareBytes(1)})
			mu.Unlock()
		}
	})
	s := mustShare(t, c, 1)
	f := testFile(1)
	if _, err := c.WriteAt(context.Background(), s, f, 0, make([]byte, each), 1); err != nil {
		t.Fatalf("the write: %v", err)
	}
	if len(stages) < 1 {
		t.Fatal("the hook read no stage between the append and the publish")
	}
	// The one-piece write's record is the share's when the hook runs: the
	// reservation holds none of it any more, and the share counts the
	// record once.
	for i, st := range stages {
		if st.reserved != 0 {
			t.Fatalf("stage %d holds %d reserved beside %d settled, want none", i, st.reserved, st.share)
		}
		if st.share != recordBytes(each)*int64(i+1) {
			t.Fatalf("stage %d counts %d settled, want the record counted once (%d)", i, st.share, recordBytes(each)*int64(i+1))
		}
	}
	waitIn(t, 5*time.Second, "the share to settle", func() bool {
		return c.set.ShareBytes(1) == recordBytes(each)
	})
	if got := c.ReservedBytes(); got != 0 {
		t.Fatalf("ReservedBytes = %d after the write returned, want 0", got)
	}
}

// TestWriteAt_IndexBoundRefusesBeforeIO fills the extent bound with one
// write and checks the next is refused naming the index before any seam
// call, with the version undrawn, the index's own refusal counter up by one
// and the reservation counter up by one.
func TestWriteAt_IndexBoundRefusesBeforeIO(t *testing.T) {
	counters := &index.Counters{}
	if err := counters.SetVersionLimit(core.Version{Counter: 1 << 40}); err != nil {
		t.Fatal(err)
	}
	c, seam := newTestCore(t, func(o *coreOptions) {
		o.state = State{Counters: counters}
		o.extentLimit = 1
	})
	s := mustShare(t, c, 1)
	f := testFile(1)
	ctx := context.Background()

	// The bound's one entry: the first write passes it. Its roll spent a
	// spare; let the refill finish before the refusal census, so the
	// background make's own seam calls happen outside the window.
	if _, err := c.WriteAt(ctx, s, f, 0, make([]byte, 4), 1); err != nil {
		t.Fatalf("the bound-filling write: %v", err)
	}
	waitIn(t, 5*time.Second, "the pool to come home to its target", func() bool {
		return c.pool.Counters().Spares == 2*testStreams
	})

	beforeV := c.counters.NextVersion()
	beforeC := seamCensus(seam)
	beforeRefused := c.ReservationRefused()
	beforeIndexRefused := c.index.Refused()

	ns := refused(t, func() error {
		_, err := c.WriteAt(ctx, s, f, 1<<20, make([]byte, 4096), 1)
		return err
	}(), core.LimitIndex)
	if ns.Missing < 1 || ns.Unit != core.UnitEntries {
		t.Fatalf("the index refusal = %d %s, want entries missing", ns.Missing, ns.Unit)
	}
	afterC := seamCensus(seam)
	if afterC != beforeC {
		t.Fatalf("the refusal made %d seam calls, want none", afterC-beforeC)
	}
	if afterV := c.counters.NextVersion(); afterV != beforeV {
		t.Fatalf("the refusal drew a version: next %v was %v", afterV, beforeV)
	}
	if _, ok := c.Index().Lookup(f, 1<<20); ok {
		t.Fatal("the refused write was published")
	}
	if got := c.ReservationRefused(); got != beforeRefused+1 {
		t.Fatalf("ReservationRefused = %d, want %d", got, beforeRefused+1)
	}
	if got := c.index.Refused(); got != beforeIndexRefused+1 {
		t.Fatalf("the index's own refusal counter = %d, want %d", got, beforeIndexRefused+1)
	}
}

// TestWriteAt_RefusalEvictsNothing fills the journal with held content,
// refuses a write by the share's limit and checks the refusal changed
// nothing it must not: no held entry's offloaded bit moved, no extent was
// exported and the set lost no segment.
func TestWriteAt_RefusalEvictsNothing(t *testing.T) {
	c, _ := newTestCore(t)
	const each = 4096
	s := mustLimitShare(t, c, 1, recordBytes(each))
	ctx := context.Background()
	f := testFile(1)
	if _, err := c.WriteAt(ctx, s, f, 0, make([]byte, each), 1); err != nil {
		t.Fatalf("the first write: %v", err)
	}
	beforeEntries := c.Index().FileEntries(f)
	beforeCounters := c.set.Counters()

	_, err := c.WriteAt(ctx, s, f, each, make([]byte, each), 1)
	_ = refused(t, err, core.LimitShare)

	afterEntries := c.Index().FileEntries(f)
	if len(afterEntries) != len(beforeEntries) {
		t.Fatalf("the refusal moved the index: %d entries were %d", len(afterEntries), len(beforeEntries))
	}
	for i := range beforeEntries {
		if afterEntries[i] != beforeEntries[i] {
			t.Fatalf("entry %d changed: %+v was %+v", i, afterEntries[i], beforeEntries[i])
		}
	}
	if got := c.set.Counters(); got.Segments != beforeCounters.Segments || got.UsedBytes < beforeCounters.UsedBytes {
		t.Fatalf("the refusal moved the set: %+v was %+v", got, beforeCounters)
	}
}

// TestWriteAt_SameStreamWritersNeverMeetMidWriteRefusal drives eight
// concurrent writers on one stream, each asking a three-piece write on the
// stream's fresh, roomless state — so every roll runs while other writers'
// pieces are still landing — with the pool's refill gated at the seam: the
// four ready spares it starts with are all there are, an unreserved roll is
// really refused, and the fixture checks the admission settles every write
// whole: each either lands every piece or is refused whole before its
// version is drawn. None meets the pool's
// refusal mid-flight, after its first piece published — the failure a
// room read by the caller, stale against another writer's landing,
// allowed: a take that grants a stale-high window holds no spare for the
// piece its write finds itself rolling.
func TestWriteAt_SameStreamWritersNeverMeetMidWriteRefusal(t *testing.T) {
	c, seam := newTestCore(t)
	s := mustShare(t, c, 1)
	files := []core.FileID{testFile(1), testFile(3), testFile(5), testFile(7),
		testFile(9), testFile(11), testFile(13), testFile(15)}
	for _, f := range files {
		if streamOf(c, f) != streamOf(c, files[0]) {
			t.Fatal("the fixture's files must share one stream")
		}
	}
	// Each writer's write is three whole pieces on the stream's fresh,
	// roomless state — two full empty segments and a small tail — so the
	// only spares its rolls reach are the ones its own grant holds. The
	// stagger below keeps the takes landing among the first three's
	// appends, so a grant's sized pieces meet whatever the earlier
	// grant's landings leave them.
	each := 2*emptyRoomOf(testSegmentCap) + 50
	if int64(len(files))*(recordBytes(each)) <= 8*testSegmentCap {
		t.Fatal("the fixture's combined asks do not cross enough segments")
	}
	// Gate the maker at the seam: the refill makes nothing while the
	// writers run, so the pool's four ready spares are all it has, an
	// unreserved roll IS refused, and a write short of its pieces meets
	// the refusal the moment its own hold runs out.
	awaitSettledSpares(t, c, 2*testStreams)
	if err := seam.Schedule(kit.Failing(kit.OpAllocate, "*", 0, errMakeSpares)); err != nil {
		t.Fatalf("Schedule: %v", err)
	}
	var wg sync.WaitGroup
	var minted, refusedWhole atomic.Int64
	var mu sync.Mutex
	midWrite := map[*core.NoSpaceError]struct{}{}
	// The stagger that puts the later writers' takes among the first
	// three's landings, staged by events rather than a timer: the first
	// three mark their arrival on the barrier's own WaitGroup, a waiter
	// lifts the gate once all three have arrived, and only the later
	// writers wait on the gate — the barrier and the gate are separate
	// channels, so no late writer can steal an arrival the waiter counts.
	var arrivals sync.WaitGroup
	arrivals.Add(3)
	gate := make(chan struct{})
	go func() {
		arrivals.Wait()
		close(gate)
	}()
	for i, f := range files {
		wg.Add(1)
		go func(f core.FileID, i int) {
			defer wg.Done()
			if i < 3 {
				arrivals.Done()
			} else {
				<-gate
			}
			_, err := c.WriteAt(context.Background(), s, f, 0, make([]byte, each), 1)
			if err != nil {
				// A take refuses before any byte moves, so a failed
				// write whose records are on disk is one refused past
				// its own first landed piece — the mid-write failure
				// the admission exists to prevent.
				recs := fileWrites(t, seam, f)
				if len(recs) > 0 && int64(len(recs)) < each/emptyRoomOf(testSegmentCap)+2 {
					mu.Lock()
					for ns := range errChain(err) {
						if ns.Limit == core.LimitSpare {
							midWrite[ns] = struct{}{}
						}
					}
					mu.Unlock()
				}
				refusedWhole.Add(1)
				return
			}
			minted.Add(1)
		}(f, i)
	}
	wg.Wait()
	if minted.Load() == 0 && refusedWhole.Load() == 0 {
		t.Fatal("no write answered at all; the fixture never gave the pool its spares")
	}
	for ns := range midWrite {
		t.Fatalf("a write met the pool's refusal mid-flight, past its first landed piece (%s)", ns)
	}
}

// errChain yields every NoSpaceError inside err, joined or wrapped.
func errChain(err error) map[*core.NoSpaceError]struct{} {
	out := map[*core.NoSpaceError]struct{}{}
	var walk func(error)
	walk = func(e error) {
		if e == nil {
			return
		}
		var ns *core.NoSpaceError
		if errors.As(e, &ns) {
			out[ns] = struct{}{}
		}
		switch x := e.(type) {
		case interface{ Unwrap() error }:
			walk(x.Unwrap())
		case interface{ Unwrap() []error }:
			for _, c := range x.Unwrap() {
				walk(c)
			}
		}
	}
	walk(err)
	return out
}

// TestWriteAt_StaggeredWritersFillTheLimitExactly drives the share's
// boundary over the WriteAt path with two same-stream writers staggered
// around a landing — writer two's take reads the room writer one's landed
// record left — against a limit of exactly their two records, and a third
// writer arriving after the pair settled. The limit bounds record bytes
// (RFC 1 section 6): both writers are admitted, the settled bytes sit at
// the limit and not past it, and the third is refused naming its whole
// record as missing. The two writers are serialized end to end, so no
// piece of theirs is displaced; a displaced write draws a piece beyond its
// split and can pass the limit by at most one record header, which the
// admission's decision states and capacity's
// TestTake_InterleavedDrawnPieceOversetsTheShareByOneHeader pins at the
// admission's own seam.
//
// The arithmetic: rb(4096) = 4200, so the limit is 8400 and the settled
// bytes after the pair are 8400; the third writer's 4200-byte record is all
// missing.
func TestWriteAt_StaggeredWritersFillTheLimitExactly(t *testing.T) {
	c, _ := newTestCore(t)
	files := []core.FileID{testFile(1), testFile(3), testFile(5)}
	for _, f := range files {
		if streamOf(c, f) != streamOf(c, files[0]) {
			t.Fatal("the fixture's files must share one stream")
		}
	}
	const each = 4096
	s := mustLimitShare(t, c, 1, 2*(recordBytes(each)))
	ctx := context.Background()

	// The two writers the limit admits: writer two is held at its arrival
	// gate until writer one's WriteAt has fully answered, so its take
	// names the room writer one's landed record left — the stagger that
	// puts the pair's takes on either side of a landing without a timer.
	var wg sync.WaitGroup
	oneDone := make(chan error, 1)
	twoGo := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, err := c.WriteAt(ctx, s, files[0], 0, make([]byte, each), 1)
		oneDone <- err
		close(oneDone)
	}()
	wg.Add(1)
	go func() {
		defer wg.Done()
		<-twoGo
		if _, err := c.WriteAt(ctx, s, files[1], 0, make([]byte, each), 1); err != nil {
			t.Errorf("the second admitted writer's WriteAt: %v", err)
		}
	}()
	if err := <-oneDone; err != nil {
		t.Fatalf("the first admitted writer's WriteAt: %v", err)
	}
	close(twoGo)
	wg.Wait()

	// The stream now holds the two landed records and no reservation: the
	// third writer's record adds to the settled 8400 and overruns the
	// limit by exactly its own bytes.
	third := files[2]
	ns := refused(t, func() error {
		_, err := c.WriteAt(ctx, s, third, 0, make([]byte, each), 1)
		return err
	}(), core.LimitShare)
	if want := recordBytes(each); ns.Missing != want {
		t.Fatalf("the third writer's refusal names %d missing, want %d: its record over the settled bytes", ns.Missing, want)
	}
	// Both admitted records landed exactly at the limit, none past it.
	if got := c.set.ShareBytes(1); got != s.Limit() {
		t.Fatalf("the share's settled bytes %d, want exactly the limit %d", got, s.Limit())
	}
	if got := c.ReservedBytes(); got != 0 {
		t.Fatalf("ReservedBytes = %d after the writers, want 0", got)
	}
}

// TestWriteAt_HeldSparesCoverEveryPieceOfAMultiSegmentWrite checks where
// the refusal answers for a multi-segment write the journal cannot hold: a
// write of two full empty-segment payloads and a small tail — three pieces
// on a fresh stream, each an empty segment but the tail's — against a
// journal of four caps whose footprint the warm write already sits at. A
// hold one spare short of the pieces the stream's own split draws — the
// ask bytes-over-the-cap sized — admitted the write, published its first
// piece, drew its version and met the pool's refusal on its third roll; the
// hold the pieces name refuses the whole write up front, naming the
// journal's maximum ahead of the spare pool, and the write leaves nothing
// behind.
func TestWriteAt_HeldSparesCoverEveryPieceOfAMultiSegmentWrite(t *testing.T) {
	// A journal of four caps: the warm write below spends two of them, the
	// pool settles with the other two, and the footprint sits at the
	// maximum, so the three-piece write's uncovered third segment is the
	// journal's to refuse.
	c, _ := newTestCore(t, capacityCoreOption(4*testSegmentCap))
	s := mustShare(t, c, 1)
	ctx := context.Background()

	warm := testFile(1)
	journalSettled(t, c, s, warm, 4*testSegmentCap)
	f := testFile(2)
	if streamOf(c, f) == streamOf(c, warm) {
		t.Fatal("the two files share a stream; the fresh-stream leg cannot run")
	}
	beforeV := c.counters.NextVersion()

	p := make([]byte, 2*emptyRoomOf(testSegmentCap)+50)
	ns := refused(t, func() error {
		_, err := c.WriteAt(ctx, s, f, 0, p, 1)
		return err
	}(), core.LimitJournal)
	// The journal's refusal names the third segment the write needs that
	// no ready spare covers and no headroom admits: one cap, the whole
	// figure the maximum lacks.
	if ns.Missing != testSegmentCap {
		t.Fatalf("Missing = %d, want the one uncovered cap %d", ns.Missing, testSegmentCap)
	}
	// The refusal answered before the write drew or published anything:
	// the short-held version of this fixture published its first piece
	// and drew its version before the refusal met it.
	if _, ok := c.Index().Lookup(f, 0); ok {
		t.Fatal("the refused write published a piece")
	}
	if afterV := c.counters.NextVersion(); afterV != beforeV {
		t.Fatalf("the refusal drew a version: next %v was %v", afterV, beforeV)
	}
	if got := c.ReservedBytes(); got != 0 {
		t.Fatalf("ReservedBytes = %d after the refusal, want 0", got)
	}
}

// TestWriteRefusedBelowHalfHeadroom drives a core whose journal sits at its
// limit — the footprint at the maximum, the pool settled — draws the
// headroom below half and checks the next content write is refused naming
// the headroom: at once, before its version is drawn, nothing appended, no
// seam call made, no reservation held, and the spares the write found still
// ready.
func TestWriteRefusedBelowHalfHeadroom(t *testing.T) {
	c, seam := newTestCore(t,
		capacityCoreOption(5*testSegmentCap),
		func(o *coreOptions) { o.headroomRecords = headroomFixture })
	s := mustShare(t, c, 1)
	f := testFile(1)
	ctx := context.Background()

	// The warm write takes two of the pool's four content spares — one it
	// fills, one its last byte rolls onto — so the footprint settles at the
	// maximum of five caps (the headroom's spare among them) with two
	// spares ready: the maker refuses every further reservation before any
	// I/O, and the journal holds its limit.
	if _, err := c.WriteAt(ctx, s, f, 0, make([]byte, roomAfter(0)+1), 1); err != nil {
		t.Fatalf("the warm write: %v", err)
	}
	waitIn(t, 5*time.Second, "the pool to settle at its last two spares", func() bool {
		pc := c.pool.Counters()
		return pc.Spares == 2 && pc.Held == 0
	})

	drawHeadroom(t, c, 3) // three records drawn, one past the drawing side's half

	beforeV := c.counters.NextVersion()
	beforeC := seamCensus(seam)
	beforeRefused := c.ReservationRefused()

	ns := refused(t, func() error {
		_, err := c.WriteAt(ctx, s, f, 1<<20, []byte("w"), 1)
		return err
	}(), core.LimitHeadroom)
	if ns.Missing < 1 || ns.Unit != core.UnitEntries {
		t.Fatalf("the headroom refusal = %d %s, want at least one entry missing", ns.Missing, ns.Unit)
	}

	afterC := seamCensus(seam)
	if afterC != beforeC {
		t.Fatalf("the refusal made %d seam calls, want none", afterC-beforeC)
	}
	if afterV := c.counters.NextVersion(); afterV != beforeV {
		t.Fatalf("the refusal drew a version: next %v was %v", afterV, beforeV)
	}
	if _, ok := c.Index().Lookup(f, 1<<20); ok {
		t.Fatal("the refused write was published")
	}
	if got := c.ReservedBytes(); got != 0 {
		t.Fatalf("ReservedBytes = %d after the refusal, want 0", got)
	}
	if got := c.ReservationRefused(); got != beforeRefused+1 {
		t.Fatalf("ReservationRefused = %d, want %d", got, beforeRefused+1)
	}
	if got := c.pool.Counters().Spares; got != 2 {
		t.Fatalf("the refusal took a spare: %d ready, want the 2 it found", got)
	}
}

// TestWriteAtLimitNamesHeadroomOverSpare checks the order the limits refuse
// in on a journal at its limit with the headroom below half: a roomless
// write whose three whole segments not one ready spare covers — a write the
// spare pool would refuse, and the journal's maximum after it — is refused
// naming the headroom, the first limit in the order, with nothing of it
// published and no version drawn.
func TestWriteAtLimitNamesHeadroomOverSpare(t *testing.T) {
	c, _ := newTestCore(t,
		capacityCoreOption(5*testSegmentCap),
		func(o *coreOptions) { o.headroomRecords = headroomFixture })
	s := mustShare(t, c, 1)
	warm := testFile(1)
	f := testFile(2)
	if streamOf(c, f) == streamOf(c, warm) {
		t.Fatal("the two files share a stream; the fresh-stream leg cannot run")
	}
	ctx := context.Background()
	if _, err := c.WriteAt(ctx, s, warm, 0, make([]byte, roomAfter(0)+1), 1); err != nil {
		t.Fatalf("the warm write: %v", err)
	}
	waitIn(t, 5*time.Second, "the pool to settle at its last two spares", func() bool {
		pc := c.pool.Counters()
		return pc.Spares == 2 && pc.Held == 0
	})
	drawHeadroom(t, c, 3)

	// Drain the ready content spares, so the write's own hold would be the
	// pool's refusal: only the order the refusal names is left to observe.
	hold, err := c.pool.Hold(2)
	if err != nil {
		t.Fatalf("draining the pool's ready spares: %v", err)
	}
	defer hold.Close()
	if got := c.pool.ReadyContent(); got != 0 {
		t.Fatalf("the drain left %d content spares ready", got)
	}

	beforeV := c.counters.NextVersion()
	_ = refused(t, func() error {
		_, err := c.WriteAt(ctx, s, f, 0, make([]byte, testSegmentCap*5/2), 1)
		return err
	}(), core.LimitHeadroom)
	if afterV := c.counters.NextVersion(); afterV != beforeV {
		t.Fatalf("the refusal drew a version: next %v was %v", afterV, beforeV)
	}
	if _, ok := c.Index().Lookup(f, 0); ok {
		t.Fatal("the refused write was published")
	}
}

// TestWriteAdmittedBelowHalfHeadroomOffTheLimit checks the half rule is a
// state of the journal at its limit: the same headroom drawn below half
// refuses a content write while the journal holds its limit and admits it
// once a retirement takes the journal below — and refuses again the moment
// the limit returns.
func TestWriteAdmittedBelowHalfHeadroomOffTheLimit(t *testing.T) {
	c, _ := newTestCore(t,
		capacityCoreOption(5*testSegmentCap),
		func(o *coreOptions) { o.headroomRecords = headroomFixture })
	s := mustShare(t, c, 1)
	f := testFile(1)
	ctx := context.Background()
	if _, err := c.WriteAt(ctx, s, f, 0, make([]byte, roomAfter(0)+1), 1); err != nil {
		t.Fatalf("the warm write: %v", err)
	}
	waitIn(t, 5*time.Second, "the pool to settle at its last two spares", func() bool {
		pc := c.pool.Counters()
		return pc.Spares == 2 && pc.Held == 0
	})
	drawHeadroom(t, c, 3)
	// The gate holds over the drawn figure: below half at the limit, its
	// own admission class is refused.
	if !c.headroom.BelowHalf() {
		t.Fatal("three records drawn left the headroom above half")
	}

	// One retirement takes the journal off its limit: the warm write's
	// sealed first segment leaves the ledger, and the footprint falls a
	// whole cap below the maximum — a retirement of a live segment, as the
	// reclamation path's own, with the stream still writing in its
	// successor.
	p, ok := c.Index().Lookup(f, 0)
	if !ok {
		t.Fatal("the warm write's first piece is not held")
	}
	if _, err := c.set.Retire(p.Record.Segment); err != nil {
		t.Fatalf("retiring the warm write's sealed segment: %v", err)
	}

	// Off the limit, the same write the throttle refused is admitted and
	// its record appends whole, published and held.
	beforeV := c.counters.NextVersion()
	v, err := c.WriteAt(ctx, s, f, 1<<20, []byte("w"), 1)
	if err != nil {
		t.Fatalf("the write off the limit: %v", err)
	}
	// Off the limit the write drew the version it asked for and its record
	// is published: the whole half-drawn headroom throttles nothing below
	// the journal's limit.
	if v.Counter != beforeV.Counter {
		t.Fatalf("the admitted write drew version %v, want %v", v.Counter, beforeV.Counter)
	}
	readBack(t, c, f, 1<<20, 1)

	// The limit back, the headroom throttles the same write again. The
	// adopted spare raises the ledger a cap — a spare the open path
	// accounts the way its own makes do — for no retirement stands between
	// the journal and its maximum here.
	if err := c.set.AdoptSpare(1 << 20); err != nil {
		t.Fatalf("raising the ledger with an adopted spare: %v", err)
	}
	beforeRefused := c.ReservationRefused()
	beforeV = c.counters.NextVersion()
	_ = refused(t, func() error {
		_, err := c.WriteAt(ctx, s, f, 2<<20, []byte("w"), 1)
		return err
	}(), core.LimitHeadroom)
	if afterV := c.counters.NextVersion(); afterV != beforeV {
		t.Fatalf("the refusal drew a version: next %v was %v", afterV, beforeV)
	}
	if got := c.ReservationRefused(); got != beforeRefused+1 {
		t.Fatalf("ReservationRefused = %d, want %d", got, beforeRefused+1)
	}
}

// TestFillRefusedBelowHalfHeadroom: a fill is admitted like a write, every
// limit but the version limit, so on a journal at its limit with the headroom
// below half it is refused naming the headroom, with nothing appended and
// nothing held.
func TestFillRefusedBelowHalfHeadroom(t *testing.T) {
	c, seam := newTestCore(t,
		capacityCoreOption(5*testSegmentCap),
		func(o *coreOptions) { o.headroomRecords = headroomFixture })
	s := mustShare(t, c, 1)
	ctx := context.Background()

	if _, err := c.WriteAt(ctx, s, testFile(1), 0, make([]byte, roomAfter(0)+1), 1); err != nil {
		t.Fatalf("the warm write: %v", err)
	}
	waitIn(t, 5*time.Second, "the pool to settle at its last two spares", func() bool {
		pc := c.pool.Counters()
		return pc.Spares == 2 && pc.Held == 0
	})
	drawHeadroom(t, c, 3)

	beforeC := seamCensus(seam)
	f := testFile(2)
	err := c.Fill(ctx, s, f, 0, ff(1), c.Index().AsOf(f), ver(1))
	_ = refused(t, err, core.LimitHeadroom)
	if afterC := seamCensus(seam); afterC != beforeC {
		t.Fatalf("the refusal made %d seam calls, want none", afterC-beforeC)
	}
	if _, ok := c.Index().Lookup(f, 0); ok {
		t.Fatal("the refused fill was published")
	}
	if got := c.ReservedBytes(); got != 0 {
		t.Fatalf("ReservedBytes = %d after the refusal, want 0", got)
	}
}
