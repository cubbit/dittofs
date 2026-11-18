package identity

import (
	"bytes"
	"errors"
	"log/slog"
	"net/netip"
	"testing"
	"time"

	"github.com/jcmturner/gokrb5/v8/crypto"
	"github.com/jcmturner/gokrb5/v8/iana/etypeID"
	"github.com/jcmturner/gokrb5/v8/iana/keyusage"
	"github.com/jcmturner/gokrb5/v8/messages"

	"github.com/cubbit/dittofs/internal/prof"
	"github.com/cubbit/dittofs/pkg/identity"
	"github.com/cubbit/dittofs/pkg/identity/identitytest"
	"github.com/cubbit/dittofs/pkg/secrets"
	"github.com/cubbit/dittofs/pkg/vfs/vfserr"
)

const testRealm = "EXAMPLE.TEST"

// t0 is the injected clock's time in these tests, years from the wall
// clock's, in whole seconds since a Kerberos time carries none finer.
var t0 = time.Date(2031, 3, 4, 12, 0, 0, 0, time.UTC)

var (
	addr1 = netip.MustParseAddr("192.0.2.1")
	addr2 = netip.MustParseAddr("192.0.2.2")
)

// testRealmAndState is the test realm for cifs/, host/ and nfs/nas.test, and a
// State whose ring is its keytab plus, for cifs/nas.test only, its RC4 and
// triple DES keys, which no keytab may hold, so only the verifier's own type
// check stands between a ticket under them and its acceptance.
func testRealmAndState(t *testing.T) (*identitytest.Realm, *State) {
	t.Helper()
	prof.Watch(t, prof.Options{})
	r := identitytest.NewRealm(testRealm, "cifs/nas.test", "host/nas.test", "nfs/nas.test")
	st := &State{keys: buildRing([]secrets.Version{{Number: 1, Value: r.Keytab()}}, slog.New(slog.DiscardHandler))}
	for _, et := range []int32{etypeID.RC4_HMAC, etypeID.DES3_CBC_SHA1_KD} {
		st.keys[ringKey{"cifs/nas.test", testRealm, 1, et}] = r.Key("cifs/nas.test", et)
	}
	return r, st
}

// Every check refuses its own case, and the ticket each one refuses passes
// once its one field is moved back within bounds.
func TestVerifyAPReq(t *testing.T) {
	r, st := testRealmAndState(t)
	other := identitytest.NewRealm(testRealm, "cifs/gone.test")
	cifs := func(o identitytest.TicketOpts) identitytest.TicketOpts {
		if o.Service == "" {
			o.Service = "cifs/nas.test"
		}
		o.Now = t0
		return o
	}
	for _, tc := range []struct {
		name   string
		realm  *identitytest.Realm
		opts   identitytest.TicketOpts
		svc    identity.Service
		tr     identity.Transport
		client netip.Addr
		want   error
	}{
		{name: "a ticket for cifs/nas.test", opts: cifs(identitytest.TicketOpts{})},
		{name: "start 6 minutes ahead", opts: cifs(identitytest.TicketOpts{Start: t0.Add(6 * time.Minute)}), want: vfserr.ErrLogonFailure},
		{name: "start 4 minutes ahead", opts: cifs(identitytest.TicketOpts{Start: t0.Add(4 * time.Minute)})},
		{name: "end 6 minutes past", opts: identitytest.TicketOpts{Service: "cifs/nas.test", Now: t0.Add(-time.Hour), End: t0.Add(-6 * time.Minute), AuthTime: t0}, want: vfserr.ErrLogonFailure},
		{name: "end 4 minutes past", opts: identitytest.TicketOpts{Service: "cifs/nas.test", Now: t0.Add(-time.Hour), End: t0.Add(-4 * time.Minute), AuthTime: t0}},
		{name: "authenticator 6 minutes ahead", opts: cifs(identitytest.TicketOpts{AuthTime: t0.Add(6 * time.Minute)}), want: vfserr.ErrLogonFailure},
		{name: "authenticator 6 minutes behind", opts: cifs(identitytest.TicketOpts{AuthTime: t0.Add(-6 * time.Minute)}), want: vfserr.ErrLogonFailure},
		{name: "authenticator 4 minutes ahead", opts: cifs(identitytest.TicketOpts{AuthTime: t0.Add(4 * time.Minute)})},
		{name: "authenticator 4 minutes behind", opts: cifs(identitytest.TicketOpts{AuthTime: t0.Add(-4 * time.Minute)})},
		{name: "service name rewritten under the same key", opts: cifs(identitytest.TicketOpts{Service: "cifs/other.test", KeyOf: "cifs/nas.test"}), want: vfserr.ErrLogonFailure},
		{name: "a name the ring lacks", realm: other, opts: identitytest.TicketOpts{Service: "cifs/gone.test", Now: t0}, want: vfserr.ErrLogonFailure},
		{name: "the name spelled CIFS/NAS.TEST", opts: identitytest.TicketOpts{Service: "CIFS/NAS.TEST", KeyOf: "cifs/nas.test", Now: t0}},
		{name: "a cifs/ ticket presented as NFS", opts: cifs(identitytest.TicketOpts{}), svc: identity.ServiceNFS, want: vfserr.ErrLogonFailure},
		{name: "addresses: listed, from it", opts: cifs(identitytest.TicketOpts{Addresses: []netip.Addr{addr1}}), client: addr1},
		{name: "addresses: listed, from another", opts: cifs(identitytest.TicketOpts{Addresses: []netip.Addr{addr1}}), client: addr2, want: vfserr.ErrLogonFailure},
		{name: "addresses: NetBIOS only", opts: cifs(identitytest.TicketOpts{NetBIOS: "WORKSTATION"}), client: addr2},
		{name: "addresses: none", opts: cifs(identitytest.TicketOpts{}), client: addr2},
		{name: "host/ over SMB from another address", opts: identitytest.TicketOpts{Service: "host/nas.test", Now: t0, Addresses: []netip.Addr{addr1}}, svc: identity.ServiceHost, client: addr2, want: vfserr.ErrLogonFailure},
		{name: "host/ over NFS from another address", opts: identitytest.TicketOpts{Service: "host/nas.test", Now: t0, Addresses: []netip.Addr{addr1}}, svc: identity.ServiceHost, tr: identity.TransportNFS, client: addr2},
		{name: "no transport", opts: cifs(identitytest.TicketOpts{Addresses: []netip.Addr{addr1}}), tr: 255, client: addr2, want: vfserr.ErrInvalid},
		{name: "ticket under RC4", opts: cifs(identitytest.TicketOpts{Etype: etypeID.RC4_HMAC}), want: vfserr.ErrLogonFailure},
		{name: "ticket under DES", opts: cifs(identitytest.TicketOpts{Etype: etypeID.DES3_CBC_SHA1_KD}), want: vfserr.ErrLogonFailure},
		{name: "RC4 session key", opts: cifs(identitytest.TicketOpts{Session: identitytest.NewKey(etypeID.RC4_HMAC)}), want: vfserr.ErrLogonFailure},
		{name: "RC4 subkey", opts: cifs(identitytest.TicketOpts{Subkey: identitytest.NewKey(etypeID.RC4_HMAC)}), want: vfserr.ErrLogonFailure},
		{name: "AES128 ticket and keys", opts: cifs(identitytest.TicketOpts{Etype: etypeID.AES128_CTS_HMAC_SHA1_96, Session: identitytest.NewKey(etypeID.AES128_CTS_HMAC_SHA1_96), Subkey: identitytest.NewKey(etypeID.AES128_CTS_HMAC_SHA1_96)})},
		{name: "AES256 subkey", opts: cifs(identitytest.TicketOpts{Subkey: identitytest.NewKey(etypeID.AES256_CTS_HMAC_SHA1_96)})},
		{name: "authenticator naming another client", opts: cifs(identitytest.TicketOpts{AuthClient: "mallory"}), want: vfserr.ErrLogonFailure},
	} {
		t.Run(tc.name, func(t *testing.T) {
			realm, svc, tr, client := tc.realm, tc.svc, tc.tr, tc.client
			if realm == nil {
				realm = r
			}
			if svc == 0 {
				svc = identity.ServiceCIFS
			}
			switch tr {
			case 0:
				tr = identity.TransportSMB
			case 255:
				tr = 0
			}
			if !client.IsValid() {
				client = addr1
			}
			_, err := verifyAPReq(st, realm.Ticket(tc.opts), svc, tr, client, t0)
			if tc.want == nil && err != nil {
				t.Fatalf("verifyAPReq = %v, want accepted", err)
			}
			if tc.want != nil && !errors.Is(err, tc.want) {
				t.Fatalf("verifyAPReq = %v, want %v", err, tc.want)
			}
		})
	}
}

// Bytes that do not decode are malformed; a ticket under a held name, key
// version number and type whose ciphertext was flipped is a logon failure.
func TestVerifyAPReqMalformedAndTampered(t *testing.T) {
	r, st := testRealmAndState(t)
	b := r.Ticket(identitytest.TicketOpts{Service: "cifs/nas.test", Now: t0})
	if _, err := verifyAPReq(st, b[:len(b)/2], identity.ServiceCIFS, identity.TransportSMB, addr1, t0); !errors.Is(err, vfserr.ErrInvalid) {
		t.Errorf("truncated AP-REQ: verifyAPReq = %v, want ErrInvalid", err)
	}
	req, err := parseAPReq(b)
	if err != nil {
		t.Fatal(err)
	}
	req.Ticket.EncPart.Cipher[len(req.Ticket.EncPart.Cipher)/2] ^= 1
	flipped, err := req.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := verifyAPReq(st, flipped, identity.ServiceCIFS, identity.TransportSMB, addr1, t0); !errors.Is(err, vfserr.ErrLogonFailure) {
		t.Errorf("flipped ticket ciphertext: verifyAPReq = %v, want ErrLogonFailure", err)
	}
}

// Keys.Session is the authenticator's subkey when it has one, else the
// ticket's session key; Keys.Subkey stays empty, and the AP-REP decrypts
// under the ticket's session key to the authenticator's time.
func TestVerifyAPReqKeys(t *testing.T) {
	r, st := testRealmAndState(t)
	session := identitytest.NewKey(etypeID.AES256_CTS_HMAC_SHA1_96)
	subkey := identitytest.NewKey(etypeID.AES128_CTS_HMAC_SHA1_96)
	authTime := t0.Add(time.Minute)
	for _, tc := range []struct {
		name        string
		opts        identitytest.TicketOpts
		wantSession []byte
	}{
		{"no subkey", identitytest.TicketOpts{Session: session}, session.KeyValue},
		{"a subkey", identitytest.TicketOpts{Session: session, Subkey: subkey}, subkey.KeyValue},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o := tc.opts
			o.Service, o.Now, o.AuthTime, o.Cusec = "cifs/nas.test", t0, authTime, 4242
			tk, err := verifyAPReq(st, r.Ticket(o), identity.ServiceCIFS, identity.TransportSMB, addr1, t0)
			if err != nil {
				t.Fatalf("verifyAPReq: %v", err)
			}
			if !bytes.Equal(tk.keys.Session, tc.wantSession) {
				t.Errorf("Keys.Session = %x, want %x", tk.keys.Session, tc.wantSession)
			}
			if len(tk.keys.Subkey) != 0 {
				t.Errorf("Keys.Subkey = %x, want empty: the AP-REP sends no acceptor subkey", tk.keys.Subkey)
			}
			var rep messages.APRep
			if err := rep.Unmarshal(tk.keys.APRep); err != nil {
				t.Fatalf("AP-REP does not decode: %v", err)
			}
			plain, err := crypto.DecryptEncPart(rep.EncPart, session, keyusage.AP_REP_ENCPART)
			if err != nil {
				t.Fatalf("AP-REP does not decrypt under the ticket's session key: %v", err)
			}
			var part messages.EncAPRepPart
			if err := part.Unmarshal(plain); err != nil {
				t.Fatalf("AP-REP's encrypted part does not decode: %v", err)
			}
			if !part.CTime.Equal(authTime) || part.Cusec != 4242 {
				t.Errorf("AP-REP time = %v.%06d, want the authenticator's %v.%06d", part.CTime, part.Cusec, authTime, 4242)
			}
			if part.Subkey.KeyType != 0 || len(part.Subkey.KeyValue) != 0 {
				t.Errorf("AP-REP subkey = %+v, want none", part.Subkey)
			}
		})
	}
}
