package journal_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/block/journal"
	"github.com/cubbit/dittofs/pkg/block/journal/journaltest"
	"github.com/cubbit/dittofs/pkg/block/journal/journaltest/kit"
)

// writeOutcome is what one public WriteAt returned.
type writeOutcome struct {
	v   journal.Version
	err error
}

// waitUntil polls cond with a short pause between tries until it holds,
// failing the test at the deadline.
func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); !cond(); time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
	}
}

// TestJournalCloseFencesThenDrains checks the public Close end to end: a write
// in flight, held at storage, holds the close back on the journal's fence — a
// new call through the handle, a Draw, a Share and the reservation surface are
// all refused while the write finishes — and once it lands, Close returns nil
// and every call after it is refused. The profiler's watch, which the fixture
// installs, fails the test on goroutines the close leaked.
func TestJournalCloseFencesThenDrains(t *testing.T) {
	f := journaltest.New(t)
	h := f.Share(0)
	file := journaltest.File(0)
	other := journaltest.File(1)
	ctx := context.Background()

	hold := f.Seam.HoldFirst(t, kit.OpWrite, "*")
	wrote := make(chan writeOutcome, 1)
	go func() {
		for {
			// The file's stream may meet no spare the pool has readied yet,
			// which refuses the write with the retryable spare limit before
			// it reaches the seam; the attempt drew nothing and appended
			// nothing, so it is simply made again until one lands.
			v, err := h.WriteAt(ctx, file, 0, []byte("in flight"), 1)
			var ns *journal.NoSpaceError
			if errors.As(err, &ns) && ns.Limit == journal.LimitSpare {
				continue
			}
			wrote <- writeOutcome{v, err}
			return
		}
	}()
	<-hold.Reached()

	// The reservation is taken before the close, as an operation preparing
	// its writes does, and its writes are refused once the fence is up while
	// its own Close still returns what it held. The pool refills the spares
	// the reservation holds off the write path, so the ask is retried until
	// the pool has readied them.
	var res *journal.Reservation
	waitUntil(t, "the reservation to be taken", func() bool {
		r, err := h.Reserve(64<<10, 1)
		res = r
		return err == nil
	})

	closed := make(chan error, 1)
	go func() { closed <- f.J.Close() }()

	// The fence is observable once a new call through the handle is refused;
	// before it, the probe on a fresh file is a real write and lands.
	waitUntil(t, "a new write to be refused once the journal's Close began", func() bool {
		_, err := h.WriteAt(ctx, other, 0, []byte("late"), 1)
		return errors.Is(err, journal.ErrClosed)
	})
	if _, err := f.J.Draw(); !errors.Is(err, journal.ErrClosed) {
		t.Fatalf("the Draw while the close drains = %v, want ErrClosed", err)
	}
	if _, err := f.J.Share(journaltest.Tag(1), 0, f.Floor()); !errors.Is(err, journal.ErrClosed) {
		t.Fatalf("the Share of a new tag while the close drains = %v, want ErrClosed", err)
	}
	if _, err := h.Reserve(64<<10, 1); !errors.Is(err, journal.ErrClosed) {
		t.Fatalf("the Reserve while the close drains = %v, want ErrClosed", err)
	}
	if _, err := res.WriteAt(ctx, other, 0, []byte("late"), 1); !errors.Is(err, journal.ErrClosed) {
		t.Fatalf("the reserved write while the close drains = %v, want ErrClosed", err)
	}
	// The journal's own release of the handle's reservations runs after the
	// drain, so the reservation is still open through its owner, and its
	// Close returns what it held: nil.
	if err := res.Close(); err != nil {
		t.Fatalf("the reservation's Close while the close drains = %v, want nil", err)
	}

	// The bounded wait is the assertion, not a sleep before one: a close that
	// failed to fence or to drain returns while the storage hold keeps its
	// write in flight and fails here.
	select {
	case err := <-closed:
		t.Fatalf("Journal.Close returned %v while its write was still in flight", err)
	case <-time.After(250 * time.Millisecond):
	}

	hold.Release()
	got := <-wrote
	if got.err != nil {
		t.Fatalf("the write in flight at Close = %v, want its version", got.err)
	}
	if got.v == (journal.Version{}) {
		t.Fatal("the write in flight at Close returned no version")
	}
	// The write's version came back before the close returned, as the drain
	// orders it: nothing after a closed journal can be read to prove it,
	// so the receipt above is the proof.
	if err := <-closed; err != nil {
		t.Fatalf("Journal.Close = %v, want nil", err)
	}
	if err := f.J.Close(); !errors.Is(err, journal.ErrClosed) {
		t.Fatalf("the second Journal.Close = %v, want ErrClosed", err)
	}
	if _, err := f.J.Draw(); !errors.Is(err, journal.ErrClosed) {
		t.Fatalf("the Draw after the journal closed = %v, want ErrClosed", err)
	}
	if _, err := h.Reserve(64<<10, 1); !errors.Is(err, journal.ErrClosed) {
		t.Fatalf("the Reserve after the journal closed = %v, want ErrClosed", err)
	}
}
