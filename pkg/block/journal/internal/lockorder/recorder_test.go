package lockorder_test

import (
	"reflect"
	"sync"
	"testing"

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
			// Nesting all six is in order, so no OutOfOrder and no
			// SameDomain — but the RFC's two absolute prohibitions still
			// hold over the order itself, so a full nest reports the
			// segment append taken under the file map and the storage
			// guard taken under the file index. No real run nests them;
			// the row pins what a full nest costs against the rules.
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
				// Two distinct read locks of one domain: a re-entrant RLock
				// of one guard is shared readers, which is exempt.
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
				rec.IO("read") // the lock does real work before its release
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
				if got := rec.Held(); got != nil {
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
	if got := rec.Held(); got != nil {
		t.Errorf("main goroutine Held() = %v, want none", got)
	}
	if got := kinds(rec.Violations()); got != nil {
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
		if got := rec.Held(); got != nil {
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
	if got := kinds(rec.Violations()); got != nil {
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

	if got := kinds(rec.Violations()); got != nil {
		t.Errorf("violation kinds = %v, want none: the recorder never saw the held lock", got)
	}
	if got := rec.Held(); got != nil {
		t.Errorf("Held() after unlock = %v, want none", got)
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
