package resources

import (
	"context"
	"errors"
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
					stale.Release()
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
