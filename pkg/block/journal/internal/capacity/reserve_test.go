package capacity

import (
	"context"
	"errors"
	"math"
	"testing"

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
	if got := r.c.reservedAsk; got != 4096+int64(pieces)*format.CatalogEntrySize {
		t.Fatalf("the journal-side ask = %d, want the bytes and their %d footer entries", got, pieces)
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

// TestReserve_JournalRoomHeldAcrossReservations checks two reservations that
// both read one footprint's remaining room cannot both be taken: the room a
// reservation names is in segments not yet allocated, so the first's ask is
// charged beside the footprint for the second to find spent.
func TestReserve_JournalRoomHeldAcrossReservations(t *testing.T) {
	r := newRig(t, std())
	pieces, _ := PiecesFor(4096, 1, testCap)
	one := int64(4096 + pieces*format.CatalogEntrySize)
	r.c.cfg.MaxBytes = r.set.Counters().UsedBytes + one

	rs := r.reserve(t, rq())
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

	rs.Release()
	r.reserve(t, rq()).Release()
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

// TestReserve_JournalRoomCannotBeSpentBySpares checks a plain write that needs
// a segment no ready spare covers is refused naming the journal's maximum once
// a reservation holds the ask's bytes: the room the reservation names is in
// segments not yet allocated, and the write's own segment has none left.
func TestReserve_JournalRoomCannotBeSpentBySpares(t *testing.T) {
	o := std()
	o.unprimed, o.spares = true, 2 // the plain write's every piece rolls onto a spare
	r := newRig(t, o)

	pieces, spares := PiecesFor(4096, 1, testCap)
	ask := int64(4096 + pieces*format.CatalogEntrySize)
	if spares != 2 {
		t.Fatalf("the reservation needs %d spares, want the pool's own 2", spares)
	}

	q := req()
	q.Payload = segment.EmptyRoom(testCap) + 50 // two pieces, both on spares
	// The maximum holds the footprint, the reservation's ask and a byte less
	// than the two caps the plain write's spares need.
	spareCaps := int64(spares) * testCap
	r.c.cfg.MaxBytes = r.set.Counters().UsedBytes + ask + spareCaps - 1

	// While nothing is reserved the plain write is admitted: the room is
	// there for its two spares.
	r.grant(t, q).Release()

	// The same write beside the reservation: the byte the reservation holds
	// is the byte the write's second cap lacks.
	rs := r.reserve(t, rq())
	defer rs.Release()
	ns := r.refusedWrite(t, q, core.LimitJournal)
	if ns.Missing != 1 || ns.Unit != core.UnitBytes {
		t.Fatalf("the plain write's refusal = %d %s, want the one byte the reservation holds", ns.Missing, ns.Unit)
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

// TestReservationReleaseRacesADraw releases a reservation under a grant an
// in-flight write still holds, and checks nothing leaks: the counters come back
// to zero, the held figures do, and a second release is a no-op.
func TestReservationReleaseRacesADraw(t *testing.T) {
	r := newRig(t, std())
	rs := r.reserve(t, rq())

	g, err := rs.Draw(1024)
	if err != nil {
		t.Fatalf("Draw: %v", err)
	}
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
// would otherwise free.
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
