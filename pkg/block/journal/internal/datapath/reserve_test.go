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
	cost := recordBytes(payload) // one write's record bytes

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
	r := reserve(t, c, s, 10*recordBytes(4096), 10)
	if held := c.pool.Counters().Held; held == 0 {
		t.Fatal("the reservation holds no spare")
	}
	if _, err := c.WriteReserved(context.Background(), s, r, testFile(1), 0, make([]byte, 4096), 1); err != nil {
		t.Fatalf("the reservation's first write: %v", err)
	}
	spent := c.ReservedBytes()
	if spent <= 0 || spent >= 10*recordBytes(4096) {
		t.Fatalf("ReservedBytes after one of ten writes = %d, want between 0 and %d", spent, 10*recordBytes(4096))
	}
	if got, want := r.Remaining(), 9*recordBytes(4096); got != want {
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
	reserve(t, c, s, 10*recordBytes(4096), 10).Release()
}

// TestReserveAhead_PastTheIndexBound checks ten thousand small reserved writes
// all land while a plain write is refused naming the index: the reservation
// holds every index entry, and its own draws spend them without the bound
// consulting itself again.
func TestReserveAhead_PastTheIndexBound(t *testing.T) {
	const writes = 10_000
	cost := int64(writes) * recordBytes(1) // one-byte writes, their record bytes the reservation's figure
	pieces, spares := capacity.PiecesFor(cost, writes, testSegmentCap)
	c, _ := newTestCore(t, func(o *coreOptions) {
		o.extentLimit = int64(pieces)
		// The pool readies the spares the ask rolls into, as a production
		// pool at its target would.
		o.contentSpares = spares
	})
	s := mustShare(t, c, 1)

	r := reserve(t, c, s, cost, writes)
	defer r.Release()

	// The index's bound is the reservation's: a plain write adds one entry and
	// is refused, one entry short of what the reservation holds.
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
	r := reserve(t, c, s, recordBytes(1), 1)
	defer r.Release()

	// The warm write fills the journal, and eleven headroom draws put its
	// drawing side below half.
	if _, err := c.WriteAt(context.Background(), s, testFile(1), 0, make([]byte, roomAfter(0)+1), 1); err != nil {
		t.Fatalf("the warm write: %v", err)
	}
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
	r := reserve(t, c, s, recordBytes(4096), 1)
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
	r := reserve(t, c, s, writes*recordBytes(4096), writes)
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
	if got := c.ReservedBytes(); got < 0 {
		t.Fatalf("ReservedBytes = %d after both writers", got)
	}
}

// isRefusal reports whether err is a journal limit's refusal.
func isRefusal(err error) bool { return errors.As(err, new(*core.NoSpaceError)) }

// isExhausted reports whether err is a spent reservation's refusal.
func isExhausted(err error) bool { return errors.Is(err, core.ErrReservationExhausted) }
