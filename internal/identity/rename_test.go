package identity_test

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"

	"github.com/cubbit/dittofs/internal/identity/directory"
	"github.com/cubbit/dittofs/pkg/auth/sid"
	pid "github.com/cubbit/dittofs/pkg/identity"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	merrs "github.com/cubbit/dittofs/pkg/metadata/errors"
	"github.com/cubbit/dittofs/pkg/sim"
)

// pastTTL is a step of the injected clock past a positive directory answer's
// 5-minute TTL and well inside the default 15-minute stale grace.
const pastTTL = 5*time.Minute + time.Second

// entryIDSearches is the filter of every search for an ad entry by its ID.
const entryIDSearches = "(objectGUID=*"

func qualified(name string) pid.Spelling {
	return pid.Spelling{Scheme: pid.SchemeQualified, Text: `KORP\` + name}
}

// rename gives the entry dn the account name name in the directory.
func (e *dirPrincipals) rename(dn, name string) {
	e.d.Modify(dn, map[string][][]byte{"sAMAccountName": attr(name)})
}

// holder is the principal of kind holding name in the store, the zero one
// for none.
func (e *dirPrincipals) holder(t *testing.T, kind entity.PrincipalKind, name string) entity.Principal {
	t.Helper()
	p, err := e.st.Principals().ByName(context.Background(), kind, name)
	if merrs.IsNotFoundError(err) {
		return entity.Principal{}
	}
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// nameOf is p's stored name.
func (e *dirPrincipals) nameOf(t *testing.T, p entity.Principal) string {
	t.Helper()
	if p.Kind == entity.PrincipalGroup {
		g, err := e.st.Records().Group(context.Background(), p)
		if err != nil {
			t.Fatal(err)
		}
		return g.Name
	}
	u, err := e.st.Principals().User(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	return u.Name
}

// RFC 18 §10's "name swap" row: alice and bob swap names in the directory,
// and once the clock is past the TTL, Find of alice answers the principal
// minted for the entry first called bob, and of bob the other, with one
// principals generation raise.
func TestRefreshNameSwap(t *testing.T) {
	e := newAD(t)
	dnA, dnB := e.d.AddUser(t, "alice", nil), e.d.AddUser(t, "bob", nil)
	pA, pB := e.mustFind(t, qualified("alice"), true), e.mustFind(t, qualified("bob"), true)
	e.rename(dnA, "bob")
	e.rename(dnB, "alice")
	e.clock.Advance(pastTTL)
	before := e.pgen(t)

	if got := e.mustFind(t, qualified("alice"), true); got != pB {
		t.Errorf(`Find(KORP\alice) after the swap = %v, want %v, minted as bob`, got, pB)
	}
	if got := e.mustFind(t, qualified("bob"), true); got != pA {
		t.Errorf(`Find(KORP\bob) after the swap = %v, want %v, minted as alice`, got, pA)
	}
	if got := e.pgen(t); got != before+1 {
		t.Errorf("PGEN across the swap %d, want %d: one Rename", got, before+1)
	}
	if got := e.holder(t, entity.PrincipalUser, "alice"); got != pB {
		t.Errorf("the store's alice = %v, want %v", got, pB)
	}
	if got := e.counter(t, "identity_renames_unnamed_total", "kind", "user"); got != 0 {
		t.Errorf("identity_renames_unnamed_total{kind=user} %v, want 0", got)
	}
}

// A holder whose entry now answers a name a third principal holds gives up
// the name the renaming record takes and is left unnamed, counted: one hop,
// never further.
func TestRefreshHolderNewNameHeld(t *testing.T) {
	e := newAD(t)
	dnA, dnB := e.d.AddUser(t, "alice", nil), e.d.AddUser(t, "bob", nil)
	e.d.AddUser(t, "carol", nil)
	pA, pB, pC := e.mustFind(t, qualified("alice"), true), e.mustFind(t, qualified("bob"), true), e.mustFind(t, qualified("carol"), true)
	e.rename(dnA, "bob")
	e.rename(dnB, "carol")
	e.clock.Advance(pastTTL)
	if got := e.mustFind(t, qualified("bob"), true); got != pA {
		t.Fatalf(`Find(KORP\bob) = %v, want %v, minted as alice`, got, pA)
	}
	for name, want := range map[string]entity.Principal{"bob": pA, "carol": pC} {
		if got := e.holder(t, entity.PrincipalUser, name); got != want {
			t.Errorf("%s held by %v, want %v", name, got, want)
		}
	}
	if n := e.nameOf(t, pB); n != "" {
		t.Errorf("the holder whose new name carol holds: name %q, want none", n)
	}
	if n := e.counter(t, "identity_renames_unnamed_total", "kind", "user"); n != 1 {
		t.Errorf("identity_renames_unnamed_total{kind=user} %v, want 1", n)
	}
}

// When the directory cannot answer, fresh, that the holder of a name no
// longer answers it, the holder keeps its name and the renaming record is
// left unnamed, counted: the query about bob's entry failing, timing out,
// or answerable only from an entry past its TTL.
func TestRefreshHolderUnanswered(t *testing.T) {
	for _, c := range []string{"fails", "times out", "stale"} {
		t.Run(c, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				st, inst := dirStore(t)
				e := newDirPrincipals(t, st, inst, directory.SchemaAD)
				dnA, dnB := e.d.AddUser(t, "alice", nil), e.d.AddUser(t, "bob", nil)
				pA, pB := e.mustFind(t, qualified("alice"), true), e.mustFind(t, qualified("bob"), true)
				if c == "stale" {
					// bob's entry is answered, and cached, under another name
					// before the TTL runs out, so only a stale answer could say
					// he gave bob up.
					e.rename(dnB, "robert")
					b := e.entry(t, directory.ByUserName, "robert")
					if _, _, err := e.s.DirectoryLookup(context.Background(), directory.ByEntryID, b.EntryID); err != nil {
						t.Fatal(err)
					}
				} else {
					e.rename(dnB, "alice")
				}
				e.rename(dnA, "bob")
				e.clock.Advance(pastTTL)
				action := sim.FaultAction{Err: errors.New("injected directory fault")}
				if c == "times out" {
					action = sim.FaultAction{Delay: time.Minute}
				}
				e.fail(t, sim.Rule{Op: "search", Key: entryIDSearches, Action: action})

				done := make(chan error, 1)
				var got entity.Principal
				go func() {
					var err error
					got, err = e.s.Find(context.Background(), qualified("bob"), pid.FindOpts{Mint: true})
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
					e.clock.Advance(time.Second)
				}
				if err != nil || got != pA {
					t.Fatalf(`Find(KORP\bob) = %v, %v; want %v, minted as alice`, got, err, pA)
				}
				if h := e.holder(t, entity.PrincipalUser, "bob"); h != pB {
					t.Errorf("bob held by %v, want %v: a holder lost its name on an unsettled query", h, pB)
				}
				if n := e.nameOf(t, pA); n != "" {
					t.Errorf("the renaming record's name %q, want none", n)
				}
				if h := e.holder(t, entity.PrincipalUser, "alice"); !h.IsZero() {
					t.Errorf("alice held by %v, want free: the record gave it up", h)
				}
				if n := e.counter(t, "identity_renames_unnamed_total", "kind", "user"); n != 1 {
					t.Errorf("identity_renames_unnamed_total{kind=user} %v, want 1", n)
				}
				e.fail(t)
				for range 70 {
					e.clock.Advance(time.Second)
					synctest.Wait()
				}
			})
		})
	}
}

// A renamed entry whose new name is free takes it and frees the old one; an
// answer matching the record writes nothing.
func TestRefreshRenameFree(t *testing.T) {
	e := newAD(t)
	dn := e.d.AddUser(t, "alice", nil)
	p := e.mustFind(t, qualified("alice"), true)
	e.clock.Advance(pastTTL)
	before := e.pgen(t)
	e.mustFind(t, qualified("alice"), true)
	if got := e.pgen(t); got != before {
		t.Errorf("PGEN after an unchanged answer %d, want %d", got, before)
	}
	e.rename(dn, "alicia")
	e.clock.Advance(pastTTL)
	if got := e.mustFind(t, qualified("alicia"), true); got != p {
		t.Fatalf(`Find(KORP\alicia) = %v, want %v`, got, p)
	}
	if got := e.holder(t, entity.PrincipalUser, "alicia"); got != p {
		t.Errorf("alicia held by %v, want %v", got, p)
	}
	if got := e.holder(t, entity.PrincipalUser, "alice"); !got.IsZero() {
		t.Errorf("alice held by %v after the rename, want free", got)
	}
	if got := e.pgen(t); got != before+1 {
		t.Errorf("PGEN after the rename %d, want %d", got, before+1)
	}
}

// A changed mail attribute is written once; an unchanged one is not.
func TestRefreshEmail(t *testing.T) {
	e := newAD(t)
	dn := e.d.AddUser(t, "alice", map[string][][]byte{"mail": attr("alice@corp.example")})
	p := e.mustFind(t, qualified("alice"), true)
	e.d.Modify(dn, map[string][][]byte{"mail": attr("alice@new.example")})
	e.clock.Advance(pastTTL)
	before := e.pgen(t)
	e.mustFind(t, qualified("alice"), true)
	u, err := e.st.Principals().User(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	if u.Email != "alice@new.example" {
		t.Errorf("e-mail after the change %q, want alice@new.example", u.Email)
	}
	if got := e.pgen(t); got != before+1 {
		t.Errorf("PGEN after the changed address %d, want %d: one write", got, before+1)
	}
	e.clock.Advance(pastTTL)
	e.mustFind(t, qualified("alice"), true)
	if got := e.pgen(t); got != before+1 {
		t.Errorf("PGEN after an unchanged address %d, want %d", got, before+1)
	}
}

// RFC 18 §10's "SID-less directory" row, a later answer: the entry given a
// SID leaves the record's allocated SID as it was.
func TestRefreshSIDlessKeepsSID(t *testing.T) {
	st, inst := dirStore(t)
	e := newDirPrincipals(t, st, inst, directory.SchemaRFC2307)
	dn := e.d.AddUser(t, "erin", map[string][][]byte{
		"objectClass": {[]byte("top"), []byte("posixAccount")}, "uid": attr("erin"),
		"entryUUID": attr("6f1c7a52-0d1e-4b55-9a57-1b9f0c1d2e3f"), "objectSid": nil, "uidNumber": attr("4000"),
	})
	p := e.mustFind(t, qualified("erin"), true)
	was, err := e.spell(t, p, pid.SchemeSID)
	if err != nil {
		t.Fatal(err)
	}
	var b bytes.Buffer
	sid.EncodeSID(&b, sid.ParseSIDMust("S-1-5-21-7-8-9-1200"))
	e.d.Modify(dn, map[string][][]byte{"objectSid": {b.Bytes()}, "uid": attr("erin2")})
	e.clock.Advance(pastTTL)
	if got := e.mustFind(t, qualified("erin2"), true); got != p {
		t.Fatalf(`Find(KORP\erin2) = %v, want %v`, got, p)
	}
	now, err := e.spell(t, p, pid.SchemeSID)
	if err != nil || !now.SID.Equal(&was.SID) {
		t.Errorf("erin's SID after the entry got one: %s, %v; want the allocated %s", sid.FormatSID(&now.SID), err, sid.FormatSID(&was.SID))
	}
}

// RFC 18 §10's "name held at mint" row, its rename: once the local finance
// is renamed, the next answer names the directory group Finance.
func TestRefreshNameHeldAtMint(t *testing.T) {
	e := newAD(t)
	ctx := context.Background()
	local := entity.NewGroup()
	if err := e.st.Minter().Create(ctx, entity.MintBatch{Groups: []entity.MintGroup{{
		Group: entity.Group{Principal: local, Name: "finance"},
		Alloc: entity.Alloc{GID: true, RID: true, Range: entity.DefaultIDRange},
	}}}); err != nil {
		t.Fatal(err)
	}
	alice := e.d.AddUser(t, "alice", nil)
	e.d.AddGroup(t, "Finance", map[string][][]byte{"member": attr(alice)})
	e.mustFind(t, qualified("alice"), true)
	g := e.mustFind(t, sidOf(t, e.entry(t, directory.ByGroupName, "Finance")), false)

	lg, err := e.st.Records().Group(ctx, local)
	if err != nil {
		t.Fatal(err)
	}
	lg.Name = "finance-local"
	if _, err := e.st.ControlPlane().PutGroup(ctx, lg, lg.Generation, entity.Alloc{}); err != nil {
		t.Fatal(err)
	}
	// A record is refreshed only from an answer queried for it, so the
	// cached one is let expire first.
	e.clock.Advance(5 * time.Minute)
	if got := e.mustFind(t, qualified("Finance"), true); got != g {
		t.Fatalf(`Find(KORP\Finance) = %v, want the directory group %v`, got, g)
	}
	sp, err := e.spell(t, g, pid.SchemeQualified)
	if err != nil || sp.Text != `KORP\Finance` {
		t.Errorf(`Spell(Finance, qualified) = %q, %v; want KORP\Finance`, sp.Text, err)
	}
}

// groupsOf mints a user named user in groups, each a new directory group
// holding only that user, and returns the groups' DNs and principals.
func (e *dirPrincipals) groupsOf(t *testing.T, user string, groups ...string) ([]string, []entity.Principal) {
	t.Helper()
	dn := e.d.AddUser(t, user, nil)
	var dns []string
	for _, g := range groups {
		dns = append(dns, e.d.AddGroup(t, g, map[string][][]byte{"member": attr(dn)}))
	}
	e.mustFind(t, qualified(user), true)
	var ps []entity.Principal
	for _, g := range groups {
		ps = append(ps, e.mustFind(t, sidOf(t, e.entry(t, directory.ByGroupName, g)), false))
	}
	e.first = dn
	return dns, ps
}

// addMember adds a new directory user named user to the groups dns, which
// groupsOf made.
func (e *dirPrincipals) addMember(t *testing.T, user string, dns ...string) {
	t.Helper()
	dn := e.d.AddUser(t, user, nil)
	for _, g := range dns {
		e.d.Modify(g, map[string][][]byte{"member": {[]byte(e.first), []byte(dn)}})
	}
}

// A Create taking carol between a refresh's reads and its Rename makes the
// list be planned again: it commits, the swap in it done and carol's take
// left unnamed and counted.
func TestRefreshConcurrentCreate(t *testing.T) {
	e := newAD(t)
	ctx := context.Background()
	dns, gs := e.groupsOf(t, "u1", "alice", "bob", "dave")
	e.rename(dns[0], "bob")
	e.rename(dns[1], "alice")
	e.rename(dns[2], "carol")
	e.addMember(t, "u2", dns...)
	taker := entity.NewGroup()
	fired := false
	e.s.OnRename(func() {
		if fired {
			return
		}
		fired = true
		if err := e.st.Minter().Create(ctx, entity.MintBatch{Groups: []entity.MintGroup{{
			Group: entity.Group{Principal: taker, Name: "carol"},
			Alloc: entity.Alloc{GID: true, RID: true, Range: entity.DefaultIDRange},
		}}}); err != nil {
			t.Error(err)
		}
	})
	e.mustFind(t, qualified("u2"), true)
	if !fired {
		t.Fatal("no refresh reached its Rename")
	}
	for name, want := range map[string]entity.Principal{"bob": gs[0], "alice": gs[1], "carol": taker} {
		if got := e.holder(t, entity.PrincipalGroup, name); got != want {
			t.Errorf("group %s held by %v, want %v", name, got, want)
		}
	}
	if n := e.nameOf(t, gs[2]); n != "" {
		t.Errorf("the group whose take carol was taken: name %q, want none", n)
	}
	if n := e.counter(t, "identity_renames_unnamed_total", "kind", "group"); n != 1 {
		t.Errorf("identity_renames_unnamed_total{kind=group} %v, want 1", n)
	}
}

// An answer renaming two groups to Finance and finance commits: the second
// take is listed unnamed and counted, never refused as invalid.
func TestRefreshFoldEqualTakes(t *testing.T) {
	e := newAD(t)
	dns, gs := e.groupsOf(t, "u1", "g1", "g2")
	e.rename(dns[0], "Finance")
	e.rename(dns[1], "finance")
	e.addMember(t, "u2", dns...)
	if _, err := e.find(t, qualified("u2"), true); err != nil {
		t.Fatalf(`Find(KORP\u2) = %v, want it minted`, err)
	}
	h := e.holder(t, entity.PrincipalGroup, "finance")
	if h != gs[0] && h != gs[1] {
		t.Fatalf("finance held by %v, want one of the two renamed groups", h)
	}
	other := gs[0]
	if h == gs[0] {
		other = gs[1]
	}
	if n := e.nameOf(t, other); n != "" {
		t.Errorf("the second take's group: name %q, want none", n)
	}
	if n := e.counter(t, "identity_renames_unnamed_total", "kind", "group"); n != 1 {
		t.Errorf("identity_renames_unnamed_total{kind=group} %v, want 1", n)
	}
}
