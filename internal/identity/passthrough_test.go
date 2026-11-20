package identity_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/cubbit/dittofs/internal/identity"
	"github.com/cubbit/dittofs/internal/identity/directory"
	"github.com/cubbit/dittofs/internal/identity/netlogon"
	"github.com/cubbit/dittofs/internal/prof"
	"github.com/cubbit/dittofs/pkg/auth/sid"
	pid "github.com/cubbit/dittofs/pkg/identity"
	"github.com/cubbit/dittofs/pkg/identity/identitytest"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/metadata/kvstore"
	"github.com/cubbit/dittofs/pkg/metrics"
	"github.com/cubbit/dittofs/pkg/secrets"
	"github.com/cubbit/dittofs/pkg/sim"
	"github.com/cubbit/dittofs/pkg/vfs/vfserr"
)

// ptFixture is a server joined as NAS$ to the domain CORP, realm
// CORP.EXAMPLE, of one controller, dc1, reached through the fault wrapper,
// whose directory, an in-memory one behind its own fault wrapper, serves the
// same domain and holds alice under the SID the controller answers for her.
type ptFixture struct {
	s      *identity.Server
	st     *kvstore.Store
	d      *identitytest.DomainController
	f      *identitytest.FaultyDomainController
	dirf   *switched
	clock  *sim.TestClock
	reg    *metrics.Registry
	logons *logonRecorder
	domain string // the domain's SID
	alice  sid.SID
	bind   string // the directory's bind name
}

func newPassThrough(t *testing.T, rules ...sim.Rule) *ptFixture {
	t.Helper()
	prof.Watch(t, prof.Options{})
	st, _ := dirStore(t)
	fx := &ptFixture{st: st, dirf: &switched{}, clock: sim.NewTestClock(mt0), reg: metrics.New("test", "test"), logons: &logonRecorder{}}
	dir := identitytest.NewDirectory("DC=corp,DC=example")
	domain, _, err := sid.DecodeSID(dir.DomainSID())
	if err != nil {
		t.Fatal(err)
	}
	fx.domain = sid.FormatSID(domain)
	fx.d = identitytest.NewDomainController("CORP", machineRealm, "DC=corp,DC=example", dc1)
	fx.d.UseDomainSID(t, *domain)
	fx.d.StageComputer(t, identitytest.StagedComputer{Name: "NAS", Password: []byte(pw2), EncTypes: 0x18})
	fx.alice = fx.d.AddUser(t, "alice", alicePW)
	var aliceSID bytes.Buffer
	sid.EncodeSID(&aliceSID, &fx.alice)
	dir.AddUser(t, "alice", map[string][][]byte{"objectSid": {aliceSID.Bytes()}})
	// The domain's Guest, as every domain holds one, so a reply naming it
	// finds an entry to mint.
	dir.AddUser(t, "Guest", map[string][][]byte{"objectSid": {directory.WithRID(dir.DomainSID(), 501)}})

	sched, err := sim.NewSchedule(1, fx.clock, rules...)
	if err != nil {
		t.Fatal(err)
	}
	fx.f = identitytest.NewFaultyDomainController(fx.d, sched)
	p := protocolSecrets(t, st)
	var pw []byte
	fx.bind, pw = dir.Account()
	owner := entity.SecretOwner{Scope: entity.SettingScope{Kind: entity.ScopeInstallation}, Key: "identity.directory"}
	var ref entity.SecretRef
	ref[0] = 1
	sec, err := p.Seal(context.Background(), secrets.Protocol, entity.SecretDirectoryCredential, owner, ref, pw, secrets.Meta{})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.ControlPlane().Write(context.Background(), entity.WriteMeta{Audit: &entity.AuditRecord{
		Target: entity.ResourceRef{Kind: entity.ResourceSecret, ID: sec.ID[:]}, Change: []byte("set")}},
		func(tx entity.ControlPlaneTx) error { return tx.PutSecret(sec, sec.Version-1) }); err != nil {
		t.Fatal(err)
	}
	dirFault := identitytest.NewFaultyDirectory(dir.Dial, fx.dirf)
	fx.s, err = identity.New(identity.Deps{Store: st, Clock: fx.clock, Metrics: fx.reg, Secrets: p,
		Directory: dirFault.Dial, DomainController: fx.f, Logons: fx.logons})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = fx.s.Stop(context.Background()) })
	fx.apply(t, true)
	return fx
}

// apply applies the joined State, or the same State not joined.
func (fx *ptFixture) apply(t *testing.T, joined bool) {
	t.Helper()
	st := joinedState(2, ver(2, 2, pw2, false))
	st.Join.Joined = joined
	var ref entity.SecretRef
	ref[0] = 1
	st.Directory = &directory.Record{Schema: directory.SchemaAD, Address: "ldaps://dc1.corp.example", Base: "DC=corp,DC=example",
		BindName: fx.bind, BindCredential: ref, Realm: machineRealm, NetBIOSDomain: "CORP", DCAddress: dc1}
	if err := fx.s.Apply(st); err != nil {
		t.Fatalf("Apply: %v", err)
	}
}

// signIn signs step in, naming the outcome.
func (fx *ptFixture) signIn(step pid.SignInStep) (pid.SignInResult, string, error) {
	return fx.s.SignInOutcome(context.Background(), step)
}

// signedIn signs step in and wants it bound to the principal holding alice's
// SID, which it returns.
func (fx *ptFixture) signedIn(t *testing.T, what string, step pid.SignInStep) pid.SignInResult {
	t.Helper()
	res, oc, err := fx.signIn(step)
	if err != nil || oc != "ok" {
		t.Fatalf("%s: SignIn = %v, outcome %q; want ok", what, err, oc)
	}
	p, err := fx.s.Find(context.Background(), pid.Spelling{Scheme: pid.SchemeSID, SID: fx.alice}, pid.FindOpts{})
	if err != nil {
		t.Fatalf("%s: Find of alice's SID: %v", what, err)
	}
	if got := res.Identity.Evidence.Principal; got != p {
		t.Errorf("%s: bound %v, want %v, the holder of alice's SID", what, got, p)
	}
	return res
}

func (fx *ptFixture) badPwd(t *testing.T) int { return fx.d.BadPwdCount(t, "alice") }

// plainStep is alice's response to ntlmChallenge for user in domain with no
// key exchange, no signing and no MIC, so the exported key is the reply's
// session key as it came.
func plainStep(password, user, domain string) pid.SignInStep {
	step := ntlmStep(password, user, domain, false)
	step.NTLM.Flags, step.NTLM.EncryptedSessionKey = 0, nil
	return step
}

// RFC 18 §10 "pass-through": alice, passed through, is bound to the
// principal minted for her SID, her SIDs are her own and Domain Users, her
// key is the exported one, and a caller resolved from her identity is that
// principal.
func TestPassThroughSignsIn(t *testing.T) {
	fx := newPassThrough(t)
	res := fx.signedIn(t, "alice in CORP", ntlmStep(string(alicePW), "alice", "CORP", true))
	ev := res.Identity.Evidence
	if got, want := sidStrings(ev.SIDs), []string{sid.FormatSID(&fx.alice), fx.domain + "-513"}; !slices.Equal(got, want) {
		t.Errorf("SIDs = %v, want %v", got, want)
	}
	if ev.Name != `CORP\alice` || res.Identity.Proof != pid.ProofSecret || res.Identity.Client != ntlmClient {
		t.Errorf("evidence %+v, proof %v, client %v; want CORP\\alice, a secret proof, from %v", ev, res.Identity.Proof, res.Identity.Client, ntlmClient)
	}
	if !bytes.Equal(res.Keys.Session, clientKey) {
		t.Errorf("session key %x, want the exported key %x", res.Keys.Session, clientKey)
	}
	share := fx.share(t, ev.Principal)
	nobody := entity.User{Principal: entity.NewUser(), Name: "nobody", IDs: fileIDs("60000")}
	c, err := fx.s.Resolve(context.Background(), res.Identity, entity.SharePolicy{Share: share, Squash: entity.SquashNone, Anonymous: nobody.Principal}, pid.ReadOnlyNotNarrowed)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if c.Principal != ev.Principal || !c.Authenticated {
		t.Errorf("resolved caller %v (authenticated %v), want %v, authenticated", c.Principal, c.Authenticated, ev.Principal)
	}
}

// share is a share granting p read access.
func (fx *ptFixture) share(t *testing.T, p entity.Principal) entity.ShareID {
	t.Helper()
	ctx := context.Background()
	cp := fx.st.ControlPlane()
	ns := entity.Namespace{ID: entity.NewNamespaceID(), Store: [16]byte{15: 1}}
	if _, err := cp.PutNamespace(ctx, ns, 0); err != nil {
		t.Fatal(err)
	}
	id := entity.NewShareID()
	sh := entity.Share{ID: id, Name: "s" + id.String()[:8], Path: "/s" + id.String()[:8], Root: entity.NewFileID(), Namespaces: []entity.NamespaceGen{{Generation: 1, Namespace: ns.ID}}}
	if _, err := cp.PutShare(ctx, sh, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := cp.SetGrant(ctx, entity.ShareGrant{Share: id, Principal: p, Access: entity.AccessRead}, 0); err != nil {
		t.Fatal(err)
	}
	return id
}

// A wrong password raises the controller's count by one, refused
// logon_failure; a 24-byte response, NTLMv1, raises it by none.
func TestPassThroughCountsAtTheController(t *testing.T) {
	fx := newPassThrough(t)
	res, oc, err := fx.signIn(ntlmStep("wrong", "alice", "CORP", true))
	refused(t, "a wrong password", res, oc, err, vfserr.ErrLogonFailure, "logon_failure")
	if got := fx.badPwd(t); got != 1 {
		t.Errorf("badPwdCount after a wrong password = %d, want 1", got)
	}
	v1 := ntlmStep(string(alicePW), "alice", "CORP", false)
	v1.NTLM.NTResponse = v1.NTLM.NTResponse[:24]
	res, oc, err = fx.signIn(v1)
	if err == nil || res.Identity.Evidence.Principal != (entity.Principal{}) {
		t.Errorf("a 24-byte response: SignIn = %v, outcome %q, bound %v; want refused", err, oc, res.Identity.Evidence.Principal)
	}
	if got := fx.badPwd(t); got != 1 {
		t.Errorf("badPwdCount after a 24-byte response = %d, want 1: it reached the controller", got)
	}
}

// Replies that are a guest's, by UserFlags or by RID 501, and replies whose
// session key is zero, 15 bytes or missing, or whose logon domain is
// another's, are each refused logon_failure.
func TestPassThroughChecksTheReply(t *testing.T) {
	other := *sid.ParseSIDMust("S-1-5-21-8-8-8")
	for _, tc := range []struct {
		name    string
		rewrite func(*netlogon.Reply)
	}{
		{"LOGON_GUEST", func(r *netlogon.Reply) { r.UserFlags |= 0x1 }},
		{"RID 501", func(r *netlogon.Reply) { r.User.SubAuthorities[len(r.User.SubAuthorities)-1] = 501 }},
		{"a zero session key", func(r *netlogon.Reply) { r.SessionKey = make([]byte, 16) }},
		{"a 15-byte session key", func(r *netlogon.Reply) { r.SessionKey = r.SessionKey[:15] }},
		{"no session key", func(r *netlogon.Reply) { r.SessionKey = nil }},
		{"another domain's LogonDomainID", func(r *netlogon.Reply) { r.LogonDomain = other }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fx := newPassThrough(t)
			fx.f.RewriteReplies(tc.rewrite)
			res, oc, err := fx.signIn(plainStep(string(alicePW), "alice", "CORP"))
			refused(t, tc.name, res, oc, err, vfserr.ErrLogonFailure, "logon_failure")
		})
	}
}

// "NTLM MIC"'s pass-through half: a corrupted and a zeroed MIC, and signing
// without key exchange, are refused logon_failure.
func TestPassThroughMIC(t *testing.T) {
	fx := newPassThrough(t)
	corrupt := ntlmStep(string(alicePW), "alice", "CORP", true)
	corrupt.NTLM.Raw[72] ^= 1
	zero := ntlmStep(string(alicePW), "alice", "CORP", true)
	clear(zero.NTLM.Raw[72:88])
	noKeyExch := ntlmStep(string(alicePW), "alice", "CORP", false)
	noKeyExch.NTLM.Flags, noKeyExch.NTLM.EncryptedSessionKey = flagSign, nil
	for _, tc := range []struct {
		name string
		step pid.SignInStep
	}{{"a corrupted MIC", corrupt}, {"a zeroed MIC", zero}, {"signing without key exchange", noKeyExch}} {
		res, oc, err := fx.signIn(tc.step)
		refused(t, tc.name, res, oc, err, vfserr.ErrLogonFailure, "logon_failure")
	}
}

// With the directory's domain SID unread, pass-through answers
// ErrUnavailable and sends nothing.
func TestPassThroughDomainSIDUnread(t *testing.T) {
	fx := newPassThrough(t)
	s, err := sim.NewSchedule(1, fx.clock, sim.Rule{Op: "*", Key: "*", Action: sim.FaultAction{Err: errDown}})
	if err != nil {
		t.Fatal(err)
	}
	fx.dirf.cur.Store(s)
	res, oc, err := fx.signIn(ntlmStep("wrong", "alice", "CORP", true))
	refused(t, "the domain SID unread", res, oc, err, vfserr.ErrUnavailable, "unavailable")
	if got := fx.badPwd(t); got != 0 {
		t.Errorf("badPwdCount = %d, want 0: the response was sent", got)
	}
}

// "NTLM domain"'s pass-through half: the realm as the domain, and a UPN in
// the realm with no domain, pass through; an alternate suffix, a trailing
// dot, and any domain while not joined are unknown_domain and send nothing.
func TestPassThroughDomainTable(t *testing.T) {
	fx := newPassThrough(t)
	fx.signedIn(t, `corp.example\alice`, plainStep(string(alicePW), "alice", "corp.example"))
	// The in-memory controller holds no UPN, so it refuses the name; that it
	// answered at all is what shows the row routed it.
	if _, oc, _ := fx.signIn(plainStep(string(alicePW), "alice@corp.example", "")); oc != "logon_failure" {
		t.Errorf("alice@corp.example with no domain: outcome %q, want logon_failure, the controller's answer", oc)
	}
	for _, tc := range []struct{ user, domain string }{
		{"alice@alt.example", ""},
		{"alice", "corp.example."},
	} {
		res, oc, err := fx.signIn(plainStep("wrong", tc.user, tc.domain))
		refused(t, tc.user+` in "`+tc.domain+`"`, res, oc, err, vfserr.ErrLogonFailure, "unknown_domain")
	}
	fx.apply(t, false)
	res, oc, err := fx.signIn(plainStep("wrong", "alice", "CORP"))
	refused(t, "not joined", res, oc, err, vfserr.ErrLogonFailure, "unknown_domain")
	if got := fx.badPwd(t); got != 0 {
		t.Errorf("badPwdCount = %d, want 0: a response was sent", got)
	}
}

// "logon throttle"'s controller count: ten wrong passwords, then the right
// one, throttled before it reaches the controller, whose count stays 10.
func TestPassThroughThrottled(t *testing.T) {
	fx := newPassThrough(t)
	for range 10 {
		if _, oc, _ := fx.signIn(ntlmStep("wrong", "alice", "CORP", true)); oc != "logon_failure" {
			t.Fatalf("a wrong password: outcome %q, want logon_failure", oc)
		}
	}
	res, oc, err := fx.signIn(ntlmStep(string(alicePW), "alice", "CORP", true))
	refused(t, "the right password after ten wrong", res, oc, err, vfserr.ErrLogonFailure, "throttled")
	if got := fx.badPwd(t); got != 10 {
		t.Errorf("badPwdCount = %d, want 10", got)
	}
}

// A deny-only group SID is kept in DenyOnly and S-1-5-32-544 is dropped and
// counted builtin, through the PAC's filter.
func TestPassThroughFiltersSIDs(t *testing.T) {
	fx := newPassThrough(t)
	payroll := *sid.ParseSIDMust(fx.domain + "-2002")
	fx.f.RewriteReplies(func(r *netlogon.Reply) {
		r.Groups = append(r.Groups, netlogon.SIDAttr{SID: payroll, Attrs: denyOnly},
			netlogon.SIDAttr{SID: *sid.ParseSIDMust("S-1-5-32-544"), Attrs: enabled})
	})
	ev := fx.signedIn(t, "alice", ntlmStep(string(alicePW), "alice", "CORP", true)).Identity.Evidence
	if got, want := sidStrings(ev.DenyOnly), []string{fx.domain + "-2002"}; !slices.Equal(got, want) {
		t.Errorf("DenyOnly = %v, want %v", got, want)
	}
	if got := sidStrings(ev.SIDs); slices.Contains(got, "S-1-5-32-544") {
		t.Errorf("SIDs = %v, want S-1-5-32-544 dropped", got)
	}
	if got := fx.counter(t, "builtin"); got != 1 {
		t.Errorf("identity_pac_sids_dropped_total{builtin} = %v, want 1", got)
	}
}

func (fx *ptFixture) counter(t *testing.T, reason string) float64 {
	t.Helper()
	mfs, err := fx.reg.Registry().Gather()
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

// alice's record disabled is refused with outcome disabled; marked deleted,
// she is refused logon_failure as an unknown name is, and no principal is
// minted for her SID while the deletion runs; once it has ended, her next
// sign-in mints her a new principal.
func TestPassThroughDisabledAndDeleted(t *testing.T) {
	fx := newPassThrough(t)
	ctx := context.Background()
	p := fx.signedIn(t, "alice", ntlmStep(string(alicePW), "alice", "CORP", true)).Identity.Evidence.Principal
	pr, cp := fx.st.Principals(), fx.st.ControlPlane()
	u, err := pr.User(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	u.Disabled = true
	gen, err := cp.PutUser(ctx, u, u.Generation, entity.Alloc{})
	if err != nil {
		t.Fatalf("PutUser: %v", err)
	}
	res, oc, err := fx.signIn(ntlmStep(string(alicePW), "alice", "CORP", true))
	refused(t, "alice disabled", res, oc, err, vfserr.ErrLogonFailure, "disabled")

	if err := cp.DeleteUser(ctx, p, gen); err != nil {
		t.Fatalf("DeleteUser: %v", err)
	}
	res, oc, err = fx.signIn(ntlmStep(string(alicePW), "alice", "CORP", true))
	refused(t, "alice marked deleted", res, oc, err, vfserr.ErrLogonFailure, "logon_failure")
	if got, err := fx.s.Find(ctx, pid.Spelling{Scheme: pid.SchemeSID, SID: fx.alice}, pid.FindOpts{}); !errors.Is(err, vfserr.ErrUnknownPrincipal) {
		t.Errorf("Find of alice's SID after her refused sign-in: %v, %v; want ErrUnknownPrincipal, nothing minted", got, err)
	}

	for done := false; !done; {
		if done, err = fx.st.Nodes().ContinueDelete(ctx, p); err != nil {
			t.Fatalf("ContinueDelete: %v", err)
		}
	}
	if got := fx.signedIn(t, "alice after her deletion ended", ntlmStep(string(alicePW), "alice", "CORP", true)).Identity.Evidence.Principal; got == p {
		t.Errorf("alice after her deletion ended: bound %v, the deleted principal; want a new one", got)
	}
}

// Ten sign-ins the controller holds take every slot; an eleventh, started
// at 1 s, answers ErrUnavailable at 6 s and nothing is sent for it.
func TestPassThroughBounds(t *testing.T) {
	fx := newPassThrough(t, sim.Rule{Op: "logon", Key: "held*", Action: sim.FaultAction{Err: identitytest.ErrHold}})
	fx.signedIn(t, "alice, reading the domain SID", ntlmStep(string(alicePW), "alice", "CORP", true))
	ten := make(chan error, 10)
	for i := range 10 {
		go func() {
			_, err := fx.s.SignIn(context.Background(), ntlmStep("x", fmt.Sprintf("held%d", i), "CORP", true))
			ten <- err
		}()
	}
	wait(t, "ten held sign-ins", func() bool { return fx.f.Holding() == 10 })
	fx.clock.Advance(time.Second)
	before := fx.clock.Pending()
	eleventh := make(chan error, 1)
	go func() {
		_, err := fx.s.SignIn(context.Background(), ntlmStep("wrong", "alice", "CORP", true))
		eleventh <- err
	}()
	// Its directory bound and its wait for a slot.
	wait(t, "the eleventh waiting for a slot", func() bool { return fx.clock.Pending() == before+2 })
	fx.clock.Advance(4 * time.Second)
	for i := range 10 {
		if err := recv(t, ten, "a held sign-in's answer"); !errors.Is(err, vfserr.ErrUnavailable) {
			t.Errorf("held sign-in %d at 5 s: %v, want ErrUnavailable", i, err)
		}
	}
	fx.clock.Advance(time.Second)
	if err := recv(t, eleventh, "the eleventh's answer"); !errors.Is(err, vfserr.ErrUnavailable) {
		t.Errorf("eleventh sign-in at 6 s: %v, want ErrUnavailable", err)
	}
	if got := fx.badPwd(t); got != 0 {
		t.Errorf("badPwdCount = %d, want 0: the eleventh reached the controller", got)
	}
	fx.f.Release()
	wait(t, "the held calls' return", func() bool { return fx.s.PassThroughs() == 0 })
}

// The seam answers identity.netbios_name from the State in force, a new one
// after Apply, and ErrUnavailable before any.
func TestComputerName(t *testing.T) {
	s, err := identity.New(identity.Deps{Store: newStore(t), Clock: sim.NewTestClock(mt0)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ComputerName(); !errors.Is(err, vfserr.ErrUnavailable) {
		t.Errorf("ComputerName before Apply: %v, want ErrUnavailable", err)
	}
	for _, name := range []string{"NAS", "FILER-2"} {
		st := state(1)
		st.Settings.NetBIOSName = name
		if err := s.Apply(st); err != nil {
			t.Fatal(err)
		}
		if got, err := s.ComputerName(); err != nil || got != name {
			t.Errorf("ComputerName after Apply of %s = %q, %v; want %q", name, got, err, name)
		}
	}
}
