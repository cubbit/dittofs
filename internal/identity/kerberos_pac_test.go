package identity_test

import (
	"context"
	"encoding/binary"
	"strings"
	"testing"

	"github.com/jcmturner/gokrb5/v8/iana/chksumtype"
	"github.com/jcmturner/gokrb5/v8/iana/etypeID"
	"github.com/jcmturner/gokrb5/v8/types"

	"github.com/cubbit/dittofs/internal/identity/directory"
	pid "github.com/cubbit/dittofs/pkg/identity"
	"github.com/cubbit/dittofs/pkg/identity/identitytest"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/secrets"
	"github.com/cubbit/dittofs/pkg/vfs/vfserr"
)

// aliceLogon is the logon information of alice, a user of the test realm's
// domain.
var aliceLogon = identitytest.LogonInfo{Name: "alice", Domain: "TEST", DomainSID: "S-1-5-21-7-7-7", UserRID: 1105,
	Groups: []identitytest.GroupRID{{RID: 513, Attributes: 7}}}

// pacStep is an SMB step presenting a cifs/nas.test ticket for alice, issued
// now, whose PAC is built with o, signed by default under the ticket's key
// for alice at the ticket's authentication time.
func (k *kerberos) pacStep(o identitytest.PACOpts) pid.SignInStep {
	o.Service = "cifs/nas.test"
	o.AuthTime = k.clock.Now()
	return k.step(identitytest.TicketOpts{Client: "alice", PAC: k.realm.PAC(o)}, krbClient)
}

// RFC 18 §10's "PAC" clauses on the signature, at SignIn: a PAC whose server
// signature verifies under the key that decrypted the ticket, AES256 or
// AES128, is accepted,
// one holding no logon information as no PAC; every other is refused
// ErrLogonFailure with outcome logon_failure. Catches a corrupted or
// re-encoded signature accepted, an unkeyed or wrongly typed checksum
// accepted, the checksum tried against every held key, a PAC without logon
// information refused, and an AES128 service key's signature refused.
func TestKerberosSignInVerifiesThePAC(t *testing.T) {
	logon := []identitytest.LogonInfo{aliceLogon}
	corrupt := func(o identitytest.PACOpts, at func(b []byte) int) func(k *kerberos) pid.SignInStep {
		return func(k *kerberos) pid.SignInStep {
			o.Service, o.AuthTime = "cifs/nas.test", k.clock.Now()
			b := k.realm.PAC(o)
			b[at(b)] ^= 0x01
			return k.step(identitytest.TicketOpts{Client: "alice", PAC: b}, krbClient)
		}
	}
	plain := func(o identitytest.PACOpts) func(k *kerberos) pid.SignInStep {
		return func(k *kerberos) pid.SignInStep { return k.pacStep(o) }
	}
	for _, c := range []struct {
		name string
		step func(k *kerberos) pid.SignInStep
		ok   bool
	}{
		{"a PAC signed under the ticket's key", plain(identitytest.PACOpts{Logon: logon}), true},
		{"a PAC with no logon information, read as none", plain(identitytest.PACOpts{}), true},
		{"a PAC signed under the ticket's AES128 key", func(k *kerberos) pid.SignInStep {
			aes128 := int32(etypeID.AES128_CTS_HMAC_SHA1_96)
			o := identitytest.PACOpts{Service: "cifs/nas.test", Etype: aes128, AuthTime: k.clock.Now(), Logon: logon}
			return k.step(identitytest.TicketOpts{Client: "alice", Etype: aes128, PAC: k.realm.PAC(o)}, krbClient)
		}, true},
		{"a corrupted server signature", corrupt(identitytest.PACOpts{Logon: logon}, serverSignatureAt), false},
		{"a PAC changed only in padding between buffers", corrupt(identitytest.PACOpts{Logon: logon}, paddingAt), false},
		{"a server signature of the CRC32 type", plain(identitytest.PACOpts{Logon: logon, ServerType: chksumtype.CRC32}), false},
		{"a server signature of the AES128 type under an AES256 key",
			plain(identitytest.PACOpts{Logon: logon, ServerType: chksumtype.HMAC_SHA1_96_AES128}), false},
		{"a server signature of the right type with a 16-byte value", plain(identitytest.PACOpts{Logon: logon, ServerLen: 16}), false},
		{"a PAC signed under another held key, not the ticket's",
			plain(identitytest.PACOpts{Logon: logon, Key: kerberosKey("nfs/nas.test")}), false},
	} {
		t.Run(c.name, func(t *testing.T) {
			k := kerberosServer(t)
			alice := entity.User{Principal: entity.NewUser(), Name: "alice", IDs: fileIDs("1500")}
			k.user(t, alice, 0)
			res, oc, err := k.srv.SignInOutcome(context.Background(), c.step(k))
			if !c.ok {
				refused(t, c.name, res, oc, err, vfserr.ErrLogonFailure, "logon_failure")
				return
			}
			if err != nil || oc != "ok" || res.Identity.Evidence.Principal != alice.Principal {
				t.Errorf("SignIn = %v, outcome %q, bound %v; want ok, alice", err, oc, res.Identity.Evidence.Principal)
			}
		})
	}
}

// A PAC whose client information names another client, or another
// authentication time, than its ticket is refused, and the log line saying
// so carries neither name, which RFC 25 §5.4 keeps out of log lines.
// Catches the PAC's client information unchecked.
func TestKerberosSignInChecksThePACClient(t *testing.T) {
	for _, c := range []struct {
		name string
		opts func(k *kerberos) identitytest.PACOpts
	}{
		{"another client", func(k *kerberos) identitytest.PACOpts {
			return identitytest.PACOpts{Client: "mallory"}
		}},
		{"another time", func(k *kerberos) identitytest.PACOpts {
			return identitytest.PACOpts{AuthTime: k.clock.Now().Add(-1e9)}
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			k := kerberosServer(t)
			k.user(t, entity.User{Principal: entity.NewUser(), Name: "alice", IDs: fileIDs("1500")}, 0)
			o := c.opts(k)
			o.Service, o.Logon = "cifs/nas.test", []identitytest.LogonInfo{aliceLogon}
			if o.AuthTime.IsZero() {
				o.AuthTime = k.clock.Now()
			}
			step := k.step(identitytest.TicketOpts{Client: "alice", PAC: k.realm.PAC(o)}, krbClient)
			res, oc, err := k.srv.SignInOutcome(context.Background(), step)
			refused(t, c.name, res, oc, err, vfserr.ErrLogonFailure, "logon_failure")
			logs := k.logs.String()
			if !strings.Contains(logs, "a PAC's client information differs from its ticket's") {
				t.Errorf("logs %s, want the mismatch logged", logs)
			}
			for _, name := range []string{"alice", "mallory"} {
				if strings.Contains(logs, name) {
					t.Errorf("logs %s carry the name %s", logs, name)
				}
			}
		})
	}
}

// applyDirectory applies the fixture's keytab with a directory record of
// schema serving realm. No directory is dialed: a sign-in reaching step 3
// finds it unavailable.
func (k *kerberos) applyDirectory(t *testing.T, schema, realm string) {
	t.Helper()
	st := state(1)
	st.Settings.NetBIOSName = "NAS01"
	st.Keytab = []secrets.Version{{Number: 1, Value: k.realm.Keytab()}}
	st.Directory = &directory.Record{Schema: schema, Address: "ldaps://dc.test.realm", Realm: realm}
	if err := k.srv.Apply(st); err != nil {
		t.Fatalf("Apply: %v", err)
	}
}

// In the directory's realm, a ticket with no PAC and one whose verified PAC
// holds no logon information are refused ErrLogonFailure, counted no_pac,
// and one whose PAC verifies is refused ErrUnavailable while nothing checks
// its user SID against the record (RFC 18 §3.4); each would otherwise bind
// the local alice through her krb row. Catches a directory-realm ticket bound whatever
// its PAC holds.
func TestKerberosSignInDirectoryRealmPAC(t *testing.T) {
	foreign := aliceLogon
	foreign.DomainSID, foreign.UserRID = "S-1-5-21-9-9-9", 4242
	for _, c := range []struct {
		name    string
		step    func(k *kerberos) pid.SignInStep
		err     error
		outcome string
	}{
		{"no PAC", func(k *kerberos) pid.SignInStep { return k.as("alice") }, vfserr.ErrLogonFailure, "no_pac"},
		{"a PAC with no logon information", func(k *kerberos) pid.SignInStep { return k.pacStep(identitytest.PACOpts{}) },
			vfserr.ErrLogonFailure, "no_pac"},
		{"a PAC naming a foreign SID", func(k *kerberos) pid.SignInStep {
			return k.pacStep(identitytest.PACOpts{Logon: []identitytest.LogonInfo{foreign}})
		}, vfserr.ErrUnavailable, "unavailable"},
	} {
		t.Run(c.name, func(t *testing.T) {
			k := kerberosServer(t)
			k.user(t, entity.User{Principal: entity.NewUser(), Name: "alice", IDs: append(fileIDs("1500"), krbID("alice@TEST.REALM"))}, 0)
			k.applyDirectory(t, directory.SchemaAD, krbRealm)
			res, oc, err := k.srv.SignInOutcome(context.Background(), c.step(k))
			refused(t, c.name, res, oc, err, c.err, c.outcome)
		})
	}
}

// kerberosKey is the test realm's AES256 key of service, built as
// kerberosServer's realm builds it.
func kerberosKey(service string) types.EncryptionKey {
	return identitytest.NewRealm(krbRealm, "cifs/nas.test", "nfs/nas.test").Key(service, etypeID.AES256_CTS_HMAC_SHA1_96)
}

// pacEntry is the offset and size of b's first buffer of type typ, read from
// its buffer array (MS-PAC 2.4).
func pacEntry(b []byte, typ uint32) (off, size int) {
	for i := range int(binary.LittleEndian.Uint32(b)) {
		e := b[8+16*i:]
		if binary.LittleEndian.Uint32(e) == typ {
			return int(binary.LittleEndian.Uint64(e[8:])), int(binary.LittleEndian.Uint32(e[4:]))
		}
	}
	panic("no PAC buffer of the type")
}

// serverSignatureAt is the first byte of b's server signature value.
func serverSignatureAt(b []byte) int {
	off, _ := pacEntry(b, identitytest.PACServerSignature)
	return off + 4
}

// paddingAt is a byte of the padding after b's client information, whose
// 20 bytes for alice end short of the next 8-byte boundary.
func paddingAt(b []byte) int {
	off, size := pacEntry(b, identitytest.PACClientInfo)
	if size%8 == 0 {
		panic("the client information ends on a boundary")
	}
	return off + size
}
