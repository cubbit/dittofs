package identity_test

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/md5"
	"crypto/rc4"
	"encoding/binary"
	"errors"
	"net/netip"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cubbit/dittofs/internal/identity"
	"github.com/cubbit/dittofs/pkg/auth/sid"
	pid "github.com/cubbit/dittofs/pkg/identity"
	"github.com/cubbit/dittofs/pkg/identity/identitytest"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/metadata/kv/kvtest"
	"github.com/cubbit/dittofs/pkg/secrets"
	"github.com/cubbit/dittofs/pkg/sim"
	"github.com/cubbit/dittofs/pkg/vfs/vfserr"
)

// NTLM's negotiate flags a step sets (MS-NLMP 2.2.2.5).
const (
	flagSign    = 0x00000010
	flagKeyExch = 0x40000000
)

var (
	ntlmChallenge = [8]byte{0x11, 0x22, 0x33, 0x44, 0x55, 0x66, 0x77, 0x88}
	ntlmClient    = netip.MustParseAddr("192.0.2.10")
	clientKey     = bytes.Repeat([]byte{0x77}, 16) // the random session key the client wraps
)

// ntlmStep answers ntlmChallenge as a client does for user in domain with
// password: a key exchange wrapping clientKey and, with mic, MsvAvFlags
// marking a MIC, which Raw carries at offset 72 (MS-NLMP 3.2.5.1.2). The MIC
// is computed here, independently of identity's code; Raw is never parsed by
// identity, so its other bytes are filler.
func ntlmStep(password, user, domain string, mic bool) pid.SignInStep {
	var pairs [][]byte
	if mic {
		pairs = append(pairs, binary.LittleEndian.AppendUint32([]byte{6, 0, 4, 0}, 2)) // MsvAvFlags: MIC present
	}
	nt, _, base := identitytest.NTLMv2Response([]byte(password), user, domain, ntlmChallenge, [8]byte{0xaa, 0xbb}, pairs...)
	c, err := rc4.NewCipher(base)
	if err != nil {
		panic(err)
	}
	enc := make([]byte, 16)
	c.XORKeyStream(enc, clientKey)
	a := &pid.NTLMAuthenticate{
		User: user, Domain: domain, NTResponse: nt, EncryptedSessionKey: enc,
		Flags:     flagKeyExch | flagSign,
		Negotiate: []byte("the NEGOTIATE's bytes"), Challenge: []byte("the CHALLENGE's bytes"),
		Raw: bytes.Repeat([]byte{0x5a}, 120),
	}
	if mic {
		setMIC(a, clientKey)
	}
	return pid.SignInStep{NTLM: a, Challenge: ntlmChallenge, Client: ntlmClient}
}

// setMIC writes the MIC keyed by key over a's three messages into a.Raw.
func setMIC(a *pid.NTLMAuthenticate, key []byte) {
	clear(a.Raw[72:88])
	m := hmac.New(md5.New, key)
	m.Write(a.Negotiate)
	m.Write(a.Challenge)
	m.Write(a.Raw)
	copy(a.Raw[72:88], m.Sum(nil))
}

// ntlmServer is a signing fixture under netbios with NTLM on.
func ntlmServer(t *testing.T, netbios string) (*resolving, *secrets.Provider) {
	t.Helper()
	r, p := signing(t)
	r.applyNTLM(t, netbios, true)
	return r, p
}

func (r *resolving) applyNTLM(t *testing.T, netbios string, on bool) {
	t.Helper()
	st := state(1)
	st.Settings.NetBIOSName, st.Settings.NTLM = netbios, on
	if err := r.srv.Apply(st); err != nil {
		t.Fatalf("Apply: %v", err)
	}
}

// withNTHash derives u's NT hash from password through NewHashes with NTLM
// on, seals and stores it as the API does, owned by u's record under
// "nt_hash" (RFC 16 §2.3), and names it in u.NTHash. NewHashes derives none
// for an account with no file spelling, which then keeps none.
func (r *resolving) withNTHash(t *testing.T, p *secrets.Provider, u *entity.User, password string) {
	t.Helper()
	h, err := pid.NewHashes(*u, []byte(password), pid.NTLMSetting{On: true})
	if err != nil {
		t.Fatal(err)
	}
	if h.NTHash != nil {
		r.sealNTHash(t, p, u, h.NTHash)
	}
}

// sealNTHash seals hash as u's NT hash and names it in u.NTHash.
func (r *resolving) sealNTHash(t *testing.T, p *secrets.Provider, u *entity.User, hash []byte) {
	t.Helper()
	ctx := context.Background()
	u.NTHash = entity.SecretRef(uuid.New())
	owner := entity.SecretOwner{Record: entity.ResourceRef{Kind: entity.ResourceUser, ID: bytes.Clone(u.Principal.ID[:])}, Key: "nt_hash"}
	sec, err := p.Seal(ctx, secrets.Protocol, entity.SecretNTHash, owner, u.NTHash, hash, secrets.Meta{})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.cp.PutSecret(ctx, sec, 0); err != nil {
		t.Fatal(err)
	}
}

// ntlmUser stores a file user name with password's NT hash.
func (r *resolving) ntlmUser(t *testing.T, p *secrets.Provider, name, uid, password string) entity.User {
	t.Helper()
	u := entity.User{Principal: entity.NewUser(), Name: name, IDs: fileIDs(uid)}
	r.withNTHash(t, p, &u, password)
	r.user(t, u, 0)
	return u
}

// refused checks that a sign-in failed with want and outcome and answered
// the zero result: no identity, guest or anonymous among them, and no key.
func refused(t *testing.T, what string, res pid.SignInResult, oc string, err error, want error, wantOutcome string) {
	t.Helper()
	if !errors.Is(err, want) {
		t.Errorf("%s: error %v, want %v", what, err, want)
	}
	if oc != wantOutcome {
		t.Errorf("%s: outcome %q, want %q", what, oc, wantOutcome)
	}
	if res.Identity.Proof != pid.ProofNone || res.Identity.Evidence.Principal != (entity.Principal{}) || res.Identity.Evidence.Name != "" ||
		res.Identity.Client.IsValid() || res.Keys.Session != nil || res.Keys.Subkey != nil || res.Keys.APRep != nil {
		t.Errorf("%s: result %+v, want the zero result", what, res)
	}
}

// MS-NLMP §4.2.4's AUTHENTICATE signs in a user "User" seeded with password
// "Password" through NewHashes, under NetBIOS name DOMAIN; with its user
// rewritten USER too, since NTOWFv2 upper-cases the user. Catches an NT hash
// NewHashes writes that NTLM cannot verify, a name echoed as presented rather
// than as stored, and a session key other than the unwrapped one.
func TestSignInPublishedExample(t *testing.T) {
	r, p := ntlmServer(t, "DOMAIN")
	user := r.ntlmUser(t, p, "User", "1500", "Password")
	for _, presented := range []string{"User", "USER"} {
		step := pid.SignInStep{
			NTLM: &pid.NTLMAuthenticate{
				User: presented, Domain: "Domain", NTResponse: identity.NLMPResponse(),
				EncryptedSessionKey: identity.NLMPEncryptedSKey, Flags: identity.NLMPFlags,
			},
			Challenge: identity.NLMPServerChallenge,
			Client:    ntlmClient,
		}
		res, oc, err := r.srv.SignInOutcome(context.Background(), step)
		if err != nil || oc != "ok" {
			t.Fatalf("SignIn(§4.2.4 as %q) = %v, outcome %q; want nil, ok", presented, err, oc)
		}
		want := pid.Identity{Proof: pid.ProofSecret, Protection: pid.ProtectionNone,
			Evidence: pid.Evidence{Name: "User", Principal: user.Principal}, Client: ntlmClient}
		if got := res.Identity; got.Proof != want.Proof || got.Protection != want.Protection || got.Client != want.Client ||
			got.Evidence.Name != want.Evidence.Name || got.Evidence.Principal != want.Evidence.Principal ||
			got.Evidence.SIDs != nil || got.Evidence.DenyOnly != nil || !got.Evidence.Expires.IsZero() {
			t.Errorf("SignIn(§4.2.4 as %q) identity = %+v, want %+v", presented, got, want)
		}
		if !bytes.Equal(res.Keys.Session, identity.NLMPRandomKey[:]) || res.Keys.Subkey != nil || res.Keys.APRep != nil {
			t.Errorf("SignIn(§4.2.4 as %q) keys = %+v, want Session %x alone", presented, res.Keys, identity.NLMPRandomKey)
		}
	}
}

// The domain table, local and refusal rows (RFC 18 §3.5, §10 "NTLM domain"):
// the NetBIOS name in another case and an empty domain are local; any other
// domain, a qualified user name with an empty domain, the Kelvin sign for K,
// a trailing dot and an alternate UPN suffix are unknown_domain; a response
// computed for an empty domain but sent naming the NetBIOS name is refused,
// so no domain but the one the message names is tried.
func TestSignInDomains(t *testing.T) {
	r, p := ntlmServer(t, "KIOSK")
	alice := r.ntlmUser(t, p, "alice", "1500", alicePassword)
	r.ntlmUser(t, p, "bob", "1501", alicePassword)
	for _, c := range []struct {
		name                 string
		user, domain, sentAs string
		err                  error
		outcome              string
	}{
		{"the NetBIOS name in lower case", "alice", "kiosk", "kiosk", nil, "ok"},
		{"an empty domain", "alice", "", "", nil, "ok"},
		{"another domain", "alice", "OTHER", "OTHER", vfserr.ErrLogonFailure, "unknown_domain"},
		{"a UPN, empty domain", "alice@corp.example.com", "", "", vfserr.ErrLogonFailure, "unknown_domain"},
		{"an alternate UPN suffix", "alice@alt.example", "", "", vfserr.ErrLogonFailure, "unknown_domain"},
		{`CORP\bob, empty domain`, `CORP\bob`, "", "", vfserr.ErrLogonFailure, "unknown_domain"},
		{"the Kelvin sign for K", "alice", "KIOSK", "KIOSK", vfserr.ErrLogonFailure, "unknown_domain"},
		{"a trailing dot", "alice", "KIOSK.", "KIOSK.", vfserr.ErrLogonFailure, "unknown_domain"},
		{"computed for no domain, sent as KIOSK", "alice", "", "KIOSK", vfserr.ErrLogonFailure, "logon_failure"},
	} {
		step := ntlmStep(alicePassword, c.user, c.domain, false)
		step.NTLM.Domain = c.sentAs
		res, oc, err := r.srv.SignInOutcome(context.Background(), step)
		if c.err != nil {
			refused(t, c.name, res, oc, err, c.err, c.outcome)
			continue
		}
		if err != nil || oc != c.outcome || res.Identity.Evidence.Principal != alice.Principal {
			t.Errorf("%s: SignIn = %v, %v, outcome %q; want alice's principal, nil, ok", c.name, res.Identity.Evidence.Principal, err, oc)
		}
	}
}

// Refusals decided from the message alone answer before any read: with the
// store failing every call, an NTLMv1 or LM-only response, signing without
// key exchange and NTLM off are ErrLogonFailure, not ErrUnavailable, and a
// malformed response, a key exchange of 15 bytes, an AUTHENTICATE naming no
// user and a step with neither or both mechanisms are ErrInvalid. Catches a
// check run after a read, and a malformed message decided by the password.
func TestSignInRefusesFromTheMessage(t *testing.T) {
	r, p := ntlmServer(t, "NAS01")
	r.ntlmUser(t, p, "alice", "1500", alicePassword)
	r.fail(t, "*", "*")
	right := func() pid.SignInStep { return ntlmStep(alicePassword, "alice", "", false) }
	for _, c := range []struct {
		name    string
		step    func() pid.SignInStep
		err     error
		outcome string
	}{
		{"an NTLMv1 response", func() pid.SignInStep {
			s := right()
			s.NTLM.NTResponse = s.NTLM.NTResponse[:24]
			return s
		}, vfserr.ErrLogonFailure, "logon_failure"},
		{"an LM response only", func() pid.SignInStep {
			s := right()
			s.NTLM.NTResponse = nil
			return s
		}, vfserr.ErrLogonFailure, "logon_failure"},
		{"signing without key exchange", func() pid.SignInStep {
			s := right()
			s.NTLM.Flags = flagSign
			return s
		}, vfserr.ErrLogonFailure, "logon_failure"},
		{"a malformed response", func() pid.SignInStep {
			s := right()
			s.NTLM.NTResponse[16] = 2 // RespType
			return s
		}, vfserr.ErrInvalid, "invalid"},
		{"a key exchange of 15 bytes", func() pid.SignInStep {
			s := right()
			s.NTLM.EncryptedSessionKey = s.NTLM.EncryptedSessionKey[:15]
			return s
		}, vfserr.ErrInvalid, "invalid"},
		{"no user name", func() pid.SignInStep {
			s := right()
			s.NTLM.User = ""
			return s
		}, vfserr.ErrInvalid, ""},
		{"neither mechanism", func() pid.SignInStep { return pid.SignInStep{Client: ntlmClient} }, vfserr.ErrInvalid, ""},
		{"both mechanisms", func() pid.SignInStep {
			s := right()
			s.Kerberos = []byte("an AP-REQ")
			return s
		}, vfserr.ErrInvalid, ""},
	} {
		res, oc, err := r.srv.SignInOutcome(context.Background(), c.step())
		refused(t, c.name, res, oc, err, c.err, c.outcome)
	}
	r.fault.SetFault(nil)
	r.applyNTLM(t, "NAS01", false)
	r.fail(t, "*", "*")
	res, oc, err := r.srv.SignInOutcome(context.Background(), right())
	refused(t, "NTLM off", res, oc, err, vfserr.ErrLogonFailure, "logon_failure")
}

// A wrong password, an unknown user, a user seeded while NTLM was off, an
// API-only account, a directory principal whose NT hash was written straight
// to the store and a disabled user are each refused ErrLogonFailure; all but
// the wrong password compute the proof against the dummy NT hash, so an
// unknown name costs the NTLMv2 work a wrong password does. Catches a
// directory principal refused only by its hash's absence, and a short-cut
// for a user that cannot sign in.
func TestSignInRefusesAccounts(t *testing.T) {
	r, p := ntlmServer(t, "NAS01")
	alice := r.ntlmUser(t, p, "alice", "1500", alicePassword)

	noHash := entity.User{Principal: entity.NewUser(), Name: "nohash", IDs: fileIDs("1501")}
	r.user(t, noHash, 0) // seeded while NTLM was off: NewHashes derived no NT hash
	api := entity.User{Principal: entity.NewUser(), Name: "api"}
	r.withNTHash(t, p, &api, alicePassword)
	r.user(t, api, 0)
	// A directory principal holds a dir spelling; its NT hash is named in its
	// record directly, as NewHashes never derives one for it.
	dir := entity.User{Principal: entity.NewUser(), Name: "dirbob", IDs: append(fileIDs("1503"), entity.ProtocolID{Scheme: entity.SchemeDir, Value: "dir-entry-1"})}
	h, err := pid.NewHashes(entity.User{IDs: fileIDs("1")}, []byte(alicePassword), pid.NTLMSetting{On: true})
	if err != nil {
		t.Fatal(err)
	}
	r.sealNTHash(t, p, &dir, h.NTHash)
	r.user(t, dir, 0)
	disabled := entity.User{Principal: entity.NewUser(), Name: "carol", IDs: fileIDs("1502"), Disabled: true}
	r.withNTHash(t, p, &disabled, alicePassword)
	r.user(t, disabled, 0)

	var real [16]byte
	r.srv.OnNTHash(func(nt [16]byte) { real = nt })
	if _, err := r.srv.SignIn(context.Background(), ntlmStep(alicePassword, "alice", "", false)); err != nil {
		t.Fatalf("SignIn(alice, her password) = %v, want nil", err)
	}
	if real == r.srv.DummyNTHash() {
		t.Fatal("alice's sign-in computed its proof with the dummy NT hash")
	}

	for _, c := range []struct {
		name, user, password string
		outcome              string
		dummy                bool
	}{
		{"a wrong password", alice.Name, "wrong", "logon_failure", false},
		{"an unknown user", "nobody", alicePassword, "logon_failure", true},
		{"a user with no NT hash", noHash.Name, alicePassword, "logon_failure", true},
		{"an API-only account", api.Name, alicePassword, "logon_failure", true},
		{"a directory principal with an NT hash", dir.Name, alicePassword, "logon_failure", true},
		{"a disabled user", disabled.Name, alicePassword, "disabled", true},
	} {
		var used [][16]byte
		r.srv.OnNTHash(func(nt [16]byte) { used = append(used, nt) })
		res, oc, err := r.srv.SignInOutcome(context.Background(), ntlmStep(c.password, c.user, "", false))
		refused(t, c.name, res, oc, err, vfserr.ErrLogonFailure, c.outcome)
		if len(used) != 1 {
			t.Errorf("%s: proofs computed = %d, want 1", c.name, len(used))
			continue
		}
		if got := used[0] == r.srv.DummyNTHash(); got != c.dummy {
			t.Errorf("%s: proof computed with the dummy NT hash = %v, want %v", c.name, got, c.dummy)
		}
	}
}

// A store that cannot answer, the user's read or the NT hash's, and a server
// with no secret provider, are ErrUnavailable with the zero result, never a
// wrong password or a session.
func TestSignInOutage(t *testing.T) {
	for _, c := range []struct {
		name, op, key string
	}{
		{"the name's read", kvtest.OpGet, "NX‖*"},
		{"the user's read", kvtest.OpGet, "U‖*"},
		{"the NT hash's read", kvtest.OpBefore, "SEC‖*"},
	} {
		t.Run(c.name, func(t *testing.T) {
			r, p := ntlmServer(t, "NAS01")
			r.ntlmUser(t, p, "alice", "1500", alicePassword)
			r.fail(t, c.op, c.key)
			res, oc, err := r.srv.SignInOutcome(context.Background(), ntlmStep(alicePassword, "alice", "", false))
			refused(t, c.name+" failing", res, oc, err, vfserr.ErrUnavailable, "unavailable")
		})
	}
	t.Run("no secret provider", func(t *testing.T) {
		r, p := ntlmServer(t, "NAS01")
		r.ntlmUser(t, p, "alice", "1500", alicePassword)
		s, err := identity.New(identity.Deps{Store: r.st, Clock: sim.NewTestClock(time.Unix(0, 0))})
		if err != nil {
			t.Fatal(err)
		}
		if err := s.Apply(state(1)); err != nil {
			t.Fatal(err)
		}
		res, oc, err := s.SignInOutcome(context.Background(), ntlmStep(alicePassword, "alice", "", false))
		refused(t, "no secret provider", res, oc, err, vfserr.ErrUnavailable, "unavailable")
	})
}

// A sign-in writes nothing: with every write failing, alice still signs in.
// Catches a write on sign-in, such as a last-logon time.
func TestSignInWritesNothing(t *testing.T) {
	r, p := ntlmServer(t, "NAS01")
	r.ntlmUser(t, p, "alice", "1500", alicePassword)
	var rules []sim.Rule
	for _, op := range []string{kvtest.OpUpdate, kvtest.OpSet, kvtest.OpDelete, kvtest.OpGuard, kvtest.OpGuardRange} {
		rules = append(rules, sim.Rule{Op: op, Key: "*", Action: sim.FaultAction{Err: errors.New("injected write fault")}})
	}
	s, err := sim.NewSchedule(1, sim.NewTestClock(time.Unix(0, 0)), rules...)
	if err != nil {
		t.Fatal(err)
	}
	r.fault.SetFault(s)
	if _, err := r.srv.SignIn(context.Background(), ntlmStep(alicePassword, "alice", "", true)); err != nil {
		t.Errorf("SignIn with every write failing = %v, want nil", err)
	}
}

// §10's "NTLM MIC", local half: with the right password and a MIC flagged,
// a correct MIC signs in with the client's key, while a corrupted MIC, a
// zeroed one and a Raw too short to hold one are each ErrLogonFailure.
// Catches an advisory MIC check, and a right password told apart by its
// answer.
func TestSignInMIC(t *testing.T) {
	r, p := ntlmServer(t, "NAS01")
	alice := r.ntlmUser(t, p, "alice", "1500", alicePassword)
	res, err := r.srv.SignIn(context.Background(), ntlmStep(alicePassword, "alice", "", true))
	if err != nil || res.Identity.Evidence.Principal != alice.Principal || !bytes.Equal(res.Keys.Session, clientKey) {
		t.Fatalf("SignIn with a correct MIC = %v, %+v; want alice with the client's key %x", err, res, clientKey)
	}
	for _, c := range []struct {
		name  string
		spoil func(a *pid.NTLMAuthenticate)
	}{
		{"a corrupted MIC", func(a *pid.NTLMAuthenticate) { a.Raw[80] ^= 1 }},
		{"a zeroed MIC", func(a *pid.NTLMAuthenticate) { clear(a.Raw[72:88]) }},
		{"a Raw of 87 bytes", func(a *pid.NTLMAuthenticate) { a.Raw = a.Raw[:87] }},
		{"a message byte changed", func(a *pid.NTLMAuthenticate) { a.Challenge[0] ^= 1 }},
	} {
		step := ntlmStep(alicePassword, "alice", "", true)
		c.spoil(step.NTLM)
		res, oc, err := r.srv.SignInOutcome(context.Background(), step)
		refused(t, c.name, res, oc, err, vfserr.ErrLogonFailure, "logon_failure")
	}
}

// identity.ntlm off refuses NTLM sign-in from the Apply that delivers it,
// and on again accepts it.
func TestSignInFollowsNTLMSetting(t *testing.T) {
	r, p := ntlmServer(t, "NAS01")
	r.ntlmUser(t, p, "alice", "1500", alicePassword)
	for _, on := range []bool{false, true, false} {
		r.applyNTLM(t, "NAS01", on)
		_, err := r.srv.SignIn(context.Background(), ntlmStep(alicePassword, "alice", "", false))
		if on && err != nil {
			t.Errorf("SignIn with NTLM on = %v, want nil", err)
		}
		if !on && !errors.Is(err, vfserr.ErrLogonFailure) {
			t.Errorf("SignIn with NTLM off = %v, want ErrLogonFailure", err)
		}
	}
}

// A re-authentication answers with the identity the new proof establishes
// and nothing of the context's evidence; a failed one answers none. Catches
// the old evidence kept or merged (RFC 18 §3.8).
func TestSignInReauthentication(t *testing.T) {
	r, p := ntlmServer(t, "NAS01")
	alice := r.ntlmUser(t, p, "alice", "1500", alicePassword)
	bob := r.ntlmUser(t, p, "bob", "1501", "bob's password")
	ctxEvidence := &pid.Evidence{Name: "alice", Principal: alice.Principal,
		SIDs: []pid.SID{*sid.ParseSIDMust("S-1-5-21-9-9-9-1500")}, Expires: time.Unix(1e9, 0)}

	step := ntlmStep("bob's password", "bob", "", false)
	step.Context = ctxEvidence
	res, err := r.srv.SignIn(context.Background(), step)
	if err != nil {
		t.Fatalf("re-authentication as bob = %v, want nil", err)
	}
	if ev := res.Identity.Evidence; ev.Principal != bob.Principal || ev.Name != "bob" || ev.SIDs != nil || ev.DenyOnly != nil || !ev.Expires.IsZero() {
		t.Errorf("re-authentication as bob: evidence %+v, want bob's alone", ev)
	}

	step = ntlmStep("wrong", "bob", "", false)
	step.Context = ctxEvidence
	res, oc, err := r.srv.SignInOutcome(context.Background(), step)
	refused(t, "a failed re-authentication", res, oc, err, vfserr.ErrLogonFailure, "logon_failure")
}
