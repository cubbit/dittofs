package identity_test

import (
	"context"

	"errors"
	"fmt"
	"github.com/google/uuid"
	"slices"
	"strconv"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/cubbit/dittofs/internal/identity"
	"github.com/cubbit/dittofs/internal/identity/directory"
	"github.com/cubbit/dittofs/internal/prof"
	pid "github.com/cubbit/dittofs/pkg/identity"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/metadata/kv/badgerkv"
	"github.com/cubbit/dittofs/pkg/metadata/kv/kvtest"
	"github.com/cubbit/dittofs/pkg/metadata/kvstore"
	"github.com/cubbit/dittofs/pkg/sim"
	"github.com/cubbit/dittofs/pkg/vfs/vfserr"
)

// resolveAs resolves p, proven by a secret, on pol.
func (e *dirPrincipals) resolveAs(p entity.Principal, pol entity.SharePolicy) (*pid.Caller, error) {
	return e.s.Resolve(context.Background(), pid.Identity{Proof: pid.ProofSecret, Evidence: pid.Evidence{Principal: p}}, pol, pid.ReadOnlyNotNarrowed)
}

// refusals is identity_resolution_refusals_total{reason}.
func (e *dirPrincipals) refusals(t *testing.T, reason string) float64 {
	t.Helper()
	return e.counter(t, "identity_resolution_refusals_total", "reason", reason)
}

// minted adds the directory user name, attrs set on it, and mints it,
// returning its DN and principal.
func (e *dirPrincipals) minted(t *testing.T, name string, attrs map[string][][]byte) (string, entity.Principal) {
	t.Helper()
	dn := e.d.AddUser(t, name, attrs)
	return dn, e.mustFind(t, pid.Spelling{Scheme: pid.SchemeQualified, Text: `KORP\` + name}, true)
}

// groupOf is the principal minted for the directory group name.
func (e *dirPrincipals) groupOf(t *testing.T, name string) entity.Principal {
	t.Helper()
	return e.mustFind(t, sidOf(t, e.entry(t, directory.ByGroupName, name)), false)
}

// fileShare is a share that squashes nobody.
func fileShare() entity.SharePolicy { return onShare(entity.SquashNone, entity.AnonymousPrincipal) }

// A local principal's resolution sends no directory query, and its caller
// keeps the evidence's Fresh, zero for a secret.
func TestResolveLocalPrincipalAsksNoDirectory(t *testing.T) {
	e := newAD(t)
	u := entity.User{Principal: entity.NewUser(), Name: "lee", IDs: fileIDs("1000")}
	if _, err := e.st.ControlPlane().PutUser(context.Background(), u, 0, entity.Alloc{}); err != nil {
		t.Fatal(err)
	}
	sent := len(e.f.Queries())
	c, err := e.resolveAs(u.Principal, fileShare())
	if err != nil {
		t.Fatalf("Resolve(local lee) = %v", err)
	}
	if got := len(e.f.Queries()); got != sent {
		t.Errorf("searches after a local resolution %d, want %d unchanged", got, sent)
	}
	if !c.Fresh.IsZero() {
		t.Errorf("local caller's Fresh = %v, want zero", c.Fresh)
	}
}

// A directory user's groups are the directory's, read through the cache: in
// eng, his caller holds it and is fresh for 5 minutes; taken out of eng, he
// keeps it while the answer is fresh and drops it at the first resolution
// past the TTL, with no generation raised.
func TestResolveDirectoryGroupsFollowTheDirectory(t *testing.T) {
	e := newAD(t)
	bobDN := e.d.AddUser(t, "bob", nil)
	engDN := e.d.AddGroup(t, "eng", map[string][][]byte{"member": attr(bobDN)})
	bob := e.mustFind(t, pid.Spelling{Scheme: pid.SchemeQualified, Text: `KORP\bob`}, true)
	eng := e.groupOf(t, "eng")
	t0 := e.clock.Now()
	c, err := e.resolveAs(bob, fileShare())
	if err != nil {
		t.Fatalf("Resolve(bob) = %v", err)
	}
	if !slices.Contains(c.Groups, eng) {
		t.Errorf("bob's groups %v, want eng %v among them", c.Groups, eng)
	}
	if want := t0.Add(5 * time.Minute); !c.Fresh.Equal(want) {
		t.Errorf("bob's Fresh = %v, want %v: 5 minutes after it was built", c.Fresh, want)
	}

	e.d.Modify(engDN, map[string][][]byte{"member": nil})
	before := e.pgen(t)
	e.clock.Advance(time.Minute)
	if c, err = e.resolveAs(bob, fileShare()); err != nil || !slices.Contains(c.Groups, eng) {
		t.Errorf("Resolve(bob) within the TTL = groups %v, %v; want eng still, from the fresh answer", c.Groups, err)
	}
	e.clock.Advance(4 * time.Minute)
	c, err = e.resolveAs(bob, fileShare())
	if err != nil {
		t.Fatalf("Resolve(bob) past the TTL = %v", err)
	}
	if slices.Contains(c.Groups, eng) {
		t.Errorf("bob's groups past the TTL %v, want eng dropped", c.Groups)
	}
	if got := e.pgen(t); got != before {
		t.Errorf("PGEN %d, want %d: the dropped group wrote", got, before)
	}
}

// An account the directory disables, by userAccountControl's disable bit or
// by accountExpires passing, is refused ErrAccess, counted disabled, at the
// first resolution past the TTL.
func TestResolveDirectoryAccountDisabled(t *testing.T) {
	e := newAD(t)
	bobDN, bob := e.minted(t, "bob", nil)
	carolDN, carol := e.minted(t, "carol", nil)
	for _, p := range []entity.Principal{bob, carol} {
		if _, err := e.resolveAs(p, fileShare()); err != nil {
			t.Fatalf("Resolve(%v) while enabled = %v", p, err)
		}
	}
	e.d.Modify(bobDN, map[string][][]byte{"userAccountControl": attr("514")})
	// FILETIME a minute on: 100 ns intervals since 1601.
	expires := (e.clock.Now().Add(time.Minute).Unix() + 11644473600) * 10_000_000
	e.d.Modify(carolDN, map[string][][]byte{"accountExpires": attr(strconv.FormatInt(expires, 10))})
	e.clock.Advance(5*time.Minute + time.Second)
	for name, p := range map[string]entity.Principal{"bob": bob, "carol": carol} {
		before := e.refusals(t, "disabled")
		if _, err := e.resolveAs(p, fileShare()); !errors.Is(err, vfserr.ErrAccess) {
			t.Errorf("Resolve(%s) disabled in the directory = %v, want ErrAccess", name, err)
		}
		if got := e.refusals(t, "disabled"); got != before+1 {
			t.Errorf("refusals{disabled} after %s = %v, want %v", name, got, before+1)
		}
	}
}

// An entry deleted from the directory reads as disabled, never as "no
// change": past the TTL its principal is refused ErrAccess, counted
// disabled, and again within 30 seconds with no query sent.
func TestResolveDirectoryEntryGone(t *testing.T) {
	e := newAD(t)
	bobDN, bob := e.minted(t, "bob", nil)
	if _, err := e.resolveAs(bob, fileShare()); err != nil {
		t.Fatalf("Resolve(bob) = %v", err)
	}
	e.d.Delete(bobDN)
	e.clock.Advance(5 * time.Minute)
	before := e.refusals(t, "disabled")
	if _, err := e.resolveAs(bob, fileShare()); !errors.Is(err, vfserr.ErrAccess) {
		t.Fatalf("Resolve(bob) with his entry gone = %v, want ErrAccess", err)
	}
	if got := e.refusals(t, "disabled"); got != before+1 {
		t.Errorf("refusals{disabled} = %v, want %v", got, before+1)
	}
	sent := len(e.f.Queries())
	e.clock.Advance(20 * time.Second)
	if _, err := e.resolveAs(bob, fileShare()); !errors.Is(err, vfserr.ErrAccess) {
		t.Errorf("Resolve(bob) again within 30 seconds = %v, want ErrAccess", err)
	}
	if got := len(e.f.Queries()); got != sent {
		t.Errorf("searches for the second refusal %d, want %d: the negative answer was not used", got, sent)
	}
}

// A cached caller's bound: built at t0 it is fresh to t0+5m; the directory
// failing, it is rebuilt from answers served stale, each rebuild's Fresh no
// later than t0 plus the TTL plus the grace, and past that it is refused
// ErrUnavailable, counted directory_unavailable, never served the share's
// everyone grant.
func TestResolveDirectoryCallerBound(t *testing.T) {
	e := newAD(t)
	bobDN := e.d.AddUser(t, "bob", nil)
	e.d.AddGroup(t, "eng", map[string][][]byte{"member": attr(bobDN)})
	bob := e.mustFind(t, pid.Spelling{Scheme: pid.SchemeQualified, Text: `KORP\bob`}, true)
	pol := grantedShare(t, e.st.ControlPlane(), entity.SquashNone, entity.AnonymousPrincipal, entity.Everyone, entity.AccessReadWrite)
	t0 := e.clock.Now()
	grace := identity.DefaultSettings().DirectoryStaleGrace
	if _, err := e.resolveAs(bob, pol); err != nil {
		t.Fatalf("Resolve(bob) = %v", err)
	}
	e.fail(t, sim.Rule{Op: "search", Key: "*", Action: sim.FaultAction{Err: errors.New("injected directory fault")}})
	for _, at := range []time.Duration{5 * time.Minute, 5*time.Minute + grace} {
		e.clock.Advance(t0.Add(at).Sub(e.clock.Now()))
		c, err := e.resolveAs(bob, pol)
		if err != nil {
			t.Fatalf("Resolve(bob) at t0+%v, the directory down = %v, want a caller from the stale answers", at, err)
		}
		// The earlier of 5 minutes on and the stale answers' TTL plus grace.
		want := e.clock.Now().Add(5 * time.Minute)
		if limit := t0.Add(5*time.Minute + grace); limit.Before(want) {
			want = limit
		}
		if !c.Fresh.Equal(want) {
			t.Errorf("Fresh at t0+%v = %v, want %v", at, c.Fresh, want)
		}
	}
	e.clock.Advance(time.Second)
	before := e.refusals(t, "directory_unavailable")
	c, err := e.resolveAs(bob, pol)
	if !errors.Is(err, vfserr.ErrUnavailable) || c != nil {
		t.Errorf("Resolve(bob) past the TTL plus grace = %v, %v; want no caller, ErrUnavailable", c, err)
	}
	if got := e.refusals(t, "directory_unavailable"); got != before+1 {
		t.Errorf("refusals{directory_unavailable} = %v, want %v", got, before+1)
	}
}

// A groups question that fails is ErrUnavailable, never a caller without
// the group; the next one answered holds it.
func TestResolveDirectoryGroupsFailWhole(t *testing.T) {
	e := newAD(t)
	bobDN := e.d.AddUser(t, "bob", nil)
	e.d.AddGroup(t, "eng", map[string][][]byte{"member": attr(bobDN)})
	bob := e.mustFind(t, pid.Spelling{Scheme: pid.SchemeQualified, Text: `KORP\bob`}, true)
	eng := e.groupOf(t, "eng")
	// Past the TTL plus the grace of the closure the mint cached, so none is
	// served stale.
	e.clock.Advance(21 * time.Minute)
	// The tokenGroups read is a base search under exactly this filter.
	e.fail(t, sim.Rule{Op: "search", Key: "(objectClass=user)", Count: 1, Action: sim.FaultAction{Err: errors.New("injected directory fault")}})
	before := e.refusals(t, "directory_unavailable")
	c, err := e.resolveAs(bob, fileShare())
	if !errors.Is(err, vfserr.ErrUnavailable) || c != nil {
		t.Fatalf("Resolve(bob), his groups failing = %v, %v; want no caller, ErrUnavailable", c, err)
	}
	if got := e.refusals(t, "directory_unavailable"); got != before+1 {
		t.Errorf("refusals{directory_unavailable} = %v, want %v", got, before+1)
	}
	e.clock.Advance(11 * time.Second) // the next lookup probes
	if c, err = e.resolveAs(bob, fileShare()); err != nil || !slices.Contains(c.Groups, eng) {
		t.Errorf("Resolve(bob) once answered = groups %v, %v; want eng", c, err)
	}
}

// A ticket's SID of a directory group no principal holds, one the user's
// directory closure does not list, is minted at resolution and held.
func TestResolveDirectoryMintsTheEvidenceSIDs(t *testing.T) {
	e := newAD(t)
	_, bob := e.minted(t, "bob", nil)
	e.d.AddGroup(t, "audit", nil)
	audit := sidOf(t, e.entry(t, directory.ByGroupName, "audit"))
	if _, err := e.find(t, audit, false); !errors.Is(err, vfserr.ErrUnknownPrincipal) {
		t.Fatalf("Find(audit) before = %v, want ErrUnknownPrincipal", err)
	}
	id := pid.Identity{Proof: pid.ProofTicket, Evidence: pid.Evidence{Principal: bob, SIDs: []pid.SID{audit.SID}}}
	c, err := e.s.Resolve(context.Background(), id, fileShare(), pid.ReadOnlyNotNarrowed)
	if err != nil {
		t.Fatalf("Resolve(bob, audit's SID) = %v", err)
	}
	p, err := e.find(t, audit, false)
	if err != nil {
		t.Fatalf("Find(audit) after the resolution = %v, want the principal it minted", err)
	}
	if !slices.Contains(c.Groups, p) {
		t.Errorf("bob's groups %v, want audit %v among them", c.Groups, p)
	}
}

// RFC 18 §10's mint burst: 50 first resolutions of distinct users each in
// the same 300 directory groups, run at once, all succeed, each group minted
// once and the generation raised at most once per resolution; then a second
// wave whose group sets overlap pairwise without being equal all succeed.
//
// It runs RFC 18 §10's own numbers; TestResolveDirectoryMintReRuns pins the
// re-runs deterministically, so a short run skips this one.
func TestResolveDirectoryMintBurst(t *testing.T) {
	if testing.Short() {
		t.Skip("50 resolutions over 300 groups")
	}
	e := newAD(t)
	const users, groups = 50, 300
	dns := make([][]byte, users)
	ps := make([]entity.Principal, users)
	for i := range users {
		var dn string
		dn, ps[i] = e.minted(t, fmt.Sprintf("u%02d", i), nil)
		dns[i] = []byte(dn)
	}
	burst := func(wave string, want float64) {
		t.Helper()
		minted, pgen := e.counter(t, "identity_mints_total", "kind", "group"), e.pgen(t)
		var wg sync.WaitGroup
		errs := make([]error, users)
		for i := range users {
			wg.Go(func() { _, errs[i] = e.resolveAs(ps[i], fileShare()) })
		}
		wg.Wait()
		for i, err := range errs {
			if err != nil {
				t.Errorf("%s: Resolve(u%02d) = %v", wave, i, err)
			}
		}
		if got := e.counter(t, "identity_mints_total", "kind", "group") - minted; got != want {
			t.Errorf("%s: groups minted %v, want %v: each once", wave, got, want)
		}
		if got := e.pgen(t) - pgen; got < 1 || got > users {
			t.Errorf("%s: PGEN raised %d, want 1 to %d, once per minting resolution", wave, got, users)
		}
	}
	for i := range groups {
		e.d.AddGroup(t, fmt.Sprintf("g%03d", i), map[string][][]byte{"member": dns})
	}
	e.clock.Advance(5 * time.Minute) // past the closures the users' mints cached
	burst("first wave", groups)
	// User i is in h(i) and h(i+1): every two neighbours share one group.
	for i := range users {
		e.d.AddGroup(t, fmt.Sprintf("h%02d", i), map[string][][]byte{"member": {dns[i], dns[(i+users-1)%users]}})
	}
	e.clock.Advance(5 * time.Minute)
	burst("second wave", users)
}

// A cold resolution of a user in 300 groups, every search delayed 4
// seconds, fails ErrUnavailable at 20 seconds on the injected clock, counted
// directory_unavailable.
func TestResolveDirectoryStopsAtTwentySeconds(t *testing.T) {
	prof.Watch(t, prof.Options{})
	synctest.Test(t, func(t *testing.T) {
		st, inst := dirStore(t)
		e := newDirPrincipals(t, st, inst, directory.SchemaAD)
		_, alice := e.minted(t, "alice", nil)
		a := e.entry(t, directory.ByUserName, "alice")
		for i := range 300 {
			e.d.AddGroup(t, fmt.Sprintf("g%03d", i), map[string][][]byte{"member": attr(a.DN)})
		}
		e.clock.Advance(21 * time.Minute) // past the grace of the closure the mint cached
		e.fail(t, sim.Rule{Op: "search", Key: "*", Action: sim.FaultAction{Delay: 4 * time.Second}})
		before := e.refusals(t, "directory_unavailable")
		start := e.clock.Now()
		done := make(chan error, 1)
		go func() {
			_, err := e.resolveAs(alice, fileShare())
			done <- err
		}()
		var err error
	wait:
		for {
			synctest.Wait()
			select {
			case err = <-done:
				break wait
			default:
			}
			if e.clock.Now().Sub(start) > time.Minute {
				t.Fatal("Resolve still running a minute in")
			}
			e.clock.Advance(time.Second)
		}
		if took := e.clock.Now().Sub(start); took != 20*time.Second {
			t.Errorf("Resolve returned at %v, want 20s", took)
		}
		if !errors.Is(err, vfserr.ErrUnavailable) {
			t.Errorf("Resolve = %v, want ErrUnavailable", err)
		}
		if got := e.refusals(t, "directory_unavailable"); got != before+1 {
			t.Errorf("refusals{directory_unavailable} = %v, want %v", got, before+1)
		}
		// The searches the bound left behind finish on the clock.
		for range 60 {
			e.clock.Advance(time.Second)
			synctest.Wait()
		}
	})
}

// A resolution whose mint loses the race twice still succeeds: bob's
// resolution plans eng and ops, and just before each of its Creates a
// competing Find mints one of them, so its first Create and its first re-run's
// both fail ErrExist and only the second re-run binds everything. Each group
// is minted once, and bob holds both.
func TestResolveDirectoryMintReRuns(t *testing.T) {
	e := newAD(t)
	bobDN, bob := e.minted(t, "bob", nil)
	e.d.AddGroup(t, "eng", map[string][][]byte{"member": attr(bobDN)})
	e.d.AddGroup(t, "ops", map[string][][]byte{"member": attr(bobDN)})
	e.clock.Advance(5 * time.Minute) // past the closure bob's mint cached
	race := []pid.Spelling{sidOf(t, e.entry(t, directory.ByGroupName, "eng")), sidOf(t, e.entry(t, directory.ByGroupName, "ops"))}
	competing, creates := false, 0
	// The competing mint runs inside the hook, to its commit, on the
	// resolution's own goroutine; its own Create passes the hook untouched.
	e.s.OnCreate(func(entity.MintBatch) {
		if competing || creates >= len(race) {
			return
		}
		competing = true
		defer func() { competing = false }()
		e.mustFind(t, race[creates], true)
		creates++
	})
	before := e.counter(t, "identity_mints_total", "kind", "group")
	c, err := e.resolveAs(bob, fileShare())
	e.s.OnCreate(nil)
	if err != nil {
		t.Fatalf("Resolve(bob) after two lost Creates = %v, want a caller from the second re-run", err)
	}
	if creates != len(race) {
		t.Fatalf("competing mints %d, want %d: the resolution's Creates were not each overtaken", creates, len(race))
	}
	if got := e.counter(t, "identity_mints_total", "kind", "group") - before; got != 2 {
		t.Errorf("groups minted %v, want 2: each once", got)
	}
	for i, name := range []string{"eng", "ops"} {
		if p := e.mustFind(t, race[i], false); !slices.Contains(c.Groups, p) {
			t.Errorf("bob's groups %v, want %s %v", c.Groups, name, p)
		}
	}
}

// spelled is p's qualified name, "" for none.
func (e *dirPrincipals) spelled(t *testing.T, p entity.Principal) string {
	t.Helper()
	sp, err := e.spell(t, p, pid.SchemeQualified)
	if err != nil {
		return ""
	}
	return sp.Text
}

// A resolution never renames a record from an answer the cache held: bob's
// entry-ID answer, cached at his first resolution, still names him bob after
// the directory renamed him bobby and a Find by the new name renamed his
// record; his next resolution leaves the record bobby, writing nothing.
func TestResolveDirectoryNeverRenamesBack(t *testing.T) {
	e := newAD(t)
	bobDN, bob := e.minted(t, "bob", nil)
	if _, err := e.resolveAs(bob, fileShare()); err != nil {
		t.Fatalf("Resolve(bob) = %v", err)
	}
	e.d.Modify(bobDN, map[string][][]byte{"sAMAccountName": attr("bobby")})
	if got := e.mustFind(t, pid.Spelling{Scheme: pid.SchemeQualified, Text: `KORP\bobby`}, true); got != bob {
		t.Fatalf(`Find(KORP\bobby) = %v, want bob's %v`, got, bob)
	}
	if got := e.spelled(t, bob); got != `KORP\bobby` {
		t.Fatalf(`bob's name after Find(KORP\bobby) = %q, want KORP\bobby`, got)
	}
	before := e.pgen(t)
	if _, err := e.resolveAs(bob, fileShare()); err != nil {
		t.Fatalf("Resolve(bob) after the rename = %v", err)
	}
	if got := e.spelled(t, bob); got != `KORP\bobby` {
		t.Errorf(`bob's name after his next resolution = %q, want KORP\bobby: renamed back from the cached answer`, got)
	}
	if got := e.pgen(t); got != before {
		t.Errorf("PGEN %d after the resolution, want %d unchanged", got, before)
	}
}

// Nor a group of the closure: eng, renamed engineering in the directory and
// its record renamed by a Find of the new name, keeps it at bob's next
// resolution, which reads the closure the cache still holds.
func TestResolveDirectoryNeverRenamesAGroupBack(t *testing.T) {
	e := newAD(t)
	bobDN := e.d.AddUser(t, "bob", nil)
	engDN := e.d.AddGroup(t, "eng", map[string][][]byte{"member": attr(bobDN)})
	bob := e.mustFind(t, pid.Spelling{Scheme: pid.SchemeQualified, Text: `KORP\bob`}, true)
	eng := e.groupOf(t, "eng")
	if _, err := e.resolveAs(bob, fileShare()); err != nil {
		t.Fatalf("Resolve(bob) = %v", err)
	}
	e.d.Modify(engDN, map[string][][]byte{"sAMAccountName": attr("engineering")})
	if got := e.mustFind(t, pid.Spelling{Scheme: pid.SchemeQualified, Text: `KORP\engineering`}, true); got != eng {
		t.Fatalf(`Find(KORP\engineering) = %v, want eng's %v`, got, eng)
	}
	before := e.pgen(t)
	if _, err := e.resolveAs(bob, fileShare()); err != nil {
		t.Fatalf("Resolve(bob) after the rename = %v", err)
	}
	if got := e.spelled(t, eng); got != `KORP\engineering` {
		t.Errorf(`eng's name after bob's next resolution = %q, want KORP\engineering: renamed back from the cached closure`, got)
	}
	if got := e.pgen(t); got != before {
		t.Errorf("PGEN %d after the resolution, want %d unchanged", got, before)
	}
}

// dirFaultStore is dirStore behind the kv fault wrapper.
func dirFaultStore(t *testing.T) (*kvstore.Store, uuid.UUID, *kvtest.FaultKV) {
	t.Helper()
	ctx := context.Background()
	db, err := badgerkv.Open(badgerkv.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	inst := uuid.New()
	if err := kvstore.Init(ctx, db, inst); err != nil {
		t.Fatal(err)
	}
	f := kvtest.NewFaultKV(db)
	st, err := kvstore.Open(ctx, f)
	if err != nil {
		t.Fatal(err)
	}
	return st, inst, f
}

// A refresh the store cannot write never refuses the resolution: renamed
// bobby in the directory and every store write failing, bob still resolves
// with his group, the failure counted, his record left bob.
func TestResolveDirectoryRefreshFailureCarriesOn(t *testing.T) {
	prof.Watch(t, prof.Options{})
	st, inst, f := dirFaultStore(t)
	e := newDirPrincipals(t, st, inst, directory.SchemaAD)
	bobDN := e.d.AddUser(t, "bob", nil)
	e.d.AddGroup(t, "eng", map[string][][]byte{"member": attr(bobDN)})
	bob := e.mustFind(t, pid.Spelling{Scheme: pid.SchemeQualified, Text: `KORP\bob`}, true)
	eng := e.groupOf(t, "eng")
	e.d.Modify(bobDN, map[string][][]byte{"sAMAccountName": attr("bobby")})
	e.clock.Advance(5 * time.Minute)
	s, err := sim.NewSchedule(1, e.clock, sim.Rule{Op: kvtest.OpUpdate, Key: "*", Action: sim.FaultAction{Err: errors.New("injected store fault")}})
	if err != nil {
		t.Fatal(err)
	}
	f.SetFault(s)
	c, err := e.resolveAs(bob, fileShare())
	f.SetFault(nil)
	if err != nil {
		t.Fatalf("Resolve(bob) with his rename unwritable = %v, want a caller", err)
	}
	if !slices.Contains(c.Groups, eng) {
		t.Errorf("bob's groups %v, want eng %v", c.Groups, eng)
	}
	if got := e.counter(t, "identity_directory_refresh_failures_total", "kind", "user"); got != 1 {
		t.Errorf("identity_directory_refresh_failures_total{kind=user} = %v, want 1", got)
	}
	if got := e.spelled(t, bob); got != `KORP\bob` {
		t.Errorf("bob's name = %q, want KORP\\bob: the failed rename wrote", got)
	}
}

// A directory group the ticket marks deny-only is in the caller's deny list,
// never in its groups.
func TestResolveDirectoryDenyOnlyGroup(t *testing.T) {
	e := newAD(t)
	bobDN := e.d.AddUser(t, "bob", nil)
	e.d.AddGroup(t, "eng", map[string][][]byte{"member": attr(bobDN)})
	bob := e.mustFind(t, pid.Spelling{Scheme: pid.SchemeQualified, Text: `KORP\bob`}, true)
	engSID := sidOf(t, e.entry(t, directory.ByGroupName, "eng"))
	eng := e.mustFind(t, engSID, false)
	id := pid.Identity{Proof: pid.ProofTicket, Evidence: pid.Evidence{Principal: bob, DenyOnly: []pid.SID{engSID.SID}}}
	c, err := e.s.Resolve(context.Background(), id, fileShare(), pid.ReadOnlyNotNarrowed)
	if err != nil {
		t.Fatalf("Resolve(bob, eng deny-only) = %v", err)
	}
	if slices.Contains(c.Groups, eng) {
		t.Errorf("bob's groups %v hold eng %v, which the ticket marks deny-only", c.Groups, eng)
	}
	if !slices.Contains(c.DenyOnly, eng) {
		t.Errorf("bob's deny-only %v, want eng %v", c.DenyOnly, eng)
	}
}

// freshUntil is the earliest of 5 minutes after the build, the oldest
// answer's TTL plus grace, and the evidence's end when it has one.
func TestFreshUntil(t *testing.T) {
	now := time.Date(2026, 11, 19, 8, 0, 0, 0, time.UTC)
	grace := 15 * time.Minute
	for _, tc := range []struct {
		name            string
		oldest, expires time.Time
		want            time.Time
	}{
		{"fresh answer, no evidence end", now, time.Time{}, now.Add(5 * time.Minute)},
		{"answer 18 minutes old", now.Add(-18 * time.Minute), time.Time{}, now.Add(2 * time.Minute)},
		{"evidence ending first", now, now.Add(time.Minute), now.Add(time.Minute)},
		{"evidence ending later", now, now.Add(time.Hour), now.Add(5 * time.Minute)},
	} {
		if got := identity.FreshUntil(now, tc.oldest, tc.expires, grace); !got.Equal(tc.want) {
			t.Errorf("%s: freshUntil = %v, want %v", tc.name, got, tc.want)
		}
	}
}
