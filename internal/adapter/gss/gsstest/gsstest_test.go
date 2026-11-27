package gsstest_test

import (
	"net/netip"
	"testing"
	"time"

	"github.com/jcmturner/gokrb5/v8/iana/etypeID"
	"github.com/jcmturner/gokrb5/v8/types"

	"github.com/cubbit/dittofs/internal/adapter/gss"
	"github.com/cubbit/dittofs/internal/adapter/gss/gsstest"
	"github.com/cubbit/dittofs/pkg/identity/identitytest"
	"github.com/cubbit/dittofs/pkg/vfs"
	"github.com/cubbit/dittofs/pkg/vfs/vfstest/memory"
)

// The initiator is done only on the AP-REP answering its own AP-REQ: one
// answering another AP-REQ under the same session key, and one under another
// session key, are refused and leave no context. Catches an AP-REP accepted
// without its authenticator's time checked, or without decrypting it.
func TestKerberosInitiatorChecksTheAPRep(t *testing.T) {
	realm := identitytest.NewRealm("TEST.REALM", "nfs/nas.test")
	svc := memory.WithKeytab(t, realm.Keytab())
	step := vfs.SignInStep{Service: vfs.ServiceNFS, Transport: vfs.TransportNFS, Client: netip.MustParseAddr("192.0.2.9")}
	session := identitytest.NewKey(etypeID.AES256_CTS_HMAC_SHA1_96)
	// mint is the initiator of an AP-REQ from admin under session key, its
	// authenticator at cusec microseconds.
	mint := func(cusec int, key types.EncryptionKey) *gsstest.KerberosInitiator {
		o := identitytest.TicketOpts{Client: "admin", Service: "nfs/nas.test", Now: time.Unix(0, 0), Cusec: cusec, Session: key}
		init, err := gsstest.Kerberos(realm.Ticket(o), key)
		if err != nil {
			t.Fatal(err)
		}
		return init
	}
	// reply is the acceptor's AP-REP to init's AP-REQ.
	reply := func(init *gsstest.KerberosInitiator) []byte {
		tok, _, err := init.Step(t.Context(), nil)
		if err != nil {
			t.Fatal(err)
		}
		out, done, err := gss.Kerberos(svc, step).Step(t.Context(), tok)
		if err != nil || !done {
			t.Fatalf("the acceptor's Step = done %v, %v; want done", done, err)
		}
		return out
	}
	own := mint(1, session)
	for _, tc := range []struct {
		name  string
		other *gsstest.KerberosInitiator
	}{
		{name: "another authenticator's time", other: mint(2, session)},
		{name: "another session key", other: mint(3, identitytest.NewKey(etypeID.AES256_CTS_HMAC_SHA1_96))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			init := mint(1, session)
			if _, _, err := init.Step(t.Context(), nil); err != nil {
				t.Fatal(err)
			}
			if _, done, err := init.Step(t.Context(), reply(tc.other)); err == nil || done || init.Context() != nil {
				t.Errorf("the initiator's check of an AP-REP to another AP-REQ = done %v, %v, context %+v; want refused", done, err, init.Context())
			}
		})
	}
	t.Run("its own", func(t *testing.T) {
		if _, done, err := own.Step(t.Context(), reply(own)); err != nil || !done || own.Context() == nil {
			t.Errorf("the initiator's check of its own AP-REP = done %v, %v; want done with a context", done, err)
		}
	})
}
