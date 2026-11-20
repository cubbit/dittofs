package identity_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	dto "github.com/prometheus/client_model/go"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/cubbit/dittofs/internal/identity"
	"github.com/cubbit/dittofs/internal/identity/directory"
	"github.com/cubbit/dittofs/internal/prof"
	"github.com/cubbit/dittofs/pkg/identity/identitytest"
	"github.com/cubbit/dittofs/pkg/metrics"
	"github.com/cubbit/dittofs/pkg/sim"
	"github.com/cubbit/dittofs/pkg/vfs/vfserr"
)

// userSearches matches every "who holds this user name" search, and none of
// the root DSE's or the domain object's reads a connection makes first.
const userSearches = "(&(objectClass=user)*"

// switched is a fault a test sets mid-run: the schedule it holds decides
// each call, and with none every call passes.
type switched struct{ cur atomic.Pointer[sim.Schedule] }

func (s *switched) At(op string, key []byte) (time.Duration, int, error) {
	if f := s.cur.Load(); f != nil {
		return f.At(op, key)
	}
	return 0, 0, nil
}

// dirEnv is an in-memory directory behind a fault wrapper, on an injected
// clock, with the caches' metrics and spans recorded.
type dirEnv struct {
	d     *identitytest.Directory
	clock *sim.TestClock
	fault *switched
	f     *identitytest.FaultyDirectory
	reg   *metrics.Registry
	spans *tracetest.SpanRecorder
	tp    *sdktrace.TracerProvider
}

func newDirEnv(t *testing.T) *dirEnv {
	t.Helper()
	e := &dirEnv{
		d:     identitytest.NewDirectory("DC=corp,DC=example"),
		clock: sim.NewTestClock(time.Date(2026, 11, 19, 8, 0, 0, 0, time.UTC)),
		fault: &switched{},
		reg:   metrics.New("test", "test"),
		spans: tracetest.NewSpanRecorder(),
	}
	e.f = identitytest.NewFaultyDirectory(e.d.Dial, e.fault)
	e.tp = sdktrace.NewTracerProvider(sdktrace.WithSampler(sdktrace.AlwaysSample()), sdktrace.WithSpanProcessor(e.spans))
	t.Cleanup(func() { _ = e.tp.Shutdown(context.Background()) })
	return e
}

// cache is a new client of e's directory and its cache.
func (e *dirEnv) cache(t *testing.T) *identity.DirectoryCache {
	t.Helper()
	name, pw := e.d.Account()
	rec := directory.Record{Address: "ldaps://dc.corp.example", Base: "DC=corp,DC=example", BindName: name, Realm: "CORP.EXAMPLE"}
	c := directory.NewClient(rec, e.f.Dial, func(context.Context) ([]byte, error) { return pw, nil }, e.clock, nil)
	dc := identity.NewDirectoryCache(c, identity.Deps{Clock: e.clock, Metrics: e.reg, Tracer: e.tp.Tracer("test")})
	t.Cleanup(dc.Close)
	return dc
}

// fail sets the rules every later dial, bind and search runs under; none
// lets every call through.
func (e *dirEnv) fail(t *testing.T, rules ...sim.Rule) {
	t.Helper()
	if len(rules) == 0 {
		e.fault.cur.Store(nil)
		return
	}
	s, err := sim.NewSchedule(1, e.clock, rules...)
	if err != nil {
		t.Fatal(err)
	}
	e.fault.cur.Store(s)
}

// down fails every user search.
func (e *dirEnv) down(t *testing.T) {
	t.Helper()
	e.fail(t, sim.Rule{Op: "search", Key: userSearches, Action: sim.FaultAction{Err: errors.New("injected directory fault")}})
}

// results is identity_directory_lookups_total{question="name"} by result.
func (e *dirEnv) results(t *testing.T) map[string]float64 {
	t.Helper()
	return e.resultsOf(t, "name")
}

// resultsOf is identity_directory_lookups_total{question=q} by result.
func (e *dirEnv) resultsOf(t *testing.T, q string) map[string]float64 {
	t.Helper()
	got := map[string]float64{}
	for _, m := range e.family(t, "identity_directory_lookups_total") {
		labels := map[string]string{}
		for _, l := range m.GetLabel() {
			labels[l.GetName()] = l.GetValue()
		}
		if labels["question"] == q {
			got[labels["result"]] = m.GetCounter().GetValue()
		}
	}
	return got
}

// queries is identity_directory_seconds' sample count: the queries sent.
func (e *dirEnv) queries(t *testing.T) uint64 {
	t.Helper()
	for _, m := range e.family(t, "identity_directory_seconds") {
		return m.GetHistogram().GetSampleCount()
	}
	return 0
}

// evictions is identity_directory_evictions_total.
func (e *dirEnv) evictions(t *testing.T) float64 {
	t.Helper()
	for _, m := range e.family(t, "identity_directory_evictions_total") {
		return m.GetCounter().GetValue()
	}
	return 0
}

func (e *dirEnv) family(t *testing.T, name string) []*dto.Metric {
	t.Helper()
	return family(t, e.reg, name)
}

// family is the metric family name gathered from reg.
func family(t *testing.T, reg *metrics.Registry, name string) []*dto.Metric {
	t.Helper()
	mfs, err := reg.Registry().Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, mf := range mfs {
		if mf.GetName() == metrics.Namespace+"_"+name {
			return mf.GetMetric()
		}
	}
	return nil
}

// lookup asks c who holds the user name name under grace.
func lookup(c *identity.DirectoryCache, name string, grace time.Duration) (directory.Answer, bool, error) {
	return c.Lookup(context.Background(), directory.ByUserName, []byte(name), grace, false)
}

// mustFind looks name up and fails the test unless it is found.
func mustFind(t *testing.T, c *identity.DirectoryCache, name string) {
	t.Helper()
	a, found, err := lookup(c, name, 15*time.Minute)
	if err != nil || !found || !strings.EqualFold(a.Name, name) {
		t.Fatalf("lookup %s = %q, found %v, %v; want it found", name, a.Name, found, err)
	}
}

// mustRefuse looks name up and fails the test unless it is ErrUnavailable.
func mustRefuse(t *testing.T, c *identity.DirectoryCache, name string, grace time.Duration) {
	t.Helper()
	if _, found, err := lookup(c, name, grace); !errors.Is(err, vfserr.ErrUnavailable) {
		t.Fatalf("lookup %s = found %v, %v; want ErrUnavailable", name, found, err)
	}
}

// wantQueries fails unless want queries have been sent.
func (e *dirEnv) wantQueries(t *testing.T, want uint64, when string) {
	t.Helper()
	if got := e.queries(t); got != want {
		t.Fatalf("%s: identity_directory_seconds count %d, want %d", when, got, want)
	}
}

// 100 concurrent lookups of one key share one query, each counted.
func TestDirectoryCacheOneQueryPerKey(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newDirEnv(t)
		e.d.AddUser(t, "alice", nil)
		c := e.cache(t)
		// The first user search waits a second, so all 100 lookups ask
		// before it is answered.
		e.fail(t, sim.Rule{Op: "search", Key: userSearches, Count: 1, Action: sim.FaultAction{Delay: time.Second}})
		var wg sync.WaitGroup
		errs := make(chan error, 100)
		for range 100 {
			wg.Go(func() {
				if _, found, err := lookup(c, "alice", 0); err != nil || !found {
					errs <- fmt.Errorf("found %v, %v", found, err)
				}
			})
		}
		synctest.Wait()
		e.clock.Advance(time.Second)
		wg.Wait()
		close(errs)
		for err := range errs {
			t.Errorf("lookup alice: %v, want found", err)
		}
		e.wantQueries(t, 1, "100 concurrent lookups")
		if got := e.results(t)["queried"]; got != 100 {
			t.Errorf("lookups counted queried %v, want 100", got)
		}
	})
}

// alice and ALICE are one entry: the second lookup sends no query.
func TestDirectoryCacheFoldsNames(t *testing.T) {
	prof.Watch(t, prof.Options{})
	e := newDirEnv(t)
	e.d.AddUser(t, "alice", nil)
	c := e.cache(t)
	mustFind(t, c, "alice")
	mustFind(t, c, "ALICE")
	e.wantQueries(t, 1, "alice then ALICE")
}

// A positive answer is fresh for 5 minutes and a negative one for 30
// seconds.
func TestDirectoryCacheTTLs(t *testing.T) {
	prof.Watch(t, prof.Options{})
	for _, tc := range []struct {
		name         string
		within, past time.Duration
	}{
		{"alice", 4*time.Minute + 59*time.Second, 5*time.Minute + time.Second},
		{"nobody", 29 * time.Second, 31 * time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newDirEnv(t)
			e.d.AddUser(t, "alice", nil)
			c := e.cache(t)
			if _, _, err := lookup(c, tc.name, 0); err != nil {
				t.Fatal(err)
			}
			e.clock.Advance(tc.within)
			if _, _, err := lookup(c, tc.name, 0); err != nil {
				t.Fatal(err)
			}
			e.wantQueries(t, 1, fmt.Sprintf("at %v", tc.within))
			e.clock.Advance(tc.past - tc.within)
			if _, _, err := lookup(c, tc.name, 0); err != nil {
				t.Fatal(err)
			}
			e.wantQueries(t, 2, fmt.Sprintf("at %v", tc.past))
		})
	}
}

// Two caches over one directory keep their own answers: each queries once.
func TestDirectoryCachePerServer(t *testing.T) {
	prof.Watch(t, prof.Options{})
	e := newDirEnv(t)
	e.d.AddUser(t, "alice", nil)
	a, b := e.cache(t), e.cache(t)
	mustFind(t, a, "alice")
	mustFind(t, b, "alice")
	mustFind(t, a, "alice")
	mustFind(t, b, "alice")
	e.wantQueries(t, 2, "two caches, two lookups each")
}

// No error is stored: a failed query leaves the positive entry it followed
// in place, and a first dial whose handshake fails is dialled again and
// answers at the next probe.
func TestDirectoryCacheStoresNoError(t *testing.T) {
	prof.Watch(t, prof.Options{})
	t.Run("positive entry kept", func(t *testing.T) {
		e := newDirEnv(t)
		e.d.AddUser(t, "alice", nil)
		c := e.cache(t)
		mustFind(t, c, "alice")
		e.down(t)
		e.clock.Advance(6 * time.Minute)
		mustFind(t, c, "alice")
		e.wantQueries(t, 2, "a failed query after a positive answer")
	})
	t.Run("failed dial", func(t *testing.T) {
		e := newDirEnv(t)
		e.d.AddUser(t, "alice", nil)
		c := e.cache(t)
		e.fail(t, sim.Rule{Op: "dial", Key: "*", Count: 1, Action: sim.FaultAction{Err: errors.New("injected handshake failure")}})
		mustRefuse(t, c, "alice", 15*time.Minute)
		e.clock.Advance(10 * time.Second)
		mustFind(t, c, "alice")
		if got := e.f.Dials(); len(got) != 2 {
			t.Errorf("dials %v, want two", got)
		}
	})
}

// An answer more than one entry gives is counted ambiguous and stored
// nowhere: the next lookup queries again.
func TestDirectoryCacheAmbiguous(t *testing.T) {
	prof.Watch(t, prof.Options{})
	e := newDirEnv(t)
	e.d.AddUser(t, "alice", nil)
	e.d.AddUser(t, "alice2", map[string][][]byte{"sAMAccountName": {[]byte("alice")}})
	c := e.cache(t)
	mustRefuse(t, c, "alice", 15*time.Minute)
	mustRefuse(t, c, "alice", 15*time.Minute)
	if got := e.results(t)["ambiguous"]; got != 2 {
		t.Errorf("lookups counted ambiguous %v, want 2", got)
	}
	e.wantQueries(t, 2, "two ambiguous lookups")
}

// The stale grace: alice and bob, resolved before the directory failed, are
// served stale within the TTL plus the grace; carol, never resolved, is
// refused; alice is refused past the grace; and with a grace of zero she is
// refused past the TTL.
func TestDirectoryCacheStaleGrace(t *testing.T) {
	prof.Watch(t, prof.Options{})
	t.Run("15 minutes", func(t *testing.T) {
		e := newDirEnv(t)
		for _, n := range []string{"alice", "bob", "carol"} {
			e.d.AddUser(t, n, nil)
		}
		c := e.cache(t)
		mustFind(t, c, "alice")
		mustFind(t, c, "bob")
		e.down(t)
		e.clock.Advance(6 * time.Minute)
		mustFind(t, c, "alice")
		mustFind(t, c, "bob")
		e.clock.Advance(14 * time.Minute) // alice's answer is now the TTL plus the grace old
		mustFind(t, c, "alice")
		mustRefuse(t, c, "carol", 15*time.Minute)
		e.clock.Advance(time.Second)
		mustRefuse(t, c, "alice", 15*time.Minute)
		got := e.results(t)
		if got["stale"] != 3 || got["refused"] != 2 {
			t.Errorf("lookups counted stale %v, refused %v; want 3 and 2", got["stale"], got["refused"])
		}
	})
	t.Run("zero", func(t *testing.T) {
		e := newDirEnv(t)
		e.d.AddUser(t, "alice", nil)
		c := e.cache(t)
		mustFind(t, c, "alice")
		e.down(t)
		e.clock.Advance(5*time.Minute + time.Second)
		mustRefuse(t, c, "alice", 0)
	})
}

// A lookup that asks for a fresh answer only is refused where a stale one
// would be served.
func TestDirectoryCacheFreshOnly(t *testing.T) {
	prof.Watch(t, prof.Options{})
	e := newDirEnv(t)
	e.d.AddUser(t, "alice", nil)
	c := e.cache(t)
	mustFind(t, c, "alice")
	e.down(t)
	e.clock.Advance(6 * time.Minute)
	if _, _, err := c.Lookup(context.Background(), directory.ByUserName, []byte("alice"), 15*time.Minute, true); !errors.Is(err, vfserr.ErrUnavailable) {
		t.Fatalf("fresh-only lookup = %v, want ErrUnavailable", err)
	}
	mustFind(t, c, "alice")
}

// A negative answer is never served stale: past its 30 seconds, with the
// directory failing, it is refused.
func TestDirectoryCacheNegativeNeverStale(t *testing.T) {
	prof.Watch(t, prof.Options{})
	e := newDirEnv(t)
	c := e.cache(t)
	if _, found, err := lookup(c, "nobody", 15*time.Minute); err != nil || found {
		t.Fatalf("lookup nobody = found %v, %v; want not found", found, err)
	}
	e.down(t)
	e.clock.Advance(31 * time.Second)
	mustRefuse(t, c, "nobody", 15*time.Minute)
}

// A query the directory has not answered by 5 seconds fails at the timeout.
// The search is held until its context ends rather than delayed 6 seconds,
// since the fault wrapper's delay ignores the context, as no real connection
// does.
func TestDirectoryCacheQueryTimeout(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newDirEnv(t)
		e.d.AddUser(t, "alice", nil)
		c := e.cache(t)
		e.fail(t, sim.Rule{Op: "search", Key: userSearches, Action: sim.FaultAction{Err: identitytest.ErrHold}})
		done := make(chan error, 1)
		go func() {
			_, _, err := lookup(c, "alice", 0)
			done <- err
		}()
		synctest.Wait()
		e.clock.Advance(5 * time.Second)
		synctest.Wait()
		select {
		case err := <-done:
			if !errors.Is(err, vfserr.ErrUnavailable) {
				t.Errorf("lookup = %v, want ErrUnavailable", err)
			}
		default:
			t.Error("lookup still waiting at 5 seconds, want it failed at the timeout")
		}
	})
}

// Past 65 536 entries the least recently used is evicted, counted, and its
// next lookup is a query that finds it.
func TestDirectoryCacheEvicts(t *testing.T) {
	if testing.Short() {
		t.Skip("65 537 lookups")
	}
	// The heap holds the full cache and the test clock's 65 537 spent
	// timers until the test ends.
	prof.Watch(t, prof.Options{HeapSlack: 256 << 20})
	e := newDirEnv(t)
	e.d.AddUser(t, "alice", nil)
	c := e.cache(t)
	mustFind(t, c, "alice")
	for i := range 1 << 16 {
		if _, _, err := lookup(c, fmt.Sprintf("u%d", i), 0); err != nil {
			t.Fatal(err)
		}
	}
	if got := e.evictions(t); got != 1 {
		t.Errorf("evictions %v, want 1", got)
	}
	before := e.queries(t)
	mustFind(t, c, "alice")
	e.wantQueries(t, before+1, "alice after her eviction")
}

// A 65th concurrent query waits until one of the 64 running ends.
func TestDirectoryCacheBoundsQueriesInFlight(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newDirEnv(t)
		c := e.cache(t)
		mustFind(t, c, "Administrator") // the connection, opened before the burst
		e.fail(t, sim.Rule{Op: "search", Key: userSearches, Count: 64, Action: sim.FaultAction{Delay: time.Second}})
		var wg sync.WaitGroup
		for i := range 65 {
			wg.Go(func() { _, _, _ = lookup(c, fmt.Sprintf("u%d", i), 0) })
		}
		synctest.Wait()
		if got := c.Waiting(); got != 1 {
			t.Errorf("queries waiting for a slot %d, want 1", got)
		}
		// The connection's two reads, the first lookup's search and 64 of the
		// burst's.
		if got := len(e.f.Queries()); got != 3+64 {
			t.Errorf("searches sent %d, want 67, 64 of them the burst's", got)
		}
		e.clock.Advance(time.Second)
		wg.Wait()
		if got := len(e.f.Queries()); got != 3+65 {
			t.Errorf("searches sent %d once the slots freed, want 68", got)
		}
	})
}

// A query that waits 5 seconds for a slot is not sent: its lookup answers
// from the stale grace, and carol's, with no entry, is refused.
func TestDirectoryCacheBoundsTheSlotWait(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newDirEnv(t)
		e.d.AddUser(t, "alice", nil)
		e.d.AddUser(t, "carol", nil)
		c := e.cache(t)
		mustFind(t, c, "alice")
		e.clock.Advance(6 * time.Minute)
		// The burst holds every slot for 10 seconds, past the slot wait.
		e.fail(t, sim.Rule{Op: "search", Key: userSearches, Count: 64, Action: sim.FaultAction{Delay: 10 * time.Second}})
		var wg sync.WaitGroup
		for i := range 64 {
			wg.Go(func() { _, _, _ = lookup(c, fmt.Sprintf("u%d", i), 0) })
		}
		synctest.Wait()
		sent := len(e.f.Queries())
		type answer struct {
			found bool
			err   error
		}
		done := make(chan answer, 2)
		for _, n := range []string{"alice", "carol"} {
			go func() {
				_, found, err := lookup(c, n, 15*time.Minute)
				done <- answer{found, err}
			}()
		}
		synctest.Wait()
		if got := c.Waiting(); got != 2 {
			t.Fatalf("queries waiting for a slot %d, want 2", got)
		}
		e.clock.Advance(5 * time.Second)
		synctest.Wait()
		if got := c.Waiting(); got != 0 {
			t.Errorf("queries waiting for a slot at 5 seconds %d, want 0", got)
		}
		for range 2 {
			select {
			case a := <-done:
				if !a.found && !errors.Is(a.err, vfserr.ErrUnavailable) {
					t.Errorf("lookup = found %v, %v; want alice found and carol ErrUnavailable", a.found, a.err)
				}
			default:
				t.Fatal("a lookup still waiting at 5 seconds, want it answered at the slot wait")
			}
		}
		got := e.results(t)
		if got["stale"] != 1 || got["refused"] != 1 {
			t.Errorf("lookups counted stale %v, refused %v; want 1 and 1", got["stale"], got["refused"])
		}
		e.clock.Advance(5 * time.Second)
		wg.Wait()
		if got := len(e.f.Queries()); got != sent {
			t.Errorf("searches sent %d once the slots freed, want %d: none for the lookups that left", got, sent)
		}
	})
}

// A caller that stops waiting is served a positive entry within the grace,
// as a failed query is.
func TestDirectoryCacheLeavingCallerServedStale(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newDirEnv(t)
		e.d.AddUser(t, "alice", nil)
		c := e.cache(t)
		mustFind(t, c, "alice")
		e.clock.Advance(6 * time.Minute)
		e.fail(t, sim.Rule{Op: "search", Key: userSearches, Count: 1, Action: sim.FaultAction{Delay: time.Second}})
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if _, found, err := c.Lookup(ctx, directory.ByUserName, []byte("alice"), 15*time.Minute, false); err != nil || !found {
			t.Errorf("lookup alice by a caller that left = found %v, %v; want found stale", found, err)
		}
		if got := e.results(t)["stale"]; got != 1 {
			t.Errorf("lookups counted stale %v, want 1", got)
		}
		// The detached query is still on its way to the delayed search: let
		// it arm its timers before the clock passes them.
		synctest.Wait()
		e.clock.Advance(time.Second)
		synctest.Wait()
	})
}

// The query runs without the first caller's cancellation: A opens it and
// leaves, and B, who joined it, is answered when it completes.
func TestDirectoryCacheQueryOutlivesFirstCaller(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newDirEnv(t)
		e.d.AddUser(t, "alice", nil)
		c := e.cache(t)
		e.fail(t, sim.Rule{Op: "search", Key: userSearches, Count: 1, Action: sim.FaultAction{Delay: time.Second}})
		ctx, cancel := context.WithCancel(context.Background())
		a := make(chan error, 1)
		go func() {
			_, _, err := c.Lookup(ctx, directory.ByUserName, []byte("alice"), 0, false)
			a <- err
		}()
		synctest.Wait()
		type answer struct {
			found bool
			err   error
		}
		b := make(chan answer, 1)
		go func() {
			_, found, err := lookup(c, "alice", 0)
			b <- answer{found, err}
		}()
		synctest.Wait()
		cancel()
		if err := <-a; !errors.Is(err, vfserr.ErrUnavailable) {
			t.Errorf("lookup by A, who left = %v, want ErrUnavailable", err)
		}
		e.clock.Advance(time.Second)
		if got := <-b; got.err != nil || !got.found {
			t.Errorf("lookup by B, after A left = found %v, %v; want found", got.found, got.err)
		}
		e.wantQueries(t, 1, "A and B's shared query")
	})
}

// Each lookup is counted once under its result, and each query sent adds
// one sample to identity_directory_seconds.
func TestDirectoryCacheCountsEachResult(t *testing.T) {
	prof.Watch(t, prof.Options{})
	e := newDirEnv(t)
	e.d.AddUser(t, "alice", nil)
	e.d.AddUser(t, "carol", nil)
	c := e.cache(t)
	want := map[string]float64{}
	var sent uint64
	for _, step := range []struct {
		do     func()
		result string
		query  bool
	}{
		{func() { mustFind(t, c, "alice") }, "queried", true},
		{func() { mustFind(t, c, "alice") }, "fresh", false},
		{func() { _, _, _ = lookup(c, "nobody", 0) }, "not_found", true},
		{func() { e.down(t); e.clock.Advance(6 * time.Minute); mustFind(t, c, "alice") }, "stale", true},
		// alice's failed query raised directory_unreachable, so carol's
		// lookup is refused unsent.
		{func() { mustRefuse(t, c, "carol", 15*time.Minute) }, "refused", false},
	} {
		step.do()
		want[step.result]++
		if step.query {
			sent++
		}
		got := e.results(t)
		for _, r := range []string{"fresh", "queried", "not_found", "stale", "refused", "ambiguous"} {
			if got[r] != want[r] {
				t.Errorf("after a %s lookup: counted %s %v, want %v", step.result, r, got[r], want[r])
			}
		}
		e.wantQueries(t, sent, "after a "+step.result+" lookup")
	}
}

// Each query is one span, a child of the span that opened the lookup, and
// none carries a name or SID; a fresh answer opens none.
func TestDirectoryCacheSpans(t *testing.T) {
	prof.Watch(t, prof.Options{})
	e := newDirEnv(t)
	e.d.AddUser(t, "alice", nil)
	e.d.AddUser(t, "bob", nil)
	c := e.cache(t)
	ctx, parent := e.tp.Tracer("test").Start(context.Background(), "request")
	for _, n := range []string{"alice", "bob", "alice"} {
		if _, found, err := c.Lookup(ctx, directory.ByUserName, []byte(n), 0, false); err != nil || !found {
			t.Fatalf("lookup %s = found %v, %v", n, found, err)
		}
	}
	parent.End()
	var queries int
	for _, s := range e.spans.Ended() {
		if s.Name() != "identity.directory.query" {
			continue
		}
		queries++
		if s.Parent().SpanID() != parent.SpanContext().SpanID() {
			t.Errorf("query span's parent %v, want the lookup's span %v", s.Parent().SpanID(), parent.SpanContext().SpanID())
		}
		for _, a := range s.Attributes() {
			v := strings.ToLower(a.Value.String())
			if strings.Contains(v, "alice") || strings.Contains(v, "bob") || strings.Contains(v, "s-1-") {
				t.Errorf("query span attribute %s = %q carries a name or SID", a.Key, v)
			}
		}
	}
	if queries != 2 {
		t.Errorf("query spans %d, want 2: alice's and bob's, none for the fresh answer", queries)
	}
}

// A thousand and twenty-four SIDs cost 16 queries, one per batch, and asked
// again none, every answer fresh and in the order asked.
func TestDirectoryCacheBatchesSIDs(t *testing.T) {
	prof.Watch(t, prof.Options{})
	ctx := context.Background()
	e := newDirEnv(t)
	name, pw := e.d.Account()
	plain := directory.NewClient(directory.Record{Address: "ldaps://dc.corp.example", Base: "DC=corp,DC=example",
		BindName: name, Realm: "CORP.EXAMPLE"}, e.d.Dial, func(context.Context) ([]byte, error) { return pw, nil }, e.clock, nil)
	t.Cleanup(plain.Close)
	var sids [][]byte
	for i := range 1024 {
		n := fmt.Sprintf("u%04d", i)
		e.d.AddUser(t, n, nil)
		a, _, err := plain.Lookup(ctx, directory.ByUserName, []byte(n))
		if err != nil {
			t.Fatal(err)
		}
		sids = append(sids, a.SID)
	}
	c := e.cache(t)
	// total is the queries sent by the end of the round: the warm one adds none.
	for _, round := range []struct {
		when  string
		total uint64
	}{{"cold", 16}, {"warm", 16}} {
		got, err := c.LookupSIDs(ctx, sids, 15*time.Minute)
		if err != nil || len(got) != len(sids) {
			t.Fatalf("%s: LookupSIDs(1024) = %d answers, %v; want 1024", round.when, len(got), err)
		}
		for i, a := range got {
			if string(a.SID) != string(sids[i]) {
				t.Fatalf("%s: answer %d holds SID %x, want %x", round.when, i, a.SID, sids[i])
			}
		}
		e.wantQueries(t, round.total, round.when+" lookup of 1024 SIDs")
	}
}

// sidEnv is alice, bob and carol in a directory, each looked up by name
// through a new cache, and their SIDs.
func sidEnv(t *testing.T) (*dirEnv, *identity.DirectoryCache, map[string][]byte) {
	t.Helper()
	e := newDirEnv(t)
	for _, n := range []string{"alice", "bob", "carol"} {
		e.d.AddUser(t, n, nil)
	}
	c := e.cache(t)
	sid := map[string][]byte{}
	for _, n := range []string{"alice", "bob", "carol"} {
		a, _, err := lookup(c, n, 0)
		if err != nil {
			t.Fatal(err)
		}
		sid[n] = a.SID
	}
	return e, c, sid
}

// sidsDown fails every SID batch search.
func (e *dirEnv) sidsDown(t *testing.T) {
	t.Helper()
	e.fail(t, sim.Rule{Op: "search", Key: "(|*", Action: sim.FaultAction{Err: errors.New("injected directory fault")}})
}

// A failed batch serves a SID from a positive entry within the grace, a SID
// it does not hold fails the whole lookup, and a SID no entry holds is left
// out of an answered one. A batch in which two entries hold one SID fails
// the lookup, counted ambiguous, served from no entry, and raises no outage.
func TestDirectoryCacheSIDsFailWhole(t *testing.T) {
	prof.Watch(t, prof.Options{})
	ctx := context.Background()
	t.Run("outage", func(t *testing.T) {
		e, c, sid := sidEnv(t)
		if got, err := c.LookupSIDs(ctx, [][]byte{sid["alice"], []byte("no such SID"), sid["bob"]}, 0); err != nil || len(got) != 2 {
			t.Fatalf("LookupSIDs(alice, unknown, bob) = %d answers, %v; want 2", len(got), err)
		}
		e.sidsDown(t)
		e.clock.Advance(6 * time.Minute)
		if got, err := c.LookupSIDs(ctx, [][]byte{sid["alice"], sid["bob"]}, 15*time.Minute); err != nil || len(got) != 2 {
			t.Errorf("LookupSIDs(alice, bob) with the directory down = %d answers, %v; want both served stale", len(got), err)
		}
		if got, err := c.LookupSIDs(ctx, [][]byte{sid["alice"], sid["carol"]}, 15*time.Minute); !errors.Is(err, vfserr.ErrUnavailable) || got != nil {
			t.Errorf("LookupSIDs(alice, carol) with carol never asked by SID = %d answers, %v; want none, ErrUnavailable", len(got), err)
		}
	})
	t.Run("two entries hold one SID", func(t *testing.T) {
		e, c, sid := sidEnv(t)
		if got, err := c.LookupSIDs(ctx, [][]byte{sid["alice"]}, 0); err != nil || len(got) != 1 {
			t.Fatalf("LookupSIDs(alice) = %d answers, %v; want 1", len(got), err)
		}
		e.d.AddUser(t, "twin", map[string][][]byte{"objectSid": {sid["alice"]}})
		e.clock.Advance(6 * time.Minute)
		if got, err := c.LookupSIDs(ctx, [][]byte{sid["alice"]}, 15*time.Minute); !errors.Is(err, vfserr.ErrUnavailable) || got != nil {
			t.Errorf("LookupSIDs(alice) with a twin holding her SID = %d answers, %v; want none, ErrUnavailable", len(got), err)
		}
		if got := e.resultsOf(t, "sid"); got["ambiguous"] != 1 || got["stale"] != 0 {
			t.Errorf("SID lookups counted ambiguous %v, stale %v; want 1 and 0", got["ambiguous"], got["stale"])
		}
		// No outage raised: a user no entry holds yet is still asked, where a
		// raised outage would refuse it unsent.
		e.d.AddUser(t, "dave", nil)
		mustFind(t, c, "dave")
	})
}

// A failed batch serves stale only what a failed single lookup would: a
// "no such entry" SID fails the set rather than being dropped from it, and a
// positive one is refused past the TTL plus the grace.
func TestDirectoryCacheSIDsStaleGrace(t *testing.T) {
	prof.Watch(t, prof.Options{})
	ctx := context.Background()
	t.Run("no such entry", func(t *testing.T) {
		e, c, sid := sidEnv(t)
		unknown := []byte("no such SID")
		if got, err := c.LookupSIDs(ctx, [][]byte{sid["alice"], unknown}, 0); err != nil || len(got) != 1 {
			t.Fatalf("LookupSIDs(alice, unknown) = %d answers, %v; want 1", len(got), err)
		}
		e.sidsDown(t)
		e.clock.Advance(6 * time.Minute)
		if got, err := c.LookupSIDs(ctx, [][]byte{sid["alice"], unknown}, 15*time.Minute); !errors.Is(err, vfserr.ErrUnavailable) || got != nil {
			t.Errorf("LookupSIDs(alice, unknown) with the directory down = %d answers, %v; want none, ErrUnavailable", len(got), err)
		}
		if got := e.resultsOf(t, "sid"); got["stale"] != 0 || got["refused"] != 1 {
			t.Errorf("SID lookups counted stale %v, refused %v; want 0 and 1: a refused set serves nothing stale", got["stale"], got["refused"])
		}
	})
	t.Run("past the grace", func(t *testing.T) {
		e, c, sid := sidEnv(t)
		if got, err := c.LookupSIDs(ctx, [][]byte{sid["alice"]}, 0); err != nil || len(got) != 1 {
			t.Fatalf("LookupSIDs(alice) = %d answers, %v; want 1", len(got), err)
		}
		e.sidsDown(t)
		e.clock.Advance(20*time.Minute + time.Second) // the TTL plus the grace, and a second
		if got, err := c.LookupSIDs(ctx, [][]byte{sid["alice"]}, 15*time.Minute); !errors.Is(err, vfserr.ErrUnavailable) || got != nil {
			t.Errorf("LookupSIDs(alice) past the grace = %d answers, %v; want none, ErrUnavailable", len(got), err)
		}
	})
}

// Concurrent batches whose SIDs concatenate to the same bytes, ["ab", "c"]
// and ["a", "bc"], are distinct questions: each sends its own query. The
// searches are held until the query timeout so both are in flight at once.
func TestDirectoryCacheSIDBatchKeys(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newDirEnv(t)
		c := e.cache(t)
		e.fail(t, sim.Rule{Op: "search", Key: "(|*", Action: sim.FaultAction{Err: identitytest.ErrHold}})
		var wg sync.WaitGroup
		for _, batch := range [][][]byte{{[]byte("ab"), []byte("c")}, {[]byte("a"), []byte("bc")}} {
			wg.Go(func() {
				if _, err := c.LookupSIDs(context.Background(), batch, 0); !errors.Is(err, vfserr.ErrUnavailable) {
					t.Errorf("LookupSIDs(%q) at the timeout = %v, want ErrUnavailable", batch, err)
				}
			})
		}
		synctest.Wait()
		e.clock.Advance(5 * time.Second)
		wg.Wait()
		var sent int
		for _, q := range e.f.Queries() {
			if strings.HasPrefix(q.Filter, "(|") {
				sent++
			}
		}
		if sent != 2 {
			t.Errorf("SID batch searches %d, want 2: one per batch", sent)
		}
	})
}

// The SID path keeps the single-key lookup rules (RFC 18 §5.2, §5.3, §5.4,
// §9.2): a SID a set repeats is one question, answered once; a "no such
// entry" answer is fresh for 30 seconds; a failed batch serves a positive
// entry within the TTL plus the grace, each use counted stale, and raises
// directory_unreachable, degraded, so the next lookup within the probe
// interval sends no query; a "no such entry" past its 30 seconds is never
// served stale, nor an entry past the grace, and a refusal makes the
// condition unhealthy; the first batch the directory answers clears it.
func TestDirectoryCacheSIDsKeepLookupRules(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx := context.Background()
		e, c, sid := sidEnv(t)
		alice, unknown := sid["alice"], []byte("no such SID")
		if got, err := c.LookupSIDs(ctx, [][]byte{alice, unknown, alice}, 15*time.Minute); err != nil || len(got) != 1 {
			t.Fatalf("LookupSIDs(alice, unknown, alice) = %d answers, %v; want alice once", len(got), err)
		}
		e.wantQueries(t, 4, "three names and one SID batch")
		e.clock.Advance(29 * time.Second)
		if got, err := c.LookupSIDs(ctx, [][]byte{unknown}, 15*time.Minute); err != nil || len(got) != 0 {
			t.Fatalf("LookupSIDs(unknown) at 29 seconds = %d answers, %v; want none", len(got), err)
		}
		e.wantQueries(t, 4, `a "no such entry" 29 seconds old`)

		// Every search fails, the probe loop's single-SID question included.
		e.fail(t, sim.Rule{Op: "search", Key: "*", Action: sim.FaultAction{Err: errors.New("injected directory fault")}})
		e.clock.Advance(6 * time.Minute)
		for i := range 2 {
			if got, err := c.LookupSIDs(ctx, [][]byte{alice}, 15*time.Minute); err != nil || len(got) != 1 {
				t.Fatalf("LookupSIDs(alice) %d with the directory down = %d answers, %v; want her served stale", i, len(got), err)
			}
		}
		e.wantQueries(t, 5, "two lookups within the probe interval")
		if got := e.resultsOf(t, "sid")["stale"]; got != 2 {
			t.Errorf("SID lookups counted stale %v, want 2", got)
		}
		wantCondition(t, c, "degraded", "alice served stale")
		if got, err := c.LookupSIDs(ctx, [][]byte{alice, unknown}, 15*time.Minute); !errors.Is(err, vfserr.ErrUnavailable) || got != nil {
			t.Errorf(`LookupSIDs(alice, unknown) with its "no such entry" expired = %d answers, %v; want none, ErrUnavailable`, len(got), err)
		}
		wantCondition(t, c, "unhealthy", "unknown refused")
		e.clock.Advance(15 * time.Minute)
		synctest.Wait() // the probe loop's quiet probe, failed
		if got, err := c.LookupSIDs(ctx, [][]byte{alice}, 15*time.Minute); !errors.Is(err, vfserr.ErrUnavailable) || got != nil {
			t.Errorf("LookupSIDs(alice) past the grace = %d answers, %v; want none, ErrUnavailable", len(got), err)
		}

		e.fail(t)
		e.clock.Advance(10 * time.Second) // the next probe is due
		if got, err := c.LookupSIDs(ctx, [][]byte{alice}, 15*time.Minute); err != nil || len(got) != 1 {
			t.Fatalf("LookupSIDs(alice) with the directory back = %d answers, %v; want her", len(got), err)
		}
		wantCondition(t, c, "", "an answered batch")
	})
}

// Concurrent lookups of one SID set share one query (RFC 18 §5.2), which runs
// on when the caller that opened it leaves: the nine that joined it are
// answered by it.
func TestDirectoryCacheSIDsShareOneQuery(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e, c, sid := sidEnv(t)
		// The first SID batch search waits a second, so every lookup asks
		// before it is answered.
		e.fail(t, sim.Rule{Op: "search", Key: "(|*", Count: 1, Action: sim.FaultAction{Delay: time.Second}})
		ctx, cancel := context.WithCancel(context.Background())
		first := make(chan error, 1)
		go func() {
			_, err := c.LookupSIDs(ctx, [][]byte{sid["alice"]}, 0)
			first <- err
		}()
		synctest.Wait()
		errs := make(chan error, 9)
		for range 9 {
			go func() {
				got, err := c.LookupSIDs(context.Background(), [][]byte{sid["alice"]}, 0)
				if err == nil && len(got) != 1 {
					err = fmt.Errorf("%d answers", len(got))
				}
				errs <- err
			}()
		}
		synctest.Wait()
		cancel()
		if err := <-first; !errors.Is(err, vfserr.ErrUnavailable) {
			t.Errorf("LookupSIDs(alice) by the caller that left = %v, want ErrUnavailable", err)
		}
		synctest.Wait()
		e.clock.Advance(time.Second)
		for range 9 {
			if err := <-errs; err != nil {
				t.Errorf("LookupSIDs(alice) after the first caller left = %v, want her", err)
			}
		}
		e.wantQueries(t, 4, "three names and ten concurrent lookups of one SID set")
	})
}

// A changed record's first lookup queries the new client; an Apply that
// changes only the stale grace keeps the cache, and the grace it sets applies
// at the next lookup: the bind password resolved through the secret provider.
func TestDirectoryCacheFollowsApply(t *testing.T) {
	e := newAD(t)
	e.d.AddUser(t, "alice", nil)
	queries := func() uint64 {
		for _, m := range family(t, e.reg, "identity_directory_seconds") {
			return m.GetHistogram().GetSampleCount()
		}
		return 0
	}
	ask := func(when string, want uint64, found bool) {
		t.Helper()
		_, ok, err := e.s.DirectoryLookup(context.Background(), directory.ByUserName, []byte("alice"))
		if found && (err != nil || !ok) {
			t.Errorf("%s: lookup = found %v, %v; want found", when, ok, err)
		}
		if !found && !errors.Is(err, vfserr.ErrUnavailable) {
			t.Errorf("%s: lookup = found %v, %v; want ErrUnavailable", when, ok, err)
		}
		if got := queries(); got != want {
			t.Errorf("%s: identity_directory_seconds count %d, want %d", when, got, want)
		}
	}
	apply := func(f func(*identity.State)) {
		t.Helper()
		st := *e.s.Applied()
		d := *st.Directory
		st.Directory = &d
		f(&st)
		if err := e.s.Apply(st); err != nil {
			t.Fatal(err)
		}
	}
	ask("first", 1, true)
	ask("fresh", 1, true)
	apply(func(st *identity.State) { st.Directory.Address = "ldaps://dc2.corp.example" })
	ask("after a new record", 2, true)
	e.fail(t, sim.Rule{Op: "search", Key: userSearches, Action: sim.FaultAction{Err: errors.New("injected directory fault")}})
	e.clock.Advance(6 * time.Minute)
	apply(func(st *identity.State) { st.Settings.DirectoryStaleGrace = 0 })
	ask("grace zero, past the TTL", 3, false)
	apply(func(st *identity.State) { st.Settings.DirectoryStaleGrace = 15 * time.Minute })
	ask("grace 15 minutes, within it", 3, true)
}

// sidTerms is the SIDs the SID batch searches sent so far asked, each batch's
// terms counted: a SID asked by two searches counts twice.
func (e *dirEnv) sidTerms() int {
	n := 0
	for _, q := range e.f.Queries() {
		if strings.HasPrefix(q.Filter, "(|") {
			n += strings.Count(q.Filter, "(objectSid=")
		}
	}
	return n
}

// Concurrent lookups whose SID sets overlap, {alice, bob} and {bob, carol},
// share one query per SID (RFC 18 §5.2): bob is asked once, by the query the
// first lookup opened, and the second asks only carol. When the shared query
// fails, the second lookup's bob is answered as a failed query is: served
// stale within the grace, refused with no entry to serve.
func TestDirectoryCacheSIDsShareOneQueryPerSID(t *testing.T) {
	t.Run("answered", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			e, c, sid := sidEnv(t)
			// The first SID batch search waits a second, so the second lookup
			// asks while it is in flight.
			e.fail(t, sim.Rule{Op: "search", Key: "(|*", Count: 1, Action: sim.FaultAction{Delay: time.Second}})
			sets := [][]string{{"alice", "bob"}, {"bob", "carol"}}
			type result struct {
				got []directory.Answer
				err error
			}
			res := make([]chan result, len(sets))
			for i, names := range sets {
				res[i] = make(chan result, 1)
				go func() {
					got, err := c.LookupSIDs(context.Background(), [][]byte{sid[names[0]], sid[names[1]]}, 0)
					res[i] <- result{got, err}
				}()
				synctest.Wait()
			}
			e.clock.Advance(time.Second)
			for i, names := range sets {
				r := <-res[i]
				if r.err != nil || len(r.got) != 2 {
					t.Fatalf("LookupSIDs(%v) = %d answers, %v; want 2", names, len(r.got), r.err)
				}
				for j, a := range r.got {
					if string(a.SID) != string(sid[names[j]]) {
						t.Errorf("LookupSIDs(%v) answer %d holds SID %x, want %s's %x", names, j, a.SID, names[j], sid[names[j]])
					}
				}
			}
			if got := e.sidTerms(); got != 3 {
				t.Errorf("SIDs asked by the batch searches %d, want 3: bob asked once", got)
			}
			e.wantQueries(t, 5, "three names and two SID batches")
		})
	})
	for _, tc := range []struct {
		name      string
		warm      bool // every SID looked up, and expired, before the overlap
		wantErr   bool
		wantStale float64
	}{
		{"failed, served stale", true, false, 3},
		{"failed, refused", false, true, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				e, c, sid := sidEnv(t)
				if tc.warm {
					if got, err := c.LookupSIDs(context.Background(), [][]byte{sid["alice"], sid["bob"], sid["carol"]}, 0); err != nil || len(got) != 3 {
						t.Fatalf("LookupSIDs(alice, bob, carol) = %d answers, %v; want 3", len(got), err)
					}
					e.clock.Advance(6 * time.Minute) // past the TTL, within the grace
				}
				// The first SID batch search, {alice, bob}, is held until the
				// query timeout and fails; carol's is answered.
				e.fail(t, sim.Rule{Op: "search", Key: "(|*", Count: 1, Action: sim.FaultAction{Err: identitytest.ErrHold}})
				first := make(chan error, 1)
				go func() {
					_, err := c.LookupSIDs(context.Background(), [][]byte{sid["alice"], sid["bob"]}, 15*time.Minute)
					first <- err
				}()
				synctest.Wait()
				second := make(chan error, 1)
				var got []directory.Answer
				go func() {
					var err error
					got, err = c.LookupSIDs(context.Background(), [][]byte{sid["bob"], sid["carol"]}, 15*time.Minute)
					second <- err
				}()
				synctest.Wait()
				e.clock.Advance(5 * time.Second)
				if err := <-first; (err != nil) != tc.wantErr {
					t.Errorf("LookupSIDs(alice, bob) with its query failed = %v, want an error %v", err, tc.wantErr)
				}
				err := <-second
				if tc.wantErr {
					if !errors.Is(err, vfserr.ErrUnavailable) || got != nil {
						t.Errorf("LookupSIDs(bob, carol) with the shared query failed and bob never answered = %d answers, %v; want none, ErrUnavailable", len(got), err)
					}
				} else if err != nil || len(got) != 2 {
					t.Errorf("LookupSIDs(bob, carol) with the shared query failed = %d answers, %v; want bob stale and carol", len(got), err)
				}
				if got := e.resultsOf(t, "sid")["stale"]; got != tc.wantStale {
					t.Errorf("SID lookups counted stale %v, want %v", got, tc.wantStale)
				}
				if got, want := e.sidTerms(), map[bool]int{true: 6, false: 3}[tc.warm]; got != want {
					t.Errorf("SIDs asked by the batch searches %d, want %d: bob asked once by the overlap", got, want)
				}
			})
		})
	}
}

// A lookup fails ambiguous only when its own query holds the SID two entries
// hold: with a twin holding carol's SID, {carol, alice} fails ambiguous, and
// {alice, bob}, which joined its query for alice, asks alice again in a query
// of its own and is answered for both.
func TestDirectoryCacheSIDsAmbiguityStaysWithItsLookup(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e, c, sid := sidEnv(t)
		e.d.AddUser(t, "twin", map[string][][]byte{"objectSid": {sid["carol"]}})
		// The first SID batch search waits a second, so the second lookup
		// joins it for alice.
		e.fail(t, sim.Rule{Op: "search", Key: "(|*", Count: 1, Action: sim.FaultAction{Delay: time.Second}})
		first := make(chan error, 1)
		go func() {
			_, err := c.LookupSIDs(context.Background(), [][]byte{sid["carol"], sid["alice"]}, 15*time.Minute)
			first <- err
		}()
		synctest.Wait()
		second := make(chan error, 1)
		var got []directory.Answer
		go func() {
			var err error
			got, err = c.LookupSIDs(context.Background(), [][]byte{sid["alice"], sid["bob"]}, 15*time.Minute)
			second <- err
		}()
		synctest.Wait()
		e.clock.Advance(time.Second)
		if err := <-first; !errors.Is(err, directory.ErrAmbiguous) {
			t.Errorf("LookupSIDs(carol, alice) with a twin holding carol's SID = %v, want ErrAmbiguous", err)
		}
		if err := <-second; err != nil || len(got) != 2 {
			t.Fatalf("LookupSIDs(alice, bob), joined to the ambiguous query for alice = %d answers, %v; want 2", len(got), err)
		}
		for i, n := range []string{"alice", "bob"} {
			if string(got[i].SID) != string(sid[n]) {
				t.Errorf("LookupSIDs(alice, bob) answer %d holds SID %x, want %s's %x", i, got[i].SID, n, sid[n])
			}
		}
		if got := e.resultsOf(t, "sid")["ambiguous"]; got != 1 {
			t.Errorf("SID lookups counted ambiguous %v, want 1", got)
		}
	})
}
