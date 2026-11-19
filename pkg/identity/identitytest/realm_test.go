package identitytest_test

import (
	"testing"
	"time"

	"github.com/jcmturner/gokrb5/v8/iana/etypeID"
	"github.com/jcmturner/gokrb5/v8/iana/flags"
	"github.com/jcmturner/gokrb5/v8/keytab"
	"github.com/jcmturner/gokrb5/v8/messages"
	"github.com/jcmturner/gokrb5/v8/types"

	"github.com/cubbit/dittofs/pkg/identity/identitytest"
)

// The realm's keytab holds AES keys only, one of each AES type per service,
// so a verifier reading it never finds an RC4 or DES key.
func TestRealmKeytabHoldsAESOnly(t *testing.T) {
	r := identitytest.NewRealm("TEST.REALM", "cifs/nas.test.realm", "nfs/nas.test.realm")
	kt := keytab.New()
	if err := kt.Unmarshal(r.Keytab()); err != nil {
		t.Fatalf("Keytab does not decode: %v", err)
	}
	seen := map[string]map[int32]bool{}
	for _, e := range kt.Entries {
		et := e.Key.KeyType
		if et != etypeID.AES128_CTS_HMAC_SHA1_96 && et != etypeID.AES256_CTS_HMAC_SHA1_96 {
			t.Errorf("keytab entry for %v has etype %d, want AES only", e.Principal.Components, et)
		}
		name := e.Principal.Components[0] + "/" + e.Principal.Components[1]
		if seen[name] == nil {
			seen[name] = map[int32]bool{}
		}
		seen[name][et] = true
	}
	for _, svc := range []string{"cifs/nas.test.realm", "nfs/nas.test.realm"} {
		if !seen[svc][etypeID.AES128_CTS_HMAC_SHA1_96] || !seen[svc][etypeID.AES256_CTS_HMAC_SHA1_96] {
			t.Errorf("keytab keys for %s = %v, want both AES types", svc, seen[svc])
		}
	}
}

// A ticket decrypts under the realm's key for its service and carries what
// the options asked for: its client and its flags.
func TestRealmTicketCarriesItsOptions(t *testing.T) {
	r := identitytest.NewRealm("TEST.REALM", "cifs/nas.test.realm")
	now := time.Date(2031, 1, 2, 3, 4, 5, 0, time.UTC)
	b := r.Ticket(identitytest.TicketOpts{Client: "bob", Service: "cifs/nas.test.realm", Now: now, Flags: []int{flags.Invalid}})
	var req messages.APReq
	if err := req.Unmarshal(b); err != nil {
		t.Fatalf("Ticket is not an AP-REQ: %v", err)
	}
	key := r.Key("cifs/nas.test.realm", etypeID.AES256_CTS_HMAC_SHA1_96)
	if err := req.Ticket.Decrypt(key); err != nil {
		t.Fatalf("ticket does not decrypt under the service's AES256 key: %v", err)
	}
	if got := req.Ticket.DecryptedEncPart.CName.PrincipalNameString(); got != "bob" {
		t.Errorf("ticket client = %q, want bob", got)
	}
	if !types.IsFlagSet(&req.Ticket.DecryptedEncPart.Flags, flags.Invalid) {
		t.Errorf("ticket flags %v lack INVALID, which the options set", req.Ticket.DecryptedEncPart.Flags)
	}
	if !req.Ticket.DecryptedEncPart.AuthTime.Equal(now) {
		t.Errorf("ticket auth time = %v, want %v", req.Ticket.DecryptedEncPart.AuthTime, now)
	}
}
