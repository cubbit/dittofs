package lockorder_test

import (
	"sync"
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/lockorder"
)

// tryAcquire starts lock on another goroutine and returns the channel its
// completion closes. The caller must let the goroutine finish — by
// releasing what blocks it — and then release the hold it took, since the
// goroutine cannot.
func tryAcquire(lock func()) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		lock()
		close(done)
	}()
	return done
}

// waitBackstop waits for done, failing the test on the generous backstop
// only: a wait long enough to cross proves the event never came, not that
// it was slow.
func waitBackstop(t *testing.T, done <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Error(what)
	}
}

// TestNilRecorder_Excludes exercises the mutexes with no recorder at all: the
// recorder is diagnostic and never part of the locking, so each exclusion
// must hold without one. A lock that excludes is checked non-blocking, its
// release checked by joining the blocked goroutine afterwards.
func TestNilRecorder_Excludes(t *testing.T) {
	t.Run("Mutex", func(t *testing.T) {
		var m lockorder.Mutex
		m.Init(lockorder.FileAppend, nil)
		m.Lock()
		done := tryAcquire(m.Lock)
		select {
		case <-done:
			t.Error("Lock under a held Lock succeeded: the mutex does not exclude")
		default:
		}
		m.Unlock()
		waitBackstop(t, done, "the second Lock never acquired after Unlock: the mutex does not release")
		m.Unlock() // release the hold the other goroutine took
	})

	t.Run("RWMutex blocks a writer", func(t *testing.T) {
		var rw lockorder.RWMutex
		rw.Init(lockorder.FileIndex, nil)
		rw.Lock()
		done := tryAcquire(rw.Lock)
		select {
		case <-done:
			t.Error("Lock under a held Lock succeeded: the writer does not exclude the writer")
		default:
		}
		rw.Unlock()
		waitBackstop(t, done, "the blocked Lock never acquired after Unlock: the writer does not release")
		rw.Unlock()
	})

	t.Run("RWMutex blocks a reader under a writer", func(t *testing.T) {
		var rw lockorder.RWMutex
		rw.Init(lockorder.FileIndex, nil)
		rw.Lock()
		done := tryAcquire(rw.RLock)
		select {
		case <-done:
			t.Error("RLock under a held Lock succeeded: the writer does not exclude the reader")
		default:
		}
		rw.Unlock()
		waitBackstop(t, done, "the blocked RLock never acquired after Unlock: the writer does not release the reader")
		rw.RUnlock()
	})

	t.Run("RWMutex shares between readers", func(t *testing.T) {
		var rw lockorder.RWMutex
		rw.Init(lockorder.FileIndex, nil)
		rw.RLock()
		done := tryAcquire(rw.RLock)
		waitBackstop(t, done, "RLock under a held RLock never completed: the guard does not share between readers")
		rw.RUnlock() // the main goroutine's hold
		rw.RUnlock() // the joined goroutine's hold
	})
}

// TestNilRecorder_Uninit locks an un-Init-ed Mutex and RWMutex: the zero
// value is a working plain mutex, so Init is an option rather than a
// requirement.
func TestNilRecorder_Uninit(t *testing.T) {
	var m lockorder.Mutex
	m.Lock()
	done := tryAcquire(m.Lock)
	select {
	case <-done:
		t.Error("un-Init-ed Lock under a held Lock succeeded: the zero value does not exclude")
	default:
	}
	m.Unlock()
	waitBackstop(t, done, "the blocked un-Init-ed Lock never acquired after Unlock")
	m.Unlock()

	var rw lockorder.RWMutex
	rw.Lock()
	done = tryAcquire(rw.RLock)
	select {
	case <-done:
		t.Error("un-Init-ed RLock under a held Lock succeeded: the zero value does not exclude")
	default:
	}
	rw.Unlock()
	waitBackstop(t, done, "the blocked un-Init-ed RLock never acquired after Unlock")
	rw.RUnlock()
}

func TestNilRecorder_MethodsAreNoOps(t *testing.T) {
	var rec *lockorder.Recorder
	id := new(int)
	rec.Acquire(lockorder.FileAppend, id)
	rec.Release(lockorder.FileAppend, id)
	rec.IO("read")
	rec.AssertNoneHeld("offload")
	rec.AssertNotHeld(lockorder.FileAppend, "wait")
	if got := rec.Held(); got != nil {
		t.Errorf("nil receiver Held() = %v, want nil", got)
	}
	if got := rec.Checks("any"); got != 0 {
		t.Errorf("nil receiver Checks(%q) = %d, want 0", "any", got)
	}
	if got := rec.Violations(); got != nil {
		t.Errorf("nil receiver Violations() = %v, want nil", got)
	}
}

// sink gives each measured critical section a body, which is also what keeps
// staticcheck's empty-critical-section check off the measurement: the body
// allocates nothing, so the count still reflects only the lock pair.
var sink int

func TestNilRecorder_ZeroAllocs(t *testing.T) {
	var m lockorder.Mutex
	m.Init(lockorder.FileAppend, nil)
	m.Lock()
	t.Log("the lock is held: warm-up outside the measured run")
	m.Unlock()
	got := testing.AllocsPerRun(100, func() {
		m.Lock()
		sink++
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
		sink++
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
			sink++
			m.Unlock()
		}
	})
	b.Run("sync.Mutex", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			plain.Lock()
			sink++
			plain.Unlock()
		}
	})
}
