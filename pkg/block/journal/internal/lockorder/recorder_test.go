package lockorder_test

import (
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/lockorder"
)

// kinds returns the violation kinds in order, nil when there are none, so a
// want of nil compares equal.
func kinds(vs []lockorder.Violation) []lockorder.Kind {
	if len(vs) == 0 {
		return nil
	}
	out := make([]lockorder.Kind, len(vs))
	for i, v := range vs {
		out[i] = v.Kind
	}
	return out
}

// The exclusive-lock checks wait for events that must happen; the backstop
// constant is declared in lockorder_test.go.

func TestRanks_Order(t *testing.T) {
	order := []lockorder.Rank{
		lockorder.FileAppend,
		lockorder.Capacity,
		lockorder.FileIndex,
		lockorder.FileMap,
		lockorder.SegmentAppend,
		lockorder.StorageGuard,
	}
	for i := 1; i < len(order); i++ {
		if order[i-1] >= order[i] {
			t.Errorf("rank %s (%d) does not come before %s (%d) in the total order",
				order[i-1], order[i-1], order[i], order[i])
		}
	}
	if got, want := lockorder.Rank(0).String(), "unranked"; got != want {
		t.Errorf("Rank(0).String() = %q, want %q", got, want)
	}
	names := map[lockorder.Rank]string{
		lockorder.FileAppend:    "file append",
		lockorder.Capacity:      "capacity",
		lockorder.FileIndex:     "file index",
		lockorder.FileMap:       "file map",
		lockorder.SegmentAppend: "segment append",
		lockorder.StorageGuard:  "storage guard",
	}
	for rank, want := range names {
		if got := rank.String(); got != want {
			t.Errorf("Rank(%d).String() = %q, want %q", rank, got, want)
		}
	}
}

func TestRecorder_Reports(t *testing.T) {
	tests := []struct {
		name      string
		drive     func(t *testing.T, rec *lockorder.Recorder)
		wantKinds []lockorder.Kind
		wantRank  lockorder.Rank
	}{
		{
			// Nesting all six is in order, so no out-of-order and no
			// same-domain violation — but the file map permits nothing
			// acquired under it and the file index must be gone before
			// the storage guard, so the full nest still reports the
			// segment append taken under the map and the storage guard
			// taken under the index.
			name: "all six in order, released in reverse",
			drive: func(t *testing.T, rec *lockorder.Recorder) {
				var mus [6]lockorder.Mutex
				for i, rank := range []lockorder.Rank{
					lockorder.FileAppend,
					lockorder.Capacity,
					lockorder.FileIndex,
					lockorder.FileMap,
					lockorder.SegmentAppend,
					lockorder.StorageGuard,
				} {
					mus[i].Init(rank, rec)
					mus[i].Lock()
				}
				for i := len(mus) - 1; i >= 0; i-- {
					mus[i].Unlock()
				}
				if got, want := rec.Held(), ([]lockorder.Rank)(nil); !reflect.DeepEqual(got, want) {
					t.Errorf("Held() after the releases = %v, want none", got)
				}
			},
			wantKinds: []lockorder.Kind{lockorder.AcquireUnderMap, lockorder.GuardUnderIndex},
		},
		{
			name: "capacity then file append",
			drive: func(t *testing.T, rec *lockorder.Recorder) {
				var capMu, faMu lockorder.Mutex
				capMu.Init(lockorder.Capacity, rec)
				faMu.Init(lockorder.FileAppend, rec)
				capMu.Lock()
				faMu.Lock()
			},
			wantKinds: []lockorder.Kind{lockorder.OutOfOrder},
			wantRank:  lockorder.FileAppend,
		},
		{
			name: "two file-append mutexes",
			drive: func(t *testing.T, rec *lockorder.Recorder) {
				var first, second lockorder.Mutex
				first.Init(lockorder.FileAppend, rec)
				second.Init(lockorder.FileAppend, rec)
				first.Lock()
				second.Lock()
			},
			wantKinds: []lockorder.Kind{lockorder.SameDomain},
			wantRank:  lockorder.FileAppend,
		},
		{
			name: "two file-index read locks",
			drive: func(t *testing.T, rec *lockorder.Recorder) {
				var first, second lockorder.RWMutex
				first.Init(lockorder.FileIndex, rec)
				second.Init(lockorder.FileIndex, rec)
				first.RLock()
				second.RLock()
			},
			wantKinds: []lockorder.Kind{lockorder.SameDomain},
			wantRank:  lockorder.FileIndex,
		},
		{
			name: "one mutex locked twice by one goroutine",
			drive: func(t *testing.T, rec *lockorder.Recorder) {
				var m lockorder.Mutex
				m.Init(lockorder.FileAppend, rec)
				m.Lock()
				// The second Lock self-deadlocks once its attempt is
				// recorded, which is why the attempt is recorded
				// before the wait. Drive the recorder side of that
				// attempt — the same call Lock makes before it
				// blocks, on the identity it holds — and the release
				// the deadlocked lock never reaches.
				rec.Acquire(lockorder.FileAppend, &m)
			},
			wantKinds: []lockorder.Kind{lockorder.SameDomain},
			wantRank:  lockorder.FileAppend,
		},
		{
			name: "read lock then write lock on one rw mutex",
			drive: func(t *testing.T, rec *lockorder.Recorder) {
				var rw lockorder.RWMutex
				rw.Init(lockorder.StorageGuard, rec)
				rw.RLock()
				// A reader taking its own guard exclusively
				// self-deadlocks once the write attempt is recorded.
				// Drive the recorder side of that attempt — the same
				// call Lock makes before it blocks, on the identity
				// the write hold records itself under.
				rec.Acquire(lockorder.StorageGuard, &rw)
			},
			wantKinds: []lockorder.Kind{lockorder.SameDomain},
			wantRank:  lockorder.StorageGuard,
		},
		{
			name: "nested read locks of one rw mutex",
			drive: func(t *testing.T, rec *lockorder.Recorder) {
				var rw lockorder.RWMutex
				rw.Init(lockorder.StorageGuard, rec)
				rw.RLock()
				rw.RLock() // readers share: the one re-entrant hold allowed
				if got, want := rec.Held(), []lockorder.Rank{lockorder.StorageGuard, lockorder.StorageGuard}; !reflect.DeepEqual(got, want) {
					t.Errorf("Held() while both read holds are nested = %v, want %v: one release must balance one hold", got, want)
				}
				rw.RUnlock()
				rw.RUnlock()
			},
		},
		{
			name: "rank zero is never recorded",
			drive: func(t *testing.T, rec *lockorder.Recorder) {
				var m lockorder.Mutex
				m.Init(0, rec)
				m.Lock()
				var rw lockorder.RWMutex
				rw.Init(0, rec)
				rw.RLock()
				if got, want := rec.Held(), ([]lockorder.Rank)(nil); !reflect.DeepEqual(got, want) {
					t.Errorf("Held() while two rank-zero locks are held = %v, want none: an unranked lock is never recorded", got)
				}
				rw.RUnlock()
				m.Unlock()
			},
		},
		{
			name: "storage guard under file index",
			drive: func(t *testing.T, rec *lockorder.Recorder) {
				idx, guard := new(int), new(int)
				rec.Acquire(lockorder.FileIndex, idx)
				rec.Acquire(lockorder.StorageGuard, guard)
			},
			wantKinds: []lockorder.Kind{lockorder.GuardUnderIndex},
			wantRank:  lockorder.StorageGuard,
		},
		{
			name: "segment append under file map",
			drive: func(t *testing.T, rec *lockorder.Recorder) {
				var mapMu, segMu lockorder.Mutex
				mapMu.Init(lockorder.FileMap, rec)
				segMu.Init(lockorder.SegmentAppend, rec)
				mapMu.Lock()
				segMu.Lock()
			},
			wantKinds: []lockorder.Kind{lockorder.AcquireUnderMap},
			wantRank:  lockorder.SegmentAppend,
		},
		{
			name: "io under file index",
			drive: func(t *testing.T, rec *lockorder.Recorder) {
				var m lockorder.Mutex
				m.Init(lockorder.FileIndex, rec)
				m.Lock()
				rec.IO("read")
			},
			wantKinds: []lockorder.Kind{lockorder.IOUnderIndex},
			wantRank:  lockorder.FileIndex,
		},
		{
			name: "io under file append alone",
			drive: func(t *testing.T, rec *lockorder.Recorder) {
				var m lockorder.Mutex
				m.Init(lockorder.FileAppend, rec)
				m.Lock()
				rec.IO("read")
			},
		},
		{
			name: "io under segment append",
			drive: func(t *testing.T, rec *lockorder.Recorder) {
				var m lockorder.Mutex
				m.Init(lockorder.SegmentAppend, rec)
				m.Lock()
				rec.IO("read")
			},
		},
		{
			name: "assert none held with capacity held",
			drive: func(t *testing.T, rec *lockorder.Recorder) {
				var m lockorder.Mutex
				m.Init(lockorder.Capacity, rec)
				m.Lock()
				rec.AssertNoneHeld("offload")
			},
			wantKinds: []lockorder.Kind{lockorder.HeldAtCallback},
			wantRank:  lockorder.Capacity,
		},
		{
			name: "assert none held with nothing held",
			drive: func(t *testing.T, rec *lockorder.Recorder) {
				rec.AssertNoneHeld("offload")
			},
		},
		{
			name: "assert not held with it held",
			drive: func(t *testing.T, rec *lockorder.Recorder) {
				var m lockorder.Mutex
				m.Init(lockorder.FileAppend, rec)
				m.Lock()
				rec.AssertNotHeld(lockorder.FileAppend, "removal wait")
			},
			wantKinds: []lockorder.Kind{lockorder.HeldAtWait},
			wantRank:  lockorder.FileAppend,
		},
		{
			name: "assert not held after release",
			drive: func(t *testing.T, rec *lockorder.Recorder) {
				var m lockorder.Mutex
				m.Init(lockorder.FileAppend, rec)
				m.Lock()
				rec.IO("read") // the hold does real work before its release
				m.Unlock()
				rec.AssertNotHeld(lockorder.FileAppend, "removal wait")
			},
		},
		{
			name: "release in non-lifo order",
			drive: func(t *testing.T, rec *lockorder.Recorder) {
				a, b, c := new(int), new(int), new(int)
				rec.Acquire(lockorder.FileAppend, a)
				rec.Acquire(lockorder.Capacity, b)
				rec.Acquire(lockorder.FileIndex, c)
				rec.Release(lockorder.Capacity, b)
				rec.Release(lockorder.FileIndex, c)
				rec.Release(lockorder.FileAppend, a)
				if got, want := rec.Held(), ([]lockorder.Rank)(nil); !reflect.DeepEqual(got, want) {
					t.Errorf("Held() after non-LIFO releases = %v, want none", got)
				}
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec := lockorder.NewRecorder()
			tc.drive(t, rec)
			vs := rec.Violations()
			if got, want := kinds(vs), tc.wantKinds; !reflect.DeepEqual(got, want) {
				t.Errorf("violation kinds = %v, want %v", got, want)
			} else if len(tc.wantKinds) == 1 {
				if got, want := vs[0].Stack, ""; got == want {
					t.Errorf("violation Stack is empty: it must name the acquisition that broke the rule")
				}
				if got, want := vs[0].Rank, tc.wantRank; got != want {
					t.Errorf("first violation Rank = %v, want %v", got, want)
				}
			}
		})
	}
}

func TestRecorder_PerGoroutine(t *testing.T) {
	rec := lockorder.NewRecorder()
	var capMu, faMu, fiMu lockorder.Mutex
	capMu.Init(lockorder.Capacity, rec)
	faMu.Init(lockorder.FileAppend, rec)
	fiMu.Init(lockorder.FileIndex, rec)

	capHeld := make(chan struct{})
	bDone := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		capMu.Lock()
		close(capHeld)
		<-bDone
		if got, want := rec.Held(), []lockorder.Rank{lockorder.Capacity}; !reflect.DeepEqual(got, want) {
			t.Errorf("goroutine A Held() = %v, want %v", got, want)
		}
		capMu.Unlock()
	}()
	go func() {
		defer wg.Done()
		<-capHeld
		faMu.Lock()
		fiMu.Lock()
		want := []lockorder.Rank{lockorder.FileAppend, lockorder.FileIndex}
		if got := rec.Held(); !reflect.DeepEqual(got, want) {
			t.Errorf("goroutine B Held() = %v, want %v", got, want)
		}
		fiMu.Unlock()
		faMu.Unlock()
		close(bDone)
	}()
	wg.Wait()
	if got, want := rec.Held(), ([]lockorder.Rank)(nil); !reflect.DeepEqual(got, want) {
		t.Errorf("main goroutine Held() after both = %v, want none", got)
	}
	if got, want := kinds(rec.Violations()), ([]lockorder.Kind)(nil); !reflect.DeepEqual(got, want) {
		t.Errorf("violation kinds = %v, want none", got)
	}
}

func TestRecorder_UnlockOnOtherGoroutine(t *testing.T) {
	rec := lockorder.NewRecorder()
	var m lockorder.Mutex
	m.Init(lockorder.FileAppend, rec)

	locked := make(chan struct{})
	unlocked := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		m.Lock()
		close(locked)
		<-unlocked
		if got, want := rec.Held(), ([]lockorder.Rank)(nil); !reflect.DeepEqual(got, want) {
			t.Errorf("goroutine A Held() after B's unlock = %v, want none", got)
		}
	}()
	go func() {
		defer wg.Done()
		<-locked
		m.Unlock()
		close(unlocked)
	}()
	wg.Wait()
	if got, want := kinds(rec.Violations()), ([]lockorder.Kind)(nil); !reflect.DeepEqual(got, want) {
		t.Errorf("violation kinds = %v, want none", got)
	}
}

func TestRecorder_InstalledLate(t *testing.T) {
	var m lockorder.Mutex
	m.Init(lockorder.FileAppend, nil)
	m.Lock()

	rec := lockorder.NewRecorder()
	m.Init(lockorder.FileAppend, rec)
	m.Unlock()

	if got, want := kinds(rec.Violations()), ([]lockorder.Kind)(nil); !reflect.DeepEqual(got, want) {
		t.Errorf("violation kinds = %v, want none: the recorder never saw the held lock, so the release names a hold it cannot find", got)
	}
	if got, want := rec.Held(), ([]lockorder.Rank)(nil); !reflect.DeepEqual(got, want) {
		t.Errorf("Held() after unlock = %v, want none", got)
	}
}

// TestRecorder_ContendedMutex walks a contended handover of one Mutex from
// goroutine A to goroutine B: B's attempt is recorded while A still holds,
// then B acquires, and each releases in turn. The books must end empty
// whichever side of the handover crossed whose.
func TestRecorder_ContendedMutex(t *testing.T) {
	rec := lockorder.NewRecorder()
	var m lockorder.Mutex
	m.Init(lockorder.FileAppend, rec)

	bHeld := make(chan []lockorder.Rank)
	bDone := make(chan struct{})
	bRelease := make(chan struct{})
	m.Lock()    // A holds first, so B's attempt is recorded under A's hold
	go func() { // B: contends with A for the lock
		defer close(bDone)
		defer m.Unlock()
		m.Lock() // records its attempt, waits out A, then acquires
		bHeld <- rec.Held()
		<-bRelease
	}()
	m.Unlock() // the handover: A releases under B's recorded attempt
	gotB := <-bHeld
	if want := []lockorder.Rank{lockorder.FileAppend}; !reflect.DeepEqual(gotB, want) {
		t.Errorf("B's Held() while it holds the lock = %v, want %v: the handover lost B's hold", gotB, want)
	}
	if got, want := rec.Held(), ([]lockorder.Rank)(nil); !reflect.DeepEqual(got, want) {
		t.Errorf("A's Held() after its own release = %v, want none: the handover left A's hold behind", got)
	}
	close(bRelease)
	<-bDone
	if got, want := kinds(rec.Violations()), ([]lockorder.Kind)(nil); !reflect.DeepEqual(got, want) {
		t.Errorf("violation kinds = %v, want none: contended hands-over of one mutex break no rule", got)
	}
}

// TestRecorder_ContendedAttemptUnderHold pins the bookkeeping crossing a
// contended handover makes, deterministically: a mutex gives no window to
// observe that the attempt is recorded and B still waits, so the low-level
// pair — the exact calls Lock and Unlock make, on the identity the mutex
// records itself under — drives the crossing in a fixed order. B's attempt
// is on the books while A holds; A releases; B releases.
func TestRecorder_ContendedAttemptUnderHold(t *testing.T) {
	rec := lockorder.NewRecorder()
	m := new(int) // stands in for the lock identity the mutex passes

	aHeld := make(chan struct{})
	aRelease := make(chan struct{})
	bAttempted := make(chan struct{})
	aHeldAfter, bHeldAfter := make(chan []lockorder.Rank, 1), make(chan []lockorder.Rank, 1)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { // A: holds, then releases under B's recorded attempt
		defer wg.Done()
		rec.Acquire(lockorder.FileAppend, m)
		close(aHeld)
		<-aRelease
		rec.Release(lockorder.FileAppend, m)
		aHeldAfter <- rec.Held()
	}()
	go func() { // B: attempt recorded while A holds, then released
		defer wg.Done()
		<-aHeld
		rec.Acquire(lockorder.FileAppend, m)
		close(bAttempted)
		rec.Release(lockorder.FileAppend, m)
		bHeldAfter <- rec.Held()
	}()

	<-aHeld
	<-bAttempted // the crossing exists on the books now
	close(aRelease)
	wg.Wait()
	if got, want := <-aHeldAfter, ([]lockorder.Rank)(nil); !reflect.DeepEqual(got, want) {
		t.Errorf("A's Held() after its release = %v, want none: B's attempt must not make A's hold stick", got)
	}
	if got, want := <-bHeldAfter, ([]lockorder.Rank)(nil); !reflect.DeepEqual(got, want) {
		t.Errorf("B's Held() after its release = %v, want none", got)
	}
	if got, want := kinds(rec.Violations()), ([]lockorder.Kind)(nil); !reflect.DeepEqual(got, want) {
		t.Errorf("violation kinds = %v, want none: an attempt that waits out another holder breaks no rule", got)
	}
}

// TestRecorder_SharedReadLocks walks two goroutines holding one RWMutex as
// readers at the same time — the sharing the guard exists for — with each
// releasing in turn. One reader's release must not remove the other's hold,
// and a third goroutine's callback assertion must find nothing held once
// both are out.
func TestRecorder_SharedReadLocks(t *testing.T) {
	rec := lockorder.NewRecorder()
	var rw lockorder.RWMutex
	rw.Init(lockorder.StorageGuard, rec)

	// The rendezvous completes only with both readers inside the guard
	// together, which is sharing itself. Each reader answers a request
	// for its Held() with a send, so every step of the test waits on an
	// event rather than an ordering.
	r1In, r2In := make(chan struct{}), make(chan struct{})
	held := func() (chan []lockorder.Rank, chan struct{}) {
		return make(chan []lockorder.Rank), make(chan struct{})
	}
	r1Held, r1Leave := held()
	r2Held, r2Leave := held()
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		rw.RLock()
		close(r1In)
		r1Held <- rec.Held()
		<-r1Leave
		rw.RUnlock()
		r1Held <- rec.Held()
	}()
	go func() {
		defer wg.Done()
		rw.RLock()
		close(r2In)
		r2Held <- rec.Held()
		r2Held <- rec.Held() // still holding while reader one is gone
		<-r2Leave
		rw.RUnlock()
	}()

	for i, in := range []<-chan struct{}{r1In, r2In} {
		select {
		case <-in:
		case <-time.After(backstop):
			t.Fatalf("reader %d never entered the guard within the backstop: the guard does not share between readers", i+1)
		}
	}

	guard := []lockorder.Rank{lockorder.StorageGuard}
	if got := <-r1Held; !reflect.DeepEqual(got, guard) {
		t.Errorf("reader one's Held() while it shares the guard = %v, want %v", got, guard)
	}
	if got := <-r2Held; !reflect.DeepEqual(got, guard) {
		t.Errorf("reader two's Held() while it shares the guard = %v, want %v", got, guard)
	}

	// Reader one leaves and reports empty; reader two, still inside,
	// must keep its own hold.
	close(r1Leave)
	if got, want := <-r1Held, ([]lockorder.Rank)(nil); !reflect.DeepEqual(got, want) {
		t.Errorf("reader one's Held() after its release = %v, want none", got)
	}
	if got := <-r2Held; !reflect.DeepEqual(got, guard) {
		t.Errorf("reader two's Held() after reader one left = %v, want %v: one reader's release must not drop the other's hold", got, guard)
	}

	// Reader two leaves; the books must end empty and a goroutine that
	// holds nothing passes the callback assertion.
	close(r2Leave)
	wg.Wait()
	if got, want := kinds(rec.Violations()), ([]lockorder.Kind)(nil); !reflect.DeepEqual(got, want) {
		t.Errorf("violation kinds = %v, want none: readers sharing one guard break no rule", got)
	}
	third := make(chan struct{})
	go func() { // a goroutine that holds nothing runs the offload assertion
		rec.AssertNoneHeld("offload")
		close(third)
	}()
	select {
	case <-third:
	case <-time.After(backstop):
		t.Fatal("the third goroutine's assertion never ran within the backstop")
	}
	if got, want := kinds(rec.Violations()), ([]lockorder.Kind)(nil); !reflect.DeepEqual(got, want) {
		t.Errorf("violation kinds = %v, want none: nothing was held at the assertion from the third goroutine", got)
	}
	if got, want := rec.Checks("offload"), 1; got != want {
		t.Errorf("Checks(%q) = %d, want %d", "offload", got, want)
	}
}

func TestChecks_CountsByLabel(t *testing.T) {
	rec := lockorder.NewRecorder()
	rec.AssertNoneHeld("a")
	rec.AssertNotHeld(lockorder.FileAppend, "a")
	rec.AssertNoneHeld("b")
	if got, want := rec.Checks("a"), 2; got != want {
		t.Errorf("Checks(%q) = %d, want %d", "a", got, want)
	}
	if got, want := rec.Checks("b"), 1; got != want {
		t.Errorf("Checks(%q) = %d, want %d", "b", got, want)
	}
	if got, want := rec.Checks("c"), 0; got != want {
		t.Errorf("Checks(%q) = %d, want %d", "c", got, want)
	}
}

// TestRecorder_GuardRefs records the rules a storage guard reference breaks
// and the one it may not: two refs of any segments held by one goroutine are
// exempt from the same-domain rule — a reference never waits on another
// reference — while the drain wait's plain guard acquisition stays checked
// against every ref, and every other rule binds a ref as it binds any hold.
func TestRecorder_GuardRefs(t *testing.T) {
	tests := []struct {
		name      string
		drive     func(t *testing.T, rec *lockorder.Recorder)
		wantKinds []lockorder.Kind
		wantRank  lockorder.Rank
	}{
		{
			name: "two refs of two segments",
			drive: func(t *testing.T, rec *lockorder.Recorder) {
				a, b := new(int), new(int)
				rec.AcquireRef(a)
				rec.AcquireRef(b)
				if got, want := rec.Held(), []lockorder.Rank{lockorder.StorageGuard, lockorder.StorageGuard}; !reflect.DeepEqual(got, want) {
					t.Errorf("Held() with two refs = %v, want %v: both sits on the books until released", got, want)
				}
				rec.ReleaseRef(b)
				rec.ReleaseRef(a)
				if got, want := rec.Held(), ([]lockorder.Rank)(nil); !reflect.DeepEqual(got, want) {
					t.Errorf("Held() after both refs released = %v, want none", got)
				}
			},
		},
		{
			name: "the same segment's ref twice",
			drive: func(t *testing.T, rec *lockorder.Recorder) {
				a := new(int)
				rec.AcquireRef(a)
				rec.AcquireRef(a)
				rec.ReleaseRef(a)
				rec.ReleaseRef(a)
			},
		},
		{
			name: "ref under file index",
			drive: func(t *testing.T, rec *lockorder.Recorder) {
				idx := new(int)
				rec.Acquire(lockorder.FileIndex, idx)
				rec.AcquireRef(new(int))
			},
			wantKinds: []lockorder.Kind{lockorder.GuardUnderIndex},
			wantRank:  lockorder.StorageGuard,
		},
		{
			name: "ref then the drain wait's plain guard",
			drive: func(t *testing.T, rec *lockorder.Recorder) {
				rec.AcquireRef(new(int))
				rec.Acquire(lockorder.StorageGuard, new(int))
			},
			wantKinds: []lockorder.Kind{lockorder.SameDomain},
			wantRank:  lockorder.StorageGuard,
		},
		{
			name: "the drain wait's plain guard then a ref",
			drive: func(t *testing.T, rec *lockorder.Recorder) {
				rec.Acquire(lockorder.StorageGuard, new(int))
				rec.AcquireRef(new(int))
			},
			wantKinds: []lockorder.Kind{lockorder.SameDomain},
			wantRank:  lockorder.StorageGuard,
		},
		{
			name: "two refs under file index",
			drive: func(t *testing.T, rec *lockorder.Recorder) {
				rec.Acquire(lockorder.FileIndex, new(int))
				rec.AcquireRef(new(int))
				rec.AcquireRef(new(int))
			},
			// The exemption reaches only the same-domain rule: the first
			// ref is reported for the index and the second is too — the
			// nested-ref exemption never covers a rule a plain hold
			// breaks. The priority order keeps one report per ref.
			wantKinds: []lockorder.Kind{lockorder.GuardUnderIndex, lockorder.GuardUnderIndex},
			wantRank:  lockorder.StorageGuard,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec := lockorder.NewRecorder()
			tc.drive(t, rec)
			vs := rec.Violations()
			if got, want := kinds(vs), tc.wantKinds; !reflect.DeepEqual(got, want) {
				t.Errorf("violation kinds = %v, want %v", got, want)
			} else if len(tc.wantKinds) >= 1 {
				if got, want := vs[0].Stack, ""; got == want {
					t.Errorf("violation Stack is empty: it must name the acquisition that broke the rule")
				}
				if got, want := vs[0].Rank, tc.wantRank; got != want {
					t.Errorf("first violation Rank = %v, want %v", got, want)
				}
			}
		})
	}
}

// TestRecorder_GuardRefReleasedOnOtherGoroutine checks a ref acquired on one
// goroutine may be released on another, as a descriptor reference is: the
// holder's books empty and no violation is recorded.
func TestRecorder_GuardRefReleasedOnOtherGoroutine(t *testing.T) {
	rec := lockorder.NewRecorder()
	a := new(int)
	acquired := make(chan struct{})
	released := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		rec.AcquireRef(a)
		close(acquired)
		<-released
		if got, want := rec.Held(), ([]lockorder.Rank)(nil); !reflect.DeepEqual(got, want) {
			t.Errorf("the acquiring goroutine's Held() after the other's release = %v, want none", got)
		}
	}()
	go func() {
		defer wg.Done()
		<-acquired
		rec.ReleaseRef(a)
		close(released)
	}()
	wg.Wait()
	if got, want := kinds(rec.Violations()), ([]lockorder.Kind)(nil); !reflect.DeepEqual(got, want) {
		t.Errorf("violation kinds = %v, want none", got)
	}
}
