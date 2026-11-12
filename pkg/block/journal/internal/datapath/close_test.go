package datapath

import (
	"bytes"
	"context"
	"errors"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/block/journal/journaltest/kit"
)

// TestCallsAfterCloseFailErrClosed checks the fence from every entry point of
// the data path: once BeginClose ran — before the core itself is torn down —
// every call, through a handle or on the journal's own surface, is refused
// with nothing changed. A missing gate lets its call through and fails its
// leg: the handle calls return their own results, the journal-level calls
// succeed.
func TestCallsAfterCloseFailErrClosed(t *testing.T) {
	c, _ := newTestCore(t)
	f1, f2 := testFile(1), testFile(2)
	s1, s2 := mustShare(t, c, 1), mustShare(t, c, 2)
	ctx := context.Background()

	c.BeginClose()

	if _, err := c.WriteAt(ctx, s1, f1, 0, []byte("late"), 1); !errors.Is(err, core.ErrClosed) {
		t.Fatalf("the WriteAt refused by the fence = %v, want ErrClosed", err)
	}
	p := make([]byte, 4)
	if _, _, _, _, err := c.ReadAt(ctx, s2, f2, 0, p); !errors.Is(err, core.ErrClosed) {
		t.Fatalf("the ReadAt refused by the fence = %v, want ErrClosed", err)
	}
	if err := c.Fill(ctx, s2, f2, 0, []byte("late"), core.Version{}, core.Version{}); !errors.Is(err, core.ErrClosed) {
		t.Fatalf("the Fill refused by the fence = %v, want ErrClosed", err)
	}
	if _, err := c.Share(3, 0, core.Version{}); !errors.Is(err, core.ErrClosed) {
		t.Fatalf("the Share after the close began = %v, want ErrClosed", err)
	}
	if _, err := c.Draw(); !errors.Is(err, core.ErrClosed) {
		t.Fatalf("the Draw after the close began = %v, want ErrClosed", err)
	}
	if err := c.SetVersionLimit(core.Version{Counter: limitCount}); !errors.Is(err, core.ErrClosed) {
		t.Fatalf("the SetVersionLimit after the close began = %v, want ErrClosed", err)
	}
}

// TestInFlightCallsFinishAtClose checks the drain: Close waits for the calls
// already in flight on every fence share — a write parked before its append
// with its file's append domain held, and a read parked before its storage
// read — and returns nil only after both returned their own results. A close
// that stopped the core under its in-flight calls instead of draining them
// returns while the park holds both, and fails the bounded wait that is the
// assertion.
func TestInFlightCallsFinishAtClose(t *testing.T) {
	writeParked, writeRelease := make(chan struct{}), make(chan struct{})
	readParked, readRelease := make(chan struct{}), make(chan struct{})
	var appendN, readN atomic.Int32
	c, _ := newTestCore(t, func(o *coreOptions) {
		// The parked write is the first call to reach beforeAppend and the
		// parked read the first to reach beforeRead; the probe write below is
		// a later arrival at beforeAppend and passes the seam through.
		o.beforeAppend = func() {
			if appendN.Add(1) == 1 {
				close(writeParked)
				<-writeRelease
			}
		}
		o.beforeRead = func() {
			if readN.Add(1) == 1 {
				close(readParked)
				<-readRelease
			}
		}
	})
	s1, s2 := mustShare(t, c, 1), mustShare(t, c, 2)
	f1, f2 := testFile(1), testFile(2)
	// The parked read's bytes are published before anything runs, through the
	// index and the streams directly, so the read has a record to serve and
	// the park never waits on a warm-up.
	payload := []byte("the bytes the parked read serves")
	publish(t, c, f2, 0, string(payload))
	stopWrite := releaser(t, writeRelease)
	stopRead := releaser(t, readRelease)

	wrote := make(chan writeResult, 1)
	go func() {
		v, err := c.WriteAt(context.Background(), s1, f1, 0, []byte("in flight"), 1)
		wrote <- writeResult{v, err}
	}()
	within(t, writeParked, "the write to park before its append")

	p := make([]byte, len(payload))
	read := make(chan readResult, 1)
	go func() {
		n, missing, held, asOf, err := c.ReadAt(context.Background(), s1, f2, 0, p)
		read <- readResult{n, missing, held, asOf, err}
	}()
	within(t, readParked, "the read to park before its records")

	closed := make(chan error, 1)
	go func() { closed <- c.Close() }()

	// The fence is observable once a probe call through the second share is
	// refused; before the fence it is a real write on a fresh file and simply
	// lands.
	eventually(t, "a probe write to be refused once the fence went up", func() bool {
		_, err := c.WriteAt(context.Background(), s2, testFile(3), 0, []byte("probe"), 1)
		return errors.Is(err, core.ErrClosed)
	})
	// The bounded wait is the assertion, not a sleep before one: a close that
	// fails to drain its in-flight calls returns while the park holds both
	// and fails here.
	select {
	case err := <-closed:
		t.Fatalf("Close returned %v while its write and read were still in flight", err)
	case <-time.After(250 * time.Millisecond):
	}

	stopWrite()
	stopRead()

	got := within(t, wrote, "the write that was in flight at Close")
	if got.err != nil {
		t.Fatalf("the write in flight at Close = %v, want its version", got.err)
	}
	if got.v == (core.Version{}) {
		t.Fatal("the write in flight at Close returned no version")
	}
	r := within(t, read, "the read that was in flight at Close")
	if r.err != nil || r.n != len(payload) || !bytes.Equal(p, payload) {
		t.Fatalf("the read in flight at Close = %d %q (%v), want %q whole", r.n, p, r.err, payload)
	}
	if err := <-closed; err != nil {
		t.Fatalf("Close = %v, want nil: it drained its calls in flight first", err)
	}

	// Every call tried after Close returned is refused as one tried before
	// the drain was over.
	ctx := context.Background()
	if _, err := c.WriteAt(ctx, s1, f1, 0, []byte("late"), 1); !errors.Is(err, core.ErrClosed) {
		t.Errorf("the WriteAt after Close = %v, want ErrClosed", err)
	}
	if _, _, _, _, err := c.ReadAt(ctx, s2, f2, 0, p); !errors.Is(err, core.ErrClosed) {
		t.Errorf("the ReadAt after Close = %v, want ErrClosed", err)
	}
	if err := c.Fill(ctx, s1, f1, 0, []byte("late"), core.Version{}, core.Version{}); !errors.Is(err, core.ErrClosed) {
		t.Errorf("the Fill after Close = %v, want ErrClosed", err)
	}
	if _, err := c.Share(4, 0, core.Version{}); !errors.Is(err, core.ErrClosed) {
		t.Errorf("the Share after Close = %v, want ErrClosed", err)
	}
	if _, err := c.Draw(); !errors.Is(err, core.ErrClosed) {
		t.Errorf("the Draw after Close = %v, want ErrClosed", err)
	}
	if err := c.SetVersionLimit(core.Version{Counter: limitCount}); !errors.Is(err, core.ErrClosed) {
		t.Errorf("the SetVersionLimit after Close = %v, want ErrClosed", err)
	}
}

// seamOps is every operation name the seam counts, so a test can hold all of
// them and compare.
var seamOps = []string{
	kit.OpCreate, kit.OpOpen, kit.OpAllocate, kit.OpWrite, kit.OpRead,
	kit.OpReadDirect, kit.OpSync, kit.OpTruncate, kit.OpClose, kit.OpRename,
	kit.OpRemove, kit.OpList, kit.OpSyncDir,
}

// seamCounts is the seam's call count of every operation, in seamOps order.
func seamCounts(seam *kit.Seam) []int {
	counts := make([]int, len(seamOps))
	for i, op := range seamOps {
		counts[i] = seam.Calls(op)
	}
	return counts
}

// TestReadBegunAfterCloseFails checks the acceptance the fence owes a read
// begun after it: the call is refused with ErrClosed before it touches
// storage, so no operation of the seam counts one more call.
func TestReadBegunAfterCloseFails(t *testing.T) {
	c, seam := newTestCore(t)
	s := mustShare(t, c, 1)
	f := testFile(1)
	// The file holds a warmed record, so a read that reached storage would
	// count at the seam.
	warm(t, c, s, f)

	before := seamCounts(seam)
	c.BeginClose()
	p := make([]byte, 8)
	if _, _, _, _, err := c.ReadAt(context.Background(), s, f, 1<<20, p); !errors.Is(err, core.ErrClosed) {
		t.Fatalf("the ReadAt begun after the fence = %v, want ErrClosed", err)
	}
	if after := seamCounts(seam); !slices.Equal(after, before) {
		t.Fatalf("the refused read touched storage: counts %v before the refusal and %v after, want unchanged", before, after)
	}
}
