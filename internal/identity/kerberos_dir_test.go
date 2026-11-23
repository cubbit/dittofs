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
// refused, never the old one. Catches a reused name bound to the old person
// through step 3.
func TestKerberosSignInReusedDirectoryName(t *testing.T) {
	e := newAD(t)
	realm := e.kerberos(t)
	dn := e.d.AddUser(t, "bob", nil)
	old := e.signedIn(t, realm, "bob")
	e.d.Delete(dn)
	e.d.AddUser(t, "bob", nil)
	e.clock.Advance(5*time.Minute + time.Second) // past bob's cached answer
	res, err := e.s.SignIn(context.Background(), e.ticket(realm, "bob", e.logonOf(t, "bob")))
	if err == nil && res.Identity.Evidence.Principal == old {
		t.Fatalf("the new bob bound the old bob's principal %v", old)
	}
}

// RFC 18 §10's "mint conflict": alice minted, bob's entry holding another SID
// and alice's uidNumber signs in: bob is bound to no principal of alice's,
// alice's record is byte-equal and keeps her uid. Two concurrent sign-ins of
// one unminted entry, each mint's commit held until both have planned
// theirs, bind one principal. Catches a lost mint bound to whoever holds the
// row.
func TestKerberosSignInMintConflict(t *testing.T) {
	t.Run("a held uidNumber", func(t *testing.T) {
		e := newAD(t)
		realm := e.kerberos(t)
		e.d.AddUser(t, "alice", map[string][][]byte{"uidNumber": attr("3000")})
		alice := e.signedIn(t, realm, "alice")
		before := e.record(t, alice)
		e.d.AddUser(t, "bob", map[string][][]byte{"uidNumber": attr("3000")})
		res, err := e.s.SignIn(context.Background(), e.ticket(realm, "bob", e.logonOf(t, "bob")))
		if err == nil {
			bob := res.Identity.Evidence.Principal
			if bob == alice {
				t.Fatalf("bob bound alice's principal %v", alice)
			}
			if got := e.num(t, bob, pid.SchemeUID); got == 3000 {
				t.Errorf("bob's uid %d, want any but alice's 3000", got)
			}
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
		close(release)
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
