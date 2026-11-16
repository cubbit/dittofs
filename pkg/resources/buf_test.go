package resources

import (
	"context"
	"errors"
	"net/netip"
	"runtime"
	"runtime/debug"
	"sync"
	"testing"
	"testing/synctest"

	"github.com/cubbit/dittofs/internal/prof"
)

const (
	kib = 1 << 10
	mib = 1 << 20
)

func mustGet(t *testing.T, p *BufPool, n int) *Buf {
	t.Helper()
	b, err := p.Get(t.Context(), client, n)
	if err != nil {
		t.Fatalf("Get(%d): %v", n, err)
	}
	return b
}

// Get draws the smallest class that holds n with its header room, hands out B
// at the length asked and the class's capacity, and charges that capacity
// plus the overhead; past the largest class it charges n exactly.
func TestGetDrawsTheSmallestClassThatHolds(t *testing.T) {
	budget := NewWaitBudget("receive", 64*mib)
	p := NewBufPool(budget)
	for _, c := range []struct {
		name   string
		n      int
		cap    int
		charge int64
	}{
		{"nothing", 0, 4 * kib, 12 * kib},
		{"small", 1, 4 * kib, 12 * kib},
		{"small's edge", 4 * kib, 4 * kib, 12 * kib},
		{"past small", 4*kib + 1, 68 * kib, 76 * kib},
		{"directory's edge", 68 * kib, 68 * kib, 76 * kib},
		{"past directory", 68*kib + 1, mib + 4*kib, mib + 12*kib},
		{"a transfer with its headers", mib + 4*kib, mib + 4*kib, mib + 12*kib},
		{"past transfer", mib + 4*kib + 1, 8*mib + 4*kib, 8*mib + 12*kib},
		{"large's edge", 8*mib + 4*kib, 8*mib + 4*kib, 8*mib + 12*kib},
		{"past large", 8*mib + 4*kib + 1, 8*mib + 4*kib + 1, 8*mib + 12*kib + 1},
	} {
		t.Run(c.name, func(t *testing.T) {
			b := mustGet(t, p, c.n)
			if len(b.B) != c.n || cap(b.B) != c.cap {
				t.Errorf("Get(%d): len %d cap %d, want len %d cap %d", c.n, len(b.B), cap(b.B), c.n, c.cap)
			}
			if got := budget.Reserved(); got != c.charge {
				t.Errorf("Get(%d) charged %d, want %d", c.n, got, c.charge)
			}
			b.Release()
			if got := budget.Reserved(); got != 0 {
				t.Errorf("after Release %d reserved, want 0", got)
			}
		})
	}
	if _, err := p.Get(t.Context(), client, -1); err == nil || budget.Reserved() != 0 {
		t.Errorf("Get(-1) = %v with %d reserved, want a refusal and 0", err, budget.Reserved())
	}
}

// countNew counts the buffers the large class allocates.
func countNew(p *BufPool) *int {
	made := new(int)
	large := &p.free[len(classes)-1]
	alloc := large.New
	large.New = func() any {
		*made++
		return alloc()
	}
	return made
}

// A buffer past the largest class is not pooled: after its release the next
// large Get still allocates.
func TestOversizedBufferIsNotPooled(t *testing.T) {
	if prof.RaceEnabled {
		t.Skip("under the race detector sync.Pool drops some of what it is given")
	}
	defer debug.SetGCPercent(debug.SetGCPercent(-1))
	// Put fills the current P's private slot, which a Get on another P cannot reach.
	defer runtime.GOMAXPROCS(runtime.GOMAXPROCS(1))
	p := NewBufPool(NewWaitBudget("receive", 64*mib))
	made := countNew(p)
	mustGet(t, p, capacity(len(classes)-1)+1).Release()
	mustGet(t, p, 8*mib).Release()
	if *made != 1 {
		t.Fatalf("after an oversized buffer's release, a large Get allocated %d buffers, want 1", *made)
	}
	// The counter does see a pooled buffer reused.
	mustGet(t, p, 8*mib).Release()
	if *made != 1 {
		t.Errorf("a large Get after a large Release allocated again: %d buffers, want 1", *made)
	}
}

// Get waits in the budget and takes no buffer until its reservation is
// granted; a cancelled Get keeps neither.
func TestGetReservesBeforeItTakesABuffer(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const charge = 8*mib + 12*kib
		budget := NewWaitBudget("receive", charge)
		p := NewBufPool(budget)
		made := countNew(p)
		held := mustGet(t, p, 8*mib)

		cancelled, cancel := context.WithCancel(t.Context())
		cancel()
		if _, err := p.Get(cancelled, client, 8*mib); !errors.Is(err, context.Canceled) {
			t.Fatalf("Get on a full budget under a cancelled context = %v, want Canceled", err)
		}
		if *made != 1 || budget.Reserved() != charge {
			t.Fatalf("after the cancelled Get: %d buffers made and %d reserved, want 1 and %d", *made, budget.Reserved(), charge)
		}

		var got *Buf
		done := make(chan struct{})
		go func() {
			defer close(done)
			got, _ = p.Get(t.Context(), client, 8*mib)
		}()
		synctest.Wait()
		if *made != 1 {
			t.Fatalf("a Get blocked on its reservation already took a buffer: %d made, want 1", *made)
		}
		held.Release()
		<-done
		if got == nil || budget.Reserved() != charge {
			t.Fatalf("after the grant: buffer %v and %d reserved; want a buffer and %d", got != nil, budget.Reserved(), charge)
		}
		got.Release()
		if budget.Reserved() != 0 {
			t.Errorf("at the end %d reserved, want 0", budget.Reserved())
		}
	})
}

// A buffer taken from the free list comes zeroed over the length asked,
// whatever its last holder left in it.
func TestGetHandsOutZeroedBytes(t *testing.T) {
	if prof.RaceEnabled {
		t.Skip("under the race detector sync.Pool drops some of what it is given")
	}
	defer debug.SetGCPercent(debug.SetGCPercent(-1))
	// Put fills the current P's private slot, which a Get on another P cannot reach.
	defer runtime.GOMAXPROCS(runtime.GOMAXPROCS(1))
	p := NewBufPool(NewWaitBudget("reply", mib))
	first := mustGet(t, p, 3)
	copy(first.B, "abc")
	first.Release()
	again := mustGet(t, p, 3)
	defer again.Release()
	if again != first {
		t.Fatal("the released buffer was not handed out again, so the test saw no reuse")
	}
	if string(again.B) != "\x00\x00\x00" {
		t.Errorf("a reused buffer reads %q, want zeros", again.B)
	}
}

// A stale Release racing the Get that hands its buffer out again reads the
// buffer only once Get has set it: were the count to leave -1 before res and
// B are written, the race detector would see the stale Release read them.
func TestStaleReleaseSeesAReusedBufferOnlyOnceReady(t *testing.T) {
	if !prof.RaceEnabled {
		t.Skip("the race detector is what sees the order")
	}
	defer debug.SetGCPercent(debug.SetGCPercent(-1))
	// Put fills the current P's private slot, which a Get on another P cannot reach.
	defer runtime.GOMAXPROCS(runtime.GOMAXPROCS(1))
	p := NewBufPool(NewWaitBudget("reply", mib))
	reused := 0
	for range 100 {
		stale := mustGet(t, p, 3)
		stale.Release()
		refused := make(chan bool)
		go func() {
			defer func() { refused <- recover() != nil }()
			stale.Release()
		}()
		again := mustGet(t, p, 3)
		if <-refused {
			again.Release()
		}
		if again == stale {
			reused++
		}
	}
	if reused == 0 {
		t.Fatal("no released buffer was handed out again, so the test saw no reuse")
	}
}

// Holders retaining and releasing one buffer at once leave its count, and its
// budget, where the last Release alone would.
func TestConcurrentRetainAndReleaseKeepTheCount(t *testing.T) {
	budget := NewWaitBudget("reply", mib)
	b := mustGet(t, NewBufPool(budget), 3)
	var wg sync.WaitGroup
	for range 16 {
		wg.Go(func() {
			for range 100 {
				b.Retain().Release()
			}
		})
	}
	wg.Wait()
	b.Release()
	if got := budget.Reserved(); got != 0 {
		t.Errorf("after the holders and the last Release %d reserved, want 0", got)
	}
}

// A retained buffer keeps its bytes and its reservation until its last
// borrower releases it.
func TestBufReturnsAtItsLastRelease(t *testing.T) {
	budget := NewWaitBudget("reply", mib)
	p := NewBufPool(budget)
	b := mustGet(t, p, 3)
	copy(b.B, "abc")
	kept := b.B
	if b.Retain() != b {
		t.Fatal("Retain returned another Buf")
	}
	b.Release()
	if got := budget.Reserved(); got != 12*kib || string(kept) != "abc" || string(b.B) != "abc" {
		t.Fatalf("after the first of two releases: %d reserved, bytes %q; want %d and abc", got, kept, 12*kib)
	}
	b.Release()
	if got := budget.Reserved(); got != 0 {
		t.Errorf("after the last release %d reserved, want 0", got)
	}
}

func panics(f func()) (did bool) {
	defer func() { did = recover() != nil }()
	f()
	return false
}

// One Release more than the holders panics, and so does a Retain after the
// last Release, on a buffer from Get and on a literal; a literal's holders
// pair as a pooled buffer's do.
func TestBufPastItsLastReleasePanics(t *testing.T) {
	p := NewBufPool(NewWaitBudget("reply", mib))
	for name, get := range map[string]func() *Buf{
		"pooled":  func() *Buf { return mustGet(t, p, 3) },
		"literal": func() *Buf { return &Buf{B: []byte("abc")} },
	} {
		t.Run(name, func(t *testing.T) {
			b := get()
			b.Retain()
			for range 2 {
				if panics(b.Release) {
					t.Fatal("a Release by one of two holders panicked")
				}
			}
			if !panics(b.Release) {
				t.Error("a Release after the last did not panic")
			}
			if !panics(func() { b.Retain() }) {
				t.Error("a Retain after the last Release did not panic")
			}
			if !panics(b.Release) {
				t.Error("a Release after the refused Retain did not panic: the Retain revived the buffer")
			}
			if got := p.budget.Reserved(); got != 0 {
				t.Errorf("at the end %d reserved, want 0", got)
			}
		})
	}
}

// A stale Retain refused while its buffer sits released writes nothing, so
// a Get that hands the buffer out in the same moment finds its count intact:
// the new holder's one Release returns it.
func TestRefusedRetainLeavesTheNextHolderItsCount(t *testing.T) {
	budget := NewWaitBudget("reply", mib)
	p := NewBufPool(budget)
	stale := mustGet(t, p, 1)
	stale.Release()

	stop := make(chan struct{})
	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() {
			for {
				select {
				case <-stop:
					return
				default:
				}
				// A Retain that lands while a holder has the buffer is a
				// borrower like any other, and pairs with its Release.
				if !panics(func() { stale.Retain() }) {
					_ = panics(stale.Release)
				}
			}
		})
	}
	for range 20000 {
		b := mustGet(t, p, 1)
		if panics(b.Release) {
			t.Error("the one holder's Release panicked")
			break
		}
	}
	close(stop)
	wg.Wait()
	if got := budget.Reserved(); got != 0 {
		t.Errorf("at the end %d reserved, want 0", got)
	}
}

// Under the race detector the last Release fills the bytes Get took with the
// poison byte, also when the holder pointed B elsewhere first, and never what
// B was pointed at.
func TestReleasePoisonsTheBytesGetTook(t *testing.T) {
	if !prof.RaceEnabled {
		t.Skip("buffers are poisoned only under the race detector")
	}
	p := NewBufPool(NewWaitBudget("reply", 64*mib))
	for name, n := range map[string]int{"pooled": 3, "unpooled": 8*mib + 4*kib + 1} {
		t.Run(name, func(t *testing.T) {
			b := mustGet(t, p, n)
			kept := b.B[:cap(b.B)]
			clear(kept)
			b.Retain().Release()
			if kept[0] != 0 {
				t.Fatal("the bytes were poisoned while a borrower held them")
			}
			other := []byte("elsewhere")
			b.B = other
			b.Release()
			for i, c := range kept {
				if c != poisonByte {
					t.Fatalf("byte %d of %d reads %#x after the last Release, want %#x", i, len(kept), c, poisonByte)
				}
			}
			if string(other) != "elsewhere" {
				t.Errorf("Release poisoned what B was pointed at: %q", other)
			}
		})
	}
}

// A Get on a class with a free buffer, and the Release that returns it,
// allocate nothing.
func TestGetAndReleaseAllocateNothing(t *testing.T) {
	if prof.RaceEnabled {
		t.Skip("the race detector changes allocation counts")
	}
	// Collection would empty the free list under the measurement.
	defer debug.SetGCPercent(debug.SetGCPercent(-1))
	ctx := t.Context()
	p := NewBufPool(NewWaitBudget("receive", 64*mib))
	for class, size := range classes {
		allocs := testing.AllocsPerRun(100, func() {
			b, err := p.Get(ctx, client, size)
			if err != nil {
				t.Fatal(err)
			}
			b.Release()
		})
		if allocs != 0 {
			t.Errorf("class %d: a Get and Release allocate %v times, want 0", class, allocs)
		}
	}
}

// TryGet takes Get's charge when it fits with no waiter ahead, and refuses at
// once, charging nothing, a charge past what is left or one behind a waiter;
// Ceiling reads the budget's ceiling.
func TestTryGetTakesOnlyWhatFitsWithNoWaiter(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const ceiling = 100 * kib
		budget := NewWaitBudget("reply", ceiling)
		p := NewBufPool(budget)
		if got := p.Ceiling(); got != ceiling {
			t.Fatalf("Ceiling() = %d, want the budget's %d", got, ceiling)
		}
		const n = 4*kib + 1
		got := mustGet(t, p, n)
		charge := budget.Reserved()
		got.Release()

		b, ok := p.TryGet(client, n)
		if !ok || len(b.B) != n || budget.Reserved() != charge {
			t.Fatalf("TryGet(%d) on an empty budget: ok %v, %d reserved; want a buffer of %d bytes charged %d as Get charges it",
				n, ok, budget.Reserved(), n, charge)
		}
		// 76 KiB of 100 are reserved: a second directory-class charge does not fit.
		if _, ok := p.TryGet(client, n); ok || budget.Reserved() != charge {
			t.Errorf("TryGet past what is left: ok %v, %d reserved; want a refusal and %d", ok, budget.Reserved(), charge)
		}
		// 24 KiB are left, and a small charge of 12 KiB fits, but not behind
		// a waiter.
		w := startWaiting(t, t.Context(), budget, 76*kib)
		if _, ok := p.TryGet(client, 3); ok || budget.Reserved() != charge {
			t.Errorf("TryGet behind a waiter: ok %v, %d reserved; want a refusal and %d", ok, budget.Reserved(), charge)
		}
		b.Release()
		synctest.Wait()
		if !w.returned() || budget.Reserved() != 76*kib {
			t.Fatalf("after the release the waiter returned %v with %d reserved; want it granted at %d", w.returned(), budget.Reserved(), 76*kib)
		}
		w.res.Release()
		if _, ok := p.TryGet(client, -1); ok || budget.Reserved() != 0 {
			t.Errorf("TryGet(-1): ok %v with %d reserved, want a refusal and 0", ok, budget.Reserved())
		}
	})
}

// A refused TryGet allocates nothing: it builds no error.
func TestTryGetRefusalAllocatesNothing(t *testing.T) {
	if prof.RaceEnabled {
		t.Skip("the race detector changes allocation counts")
	}
	p := NewBufPool(NewWaitBudget("reply", 12*kib))
	held := mustGet(t, p, 3)
	defer held.Release()
	allocs := testing.AllocsPerRun(100, func() {
		if _, ok := p.TryGet(client, 3); ok {
			t.Fatal("TryGet on a full budget took a buffer")
		}
	})
	if allocs != 0 {
		t.Errorf("a refused TryGet allocates %v times, want 0", allocs)
	}
}

// A Recharge that fits moves the buffer's whole charge, class and overhead,
// from the receive budget to the hold budget, where the last Release returns
// it; one that does not fit returns an error and leaves both budgets as they
// were.
func TestRechargeMovesTheWholeCharge(t *testing.T) {
	const n, charge = 4*kib + 1, 76 * kib
	for _, c := range []struct {
		name                     string
		hold                     int64
		fits                     bool
		wantReceive, wantHolding int64
	}{
		{"fits", charge, true, 0, charge},
		{"refused", charge - 1, false, charge, 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			receive, hold := NewWaitBudget("receive", mib), NewBudget("hold", c.hold)
			b := mustGet(t, NewBufPool(receive), n)
			err := b.Recharge(hold)
			if (err == nil) != c.fits {
				t.Fatalf("Recharge into a hold budget of %d = %v, want fits %v", c.hold, err, c.fits)
			}
			if receive.Reserved() != c.wantReceive || hold.Reserved() != c.wantHolding || b.Charge() != charge {
				t.Errorf("after Recharge: receive %d, hold %d, Charge %d; want %d, %d, %d",
					receive.Reserved(), hold.Reserved(), b.Charge(), c.wantReceive, c.wantHolding, charge)
			}
			b.Release()
			if receive.Reserved() != 0 || hold.Reserved() != 0 {
				t.Errorf("after the last Release: receive %d, hold %d; want 0 and 0", receive.Reserved(), hold.Reserved())
			}
		})
	}
}

// Recharge reserves from the new budget before it releases the old: refused
// by a full hold budget, it never lets the receive charge go, so a waiter
// queued behind it on the receive budget is not granted.
func TestRechargeReservesBeforeItReleases(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const charge = 12 * kib
		receive, hold := NewWaitBudget("receive", charge), NewBudget("hold", charge)
		b := mustGet(t, NewBufPool(receive), 3)
		full, err := hold.Reserve(charge)
		if err != nil {
			t.Fatal(err)
		}
		w := startWaiting(t, t.Context(), receive, charge)
		if err := b.Recharge(hold); err == nil {
			t.Fatal("Recharge into a full hold budget succeeded")
		}
		synctest.Wait()
		if w.returned() || receive.Reserved() != charge || b.Charge() != charge {
			t.Fatalf("after the refused Recharge: the receive waiter returned %v, receive %d, Charge %d; want it waiting, %d and %d",
				w.returned(), receive.Reserved(), b.Charge(), charge, charge)
		}
		b.Release()
		synctest.Wait()
		w.res.Release()
		full.Release()
		if receive.Reserved() != 0 || hold.Reserved() != 0 {
			t.Errorf("at the end: receive %d, hold %d; want 0 and 0", receive.Reserved(), hold.Reserved())
		}
	})
}

// Charge is what Get reserves for a buffer of n bytes: its class's capacity
// and the overhead, or n and the overhead past the largest class.
func TestChargeIsWhatGetReserves(t *testing.T) {
	for _, n := range []int{0, 4 << 10, 1 << 20, 9 << 20} {
		b := NewWaitBudget("t", 1<<30)
		p := NewBufPool(b)
		buf, err := p.Get(context.Background(), netip.Addr{}, n)
		if err != nil {
			t.Fatal(err)
		}
		if got, want := p.Charge(n), b.Reserved(); got != want {
			t.Errorf("Charge(%d) = %d, Get reserved %d", n, got, want)
		}
		buf.Release()
	}
}
