package identity_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
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

// A lookup of more SIDs than one batch, at the probe, sends one query, the
// probe: with the directory still down its other batches are refused unsent,
// and with it back they are asked once the probe answers, so the whole set is
// answered and the condition cleared.
func TestDirectoryOutageSIDLookupProbesOnce(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx := context.Background()
		e, c, sid := sidEnv(t)
		// Two batches, alice in the second; the others no entry holds.
		var sids [][]byte
		for i := range directory.SIDBatch {
			sids = append(sids, []byte(fmt.Sprintf("no such SID %d", i)))
		}
		sids = append(sids, sid["alice"])
		e.sidsDown(t)
		if _, err := c.LookupSIDs(ctx, [][]byte{sid["carol"]}, 0); !errors.Is(err, vfserr.ErrUnavailable) {
			t.Fatalf("LookupSIDs(carol) with the directory down = %v, want ErrUnavailable", err)
		}
		wantCondition(t, c, "unhealthy", "carol refused")

		e.clock.Advance(10 * time.Second)
		sent := e.queries(t)
		if got, err := c.LookupSIDs(ctx, sids, 0); !errors.Is(err, vfserr.ErrUnavailable) || got != nil {
			t.Fatalf("LookupSIDs(65) at the probe with the directory down = %d answers, %v; want none, ErrUnavailable", len(got), err)
		}
		synctest.Wait()
		e.wantQueries(t, sent+1, "a lookup of two batches at the probe, the directory down")

		e.fail(t)
		e.clock.Advance(10 * time.Second)
		sent = e.queries(t)
		got, err := c.LookupSIDs(ctx, sids, 0)
		if err != nil || len(got) != 1 || string(got[0].SID) != string(sid["alice"]) {
			t.Fatalf("LookupSIDs(65) at the probe with the directory back = %d answers, %v; want alice's alone", len(got), err)
		}
		e.wantQueries(t, sent+2, "a lookup of two batches at the probe, the directory back")
		wantCondition(t, c, "", "after the probe answered")
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
	// The detail names the failure, which the sentinel cause does not.
	if cs := c.Conditions(nil); !strings.Contains(cs[0].Detail, "injected directory fault") {
		t.Fatalf("detail %q, want it to name the query's failure", cs[0].Detail)
	}
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
	// A changed record's new client starts with no outage.
	st.Directory.Address = "ldaps://dc2.corp.example"
	if err := s.Apply(st); err != nil {
		t.Fatalf("Apply with a changed directory: %v", err)
	}
	for _, c := range s.Conditions(nil) {
		if c.Name == "directory_unreachable" {
			t.Errorf("directory_unreachable %v after the record changed, want the new client to start without it", c)
		}
	}
}

// A lookup of an empty value is answered without the directory, so while the
// outage is raised and a probe is due it sends nothing and leaves the
// condition as it was.
func TestDirectoryOutageEmptyValueIsNoProbe(t *testing.T) {
	prof.Watch(t, prof.Options{})
	e := newDirEnv(t)
	e.d.AddUser(t, "alice", nil)
	c := e.cache(t)
	mustFind(t, c, "alice")
	e.down(t)
	e.clock.Advance(6 * time.Minute)
	mustFind(t, c, "alice") // its failed query raises the outage
	e.clock.Advance(11 * time.Second)
	sent := e.queries(t)
	if _, found, err := c.Lookup(context.Background(), directory.ByUserName, nil, 15*time.Minute, false); err != nil || found {
		t.Fatalf("lookup of an empty name = found %v, %v; want not found", found, err)
	}
	e.wantQueries(t, sent, "an empty name with the probe due")
	wantCondition(t, c, "degraded", "after an empty name")
}

// A query sent before the directory's last answer that fails after it raises
// nothing: carol's search is sent with bob's, alice's failure raises the
// outage, bob's answer clears it, and carol's failure, two seconds later,
// leaves it clear.
func TestDirectoryOutageEarlierFailureRaisesNothing(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newDirEnv(t)
		for _, n := range []string{"alice", "bob", "carol"} {
			e.d.AddUser(t, n, nil)
		}
		c := e.cache(t)
		e.fail(t,
			sim.Rule{Op: "search", Key: "*carol*", Action: sim.FaultAction{Delay: 4 * time.Second, Err: errors.New("injected late failure")}},
			sim.Rule{Op: "search", Key: "*bob*", Action: sim.FaultAction{Delay: 2 * time.Second}},
			sim.Rule{Op: "search", Key: "*alice*", Action: sim.FaultAction{Err: errors.New("injected directory fault")}})
		carol, bob := make(chan error, 1), make(chan error, 1)
		go func() {
			_, _, err := lookup(c, "carol", 15*time.Minute)
			carol <- err
		}()
		go func() {
			_, found, err := lookup(c, "bob", 15*time.Minute)
			if err == nil && !found {
				err = errors.New("not found")
			}
			bob <- err
		}()
		synctest.Wait()
		mustRefuse(t, c, "alice", 15*time.Minute)
		wantCondition(t, c, "unhealthy", "alice refused")
		e.clock.Advance(2 * time.Second)
		if err := <-bob; err != nil {
			t.Fatalf("lookup bob = %v, want found", err)
		}
		wantCondition(t, c, "", "bob answered")
		e.clock.Advance(2 * time.Second)
		if err := <-carol; !errors.Is(err, vfserr.ErrUnavailable) {
			t.Fatalf("lookup carol = %v, want ErrUnavailable", err)
		}
		wantCondition(t, c, "", "carol's query, sent before bob's answer, failed after it")
	})
}

// With the outage raised and no lookup arriving, the cache's own loop probes
// the directory dirQuietProbe after the last probe, and the condition clears
// once the directory is back.
func TestDirectoryOutageQuietNodeClears(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newDirEnv(t)
		e.d.AddUser(t, "alice", nil)
		c := e.cache(t)
		mustFind(t, c, "alice")
		e.down(t)
		e.clock.Advance(6 * time.Minute)
		mustFind(t, c, "alice") // its failed query raises the outage
		synctest.Wait()
		sent := e.queries(t)
		e.fail(t)
		e.clock.Advance(19 * time.Second)
		synctest.Wait()
		wantCondition(t, c, "degraded", "19 seconds after the last probe")
		e.wantQueries(t, sent, "before the quiet probe is due")
		e.clock.Advance(time.Second)
		synctest.Wait()
		e.wantQueries(t, sent+1, "the quiet probe")
		wantCondition(t, c, "", "after the quiet probe answered")
	})
}
