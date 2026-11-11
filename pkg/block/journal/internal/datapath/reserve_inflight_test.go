package datapath

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/capacity"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/durability"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/segment"
)

// TestReserveAhead_InFlightPlainCannotEatTheReservationsEntries pins the gap
// the admission's held-entry check alone leaves: plain writes admitted before a
// Reserve, publishing after it, must not eat the entries it takes. Three plain
// writes are parked between their grant and their publish, with an index bound
// of exactly the three entries a reservation of one write takes; the Reserve
// then either succeeds — and every entry of its write must still publish — or
// is refused naming the index at Reserve. It is never admitted to fail later:
// a reserved write refused LimitIndex part-way is the defect.
func TestReserveAhead_InFlightPlainCannotEatTheReservationsEntries(t *testing.T) {
	cost := capacity.WriteCharge(1, testSegmentCap)
	pieces, spares := capacity.PiecesFor(cost, 1, testSegmentCap)
	var parkN atomic.Int32
	parked := make(chan struct{}, 8)
	release := make(chan struct{})
	c, _ := newTestCore(t, func(o *coreOptions) {
		// The bound is exactly what the reservation of one write takes: three
		// entries, so three plain writes in flight fill it and would starve the
		// reservation's publish if their entries went unheld.
		o.extentLimit = int64(pieces)
		o.contentSpares = spares + 4
		o.beforeAppend = func() {
			if parkN.Add(1) <= int32(pieces) {
				parked <- struct{}{}
				<-release
			}
		}
	})
	s := mustShare(t, c, 1)

	var wg sync.WaitGroup
	for i := range pieces {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			// Each plain write is admitted — Take holds its own entries — and
			// parked before its append, before its publish.
			if _, err := c.WriteAt(context.Background(), s, testFile(byte(50+i)), 0, []byte("p"), 1); err != nil {
				t.Errorf("plain write %d: %v", i, err)
			}
		}(i)
	}
	for range pieces {
		<-parked
	}

	// The reservation is taken while the plain writes hold their entries: it
	// must be refused naming the index here, at Reserve, if it cannot fit.
	r, err := c.Reserve(context.Background(), s, cost, 1)
	if err != nil {
		close(release)
		wg.Wait()
		var ns *core.NoSpaceError
		if !errors.As(err, &ns) || ns.Limit != core.LimitIndex {
			t.Fatalf("Reserve beside the parked plain writes = %v, want either the reservation or a NoSpaceError naming the index", err)
		}
		return // refused at Reserve: the reserved write can never fail part-way
	}
	close(release)
	wg.Wait()

	// The reservation was granted, so every entry of its one write must still
	// publish: the plain writes' entries were held and handed over, never
	// allowed to starve it.
	if _, err := c.WriteReserved(context.Background(), s, r, testFile(9), 0, []byte("r"), 1); err != nil {
		t.Fatalf("the reserved write = %v, want it published: the reservation held its entry", err)
	}
	r.Release()
}

// blockOnce is a Domains that holds the first Lock of any file until the test
// opens the gate, so a write can be parked between its admission and its
// append domain.
type blockOnce struct {
	durability.Domains
	gate *atomic.Pointer[chan struct{}]
	hit  chan struct{}
}

func (b blockOnce) Lock(id core.FileID) func() {
	if ch := b.gate.Swap(nil); ch != nil {
		b.hit <- struct{}{}
		<-*ch
	}
	return b.Domains.Lock(id)
}

// TestReserveAhead_ReservedWriteAppendsInVersionOrder pins the ordering
// guarantee every write of one file keeps: a write's version is drawn under the
// file's append domain, so a reserved write and a plain write of one file
// append in the order their versions were assigned. A reserved write parked
// before its append domain must not carry an older version than a plain write
// that appends first; otherwise the index holds the older record and a
// Since(from=newer) consumer never sees the change.
func TestReserveAhead_ReservedWriteAppendsInVersionOrder(t *testing.T) {
	var gate atomic.Pointer[chan struct{}]
	hit := make(chan struct{}, 1)
	c, _ := newTestCore(t, func(o *coreOptions) {
		o.wrapDomains = func(d durability.Domains) durability.Domains { return blockOnce{d, &gate, hit} }
	})
	s := mustShare(t, c, 1)
	f := testFile(7)
	// A first write takes the file's append domain once, so the parked reserved
	// write is the next Lock the gate holds.
	if _, err := c.WriteAt(context.Background(), s, f, 0, []byte("a"), 1); err != nil {
		t.Fatal(err)
	}
	r := reserve(t, c, s, capacity.WriteCharge(1, testSegmentCap), 1)
	defer r.Release()

	ch := make(chan struct{})
	gate.Store(&ch)
	type res struct {
		v   core.Version
		err error
	}
	out := make(chan res, 1)
	go func() {
		v, err := c.WriteReserved(context.Background(), s, r, f, 1, []byte("r"), 1)
		out <- res{v, err}
	}()
	<-hit // the reserved write is admitted, waiting for the file's append domain

	// The plain write takes the domain first and appends its newer version.
	vp, err := c.WriteAt(context.Background(), s, f, 2, []byte("p"), 1)
	if err != nil {
		t.Fatal(err)
	}
	close(ch)
	got := <-out
	if got.err != nil {
		t.Fatal(got.err)
	}
	if got.v.Counter < vp.Counter {
		t.Fatalf("the reserved write was given version %d, below the plain write's %d drawn while it waited: a later draw must never carry an older version",
			got.v.Counter, vp.Counter)
	}
	// The plain write appended first and the reserved one after it, and each
	// range holds its own record's version: the file's ranges are in the order
	// the versions were assigned, so the newer write is never hidden under the
	// older by a Since(from=older) consumer.
	if pl, ok := c.Index().Lookup(f, 2); !ok || pl.Version.Counter != vp.Counter {
		t.Fatalf("the plain write's range holds version %d (found %v), want its own %d", pl.Version.Counter, ok, vp.Counter)
	}
	if rv, ok := c.Index().Lookup(f, 1); !ok || rv.Version.Counter != got.v.Counter {
		t.Fatalf("the reserved write's range holds version %d (found %v), want its own %d", rv.Version.Counter, ok, got.v.Counter)
	}
}

// TestReserveAhead_RollingPlainHoldsEveryPiece pins that a rolling plain write
// holds every index entry its pieces will publish: a plain write that rolls
// publishes one entry per piece, not the one the pre-check counts, and every
// one of those must be held as pending while it is in flight. One multi-segment
// plain write is parked between its grant and its append with nothing held and
// nothing pending before it, so its count comes from the pre-check alone unless
// Take counts the pieces; a reservation taken while it is parked and then
// granted must still let its own write publish, never refuse it LimitIndex
// part-way.
func TestReserveAhead_RollingPlainHoldsEveryPiece(t *testing.T) {
	cost := capacity.WriteCharge(1, testSegmentCap)
	pieces, spares := capacity.PiecesFor(cost, 1, testSegmentCap)
	parked := make(chan struct{}, 1)
	release := make(chan struct{})
	var once sync.Once
	c, _ := newTestCore(t, func(o *coreOptions) {
		// The bound is the reservation's pieces plus one: the room the single
		// plain write must not be able to spend from outside its own pending
		// count.
		o.extentLimit = int64(pieces) + 1
		o.contentSpares = spares + 8
		o.beforeAppend = func() {
			once.Do(func() { parked <- struct{}{}; <-release })
		}
	})
	s := mustShare(t, c, 1)

	// One plain write of three and a half segments, parked after its grant and
	// before its first append: nothing is held or pending when it takes it. A
	// roomless stream splits it into one piece per empty segment it fills, the
	// same figure the split itself draws.
	payload := int64(3*testSegmentCap + testSegmentCap/2)
	want := (payload-1)/segment.EmptyRoom(testSegmentCap) + 1
	done := make(chan error, 1)
	go func() {
		_, err := c.WriteAt(context.Background(), s, testFile(60), 0, make([]byte, payload), 1)
		done <- err
	}()
	<-parked

	// The pending figure already holds every piece the write will publish, not
	// the one the pre-check counts: a reservation taken here must size against
	// all of them.
	if got := c.PendingEntries(); got != want {
		close(release)
		if err := <-done; err != nil {
			t.Logf("the plain write while the count is short: %v", err)
		}
		t.Fatalf("the parked plain write holds %d pending entries, want the %d pieces its split publishes", got, want)
	}
	r, err := c.Reserve(context.Background(), s, cost, 1)
	if err != nil {
		// A refusal here is correct too: the point is that a granted
		// reservation's write is never refused part-way, and a Reserve refused
		// at once, naming the index, honours that where a smaller held figure
		// would have granted it and starved its write later. Assert it holds
		// nothing, so a refusal that leaked its ask fails here.
		close(release)
		if werr := <-done; werr != nil {
			t.Logf("the plain write worth a refused Reserve: %v", werr)
		}
		var ns *core.NoSpaceError
		if !errors.As(err, &ns) || ns.Limit != core.LimitIndex {
			t.Fatalf("Reserve beside the parked rolling write = %v, want the reservation or a refusal naming the index", err)
		}
		if got := c.ReservedBytes(); got != 0 {
			t.Fatalf("the refused Reserve left ReservedBytes = %d, want 0", got)
		}
		if got := c.HeldEntries(); got != 0 {
			t.Fatalf("the refused Reserve left %d held entries, want 0", got)
		}
		return
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatalf("the plain rolling write: %v", err)
	}
	// The plain write's every piece is the index's now; the reservation's own
	// write must still publish its entry, never be refused part-way.
	if _, err := c.WriteReserved(context.Background(), s, r, testFile(9), 0, []byte("r"), 1); err != nil {
		t.Fatalf("the reserved write after a granted Reserve = %v, want it published: the rolling plain write held every piece", err)
	}
	r.Release()
}
