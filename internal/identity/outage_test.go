package identity_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"testing/synctest"
	"time"

	"github.com/cubbit/dittofs/internal/identity"
	"github.com/cubbit/dittofs/internal/identity/directory"
	"github.com/cubbit/dittofs/internal/prof"
	"github.com/cubbit/dittofs/pkg/health"
	"github.com/cubbit/dittofs/pkg/identity/identitytest"
	"github.com/cubbit/dittofs/pkg/sim"
	"github.com/cubbit/dittofs/pkg/vfs/vfserr"
)

// wantCondition fails unless c reports want: "" for no condition, else
// directory_unreachable on the node at the severity RFC 25 §3.9's rule reads
// from its cause, "unhealthy" for ErrUnavailable and "degraded" otherwise.
func wantCondition(t *testing.T, c *identity.DirectoryCache, want, when string) {
	t.Helper()
	cs := c.Conditions(nil)
	got := ""
	switch {
	case len(cs) > 1:
		t.Fatalf("%s: conditions %v, want at most one", when, cs)
	case len(cs) == 1:
		if cs[0].Name != "directory_unreachable" || cs[0].Subject.Scope != health.ScopeNode {
			t.Fatalf("%s: condition %q on %v, want directory_unreachable on the node", when, cs[0].Name, cs[0].Subject.Scope)
		}
		got = "degraded"
		if errors.Is(cs[0].Cause, vfserr.ErrUnavailable) {
			got = "unhealthy"
		}
	}
	if got != want {
		t.Fatalf("%s: directory_unreachable %q, want %q", when, got, want)
	}
}

// With the directory answering, found, not found or ambiguous, no condition
// is reported.
func TestDirectoryOutageNoConditionWhileAnswering(t *testing.T) {
	prof.Watch(t, prof.Options{})
	e := newDirEnv(t)
	e.d.AddUser(t, "alice", nil)
	e.d.AddUser(t, "alice2", map[string][][]byte{"sAMAccountName": {[]byte("alice")}})
	e.d.AddUser(t, "bob", nil)
	c := e.cache(t)
	mustFind(t, c, "bob")
	if _, found, err := lookup(c, "nobody", 0); err != nil || found {
		t.Fatalf("lookup nobody = found %v, %v; want not found", found, err)
	}
	mustRefuse(t, c, "alice", 15*time.Minute) // ambiguous
	wantCondition(t, c, "", "directory answering")
	// An ambiguous answer is the directory's: the next lookup asks it.
	mustRefuse(t, c, "alice", 15*time.Minute)
	e.wantQueries(t, 4, "bob, nobody and alice twice")
}

// §10's "stale grace" row, the outage half: 100 users warmed, the directory
// failed and the clock past the TTL. Each signs in within the grace, one a
// second, while queries rise by at most one per 10 seconds.
func TestDirectoryOutageQueriesOncePerProbe(t *testing.T) {
	prof.Watch(t, prof.Options{})
	e := newDirEnv(t)
	for i := range 100 {
		e.d.AddUser(t, fmt.Sprintf("u%d", i), nil)
	}
	c := e.cache(t)
	for i := range 100 {
		mustFind(t, c, fmt.Sprintf("u%d", i))
	}
	before := e.queries(t)
	e.down(t)
	e.clock.Advance(6 * time.Minute)
	for i := range 100 {
		mustFind(t, c, fmt.Sprintf("u%d", i))
		// The first lookup's query raised the outage; one more each 10
		// seconds since.
		if got, limit := e.queries(t)-before, uint64(1+i/10); got > limit {
			t.Fatalf("at %d seconds into the outage: %d queries sent, want at most %d", i, got, limit)
		}
		e.clock.Advance(time.Second)
	}
	if got := e.results(t)["stale"]; got != 100 {
		t.Errorf("lookups counted stale %v, want 100", got)
	}
	wantCondition(t, c, "degraded", "100 users served stale")
}

// While the outage is raised dave, never resolved, is refused at once: no
// query is sent and the lookup returns before the clock moves, though every
// query would wait 5 seconds.
func TestDirectoryOutageColdLookupRefusedAtOnce(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newDirEnv(t)
		e.d.AddUser(t, "alice", nil)
		e.d.AddUser(t, "dave", nil)
		c := e.cache(t)
		mustFind(t, c, "alice")
		e.down(t)
		e.clock.Advance(6 * time.Minute)
		mustFind(t, c, "alice") // its failed query raises the outage
		e.fail(t, sim.Rule{Op: "search", Key: userSearches, Action: sim.FaultAction{Delay: 5 * time.Second}})
		sent := e.queries(t)
		done := make(chan error, 1)
		go func() {
			_, _, err := lookup(c, "dave", 15*time.Minute)
			done <- err
		}()
		synctest.Wait()
		select {
		case err := <-done:
			if !errors.Is(err, vfserr.ErrUnavailable) {
				t.Errorf("lookup dave = %v, want ErrUnavailable", err)
			}
		default:
			t.Fatal("lookup dave still waiting with the clock unmoved, want it refused at once")
		}
		e.wantQueries(t, sent, "dave refused during the outage")
	})
}

// The condition is degraded after a stale answer and unhealthy after a
// refusal, staying unhealthy through later stale answers; once the
// directory is back, lookups before the probe is due still send nothing,
// and the probe clears the condition, after which lookups ask again.
func TestDirectoryOutageConditionAndRecovery(t *testing.T) {
	prof.Watch(t, prof.Options{})
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
	wantCondition(t, c, "degraded", "alice served stale")
	mustRefuse(t, c, "carol", 15*time.Minute)
	wantCondition(t, c, "unhealthy", "carol refused")
	mustFind(t, c, "bob")
	wantCondition(t, c, "unhealthy", "bob served stale after carol's refusal")

	e.fail(t)
	sent := e.queries(t)
	e.clock.Advance(9 * time.Second)
	mustRefuse(t, c, "carol", 15*time.Minute)
	e.wantQueries(t, sent, "carol 9 seconds after the last probe")
	wantCondition(t, c, "unhealthy", "before the probe")

	e.clock.Advance(time.Second)
	mustFind(t, c, "carol")
	e.wantQueries(t, sent+1, "the probe")
	wantCondition(t, c, "", "after the probe answered")
	mustFind(t, c, "bob")
	e.wantQueries(t, sent+2, "bob, expired, after the outage cleared")
}

// One Conditions call reports every identity condition raised: with the
// secure channel failed at both controllers and the directory's queries
// failing, both machine_account_unhealthy and directory_unreachable.
func TestServerConditionsReportsBoth(t *testing.T) {
	prof.Watch(t, prof.Options{})
	clock := sim.NewTestClock(mt0)
	d := identitytest.NewDomainController("CORP", machineRealm, "DC=corp,DC=example", dc1, dc2)
	d.StageComputer(t, identitytest.StagedComputer{Name: "NAS", Password: []byte(pw2), EncTypes: 0x18})
	sched, err := sim.NewSchedule(1, clock, sim.Rule{Op: "dial", Key: "*", Action: sim.FaultAction{Err: errDown}})
	if err != nil {
		t.Fatal(err)
	}
	dir := identitytest.NewDirectory("DC=corp,DC=example")
	// No secret provider: the bind credential is never resolved, so every
	// directory query fails before it is sent.
	s, err := identity.New(identity.Deps{Store: newStore(t), Clock: clock,
		DomainController: identitytest.NewFaultyDomainController(d, sched), Directory: passThrough(t, dir).Dial})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Stop(context.Background()) })
	st := joinedState(2, ver(2, 2, pw2, false))
	st.Directory = dirState(dir).Directory
	st.Directory.Realm = machineRealm
	if err := s.Apply(st); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if cs := s.Conditions(nil); len(cs) != 0 {
		t.Fatalf("conditions %v before any channel or query, want none", cs)
	}
	(&chanFixture{s: s}).noSignIn(t)
	if _, _, err := s.DirectoryLookup(context.Background(), directory.ByUserName, []byte("alice")); !errors.Is(err, vfserr.ErrUnavailable) {
		t.Fatalf("lookup alice = %v, want ErrUnavailable", err)
	}
	got := map[string]bool{}
	for _, c := range s.Conditions(nil) {
		got[c.Name] = true
	}
	for _, name := range []string{"machine_account_unhealthy", "directory_unreachable"} {
		if !got[name] {
			t.Errorf("conditions %v: %s missing, want it reported", got, name)
		}
	}
	if len(got) != 2 {
		t.Errorf("conditions %v, want exactly machine_account_unhealthy and directory_unreachable", got)
	}
}
