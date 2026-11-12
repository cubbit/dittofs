package lockorder_test

import (
	"sync"
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/lockorder"
)

// backstop bounds a wait that must end: crossing it proves the event never
// came, not that it was slow.
const backstop = 10 * time.Second

// countUnder runs workers goroutines that each increment a plain, non-atomic
// counter iters times inside lock, and returns the total. The race
// detector is the check that lock excludes: any interleaving of two
// goroutines inside it reports a data race, and lost increments would
// also leave the total short.
func countUnder(lock, unlock func(), workers, iters int) int {
	var n int
	var wg sync.WaitGroup
	wg.Add(workers)
	for range workers {
		go func() {
			defer wg.Done()
			for range iters {
				lock()
				n++
				unlock()
			}
		}()
	}
	wg.Wait()
	return n
}

// TestNilRecorder_Excludes exercises the mutexes with no recorder at all:
// the recorder is diagnostic and never part of the locking, so each
// exclusion must hold without one, which the race detector plus the
// counted totals check.
func TestNilRecorder_Excludes(t *testing.T) {
	t.Run("Mutex excludes", func(t *testing.T) {
		var m lockorder.Mutex
		m.Init(lockorder.FileAppend, nil)
		if got, want := countUnder(m.Lock, m.Unlock, 8, 200), 8*200; got != want {
			t.Errorf("counted %d increments under the lock, want %d: the mutex lost updates, so it does not exclude", got, want)
		}
	})

	t.Run("RWMutex writer excludes writer", func(t *testing.T) {
		var rw lockorder.RWMutex
		rw.Init(lockorder.FileIndex, nil)
		if got, want := countUnder(rw.Lock, rw.Unlock, 8, 200), 8*200; got != want {
			t.Errorf("counted %d increments under the write lock, want %d: the writer does not exclude the writer", got, want)
		}
	})

	t.Run("RWMutex writer excludes reader", func(t *testing.T) {
		var rw lockorder.RWMutex
		rw.Init(lockorder.FileIndex, nil)
		// Writers increment a plain counter; readers read it under the
		// guard. Every increment must land — a reader inside an
		// exclusive hold pairs with a writer at the same moment, which
		// the race detector reports and the total catches — and the
		// readers' own concurrent reads of one another prove sharing.
		var n int
		var wg sync.WaitGroup
		wg.Add(8)
		for i := range 8 {
			go func() {
				defer wg.Done()
				for range 200 {
					if i%2 == 0 {
						rw.Lock()
						n++
						rw.Unlock()
					} else {
						rw.RLock()
						_ = n
						rw.RUnlock()
					}
				}
			}()
		}
		wg.Wait()
		if got, want := n, 4*200; got != want {
			t.Errorf("counted %d writer increments, want %d: the writer lost updates while readers ran", got, want)
		}
	})

	t.Run("RWMutex shares between readers", func(t *testing.T) {
		var rw lockorder.RWMutex
		rw.Init(lockorder.FileIndex, nil)
		// The rendezvous completes only with both readers inside the
		// guard at once, which is sharing itself; a writer-preferring
		// or exclusive guard leaves the second reader waiting and the
		// backstop fails.
		r1In, r2In := make(chan struct{}), make(chan struct{})
		release1, release2 := make(chan struct{}), make(chan struct{})
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			rw.RLock()
			defer rw.RUnlock()
			close(r1In)
			<-release1
		}()
		go func() {
			defer wg.Done()
			rw.RLock()
			defer rw.RUnlock()
			close(r2In)
			<-release2
		}()
		for i, in := range []<-chan struct{}{r1In, r2In} {
			select {
			case <-in:
			case <-time.After(backstop):
				t.Errorf("reader %d never entered the guard while reader 1 held it: the guard does not share between readers", i+1)
				return
			}
		}
		close(release1)
		close(release2)
		wg.Wait()
	})
}

// TestNilRecorder_AcquiresAfterRelease checks the other half of a working
// lock: a goroutine blocked on a held lock must take it over once the
// holder releases, and a block on one hold may not wedge the lock.
func TestNilRecorder_AcquiresAfterRelease(t *testing.T) {
	t.Run("Mutex", func(t *testing.T) {
		var m lockorder.Mutex
		m.Init(lockorder.FileAppend, nil)
		m.Lock()
		done := make(chan struct{})
		go func() {
			m.Lock()
			close(done)
		}()
		m.Unlock()
		select {
		case <-done:
			m.Unlock() // release the hold the other goroutine took over
		case <-time.After(backstop):
			t.Error("the blocked Lock never acquired after Unlock: the mutex does not release")
		}
	})

	t.Run("RWMutex reader under writer, un-Init-ed", func(t *testing.T) {
		// The zero value is a working plain mutex, so Init is an option
		// rather than a requirement.
		var rw lockorder.RWMutex
		rw.Lock()
		done := make(chan struct{})
		go func() {
			rw.RLock()
			close(done)
		}()
		rw.Unlock()
		select {
		case <-done:
			rw.RUnlock()
		case <-time.After(backstop):
			t.Error("the blocked un-Init-ed RLock never acquired after Unlock")
		}
	})
}

func TestNilRecorder_MethodsAreNoOps(t *testing.T) {
	var rec *lockorder.Recorder
	id := new(int)
	rec.Acquire(lockorder.FileAppend, id)
	rec.Release(id)
	rec.IO("read")
	rec.AssertNoneHeld("offload")
	rec.AssertNotHeld(lockorder.FileAppend, "wait")
	if got := rec.Held(); got != nil {
		t.Errorf("nil receiver Held() = %v, want nil", got)
	}
	if got, want := rec.Checks("any"), 0; got != want {
		t.Errorf("nil receiver Checks(%q) = %d, want %d", "any", got, want)
	}
	if got := rec.Violations(); got != nil {
		t.Errorf("nil receiver Violations() = %v, want nil", got)
	}
}

// sinkAbsorbs gives each measured critical section a body, which is also
// what keeps staticcheck's empty-critical-section check off the
// measurement: the body allocates nothing, so the count still reflects
// only the lock pair.
var sinkAbsorbs int

func TestNilRecorder_ZeroAllocs(t *testing.T) {
	var m lockorder.Mutex
	m.Init(lockorder.FileAppend, nil)
	m.Lock()
	t.Log("the lock is held: warm-up outside the measured run")
	m.Unlock()
	got := testing.AllocsPerRun(100, func() {
		m.Lock()
		sinkAbsorbs++
		m.Unlock()
	})
	if got > 0 {
		t.Errorf("Lock+Unlock with a nil recorder allocated %v times per run, want 0", got)
	}

	var rw lockorder.RWMutex
	rw.Init(lockorder.FileAppend, nil)
	rw.RLock()
	t.Log("the read lock is held: warm-up outside the measured run")
	rw.RUnlock()
	got = testing.AllocsPerRun(100, func() {
		rw.RLock()
		sinkAbsorbs++
		rw.RUnlock()
	})
	if got > 0 {
		t.Errorf("RLock+RUnlock with a nil recorder allocated %v times per run, want 0", got)
	}
}

func BenchmarkLockNilRecorder(b *testing.B) {
	var m lockorder.Mutex
	m.Init(lockorder.Capacity, nil)
	var plain sync.Mutex
	b.Run("ranked, nil recorder", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			m.Lock()
			sinkAbsorbs++
			m.Unlock()
		}
	})
	b.Run("sync.Mutex", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			plain.Lock()
			sinkAbsorbs++
			plain.Unlock()
		}
	})
}
