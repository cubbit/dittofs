package datapath

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/capacity"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
)

// reserve takes a reservation of bytes in writes through the core and fails the
// test on any error.
func reserve(t *testing.T, c *Core, s *Share, bytes int64, writes int) *capacity.Reservation {
	t.Helper()
	r, err := c.Reserve(context.Background(), s, bytes, writes)
	if err != nil {
		t.Fatalf("Reserve(%d, %d): %v", bytes, writes, err)
	}
	return r
}

// TestReserveAhead_WritePastTheShareLimit checks a reserved write lands after
// the share is at its limit — the reservation's own bytes are what it spends —
// while a plain write of the same size is refused naming the share, and Close
// returns every byte the write did not use.
func TestReserveAhead_WritePastTheShareLimit(t *testing.T) {
	c, _ := newTestCore(t)
	s := mustShare(t, c, 1)
	const payload = 4096
	cost := capacity.WriteCharge(payload, testSegmentCap) // one write's record bytes

	// A warm-up record, so the share has a settled figure beside the limit.
	if _, err := c.WriteAt(context.Background(), s, testFile(9), 0, []byte("w"), 1); err != nil {
		t.Fatalf("the warm-up write: %v", err)
	}
	if _, err := c.Share(1, recordBytes(1)+cost, core.Version{}); err != nil {
		t.Fatalf("Share(1, %d): %v", recordBytes(1)+cost, err)
	}

	r := reserve(t, c, s, cost, 1)
	defer r.Release()
	if got := c.ReservedBytes(); got != cost {
		t.Fatalf("ReservedBytes = %d after the reservation, want its %d record bytes", got, cost)
	}

	// The plain write is refused: the reservation holds the share's last room.
	// The missing amount is its record bytes over that room, counted in bytes.
	_, err := c.WriteAt(context.Background(), s, testFile(2), 0, make([]byte, payload), 1)
	ns := refused(t, err, core.LimitShare)
	if ns.Missing <= 0 || ns.Unit != core.UnitBytes {
		t.Fatalf("the plain write's refusal = %d %s, want a positive byte shortfall", ns.Missing, ns.Unit)
	}

	// The reserved write lands, though the share is at its limit.
	if _, err := c.WriteReserved(context.Background(), s, r, testFile(3), 0, make([]byte, payload), 1); err != nil {
		t.Fatalf("the reserved write at the share's limit: %v", err)
	}

	r.Release()
	if got := c.ReservedBytes(); got != 0 {
		t.Fatalf("after Release, ReservedBytes = %d, want 0", got)
	}
}

// TestReserveAhead_ReleaseReturnsTheUnused checks a reservation's Release gives
// back every byte, entry, version and spare its writes never drew.
func TestReserveAhead_ReleaseReturnsTheUnused(t *testing.T) {
	c, _ := newTestCore(t)
	s := mustShare(t, c, 1)
	r := reserve(t, c, s, 10*capacity.WriteCharge(4096, testSegmentCap), 10)
	if held := c.pool.Counters().Held; held == 0 {
		t.Fatal("the reservation holds no spare")
	}
	if _, err := c.WriteReserved(context.Background(), s, r, testFile(1), 0, make([]byte, 4096), 1); err != nil {
		t.Fatalf("the reservation's first write: %v", err)
	}
	spent := c.ReservedBytes()
	if spent <= 0 || spent >= 10*capacity.WriteCharge(4096, testSegmentCap) {
		t.Fatalf("ReservedBytes after one of ten writes = %d, want between 0 and %d", spent, 10*capacity.WriteCharge(4096, testSegmentCap))
	}
	// The draw charged the write its worst case; the append landed in room and
	// spent less, and the unlanded charge came back to the reservation.
	if got, want := r.Remaining(), 9*capacity.WriteCharge(4096, testSegmentCap)+capacity.WriteCharge(4096, testSegmentCap)-recordBytes(4096); got != want {
		t.Fatalf("Remaining after one of ten writes = %d, want %d", got, want)
	}
	r.Release()
	if got := c.ReservedBytes(); got != 0 {
		t.Fatalf("after Release, ReservedBytes = %d, want 0", got)
	}
	waitIn(t, 5*time.Second, "the pool to return the reservation's held spares", func() bool {
		return c.pool.Counters().Held == 0
	})
	// The reservation's journal-side ask is given back whole: a second
	// reservation of the same size is taken.
	reserve(t, c, s, 10*capacity.WriteCharge(4096, testSegmentCap), 10).Release()
}

// TestReserveAhead_PastTheIndexBound checks ten thousand small reserved writes
// all land beside a writer holding the index one entry below ExtentLimit: the
// reservation holds every entry, a plain write of any size is refused naming
// the index, and the reservation's own draws spend the held entries without the
// bound consulting itself again.
func TestReserveAhead_PastTheIndexBound(t *testing.T) {
	const writes = 10_000
	cost := int64(writes) * capacity.WriteCharge(1, testSegmentCap) // one-byte writes, their record bytes the reservation's figure
	pieces, spares := capacity.PiecesFor(cost, writes, testSegmentCap)
	c, _ := newTestCore(t, func(o *coreOptions) {
		// The bound is the reservation's count plus two: the one entry the
		// plain writer holds and the one its next write names, so ExtentLimit
		// sits one above what the writer holds — the brief's hard case.
		o.extentLimit = int64(pieces) + 1
		// The pool readies the spares the ask rolls into, as a production
		// pool at its target would, and one for the plain writer's piece.
		o.contentSpares = spares + 1
	})
	s := mustShare(t, c, 1)

	// A plain writer takes an entry before the reservation does, as a writer
	// landing just before it would.
	held := testFile(100)
	if _, err := c.WriteAt(context.Background(), s, held, 0, []byte("h"), 1); err != nil {
		t.Fatalf("the plain writer's warm write: %v", err)
	}

	r := reserve(t, c, s, cost, writes)
	defer r.Release()
	// The writer holds one entry and the reservation holds the rest: what is
	// left is what the reservation holds, and no more.
	if left := c.Index().EntriesLeft(); left != int64(pieces) {
		t.Fatalf("EntriesLeft beside the reservation = %d, want the reservation's %d and nothing more", left, pieces)
	}

	// The bound sits one entry above what the writer holds: the reservation's
	// every entry is held, so a plain write naming its own is refused.
	_, err := c.WriteAt(context.Background(), s, testFile(2), 0, []byte("p"), 1)
	_ = refused(t, err, core.LimitIndex)

	for i := 0; i < writes; i++ {
		f := core.NewFileID([16]byte{byte(i + 1), 0x7})
		if _, err := c.WriteReserved(context.Background(), s, r, f, 1<<20, []byte("r"), 1); err != nil {
			t.Fatalf("reserved write %d of %d: %v", i+1, writes, err)
		}
	}
}

// TestReserveAhead_BelowHalfTheHeadroom checks a reserved write lands with the
// headroom below half and the journal at its limit, where a plain write is
// refused: the reservation was taken while the journal still had the room
// Reserve charges an ask against the maximum.
func TestReserveAhead_BelowHalfTheHeadroom(t *testing.T) {
	c, _ := newTestCore(t,
		capacityCoreOption(6*testSegmentCap),
		func(o *coreOptions) { o.headroomRecords = acceptanceRecords },
	)
	s := mustShare(t, c, 1)
	// The reservation is taken with the journal below its limit: an ask names
	// room in segments not yet allocated, and the maximum holds none at it.
	r := reserve(t, c, s, capacity.WriteCharge(1, testSegmentCap), 1)
	defer r.Release()

	// The warm write fills the journal, and eleven headroom draws put its
	// drawing side below half. The headroom draws only while the journal is at
	// its limit, and the footprint counts the pool's spares: the warm write
	// rolls into a spare the maker refills on its own goroutine, so the draws
	// below are admitted only once the refill has landed and the footprint is
	// again past the maximum. Waiting on the limit itself — not a sleep — is
	// what makes every admission draw.
	if _, err := c.WriteAt(context.Background(), s, testFile(1), 0, make([]byte, roomAfter(0)+1), 1); err != nil {
		t.Fatalf("the warm write: %v", err)
	}
	waitIn(t, 5*time.Second, "the journal to reach its limit with the pool refilled", func() bool {
		return c.set.Counters().UsedBytes+testSegmentCap > 6*testSegmentCap
	})
	for i := 0; i < 11; i++ {
		mustAdmit(t, c, capacity.Release)
	}
	if !c.headroom.BelowHalf() {
		t.Fatal("eleven draws left the headroom above half")
	}

	// The plain write is refused; the reserved one lands under the same state,
	// no limit consulting itself again.
	_, err := c.WriteAt(context.Background(), s, testFile(3), 1<<20, []byte("p"), 1)
	if !isRefusal(err) {
		t.Fatalf("the plain write below half the headroom = %v, want a refusal", err)
	}
	if _, err := c.WriteReserved(context.Background(), s, r, testFile(4), 1<<20, []byte("r"), 1); err != nil {
		t.Fatalf("the reserved write below half the headroom: %v", err)
	}
}

// TestReserveAhead_ExhaustedRefusesBeforeWriting checks a draw past what the
// reservation holds is refused with ErrReservationExhausted, not a
// NoSpaceError, before any version is drawn or byte written.
func TestReserveAhead_ExhaustedRefusesBeforeWriting(t *testing.T) {
	c, _ := newTestCore(t)
	s := mustShare(t, c, 1)
	r := reserve(t, c, s, capacity.WriteCharge(4096, testSegmentCap), 1)
	defer r.Release()

	versionsBefore := c.counters.NextVersion()
	if _, err := c.WriteReserved(context.Background(), s, r, testFile(1), 0, make([]byte, 4096), 1); err != nil {
		t.Fatalf("the reservation's one write: %v", err)
	}
	f := testFile(2)
	_, err := c.WriteReserved(context.Background(), s, r, f, 0, []byte("x"), 1)
	if !isExhausted(err) {
		t.Fatalf("the write past the reservation = %v, want ErrReservationExhausted", err)
	}
	if _, ok := c.Index().Lookup(f, 0); ok {
		t.Fatal("the refused write published a piece")
	}
	if got := c.counters.NextVersion(); got.Counter != versionsBefore.Counter+1 {
		t.Fatalf("the refused write drew a version: counter moved %d -> %d", versionsBefore.Counter, got.Counter)
	}
}

// TestReserveAhead_ReservedAndPlainWriters runs reserved and plain writers at
// once over one journal and checks every reserved write lands, no reserved
// write is refused, and the counters stay consistent — the race detector's
// subject.
func TestReserveAhead_ReservedAndPlainWriters(t *testing.T) {
	c, _ := newTestCore(t)
	s := mustShare(t, c, 1)
	const writes = 16
	awaitSettledSpares(t, c, 2*testStreams)
	r := reserve(t, c, s, writes*capacity.WriteCharge(4096, testSegmentCap), writes)
	defer r.Release()

	var reserved, plain atomic.Int64
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < writes; i++ {
			if _, err := c.WriteReserved(context.Background(), s, r, core.NewFileID([16]byte{byte(i + 1), 0x7}), 0, make([]byte, 4096), 1); err != nil {
				t.Errorf("reserved write %d: %v", i, err)
				return
			}
			reserved.Add(1)
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < writes; i++ {
			// A plain writer may meet a limit; only the reserved writes are
			// promised free of refusal.
			if _, err := c.WriteAt(context.Background(), s, testFile(byte(100+i)), 0, make([]byte, 4096), 1); err == nil {
				plain.Add(1)
			}
		}
	}()
	wg.Wait()
	if got := reserved.Load(); got != writes {
		t.Fatalf("reserved writers landed %d of %d", got, writes)
	}
	// Exact figures, not inequalities a clamped read cannot fail. Every write's
	// grant is released by now, so nothing outside the reservation is reserved
	// or pending: ReservedBytes is exactly what its writes have not landed, and
	// the entries held back are exactly its own undrawn ones — a plain writer
	// leaked no pending entry and no earlier write left a held one behind.
	if got, want := c.ReservedBytes(), r.Remaining(); got != want {
		t.Fatalf("after both writers, ReservedBytes = %d, want the reservation's %d undrawn", got, want)
	}
	if got := c.PendingEntries(); got != 0 {
		t.Fatalf("after both writers, plain grants still hold %d pending entries, want 0", got)
	}
	if got, want := c.HeldEntries(), r.EntriesLeft(); got != want {
		t.Fatalf("after both writers, HeldEntries = %d, want the reservation's %d undrawn", got, want)
	}
}

// isRefusal reports whether err is a journal limit's refusal.
func isRefusal(err error) bool { return errors.As(err, new(*core.NoSpaceError)) }

// isExhausted reports whether err is a spent reservation's refusal.
func isExhausted(err error) bool { return errors.Is(err, core.ErrReservationExhausted) }

// TestReserveAhead_EntriesHeldUntilPublish checks a plain overwrite that would
// eat the entries a reservation holds is refused naming the index while a
// reserved write sits between its draw and its publish: the entries stay in the
// reservation's held figure until the publish claims them, so no plain write
// passes the bound's check in that window and leaves the reserved write to meet
// it at its publish, after its version is drawn and its record written.
func TestReserveAhead_EntriesHeldUntilPublish(t *testing.T) {
	const payload = 4096
	cost := capacity.WriteCharge(payload, testSegmentCap)
	pieces, spares := capacity.PiecesFor(cost, 1, testSegmentCap)

	// The reserved write is parked in the window between its draw and its
	// publish: the seam runs there, with the file's append domain held. It is
	// armed only once the warm write below has landed, so that write does not
	// park in it.
	var armed atomic.Bool
	parked, release := make(chan struct{}), make(chan struct{})
	c, _ := newTestCore(t, func(o *coreOptions) {
		o.extentLimit = int64(pieces) + 1
		o.contentSpares = spares + 1
		o.afterAppend = func() {
			if !armed.Load() {
				return
			}
			select {
			case <-parked:
			default:
				close(parked)
				<-release
			}
		}
	})
	s := mustShare(t, c, 1)

	// One entry of slack is held by a piece the plain overwrite would
	// supersede and retain: its adds eat the reservation's entries.
	shared := testFile(1)
	if _, err := c.WriteAt(context.Background(), s, shared, 0, make([]byte, payload), 1); err != nil {
		t.Fatalf("the warm write: %v", err)
	}

	r := reserve(t, c, s, cost, 1)
	defer r.Release()
	armed.Store(true)

	errs := make(chan error, 1)
	go func() {
		_, err := c.WriteReserved(context.Background(), s, r, testFile(2), 0, make([]byte, payload), 1)
		errs <- err
	}()
	<-parked

	// In the window: the plain overwrite is refused naming the index, and the
	// entries it would eat are still the reservation's.
	_, err := c.WriteAt(context.Background(), s, shared, 0, make([]byte, payload), 1)
	_ = refused(t, err, core.LimitIndex)
	close(release)
	if err := <-errs; err != nil {
		t.Fatalf("the reserved write beside the refused overwrite: %v", err)
	}
}

// TestReserveAhead_ConcurrentIndexBound runs a reserved writer and a plain
// writer that overwrites beside it under -race, with the index bound held by
// the reservation: every reserved write lands, every plain write that would eat
// a held entry is refused naming the index, and the count never passes the
// bound.
func TestReserveAhead_ConcurrentIndexBound(t *testing.T) {
	const writes = 32
	const payload = 512
	cost := capacity.WriteCharge(payload, testSegmentCap)
	pieces, spares := capacity.PiecesFor(int64(writes)*cost, writes, testSegmentCap)
	c, _ := newTestCore(t, func(o *coreOptions) {
		// One entry of slack beside the reservation's own, for the warm piece
		// the plain writer keeps overwriting, and one spare more than the ask
		// rolls into, since the warm write takes one before the reservation.
		o.extentLimit = int64(pieces) + 1
		o.contentSpares = spares + 1
	})
	s := mustShare(t, c, 1)

	shared := testFile(1)
	if _, err := c.WriteAt(context.Background(), s, shared, 0, make([]byte, payload), 1); err != nil {
		t.Fatalf("the warm write: %v", err)
	}
	limit := int64(pieces) + 1

	r := reserve(t, c, s, int64(writes)*cost, writes)
	defer r.Release()

	var reserved, plainRefused atomic.Int64
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < writes; i++ {
			if _, err := c.WriteReserved(context.Background(), s, r, core.NewFileID([16]byte{byte(i + 1), 0x9}), 0, make([]byte, payload), 1); err != nil {
				t.Errorf("reserved write %d: %v", i, err)
				return
			}
			reserved.Add(1)
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < writes; i++ {
			_, err := c.WriteAt(context.Background(), s, shared, 0, make([]byte, payload), 1)
			var ns *core.NoSpaceError
			if errors.As(err, &ns) && ns.Limit == core.LimitIndex {
				plainRefused.Add(1)
			}
		}
	}()
	wg.Wait()

	if got := reserved.Load(); got != writes {
		t.Fatalf("reserved writers landed %d of %d", got, writes)
	}
	if plainRefused.Load() == 0 {
		t.Fatal("no plain overwrite was refused the entries the reservation holds")
	}
	// Exact figures, not inequalities a clamped read cannot fail: after both
	// writers, ReservedBytes is exactly what the reservation has not drawn, its
	// held entries are exactly its undrawn ones, and the index count is the
	// warm piece plus the reserved pieces published — the figures the counters
	// must land on once every write's grant is released.
	if got, want := c.ReservedBytes(), r.Remaining(); got != want {
		t.Fatalf("after both writers, ReservedBytes = %d, want the reservation's %d undrawn", got, want)
	}
	if got, want := c.HeldEntries(), r.EntriesLeft(); got != want {
		t.Fatalf("after both writers, HeldEntries = %d, want the reservation's %d undrawn", got, want)
	}
	published := int64(pieces) + 1 - c.Index().EntriesLeft()
	if published > limit {
		t.Fatalf("the index count = %d, over the bound %d", published, limit)
	}
	// The warm piece is one entry; each reserved write published its own one,
	// since a 512-byte write fits its stream's room in a single piece.
	if want := int64(1) + int64(writes); published != want {
		t.Fatalf("the index holds %d entries, want the warm piece and the %d reserved writes'", published, writes)
	}
}

// TestDrawRespectsHeldVersions checks a version drawn outside a reservation is
// refused naming the version limit once only the versions a reservation holds
// are left.
func TestDrawRespectsHeldVersions(t *testing.T) {
	c, _ := newTestCore(t)
	s := mustShare(t, c, 1)
	if err := c.SetVersionLimit(core.Version{Counter: 3}); err != nil {
		t.Fatalf("SetVersionLimit: %v", err)
	}
	// One write drawn from the counter first, then the reservation holds the
	// two left.
	r := reserve(t, c, s, 2*capacity.WriteCharge(1, testSegmentCap), 2)
	defer r.Release()

	if v, err := c.Draw(); err == nil {
		t.Fatalf("Draw beside the reservation = %v, want the version limit's refusal", v)
	}
	// The reservation's own writes still draw their versions.
	for i := 0; i < 2; i++ {
		if _, err := c.WriteReserved(context.Background(), s, r, core.NewFileID([16]byte{byte(i + 1), 0x5}), 0, []byte("v"), 1); err != nil {
			t.Fatalf("reserved write %d: %v", i+1, err)
		}
	}
}

// TestReserveAhead_StraddleCannotEatHeldEntries checks a plain overwrite whose
// own entries would eat the ones a reservation holds is refused naming the
// index while the bound still shows room for the reservation and one entry
// more: a write that supersedes held pieces retains them, so its real adds are
// more than the one entry an empty file's write costs, and the check reads
// them.
func TestReserveAhead_StraddleCannotEatHeldEntries(t *testing.T) {
	const payload = 4096
	cost := capacity.WriteCharge(payload, testSegmentCap)
	pieces, spares := capacity.PiecesFor(cost, 1, testSegmentCap)

	// The bound holds the two warm pieces, the reservation's own entries, and
	// one entry of slack — visible room beside the held figure, which the
	// overwrite's real adds would still eat.
	c, _ := newTestCore(t, func(o *coreOptions) {
		o.extentLimit = int64(pieces) + 3
		o.contentSpares = spares + 1
	})
	s := mustShare(t, c, 1)

	// Two adjacent pieces on one file, so an overwrite across their boundary
	// supersedes both and adds an entry for each.
	shared := testFile(1)
	for i := 0; i < 2; i++ {
		if _, err := c.WriteAt(context.Background(), s, shared, int64(i)*payload, make([]byte, payload), 1); err != nil {
			t.Fatalf("warm write %d: %v", i, err)
		}
	}
	if left := c.Index().EntriesLeft(); left != int64(pieces)+1 {
		t.Fatalf("EntriesLeft after the warm writes = %d, want the reservation's %d and one more", left, pieces)
	}

	r := reserve(t, c, s, cost, 1)
	defer r.Release()
	if left := c.Index().EntriesLeft(); left != int64(pieces)+1 {
		t.Fatalf("EntriesLeft beside the reservation = %d, want the bound's %d and one more", left, pieces)
	}

	// The overwrite spans both pieces at off 1 for len 2*payload-2: it
	// supersedes both and retains both, three adds against one entry of slack.
	_, err := c.WriteAt(context.Background(), s, shared, 1, make([]byte, 2*payload-2), 1)
	_ = refused(t, err, core.LimitIndex)
}

// TestReserveAhead_StraddleWithinTheReservation checks a reserved write that
// straddles a partly filled segment lands without a limit's refusal, and the
// share's figure counts both records it really appended: the reservation
// charges the write's worst case, so the straddle's extra record is inside what
// it holds and the share's tally takes both pieces' bytes.
func TestReserveAhead_StraddleWithinTheReservation(t *testing.T) {
	const payload = 4096
	cost := capacity.WriteCharge(payload, testSegmentCap)
	_, spares := capacity.PiecesFor(cost, 1, testSegmentCap)
	c, _ := newTestCore(t, func(o *coreOptions) { o.contentSpares = spares + 1 })
	s := mustShare(t, c, 1)

	// A warm write of everything but the last few bytes of the segment leaves
	// a room smaller than the reserved write's payload: its split straddles.
	warm := roomAfter(4048)
	if _, err := c.WriteAt(context.Background(), s, testFile(9), 0, make([]byte, warm), 1); err != nil {
		t.Fatalf("the warm write: %v", err)
	}
	room := c.Streams().For(testFile(9)).Room()
	if room <= 0 || room >= payload {
		t.Fatalf("the stream's room = %d, want the straddle's gap between 1 and %d", room, payload)
	}

	r := reserve(t, c, s, cost, 1)
	defer r.Release()
	before, _ := c.set.ShareBytesAndRetired(1)
	if _, err := c.WriteReserved(context.Background(), s, r, testFile(1), 0, make([]byte, payload), 1); err != nil {
		t.Fatalf("the straddling reserved write: %v", err)
	}
	// The share counts the head's record and the tail's, each with its own
	// header and body: exactly the two records the straddle appended.
	after, _ := c.set.ShareBytesAndRetired(1)
	want := recordBytes(room) + recordBytes(payload-room)
	if got := after - before; got != want {
		t.Fatalf("the share's settled bytes rose by %d, want the head and tail's %d", got, want)
	}
}

// TestReserveAhead_EntriesHandedOverAsPublished checks the production path
// alone — reserved writes through Core.WriteReserved, no hand calls to the
// grant — hands the reservation's held entries over exactly as the index claims
// them: after a piece is published under an open reservation, heldEntries has
// fallen by the entries the index admitted, a plain write that fits what the
// reservation leaves is admitted, and the reservation's remaining entries still
// refuse a plain write that would eat them.
func TestReserveAhead_EntriesHandedOverAsPublished(t *testing.T) {
	const writes = 4
	cost := capacity.WriteCharge(1, testSegmentCap)
	pieces, spares := capacity.PiecesFor(int64(writes)*cost, writes, testSegmentCap)
	c, _ := newTestCore(t, func(o *coreOptions) {
		// The bound holds the reservation's entries and one: the plain write
		// below publishes one piece, so it fits once the held figure has
		// fallen, and not before.
		o.extentLimit = int64(pieces) + 1
		// The pool readies the reservation's spares and one the plain write's
		// roll needs. The reservation's own first roll onto the empty stream is
		// still replaced by the remaining-need rule — the stream cannot tell it
		// from an interleaved one — which raises the hold back to what its
		// remaining bytes need; the plain write below needs ready to exceed that
		// raised hold, so the pool carries one spare beyond it.
		o.contentSpares = spares + 2
	})
	s := mustShare(t, c, 1)

	r := reserve(t, c, s, int64(writes)*cost, writes)
	defer r.Release()
	if got := c.HeldEntries(); got != int64(pieces) {
		t.Fatalf("HeldEntries after the reservation = %d, want its %d", got, pieces)
	}

	// One one-byte reserved write publishes one piece against a fresh file: the
	// index admits one entry, and the held figure falls by exactly that.
	used := int64(pieces) + 1 - c.Index().EntriesLeft()
	if _, err := c.WriteReserved(context.Background(), s, r, testFile(1), 0, []byte("r"), 1); err != nil {
		t.Fatalf("the reserved write: %v", err)
	}
	claimed := int64(pieces) + 1 - c.Index().EntriesLeft() - used
	if claimed != 1 {
		t.Fatalf("the index claimed %d entries for the piece, want one", claimed)
	}
	if got, want := c.HeldEntries(), int64(pieces)-claimed; got != want {
		t.Fatalf("HeldEntries after one published piece = %d, want %d (the index claimed %d)", got, want, claimed)
	}

	// A plain write fits the one entry the bound's slack holds: the held figure
	// fell, so it is not refused for entries the reservation already published.
	if room := c.Streams().For(testFile(20)).Room(); room != 0 {
		t.Fatalf("the plain write's stream has a room of %d, want a fresh stream's 0", room)
	}
	if _, err := c.WriteAt(context.Background(), s, testFile(20), 0, []byte("p"), 1); err != nil {
		t.Fatalf("the plain write beside a partly published reservation: %v", err)
	}

	// A plain write that rolls — two pieces, one more entry than the bound's
	// remaining slack — is refused naming the index: the reservation still
	// holds what its later writes need.
	_, err := c.WriteAt(context.Background(), s, testFile(21), 0, make([]byte, 2*emptyRoomOf(testSegmentCap)), 1)
	_ = refused(t, err, core.LimitIndex)
}

// TestReserveAhead_RollingWriteCannotEatHeldEntries checks a plain write whose
// split rolls publishes one index entry per piece: beside a reservation with one
// entry of slack it is refused naming the index, and with no reservation it is
// admitted. A pre-check that counted the whole extent as one placement would
// admit it and spend a held entry.
func TestReserveAhead_RollingWriteCannotEatHeldEntries(t *testing.T) {
	// A payload of two pieces against an empty stream, its split past one cap.
	room := emptyRoomOf(testSegmentCap)
	payload := make([]byte, room+room/2)
	pieces, spares := capacity.PiecesFor(capacity.WriteCharge(int64(len(payload)), testSegmentCap), 1, testSegmentCap)

	// No reservation: the write is admitted, its pieces publishing their own
	// entries against the bound's room.
	noRes := func(o *coreOptions) {
		o.extentLimit = 1 << 20
		o.contentSpares = max(spares, 2) + 1
	}
	c, _ := newTestCore(t, noRes)
	s := mustShare(t, c, 1)
	if _, err := c.WriteAt(context.Background(), s, testFile(1), 0, payload, 1); err != nil {
		t.Fatalf("the rolling write with no reservation: %v", err)
	}

	// Beside a reservation with exactly one entry of slack over its own held
	// entries: the rolling write's second entry would eat one of them.
	c2, _ := newTestCore(t, func(o *coreOptions) {
		o.extentLimit = int64(pieces) + 1
		// Enough spares for the reservation's own pieces and the plain
		// write's, so only the index bound can refuse it.
		o.contentSpares = spares + 4
	})
	s2 := mustShare(t, c2, 1)
	r := reserve(t, c2, s2, capacity.WriteCharge(int64(len(payload)), testSegmentCap), 1)
	defer r.Release()

	_, err := c2.WriteAt(context.Background(), s2, testFile(2), 0, payload, 1)
	_ = refused(t, err, core.LimitIndex)
}

// TestReserveAhead_PlainWritersCannotSpendAHeldVersion runs two plain writers at
// the version counter's last free version beside a reservation holding one:
// exactly one is admitted, the other refused naming the version limit, the
// held version survives, and the reservation's own write draws it under -race.
func TestReserveAhead_PlainWritersCannotSpendAHeldVersion(t *testing.T) {
	c, _ := newTestCore(t)
	s := mustShare(t, c, 1)
	r := reserve(t, c, s, capacity.WriteCharge(1, testSegmentCap), 1)
	defer r.Release()

	// Tighten the limit so exactly one version is left free: the reservation
	// holds one, and two plain writers race for the free one.
	if err := c.SetVersionLimit(core.Version{Counter: c.counters.NextVersion().Counter + 2}); err != nil {
		t.Fatalf("SetVersionLimit: %v", err)
	}
	if left := c.counters.VersionsLeft(); left != 2 {
		t.Fatalf("VersionsLeft beside the reservation = %d, want two: one held and one free", left)
	}

	results := make(chan error, 2)
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := c.WriteAt(context.Background(), s, core.NewFileID([16]byte{byte(10 + i), 0x4}), 0, []byte("p"), 1)
			results <- err
		}()
	}
	wg.Wait()
	close(results)
	var passed, refusedN int
	for err := range results {
		if err == nil {
			passed++
			continue
		}
		refusedN++
		_ = refused(t, err, core.LimitVersion)
	}
	if passed != 1 || refusedN != 1 {
		t.Fatalf("two plain writers at one free version: %d passed and %d refused, want one each", passed, refusedN)
	}

	// The held version survived: the reservation's write draws it.
	if _, err := c.WriteReserved(context.Background(), s, r, testFile(3), 0, []byte("r"), 1); err != nil {
		t.Fatalf("the reserved write after two plain writers: %v", err)
	}
}

// TestReserveAhead_StraddleBesideAReservationCannotEatAHeldEntry checks a plain
// write that straddles a partly filled segment — its head in the room left and
// its tail beyond it, two published pieces — is refused naming the index beside
// a reservation with one entry of slack, and admitted with no reservation. The
// refusal lands on the admission, before any byte is appended, rather than at
// the publish the second piece would meet.
func TestReserveAhead_StraddleBesideAReservationCannotEatAHeldEntry(t *testing.T) {
	// A warm record leaves the stream a room between one byte and the write's
	// payload: the write below splits into a head in that room and a tail in the
	// next segment.
	warm := roomAfter(0) - 2048
	payload := make([]byte, 4096)
	cost := capacity.WriteCharge(int64(len(payload)), testSegmentCap)
	pieces, spares := capacity.PiecesFor(cost, 1, testSegmentCap)

	// No reservation: the straddling write is admitted.
	c, _ := newTestCore(t, func(o *coreOptions) {
		o.extentLimit = 1 << 20
		o.contentSpares = max(spares, 2) + 2
	})
	s := mustShare(t, c, 1)
	if _, err := c.WriteAt(context.Background(), s, testFile(9), 0, make([]byte, warm), 1); err != nil {
		t.Fatalf("the warm write: %v", err)
	}
	if room := c.Streams().For(testFile(9)).Room(); room <= 0 || room >= int64(len(payload)) {
		t.Fatalf("the warm write leaves a room of %d, want between 1 and %d", room, len(payload))
	}
	// The straddling write lands on the warm write's stream, so its split meets
	// the room left rather than a fresh stream's none.
	straddler := sameStreamAs(t, c, testFile(9), testFile(1))
	if _, err := c.WriteAt(context.Background(), s, straddler, 0, payload, 1); err != nil {
		t.Fatalf("the straddling write with no reservation: %v", err)
	}

	// Beside a reservation with one entry of slack over its held figure: the
	// straddling write's second piece would eat a held entry, so it is refused
	// at its admission — before it appends the head's record.
	c2, _ := newTestCore(t, func(o *coreOptions) {
		o.extentLimit = int64(pieces) + 2 // the warm piece, the reservation, and one slack
		o.contentSpares = spares + 4
	})
	s2 := mustShare(t, c2, 1)
	if _, err := c2.WriteAt(context.Background(), s2, testFile(9), 0, make([]byte, warm), 1); err != nil {
		t.Fatalf("the warm write beside the reservation: %v", err)
	}
	if room := c2.Streams().For(testFile(9)).Room(); room <= 0 || room >= int64(len(payload)) {
		t.Fatalf("the warm write beside the reservation leaves a room of %d, want between 1 and %d", room, len(payload))
	}
	r := reserve(t, c2, s2, cost, 1)
	defer r.Release()

	straddler2 := sameStreamAs(t, c2, testFile(9), testFile(2))
	before, _ := c2.set.ShareBytesAndRetired(1)
	_, err := c2.WriteAt(context.Background(), s2, straddler2, 0, payload, 1)
	_ = refused(t, err, core.LimitIndex)
	if after, _ := c2.set.ShareBytesAndRetired(1); after != before {
		t.Fatalf("the refused straddling write appended a record: the share's bytes rose from %d to %d", before, after)
	}
}

// TestReserveAhead_RoomlessWriteCountsItsSparesAlone checks a plain write that
// splits into two pieces on a stream with no room is refused naming the index
// only when its two entries would eat a held one, and admitted when the
// reservation leaves exactly those two: a roomless stream's pieces are the
// spares alone, the first piece included, not one more.
func TestReserveAhead_RoomlessWriteCountsItsSparesAlone(t *testing.T) {
	// Two pieces on a roomless stream: each takes a whole empty segment.
	payload := make([]byte, 2*emptyRoomOf(testSegmentCap))
	pieces, spares := capacity.PiecesFor(capacity.WriteCharge(int64(len(payload)), testSegmentCap), 1, testSegmentCap)

	// The bound leaves the reservation's entries and two beyond: the write's own
	// two entries fit, so it is admitted.
	c, _ := newTestCore(t, func(o *coreOptions) {
		o.extentLimit = int64(pieces) + 2
		o.contentSpares = spares + 3
	})
	s := mustShare(t, c, 1)
	if got := c.Streams().For(testFile(1)).Room(); got != 0 {
		t.Fatalf("the stream's room = %d, want a roomless stream's 0", got)
	}
	r := reserve(t, c, s, capacity.WriteCharge(int64(len(payload)), testSegmentCap), 1)
	defer r.Release()
	if _, err := c.WriteAt(context.Background(), s, testFile(1), 0, payload, 1); err != nil {
		t.Fatalf("the roomless two-piece write beside the reservation = %v, want it admitted", err)
	}

	// One entry less of slack: the write's second entry would eat a held one,
	// so it is refused naming the index.
	c2, _ := newTestCore(t, func(o *coreOptions) {
		o.extentLimit = int64(pieces) + 1
		o.contentSpares = spares + 3
	})
	s2 := mustShare(t, c2, 1)
	r2 := reserve(t, c2, s2, capacity.WriteCharge(int64(len(payload)), testSegmentCap), 1)
	defer r2.Release()
	_, err := c2.WriteAt(context.Background(), s2, testFile(1), 0, payload, 1)
	_ = refused(t, err, core.LimitIndex)
}

// sameStreamAs returns a file id other than avoid that hashes to the same append
// stream as anchor, so a test's second write lands on the stream its first did.
func sameStreamAs(t *testing.T, c *Core, anchor, avoid core.FileID) core.FileID {
	t.Helper()
	want := c.Streams().IndexFor(anchor)
	for i := 0; i < 256; i++ {
		f := core.NewFileID([16]byte{byte(i), 0xee})
		if f != avoid && c.Streams().IndexFor(f) == want {
			return f
		}
	}
	t.Fatalf("no file id on stream %d of anchor %v", want, anchor)
	return core.FileID{}
}

// TestReserveAhead_RemovalCannotSpendAHeldVersion checks a truncate at the
// version counter's last free version, beside a reservation holding it, is
// refused naming the version limit like a plain write, and that the held
// version is still the reservation's own to draw.
func TestReserveAhead_RemovalCannotSpendAHeldVersion(t *testing.T) {
	c, _ := newTestCore(t)
	s := mustShare(t, c, 1)
	f := testFile(1)
	if _, err := c.WriteAt(context.Background(), s, f, 0, make([]byte, 4096), 1); err != nil {
		t.Fatalf("the file's write: %v", err)
	}
	r := reserve(t, c, s, capacity.WriteCharge(1, testSegmentCap), 1)
	defer r.Release()

	// One version left, and the reservation holds it.
	if err := c.SetVersionLimit(core.Version{Counter: c.counters.NextVersion().Counter + 1}); err != nil {
		t.Fatalf("SetVersionLimit: %v", err)
	}
	if left := c.counters.VersionsLeft(); left != 1 {
		t.Fatalf("VersionsLeft = %d, want the one the reservation holds", left)
	}

	_, err := c.Truncate(context.Background(), s, f, 0)
	ns := refused(t, err, core.LimitVersion)
	if ns.Unit != core.UnitVersions {
		t.Fatalf("the removal's refusal is in %s, want versions", ns.Unit)
	}
	if _, err := c.WriteReserved(context.Background(), s, r, testFile(3), 0, []byte("r"), 1); err != nil {
		t.Fatalf("the reserved write after the refused removal: %v", err)
	}
}
