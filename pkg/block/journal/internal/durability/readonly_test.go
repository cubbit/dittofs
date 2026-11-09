package durability

import (
	"context"
	"errors"
	"sync"
	"syscall"
	"testing"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
)

// TestAdmitIsNilUntilEnter covers the state's two ends: a fresh layer admits,
// and after Enter every Admit and ReadOnly names the sentinel and the cause.
func TestAdmitIsNilUntilEnter(t *testing.T) {
	r := newRig(t, 1)
	if err := r.d.Admit(); err != nil {
		t.Fatalf("Admit on a fresh layer = %v, want nil", err)
	}
	if err := r.d.ReadOnly(); err != nil {
		t.Fatalf("ReadOnly on a fresh layer = %v, want nil", err)
	}
	if st := r.d.ReadOnlyState(); st.ReadOnly || st.Err != nil || st.SpareMade || st.Check != nil {
		t.Fatalf("ReadOnlyState on a fresh layer = %+v, want zero", st)
	}
	r.d.Enter(syscall.EROFS)
	for name, err := range map[string]error{"Admit": r.d.Admit(), "ReadOnly": r.d.ReadOnly()} {
		if !errors.Is(err, core.ErrJournalReadOnly) {
			t.Errorf("%s = %v, want core.ErrJournalReadOnly", name, err)
		}
		if !errors.Is(err, syscall.EROFS) {
			t.Errorf("%s = %v, want the cause syscall.EROFS reachable", name, err)
		}
	}
}

// TestASecondEnterKeepsTheFirstCause covers idempotence: the state names the
// failure that made the journal read-only, not a later one.
func TestASecondEnterKeepsTheFirstCause(t *testing.T) {
	r := newRig(t, 1)
	first, other := errors.New("first"), errors.New("other")
	r.d.Enter(first)
	r.d.Enter(other)
	st := r.d.ReadOnlyState()
	if !st.ReadOnly || st.Err != first {
		t.Fatalf("ReadOnlyState = %+v, want read-only with the first cause", st)
	}
	if err := r.d.Admit(); !errors.Is(err, first) || errors.Is(err, other) {
		t.Errorf("Admit = %v, want the first cause only", err)
	}
}

// TestConcurrentEntersSettleOnOneCause runs 64 Enter calls against readers:
// exactly one cause wins and every reader sees a consistent state.
func TestConcurrentEntersSettleOnOneCause(t *testing.T) {
	r := newRig(t, 1)
	causes := make([]error, 64)
	var wg sync.WaitGroup
	for i := range causes {
		causes[i] = errors.New("cause")
		wg.Add(2)
		go func() { defer wg.Done(); r.d.Enter(causes[i]) }()
		go func() {
			defer wg.Done()
			if st := r.d.ReadOnlyState(); st.ReadOnly != (st.Err != nil) {
				t.Errorf("torn state %+v", st)
			}
			_ = r.d.Admit()
		}()
	}
	wg.Wait()
	st := r.d.ReadOnlyState()
	if !st.ReadOnly {
		t.Fatal("not read-only after 64 Enter calls")
	}
	won := 0
	for _, c := range causes {
		if st.Err == c {
			won++
		}
	}
	if won != 1 {
		t.Errorf("state's cause matches %d of the callers' causes, want 1", won)
	}
	if err := r.d.Admit(); !errors.Is(err, st.Err) {
		t.Errorf("Admit = %v, want the state's cause", err)
	}
}

// TestTheSettersKeepTheReadOnlyFacts covers setSpareMade and setCheck: each
// keeps what the other and Enter recorded, under concurrent callers.
func TestTheSettersKeepTheReadOnlyFacts(t *testing.T) {
	r := newRig(t, 1)
	boom := errors.New("check failed")
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(3)
		go func() { defer wg.Done(); r.d.Enter(syscall.EROFS) }()
		go func() { defer wg.Done(); r.d.setSpareMade() }()
		go func() { defer wg.Done(); r.d.setCheck(boom) }()
	}
	wg.Wait()
	st := r.d.ReadOnlyState()
	if !st.ReadOnly || st.Err != syscall.EROFS || !st.SpareMade || st.Check != boom {
		t.Fatalf("ReadOnlyState = %+v, want all three facts kept", st)
	}
	r.d.setCheck(nil)
	if st := r.d.ReadOnlyState(); st.Check != nil || !st.SpareMade || !st.ReadOnly {
		t.Errorf("after setCheck(nil) = %+v, want only Check cleared", st)
	}
}

// TestAStreamStopReachesTheReadOnlyState covers the wiring: a stream stopped
// by a refused spare reaches Enter through its EndEvent, seen through Admit.
func TestAStreamStopReachesTheReadOnlyState(t *testing.T) {
	r := newRigConfigured(t, 1, func(*Config) {})
	r.d.Start(r.streams)
	f, g := file(1), file(2)
	_, spare := r.leaveAFailedSegment(t, f, g)
	if err := r.d.Admit(); err != nil {
		t.Fatalf("Admit before the stop = %v, want nil", err)
	}
	second := errors.New("second boom")
	r.failSyncOf(t, spare.Segment, second)
	r.settlePool(t)
	_ = mustReturn(t, "Sync in the spare", r.syncAsync(context.Background(), g))
	eventually(t, "Admit to refuse", func() bool { return r.d.Admit() != nil })
	if err := r.d.Admit(); !errors.Is(err, core.ErrJournalReadOnly) || !errors.Is(err, second) {
		t.Errorf("Admit = %v, want ErrJournalReadOnly wrapping the stop's cause", err)
	}
}
