package journal

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/block/journal/journaltest/kit"
)

// reserveAhead calls Reserve until a spare the pool makes off the write path
// is ready, and fails the test on any other refusal. The wait for a refused
// ask parks on the pool's settle event — the refill makes up every ask
// beside the open holds — never against a deadline: the minute below is a
// backstop over a settle that never comes.
func reserveAhead(t *testing.T, h *Handle, n int64, writes int) *Reservation {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute) // backstop only; the wait is the settle
	defer cancel()
	for {
		r, err := h.Reserve(n, writes)
		var ns *NoSpaceError
		if errors.As(err, &ns) && ns.Limit == LimitSpare {
			if werr := h.j.core.WaitSpares(ctx); werr != nil {
				t.Fatalf("WaitSpares after a reserve refused at the spare limit: got %v, want the pool ready", werr)
			}
			continue
		}
		if err != nil {
			t.Fatalf("Reserve(%d, %d): %v", n, writes, err)
		}
		return r
	}
}

// TestHandleReserve_RefusesACallerBug checks that an ask of no bytes or no
// writes is refused as the caller's bug, and that the refusal holds nothing:
// no reserved bytes, no held entries and no spare.
func TestHandleReserve_RefusesACallerBug(t *testing.T) {
	j := newTestJournal(t)
	withLimit(t, j)
	h, err := j.Share(1, 0, Version{})
	if err != nil {
		t.Fatalf("Share: %v", err)
	}

	for _, tt := range []struct {
		name   string
		n      int64
		writes int
	}{
		{"no bytes", 0, 1},
		{"negative bytes", -1, 1},
		{"no writes", 4096, 0},
		{"negative writes", 4096, -1},
	} {
		refused := j.core.ReservationRefused()
		r, err := h.Reserve(tt.n, tt.writes)
		if !errors.Is(err, ErrInvalidExtent) {
			t.Fatalf("%s: Reserve(%d, %d) = %v, want ErrInvalidExtent", tt.name, tt.n, tt.writes, err)
		}
		if r != nil {
			t.Fatalf("%s: Reserve(%d, %d) answered a reservation beside its refusal", tt.name, tt.n, tt.writes)
		}
		if got := j.core.ReservationRefused(); got != refused {
			t.Fatalf("%s: after the refusal, ReservationRefused rose from %d to %d, want it unchanged: a caller bug consults no limit", tt.name, refused, got)
		}
		if got := j.core.ReservedBytes(); got != 0 {
			t.Fatalf("%s: after the refusal, ReservedBytes = %d, want 0", tt.name, got)
		}
		if got := j.core.HeldEntries(); got != 0 {
			t.Fatalf("%s: after the refusal, the reservation holds %d index entries, want 0", tt.name, got)
		}
		if got := j.core.PoolCounters().Held; got != 0 {
			t.Fatalf("%s: after the refusal, the pool holds %d spares, want 0", tt.name, got)
		}
	}
}

// TestHandleClose_ReleasesOpenReservations checks that a handle's Close
// returns what its open reservation still holds to the journal, so nothing
// leaks once the handle is gone, and that the released reservation answers
// every later write and its own Close with ErrClosed.
func TestHandleClose_ReleasesOpenReservations(t *testing.T) {
	j := newTestJournal(t)
	withLimit(t, j)
	h, err := j.Share(1, 0, Version{})
	if err != nil {
		t.Fatalf("Share: %v", err)
	}
	r := reserveAhead(t, h, 64<<10, 1)
	// A reserved write lands, so the reservation is open holding the part its
	// records have not filled.
	if _, err := r.WriteAt(context.Background(), fileOf(1), 0, []byte("reserved"), 1); err != nil {
		t.Fatalf("the reserved write: %v", err)
	}
	if got := j.core.ReservedBytes(); got == 0 {
		t.Fatal("ReservedBytes is 0 with a reservation open, want it counted")
	}
	held := j.core.HeldEntries()
	if held == 0 {
		t.Fatal("the reservation holds no index entry with half its ask unused, want the entries it took")
	}

	if err := h.Close(); err != nil {
		t.Fatalf("Close the handle: %v", err)
	}
	if got := j.core.ReservedBytes(); got != 0 {
		t.Fatalf("after the handle closed, ReservedBytes = %d, want the reservation's ask released", got)
	}
	if got := j.core.HeldEntries(); got != 0 {
		t.Fatalf("after the handle closed, %d index entries are still held, want exactly 0 of the %d the reservation took", got, held)
	}
	if _, err := r.WriteAt(context.Background(), fileOf(2), 0, []byte("late"), 1); !errors.Is(err, ErrClosed) {
		t.Fatalf("a reserved write after the handle closed = %v, want ErrClosed", err)
	}
	if err := r.Close(); !errors.Is(err, ErrClosed) {
		t.Fatalf("the released reservation's Close = %v, want ErrClosed: the handle released it", err)
	}
}

// TestReservation_CloseTwice checks that Close returns what the reservation
// left to the journal, that a second Close is ErrClosed, and that a write
// after either is refused ErrClosed.
func TestReservation_CloseTwice(t *testing.T) {
	j := newTestJournal(t)
	withLimit(t, j)
	h, err := j.Share(1, 0, Version{})
	if err != nil {
		t.Fatalf("Share: %v", err)
	}
	r := reserveAhead(t, h, 4096, 1)
	if got := j.core.ReservedBytes(); got != 4096 {
		t.Fatalf("ReservedBytes = %d with the reservation open, want its ask of 4096", got)
	}

	if err := r.Close(); err != nil {
		t.Fatalf("the first Close: %v", err)
	}
	if got := j.core.ReservedBytes(); got != 0 {
		t.Fatalf("after Close, ReservedBytes = %d, want 0", got)
	}
	if err := r.Close(); !errors.Is(err, ErrClosed) {
		t.Fatalf("the second Close = %v, want ErrClosed", err)
	}
	if _, err := r.WriteAt(context.Background(), fileOf(1), 0, []byte("late"), 1); !errors.Is(err, ErrClosed) {
		t.Fatalf("a reserved write after Close = %v, want ErrClosed", err)
	}
}

// TestJournalClose_ReleasesEveryHandlesReservations checks that closing the
// journal returns every handle's open reservation: no bytes stay reserved, no
// entry stays held, the pool owes no spare, and each released reservation
// refuses a later write with ErrClosed.
func TestJournalClose_ReleasesEveryHandlesReservations(t *testing.T) {
	j := newTestJournal(t)
	withLimit(t, j)
	var reservations []*Reservation
	for tag := range 2 {
		h, err := j.Share(ShareTag(tag+1), 0, Version{})
		if err != nil {
			t.Fatalf("Share %d: %v", tag+1, err)
		}
		reservations = append(reservations, reserveAhead(t, h, 64<<10, 1))
	}
	if got := j.core.ReservedBytes(); got == 0 {
		t.Fatal("ReservedBytes is 0 with two reservations open, want them counted")
	}
	if got := j.core.PoolCounters().Held; got == 0 {
		t.Fatal("the pool owes no spare with two reservations open, want both holds counted")
	}

	if err := j.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if got := j.core.ReservedBytes(); got != 0 {
		t.Fatalf("after the journal closed, ReservedBytes = %d, want every reservation released", got)
	}
	if got := j.core.HeldEntries(); got != 0 {
		t.Fatalf("after the journal closed, %d index entries are still held, want 0", got)
	}
	if got := j.core.PoolCounters().Held; got != 0 {
		t.Fatalf("after the journal closed, the pool still owes %d spares, want 0", got)
	}
	for k, r := range reservations {
		if _, err := r.WriteAt(context.Background(), fileOf(byte(k+1)), 0, []byte("late"), 1); !errors.Is(err, ErrClosed) {
			t.Fatalf("reservation %d's write after the journal closed = %v, want ErrClosed", k, err)
		}
	}
}

// TestReservation_WriteBeyondTheAskIsExhausted checks that a reserved write the
// reservation cannot cover is refused with the public ErrReservationExhausted,
// not a NoSpaceError, so a caller outside the package can tell it apart.
func TestReservation_WriteBeyondTheAskIsExhausted(t *testing.T) {
	j := newTestJournal(t)
	withLimit(t, j)
	h, err := j.Share(1, 0, Version{})
	if err != nil {
		t.Fatalf("Share: %v", err)
	}
	r := reserveAhead(t, h, 1, 1)
	_, err = r.WriteAt(context.Background(), fileOf(1), 0, make([]byte, 4096), 1)
	if !errors.Is(err, ErrReservationExhausted) {
		t.Fatalf("a 4096-byte write into a 1-byte reservation = %v, want ErrReservationExhausted", err)
	}
	var ns *NoSpaceError
	if errors.As(err, &ns) {
		t.Fatalf("the refusal is a NoSpaceError (%v), want none: no limit was met", ns)
	}
}

// TestJournalClose_DrainsAReservedWriteBeforeReleasing checks that closing the
// journal waits for a reserved write in flight before it releases the
// reservation: the write, held at storage across a roll, lands whole, and only
// then are its bytes returned. A later Close of the handle answers as it does
// when the journal's Close did not drain it: nil once, then ErrClosed.
func TestJournalClose_DrainsAReservedWriteBeforeReleasing(t *testing.T) {
	j, seam := newSeamedJournal(t)
	withLimit(t, j)
	h, err := j.Share(1, 0, Version{})
	if err != nil {
		t.Fatalf("Share: %v", err)
	}
	const payload = 96 << 10 // past one 64 KiB segment, so the write rolls
	r := reserveAhead(t, h, 2*payload, 1)
	// The refill fills to the held-aware goal — every open hold's spares
	// beside the target — and the wait parks until its settle, so the pool is
	// full and idle when it answers: the first write the hold below stops is
	// the reserved one's, never the maker's next. The minute is a backstop
	// over a settle that never comes; the count is read once, asserted, not
	// polled for.
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute) // backstop only; the wait is the settle
	defer cancel()
	if err := j.core.WaitSpares(ctx); err != nil {
		t.Fatalf("WaitSpares before the hold: got %v, want the pool filled beside the reservation", err)
	}
	if c := j.core.PoolCounters(); c.Spares < 4+c.Held {
		t.Fatalf("the pool settled at %+v, want %d spares ready beside the %d the reservation holds", c, 4+c.Held, c.Held)
	}

	hold := seam.HoldFirst(t, kit.OpWrite, "*")
	wrote := make(chan error, 1)
	go func() {
		_, err := r.WriteAt(context.Background(), fileOf(1), 0, make([]byte, payload), 1)
		wrote <- err
	}()
	<-hold.Reached()

	closed := make(chan error, 1)
	go func() { closed <- j.Close() }()
	select {
	case err := <-closed:
		t.Fatalf("Journal.Close returned %v while a reserved write was in flight", err)
	case <-time.After(50 * time.Millisecond): // a bounded wait is the assertion: nothing to signal that a Close stays blocked
	}
	if got := j.core.ReservedBytes(); got == 0 {
		t.Fatal("the reservation was released while its write was in flight, want it held until the write returned")
	}

	hold.Release()
	if err := <-wrote; err != nil {
		t.Fatalf("the reserved write across the journal's Close = %v, want it to land whole", err)
	}
	if err := <-closed; err != nil {
		t.Fatalf("Journal.Close = %v", err)
	}
	if got := j.core.ReservedBytes(); got != 0 {
		t.Fatalf("after Close, ReservedBytes = %d, want the reservation released", got)
	}
	if err := h.Close(); err != nil {
		t.Fatalf("the handle's Close after the journal's = %v, want nil as before", err)
	}
	if err := h.Close(); !errors.Is(err, ErrClosed) {
		t.Fatalf("the handle's second Close = %v, want ErrClosed", err)
	}
}
