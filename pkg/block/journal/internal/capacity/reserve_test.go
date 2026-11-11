package capacity

import (
	"context"
	"errors"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/format"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/segment"
)

// TestPiecesFor checks the piece and spare count a reservation of a given
// size draws: two pieces per write, plus one per cap the bytes can cross,
// and one spare per cap plus the first segment.
func TestPiecesFor(t *testing.T) {
	tests := []struct {
		name       string
		bytes      int64
		writes     int
		cap        int64
		wantPieces int
		wantSpares int
	}{
		{"one write of one cap", int64(64 << 10), 1, 64 << 10, 3, 2},
		{"one write of one byte", 1, 1, 64 << 10, 3, 2},
		{"one write past one cap", (64 << 10) + 1, 1, 64 << 10, 4, 3},
		{"ten thousand writes of 1 GiB, 1 MiB caps", 1 << 30, 10_000, 1 << 20, 21_024, 1_025},
		{"ten thousand writes of 1 GiB, 64 KiB caps", 1 << 30, 10_000, 64 << 10, 36_384, 16_385},
		{"nothing asked", 0, 1, 64 << 10, 0, 0},
		{"negative bytes", -1, 1, 64 << 10, 0, 0},
		{"no writes", 4096, 0, 64 << 10, 0, 0},
		{"negative writes", 4096, -1, 64 << 10, 0, 0},
		{"an ask no count holds", math.MaxInt64, 1, 1, math.MaxInt, math.MaxInt},
		{"writes no count holds", 4096, math.MaxInt, 64 << 10, math.MaxInt, math.MaxInt},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pieces, spares := PiecesFor(tt.bytes, tt.writes, tt.cap)
			if pieces != tt.wantPieces || spares != tt.wantSpares {
				t.Fatalf("PiecesFor(%d, %d, %d) = %d pieces, %d spares, want %d and %d",
					tt.bytes, tt.writes, tt.cap, pieces, spares, tt.wantPieces, tt.wantSpares)
			}
		})
	}
}

// reserve calls Reserve and fails the test on any error.
func (r *rig) reserve(t *testing.T, rq ReserveRequest) *Reservation {
	t.Helper()
	rs, err := r.c.Reserve(context.Background(), rq)
	if err != nil {
		t.Fatalf("Reserve(%+v): %v", rq, err)
	}
	return rs
}

// refusedReserve calls Reserve and fails the test unless it was refused with
// the wanted limit, returning the refusal's fields; the reservation settled is
// asserted to have held nothing.
func (r *rig) refusedReserve(t *testing.T, rq ReserveRequest, want core.Limit) *core.NoSpaceError {
	t.Helper()
	rs, err := r.c.Reserve(context.Background(), rq)
	if err == nil {
		rs.Release()
		t.Fatalf("Reserve(%+v) was granted, want a refusal naming %s", rq, want)
	}
	var ns *core.NoSpaceError
	if !errors.As(err, &ns) {
		t.Fatalf("Reserve(%+v) = %v, want a *core.NoSpaceError", rq, err)
	}
	if want != 0 && ns.Limit != want {
		t.Fatalf("Reserve(%+v) refused by %s, want %s", rq, ns.Limit, want)
	}
	if got := r.c.ReservedBytes(); got != 0 {
		t.Fatalf("after the refusal by %s, ReservedBytes = %d, want 0", ns.Limit, got)
	}
	if got := r.pool.Counters().Held; got != 0 {
		t.Fatalf("after the refusal by %s, the pool holds %d spares, want 0", ns.Limit, got)
	}
	return ns
}

// refusedWrite calls Take and fails the test unless it was refused with the
// wanted limit. It leaves the reservation state alone: a test that holds a
// reservation checks a plain write leaves it untouched.
func (r *rig) refusedWrite(t *testing.T, rq Request, want core.Limit) *core.NoSpaceError {
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
	if ns.Limit != want {
		t.Fatalf("Take(%+v) refused by %s, want %s", rq, ns.Limit, want)
	}
	return ns
}

// rq is the reservation request every test starts from: tag1, stream 1, a
// 4096-byte ask in one write, no share limit.
func rq() ReserveRequest {
	return ReserveRequest{Tag: tag1, Stream: 1, Bytes: 4096, Writes: 1}
}

// TestReserve_ExactFitShare checks a reservation of exactly the share's
// remaining record bytes is taken and one byte more is refused naming the
// share, while the reservation's own bytes count against the limit its
// release returns.
func TestReserve_ExactFitShare(t *testing.T) {
	r := newRig(t, std())
	r.seed(t, 896) // a 1000-byte record

	pieces, spares := PiecesFor(4096, 1, testCap)
	q := rq()
	q.ShareLimit = 1000 + 4096
	rs := r.reserve(t, q)
	if want := q.Bytes; r.c.ReservedBytes() != want {
		t.Fatalf("ReservedBytes = %d after the exact-fit reservation, want the ask's %d bytes",
			r.c.ReservedBytes(), want)
	}
	if got := r.c.reservedAsk; got != 0 {
		t.Fatalf("the journal-side ask = %d once the hold covers its %d pieces, want 0: the spares are charged once, in the footprint", got, pieces)
	}
	if got := r.pool.Counters().Held; got != int64(spares) {
		t.Fatalf("the pool holds %d spares, want the reservation's %d", got, spares)
	}
	rs.Release()
	if got := r.c.ReservedBytes(); got != 0 {
		t.Fatalf("after Release, ReservedBytes = %d, want 0", got)
	}
	if got := r.c.reservedAsk; got != 0 {
		t.Fatalf("after Release, the journal-side ask = %d, want 0", got)
	}

	q.Bytes = 4097 // one byte over the share's remaining room
	ns := r.refusedReserve(t, q, core.LimitShare)
	if ns.Missing != 1 || ns.Unit != core.UnitBytes {
		t.Fatalf("the share's refusal = %d %s, want one byte", ns.Missing, ns.Unit)
	}
	// The bytes it gives back are its own ask alone: the seed's record is
	// still the share's, so the same ask is taken again.
	r.reserve(t, rq()).Release()
}

// TestReserve_ExactFitJournal checks a reservation of exactly the journal's
// remaining room is taken and one byte more is refused naming the journal's
// maximum.
func TestReserve_ExactFitJournal(t *testing.T) {
	o := std()
	// The maximum is set below, once the footprint is read.
	r := newRig(t, o)
	n := int64(1000)
	pieces, _ := PiecesFor(n, 1, testCap)
	ask := n + int64(pieces)*format.CatalogEntrySize
	maxBytes := r.set.Counters().UsedBytes + ask
	r.c.cfg.MaxBytes = maxBytes

	q := rq()
	q.Bytes = n
	r.reserve(t, q).Release()

	q.Bytes = n + 1 // the footer count is the same, so the ask is a byte over
	ns := r.refusedReserve(t, q, core.LimitJournal)
	if ns.Missing != 1 || ns.Unit != core.UnitBytes {
		t.Fatalf("the journal's refusal = %d %s, want one byte", ns.Missing, ns.Unit)
	}
}

// TestReserve_JournalRoomHeldWhileTheHoldIsInFlight checks two reservations
// that both read one footprint's remaining room cannot both pass while the
// first's hold is still being served: the first's ask is charged beside the
// footprint until its spares answer, for the second to find spent, and falls to
// zero once they do, the spares being in the footprint already.
func TestReserve_JournalRoomHeldWhileTheHoldIsInFlight(t *testing.T) {
	r := newRig(t, std())
	pieces, _ := PiecesFor(4096, 1, testCap)
	one := int64(4096 + pieces*format.CatalogEntrySize)
	r.c.cfg.MaxBytes = r.set.Counters().UsedBytes + one

	parked, resume := make(chan struct{}), make(chan struct{})
	r.holdSpy = func(int) {
		close(parked)
		<-resume
	}
	type result struct {
		rs  *Reservation
		err error
	}
	first := make(chan result, 1)
	go func() {
		rs, err := r.c.Reserve(context.Background(), rq())
		first <- result{rs, err}
	}()
	<-parked

	second, err := r.c.Reserve(context.Background(), rq())
	if err == nil {
		second.Release()
		t.Fatal("a second reservation of the room the first holds was taken")
	}
	var ns *core.NoSpaceError
	if !errors.As(err, &ns) || ns.Limit != core.LimitJournal {
		t.Fatalf("the second reservation = %v, want a refusal naming the journal", err)
	}
	if ns.Missing != one || ns.Unit != core.UnitBytes {
		t.Fatalf("the second reservation's refusal = %d %s, want the %d bytes the first holds", ns.Missing, ns.Unit, one)
	}
	if got := r.c.ReservedBytes(); got != 4096 {
		t.Fatalf("the refused second reservation left ReservedBytes = %d, want the first's 4096", got)
	}

	close(resume)
	res := <-first
	if res.err != nil {
		t.Fatalf("the first reservation: %v", res.err)
	}
	defer res.rs.Release()
	if got := r.c.reservedAsk; got != 0 {
		t.Fatalf("after the hold answered, the journal-side ask = %d, want 0", got)
	}
}

// TestReserve_JournalMaximumChargedOnce checks a reservation is charged against
// the journal's maximum once: its held spares are in the footprint, as are the
// spares the maker makes to stay ahead of them, so the ask beside the footprint
// is zero and a plain write's headroom (maximum less footprint less that ask)
// falls by the footprint's growth alone, not by the growth and the ask.
func TestReserve_JournalMaximumChargedOnce(t *testing.T) {
	o := std()
	o.spares, o.unprimed = 5, true
	o.makerMax = 20 * testCap
	r := newRig(t, o)
	foot0 := r.set.Counters().UsedBytes

	q := rq()
	q.Bytes = 3*testCap - 4096
	rs := r.reserve(t, q)
	defer rs.Release()
	held := r.pool.Counters().Held
	if held == 0 {
		t.Fatal("the reservation holds no spare")
	}
	// The maker keeps the content target ready beside every held spare.
	for deadline := time.Now().Add(5 * time.Second); r.pool.Counters().Spares < 5+held; time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("the pool never made up its target beside %d held spares: %+v", held, r.pool.Counters())
		}
	}
	growth := r.set.Counters().UsedBytes - foot0
	if want := held * testCap; growth != want {
		t.Fatalf("the footprint grew by %d, want the %d held spares' %d", growth, held, want)
	}
	if got := r.c.reservedAsk; got != 0 {
		t.Fatalf("the journal-side ask = %d beside a footprint that grew by %d, want 0: the held spares are charged once, when made", got, growth)
	}
}

// TestReserve_IndexRefusesByPieces checks a reservation whose pieces would
// pass the index's bound is refused naming the index, counting the pieces.
func TestReserve_IndexRefusesByPieces(t *testing.T) {
	o := std()
	o.indexBound = 10
	r := newRig(t, o)

	q := rq()
	q.Writes = 5 // 2*5 + 1 pieces of 4096 bytes in 64 KiB caps
	ns := r.refusedReserve(t, q, core.LimitIndex)
	if want := int64(11) - 10; ns.Missing != want || ns.Unit != core.UnitEntries {
		t.Fatalf("the index's refusal = %d %s, want %d entries", ns.Missing, ns.Unit, want)
	}
}

// TestReserve_TakeCannotSpendHeldEntries checks a plain write is refused by
// the index when its own entry would take one an open reservation holds.
func TestReserve_TakeCannotSpendHeldEntries(t *testing.T) {
	o := std()
	o.indexBound = 3
	r := newRig(t, o)

	// The reservation holds the bound's every entry; a plain write's own
	// entry would be one past it.
	rs := r.reserve(t, rq())
	defer rs.Release()
	if held := r.c.heldEntries; held != 3 {
		t.Fatalf("the reservation holds %d entries, want the bound's 3", held)
	}
	ns := r.refusedWrite(t, req(), core.LimitIndex)
	if ns.Missing != 1 || ns.Unit != core.UnitEntries {
		t.Fatalf("the plain write's index refusal = %d %s, want one entry", ns.Missing, ns.Unit)
	}
}

// TestReserve_TakeCannotSpendHeldVersions checks a plain write is refused by
// the version limit when the versions left serve an open reservation alone.
func TestReserve_TakeCannotSpendHeldVersions(t *testing.T) {
	r := newRig(t, std())
	if err := r.ctr.SetVersionLimit(core.Version{Counter: 3}); err != nil {
		t.Fatalf("SetVersionLimit: %v", err)
	}

	q := rq()
	q.Writes = 2 // the two versions left
	rs := r.reserve(t, q)
	defer rs.Release()

	ns := r.refusedWrite(t, req(), core.LimitVersion)
	if ns.Missing != 1 || ns.Unit != core.UnitVersions {
		t.Fatalf("the plain write's version refusal = %d %s, want one version", ns.Missing, ns.Unit)
	}
}

// TestReserve_VersionRefuses checks a reservation of more versions than the
// counter has left is refused naming the version limit.
func TestReserve_VersionRefuses(t *testing.T) {
	o := std()
	o.noVersions = true
	r := newRig(t, o)

	q := rq()
	q.Writes = 4
	ns := r.refusedReserve(t, q, core.LimitVersion)
	if ns.Missing != 4 || ns.Unit != core.UnitVersions {
		t.Fatalf("the version refusal = %d %s, want four versions", ns.Missing, ns.Unit)
	}
}

// TestReserve_SpareRefusalHoldsNothing checks a reservation the pool cannot
// serve is refused naming the spares, holding no byte, entry, version or
// spare.
func TestReserve_SpareRefusalHoldsNothing(t *testing.T) {
	o := std()
	o.unprimed, o.spares = true, 0
	r := newRig(t, o)

	ns := r.refusedReserve(t, rq(), core.LimitSpare)
	if _, spares := PiecesFor(4096, 1, testCap); ns.Missing != int64(spares) {
		t.Fatalf("the spare refusal = %d segments, want the ask's %d", ns.Missing, spares)
	}
	if got := r.c.heldEntries; got != 0 {
		t.Fatalf("after the spare refusal, heldEntries = %d, want 0", got)
	}
	if got := r.c.heldVersions; got != 0 {
		t.Fatalf("after the spare refusal, heldVersions = %d, want 0", got)
	}
	if got := r.c.reservedAsk; got != 0 {
		t.Fatalf("after the spare refusal, the journal-side ask = %d, want 0", got)
	}
}

// TestReserve_AskDoesNotShrinkTheRoomForASeat checks a plain write whose piece
// needs a segment no ready spare covers is refused naming the journal's maximum
// by exactly what the segments it needs lack, an open reservation's ask adding
// nothing to it: the reservation's room is in the spares it holds, which the
// footprint already counts.
func TestReserve_AskDoesNotShrinkTheRoomForASeat(t *testing.T) {
	o := std()
	o.unprimed, o.spares = true, 2 // the plain write's every piece rolls onto a spare
	r := newRig(t, o)

	q := req()
	q.Payload = segment.EmptyRoom(testCap) + 50 // two pieces, both on spares
	// The maximum leaves a byte less than the two segments beside the footprint.
	r.c.cfg.MaxBytes = r.set.Counters().UsedBytes + 2*testCap - 1

	// While nothing is reserved the plain write is admitted: the room is
	// there for its two spares.
	r.grant(t, q).Release()

	// Beside the reservation no spare is left for the write, so its pieces need
	// segments of their own, two caps against the room the maximum leaves.
	rs := r.reserve(t, rq())
	defer rs.Release()
	ns := r.refusedWrite(t, q, core.LimitJournal)
	if ns.Missing != 1 || ns.Unit != core.UnitBytes {
		t.Fatalf("the plain write's refusal = %d %s, want the one byte the two segments lack, not that and the reservation's ask", ns.Missing, ns.Unit)
	}
}

// TestReserve_CallerBugsAreRefused checks a reservation that names no byte or
// no write is refused as a caller's bug, consulting no limit and holding
// nothing.
func TestReserve_CallerBugsAreRefused(t *testing.T) {
	r := newRig(t, std())

	for _, q := range []ReserveRequest{
		{Tag: tag1, Bytes: 0, Writes: 1},
		{Tag: tag1, Bytes: -1, Writes: 1},
		{Tag: tag1, Bytes: 4096, Writes: 0},
		{Tag: tag1, Bytes: 4096, Writes: -1},
	} {
		rs, err := r.c.Reserve(context.Background(), q)
		if !errors.Is(err, core.ErrInvalidExtent) {
			t.Fatalf("Reserve(%+v) = %v, want ErrInvalidExtent", q, err)
		}
		if rs != nil {
			t.Fatalf("Reserve(%+v) returned a reservation beside its refusal", q)
		}
		if got := r.c.ReservedBytes(); got != 0 {
			t.Fatalf("Reserve(%+v) held %d bytes, want 0", q, got)
		}
		if got := r.pool.Counters().Held; got != 0 {
			t.Fatalf("Reserve(%+v) held %d spares, want 0", q, got)
		}
	}
}

// TestReserve_OrderOfRefusals drives one ask several limits refuse and checks
// the first standing names itself: the read-only sentinel, version before
// index, and share before the spare pool.
func TestReserve_OrderOfRefusals(t *testing.T) {
	// Read-only first: the sentinel itself, nothing held, no spare asked.
	o := std()
	o.noVersions, o.indexBound, o.maxBytes = true, 1, 1
	r := newRig(t, o)
	r.dur.Enter(errors.New("the directory turned read-only"))
	q := rq()
	q.ShareLimit = 1
	if _, err := r.c.Reserve(context.Background(), q); !errors.Is(err, core.ErrJournalReadOnly) {
		t.Fatalf("a read-only journal refused with %v, want ErrJournalReadOnly", err)
	}
	if got := r.c.ReservedBytes(); got != 0 {
		t.Fatalf("a read-only refusal held %d bytes, want 0", got)
	}
	if got := r.pool.Counters().Held; got != 0 {
		t.Fatalf("a read-only refusal asked for %d holds, want 0", got)
	}

	// Version before index and share.
	o = std()
	o.noVersions, o.indexBound = true, 1
	r = newRig(t, o)
	q = rq()
	q.ShareLimit = 1
	_ = r.refusedReserve(t, q, core.LimitVersion)

	// Index before share and journal.
	o = std()
	o.indexBound, o.maxBytes = 1, 1
	r = newRig(t, o)
	q = rq()
	q.ShareLimit = 1
	_ = r.refusedReserve(t, q, core.LimitIndex)

	// Journal before the spare pool.
	o = std()
	o.maxBytes = 1
	r = newRig(t, o)
	_ = r.refusedReserve(t, rq(), core.LimitJournal)

	// Share before the spare pool: the pool is dry, and the share's limit is
	// what a reservation over it is refused by.
	o = std()
	o.spares = 0
	r = newRig(t, o)
	r.seed(t, 896)
	rqOver := rq()
	rqOver.ShareLimit = 1
	_ = r.refusedReserve(t, rqOver, core.LimitShare)
}

// TestReservationDrawsFitTheReservation checks writes of unequal sizes every
// draw succeed when the ask covers each one's worst case, and a further draw is
// refused: the arithmetic Draw sizes by and the one Reserve charged agree.
func TestReservationDrawsFitTheReservation(t *testing.T) {
	r := newRig(t, std())
	const writes = 8
	sizes := []int64{4096, 8, 16384, 24, 512, 64, 8192, 16}
	var ask int64
	for _, p := range sizes {
		ask += WriteCharge(p, testCap)
	}
	_, spares := PiecesFor(ask, writes, testCap)
	r.awaitReady(t, spares)

	rs := r.reserve(t, ReserveRequest{Tag: tag1, Bytes: ask, Writes: writes})
	defer rs.Release()
	for i, p := range sizes {
		g, err := rs.Draw(p)
		if err != nil {
			t.Fatalf("Draw %d (%d payload bytes) = %v", i+1, p, err)
		}
		// The write lands every byte its grant covers, as a real append
		// hands each piece over, and the grant returns nothing.
		g.Wrote(g.bytes)
		g.Release()
	}
	// The reservation is spent to the byte: what it charged each write is what
	// the draw charges it, in both directions.
	if got := rs.Remaining(); got != 0 {
		t.Fatalf("after every write, Remaining = %d, want the ask spent whole", got)
	}
	if _, err := rs.Draw(8); !errors.Is(err, core.ErrReservationExhausted) {
		t.Fatalf("the draw past the reservation = %v, want ErrReservationExhausted", err)
	}
}

// TestReservationReleaseThenGrantReleaseReturnsEveryCounter releases a
// reservation, then the grant a write still held, and checks nothing leaks:
// the counters come back to zero, the held figures do, and a second release is
// a no-op.
func TestReservationReleaseThenGrantReleaseReturnsEveryCounter(t *testing.T) {
	r := newRig(t, std())
	rs := r.reserve(t, rq())

	g, err := rs.Draw(1024)
	if err != nil {
		t.Fatalf("Draw: %v", err)
	}
	// The production loop hands over what the index admitted for the piece
	// it published; one piece is one entry here.
	g.Claimed(1)
	g.Wrote(1024)
	rs.Release()
	g.Release()
	rs.Release() // idempotent

	if got := r.c.ReservedBytes(); got != 0 {
		t.Fatalf("ReservedBytes = %d after the release, want 0", got)
	}
	if got := r.c.reservedAsk; got != 0 {
		t.Fatalf("the journal-side ask = %d after the release, want 0", got)
	}
	if got := r.c.heldEntries; got != 0 {
		t.Fatalf("heldEntries = %d after the release, want 0", got)
	}
	if got := r.c.heldVersions; got != 0 {
		t.Fatalf("heldVersions = %d after the release, want 0", got)
	}
	if got := r.pool.Counters().Held; got != 0 {
		t.Fatalf("the pool holds %d spares after the release, want 0", got)
	}
}

// TestDrawChargesTheStraddle checks a draw charges a write more than its
// roomless split, so a record that straddles a partly filled segment is inside
// the grant and Wrote hands the share's tally every byte it really appended:
// the tally rises by the two records the straddle cost, not by the shorter
// roomless figure a clamp would leave.
func TestDrawChargesTheStraddle(t *testing.T) {
	r := newRig(t, std())
	rs := r.reserve(t, ReserveRequest{Tag: tag1, Bytes: 4304, Writes: 1})
	defer rs.Release()

	g, err := rs.Draw(4096)
	if err != nil {
		t.Fatalf("Draw: %v", err)
	}
	if want := int64(4304); g.bytes != want {
		t.Fatalf("the draw charged %d record bytes, want the write's worst case %d", g.bytes, want)
	}
	// The write straddles: a head in a small room and the tail beyond it, the
	// two records adding up to more than the roomless split but less than the
	// grant.
	head, tail := segment.RecordBytes(64), segment.RecordBytes(4096-64)
	if head+tail > g.bytes {
		t.Fatalf("the straddle costs %d, more than the grant's %d", head+tail, g.bytes)
	}
	before := r.c.shareTally[tag1]
	g.Wrote(head)
	g.Wrote(tail)
	if got, want := r.c.shareTally[tag1]-before, head+tail; got != want {
		t.Fatalf("the share's tally rose by %d, want the straddle's %d", got, want)
	}
}

// TestReservationDrawsUnclaimedEntriesStayHeld checks a drawn entry a write
// never published stays held while the reservation is open, and its Release
// returns it: the bound never gains room a released grant's unclaimed entry
// would otherwise free. It drives the hand-over directly, as the append loop
// does per piece; the production path is TestReserveAhead_EntriesHandedOverAsPublished
// in datapath.
func TestReservationDrawsUnclaimedEntriesStayHeld(t *testing.T) {
	r := newRig(t, std())
	pieces, _ := PiecesFor(4304, 1, testCap)
	rs := r.reserve(t, ReserveRequest{Tag: tag1, Bytes: 4304, Writes: 1})
	if held := r.c.heldEntries; held != int64(pieces) {
		t.Fatalf("heldEntries = %d after the reservation, want its %d", held, pieces)
	}

	g, err := rs.Draw(1024)
	if err != nil {
		t.Fatalf("Draw: %v", err)
	}
	// The write appends and publishes one piece and leaves the rest: the grant
	// returns its unclaimed entries to the reservation, which still holds them.
	g.Claimed(1)
	g.Wrote(1024)
	g.Release()
	if held, left := r.c.heldEntries, rs.leftEntries; held != left || held == 0 {
		t.Fatalf("after the grant returned its entries, heldEntries = %d and leftEntries = %d, want both the unclaimed figure", held, left)
	}

	rs.Release()
	if held := r.c.heldEntries; held != 0 {
		t.Fatalf("after Release, heldEntries = %d, want 0", held)
	}
}

// TestReservationSizedByRecordBytesSpendsItsLastWrite checks a reservation the
// caller sized by the record bytes its writes append in room — the figure
// RFC 1 §6's n names — lets every one of them draw: the last write finds less
// than its worst-case charge and draws it, since what remains still covers the
// write's own split, and it must not be refused.
//
// decision: a caller sizing n by the payload bytes it copies is short of each
// write's record overhead, so a later write, at the latest its last, is refused
// core.ErrReservationExhausted. The engine's clone sizes n that way (the source
// bytes it reads), while RFC 1 §6 names n as record bytes; that mismatch is
// open — RFC 8 §9.1 step 1 says "the source bytes" — and is raised with the
// RFC authors. The real-journal clone path is never reached today: the engine
// conformance checks that would run it fail earlier, at the store's own
// "vfs: retry shortly: commit", so the mismatch is hidden rather than absent.
func TestReservationSizedByRecordBytesSpendsItsLastWrite(t *testing.T) {
	r := newRig(t, std())
	const writes = 4
	const payload = 1
	// The caller's n: each write's record cost where its stream has room.
	n := int64(writes) * segment.RecordBytes(payload)
	rs := r.reserve(t, ReserveRequest{Tag: tag1, Bytes: n, Writes: writes})
	defer rs.Release()

	for i := range writes {
		g, err := rs.Draw(payload)
		if err != nil {
			t.Fatalf("Draw %d of %d = %v, want the write drawn: a reservation sized by its writes' record bytes covers them", i+1, writes, err)
		}
		// The write lands exactly its record, as one in room does.
		g.Wrote(segment.RecordBytes(payload))
		g.Release()
	}
	if got := rs.Remaining(); got != 0 {
		t.Fatalf("after every write, Remaining = %d, want the reservation spent", got)
	}
	if _, err := rs.Draw(payload); !errors.Is(err, core.ErrReservationExhausted) {
		t.Fatalf("the draw past the reservation = %v, want ErrReservationExhausted", err)
	}
}

// piecesOfWrite returns the pieces a write of payload bytes appends when its
// first record finds room payload bytes of room and every later one its own
// empty segment: the head the room holds, then one piece per empty segment's
// payload, the last holding the tail. It is the split the stream runs, retold
// here from the format's own figures so it agrees with no code under test.
func piecesOfWrite(payload, room, cap int64) int {
	empty := segment.EmptyRoom(cap)
	rest, pieces := payload, 0
	for rest > 0 {
		n := min(rest, empty)
		if pieces == 0 && room > 0 && n > room {
			n = room
		}
		pieces++
		rest -= n
	}
	return pieces
}

// TestPieceCountCoversEveryWrite checks R84's count against the split a write
// really makes: a write of any size up to three caps, starting at any offset
// mod the cap, appends no more pieces than PiecesFor counts for one write — so
// the footer entries and index entries a reservation takes cover every piece
// its write publishes, and none is refused part-way by an under-reserved
// count.
func TestPieceCountCoversEveryWrite(t *testing.T) {
	const cap = int64(4 << 10)
	pieces, _ := PiecesFor(3*cap, 1, cap)
	// Every offset a record can start at in a segment, from an empty one to
	// one whose tail holds nothing: the room the write's head finds is the
	// stream's own figure for that offset.
	for next := int64(format.SegmentHeaderSize); next <= cap; next += 8 {
		room := segment.WriteRoom(cap, next, 0)
		for size := int64(1); size <= 3*cap; size++ {
			want, _ := PiecesFor(size, 1, cap)
			if got := piecesOfWrite(size, room, cap); got > want {
				t.Fatalf("a %d-byte write starting at offset %d appends %d pieces, over the %d PiecesFor counts for one write of up to three caps",
					size, next, got, want)
			}
		}
	}
	if pieces < 2 {
		t.Fatalf("PiecesFor counts %d pieces for a write of three caps, want at least its head and its tail", pieces)
	}
}

// TestReservationRefusesAWritePastItsVersions checks a reservation of fewer
// writes refuses a further write that its remaining bytes could cover but its
// versions cannot, naming the version shortfall and drawing no byte: a caller
// that reserved for one write cannot draw two.
func TestReservationRefusesAWritePastItsVersions(t *testing.T) {
	r := newRig(t, std())
	rs := r.reserve(t, ReserveRequest{Tag: tag1, Bytes: 4 * WriteCharge(1, testCap), Writes: 1})
	defer rs.Release()

	g, err := rs.Draw(1)
	if err != nil {
		t.Fatalf("the reservation's one write: %v", err)
	}
	// The grant returns its unlanded bytes, so the second draw is short of a
	// version, not of bytes.
	g.Release()
	if got := rs.Remaining(); got < recordSplit(1, 0, testCap) {
		t.Fatalf("Remaining = %d after the one write returned its bytes, want at least a write's records", got)
	}
	_, err = rs.Draw(1)
	if !errors.Is(err, core.ErrReservationExhausted) || !strings.Contains(err.Error(), "versions") {
		t.Fatalf("the write past the reservation's one version = %v, want ErrReservationExhausted naming the versions", err)
	}
}

// TestDrawClampedStraddleKeepsTheShareTallyExact checks a reserved write that
// straddles a partly filled segment on the reservation's last byte appends more
// than the reservation still holds: the grant clamps what leaves the
// reservation, but the share's tally takes the record bytes the segment really
// counts, so a retirement reconciles it to nothing rather than leaving it
// permanently low.
func TestDrawClampedStraddleKeepsTheShareTallyExact(t *testing.T) {
	// A stream whose active segment leaves a tail too small for the write's
	// first record to fill whole: the write's head takes the tail and its tail
	// rolls into the next segment, so the write appends two records.
	const payload = 4096
	o := std()
	o.room = 2048 // a tail the write's head fills and its other 2048 bytes roll past
	r := newRig(t, o)
	head := r.room()
	if head <= 0 || head >= payload {
		t.Fatalf("precondition: the stream's room is %d, want a partly filled segment under the %d-byte write", head, payload)
	}
	straddle := segment.RecordBytes(head) + segment.RecordBytes(payload-head)
	// A reservation of the write's roomless split: less than the straddle
	// costs, so the grant's own charge is under what the records append and
	// Wrote clamps.
	rs := r.reserve(t, ReserveRequest{Tag: tag1, Bytes: segment.RecordBytes(payload), Writes: 1})
	defer rs.Release()

	g, err := rs.Draw(payload)
	if err != nil {
		t.Fatalf("Draw: %v", err)
	}
	if g.bytes >= straddle {
		t.Fatalf("the draw charged %d record bytes, want fewer than the straddle's %d, so the grant clamps", g.bytes, straddle)
	}
	// Append the straddle piece by piece, as the datapath's loop does, keeping
	// the segment each piece landed in.
	var segs []core.SegmentID
	for done := int64(0); done < payload; {
		pl := r.append(t, segment.Roll{Hold: g.Hold()}, tag1, payload-done)
		g.Wrote(segment.RecordBytes(pl.N))
		done += pl.N
		segs = append(segs, pl.Loc.Segment)
	}
	g.Release()

	// The share's tally is the record bytes the set counts for the tag: the
	// whole straddle, not the clamped charge.
	if got, want := r.c.shareTally[tag1], r.set.ShareBytes(tag1); got != want {
		t.Fatalf("the share's tally = %d, the set counts %d for the tag; want them equal after a clamped straddle", got, want)
	}
	if got := r.set.ShareBytes(tag1); got != straddle {
		t.Fatalf("the set counts %d record bytes for the tag, want the straddle's %d", got, straddle)
	}
	// Retiring the segment the write's head filled lowers the tally by the
	// head's record bytes with the set's own figure, so the two still agree:
	// the clamped header left nothing behind that a retirement cannot clear.
	if len(segs) < 2 || segs[0] == segs[1] {
		t.Fatalf("the straddle landed in %v, want two segments", segs)
	}
	r.seal(t)
	r.retire(t, segs[0])
	r.c.Retired(tag1)
	if got, want := r.c.shareTally[tag1], r.set.ShareBytes(tag1); got != want {
		t.Fatalf("after retiring the head's segment the tally = %d, the set counts %d; want them equal", got, want)
	}
}

// TestCountersReturnToRest checks every counter a take or draw can move —
// reserved bytes, held and pending entries, held versions, seats and the room
// granted — comes back to zero after each path that ends a grant without the
// write publishing everything: a refused take, a take released unused, a
// partial publish that fails on a later piece, and a reservation released with
// its undrawn figures.
func TestCountersReturnToRest(t *testing.T) {
	rest := func(t *testing.T, r *rig, what string) {
		t.Helper()
		if got := r.c.reserved; got != 0 {
			t.Fatalf("%s: reserved = %d, want 0", what, got)
		}
		if got := r.c.heldEntries; got != 0 {
			t.Fatalf("%s: heldEntries = %d, want 0", what, got)
		}
		if got := r.c.pendingEntries; got != 0 {
			t.Fatalf("%s: pendingEntries = %d, want 0", what, got)
		}
		if got := r.c.heldVersions; got != 0 {
			t.Fatalf("%s: heldVersions = %d, want 0", what, got)
		}
		if got := r.c.pendingSeats; got != 0 {
			t.Fatalf("%s: pendingSeats = %d, want 0", what, got)
		}
		if got := r.c.reservedAsk; got != 0 {
			t.Fatalf("%s: reservedAsk = %d, want 0", what, got)
		}
		for stream, room := range r.c.grantedRoom {
			if room != 0 {
				t.Fatalf("%s: grantedRoom[%d] = %d, want 0", what, stream, room)
			}
		}
	}

	// A refused take holds nothing: the share limit refuses the write whole.
	o := std()
	o.spares = 0
	r := newRig(t, o)
	q := req()
	q.ShareLimit = 1
	_ = r.refusedWrite(t, q, core.LimitShare)
	rest(t, r, "a refused take")

	// A take released unused holds nothing: its pending entries go back.
	r = newRig(t, std())
	g := r.grant(t, req())
	g.Release()
	rest(t, r, "a take released unused")

	// A write published in part and then failed: the entry its first piece
	// claimed is the index's, and the entry it never published comes back to
	// pendingEntries, which Release then clears.
	r = newRig(t, std())
	g = r.grant(t, req())
	if g.heldEntries < 1 {
		t.Fatalf("the take holds %d entries, want at least the entry its piece publishes", g.heldEntries)
	}
	g.Claimed(1) // the first piece published
	g.Release()  // the write failed before publishing the rest
	rest(t, r, "a partial publish")

	// A fill's take: it publishes one entry per gap piece too, so its pending
	// entries go back at release as a write's do.
	r = newRig(t, std())
	q = req()
	q.Fill = true
	g = r.grant(t, q)
	g.Release()
	rest(t, r, "a fill released unused")

	// A take the pool refuses after it held its entries: the refused-Hold path
	// gives the pending entries back, since no grant is built to release them.
	r = newRig(t, rigOpts{spares: 2, unprimed: true}) // a roomless ask needs spares
	r.failHolds(&core.NoSpaceError{Limit: core.LimitSpare, Missing: 1, Unit: core.UnitSegments}, 1)
	q = req()
	_, err := r.c.Take(context.Background(), q)
	var ns *core.NoSpaceError
	if !errors.As(err, &ns) || ns.Limit != core.LimitSpare {
		t.Fatalf("the take beside a refused hold = %v, want a refusal naming the spare", err)
	}
	rest(t, r, "a take the pool refused")

	// A drawn grant published in part and then released: the entry it claimed
	// is the index's, the rest go back to the reservation, and the reservation's
	// release returns what it still holds.
	r = newRig(t, std())
	rs2 := r.reserve(t, ReserveRequest{Tag: tag1, Bytes: 2 * WriteCharge(1, testCap), Writes: 2})
	gd2, err := rs2.Draw(1)
	if err != nil {
		t.Fatalf("Draw: %v", err)
	}
	gd2.Claimed(1) // one piece published
	gd2.Release()  // the write failed before publishing the rest
	rs2.Release()
	rest(t, r, "a drawn grant published in part")

	// A reservation released with its undrawn figures: bytes, entries, versions
	// and its journal-side ask return whole.
	r = newRig(t, std())
	rs := r.reserve(t, ReserveRequest{Tag: tag1, Bytes: 4 * WriteCharge(1, testCap), Writes: 4})
	gd, err := rs.Draw(1)
	if err != nil {
		t.Fatalf("Draw: %v", err)
	}
	gd.Release()
	rs.Release()
	rest(t, r, "a reservation released")
}

// TestReserve_ManySmallWritesKeepTheExcessCharged checks the part of a
// reservation's ask its held spares do not cover stays charged beside the
// footprint: a thousand one-byte writes carry a record header and a footer
// entry each, so their ask outruns the spares PiecesFor sizes, and the
// excess is exactly what a plain write's journal headroom loses until Release.
func TestReserve_ManySmallWritesKeepTheExcessCharged(t *testing.T) {
	o := std()
	o.spares = 8
	r := newRig(t, o)
	const writes = 1000
	q := rq()
	q.Bytes, q.Writes = writes*WriteCharge(1, testCap), writes
	pieces, spares := PiecesFor(q.Bytes, writes, testCap)
	ask := q.Bytes + int64(pieces)*format.CatalogEntrySize
	wantCharged := ask - int64(spares)*(testCap-format.SegmentHeaderSize)
	if wantCharged <= 0 {
		t.Fatalf("precondition: the ask %d fits its %d spares, want an excess", ask, spares)
	}

	rs := r.reserve(t, q)
	defer rs.Release()
	if got := r.c.reservedAsk; got != wantCharged {
		t.Fatalf("the journal-side ask = %d, want the %d the %d held spares do not cover", got, wantCharged, spares)
	}

	// The maximum leaves a byte less than one segment beside the footprint and
	// the excess: the plain write's one uncovered seat misses by that byte.
	r.c.cfg.MaxBytes = r.set.Counters().UsedBytes + wantCharged + testCap - 1
	w := req()
	pc := r.pool.Counters()
	w.Payload = r.room() + (pc.Spares-pc.Held)*segment.EmptyRoom(testCap) + 50 // one spare past those free
	w.Extent = core.Extent{Len: w.Payload}
	ns := r.refusedWrite(t, w, core.LimitJournal)
	if ns.Missing != 1 || ns.Unit != core.UnitBytes {
		t.Fatalf("the plain write's refusal = %d %s, want the one byte past the excess's %d", ns.Missing, ns.Unit, wantCharged)
	}
}
