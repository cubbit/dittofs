package gss_test

import (
	"bytes"
	"context"
	"errors"
	"net/netip"
	"testing"
	"time"

	"github.com/jcmturner/gokrb5/v8/iana/etypeID"
	"github.com/jcmturner/gokrb5/v8/types"

	"github.com/cubbit/dittofs/internal/adapter/gss"
	"github.com/cubbit/dittofs/internal/adapter/gss/gsstest"
	"github.com/cubbit/dittofs/pkg/identity/identitytest"
	"github.com/cubbit/dittofs/pkg/vfs"
	"github.com/cubbit/dittofs/pkg/vfs/vfstest"
	"github.com/cubbit/dittofs/pkg/vfs/vfstest/memory"
)

const (
	realmName = "TEST.REALM"
	service   = "nfs/nas.test"
)

// now is the time the memory service's identity clock reads.
var now = time.Unix(0, 0)

// nfsStep is the step an NFS adapter hands the mechanism.
var nfsStep = vfs.SignInStep{Service: vfs.ServiceNFS, Transport: vfs.TransportNFS, Client: netip.MustParseAddr("192.0.2.9")}

// fixture is memory's service holding a test realm's keytab for nfs/.
type fixture struct {
	svc   *memory.Service
	realm *identitytest.Realm
	cusec int
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	realm := identitytest.NewRealm(realmName, service)
	return &fixture{svc: memory.WithKeytab(t, realm.Keytab()), realm: realm}
}

// initiator is gsstest's initiator of a fresh AP-REQ for nfs/ under o, its
// authenticator's microseconds new to the replay cache.
func (f *fixture) initiator(t *testing.T, o identitytest.TicketOpts) (*gsstest.KerberosInitiator, types.EncryptionKey) {
	t.Helper()
	f.cusec++
	o.Service, o.Now, o.Cusec = service, now, f.cusec
	if o.Session.KeyType == 0 {
		o.Session = identitytest.NewKey(etypeID.AES256_CTS_HMAC_SHA1_96)
	}
	init, err := gsstest.Kerberos(f.realm.Ticket(o), o.Session)
	if err != nil {
		t.Fatal(err)
	}
	return init, o.Session
}

// firstToken is the initiator's initial context token.
func firstToken(t *testing.T, init gss.Mechanism) []byte {
	t.Helper()
	tok, done, err := init.Step(context.Background(), nil)
	if err != nil || done {
		t.Fatalf("initiator's first Step = done %v, %v; want a token", done, err)
	}
	return tok
}

// An AP-REQ for nfs/ completes in one Step through memory's SignIn: done, an
// AP-REP the initiator accepts, and a Context naming the ticket's principal
// with SignIn's keys, the context key RFC 18 §3.1 names. Catches a context
// key chosen otherwise, an AP-REP the initiator refuses, and an Evidence or
// a Keys not SignIn's.
func TestKerberosCompletesInOneStep(t *testing.T) {
	f := newFixture(t)
	end := now.Add(8 * time.Hour)
	for _, tc := range []struct {
		name   string
		subkey types.EncryptionKey
	}{
		{name: "no subkey: the ticket's session key"},
		{name: "an aes128 authenticator subkey", subkey: identitytest.NewKey(etypeID.AES128_CTS_HMAC_SHA1_96)},
		{name: "an aes256 authenticator subkey", subkey: identitytest.NewKey(etypeID.AES256_CTS_HMAC_SHA1_96)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			init, session := f.initiator(t, identitytest.TicketOpts{Client: "admin", End: end, Subkey: tc.subkey})
			want := session.KeyValue
			if tc.subkey.KeyType != 0 {
				want = tc.subkey.KeyValue
			}
			mech := gss.Kerberos(f.svc, nfsStep)
			out, done, err := mech.Step(t.Context(), firstToken(t, init))
			if err != nil || !done {
				t.Fatalf("Step = done %v, %v; want done", done, err)
			}
			if _, idone, err := init.Step(t.Context(), out); err != nil || !idone {
				t.Fatalf("the initiator's check of the AP-REP = done %v, %v; want done", idone, err)
			}
			c := mech.Context()
			if c == nil {
				t.Fatal("Context once done = nil")
			}
			if c.Evidence.Principal != f.svc.Store.Admin.Principal || c.Evidence.Name != "admin@"+realmName {
				t.Errorf("Evidence = %v %q, want the administrator %v as admin@%s", c.Evidence.Principal, c.Evidence.Name, f.svc.Store.Admin.Principal, realmName)
			}
			if !c.Evidence.Expires.Equal(end) {
				t.Errorf("Evidence.Expires = %v, want the ticket's end %v", c.Evidence.Expires, end)
			}
			if !bytes.Equal(c.Keys.Session, want) {
				t.Errorf("Keys.Session = %x, want the context key %x", c.Keys.Session, want)
			}
			if ic := init.Context(); ic == nil || !bytes.Equal(ic.Keys.Session, c.Keys.Session) {
				t.Errorf("the initiator's context = %+v, want its context key equal to the acceptor's %x", ic, c.Keys.Session)
			}
			if len(c.Keys.APRep) == 0 || !bytes.HasSuffix(out, c.Keys.APRep) {
				t.Errorf("the token out does not end with SignIn's AP-REP %x:\n%x", c.Keys.APRep, out)
			}
			again, _ := f.initiator(t, identitytest.TicketOpts{Client: "admin"})
			if _, _, err := mech.Step(t.Context(), firstToken(t, again)); !errors.Is(err, vfs.ErrLogonFailure) || mech.Context() != c {
				t.Errorf("a second valid token once done = %v, context %p; want ErrLogonFailure and the context %p kept", err, mech.Context(), c)
			}
		})
	}
}

// Every error of the exchange, SignIn's and the mechanism's own, is the
// logon failure, with no Context and no token out, and a token whose framing
// is refused never reaches SignIn. Catches an error passed through as another
// kind, an error that leaves a context, and a framing left for SignIn to
// refuse.
func TestKerberosRefusals(t *testing.T) {
	f := newFixture(t)
	faults := vfstest.Faults(f.svc)
	token := func(t *testing.T, o identitytest.TicketOpts) []byte {
		init, _ := f.initiator(t, o)
		return firstToken(t, init)
	}
	admin := func(t *testing.T) []byte { return token(t, identitytest.TicketOpts{Client: "admin"}) }
	for _, tc := range []struct {
		name    string
		signIn  error // SignIn's answer, through the fault wrapper; nil: memory's own
		framing bool  // refused by its framing, before SignIn
		tok     func(t *testing.T) []byte
	}{
		{name: "SignIn's ErrLogonFailure", signIn: vfs.ErrLogonFailure, tok: admin},
		{name: "SignIn's ErrInvalid", signIn: vfs.ErrInvalid, tok: admin},
		{name: "SignIn's ErrUnavailable", signIn: vfs.ErrUnavailable, tok: admin},
		{name: "a principal with no record", tok: func(t *testing.T) []byte {
			return token(t, identitytest.TicketOpts{Client: "bob"})
		}},
		{name: "an RFC 8009 authenticator subkey", tok: func(t *testing.T) []byte {
			return token(t, identitytest.TicketOpts{Client: "admin", Subkey: identitytest.NewKey(etypeID.AES128_CTS_HMAC_SHA256_128)})
		}},
		{name: "another mechanism's OID", framing: true, tok: func(t *testing.T) []byte {
			tok := admin(t)
			// Microsoft's legacy Kerberos OID, 1.2.840.48018.1.2.2, as long as
			// Kerberos's, in its place after the tag and two-byte length.
			if tok[1] != 0x82 || tok[4] != 0x06 || tok[5] != 0x09 {
				t.Fatalf("an initial token starting %x, want a two-byte length and a 9-byte OID", tok[:6])
			}
			return append(append(append([]byte{}, tok[:6]...), 0x2a, 0x86, 0x48, 0x82, 0xf7, 0x12, 0x01, 0x02, 0x02), tok[15:]...)
		}},
		{name: "a token whose framing runs past its bytes", framing: true, tok: func(t *testing.T) []byte {
			tok := admin(t)
			return tok[:len(tok)-1]
		}},
		{name: "a byte after the token's length", framing: true, tok: func(t *testing.T) []byte {
			return append(admin(t), 0)
		}},
		{name: "no token", framing: true, tok: func(*testing.T) []byte { return nil }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tok := tc.tok(t)
			armed := tc.signIn
			if tc.framing {
				armed = vfs.ErrUnavailable // fires only if the token reaches SignIn
			}
			if armed != nil {
				faults.Fail(vfs.OpSignIn, armed)
			}
			mech := gss.Kerberos(faults, nfsStep)
			out, done, err := mech.Step(t.Context(), tok)
			if !errors.Is(err, vfs.ErrLogonFailure) {
				t.Errorf("Step = %v, want ErrLogonFailure", err)
			}
			for _, other := range []error{vfs.ErrInvalid, vfs.ErrUnavailable} {
				if errors.Is(err, other) {
					t.Errorf("Step = %v, which also matches %v; want only ErrLogonFailure", err, other)
				}
			}
			if out != nil || done || mech.Context() != nil {
				t.Errorf("Step refused left token %x, done %v, context %+v; want none", out, done, mech.Context())
			}
			left := faults.Disarm()
			if tc.framing && len(left) != 1 {
				t.Errorf("a token refused by its framing reached SignIn")
			}
			if !tc.framing && len(left) != 0 {
				t.Errorf("SignIn's armed answer never fired: %v", left)
			}
		})
	}
}
