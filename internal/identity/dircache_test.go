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
	t.Cleanup(c.Close)
	return identity.NewDirectoryCache(c, identity.Deps{Clock: e.clock, Metrics: e.reg, Tracer: e.tp.Tracer("test")})
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
	got := map[string]float64{}
	for _, m := range e.family(t, "identity_directory_lookups_total") {
		labels := map[string]string{}
		for _, l := range m.GetLabel() {
			labels[l.GetName()] = l.GetValue()
		}
		if labels["question"] == "name" {
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
	mfs, err := e.reg.Registry().Gather()
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
// answers at the next lookup.
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
		{func() { mustRefuse(t, c, "carol", 15*time.Minute) }, "refused", true},
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
