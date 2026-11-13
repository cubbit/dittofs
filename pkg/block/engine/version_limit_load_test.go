package engine_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/block/engine"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/metadata/kv"
	"github.com/cubbit/dittofs/pkg/metadata/kv/kvtest"
	"github.com/cubbit/dittofs/pkg/sim"
	"github.com/cubbit/dittofs/pkg/vfs/vfserr"
)

const (
	// loadBlock is RFC 8 §2.7's block of 2^24 versions scaled by 2^-8, as is
	// every rate of the rows: each ratio they check holds, and they issue 256
	// times fewer versions. The conformance rows run the RFC's own numbers.
	loadBlock = 1 << 16

	// loadMargin is the trigger's margin in seconds: the self-fence's 30 s, the
	// existence age of one second and the trigger's own sampling second.
	loadMargin = 32

	// fenceSeconds is the self-fence's bound, in the clock's seconds.
	fenceSeconds = 30

	// loadCallBound is the real-time backstop of a clock second's calls: a call
	// that a refusal does not cancel fails the test instead of hanging it.
	loadCallBound = 30 * time.Second
)

// loadUnit is the unit with the store over a fault-injecting kv.KV, the fake
// journal, the test clock, and every call of a load row cancelled by the
// engine's refusal hook, so a refusal by the version limit ends the call with
// its answer instead of waiting for a raise the store is stalling.
type loadUnit struct {
	*unit
	faults   *kvtest.FaultKV
	file     entity.FileID
	payload  []byte
	cur      atomic.Pointer[loadCall]
	refusals atomic.Int64
}

// loadCall is the context the next call of a load row runs under.
type loadCall struct {
	ctx    context.Context
	cancel context.CancelFunc
}

// loadEntered is a fault schedule that closes its channel when the store
// first runs a write transaction under it.
type loadEntered struct {
	*sim.Schedule
	once sync.Once
	ch   chan struct{}
}

func (l *loadEntered) At(op string, key []byte) (time.Duration, int, error) {
	if op == kvtest.OpUpdate {
		l.once.Do(func() {
			close(l.ch)
		})
	}

	return l.Schedule.At(op, key)
}

// newLoadUnit is a started engine with a version block of block, an existence
// age of one second, and its raises tracked.
func newLoadUnit(t *testing.T, block uint64) *loadUnit {
	t.Helper()

	lu := &loadUnit{
		payload: make([]byte, 4096),
	}

	lu.unit = newUnitWith(t, unitConfig{
		unstarted: true,
		kv: func(db kv.KV) kv.KV {
			lu.faults = kvtest.NewFaultKV(db)

			return lu.faults
		},
	})
	lu.file = lu.create(t, "a")

	lu.e.SetBlockForTest(block)
	lu.e.TrackRaisesForTest()
	lu.e.SetConflictBound(time.Hour)

	settings := engine.Settings{
		ExistenceAge:        time.Second,
		GroupCommitMaxFiles: 64,
		CloneMaxLen:         1 << 30,
	}

	if err := lu.e.Apply(settings); err != nil {
		t.Fatal(err)
	}

	if err := lu.e.Start(t.Context(), nil); err != nil {
		t.Fatal(err)
	}

	if err := lu.e.Bind(t.Context(), lu.share); err != nil {
		t.Fatal(err)
	}

	lu.renew()

	lu.e.OnRefused(func() {
		lu.refusals.Add(1)
		lu.cur.Load().cancel()
	})

	// Registered after the unit's own cleanups, so it runs before them: the
	// stalled store call is released, the raise ends and the engine closes.
	t.Cleanup(func() {
		lu.faults.SetFault(nil)
		lu.clock.Advance(24 * time.Hour)

		ctx, cancel := context.WithTimeout(context.Background(), waitBound)

		defer cancel()

		if err := lu.e.Close(ctx); err != nil {
			t.Errorf("close the engine: %v", err)
		}
	})

	return lu
}

// renew gives the next calls a fresh context: one after each failed call and
// one after each clock second, so a slow run (-race) never spends the bound of
// a second on the seconds before it.
func (lu *loadUnit) renew() {
	ctx, cancel := context.WithTimeout(context.Background(), loadCallBound)

	if old := lu.cur.Swap(&loadCall{ctx: ctx, cancel: cancel}); old != nil {
		old.cancel()
	}
}

// issue draws one version and answers its counter.
func (lu *loadUnit) issue() (uint64, error) {
	v, err := lu.e.Draw(lu.cur.Load().ctx, lu.share.ID, lu.file)
	if err != nil {
		lu.renew()

		return 0, err
	}

	return v.Counter(), nil
}

// probe writes 4 KiB to the unit's file, as a client would.
func (lu *loadUnit) probe() error {
	opts := engine.WriteOpts{
		Done: func() {
		},
	}

	_, err := lu.e.Write(lu.cur.Load().ctx, lu.share.ID, lu.file, 0, lu.payload, opts)
	if err != nil {
		lu.renew()
	}

	return err
}

// last is the counter of the last version the journal issued.
func (lu *loadUnit) last() uint64 {
	return lu.j.Stats().NextVersion.Counter - 1
}

// headroom is the versions the journal can still issue.
func (lu *loadUnit) headroom() uint64 {
	st := lu.j.Stats()

	if st.NextVersion.Counter >= st.VersionLimit.Counter {
		return 0
	}

	return st.VersionLimit.Counter - st.NextVersion.Counter
}

// nodeFenced reports whether node_fenced is raised.
func (lu *loadUnit) nodeFenced() bool {
	for _, c := range lu.e.Stats().Node.Conditions {
		if c.Name == "node_fenced" {
			return true
		}
	}

	return false
}

// stall delays every write transaction of the store by d on the clock from now
// on, and answers a channel that closes once one reached the store.
func (lu *loadUnit) stall(t *testing.T, d time.Duration) <-chan struct{} {
	t.Helper()

	s, err := sim.NewSchedule(1, lu.clock, everyUpdate(sim.FaultAction{Delay: d}))
	if err != nil {
		t.Fatal(err)
	}

	f := &loadEntered{
		Schedule: s,
		ch:       make(chan struct{}),
	}

	lu.faults.SetFault(f)

	return f.ch
}

// draws draws n versions at rate a second, the clock moving a second after
// every rate versions of the second, with no raise to wait for: a raise that
// starts is waited for, so the unstalled raises of the run commit first. More
// than ceiling raises fail the run: a trigger that fires on nearly every draw
// would otherwise crawl instead of failing.
func (lu *loadUnit) draws(t *testing.T, rate, n, ceiling uint64) {
	t.Helper()

	started := lu.e.RaisesStarted()

	for i := uint64(1); i <= n; i++ {
		if _, err := lu.issue(); err != nil {
			t.Fatalf("draw %d of %d: %v", i, n, err)
		}

		if i%rate == 0 {
			lu.clock.Advance(time.Second)
			lu.renew() // each clock second gets a fresh wall bound
		}

		if s := lu.e.RaisesStarted(); s != started {
			started = s

			if uint64(s) > ceiling {
				t.Fatalf("%d raises started in %d draws, want at most %d", s, i, ceiling)
			}

			lu.e.WaitRaises(t)
		}
	}

	lu.e.WaitRaises(t)
}

// align draws at rate until the journal has trigger versions left, with one
// write committed on the way, then moves the clock one existence age, stalls
// the store for delay and draws the version at which the raise falls due. It
// answers that version's counter and the clock at which the raise's
// transaction reached the store, which starts the fence's 30 s.
func (lu *loadUnit) align(
	t *testing.T,
	rate, trigger uint64,
	delay time.Duration,
) (v0 uint64, t0 time.Duration) {
	t.Helper()

	var (
		started   = lu.e.RaisesStarted()
		left      = lu.headroom()
		committed bool
		inSecond  uint64
	)

	for left > trigger {
		if !committed && left <= trigger+64 {
			committed = true

			if err := lu.probe(); err != nil {
				t.Fatalf("the write before the stall: %v", err)
			}

			if err := lu.e.CommitExistence(t.Context(), lu.share.ID, lu.file); err != nil {
				t.Fatalf("the commit before the stall: %v", err)
			}

			left = lu.headroom()

			continue
		}

		if _, err := lu.issue(); err != nil {
			t.Fatalf("draw with %d versions left: %v", left, err)
		}

		left--
		inSecond++

		if inSecond == rate {
			inSecond = 0

			lu.clock.Advance(time.Second)
			lu.renew() // each clock second gets a fresh wall bound
		}

		if s := lu.e.RaisesStarted(); s != started {
			started = s

			if left >= trigger {
				t.Fatalf("a raise started with %d versions left, trigger %d", left, trigger)
			}

			lu.e.WaitRaises(t)

			left = lu.headroom()
		}
	}

	if !committed {
		t.Fatalf(
			"the raises kept at most %d versions left, below the trigger %d, before a write",
			left,
			trigger,
		)
	}

	if left != trigger {
		t.Fatalf("the journal has %d versions left before the stall, want %d", left, trigger)
	}

	// The last commit is now one existence age behind the stall.
	lu.clock.Advance(time.Second)

	entered := lu.stall(t, delay)

	c, err := lu.issue()
	if err != nil {
		t.Fatalf("the draw at which the raise falls due: %v", err)
	}

	if got := lu.e.RaisesStarted(); got != started+1 {
		t.Fatalf(
			"the draw with %d versions left started %d raises, want one",
			trigger-1,
			got-started,
		)
	}

	if got := lu.headroom(); got != trigger-1 {
		t.Fatalf("the raise fell due with %d versions left, want %d", got, trigger-1)
	}

	select {
	case <-entered:
	case <-time.After(waitBound):
		t.Fatal("the raise never reached the stalled store")
	}

	return c, lu.clock.Elapsed()
}

// loadEvent is what ends a run of seconds: a call that was refused, or the
// last second reached.
type loadEvent struct {
	sec   uint64 // the second since the stall's start
	m     uint64 // the versions issued since the version that started the raise
	err   error
	probe bool // the call was the second's write, not a draw
}

// run issues versions at rate a second from the version v0 that started the
// raise: at the start of each second one write, then draws until the second's
// rate versions are issued, then one second of the clock. It stops at the
// first call that fails, or after the write of second last.
func (lu *loadUnit) run(t *testing.T, v0, rate, last uint64) loadEvent {
	t.Helper()

	for sec := uint64(0); ; sec++ {
		if err := lu.probe(); err != nil {
			return loadEvent{sec: sec, m: lu.last() - v0, err: err, probe: true}
		}

		if sec == last {
			return loadEvent{sec: sec, m: lu.last() - v0}
		}

		for m := lu.last() - v0; m < (sec+1)*rate-1; {
			c, err := lu.issue()
			if err != nil {
				return loadEvent{sec: sec, m: m, err: err}
			}

			m = c - v0
		}

		lu.clock.Advance(time.Second)
		lu.renew() // each clock second gets a fresh wall bound
	}
}

// sustained is RFC 8 §15.2's version limit under sustained load: the journal
// serves rate versions a second with a version block of block, the store
// stalls as a raise falls due, and every write is acknowledged until the
// self-fence trips 30 s later, none refused by the version limit.
func sustained(t *testing.T, block, rate uint64) {
	t.Helper()

	lu := newLoadUnit(t, block)
	trigger := max(block/2, loadMargin*rate)

	lu.draws(t, rate, 60*rate, (60*rate+trigger)/block+3)

	v0, _ := lu.align(t, rate, trigger, 2*fenceSeconds*time.Second)

	ev := lu.run(t, v0, rate, fenceSeconds)

	if ev.sec != fenceSeconds || !ev.probe || !fenced(ev.err) {
		t.Fatalf(
			"second %d, %d versions after the raise fell due: %v (probe %v), "+
				"want the fence's ErrDelay at second %d with every write before it acknowledged",
			ev.sec, ev.m, ev.err, ev.probe, fenceSeconds,
		)
	}

	if got := lu.refusals.Load(); got != 0 {
		t.Errorf("the version limit refused %d calls before the fence", got)
	}

	if !lu.nodeFenced() {
		t.Error("node_fenced is not raised at the fence's second")
	}
}

// The journal serves its peak rate, the RFC's 300 000 versions a second
// scaled to 1 172, and the store stalls as a raise falls due: every write is
// acknowledged, the first 10 s and until the self-fence trips 30 s on, none
// refused by the version limit first. The raise must fall due with exactly
// the trigger's 37 504 versions left, which pins the margin at 32 s: a
// shorter one raises later and fails there.
func TestSustainedLoadIsNotRefusedBeforeTheFence(t *testing.T) {
	sustained(t, loadBlock, 1_172)
}

// At the RFC's block of 2^20 and 280 000 versions a second, scaled to 2^12
// and 1 094, the rate term alone keeps every write acknowledged until the
// fence. A design that raises only below half a block, with no rate term,
// never keeps the trigger's versions left, so it fails before the stall
// (RFC 8 §15.2's negative case).
func TestSustainedLoadAtABlockOf2To20(t *testing.T) {
	sustained(t, loadBlock>>4, 1_094)
}

// A rate that steps up inside a second makes the raise due on the very draw
// where the versions left fall below the margin at the new rate, with the
// clock unmoved: no timer decides it. The RFC's 100 000 a second and 9 000 000
// versions left are scaled to 391 and 35 156: draw n of the step leaves
// 35 156-n, the second's count n is the peak, and 35 156-n is below 32n, and
// above half a block, from n = 1 066.
func TestARateStepIsDueOnTheNextDraw(t *testing.T) {
	const (
		rate = 391
		left = 35_156
		due  = 1_066
	)

	lu := newLoadUnit(t, loadBlock)

	started := lu.e.RaisesStarted()
	inSecond := uint64(0)

	for lu.headroom() > left {
		if _, err := lu.issue(); err != nil {
			t.Fatal(err)
		}

		if got := lu.e.RaisesStarted() - started; got != 0 {
			t.Fatalf(
				"a raise started with %d versions left, none is due above %d",
				lu.headroom(),
				left,
			)
		}

		if inSecond++; inSecond == rate {
			inSecond = 0

			lu.clock.Advance(time.Second)
			lu.renew() // each clock second gets a fresh wall bound
		}
	}

	lu.e.WaitRaises(t)

	if got := lu.headroom(); got != left || lu.e.RaisesStarted() != started {
		t.Fatalf("%d versions left and %d raises started before the step, want %d and none",
			got, lu.e.RaisesStarted()-started, left)
	}

	lu.clock.Advance(time.Second)

	at := lu.clock.Elapsed()

	for n := 1; n <= due; n++ {
		if _, err := lu.issue(); err != nil {
			t.Fatalf("draw %d of the step: %v", n, err)
		}

		got := lu.e.RaisesStarted() - started

		if n < due && got != 0 {
			t.Fatalf("draw %d of the step started a raise: due from draw %d", n, due)
		}

		if n == due && got != 1 {
			t.Fatalf("draw %d of the step started %d raises, want one", n, got)
		}
	}

	if lu.clock.Elapsed() != at {
		t.Error("the clock moved during the step: the raise was not due on a draw")
	}
}

// burst stalls the store as a raise falls due after rate0 versions a second,
// the RFC's 100 000 scaled to 391, for more than a minute, then steps to rate
// in one go.
func burst(t *testing.T, rate, stallFor uint64) (*loadUnit, uint64, loadEvent) {
	t.Helper()

	const rate0 = 391

	lu := newLoadUnit(t, loadBlock)

	lu.draws(t, rate0, 60*rate0, (60*rate0+loadBlock/2)/loadBlock+3)

	v0, _ := lu.align(t, rate0, loadBlock/2, time.Duration(stallFor)*time.Second)

	return lu, v0, lu.run(t, v0, rate, fenceSeconds)
}

// A burst to the RFC's 250 000 versions a second, scaled to 977, after a
// minute at 391: the 32 767 versions left last 33 s, longer than the fence,
// so no write is refused by the version limit before the self-fence trips.
func TestBurstTo250000IsNotRefusedBeforeTheFence(t *testing.T) {
	lu, _, ev := burst(t, 977, 2*fenceSeconds)

	if ev.sec != fenceSeconds || !ev.probe || !fenced(ev.err) {
		t.Fatalf(
			"second %d, %d versions after the raise fell due: %v (probe %v), "+
				"want the fence's ErrDelay at second %d",
			ev.sec, ev.m, ev.err, ev.probe, fenceSeconds,
		)
	}

	if got := lu.refusals.Load(); got != 0 {
		t.Errorf("the version limit refused %d calls before the fence", got)
	}
}

// A burst to the RFC's 500 000 a second, scaled to 1 953: the 32 767 versions
// left run out in second 16, 14 s before the fence, and the refusals are
// retry-later (ErrDelay with DelayCommit), never ErrNoSpace, and no sooner
// than the margin's 13 s. Once the store returns and the raise commits,
// writes are acknowledged again.
func TestBurstTo500000IsDelayedBeforeTheFence(t *testing.T) {
	const (
		left   = loadBlock/2 - 1
		second = 16
		stall  = 20 // the store's stall, in clock seconds from the raise
	)

	lu, _, ev := burst(t, 1_953, stall)

	if ev.sec != second || ev.probe || ev.m != left || !fenced(ev.err) {
		t.Fatalf(
			"second %d, %d versions after the raise fell due: %v (probe %v), "+
				"want the version limit's ErrDelay(DelayCommit) at second %d after %d versions",
			ev.sec, ev.m, ev.err, ev.probe, second, left,
		)
	}

	if errors.Is(ev.err, vfserr.ErrNoSpace) {
		t.Errorf("the refusal is %v, want retry-later", ev.err)
	}

	if got := lu.refusals.Load(); got != 1 {
		t.Errorf("the version limit refused %d calls, want one", got)
	}

	if lu.nodeFenced() {
		t.Error("node_fenced is raised at the first version-limit refusal")
	}

	if err := lu.probe(); !fenced(err) || lu.refusals.Load() != 2 {
		t.Fatalf(
			"a write after the refusal: %v with %d refusals, want ErrDelay and two",
			err,
			lu.refusals.Load(),
		)
	}

	changed := lu.e.VersionLimitWatch(lu.share.ID)

	// The run moved the clock second times since the raise reached the store.
	for range stall - second {
		lu.clock.Advance(time.Second)
	}

	select {
	case <-changed:
	case <-time.After(waitBound):
		t.Fatal("the raise did not commit once the store returned")
	}

	if err := lu.probe(); err != nil {
		t.Errorf("a write once the raise committed: %v, want acknowledged", err)
	}
}
