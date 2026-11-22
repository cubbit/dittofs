package vfs

import (
	"context"
	"errors"
	"net/netip"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	identitysrv "github.com/cubbit/dittofs/internal/identity"
	"github.com/cubbit/dittofs/internal/prof"
	"github.com/cubbit/dittofs/pkg/identity"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/metadata/kv"
	"github.com/cubbit/dittofs/pkg/metadata/kv/kvtest"
	"github.com/cubbit/dittofs/pkg/metadata/kvstore/kvstoretest"
	"github.com/cubbit/dittofs/pkg/settings"
	"github.com/cubbit/dittofs/pkg/sim"
	"github.com/cubbit/dittofs/pkg/vfs"
)

// caching is a serving filesystem over a store behind the kv fault wrapper,
// resolving through a real identity server whose States the test applies and
// whose identity.resolve spans it records: one span is one resolution, so
// one miss.
type caching struct {
	*shares
	srv   *identitysrv.Server
	fault *kvtest.FaultKV
	clock *sim.TestClock // the cache's wall clock, and the identity server's
	tp    *sdktrace.TracerProvider
	spans *tracetest.SpanRecorder
	ids   identitysrv.Settings // the identity settings the store holds
	gen   uint64               // the generation of the last State applied
}

func newCaching(t *testing.T) *caching {
	t.Helper()
	prof.Watch(t, prof.Options{})
	c := &caching{clock: sim.NewTestClock(time.Unix(1_000_000, 0)), spans: tracetest.NewSpanRecorder()}
	fx := kvstoretest.NewFS(t, func(tb testing.TB) kv.KV {
		c.fault = kvtest.NewFaultKV(memKV(tb))
		return c.fault
	})
	c.tp = sdktrace.NewTracerProvider(sdktrace.WithSampler(sdktrace.AlwaysSample()), sdktrace.WithSpanProcessor(c.spans))
	t.Cleanup(func() { _ = c.tp.Shutdown(context.Background()) })
	srv, err := identitysrv.New(identitysrv.Deps{Store: fx.Store, Clock: c.clock, Tracer: c.tp.Tracer("test")})
	if err != nil {
		t.Fatal(err)
	}
	c.srv = srv
	snap, _, err := settings.Read(t.Context(), fx.Store.Nodes(), settings.Table{identitysrv.Section()}, entity.NodeID{})
	if err != nil {
		t.Fatal(err)
	}
	if c.ids, err = identitysrv.SettingsOf(snap, entity.NodeID{}); err != nil {
		t.Fatal(err)
	}
	c.raise(t)
	if err := srv.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = srv.Stop(context.Background()) })
	c.shares = startShares(t, newFixtureOver(t, Deps{Identity: srv, Clock: c.clock}, fx), nil)
	return c
}

// raise applies a State differing from the current one, which raises the
// serial.
func (c *caching) raise(t *testing.T) {
	t.Helper()
	c.gen++
	if err := c.srv.Apply(identitysrv.State{Generation: c.gen, Settings: c.ids}); err != nil {
		t.Fatal(err)
	}
}

// ticket is a call proven by a Kerberos ticket as p from at, the ticket
// ending a day from now on the cache's clock, offset by skew so that two
// skews are two different proofs.
func (c *caching) ticket(p entity.Principal, at netip.Addr, skew time.Duration) vfs.Call {
	call := proven(p, at)
	call.Identity.Evidence.Expires = c.clock.Now().Add(24*time.Hour + skew)
	return call
}

// resolutions is how many resolutions have ended.
func (c *caching) resolutions() int {
	n := 0
	for _, s := range c.spans.Ended() {
		if s.Name() == "identity.resolve" {
			n++
		}
	}
	return n
}

// admitCall admits call to the share under a sampled span, so its
// resolution, if any, is recorded, and answers the caller it acts as.
func (c *caching) admitCall(call vfs.Call) (entity.Identity, error) {
	ctx, span := c.tp.Tracer("test").Start(context.Background(), "call")
	defer span.End()
	return c.admitIn(ctx, call)
}

func (c *caching) admitIn(ctx context.Context, call vfs.Call) (entity.Identity, error) {
	a, release, err := c.admit(ctx, call, c.share, true, false)
	release()
	if err != nil {
		return entity.Identity{}, err
	}
	return a.caller.Identity, nil
}

// twice admits call twice and checks that the first resolved, and counted a
// miss, only if miss says so, and that the second was served from the cache
// and counted a hit.
func (c *caching) twice(t *testing.T, what string, call vfs.Call, miss bool) entity.Identity {
	t.Helper()
	want := 0
	if miss {
		want = 1
	}
	before, hits, misses := c.resolutions(), testutil.ToFloat64(c.callers.hits), testutil.ToFloat64(c.callers.misses)
	id, err := c.admitCall(call)
	if err != nil {
		t.Fatalf("%s: admission: %v", what, err)
	}
	if got := c.resolutions() - before; got != want {
		t.Errorf("%s: the first call resolved %d times, want %d", what, got, want)
	}
	if got := testutil.ToFloat64(c.callers.misses) - misses; got != float64(want) {
		t.Errorf("%s: the first call counted %v misses, want %d", what, got, want)
	}
	if got := testutil.ToFloat64(c.callers.hits) - hits; got != float64(1-want) {
		t.Errorf("%s: the first call counted %v hits, want %d", what, got, 1-want)
	}
	before, hits, misses = c.resolutions(), testutil.ToFloat64(c.callers.hits), testutil.ToFloat64(c.callers.misses)
	if _, err := c.admitCall(call); err != nil {
		t.Fatalf("%s: the second admission: %v", what, err)
	}
	if got := c.resolutions() - before; got != 0 {
		t.Errorf("%s: the second call resolved %d times, want 0: the first one's caller was not cached", what, got)
	}
	if got := testutil.ToFloat64(c.callers.hits) - hits; got != 1 {
		t.Errorf("%s: the second call counted %v hits, want 1", what, got)
	}
	if got := testutil.ToFloat64(c.callers.misses) - misses; got != 0 {
		t.Errorf("%s: the second call counted %v misses, want 0", what, got)
	}
	return id
}

// hold has the next n user reads wait, each its view already begun, until
// the returned clock is advanced an hour past it, or the test ends.
func (c *caching) hold(t *testing.T, n int) *sim.TestClock {
	t.Helper()
	clock := sim.NewTestClock(time.Unix(0, 0))
	s, err := sim.NewSchedule(1, clock, sim.Rule{Op: kvtest.OpGet, Key: "U‖*", Count: n, Action: sim.FaultAction{Delay: time.Hour}})
	if err != nil {
		t.Fatal(err)
	}
	c.fault.SetFault(s)
	// A test that fails while a read is held releases it, so the filesystem
	// can drain.
	t.Cleanup(func() { clock.Advance(1000 * time.Hour) })
	return clock
}

// held waits until a read is waiting on clock.
func held(t *testing.T, clock *sim.TestClock) {
	t.Helper()
	until(t, "a user read reaching the hold", func() bool { return clock.Pending() > 0 })
}

// until polls cond every millisecond, failing the test if it does not hold
// within a generous backstop.
func until(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("never saw %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

// admittedAs is one admission's answer.
type admittedAs struct {
	id  entity.Identity
	err error
}

// admitLater admits call in its own goroutine, answering on the channel.
func (c *caching) admitLater(ctx context.Context, call vfs.Call) <-chan admittedAs {
	out := make(chan admittedAs, 1)
	go func() {
		id, err := c.admitIn(ctx, call)
		out <- admittedAs{id, err}
	}()
	return out
}

func answer(t *testing.T, what string, ch <-chan admittedAs) admittedAs {
	t.Helper()
	select {
	case a := <-ch:
		return a
	case <-time.After(10 * time.Second):
		t.Fatalf("%s never answered", what)
		return admittedAs{}
	}
}

// After a warm hit, a change to any one input the caller was resolved from
// misses, and the caller it resolves is cached under the new key: the grant
// list's version, the export policy's, the read-only cause a client rule
// decides, the client's address under a client rule, the State's serial and
// the evidence. Then the caller's own grant set to none refuses its next
// call. An input dropped from the key serves the old caller instead.
func TestACachedCallerMissesOnEachInputItWasResolvedFrom(t *testing.T) {
	c := newCaching(t)
	ctx := t.Context()
	bob := c.user(t, "bob", "", c.share, entity.AccessReadWrite)
	call := c.ticket(bob.Principal, inLab, 0)
	c.twice(t, "the first call", call, true)

	c.user(t, "carol", "", c.share, entity.AccessRead)
	c.twice(t, "another principal granted on the share", call, true)

	p, err := c.cp.ExportPolicy(ctx, c.share)
	c.must(t, err)
	c.must(t, errOf(c.cp.PutExportPolicy(ctx, p, p.Version)))
	c.twice(t, "the export policy written again unchanged", call, true)
	p, err = c.cp.ExportPolicy(ctx, c.share)
	c.must(t, err)
	// The netgroup the first rule names does not exist yet, so it admits
	// nobody and the second rule decides.
	p.Clients = []entity.ClientRule{
		{Match: entity.ClientMatch{Netgroup: "readers"}, Access: entity.AccessRead},
		{Match: entity.ClientMatch{CIDR: netip.MustParsePrefix("10.0.0.0/8")}, Access: entity.AccessReadWrite},
	}
	c.must(t, errOf(c.cp.PutExportPolicy(ctx, p, p.Version)))
	c.twice(t, "the export policy given client rules", call, true)

	c.twice(t, "another host the same client rule admits", c.ticket(bob.Principal, netip.MustParseAddr("10.0.0.6"), 0), true)

	c.must(t, errOf(c.cp.PutNetgroup(ctx, entity.Netgroup{Name: "readers", Members: []string{inLab.String()}}, 0)))
	if id := c.twice(t, "the netgroup now holding the host", call, true); id.ReadOnly != entity.NarrowedByClient {
		t.Errorf("the caller once its host's rule admits it read-only: ReadOnly %v, want NarrowedByClient", id.ReadOnly)
	}

	c.raise(t)
	c.twice(t, "a State applied", call, true)

	c.twice(t, "another ticket", c.ticket(bob.Principal, inLab, time.Second), true)

	c.must(t, errOf(c.cp.SetGrant(ctx, entity.ShareGrant{Share: c.share, Principal: bob.Principal, Access: entity.AccessNone}, grantVersion(t, c.cp, c.share, bob.Principal))))
	if _, err := c.admitCall(call); !errors.Is(err, vfs.ErrAccess) {
		t.Errorf("bob's call once his own grant is none: %v, want ErrAccess", err)
	}
}

// A grant written after resolution read the grants, and before its caller
// is stored, moves the grant list's version past the one the caller is keyed
// on: the next call misses and acts under the new grant. A version read after
// the grants would key the old grant under the new version and serve it.
func TestAGrantWrittenBeforeTheStoreIsNotServedFromTheCache(t *testing.T) {
	c := newCaching(t)
	ctx := t.Context()
	bob := c.user(t, "bob", "", c.share, entity.AccessReadWrite)
	call := c.ticket(bob.Principal, inLab, 0)
	var once sync.Once
	c.callers.beforeStore = func(context.Context) {
		once.Do(func() {
			g, _, err := c.cp.ShareGrant(ctx, c.share, bob.Principal)
			if err == nil {
				_, err = c.cp.SetGrant(ctx, entity.ShareGrant{Share: c.share, Principal: bob.Principal, Access: entity.AccessNone}, g.Version)
			}
			if err != nil {
				t.Errorf("the grant written before the store: %v", err)
			}
		})
	}
	if _, err := c.admitCall(call); err != nil {
		t.Fatalf("bob's call resolved before the grant was written: %v", err)
	}
	before := c.resolutions()
	if _, err := c.admitCall(call); !errors.Is(err, vfs.ErrAccess) {
		t.Errorf("bob's next call, his grant now none: %v, want ErrAccess", err)
	}
	if got := c.resolutions() - before; got != 1 {
		t.Errorf("bob's next call resolved %d times, want 1", got)
	}
}

// RFC 18's in-flight disable, at the cache: bob's resolution reads him
// enabled in its second pass, the first overtaken by an Apply, while bob is
// disabled and the State carrying it applied. The stale caller is served to
// the call that resolved it and stored nowhere; a call of bob's that joined
// the resolution after that State resolves on its own and is refused, and so
// is his next one. The call that started the resolution waits on no flight.
func TestAnInFlightDisableIsNeitherStoredNorServedToALaterCall(t *testing.T) {
	c := newCaching(t)
	bob := c.user(t, "bob", "", c.share, entity.AccessReadWrite)
	call := c.ticket(bob.Principal, inLab, 0)
	var waiting atomic.Int32
	c.callers.waiting = func() { waiting.Add(1) }

	hold := c.hold(t, 2)
	first := c.admitLater(context.Background(), call)
	held(t, hold)
	c.raise(t)
	hold.Advance(time.Hour)
	held(t, hold)

	bob.Disabled = true
	c.must(t, errOf(c.cp.PutUser(t.Context(), bob, userGeneration(t, c.fs.Store, bob.Principal), entity.Alloc{})))
	c.raise(t)
	joined := c.admitLater(context.Background(), call)
	until(t, "the later call waiting on the resolution", func() bool { return waiting.Load() == 1 })
	hold.Advance(time.Hour)

	if a := answer(t, "the call that resolved bob", first); a.err != nil {
		t.Errorf("the call whose second pass went stale: %v, want served", a.err)
	}
	if a := answer(t, "the call that joined after the disable", joined); !errors.Is(a.err, vfs.ErrAccess) {
		t.Errorf("bob's call joining the resolution after the disable's State: %v, want ErrAccess", a.err)
	}
	if _, err := c.admitCall(call); !errors.Is(err, vfs.ErrAccess) {
		t.Errorf("bob's next call: %v, want ErrAccess", err)
	}
}

// A serial raised during both passes of every resolution never starves a
// call: each is served, and none is stored, as each caller is stamped with a
// State already replaced. The first call with no raise during it misses and
// is stored, and the one after it hits.
func TestASteadilyRaisedSerialServesEveryCallAndStoresNone(t *testing.T) {
	c := newCaching(t)
	bob := c.user(t, "bob", "", c.share, entity.AccessReadWrite)
	call := c.ticket(bob.Principal, inLab, 0)
	for round := range 3 {
		hold := c.hold(t, 2)
		done := c.admitLater(context.Background(), call)
		for range 2 {
			held(t, hold)
			c.raise(t)
			hold.Advance(time.Hour)
		}
		if a := answer(t, "a call raised during", done); a.err != nil {
			t.Fatalf("round %d: a call raised during both passes: %v, want served", round, a.err)
		}
	}
	if n := len(c.callers.entries); n != 0 {
		t.Errorf("the cache holds %d callers after calls each raised during, want 0", n)
	}
	c.twice(t, "the first call with no raise during it", call, true)
}

// A call that joins a resolution after an Apply its starter had not read is
// served the flight's caller without resolving again when that caller was
// built under the new State: the raise lands between the resolution's two
// passes, so its second pass, and the caller, are under the new serial.
func TestAJoinerAfterAnApplyTheFlightCaughtUpWithResolvesNoMore(t *testing.T) {
	c := newCaching(t)
	bob := c.user(t, "bob", "", c.share, entity.AccessReadWrite)
	call := c.ticket(bob.Principal, inLab, 0)
	var waiting atomic.Int32
	c.callers.waiting = func() { waiting.Add(1) }
	before := c.resolutions()
	ctx, span := c.tp.Tracer("test").Start(context.Background(), "call")
	defer span.End()

	hold := c.hold(t, 2)
	first := c.admitLater(ctx, call)
	held(t, hold)
	c.raise(t)
	hold.Advance(time.Hour)
	held(t, hold)
	joined := c.admitLater(ctx, call)
	until(t, "the later call waiting on the resolution", func() bool { return waiting.Load() == 1 })
	hold.Advance(time.Hour)

	for what, ch := range map[string]<-chan admittedAs{"the call that resolved bob": first, "the call that joined after the raise": joined} {
		if a := answer(t, what, ch); a.err != nil || a.id.Principal != bob.Principal {
			t.Errorf("%s: %v acting as %v, want served as bob", what, a.err, a.id.Principal)
		}
	}
	if got := c.resolutions() - before; got != 1 {
		t.Errorf("a joiner after a raise the flight's second pass read: %d resolutions, want 1, the flight's", got)
	}
}

// A lookup by a call that read an older serial than the entry was built
// under misses but keeps the entry, so the next call that reads the current
// serial is served from it; a lookup under a newer serial removes it.
func TestALookupUnderAnOlderSerialKeepsTheNewerEntry(t *testing.T) {
	c := newCaching(t)
	bob := c.user(t, "bob", "", c.share, entity.AccessReadWrite)
	call := c.ticket(bob.Principal, inLab, 0)
	c.twice(t, "bob's first calls", call, true)
	c.callers.mu.Lock()
	var got *identity.Caller
	for k := range c.callers.entries {
		got = c.callers.lookup(k, c.callers.resolver.Current()-1)
	}
	n := len(c.callers.entries)
	c.callers.mu.Unlock()
	if got != nil || n != 1 {
		t.Fatalf("a lookup under the previous serial: served %v with %d entries left, want a miss keeping the 1 entry", got, n)
	}
	before := c.resolutions()
	if _, err := c.admitCall(call); err != nil {
		t.Fatalf("bob's next call: %v", err)
	}
	if got := c.resolutions() - before; got != 0 {
		t.Errorf("bob's next call under the current serial resolved %d times, want 0: served from the kept entry", got)
	}
	c.callers.mu.Lock()
	for k := range c.callers.entries {
		c.callers.lookup(k, c.callers.resolver.Current()+1)
	}
	n = len(c.callers.entries)
	c.callers.mu.Unlock()
	if n != 0 {
		t.Errorf("a lookup under a newer serial than the entry's: %d entries left, want the older entry removed", n)
	}
}

// The mirror of the in-flight disable, a refusal: bob's resolution reads him
// disabled in its second pass while he is enabled and the State carrying it
// applied. The refusal answers the call that resolved it alone; a call of
// bob's that joined the resolution after that State resolves on its own and
// is served.
func TestAnInFlightRefusalIsNotServedToALaterCall(t *testing.T) {
	c := newCaching(t)
	bob := c.user(t, "bob", "", c.share, entity.AccessReadWrite)
	bob.Disabled = true
	c.must(t, errOf(c.cp.PutUser(t.Context(), bob, userGeneration(t, c.fs.Store, bob.Principal), entity.Alloc{})))
	c.raise(t)
	call := c.ticket(bob.Principal, inLab, 0)
	var waiting atomic.Int32
	c.callers.waiting = func() { waiting.Add(1) }

	hold := c.hold(t, 2)
	first := c.admitLater(context.Background(), call)
	held(t, hold)
	c.raise(t)
	hold.Advance(time.Hour)
	held(t, hold)

	bob.Disabled = false
	c.must(t, errOf(c.cp.PutUser(t.Context(), bob, userGeneration(t, c.fs.Store, bob.Principal), entity.Alloc{})))
	c.raise(t)
	joined := c.admitLater(context.Background(), call)
	until(t, "the later call waiting on the resolution", func() bool { return waiting.Load() == 1 })
	hold.Advance(time.Hour)

	if a := answer(t, "the call that resolved bob", first); !errors.Is(a.err, vfs.ErrAccess) {
		t.Errorf("the call whose second pass read bob disabled: %v, want ErrAccess", a.err)
	}
	if a := answer(t, "the call that joined after the enable", joined); a.err != nil {
		t.Errorf("bob's call joining the resolution after the enable's State: %v, want served", a.err)
	}
}

// 32 misses of one key share one resolution, run by the call that missed
// first. That call, cancelled, waits the resolution out and is served, as
// are the 30 that joined and waited on; one that joined and is cancelled
// answers at once: the resolution runs on a context no call's cancel reaches,
// and each call that joined waits only on its own.
func TestConcurrentMissesOfOneKeyShareOneResolution(t *testing.T) {
	c := newCaching(t)
	bob := c.user(t, "bob", "", c.share, entity.AccessReadWrite)
	call := c.ticket(bob.Principal, inLab, 0)
	var waiting atomic.Int32
	c.callers.waiting = func() { waiting.Add(1) }
	var resolvedIn atomic.Value
	c.callers.beforeStore = func(ctx context.Context) { resolvedIn.Store(ctx.Err() == nil) }
	hold := c.hold(t, 1)
	before, misses := c.resolutions(), testutil.ToFloat64(c.callers.misses)

	ctx, span := c.tp.Tracer("test").Start(context.Background(), "call")
	defer span.End()
	lead, cancelLead := context.WithCancel(ctx)
	first := c.admitLater(lead, call)
	held(t, hold)
	quitter, cancelQuitter := context.WithCancel(ctx)
	quit := c.admitLater(quitter, call)
	var rest []<-chan admittedAs
	for range 30 {
		rest = append(rest, c.admitLater(ctx, call))
	}
	until(t, "31 calls waiting on the resolution", func() bool { return waiting.Load() == 31 })
	cancelLead()
	cancelQuitter()
	if a := answer(t, "a joined call, cancelled", quit); !errors.Is(a.err, context.Canceled) {
		t.Errorf("a joined call, cancelled while the resolution is held: %v, want context.Canceled", a.err)
	}
	hold.Advance(time.Hour)
	for i, ch := range append(rest, first) {
		if a := answer(t, "a missing call", ch); a.err != nil || a.id.Principal != bob.Principal {
			t.Errorf("missing call %d (the last the one that started the resolution): %v acting as %v, want served as bob", i, a.err, a.id.Principal)
		}
	}
	if got := c.resolutions() - before; got != 1 {
		t.Errorf("32 misses of one key resolved %d times, want 1", got)
	}
	if got := testutil.ToFloat64(c.callers.misses) - misses; got != 32 {
		t.Errorf("32 misses of one key counted %v misses, want 32", got)
	}
	if live, _ := resolvedIn.Load().(bool); !live {
		t.Error("the shared resolution ran on a context the first call's cancel ended, want one detached from it")
	}
}

// A resolution that panics unwinds through the call that ran it, and answers
// the call that joined it ErrUnavailable; the key's next call resolves anew.
func TestAPanickingResolutionAnswersTheCallsJoinedAndLeavesNoFlight(t *testing.T) {
	c := newCaching(t)
	bob := c.user(t, "bob", "", c.share, entity.AccessReadWrite)
	call := c.ticket(bob.Principal, inLab, 0)
	var waiting atomic.Int32
	c.callers.waiting = func() { waiting.Add(1) }
	var once sync.Once
	c.callers.beforeStore = func(context.Context) { once.Do(func() { panic("resolution") }) }
	hold := c.hold(t, 1)
	panicked := make(chan any, 1)
	go func() {
		defer func() { panicked <- recover() }()
		_, _ = c.admitIn(context.Background(), call)
	}()
	held(t, hold)
	joined := c.admitLater(context.Background(), call)
	until(t, "the later call waiting on the resolution", func() bool { return waiting.Load() == 1 })
	hold.Advance(time.Hour)
	select {
	case r := <-panicked:
		if r == nil {
			t.Error("the call that ran the panicking resolution returned, want it to panic")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the call that ran the panicking resolution never ended")
	}
	if a := answer(t, "the call that joined", joined); !errors.Is(a.err, vfs.ErrUnavailable) {
		t.Errorf("the call that joined a panicking resolution: %v, want ErrUnavailable", a.err)
	}
	c.twice(t, "the next call", call, true)
}

// A caller is served until its ticket's end and not at it: past it, the next
// calls miss, and none is stored.
func TestACallerPastItsFreshMisses(t *testing.T) {
	c := newCaching(t)
	bob := c.user(t, "bob", "", c.share, entity.AccessReadWrite)
	call := c.ticket(bob.Principal, inLab, 0)
	c.twice(t, "a fresh ticket", call, true)
	c.clock.Advance(24 * time.Hour)
	before := c.resolutions()
	for range 2 {
		if _, err := c.admitCall(call); err != nil {
			t.Fatalf("a call at its ticket's end: %v", err)
		}
	}
	if got := c.resolutions() - before; got != 2 {
		t.Errorf("two calls at their ticket's end resolved %d times, want 2", got)
	}
}

// Past its bound of entries the cache evicts the least recently used, and
// counts it; past its bound of group references too.
func TestTheCacheEvictsTheLeastRecentlyUsed(t *testing.T) {
	c := newCaching(t)
	bob := c.user(t, "bob", "", c.share, entity.AccessReadWrite)
	staff := c.finance(t)
	c.must(t, c.cp.AddMember(t.Context(), entity.Membership{Group: staff.Principal, Member: bob.Principal}))
	c.raise(t)
	a, b, d := c.ticket(bob.Principal, inLab, 1), c.ticket(bob.Principal, inLab, 2), c.ticket(bob.Principal, inLab, 3)

	c.callers = newCallers(c.srv, c.clock.Now, 2, maxCallerGroups)
	c.twice(t, "a", a, true)
	c.twice(t, "b", b, true)
	c.twice(t, "a again", a, false)
	c.twice(t, "d, past the bound", d, true)
	if got := testutil.ToFloat64(c.callers.evictions); got != 1 {
		t.Errorf("evictions past a bound of 2 = %v, want 1", got)
	}
	c.twice(t, "a, used more recently than b", a, false)
	c.twice(t, "b, the one evicted", b, true)

	c.callers = newCallers(c.srv, c.clock.Now, 10, maxCallerGroups)
	c.twice(t, "a, measured", a, true)
	groups := c.callers.groups
	if groups == 0 {
		t.Fatal("bob's caller holds no group reference, so the group bound is not exercised")
	}
	c.callers = newCallers(c.srv, c.clock.Now, 10, groups)
	c.twice(t, "a, within the group bound", a, true)
	c.twice(t, "b, past the group bound", b, true)
	if got := testutil.ToFloat64(c.callers.evictions); got != 1 {
		t.Errorf("evictions past a bound of %d group references = %v, want 1", groups, got)
	}
	c.twice(t, "a, evicted for b's groups", a, true)
}

// An NTLM caller, whose Fresh is zero, is never stored: each call resolves,
// and a disable reaches the next call with no State applied.
func TestACallerWithNoFreshIsNeverStored(t *testing.T) {
	c := newCaching(t)
	bob := c.user(t, "bob", "", c.share, entity.AccessReadWrite)
	call := withSIDs(bob.Principal, vfs.ProofSecret, nil, nil)
	before := c.resolutions()
	for range 2 {
		if _, err := c.admitCall(call); err != nil {
			t.Fatalf("bob's NTLM call: %v", err)
		}
	}
	if got := c.resolutions() - before; got != 2 {
		t.Errorf("two NTLM calls resolved %d times, want 2", got)
	}
	if n := len(c.callers.entries); n != 0 {
		t.Errorf("the cache holds %d callers after NTLM calls alone, want 0", n)
	}
	bob.Disabled = true
	c.must(t, errOf(c.cp.PutUser(t.Context(), bob, userGeneration(t, c.fs.Store, bob.Principal), entity.Alloc{})))
	if _, err := c.admitCall(call); !errors.Is(err, vfs.ErrAccess) {
		t.Errorf("bob's NTLM call once disabled, no State applied: %v, want ErrAccess", err)
	}
}
