package identity_test

import (
	"context"
	"errors"
	"reflect"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/cubbit/dittofs/internal/identity"
	pid "github.com/cubbit/dittofs/pkg/identity"
	"github.com/cubbit/dittofs/pkg/identity/identitytest"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/secrets"
	"github.com/cubbit/dittofs/pkg/sim"
	"github.com/cubbit/dittofs/pkg/vfs/vfserr"
)

// signedIn signs name in over Kerberos through realm with a PAC carrying the
// SID the directory holds for it, and returns the principal bound.
func (e *dirPrincipals) signedIn(t *testing.T, realm *identitytest.Realm, name string) entity.Principal {
	t.Helper()
	res, err := e.s.SignIn(context.Background(), e.ticket(realm, name, e.logonOf(t, name)))
	if err != nil {
		t.Fatalf("SignIn(%s@CORP.EXAMPLE) = %v", name, err)
	}
	return res.Identity.Evidence.Principal
}

// record is p's user record as the store holds it.
func (e *dirPrincipals) record(t *testing.T, p entity.Principal) entity.User {
	t.Helper()
	u, err := e.st.Principals().User(context.Background(), p)
	if err != nil {
		t.Fatalf("User(%v) = %v", p, err)
	}
	return u
}

// A local principal reached through its krb row signs in through the
// directory's realm and asks the directory nothing. Catches the Kerberos
// branch reading the enabled state of every principal, not only a directory
// one.
func TestKerberosSignInLocalPrincipalAsksNoDirectory(t *testing.T) {
	e := newAD(t)
	ctx := context.Background()
	const pacSID = "S-1-5-21-7-7-7-1300"
	lee := entity.User{Principal: entity.NewUser(), Name: "lee", IDs: append(fileIDs("1600"), linkedKrbID("lee@CORP.EXAMPLE", pacSID))}
	if _, err := e.st.ControlPlane().PutUser(ctx, lee, 0, entity.Alloc{}); err != nil {
		t.Fatal(err)
	}
	step := e.ticket(e.kerberos(t), "lee", rid("lee", 1300))
	sent := len(e.f.Queries())
	res, err := e.s.SignIn(ctx, step)
	if err != nil {
		t.Fatalf("SignIn(lee@CORP.EXAMPLE) = %v", err)
	}
	if got := res.Identity.Evidence.Principal; got != lee.Principal {
		t.Errorf("lee bound %v, want %v", got, lee.Principal)
	}
	if q := e.f.Queries()[sent:]; len(q) != 0 {
		t.Errorf("searches during a local principal's sign-in %v, want none", q)
	}
}

// RFC 18 §10's "disabled directory account" at sign-in: alice, signed in
// with a PAC, is refused once the directory disables her and the TTL has
// passed, ErrLogonFailure with outcome disabled at the logon sink; refused
// ErrUnavailable once the directory is down past the TTL and the grace; and
// refused with outcome disabled once her entry is deleted. Each runs with
// alice reached by §4.2's step 3, by name, and again by a krb row linked
// for her SID, which asks the directory nothing on its own. Catches a PAC,
// or a step that maps without the directory, keeping a disabled account in.
func TestKerberosSignInDisabledDirectoryAccount(t *testing.T) {
	grace := identity.DefaultSettings().DirectoryStaleGrace
	for _, tc := range []struct {
		name    string
		change  func(t *testing.T, e *dirPrincipals, dn string)
		after   time.Duration
		err     error
		outcome string
	}{
		{"disabled", func(_ *testing.T, e *dirPrincipals, dn string) {
			e.d.Modify(dn, map[string][][]byte{"userAccountControl": attr("514")})
		}, 5*time.Minute + time.Second, vfserr.ErrLogonFailure, "disabled"},
		{"directory down", func(t *testing.T, e *dirPrincipals, _ string) {
			e.fail(t, sim.Rule{Op: "search", Key: "*", Action: sim.FaultAction{Err: errors.New("injected directory fault")}})
		}, 5*time.Minute + grace + time.Second, vfserr.ErrUnavailable, "unavailable"},
		{"deleted", func(_ *testing.T, e *dirPrincipals, dn string) { e.d.Delete(dn) }, 5*time.Minute + time.Second, vfserr.ErrLogonFailure, "disabled"},
	} {
		for _, linked := range []bool{false, true} {
			name := tc.name + "/by name"
			if linked {
				name = tc.name + "/by a linked krb row"
			}
			t.Run(name, func(t *testing.T) {
				e := newAD(t)
				ctx := context.Background()
				realm := e.kerberos(t)
				dn := e.d.AddUser(t, "alice", nil)
				logon := e.logonOf(t, "alice")
				alice := e.signedIn(t, realm, "alice")
				if linked {
					u := e.record(t, alice)
					u.IDs = append(u.IDs, linkedKrbID("alice@CORP.EXAMPLE", logon.DomainSID+"-"+strconv.FormatUint(uint64(logon.UserRID), 10)))
					if _, err := e.st.ControlPlane().PutUser(ctx, u, u.Generation, entity.Alloc{}); err != nil {
						t.Fatal(err)
					}
				}
				tc.change(t, e, dn)
				e.clock.Advance(tc.after)
				n := e.logon.len()
				res, oc, err := e.s.SignInOutcome(ctx, e.ticket(realm, "alice", logon))
				refused(t, "alice's sign-in", res, oc, err, tc.err, tc.outcome)
				if evs := e.logon.since(n); len(evs) != 1 || evs[0].Outcome != tc.outcome {
					t.Errorf("logon events %+v, want one with outcome %q", evs, tc.outcome)
				}
			})
		}
	}
}

// RFC 18 §10's "PAC bound to the record", its reuse clause: bob minted at
// his first sign-in, deleted in the directory and recreated under his name
// with a new SID, signs in with the new SID's PAC: a new principal or
// refused ErrLogonFailure, never the old one. Catches a reused name bound to
// the old person through step 3, and a refusal of another class.
func TestKerberosSignInReusedDirectoryName(t *testing.T) {
	e := newAD(t)
	realm := e.kerberos(t)
	dn := e.d.AddUser(t, "bob", nil)
	old := e.signedIn(t, realm, "bob")
	e.d.Delete(dn)
	e.d.AddUser(t, "bob", nil)
	e.clock.Advance(5*time.Minute + time.Second) // past bob's cached answer
	res, err := e.s.SignIn(context.Background(), e.ticket(realm, "bob", e.logonOf(t, "bob")))
	switch got := res.Identity.Evidence.Principal; {
	case err != nil && !errors.Is(err, vfserr.ErrLogonFailure):
		t.Errorf("the new bob's sign-in = %v, want a new principal or ErrLogonFailure", err)
	case err != nil && got != (entity.Principal{}):
		t.Errorf("the new bob refused %v, yet bound %v", err, got)
	case err == nil && got == old:
		t.Errorf("the new bob bound the old bob's principal %v", old)
	}
}

// RFC 18 §10's "mint conflict": alice minted, bob's entry holding another SID
// and alice's uidNumber signs in with an allocated uid in place of hers, as
// an entry with 0 or 65534 does, bound to no principal of alice's, counted in
// identity_mints_remapped_total, while alice's record stays byte-equal with
// her uid. Two concurrent sign-ins of one unminted entry, each mint's commit
// held until both have planned theirs, bind one principal. Catches a held
// number refusing the mint or reaching bob, and a lost mint bound to whoever
// holds the row.
func TestKerberosSignInMintConflict(t *testing.T) {
	t.Run("a held uidNumber", func(t *testing.T) {
		e := newAD(t)
		realm := e.kerberos(t)
		e.d.AddUser(t, "alice", map[string][][]byte{"uidNumber": attr("3000")})
		alice := e.signedIn(t, realm, "alice")
		before := e.record(t, alice)
		e.d.AddUser(t, "bob", map[string][][]byte{"uidNumber": attr("3000")})
		res, err := e.s.SignIn(context.Background(), e.ticket(realm, "bob", e.logonOf(t, "bob")))
		bob := res.Identity.Evidence.Principal
		if err != nil {
			t.Fatalf("bob's sign-in with alice's uidNumber = %v, want a principal with an allocated uid", err)
		}
		if bob == alice {
			t.Fatalf("bob bound alice's principal %v", alice)
		}
		rng := entity.DefaultIDRange
		if got := e.num(t, bob, pid.SchemeUID); got < rng.Lo || got > rng.Hi {
			t.Errorf("bob's uid %d, want one allocated from [%d, %d], not alice's 3000", got, rng.Lo, rng.Hi)
		}
		if got := e.counter(t, "identity_mints_remapped_total", "kind", "user"); got != 1 {
			t.Errorf("identity_mints_remapped_total{kind=user} after bob's mint %v, want 1", got)
		}
		if after := e.record(t, alice); !reflect.DeepEqual(after, before) {
			t.Errorf("alice's record after bob's sign-in %+v, want %+v unchanged", after, before)
		}
		if got := e.num(t, alice, pid.SchemeUID); got != 3000 {
			t.Errorf("alice's uid %d, want 3000", got)
		}
	})
	t.Run("two concurrent mints", func(t *testing.T) {
		e := newAD(t)
		realm := e.kerberos(t)
		e.d.AddUser(t, "carol", nil)
		step := func() pid.SignInStep { return e.ticket(realm, "carol", e.logonOf(t, "carol")) }
		steps := []pid.SignInStep{step(), step()}
		var mu sync.Mutex
		held := 0
		arrived, release := make(chan struct{}, 2), make(chan struct{})
		releaseOnce := sync.OnceFunc(func() { close(release) })
		defer releaseOnce() // a backstop's Fatal leaves no mint parked
		e.s.OnCreate(func(entity.MintBatch) {
			mu.Lock()
			held++
			first := held <= 2
			mu.Unlock()
			if first {
				arrived <- struct{}{}
				<-release
			}
		})
		var wg sync.WaitGroup
		ps := make([]entity.Principal, 2)
		errs := make([]error, 2)
		for i := range steps {
			wg.Go(func() {
				res, err := e.s.SignIn(context.Background(), steps[i])
				ps[i], errs[i] = res.Identity.Evidence.Principal, err
			})
		}
		for range 2 {
			select {
			case <-arrived:
			case <-time.After(30 * time.Second): // a backstop: both sign-ins plan a mint
				t.Fatal("two mints did not both reach their commit")
			}
		}
		releaseOnce()
		wg.Wait()
		for i, err := range errs {
			if err != nil {
				t.Fatalf("sign-in %d of carol = %v", i, err)
			}
		}
		if ps[0] != ps[1] {
			t.Errorf("carol bound %v and %v, want one principal", ps[0], ps[1])
		}
	})
}

// keytab applies a keytab of a realm named name for cifs/nas.corp.example in
// place of the State's, and returns the realm.
func (e *dirPrincipals) keytab(t *testing.T, name string) *identitytest.Realm {
	t.Helper()
	realm := identitytest.NewRealm(name, "cifs/nas.corp.example")
	st := *e.s.Applied()
	st.Keytab = []secrets.Version{{Number: 1, Value: realm.Keytab()}}
	if err := e.s.Apply(st); err != nil {
		t.Fatal(err)
	}
	return realm
}

// A ticket that maps to no principal is labelled disabled only when its
// realm is the directory's and the directory answers that the entry its
// PAC's SID names is gone; the label is read without writing. alice minted,
// then her entry deleted or renamed: a ticket of another realm whose PAC
// carries her SID is refused logon_failure, sending no directory query and
// writing nothing, not even her new name; in the directory's realm, her
// renamed entry, which the label reads by its ID and finds enabled, is
// refused logon_failure writing nothing either; and her deleted entry's own
// query failing past the TTL plus the grace, the refusal stays logon_failure.
// Catches a foreign ticket reaching the directory, the label's read writing a
// record, and a failed read labelled disabled.
func TestKerberosSignInGoneLabel(t *testing.T) {
	grace := identity.DefaultSettings().DirectoryStaleGrace
	for _, tc := range []struct {
		name   string
		change func(e *dirPrincipals, dn string)
		after  time.Duration
		fault  string // the searches failed before the sign-in; none for ""
		realm  string // the ticket's realm; the directory's for ""
	}{
		{"another realm, entry deleted", func(e *dirPrincipals, dn string) { e.d.Delete(dn) }, 5*time.Minute + time.Second, "", "TRUST.EXAMPLE"},
		{"another realm, entry renamed", func(e *dirPrincipals, dn string) {
			e.d.Modify(dn, map[string][][]byte{"sAMAccountName": attr("alicia")})
		}, 5*time.Minute + time.Second, "", "TRUST.EXAMPLE"},
		{"the directory's realm, entry renamed", func(e *dirPrincipals, dn string) {
			e.d.Modify(dn, map[string][][]byte{"sAMAccountName": attr("alicia")})
		}, 5*time.Minute + time.Second, "", ""},
		{"the entry's query failing", func(e *dirPrincipals, dn string) { e.d.Delete(dn) }, 5*time.Minute + grace + time.Second, "(objectGUID=*", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newAD(t)
			ctx := context.Background()
			realm := e.kerberos(t)
			dn := e.d.AddUser(t, "alice", nil)
			logon := e.logonOf(t, "alice")
			e.signedIn(t, realm, "alice")
			tc.change(e, dn)
			if tc.realm != "" {
				realm = e.keytab(t, tc.realm)
			}
			if tc.fault != "" {
				e.fail(t, sim.Rule{Op: "search", Key: tc.fault, Action: sim.FaultAction{Err: errors.New("injected directory fault")}})
			}
			e.clock.Advance(tc.after)
			sent, pgen := len(e.f.Queries()), e.pgen(t)
			res, oc, err := e.s.SignInOutcome(ctx, e.ticket(realm, "alice", logon))
			refused(t, "alice's sign-in", res, oc, err, vfserr.ErrLogonFailure, "logon_failure")
			if q := e.f.Queries()[sent:]; tc.realm != "" && len(q) != 0 {
				t.Errorf("searches for a ticket of %s %v, want none", tc.realm, q)
			}
			if got := e.pgen(t); got != pgen {
				t.Errorf("PGEN %d after the refused sign-in, want %d unchanged", got, pgen)
			}
		})
	}
}
