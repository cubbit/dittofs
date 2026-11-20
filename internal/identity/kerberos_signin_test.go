package identity_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/netip"
	"testing"
	"time"

	"github.com/jcmturner/gokrb5/v8/iana/etypeID"
	"github.com/jcmturner/gokrb5/v8/iana/flags"
	"github.com/jcmturner/gokrb5/v8/keytab"

	pid "github.com/cubbit/dittofs/pkg/identity"
	"github.com/cubbit/dittofs/pkg/identity/identitytest"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/metadata/kv/kvtest"
	"github.com/cubbit/dittofs/pkg/secrets"
	"github.com/cubbit/dittofs/pkg/vfs/vfserr"
)

const krbRealm = "TEST.REALM"

var krbClient = netip.MustParseAddr("192.0.2.9")

// kerberos is a signing fixture whose State holds the keytab of a test realm
// for cifs/ and nfs/nas.test.
type kerberos struct {
	*resolving
	realm *identitytest.Realm
}

func kerberosServer(t *testing.T) *kerberos {
	t.Helper()
	r, _ := signing(t)
	k := &kerberos{resolving: r, realm: identitytest.NewRealm(krbRealm, "cifs/nas.test", "nfs/nas.test")}
	k.applyKeytab(t, k.realm.Keytab())
	return k
}

// applyKeytab applies a State whose only keytab version is kt; none for nil.
func (k *kerberos) applyKeytab(t *testing.T, kt []byte) {
	t.Helper()
	st := state(1)
	st.Settings.NetBIOSName = "NAS01"
	if kt != nil {
		st.Keytab = []secrets.Version{{Number: 1, Value: kt}}
	}
	if err := k.srv.Apply(st); err != nil {
		t.Fatalf("Apply: %v", err)
	}
}

// step is an SMB step from client presenting a cifs/nas.test ticket for
// o.Client, issued now with o's other fields.
func (k *kerberos) step(o identitytest.TicketOpts, client netip.Addr) pid.SignInStep {
	o.Service = "cifs/nas.test"
	if o.Now.IsZero() {
		o.Now = k.clock.Now()
	}
	return pid.SignInStep{Kerberos: k.realm.Ticket(o), Service: pid.ServiceCIFS, Transport: pid.TransportSMB, Client: client}
}

// as is a step presenting a fresh ticket for client name c from krbClient.
func (k *kerberos) as(c string) pid.SignInStep {
	return k.step(identitytest.TicketOpts{Client: c}, krbClient)
}

// krbID is the krb spelling of name.
func krbID(name string) entity.ProtocolID {
	return entity.ProtocolID{Scheme: entity.SchemeKerberos, Value: name}
}

// kerberosSignIns is identity_authentications_total for Kerberos over
// protocol with outcome.
func (r *resolving) kerberosSignIns(t *testing.T, protocol, outcome string) float64 {
	t.Helper()
	return r.series(t, "identity_authentications_total", map[string]string{"protocol": protocol, "mechanism": "kerberos", "outcome": outcome})
}

// signInAs signs in with step and checks it bound want.
func (k *kerberos) signInAs(t *testing.T, what string, step pid.SignInStep, want entity.Principal) pid.SignInResult {
	t.Helper()
	res, oc, err := k.srv.SignInOutcome(context.Background(), step)
	if err != nil || oc != "ok" {
		t.Fatalf("%s: SignIn = %v, outcome %q; want ok", what, err, oc)
	}
	if got := res.Identity.Evidence.Principal; got != want {
		t.Errorf("%s: bound %v, want %v", what, got, want)
	}
	return res
}

// RFC 18 §4.2's steps in order, each case on its own store: step 1's krb row
// before step 2's local user, step 2 only for a local file user of the
// name, its realm as the ticket spells it, never for a name with a service
// part, and an unknown or disabled user refused. Catches step 2 before step
// 1, a realm folded, a directory principal or an API-only account reached by
// name, a service name matched as a user, and a disabled user bound.
func TestKerberosSignInMapsTheName(t *testing.T) {
	for _, tc := range []struct {
		name    string
		users   []entity.User
		opts    identitytest.TicketOpts
		bound   string // the user's name the sign-in binds; empty: refused
		outcome string
	}{
		{name: "a krb row binds its user before a local user of the name",
			users: []entity.User{
				{Name: "alice", IDs: fileIDs("1500")},
				{Name: "carol", IDs: append(fileIDs("1502"), krbID("alice@TEST.REALM"))},
			},
			opts: identitytest.TicketOpts{Client: "alice"}, bound: "carol", outcome: "ok"},
		{name: "a krb row naming the local backup",
			users: []entity.User{{Name: "backup", IDs: append(fileIDs("1510"), krbID("backup@TEST.REALM"))}},
			opts:  identitytest.TicketOpts{Client: "backup"}, bound: "backup", outcome: "ok"},
		{name: "step 2 binds the local backup",
			users: []entity.User{{Name: "backup", IDs: fileIDs("1510")}},
			opts:  identitytest.TicketOpts{Client: "backup"}, bound: "backup", outcome: "ok"},
		{name: "step 2 compares the user part as user names are",
			users: []entity.User{{Name: "alice", IDs: fileIDs("1500")}},
			opts:  identitytest.TicketOpts{Client: "ALICE"}, bound: "alice", outcome: "ok"},
		{name: "a realm in another case",
			users: []entity.User{{Name: "alice", IDs: fileIDs("1500")}},
			opts:  identitytest.TicketOpts{Client: "alice", ClientRealm: "test.realm"}, outcome: "logon_failure"},
		{name: "a local user holding a dir spelling",
			users: []entity.User{{Name: "alice", IDs: append(fileIDs("1500"), entity.ProtocolID{Scheme: entity.SchemeDir, Value: "entry-1"})}},
			opts:  identitytest.TicketOpts{Client: "alice"}, outcome: "logon_failure"},
		{name: "an API-only account",
			users: []entity.User{{Name: "alice"}},
			opts:  identitytest.TicketOpts{Client: "alice"}, outcome: "logon_failure"},
		{name: "a name with a service part",
			users: []entity.User{{Name: "cifs", IDs: fileIDs("1520")}, {Name: "cifs/alice", IDs: fileIDs("1521")}},
			opts:  identitytest.TicketOpts{Client: "cifs/alice"}, outcome: "logon_failure"},
		{name: "an unknown user",
			opts: identitytest.TicketOpts{Client: "bob"}, outcome: "logon_failure"},
		{name: "a disabled user",
			users: []entity.User{{Name: "alice", IDs: fileIDs("1500"), Disabled: true}},
			opts:  identitytest.TicketOpts{Client: "alice"}, outcome: "disabled"},
		{name: "a disabled user reached by its krb row",
			users: []entity.User{{Name: "carol", IDs: append(fileIDs("1502"), krbID("alice@TEST.REALM")), Disabled: true}},
			opts:  identitytest.TicketOpts{Client: "alice"}, outcome: "disabled"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			k := kerberosServer(t)
			byName := map[string]entity.Principal{}
			for _, u := range tc.users {
				u.Principal = entity.NewUser()
				k.user(t, u, 0)
				byName[u.Name] = u.Principal
			}
			res, oc, err := k.srv.SignInOutcome(context.Background(), k.step(tc.opts, krbClient))
			if tc.bound == "" {
				refused(t, tc.name, res, oc, err, vfserr.ErrLogonFailure, tc.outcome)
				return
			}
			if err != nil || oc != tc.outcome {
				t.Fatalf("SignIn = %v, outcome %q; want %q", err, oc, tc.outcome)
			}
			if got, want := res.Identity.Evidence.Principal, byName[tc.bound]; got != want {
				t.Errorf("bound %v, want %s's %v", got, tc.bound, want)
			}
		})
	}
}

// A user marked deleted has lost its name rows, so its sign-in fails as an
// unknown name's does, outcome logon_failure (RFC 18 §3.7). Catches a
// deleted user's krb row or name left to bind it.
func TestKerberosSignInRefusesADeletedUser(t *testing.T) {
	k := kerberosServer(t)
	alice := entity.User{Principal: entity.NewUser(), Name: "alice", IDs: append(fileIDs("1500"), krbID("alice@TEST.REALM"))}
	gen := k.user(t, alice, 0)
	if err := k.cp.DeleteUser(context.Background(), alice.Principal, gen); err != nil {
		t.Fatalf("DeleteUser: %v", err)
	}
	res, oc, err := k.srv.SignInOutcome(context.Background(), k.as("alice"))
	refused(t, "a deleted alice", res, oc, err, vfserr.ErrLogonFailure, "logon_failure")
}

// The evidence names the ticket's client exactly as spelled, binds the
// principal mapped, and expires at the ticket's end time; the keys are the
// ticket's. A re-authentication on a context past its Expires is admitted
// and its result holds the new ticket's end time. Catches a name rewritten
// before it reaches the evidence, an Expires taken from anywhere but the
// ticket, and an expired context refused.
func TestKerberosSignInEvidence(t *testing.T) {
	k := kerberosServer(t)
	alice := entity.User{Principal: entity.NewUser(), Name: "alice", IDs: fileIDs("1500")}
	k.user(t, alice, 0)
	session := identitytest.NewKey(etypeID.AES256_CTS_HMAC_SHA1_96)
	end := k.clock.Now().Add(3 * time.Hour)
	res := k.signInAs(t, "Alice@TEST.REALM", k.step(identitytest.TicketOpts{Client: "Alice", End: end, Session: session}, krbClient), alice.Principal)
	ev := res.Identity.Evidence
	if ev.Name != "Alice@TEST.REALM" || !ev.Expires.Equal(end) || res.Identity.Proof != pid.ProofTicket || res.Identity.Client != krbClient {
		t.Errorf("evidence %+v, proof %v, client %v; want Alice@TEST.REALM expiring %v, a ticket proof from %v",
			ev, res.Identity.Proof, res.Identity.Client, end, krbClient)
	}
	if !bytes.Equal(res.Keys.Session, session.KeyValue) || len(res.Keys.APRep) == 0 {
		t.Errorf("keys %x, AP-REP of %d bytes; want the ticket's session key and an AP-REP", res.Keys.Session, len(res.Keys.APRep))
	}

	k.clock.Advance(4 * time.Hour)
	end2 := k.clock.Now().Add(10 * time.Hour)
	step := k.step(identitytest.TicketOpts{Client: "Alice", End: end2}, krbClient)
	step.Context = &ev
	res = k.signInAs(t, "a re-authentication past Expires", step, alice.Principal)
	if got := res.Identity.Evidence.Expires; !got.Equal(end2) {
		t.Errorf("re-authenticated Expires %v, want %v", got, end2)
	}
}

// The krb row's read failing is ErrUnavailable, never ErrLogonFailure.
// Catches an outage read as an unknown name.
func TestKerberosSignInStoreFault(t *testing.T) {
	k := kerberosServer(t)
	k.user(t, entity.User{Principal: entity.NewUser(), Name: "alice", IDs: append(fileIDs("1500"), krbID("alice@TEST.REALM"))}, 0)
	k.fail(t, kvtest.OpGet, "PX*")
	res, oc, err := k.srv.SignInOutcome(context.Background(), k.as("alice"))
	refused(t, "the krb row's read failing", res, oc, err, vfserr.ErrUnavailable, "unavailable")
}

// A State with no keytab version, and one whose only version holds an RC4
// key beside its AES one, which the ring drops whole, answer ErrUnavailable,
// and none of those attempts counts against the pair: once a keytab is
// applied, the eleventh attempt signs in. Catches an unconfigured keytab
// answered as a wrong ticket, which would also lock the user out.
func TestKerberosSignInWithoutKeys(t *testing.T) {
	k := kerberosServer(t)
	alice := entity.User{Principal: entity.NewUser(), Name: "alice", IDs: fileIDs("1500")}
	k.user(t, alice, 0)
	mixed := keytab.New()
	for _, et := range []int32{etypeID.AES256_CTS_HMAC_SHA1_96, etypeID.RC4_HMAC} {
		if err := mixed.AddEntry("cifs/nas.test", krbRealm, "cifs/nas.test-password", time.Unix(1, 0), 1, et); err != nil {
			t.Fatal(err)
		}
	}
	mixedBytes, err := mixed.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name string
		kt   []byte
	}{{"no keytab", nil}, {"a keytab holding an RC4 key", mixedBytes}} {
		k.applyKeytab(t, c.kt)
		for range 5 {
			res, oc, err := k.srv.SignInOutcome(context.Background(), k.as("alice"))
			refused(t, c.name, res, oc, err, vfserr.ErrUnavailable, "unavailable")
		}
	}
	k.applyKeytab(t, k.realm.Keytab())
	k.signInAs(t, "alice once a keytab is applied", k.as("alice"), alice.Principal)
}

// Each attempt records one logon event naming the mechanism, the name as
// the ticket spells it, the transport's protocol, the outcome, the principal
// on success and the client, and counts once under (protocol, kerberos,
// outcome); no event carries the ticket or a key. Catches an attempt not
// recorded, its protocol read from Service rather than Transport, and a
// credential leaked into the event.
func TestKerberosSignInRecordsEachAttempt(t *testing.T) {
	k := kerberosServer(t)
	alice := entity.User{Principal: entity.NewUser(), Name: "alice", IDs: fileIDs("1500")}
	k.user(t, alice, 0)
	k.user(t, entity.User{Principal: entity.NewUser(), Name: "carol", IDs: fileIDs("1502"), Disabled: true}, 0)
	replayed := k.as("alice")
	if _, err := k.srv.SignIn(context.Background(), replayed); err != nil {
		t.Fatalf("the replayed ticket's first presentation: %v", err)
	}
	nfs := func(s pid.SignInStep) pid.SignInStep {
		s.Kerberos = k.realm.Ticket(identitytest.TicketOpts{Client: "alice", Service: "nfs/nas.test", Now: k.clock.Now()})
		s.Service, s.Transport = pid.ServiceNFS, pid.TransportNFS
		return s
	}
	for _, c := range []struct {
		name, protocol, outcome, presented string
		step                               pid.SignInStep
		prep                               func(t *testing.T)
	}{
		{"alice over SMB", "smb", "ok", "alice@TEST.REALM", k.as("alice"), nil},
		{"alice over NFS", "nfs", "ok", "alice@TEST.REALM", nfs(k.as("alice")), nil},
		{"unknown bob", "smb", "logon_failure", "bob@TEST.REALM", k.as("bob"), nil},
		{"disabled carol", "smb", "disabled", "carol@TEST.REALM", k.as("carol"), nil},
		{"a truncated AP-REQ", "smb", "invalid", "", func() pid.SignInStep { s := k.as("alice"); s.Kerberos = s.Kerberos[:20]; return s }(), nil},
		{"a replay", "smb", "replay", "alice@TEST.REALM", replayed, nil},
		{"the store failing", "smb", "unavailable", "alice@TEST.REALM", k.as("alice"), func(t *testing.T) { k.fail(t, kvtest.OpGet, "*") }},
	} {
		if c.prep != nil {
			c.prep(t)
		}
		events, count := k.logons.len(), k.kerberosSignIns(t, c.protocol, c.outcome)
		res, err := k.srv.SignIn(context.Background(), c.step)
		k.fault.SetFault(nil)
		var want entity.PrincipalID
		if c.outcome == "ok" {
			want = alice.Principal.ID
			if err != nil {
				t.Errorf("%s: SignIn = %v, want ok", c.name, err)
			}
		}
		got := k.logons.since(events)
		if len(got) != 1 {
			t.Errorf("%s: logon events = %d, want 1", c.name, len(got))
			continue
		}
		e := got[0]
		if e.Outcome != c.outcome || e.Mechanism != "kerberos" || e.Protocol != c.protocol || e.Name != c.presented ||
			e.Principal != want || e.Client != krbClient {
			t.Errorf("%s: logon event %+v, want outcome %q, kerberos over %s, name %q, principal %v, client %v",
				c.name, e, c.outcome, c.protocol, c.presented, want, krbClient)
		}
		text := fmt.Sprintf("%+v", e)
		for _, secret := range [][]byte{c.step.Kerberos, res.Keys.Session, res.Keys.APRep} {
			if len(secret) > 0 && (bytes.Contains([]byte(text), secret) || bytes.Contains([]byte(text), []byte(fmt.Sprint(secret)))) {
				t.Errorf("%s: the logon event %s carries a ticket or key", c.name, text)
			}
		}
		if d := k.kerberosSignIns(t, c.protocol, c.outcome) - count; d != 1 {
			t.Errorf("%s: identity_authentications_total{%s, kerberos, %s} rose by %v, want 1", c.name, c.protocol, c.outcome, d)
		}
	}
}

// A ticket carrying a PAC is refused ErrUnavailable until the PAC is
// verified, so no PAC is ever used unverified, and the refusal does not count
// on the throttle: after ten of alice's PAC tickets, her PAC-less one from
// the same address signs in. Catches the refusal counted as a logon failure,
// which would lock out a domain user whose every ticket carries a PAC.
func TestKerberosSignInRefusesAnUnverifiedPAC(t *testing.T) {
	k := kerberosServer(t)
	alice := entity.User{Principal: entity.NewUser(), Name: "alice", IDs: fileIDs("1500")}
	k.user(t, alice, 0)
	for i := range 10 {
		res, oc, err := k.srv.SignInOutcome(context.Background(), k.step(identitytest.TicketOpts{Client: "alice", Cusec: i, PAC: []byte("a PAC")}, krbClient))
		refused(t, fmt.Sprintf("alice's PAC ticket %d", i+1), res, oc, err, vfserr.ErrUnavailable, "unavailable")
	}
	k.signInAs(t, "alice's PAC-less ticket after ten PAC tickets", k.as("alice"), alice.Principal)
}

// A cross-realm ticket whose transited field names a realm is refused unless
// the KDC set TRANSITED-POLICY-CHECKED, as RFC 4120 §2.7 asks of a server
// that checks no transited realm itself; one with the flag set, and one
// whose field is empty, sign in. Catches the transited field never read.
func TestKerberosSignInChecksTransitedRealms(t *testing.T) {
	for _, c := range []struct {
		name      string
		transited string
		flags     []int
		ok        bool
	}{
		{"transited realms the KDC did not check", "MID.REALM", nil, false},
		{"transited realms the KDC checked", "MID.REALM", []int{flags.TransitedPolicyChecked}, true},
		{"no transited realm", "", nil, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			k := kerberosServer(t)
			carol := entity.User{Principal: entity.NewUser(), Name: "carol", IDs: append(fileIDs("1502"), krbID("alice@OTHER.REALM"))}
			k.user(t, carol, 0)
			step := k.step(identitytest.TicketOpts{Client: "alice", ClientRealm: "OTHER.REALM", Transited: c.transited, Flags: c.flags}, krbClient)
			if !c.ok {
				res, oc, err := k.srv.SignInOutcome(context.Background(), step)
				refused(t, c.name, res, oc, err, vfserr.ErrLogonFailure, "logon_failure")
				return
			}
			k.signInAs(t, c.name, step, carol.Principal)
		})
	}
}

// Kerberos failures count under the user part with NTLM's: 9 for unknown
// bob@TEST.REALM and one NTLM failure as CORP\bob refuse bob's valid ticket
// throttled, before any store read. Tickets that do not decrypt name no one
// and count nothing, so the eleventh failure for bob after twenty of them is
// the first refused. Catches a Kerberos failure the throttle misses, one
// counted under no name or under the full name, and the throttle asked after
// the store.
func TestKerberosSignInThrottle(t *testing.T) {
	k := kerberosServer(t)
	from1, from2 := netip.MustParseAddr("192.0.2.1"), netip.MustParseAddr("192.0.2.2")
	for i := range 9 {
		_, oc, _ := k.srv.SignInOutcome(context.Background(), k.step(identitytest.TicketOpts{Client: "bob"}, from1))
		if oc != "logon_failure" {
			t.Fatalf("unknown bob's failure %d: outcome %q, want logon_failure", i+1, oc)
		}
	}
	ntlm := ntlmStep("wrong", "bob", "CORP", false)
	ntlm.Client = from1
	if _, err := k.srv.SignIn(context.Background(), ntlm); !errors.Is(err, vfserr.ErrLogonFailure) {
		t.Fatalf(`NTLM as CORP\bob: %v, want ErrLogonFailure`, err)
	}
	k.user(t, entity.User{Principal: entity.NewUser(), Name: "bob", IDs: append(fileIDs("1600"), krbID("bob@TEST.REALM"))}, 0)
	k.fail(t, "*", "*")
	res, oc, err := k.srv.SignInOutcome(context.Background(), k.step(identitytest.TicketOpts{Client: "bob"}, from1))
	refused(t, "bob's valid ticket after 10 failures", res, oc, err, vfserr.ErrLogonFailure, "throttled")
	k.fault.SetFault(nil)

	garbled := identitytest.NewKey(etypeID.AES256_CTS_HMAC_SHA1_96)
	for range 20 {
		_, oc, _ := k.srv.SignInOutcome(context.Background(), k.step(identitytest.TicketOpts{Client: "dave", Key: garbled.KeyValue}, from2))
		if oc != "logon_failure" {
			t.Fatalf("a ticket that does not decrypt: outcome %q, want logon_failure", oc)
		}
	}
	for i := range 11 {
		_, oc, _ := k.srv.SignInOutcome(context.Background(), k.step(identitytest.TicketOpts{Client: "dave"}, from2))
		want := "logon_failure"
		if i == 10 {
			want = "throttled"
		}
		if oc != want {
			t.Fatalf("unknown dave's attempt %d after 20 undecryptable tickets: outcome %q, want %q", i+1, oc, want)
		}
	}
}

// The throttle runs before the replay cache's insert, and each ticket client
// holds at most its share of the cache. With a cache of 32 entries and a
// share of 16: unknown bob's 20 authenticators leave 10 entries, the rest
// refused throttled; carol's 40 successful sign-ins take her 16 and are then
// refused unavailable, and alice still signs in. Catches the insert placed
// ahead of the throttle, and a cache with no per-client cap, which lets one
// ticket holder fill it and refuse every client.
func TestKerberosSignInReplayCacheShare(t *testing.T) {
	k := kerberosServer(t)
	k.srv.BoundReplay(32, 16)
	carol := entity.User{Principal: entity.NewUser(), Name: "carol", IDs: fileIDs("1502")}
	alice := entity.User{Principal: entity.NewUser(), Name: "alice", IDs: fileIDs("1500")}
	k.user(t, carol, 0)
	k.user(t, alice, 0)
	for i := range 20 {
		_, oc, _ := k.srv.SignInOutcome(context.Background(), k.step(identitytest.TicketOpts{Client: "bob", Cusec: i}, netip.MustParseAddr("192.0.2.3")))
		want := "logon_failure"
		if i >= 10 {
			want = "throttled"
		}
		if oc != want {
			t.Fatalf("bob's authenticator %d: outcome %q, want %q", i+1, oc, want)
		}
	}
	if n := k.srv.ReplayLen(); n != 10 {
		t.Fatalf("replay cache after bob's 20 = %d entries, want 10", n)
	}
	for i := range 40 {
		_, oc, err := k.srv.SignInOutcome(context.Background(), k.step(identitytest.TicketOpts{Client: "carol", Cusec: i}, netip.MustParseAddr("192.0.2.5")))
		want, wantErr := "ok", error(nil)
		if i >= 16 {
			want, wantErr = "unavailable", vfserr.ErrUnavailable
		}
		if oc != want || !errors.Is(err, wantErr) {
			t.Fatalf("carol's authenticator %d: %v, outcome %q; want %v, %q", i+1, err, oc, wantErr, want)
		}
	}
	k.signInAs(t, "alice after carol's flood", k.step(identitytest.TicketOpts{Client: "alice"}, netip.MustParseAddr("192.0.2.4")), alice.Principal)
}

// In the directory's realm a ticket without a PAC is refused ErrLogonFailure
// and counted no_pac, before any mapping, so it binds neither the local alice
// of the same name nor the directory's alice (RFC 18 §3.4). Catches a
// PAC-less ticket bound by step 1, 2 or 3 in the directory's realm.
func TestKerberosSignInReachesTheDirectory(t *testing.T) {
	e := newAD(t)
	ctx := context.Background()
	local := entity.User{Principal: entity.NewUser(), Name: "alice", IDs: fileIDs("1500")}
	if _, err := e.st.ControlPlane().PutUser(ctx, local, 0, entity.Alloc{}); err != nil {
		t.Fatal(err)
	}
	e.d.AddUser(t, "alice", nil)
	realm := identitytest.NewRealm("CORP.EXAMPLE", "cifs/nas.corp.example")
	st := *e.s.Applied()
	st.Keytab = []secrets.Version{{Number: 1, Value: realm.Keytab()}}
	if err := e.s.Apply(st); err != nil {
		t.Fatal(err)
	}
	step := pid.SignInStep{Kerberos: realm.Ticket(identitytest.TicketOpts{Client: "alice", Service: "cifs/nas.corp.example", Now: e.clock.Now()}),
		Service: pid.ServiceCIFS, Transport: pid.TransportSMB, Client: krbClient}
	res, oc, err := e.s.SignInOutcome(ctx, step)
	refused(t, "alice@CORP.EXAMPLE without a PAC", res, oc, err, vfserr.ErrLogonFailure, "no_pac")
}
