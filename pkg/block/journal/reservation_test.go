package journal

import (
	"context"
	"errors"
	"runtime"
	"testing"
)

// reserveAhead calls Reserve until a spare the pool makes off the write path
// is ready, and fails the test on any other refusal. The wait is a bounded
// count of yields, never a sleep.
func reserveAhead(t *testing.T, h *Handle, n int64, writes int) *Reservation {
	t.Helper()

	for range 1 << 22 {
		r, err := h.Reserve(n, writes)
		var ns *NoSpaceError
		if !errors.As(err, &ns) || ns.Limit != LimitSpare {
			if err != nil {
				t.Fatalf("Reserve(%d, %d): %v", n, writes, err)
			}
			return r
		}
		runtime.Gosched()
	}
	t.Fatalf("Reserve(%d, %d) was refused for want of a spare 2^22 times: the journal never made one", n, writes)
	return nil
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
		r, err := h.Reserve(tt.n, tt.writes)
		if !errors.Is(err, ErrInvalidExtent) {
			t.Fatalf("%s: Reserve(%d, %d) = %v, want ErrInvalidExtent", tt.name, tt.n, tt.writes, err)
		}
		if r != nil {
			t.Fatalf("%s: Reserve(%d, %d) answered a reservation beside its refusal", tt.name, tt.n, tt.writes)
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

	if err := h.Close(); err != nil {
		t.Fatalf("Close the handle: %v", err)
	}
	if got := j.core.ReservedBytes(); got != 0 {
		t.Fatalf("after the handle closed, ReservedBytes = %d, want the reservation's ask released", got)
	}
	if got := j.core.HeldEntries(); got >= held {
		t.Fatalf("after the handle closed, the reservation holds %d index entries, want fewer than its %d", got, held)
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
