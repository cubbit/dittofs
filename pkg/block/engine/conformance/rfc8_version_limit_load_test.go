//go:build engine_conformance

package conformance

import (
	"context"
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/block/engine"
	"github.com/cubbit/dittofs/pkg/block/engine/enginetest"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
)

const (
	// versionBlock is RFC 8 §2.7's block, 2^24 versions; the fixture cannot
	// set another.
	versionBlock = 1 << 24

	// existenceAge is RFC 13's proposed existence_age, the one RFC 8 §2.7's
	// 32 s margin is counted with. The fixture runs a longer one for other
	// rows, so each load row applies this one before its first draw.
	existenceAge = time.Second

	// versionMargin is the trigger's margin in seconds: the fence's 30 s, the
	// existence age and the trigger's sampling second. It is counted from
	// existenceAge, so the row's trigger and the engine's stay the same.
	versionMargin = fenceSeconds + uint64(existenceAge/time.Second) + 1

	// fenceSeconds is the self-fence's bound.
	fenceSeconds = 30

	// callBound is the real-time bound of a batch of calls: a call refused by the
	// version limit waits for the raise, and ends with retry-later here.
	callBound = 5 * time.Second

	// batch is how many calls share one bound.
	batch = 1000
)

// loadRun is a row's view of the journal's versions: the counter of the last
// version drawn, and the share's limit as the store holds it.
type loadRun struct {
	f    *enginetest.Fixture
	id   entity.FileID
	last uint64
	n    uint64
	ctx  context.Context
	end  context.CancelFunc
}

// call is the context of the next call, renewed every batch calls.
func (r *loadRun) call() context.Context {
	if r.n%batch == 0 {
		if r.end != nil {
			r.end()
		}

		r.ctx, r.end = context.WithTimeout(context.Background(), callBound)
	}

	r.n++

	return r.ctx
}

// draw draws one version and records its counter.
func (r *loadRun) draw() error {
	v, err := r.f.E.Draw(r.call(), r.f.Share.ID, r.id)
	if err != nil {
		r.n = 0

		return err
	}

	r.last = v.Counter()

	return nil
}

// write is a client's 4 KiB write.
func (r *loadRun) write() error {
	opts := engine.WriteOpts{Done: func() {}}

	_, err := r.f.E.Write(r.call(), r.f.Share.ID, r.id, 0, make([]byte, 4096), opts)
	if err != nil {
		r.n = 0
	}

	return err
}

// headroom is the versions left below the share's committed limit.
func (r *loadRun) headroom(t *testing.T) uint64 {
	t.Helper()

	limits, err := r.f.FS.Store.Lifecycle().
		VersionLimits(t.Context(), []entity.ShareID{r.f.Share.ID})
	if err != nil {
		t.Fatal(err)
	}

	limit := limits[r.f.Share.ID].Counter()
	if r.last+1 >= limit {
		return 0
	}

	return limit - r.last - 1
}

// stallAtTheRaise draws at rate a second until trigger versions are left,
// stalls the store for d and draws the version that makes the raise due.
func (r *loadRun) stallAtTheRaise(t *testing.T, rate, trigger uint64, d time.Duration) uint64 {
	t.Helper()

	settings := enginetest.Settings
	settings.ExistenceAge = existenceAge

	if err := r.f.E.Apply(settings); err != nil {
		t.Fatalf("apply the existence age: %v", err)
	}

	if err := r.draw(); err != nil {
		t.Fatalf("the first version: %v", err)
	}

	// The store is read once a clock second: between two reads every draw
	// takes exactly one version off what is left.
	left := r.headroom(t)

	for n := uint64(1); left > trigger; n++ {
		if err := r.draw(); err != nil {
			t.Fatalf("draw with %d versions left: %v", left, err)
		}

		left--

		if n%rate == 0 {
			r.f.Clock.Advance(time.Second)

			// More versions left than the draws left means the engine raised
			// the limit before the row's trigger: the loop would never end.
			next := r.headroom(t)
			if next > left {
				t.Fatalf(
					"the engine raised the limit with %d versions left, above the row's trigger %d",
					left,
					trigger,
				)
			}

			left = next
		}
	}

	r.f.Clock.Advance(time.Second)

	entered := r.f.HoldWrites(d)

	// A store write in the hold waits d on the fixture's clock, and the
	// fixture closes the engine over a clock it never moves. When the row
	// ends, before the fixture's own cleanup runs, it lifts the hold, so a
	// write that Close sends is not held again, and moves the clock past it.
	t.Cleanup(func() {
		r.f.Heal()
		r.f.Clock.Advance(d)
	})

	if err := r.draw(); err != nil {
		t.Fatalf("the draw at which the raise falls due: %v", err)
	}

	select {
	case <-entered:
	case <-time.After(callBound):
		t.Fatalf(
			"no raise reached the store with %d versions left, trigger %d",
			r.headroom(t),
			trigger,
		)
	}

	return r.last
}

// second runs second sec of rate versions after the raise fell due at version
// v0: one write, the draws up to the second's last version, then a second of
// the clock. It answers the first error and the versions issued by then.
func (r *loadRun) second(t *testing.T, v0, rate, sec uint64) (uint64, error) {
	t.Helper()

	if err := r.write(); err != nil {
		return r.last - v0, err
	}

	for r.last-v0 < (sec+1)*rate-1 {
		if err := r.draw(); err != nil {
			return r.last - v0, err
		}
	}

	r.f.Clock.Advance(time.Second)

	return r.last - v0, nil
}

// RFC 8 §15.2, version limit under sustained load: block 2^24, the peak rate,
// the store stalled as a raise falls due. Writes are acknowledged the first
// 10 s and until the self-fence trips at 30 s, never refused by the version
// limit first. Stepping the rate up in one go is due on the next draw, not on
// a timer (the engine's own tests pin that on the fake journal).
func TestRFC8_VersionLimitSustainedLoad(t *testing.T) {
	const rate = 300_000

	f := enginetest.New(t)
	_, id := f.File("a")
	r := &loadRun{f: f, id: id}

	v0 := r.stallAtTheRaise(t, rate, versionMargin*rate, 2*fenceSeconds*time.Second)

	for sec := uint64(0); sec < fenceSeconds; sec++ {
		if m, err := r.second(t, v0, rate, sec); err != nil {
			t.Fatalf(
				"second %d, %d versions after the raise: %v, want every write acknowledged",
				sec,
				m,
				err,
			)
		}
	}

	if err := r.write(); !isFenced(err) || !nodeFenced(f) {
		t.Errorf(
			"the write at the fence's second: %v, node_fenced %v, want ErrDelay and node_fenced",
			err,
			nodeFenced(f),
		)
	}
}

// RFC 8 §15.2, burst at a stall's start, to 250 000 versions a second: block
// 2^24, 100 000 versions a second for a minute, the store stalled, then the
// rate in one step. No write is refused by the version limit before the fence.
func TestRFC8_VersionLimitBurst250000(t *testing.T) {
	const rate0 = 100_000

	f := enginetest.New(t)
	_, id := f.File("a")
	r := &loadRun{f: f, id: id}

	v0 := r.stallAtTheRaise(t, rate0, versionBlock/2, 2*fenceSeconds*time.Second)

	for sec := uint64(0); sec < fenceSeconds; sec++ {
		if m, err := r.second(t, v0, 250_000, sec); err != nil {
			t.Fatalf(
				"second %d, %d versions after the raise: %v, want no refusal before the fence",
				sec,
				m,
				err,
			)
		}
	}

	if err := r.write(); !isFenced(err) || !nodeFenced(f) {
		t.Errorf(
			"the write at the fence's second: %v, node_fenced %v, want ErrDelay and node_fenced",
			err,
			nodeFenced(f),
		)
	}
}

// RFC 8 §15.2, burst at a stall's start, to 500 000 versions a second: the
// refusals are retry-later, begin no sooner than about 13 s before the fence,
// and writes are acknowledged once the store returns and the raise commits.
func TestRFC8_VersionLimitBurst500000(t *testing.T) {
	const (
		rate0 = 100_000
		first = 16 // 8 388 607 versions at 500 000 a second
	)

	f := enginetest.New(t)
	_, id := f.File("a")
	r := &loadRun{f: f, id: id}

	v0 := r.stallAtTheRaise(t, rate0, versionBlock/2, 20*time.Second)

	for sec := uint64(0); ; sec++ {
		m, err := r.second(t, v0, 500_000, sec)
		if err == nil {
			if sec >= first {
				t.Fatalf("no refusal by second %d", sec)
			}

			continue
		}

		if sec != first || m != versionBlock/2-1 || !isFenced(err) || nodeFenced(f) {
			t.Fatalf(
				"second %d after %d versions: %v, want ErrDelay at second %d after %d",
				sec,
				m,
				err,
				first,
				versionBlock/2-1,
			)
		}

		break
	}

	for range 4 {
		f.Clock.Advance(time.Second)
	}

	eventually(t, "a write once the raise committed", func() bool {
		return r.write() == nil
	})
}
