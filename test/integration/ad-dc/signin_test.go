//go:build ad_dc

package ad_dc_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jcmturner/gokrb5/v8/client"
	"github.com/jcmturner/gokrb5/v8/crypto"
	"github.com/jcmturner/gokrb5/v8/iana/keyusage"
	"github.com/jcmturner/gokrb5/v8/keytab"
	"github.com/jcmturner/gokrb5/v8/messages"
	"github.com/jcmturner/gokrb5/v8/types"

	"github.com/cubbit/dittofs/internal/identity"
	"github.com/cubbit/dittofs/internal/identity/directory"
	"github.com/cubbit/dittofs/internal/identity/netlogon"
	"github.com/cubbit/dittofs/pkg/auth/sid"
	pid "github.com/cubbit/dittofs/pkg/identity"
	"github.com/cubbit/dittofs/pkg/identity/identitytest"
	"github.com/cubbit/dittofs/pkg/keyprovider"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/metadata/kv/badgerkv"
	"github.com/cubbit/dittofs/pkg/metadata/kvstore"
	"github.com/cubbit/dittofs/pkg/metrics"
	"github.com/cubbit/dittofs/pkg/secrets"
	"github.com/cubbit/dittofs/pkg/vfs/vfserr"
)

// The node's computer account in the sign-in tests: BUILD01$, which the
// fixture stages with a known password. Not DITTOFS$, whose name is the
// domain's NetBIOS name, which an installation's netbios_name may not be.
const (
	nodeAccount  = "BUILD01"
	nodePassword = "Build01Pass01!"
)

// logonSink keeps the logon events a sign-in delivers.
type logonSink struct {
	mu     sync.Mutex
	events []identity.LogonEvent
}

func (l *logonSink) Logon(_ context.Context, e *identity.LogonEvent) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.events = append(l.events, *e)
}

// last is the outcome of the last event delivered.
func (l *logonSink) last() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.events) == 0 {
		return ""
	}
	return l.events[len(l.events)-1].Outcome
}

// signInNode is an identity server as a node runs it against the fixture:
// a Badger store, a secret provider holding the directory's bind password,
// the directory over LDAPS, and the domain controller over NETLOGON, joined
// as BUILD01$.
type signInNode struct {
	s      *identity.Server
	st     *kvstore.Store
	reg    *metrics.Registry
	logons *logonSink
	state  identity.State
	// down sends every connection to the controller, the endpoint mapper's
	// among them, to a port nothing listens on.
	down atomic.Bool
}

func newSignInNode(t *testing.T, dcIP string, ca []byte) *signInNode {
	t.Helper()
	ctx := context.Background()
	db, err := badgerkv.Open(badgerkv.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := kvstore.Init(ctx, db, uuid.New()); err != nil {
		t.Fatal(err)
	}
	st, err := kvstore.Open(ctx, db)
	if err != nil {
		t.Fatal(err)
	}

	t.Setenv("DITTOFS_ENCRYPTION_PASSPHRASE", "ad-dc sign-in passphrase")
	kf, err := keyprovider.GenerateKeyFile("ad-dc sign-in passphrase")
	if err != nil {
		t.Fatal(err)
	}
	kfPath := filepath.Join(t.TempDir(), "protocol.key")
	if err := os.WriteFile(kfPath, kf, 0o600); err != nil {
		t.Fatal(err)
	}
	kp, err := keyprovider.NewProvider(ctx, keyprovider.Config{Kind: keyprovider.KindLocal, File: kfPath})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = kp.Close() })
	p, err := secrets.New(st.ControlPlane(), map[secrets.Role]keyprovider.KeyProvider{secrets.Protocol: kp})
	if err != nil {
		t.Fatal(err)
	}
	owner := entity.SecretOwner{Scope: entity.SettingScope{Kind: entity.ScopeInstallation}, Key: "identity.directory"}
	var ref entity.SecretRef
	ref[0] = 1
	sec, err := p.Seal(ctx, secrets.Protocol, entity.SecretDirectoryCredential, owner, ref, []byte(adAdminPassword), secrets.Meta{})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.ControlPlane().Write(ctx, entity.WriteMeta{Audit: &entity.AuditRecord{
		Target: entity.ResourceRef{Kind: entity.ResourceSecret, ID: sec.ID[:]}, Change: []byte("set")}},
		func(tx entity.ControlPlaneTx) error { return tx.PutSecret(sec, sec.Version-1) }); err != nil {
		t.Fatal(err)
	}

	n := &signInNode{st: st, reg: metrics.New("test", "test"), logons: &logonSink{}}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	closed := l.Addr().String()
	_ = l.Close()
	dial := func(ctx context.Context, network, addr string) (net.Conn, error) {
		if n.down.Load() {
			addr = closed
		}
		return (&net.Dialer{}).DialContext(ctx, network, addr)
	}
	n.s, err = identity.New(identity.Deps{Store: st, Clock: wallClock{}, Metrics: n.reg, Secrets: p, Logons: n.logons,
		Directory: directory.DialTLS, DomainController: netlogon.NewDialer(dial), ControlPlane: st.ControlPlane()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = n.s.Stop(context.Background()) })
	n.state = identity.State{
		Settings: identity.Settings{IDRange: entity.DefaultIDRange, NTLM: true, NetBIOSName: nodeAccount},
		Machine:  []secrets.Version{{Number: 1, Value: []byte(nodePassword)}},
		Join:     entity.MachineJoin{Joined: true, Account: nodeAccount + "$"},
		Directory: &directory.Record{Schema: directory.SchemaAD, Address: "ldaps://" + adDCHost, Base: adBaseDN,
			BindName: adBindDN, BindCredential: ref, TrustRoots: string(ca), Realm: adRealm, NetBIOSDomain: adDomain,
			DNSServer: dcIP, DCAddress: dcIP},
	}
	n.apply(t)
	return n
}

// apply applies n.state.
func (n *signInNode) apply(t *testing.T) {
	t.Helper()
	if err := n.s.Apply(n.state); err != nil {
		t.Fatalf("Apply: %v", err)
	}
}

// signIn signs step in and returns its result, its logon outcome and its
// error.
func (n *signInNode) signIn(step pid.SignInStep) (pid.SignInResult, string, error) {
	res, err := n.s.SignIn(context.Background(), step)
	return res, n.logons.last(), err
}

// dropped is identity_pac_sids_dropped_total for reason.
func (n *signInNode) dropped(t *testing.T, reason string) float64 {
	t.Helper()
	mfs, err := n.reg.Registry().Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, mf := range mfs {
		if mf.GetName() != metrics.Namespace+"_identity_pac_sids_dropped_total" {
			continue
		}
		for _, m := range mf.GetMetric() {
			for _, l := range m.GetLabel() {
				if l.GetName() == "reason" && l.GetValue() == reason {
					return m.GetCounter().GetValue()
				}
			}
		}
	}
	return 0
}

// objectSID is the objectSid of the samba-tool user, group or computer name.
func objectSID(t *testing.T, kind, name string) sid.SID {
	t.Helper()
	s, err := sid.ParseSIDString(adAttrs(t, kind, name, "objectSid")["objectSid"])
	if err != nil {
		t.Fatalf("%s %s's objectSid: %v", kind, name, err)
	}
	return *s
}

// sidStrings formats each SID.
func sidStrings(sids []pid.SID) []string {
	out := make([]string, len(sids))
	for i := range sids {
		out[i] = sid.FormatSID(&sids[i])
	}
	return out
}

// addSIDHistory writes sids into the sIDHistory of the entry at dn, through
// the DC's own database, the one way AD lets a SID of another domain onto an
// account; the DC then carries them in the account's PAC and logon replies.
func addSIDHistory(t *testing.T, dn string, sids ...string) {
	t.Helper()
	ldif := "dn: " + dn + "\nchangetype: modify\nadd: sIDHistory\n"
	for _, s := range sids {
		ldif += "sIDHistory: " + s + "\n"
	}
	if out, err := adDockerExec("sh", "-c", "printf '%s' \"$1\" > /tmp/sidhistory.ldif && "+
		"ldbmodify -H /var/lib/samba/private/sam.ldb --controls=relax:0 /tmp/sidhistory.ldif", "sh", ldif); err != nil {
		t.Fatalf("add sIDHistory %v to %s: %v\n%s", sids, dn, err, out)
	}
}

// RFC 18 §3.5's pass-through against the Samba controller, through SignIn:
// a domain user's right password signs in bound to the principal holding
// the reply's SID, its SIDs the reply's after the filter; a wrong password
// and a disabled account are logon_failure, a wrong one counted toward the
// throttle; a controller out of reach is unavailable and counts nothing.
func TestSignInPassThrough(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping AD-DC integration test in short mode")
	}
	dcIP := setupADDC(t)
	waitForMachinePassword(t, dcIP, nodeAccount, nodePassword)
	_, ca := dcRecord(t)
	seed := &sambaDCSeeder{conn: adminLDAP(t, dcIP, ca), ip: dcIP}
	domain := seed.Domain().SID
	n := newSignInNode(t, dcIP, ca)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	step := func(user, password string, client netip.Addr) pid.SignInStep {
		var challenge [8]byte
		_, _ = rand.Read(challenge[:])
		nt, _ := ntlmResponseFor(ctx, t, nodeAccount, user, password, challenge)
		return pid.SignInStep{NTLM: &pid.NTLMAuthenticate{User: user, Domain: adDomain, Workstation: "DCTWS", NTResponse: nt},
			Challenge: challenge, Client: client}
	}
	newUser := func(prefix string) (string, sid.SID) {
		name := prefix + randomHex()[:8]
		return name, seed.AddUser(t, name, []byte(adUserPass))
	}

	// First, while no secure channel is up: every attempt against a
	// controller out of reach is unavailable, and none counts, so the right
	// password signs in from the same address once it is back.
	t.Run("unreachable controller", func(t *testing.T) {
		user, _ := newUser("dcto")
		client := netip.MustParseAddr("192.0.2.31")
		n.down.Store(true)
		defer n.down.Store(false) // so a failure here leaves the later cases a controller
		for i := range 11 {
			if _, oc, err := n.signIn(step(user, adUserPass, client)); !errors.Is(err, vfserr.ErrUnavailable) || oc != "unavailable" {
				t.Fatalf("attempt %d with the controller out of reach: %v, outcome %q; want ErrUnavailable, unavailable", i+1, err, oc)
			}
		}
		n.down.Store(false)
		if _, oc, err := n.signIn(step(user, adUserPass, client)); err != nil || oc != "ok" {
			t.Errorf("the right password once the controller is back: %v, outcome %q; want ok, the outages uncounted", err, oc)
		}
	})

	// The reply's groups, Domain Users and one the user is a member of, all
	// of the domain the reply vouches for, are kept, and nothing else is
	// added.
	t.Run("right password", func(t *testing.T) {
		user, userSID := newUser("dctr")
		g := "dctg" + randomHex()[:8]
		if out, err := adDockerExec("samba-tool", "group", "add", g); err != nil {
			t.Fatalf("samba-tool group add %s: %v\n%s", g, err, out)
		}
		if out, err := adDockerExec("samba-tool", "group", "addmembers", g, user); err != nil {
			t.Fatalf("samba-tool group addmembers %s %s: %v\n%s", g, user, err, out)
		}
		group := objectSID(t, "group", g)

		res, oc, err := n.signIn(step(user, adUserPass, netip.MustParseAddr("192.0.2.32")))
		if err != nil || oc != "ok" {
			t.Fatalf("%s's right password: %v, outcome %q; want ok", user, err, oc)
		}
		ev := res.Identity.Evidence
		p, err := n.s.Find(ctx, pid.Spelling{Scheme: pid.SchemeSID, SID: userSID}, pid.FindOpts{})
		if err != nil {
			t.Fatalf("Find of %s's SID: %v", user, err)
		}
		if ev.Principal != p || ev.Name != adDomain+`\`+user || res.Identity.Proof != pid.ProofSecret {
			t.Errorf("bound %v as %q, proof %v; want %v, the holder of %s's SID, as %q, a secret proof",
				ev.Principal, ev.Name, res.Identity.Proof, p, user, adDomain+`\`+user)
		}
		if len(res.Keys.Session) != 16 {
			t.Errorf("session key of %d bytes, want the reply's 16", len(res.Keys.Session))
		}
		got := sidStrings(ev.SIDs)
		if len(got) == 0 || got[0] != sid.FormatSID(&userSID) {
			t.Fatalf("SIDs %v, want %s's SID first", got, user)
		}
		groups := slices.Sorted(slices.Values(got[1:]))
		want := slices.Sorted(slices.Values([]string{sid.FormatSID(&group), sid.FormatSID(&domain) + "-513"}))
		if !slices.Equal(groups, want) {
			t.Errorf("group SIDs %v, want %v, the reply's", groups, want)
		}
	})

	// A SID of another domain and a well-known one in the user's sIDHistory
	// are dropped, when the reply carries them.
	t.Run("sid history", func(t *testing.T) {
		user, _ := newUser("dcth")
		foreign := "S-1-5-21-1-2-3-1199"
		addSIDHistory(t, "CN="+user+",CN=Users,"+adBaseDN, foreign, "S-1-5-18")
		other, wellKnown := n.dropped(t, "other_domain"), n.dropped(t, "well_known")
		res, oc, err := n.signIn(step(user, adUserPass, netip.MustParseAddr("192.0.2.35")))
		if err != nil || oc != "ok" {
			t.Fatalf("%s's right password: %v, outcome %q; want ok", user, err, oc)
		}
		got := sidStrings(res.Identity.Evidence.SIDs)
		for _, s := range []string{foreign, "S-1-5-18"} {
			if slices.Contains(got, s) {
				t.Errorf("SIDs %v carry %s, which the filter drops", got, s)
			}
		}
		if n.dropped(t, "other_domain") == other && n.dropped(t, "well_known") == wellKnown {
			t.Skip("Samba's NETLOGON reply leaves sIDHistory out of ExtraSids, so no reply from the fixture " +
				"carries a SID of another domain or a well-known one for the filter to drop")
		}
	})

	t.Run("wrong password", func(t *testing.T) {
		user, _ := newUser("dctw")
		client := netip.MustParseAddr("192.0.2.33")
		before := seed.BadPwdCount(t, user)
		for i := range 10 {
			if _, oc, err := n.signIn(step(user, "Wrong-"+adUserPass, client)); !errors.Is(err, vfserr.ErrLogonFailure) || oc != "logon_failure" {
				t.Fatalf("wrong password %d: %v, outcome %q; want ErrLogonFailure, logon_failure", i+1, err, oc)
			}
		}
		if got := seed.BadPwdCount(t, user); got != before+10 {
			t.Errorf("%s's badPwdCount after 10 wrong passwords = %d, want %d", user, got, before+10)
		}
		// Ten counted failures fill the throttle: the right password is
		// refused before it reaches the controller.
		if _, oc, err := n.signIn(step(user, adUserPass, client)); !errors.Is(err, vfserr.ErrLogonFailure) || oc != "throttled" {
			t.Errorf("the right password after 10 wrong ones: %v, outcome %q; want ErrLogonFailure, throttled", err, oc)
		}
		if got := seed.BadPwdCount(t, user); got != before+10 {
			t.Errorf("%s's badPwdCount after the throttled attempt = %d, want %d unchanged", user, got, before+10)
		}
	})

	t.Run("disabled account", func(t *testing.T) {
		user, _ := newUser("dctd")
		if out, err := adDockerExec("samba-tool", "user", "disable", user); err != nil {
			t.Fatalf("samba-tool user disable %s: %v\n%s", user, err, out)
		}
		if _, oc, err := n.signIn(step(user, adUserPass, netip.MustParseAddr("192.0.2.34"))); !errors.Is(err, vfserr.ErrLogonFailure) || oc != "logon_failure" {
			t.Errorf("disabled %s's right password: %v, outcome %q; want ErrLogonFailure, logon_failure", user, err, oc)
		}
	})
}

// RFC 18 §3.3 and §3.4 with the Samba KDC's tickets, through SignIn: a
// domain user's ticket signs in with its PAC verified, its SIDs the PAC's; a
// machine account's ticket mints a machine linked for the PAC's SID; a PAC
// whose server signature is altered, a ticket for a service name the keytab
// does not hold and one under a key version it does not hold are refused
// logon_failure.
func TestSignInKerberos(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping AD-DC integration test in short mode")
	}
	dcIP := setupADDC(t)
	_, ca := dcRecord(t)
	seed := &sambaDCSeeder{conn: adminLDAP(t, dcIP, ca), ip: dcIP}
	domain := seed.Domain().SID
	ktPath := copyADKeytab(t)
	ktBytes, err := os.ReadFile(ktPath)
	if err != nil {
		t.Fatal(err)
	}
	kt, err := keytab.Load(ktPath)
	if err != nil {
		t.Fatal(err)
	}
	n := newSignInNode(t, dcIP, ca)
	n.state.Keytab = []secrets.Version{{Number: 1, Value: ktBytes}}
	n.apply(t)
	krb5Conf := writeADKrb5Conf(t, dcIP)
	from := netip.MustParseAddr("192.0.2.41")

	// apReq is a step presenting tkt with a fresh authenticator.
	apReq := func(t *testing.T, cl *client.Client, tkt messages.Ticket, key types.EncryptionKey) pid.SignInStep {
		t.Helper()
		b, _ := adAPReq(t, cl, tkt, key)
		return pid.SignInStep{Kerberos: b, Service: pid.ServiceCIFS, Transport: pid.TransportSMB, Client: from}
	}
	refused := func(t *testing.T, what string, step pid.SignInStep) {
		t.Helper()
		if _, oc, err := n.signIn(step); !errors.Is(err, vfserr.ErrLogonFailure) || oc != "logon_failure" {
			t.Errorf("%s: %v, outcome %q; want ErrLogonFailure, logon_failure", what, err, oc)
		}
	}

	alice := adLogin(t, krb5Conf, adUserAlice, adUserPass)
	aliceTkt, aliceKey := adTicket(t, alice, adSMBSPNShort)

	t.Run("domain user", func(t *testing.T) {
		res, oc, err := n.signIn(apReq(t, alice, aliceTkt, aliceKey))
		if err != nil || oc != "ok" {
			t.Fatalf("alice's ticket: %v, outcome %q; want ok", err, oc)
		}
		ev := res.Identity.Evidence
		aliceSID := objectSID(t, "user", adUserAlice)
		p, err := n.s.Find(context.Background(), pid.Spelling{Scheme: pid.SchemeSID, SID: aliceSID}, pid.FindOpts{})
		if err != nil {
			t.Fatalf("Find of alice's SID: %v", err)
		}
		if ev.Principal != p || ev.Name != adUserAlice+"@"+adRealm || res.Identity.Proof != pid.ProofTicket {
			t.Errorf("bound %v as %q, proof %v; want %v, the holder of alice's SID, as alice@%s, a ticket proof",
				ev.Principal, ev.Name, res.Identity.Proof, p, adRealm)
		}
		got := sidStrings(ev.SIDs)
		if len(got) == 0 || got[0] != sid.FormatSID(&aliceSID) {
			t.Fatalf("SIDs %v, want alice's %s first", got, sid.FormatSID(&aliceSID))
		}
		// devs directly, engineering through devs, and Domain Users, her
		// primary group: the groups her PAC carries.
		devs, eng := objectSID(t, "group", "devs"), objectSID(t, "group", "engineering")
		for _, w := range []string{sid.FormatSID(&devs), sid.FormatSID(&eng), sid.FormatSID(&domain) + "-513"} {
			if !slices.Contains(got, w) {
				t.Errorf("SIDs %v, want %s among them", got, w)
			}
		}
		if !bytes.Equal(res.Keys.Session, aliceKey.KeyValue) || len(res.Keys.APRep) == 0 {
			t.Errorf("session key %x, AP-REP of %d bytes; want the ticket's session key %x and an AP-REP",
				res.Keys.Session, len(res.Keys.APRep), aliceKey.KeyValue)
		}
	})

	t.Run("altered PAC server signature", func(t *testing.T) {
		refused(t, "alice's ticket with its PAC's server signature altered",
			apReq(t, alice, alterServerSignature(t, aliceTkt, kt), aliceKey))
	})

	// A computer account with a service name of its own: its ticket mints
	// a machine, and a ticket for its service name is one the node's
	// keytab holds no key for.
	machine := "DCT" + randomHex()[:8]
	machinePW := "Dc1!" + randomHex() + randomHex()
	otherSPN := "cifs/" + machine + ".dittofs.ad"
	seed.StageComputer(t, identitytest.StagedComputer{Name: machine, Password: []byte(machinePW), EncTypes: 0x18, SPNs: []string{otherSPN}})

	t.Run("machine account", func(t *testing.T) {
		cl := adLogin(t, krb5Conf, machine+"$", machinePW)
		tkt, key := adTicket(t, cl, adSMBSPNShort)
		res, oc, err := n.signIn(apReq(t, cl, tkt, key))
		if err != nil || oc != "ok" {
			t.Fatalf("%s$'s ticket: %v, outcome %q; want ok", machine, err, oc)
		}
		name := machine + "$@" + adRealm
		p, err := n.st.Principals().ByProtocolID(context.Background(), entity.ProtocolID{Scheme: entity.SchemeKerberos, Value: name})
		if err != nil {
			t.Fatalf("the krb row %s: %v", name, err)
		}
		if p.Kind != entity.PrincipalMachine || res.Identity.Evidence.Principal != p {
			t.Errorf("bound %v, the row's holder %v of kind %v; want the row's machine", res.Identity.Evidence.Principal, p, p.Kind)
		}
		u, err := n.st.Principals().User(context.Background(), p)
		if err != nil {
			t.Fatalf("the machine's record: %v", err)
		}
		mSID := objectSID(t, "computer", machine)
		var want bytes.Buffer
		sid.EncodeSID(&want, &mSID)
		i := slices.IndexFunc(u.IDs, func(id entity.ProtocolID) bool { return id.Scheme == entity.SchemeKerberos && id.Value == name })
		if i < 0 || u.IDs[i].Linked != want.String() {
			t.Errorf("the machine's IDs %v, want its krb row linked for %s's SID %s", u.IDs, machine, sid.FormatSID(&mSID))
		}
		// Its next ticket binds the same machine.
		if res, oc, err := n.signIn(apReq(t, cl, tkt, key)); err != nil || oc != "ok" || res.Identity.Evidence.Principal != p {
			t.Errorf("%s$'s second sign-in: %v, outcome %q, bound %v; want ok, %v", machine, err, oc, res.Identity.Evidence.Principal, p)
		}
	})

	t.Run("another service name", func(t *testing.T) {
		tkt, key := adTicket(t, alice, otherSPN)
		refused(t, "alice's ticket for "+otherSPN, apReq(t, alice, tkt, key))
	})

	// Last, since it changes the service account's keys: a ticket issued
	// after the service account's password changed is under a key version
	// the node's keytab, exported before, does not hold.
	t.Run("stale keytab", func(t *testing.T) {
		if out, err := adDockerExec("samba-tool", "user", "setpassword", "dittofs-svc", "--newpassword=Svc-"+randomHex()+"1!"); err != nil {
			t.Fatalf("samba-tool user setpassword dittofs-svc: %v\n%s", err, out)
		}
		cl := adLogin(t, krb5Conf, adUserAlice, adUserPass)
		tkt, key := adTicket(t, cl, adSMBSPNShort)
		if tkt.EncPart.KVNO == aliceTkt.EncPart.KVNO {
			t.Fatalf("the ticket after the password change is under kvno %d, the keytab's; want a newer one", tkt.EncPart.KVNO)
		}
		refused(t, fmt.Sprintf("alice's ticket under kvno %d", tkt.EncPart.KVNO), apReq(t, cl, tkt, key))
	})
}

// alterServerSignature is tkt with one byte of its PAC's server signature
// flipped, its encrypted part sealed again under the service key from kt,
// so the ticket still decrypts and only the signature is wrong.
func alterServerSignature(t *testing.T, tkt messages.Ticket, kt *keytab.Keytab) messages.Ticket {
	t.Helper()
	key, _, err := kt.GetEncryptionKey(tkt.SName, tkt.Realm, tkt.EncPart.KVNO, tkt.EncPart.EType)
	if err != nil {
		t.Fatalf("the service key for the ticket: %v", err)
	}
	plain, err := crypto.DecryptEncPart(tkt.EncPart, key, keyusage.KDC_REP_TICKET)
	if err != nil {
		t.Fatalf("decrypt the ticket: %v", err)
	}
	dec := tkt
	if err := dec.Decrypt(key); err != nil {
		t.Fatalf("decrypt the ticket: %v", err)
	}
	raw := rawPAC(t, &dec, "cifs")
	at := bytes.Index(plain, raw)
	if at < 0 {
		t.Fatal("the PAC's bytes are not in the ticket's plaintext")
	}
	// PACTYPE: cBuffers, Version, then cBuffers PAC_INFO_BUFFERs of ulType,
	// cbBufferSize and an 8-byte Offset (MS-PAC 2.3, 2.4).
	sig := -1
	for i := range int(binary.LittleEndian.Uint32(raw)) {
		e := raw[8+16*i:]
		if binary.LittleEndian.Uint32(e) == pacServerSignature {
			sig = int(binary.LittleEndian.Uint64(e[8:]))
		}
	}
	if sig < 0 {
		t.Fatal("the PAC holds no server signature")
	}
	plain[at+sig+4] ^= 0xff // past the signature's 4-byte type, into its value
	enc, err := crypto.GetEncryptedData(plain, key, keyusage.KDC_REP_TICKET, tkt.EncPart.KVNO)
	if err != nil {
		t.Fatalf("seal the altered ticket: %v", err)
	}
	out := tkt
	out.EncPart = enc
	return out
}
