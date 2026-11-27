package identitytest_test

import (
	"fmt"
	"io"
	"log"
	"slices"
	"testing"
	"time"

	"github.com/jcmturner/gokrb5/v8/iana/etypeID"
	"github.com/jcmturner/gokrb5/v8/iana/flags"
	"github.com/jcmturner/gokrb5/v8/keytab"
	"github.com/jcmturner/gokrb5/v8/messages"
	"github.com/jcmturner/gokrb5/v8/pac"
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

// A PAC the realm builds verifies under gokrb5's PAC verifier, which shares
// no code with the builder, and its logon information decodes, by gokrb5's
// NDR decoder, to the user and the groups it was built with.
func TestRealmPACVerifiesUnderGokrb5(t *testing.T) {
	r := identitytest.NewRealm("TEST.REALM", "cifs/nas.test.realm")
	key := r.Key("cifs/nas.test.realm", etypeID.AES256_CTS_HMAC_SHA1_96)
	now := time.Date(2031, 1, 2, 3, 4, 5, 0, time.UTC)
	b := r.PAC(identitytest.PACOpts{Service: "cifs/nas.test.realm", Client: "alice", AuthTime: now, Logon: []identitytest.LogonInfo{{
		Name: "alice", Domain: "CORP", DomainSID: "S-1-5-21-1-2-3", UserRID: 1105,
		Groups:         []identitytest.GroupRID{{RID: 513, Attributes: 7}, {RID: 1104, Attributes: 7}},
		ExtraSIDs:      []identitytest.SIDAttrs{{SID: "S-1-5-21-9-9-9-1200", Attributes: 0x20000007}, {SID: "S-1-18-1", Attributes: 7}},
		ResourceDomain: "S-1-5-21-4-5-6", ResourceGroups: []identitytest.GroupRID{{RID: 1300, Attributes: 0x20000007}},
	}}})
	var p pac.PACType
	if err := p.Unmarshal(b); err != nil {
		t.Fatalf("gokrb5 does not decode the PAC: %v", err)
	}
	if err := p.ProcessPACInfoBuffers(key, log.New(io.Discard, "", 0)); err != nil {
		t.Fatalf("gokrb5 does not verify the PAC under the service key: %v", err)
	}
	v := p.KerbValidationInfo
	if got := fmt.Sprintf("%s-%d", v.LogonDomainID.String(), v.UserID); got != "S-1-5-21-1-2-3-1105" {
		t.Errorf("user SID = %s, want S-1-5-21-1-2-3-1105", got)
	}
	want := []string{"S-1-5-21-1-2-3-513", "S-1-5-21-1-2-3-1104", "S-1-5-21-9-9-9-1200", "S-1-18-1", "S-1-5-21-4-5-6-1300"}
	if got := v.GetGroupMembershipSIDs(); !slices.Equal(got, want) {
		t.Errorf("group SIDs = %v, want %v", got, want)
	}
	if v.EffectiveName.Value != "alice" || v.LogonDomainName.Value != "CORP" {
		t.Errorf("names = %q, %q, want alice, CORP", v.EffectiveName.Value, v.LogonDomainName.Value)
	}
	if v.ExtraSIDs[0].Attributes != 0x20000007 || v.ResourceGroupIDs[0].Attributes != 0x20000007 {
		t.Errorf("attributes = %#x, %#x, want 0x20000007 each", v.ExtraSIDs[0].Attributes, v.ResourceGroupIDs[0].Attributes)
	}
	if p.ClientInfo.Name != "alice" || !p.ClientInfo.ClientID.Time().Equal(now) {
		t.Errorf("client information = %q at %v, want alice at %v", p.ClientInfo.Name, p.ClientInfo.ClientID.Time(), now)
	}
}
