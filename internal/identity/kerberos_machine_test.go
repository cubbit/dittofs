package identity_test

import (
	"context"
	"errors"
	"maps"
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/identity/identitytest"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/metadata/kv"
	"github.com/cubbit/dittofs/pkg/metadata/kv/kvtest"
	"github.com/cubbit/dittofs/pkg/vfs/vfserr"
)

// keys is every key the store holds, with its value.
func (r *resolving) keys(t *testing.T) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := r.reads.KV.View(context.Background(), func(rd kv.Reader) error {
		for e, err := range rd.Scan(nil, nil) {
			if err != nil {
				return err
			}
			out[string(e.Key)] = string(e.Value)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// sameKeys reports whether a and b hold the same keys, whatever their values.
func sameKeys(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k := range a {
		if _, ok := b[k]; !ok {
			return false
		}
	}
	return true
}

// machineRow is the krb spelling of name on the record holding it, and the
// record.
func (k *kerberos) machineRow(t *testing.T, name string) (entity.User, entity.ProtocolID) {
	t.Helper()
	p := k.holder(t, krbID(name))
	u, err := k.st.Principals().User(context.Background(), p)
	if err != nil {
		t.Fatalf("%s's record: %v", name, err)
	}
	for _, id := range u.IDs {
		if id.Scheme == entity.SchemeKerberos && id.Value == name {
			return u, id
		}
	}
	t.Fatalf("%s's record %+v holds no krb spelling of the name", name, u)
	return u, entity.ProtocolID{}
}

// machines is identity_mints_total{kind="machine"}.
func (k *kerberos) machines(t *testing.T) float64 {
	t.Helper()
	return k.counter(t, "identity_mints_total", "kind", "machine")
}

// RFC 18 §2.4: host/build01.test.realm, nfs/build01 and BUILD01$ are each
// minted at their first sign-in, a machine record holding one krb spelling
// linked for no SID and no name, file spelling or credential, counted in
// identity_mints_total{kind="machine"}; each one's second sign-in binds the
// same principal and writes nothing. Catches a machine name refused as an
// unknown user, a record minted with more than its name, and a write on a
// sign-in that mints nothing.
func TestKerberosMachineMintedOnFirstSignIn(t *testing.T) {
	k := kerberosServer(t)
	for _, c := range []string{"host/build01.test.realm", "nfs/build01", "BUILD01$"} {
		name := c + "@TEST.REALM"
		res, oc, err := k.srv.SignInOutcome(context.Background(), k.as(c))
		if err != nil || oc != "ok" {
			t.Fatalf("%s's first sign-in = %v, outcome %q; want ok", name, err, oc)
		}
		p := res.Identity.Evidence.Principal
		u, _ := k.machineRow(t, name)
		want := entity.ProtocolIDs{krbID(name)}
		if p.Kind != entity.PrincipalMachine || u.Principal != p || u.Name != "" || len(u.IDs) != 1 || u.IDs[0] != want[0] ||
			u.Password != (entity.SecretRef{}) || u.NTHash != (entity.SecretRef{}) || !u.Primary.IsZero() {
			t.Errorf("%s: bound %v, record %+v; want a machine record holding only %v", name, p, u, want)
		}
		before := k.keys(t)
		k.signInAs(t, name+", again", k.as(c), p)
		if after := k.keys(t); !maps.Equal(before, after) {
			t.Errorf("%s's second sign-in wrote to the store", name)
		}
	}
	if got := k.machines(t); got != 3 {
		t.Errorf("identity_mints_total{kind=machine} = %v, want 3", got)
	}
}

// RFC 18 §10's "PAC" clause: a machine minted from a ticket with a PAC is
// linked for its PAC's user SID in the mint's one batch, raising the
// principals generation once, and signs in again, bound. Catches a machine
// mint that loses its SID, left to a second write.
func TestKerberosMachineMintedWithAPAC(t *testing.T) {
	k := kerberosServer(t)
	const c, name = "host/build03.test.realm", "host/build03.test.realm@TEST.REALM"
	gen := k.pgen(t)
	res, oc, err := k.srv.SignInOutcome(context.Background(), k.pacAs(c, rid(c, 1107)))
	if err != nil || oc != "ok" {
		t.Fatalf("first sign-in = %v, outcome %q; want ok", err, oc)
	}
	if d := k.pgen(t) - gen; d != 1 {
		t.Errorf("the first sign-in raised the principals generation by %d, want 1: the mint's batch alone", d)
	}
	if _, row := k.machineRow(t, name); row.Linked != sidID(pacDomain+"-1107").Value {
		t.Errorf("the minted row's Linked %x, want %s's", row.Linked, pacDomain+"-1107")
	}
	k.signInAs(t, "the second PAC-bearing sign-in", k.pacAs(c, rid(c, 1107)), res.Identity.Evidence.Principal)
}

// In a keytab realm other than the directory's, host/build02 minted from a
// PAC-less ticket is linked by its first ticket whose PAC verifies, carrying
// S-1-5-21-7-7-7-1105: bound to the same principal, the row's Linked that
// SID and no key added; a later PAC carrying …-1106 is refused and the link
// kept. Catches a machine locked out once it presents a PAC, and a link
// every PAC rewrites.
func TestKerberosMachineLinkedByItsFirstPAC(t *testing.T) {
	k := kerberosServer(t)
	const c, name = "host/build02", "host/build02@TEST.REALM"
	res, oc, err := k.srv.SignInOutcome(context.Background(), k.as(c))
	if err != nil || oc != "ok" {
		t.Fatalf("PAC-less first sign-in = %v, outcome %q; want ok", err, oc)
	}
	p := res.Identity.Evidence.Principal
	before := k.keys(t)
	k.signInAs(t, "the first PAC", k.pacAs(c, rid(c, 1105)), p)
	if _, row := k.machineRow(t, name); row.Linked != sidID(pacDomain+"-1105").Value {
		t.Errorf("Linked after the first PAC %x, want …-1105's", row.Linked)
	}
	if after := k.keys(t); !sameKeys(before, after) {
		t.Errorf("the link changed the store's keys: %d before, %d after", len(before), len(after))
	}
	res, oc, err = k.srv.SignInOutcome(context.Background(), k.pacAs(c, rid(c, 1106)))
	refused(t, "a PAC carrying …-1106", res, oc, err, vfserr.ErrLogonFailure, "logon_failure")
	if _, row := k.machineRow(t, name); row.Linked != sidID(pacDomain+"-1105").Value {
		t.Errorf("Linked after a PAC carrying …-1106 %x, want …-1105's still", row.Linked)
	}
}

// both holds two calls at a test hook until both have reached it, then lets
// them go together; a later call passes straight through.
type both struct{ arrived, release chan struct{} }

func newBoth() *both { return &both{arrived: make(chan struct{}, 2), release: make(chan struct{})} }

func (b *both) hook() {
	select {
	case b.arrived <- struct{}{}:
		<-b.release
	default:
	}
}

// wait returns once both calls are at the hook, then releases them.
func (b *both) wait(t *testing.T) {
	t.Helper()
	for range 2 {
		select {
		case <-b.arrived:
		case <-time.After(10 * time.Second):
			t.Fatal("two calls did not reach the hook")
		}
	}
	close(b.release)
}

// signInPair runs two sign-ins at once, both held at b's hook until both
// reach it, and returns their results.
func signInPair(t *testing.T, b *both, steps ...func() (entity.Principal, string, error)) (ps [2]entity.Principal, ocs [2]string, errs [2]error) {
	t.Helper()
	done := make(chan struct{})
	for i, f := range steps {
		go func() {
			ps[i], ocs[i], errs[i] = f()
			done <- struct{}{}
		}()
	}
	b.wait(t)
	<-done
	<-done
	return
}

// Two first PAC-bearing sign-ins of host/build02, minted PAC-less, carrying
// …-1105 and …-1106, held before the link's write and released together:
// one is bound and the row linked for its SID, the other refused. Catches a
// link written unconditionally, which the second would overwrite.
func TestKerberosMachineLinkRace(t *testing.T) {
	k := kerberosServer(t)
	const c, name = "host/build02", "host/build02@TEST.REALM"
	if _, oc, err := k.srv.SignInOutcome(context.Background(), k.as(c)); err != nil || oc != "ok" {
		t.Fatalf("PAC-less first sign-in = %v, outcome %q; want ok", err, oc)
	}
	b := newBoth()
	k.srv.OnMachineLink(b.hook)
	signIn := func(r uint32) func() (entity.Principal, string, error) {
		step := k.pacAs(c, rid(c, r))
		return func() (entity.Principal, string, error) {
			res, oc, err := k.srv.SignInOutcome(context.Background(), step)
			return res.Identity.Evidence.Principal, oc, err
		}
	}
	steps := []func() (entity.Principal, string, error){signIn(1105), signIn(1106)}
	_, ocs, errs := signInPair(t, b, steps...)
	won := -1
	for i, oc := range ocs {
		if oc == "ok" {
			won = i
		} else if !errors.Is(errs[i], vfserr.ErrLogonFailure) {
			t.Errorf("sign-in %d: %v, outcome %q; want ok or logon_failure", i, errs[i], oc)
		}
	}
	if won < 0 || ocs[0] == ocs[1] {
		t.Fatalf("outcomes %v, want one ok and one logon_failure", ocs)
	}
	if _, row := k.machineRow(t, name); row.Linked != sidID(pacDomain+[]string{"-1105", "-1106"}[won]).Value {
		t.Errorf("Linked %x, want the winner's SID", row.Linked)
	}
}

// Two first sign-ins of one machine, held before the mint's Create and
// released together, mint one record and are both bound to it. Catches a
// plan that does not look the row up again on its re-run.
func TestKerberosMachineMintRace(t *testing.T) {
	k := kerberosServer(t)
	const c = "host/build04"
	b := newBoth()
	k.srv.OnMachineMint(b.hook)
	signIn := func(cusec int) func() (entity.Principal, string, error) {
		step := k.step(identitytest.TicketOpts{Client: c, Cusec: cusec}, krbClient)
		return func() (entity.Principal, string, error) {
			res, oc, err := k.srv.SignInOutcome(context.Background(), step)
			return res.Identity.Evidence.Principal, oc, err
		}
	}
	ps, ocs, errs := signInPair(t, b, signIn(1), signIn(2))
	for i := range ocs {
		if errs[i] != nil || ocs[i] != "ok" {
			t.Fatalf("sign-in %d = %v, outcome %q; want ok", i, errs[i], ocs[i])
		}
	}
	if ps[0] != ps[1] || ps[0] != k.holder(t, krbID(c+"@TEST.REALM")) {
		t.Errorf("bound %v and %v, want both the krb row's holder", ps[0], ps[1])
	}
	if got := k.machines(t); got != 1 {
		t.Errorf("identity_mints_total{kind=machine} = %v, want 1", got)
	}
}

// Only a machine name is minted: a user name with no record is refused and
// writes nothing, and an existing user's sign-in writes nothing; a machine's
// mint failing in the store is ErrUnavailable and writes nothing. Catches a
// user minted on sight, a write on a successful sign-in, and a failed mint
// leaving part of its batch.
func TestKerberosMachineMintWritesNothingElse(t *testing.T) {
	k := kerberosServer(t)
	alice := entity.User{Principal: entity.NewUser(), Name: "alice", IDs: fileIDs("1500")}
	k.user(t, alice, 0)
	before := k.keys(t)
	res, oc, err := k.srv.SignInOutcome(context.Background(), k.as("bob"))
	refused(t, "unknown bob", res, oc, err, vfserr.ErrLogonFailure, "logon_failure")
	k.signInAs(t, "alice", k.as("alice"), alice.Principal)
	if after := k.keys(t); !maps.Equal(before, after) {
		t.Errorf("bob's refusal and alice's sign-in wrote to the store")
	}
	k.fail(t, kvtest.OpUpdate, "*")
	res, oc, err = k.srv.SignInOutcome(context.Background(), k.as("host/build05"))
	k.fault.SetFault(nil)
	refused(t, "a machine whose mint fails", res, oc, err, vfserr.ErrUnavailable, "unavailable")
	if after := k.keys(t); !maps.Equal(before, after) {
		t.Errorf("the failed mint wrote to the store")
	}
}
