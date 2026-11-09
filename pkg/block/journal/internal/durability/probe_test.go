package durability_test

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/durability"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/index"
	"github.com/cubbit/dittofs/pkg/block/journal/journaltest"
	"github.com/cubbit/dittofs/pkg/block/journal/journaltest/prof"
)

// afterCounter counts the timers the layer arms.
type afterCounter struct {
	core.Clock
	n atomic.Int64
}

func (c *afterCounter) After(d time.Duration) <-chan struct{} {
	c.n.Add(1)
	return c.Clock.After(d)
}

type probeRig struct {
	d      *durability.Durability
	clock  interface{ Advance(time.Duration) }
	timers *afterCounter

	checks, spares atomic.Int64
	checkErr       atomic.Pointer[error]
	spareErr       atomic.Pointer[error]
}

var errNoFlush = errors.New("the mount does not flush its cache")

func newProbeRig(t *testing.T) *probeRig {
	t.Helper()
	prof.Watch(t, prof.Options{})
	return newUnwatchedProbeRig(t)
}

func newUnwatchedProbeRig(t *testing.T) *probeRig {
	t.Helper()
	clock := journaltest.NewStopwatchClock()
	r := &probeRig{clock: clock, timers: &afterCounter{Clock: clock}}
	d, err := durability.New(durability.Config{
		Clock:    r.timers,
		Domains:  noDomains{},
		Index:    index.New(1 << 20),
		Dir:      t.TempDir(),
		Counters: &index.Counters{},
		MarkLoss: func(core.FileID, core.Extent, core.Version) (index.HeldDelta, error) { return index.HeldDelta{}, nil },
		Check: func() error {
			r.checks.Add(1)
			if e := r.checkErr.Load(); e != nil {
				return *e
			}
			return nil
		},
		MakeSpare: func() error {
			r.spares.Add(1)
			if e := r.spareErr.Load(); e != nil {
				return *e
			}
			return nil
		},
	})
	if err != nil {
		t.Fatalf("durability.New: %v", err)
	}
	t.Cleanup(d.Stop)
	r.d = d
	return r
}

// tick waits for the probe to arm its timer, then lets one interval pass.
func (r *probeRig) tick(t *testing.T, armed int64) {
	t.Helper()
	eventually(t, "the probe to arm its timer", func() bool { return r.timers.n.Load() >= armed })
	r.clock.Advance(core.ProbeInterval)
}

func (r *probeRig) setCheck(err error) {
	if err == nil {
		r.checkErr.Store(nil)
		return
	}
	r.checkErr.Store(&err)
}

func (r *probeRig) setSpare(err error) {
	if err == nil {
		r.spareErr.Store(nil)
		return
	}
	r.spareErr.Store(&err)
}

// TestProbeRunsEveryInterval covers: no probe before the interval, one at
// exactly the interval and one every interval after.
func TestProbeRunsEveryInterval(t *testing.T) {
	r := newProbeRig(t)
	r.d.Enter(errors.New("read-only"))
	eventually(t, "the probe to arm its timer", func() bool { return r.timers.n.Load() == 1 })

	r.clock.Advance(29 * time.Second)
	time.Sleep(30 * time.Millisecond)
	if got := r.checks.Load(); got != 0 {
		t.Fatalf("%d probes after 29 s, want none", got)
	}
	r.clock.Advance(time.Second)
	eventually(t, "the probe at 30 s", func() bool { return r.checks.Load() == 1 })
	for i := int64(2); i <= 4; i++ {
		r.tick(t, i)
		eventually(t, "the next probe", func() bool { return r.checks.Load() == i })
	}
}

// TestProbeFailingCheckMakesNoSpare covers: a check that fails, as on a mount
// without cache flushes, makes no spare and is named by the state; once it
// passes one spare is made, the check is cleared and the journal stays
// read-only.
func TestProbeFailingCheckMakesNoSpare(t *testing.T) {
	r := newProbeRig(t)
	r.setCheck(errNoFlush)
	r.d.Enter(errors.New("read-only"))
	r.tick(t, 1)
	eventually(t, "the failed check to be recorded", func() bool { return r.d.ReadOnlyState().Check != nil })
	if st := r.d.ReadOnlyState(); !errors.Is(st.Check, errNoFlush) || !st.ReadOnly || st.SpareMade {
		t.Fatalf("state %+v, want the check named, read-only and no spare", st)
	}
	if got := r.spares.Load(); got != 0 {
		t.Fatalf("%d spares made under a failing check", got)
	}

	r.setCheck(nil)
	r.tick(t, 2)
	eventually(t, "the spare", func() bool { return r.d.ReadOnlyState().SpareMade })
	st := r.d.ReadOnlyState()
	if st.Check != nil || !st.ReadOnly || r.spares.Load() != 1 {
		t.Fatalf("state %+v with %d spares, want check cleared, read-only and one spare", st, r.spares.Load())
	}
	if r.d.Admit() == nil {
		t.Fatalf("Admit took a write after the spare was made; only a reopen leaves the state")
	}

	// Two more probes with the spare held make none.
	for i := int64(3); i <= 4; i++ {
		r.tick(t, i)
		eventually(t, "the probe", func() bool { return r.checks.Load() == i })
	}
	time.Sleep(30 * time.Millisecond)
	if got := r.spares.Load(); got != 1 {
		t.Fatalf("%d spares made, want one at a time", got)
	}
	if !r.d.ReadOnlyState().ReadOnly {
		t.Fatalf("the journal left read-only")
	}
}

// TestProbeRetriesAFailedSpare covers: a spare that fails with something other
// than EROFS sets nothing and is made again at the next probe.
func TestProbeRetriesAFailedSpare(t *testing.T) {
	r := newProbeRig(t)
	r.setSpare(errors.New("no space"))
	r.d.Enter(errors.New("read-only"))
	r.tick(t, 1)
	eventually(t, "the first attempt", func() bool { return r.spares.Load() == 1 })
	time.Sleep(30 * time.Millisecond)
	if r.d.ReadOnlyState().SpareMade {
		t.Fatalf("SpareMade after a failed spare")
	}
	r.setSpare(nil)
	r.tick(t, 2)
	eventually(t, "the retry", func() bool { return r.spares.Load() == 2 })
	eventually(t, "the spare", func() bool { return r.d.ReadOnlyState().SpareMade })
}

// TestProbeStopLeavesNoGoroutine covers: Stop in the middle of the wait joins
// the probe.
func TestProbeStopLeavesNoGoroutine(t *testing.T) {
	r := newProbeRig(t)
	r.d.Enter(errors.New("read-only"))
	eventually(t, "the probe to arm its timer", func() bool { return r.timers.n.Load() == 1 })
	r.d.Stop()
}

// TestProbeStopRacingATick covers: once Stop returns, neither Check nor
// MakeSpare is called, however the stop and the tick interleave.
func TestProbeStopRacingATick(t *testing.T) {
	prof.Watch(t, prof.Options{})
	for i := 0; i < 20; i++ {
		r := newUnwatchedProbeRig(t)
		r.d.Enter(errors.New("read-only"))
		eventually(t, "the probe to arm its timer", func() bool { return r.timers.n.Load() == 1 })
		var wg sync.WaitGroup
		wg.Add(1)
		go func() { defer wg.Done(); r.clock.Advance(core.ProbeInterval) }()
		r.d.Stop()
		wg.Wait()
		c, s := r.checks.Load(), r.spares.Load()
		r.clock.Advance(10 * core.ProbeInterval)
		time.Sleep(time.Millisecond)
		if r.checks.Load() != c || r.spares.Load() != s {
			t.Fatalf("the probe ran after Stop returned")
		}
	}
}

// TestProbeStartsOnceAcrossEnters covers: however many goroutines enter, one
// probe runs.
func TestProbeStartsOnceAcrossEnters(t *testing.T) {
	r := newProbeRig(t)
	var wg sync.WaitGroup
	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); r.d.Enter(errors.New("read-only")) }()
	}
	wg.Wait()
	eventually(t, "the probe to arm its timer", func() bool { return r.timers.n.Load() >= 1 })
	time.Sleep(30 * time.Millisecond)
	if got := r.timers.n.Load(); got != 1 {
		t.Fatalf("%d probe timers armed, want one probe", got)
	}
	r.tick(t, 1)
	eventually(t, "the probe", func() bool { return r.checks.Load() >= 1 })
	time.Sleep(30 * time.Millisecond)
	if got := r.checks.Load(); got != 1 {
		t.Fatalf("%d checks at one interval, want one", got)
	}
}

// TestProbeNilHooks covers: without Check and MakeSpare the probe runs and
// leaves the state as it was.
func TestProbeNilHooks(t *testing.T) {
	prof.Watch(t, prof.Options{})
	clock := journaltest.NewStopwatchClock()
	d, err := durability.New(durability.Config{
		Clock: clock, Domains: noDomains{}, Index: index.New(1 << 20), Dir: t.TempDir(),
		Counters: &index.Counters{},
		MarkLoss: func(core.FileID, core.Extent, core.Version) (index.HeldDelta, error) { return index.HeldDelta{}, nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(d.Stop)
	d.Enter(errors.New("read-only"))
	for i := 0; i < 3; i++ {
		time.Sleep(10 * time.Millisecond)
		clock.Advance(core.ProbeInterval)
	}
	if st := d.ReadOnlyState(); st.SpareMade || st.Check != nil || !st.ReadOnly {
		t.Fatalf("state %+v", st)
	}
}
