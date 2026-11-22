//go:build ad_dc

package ad_dc_test

import (
	"encoding/json"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/jcmturner/gokrb5/v8/client"
	krb5config "github.com/jcmturner/gokrb5/v8/config"
	"github.com/jcmturner/gokrb5/v8/credentials"
	"github.com/jcmturner/gokrb5/v8/iana/adtype"
	"github.com/jcmturner/gokrb5/v8/keytab"
	"github.com/jcmturner/gokrb5/v8/messages"
	"github.com/jcmturner/gokrb5/v8/types"

	"github.com/cubbit/dittofs/pkg/auth/sid"
)

// PAC buffer types (MS-PAC 2.4) the captures are checked for.
const (
	pacLogonInfo       = 1
	pacServerSignature = 6
	pacKDCSignature    = 7
	pacClientInfo      = 10
	pacUPNDNSInfo      = 12
	pacAttributesInfo  = 17
	pacRequestor       = 18
)

// TestADPACCapture captures the PACs of alice's TGT and of her cifs/ service
// ticket, as the fixture's KDC issues them, and checks each verifies under the
// key that decrypts its ticket and carries the buffer types the PAC tests need:
// the TGT's PAC holds PAC_ATTRIBUTES_INFO and PAC_REQUESTOR, the service
// ticket's does not. It also builds alice's AP-REQ for cifs/ with gokrb5's
// client from the service ticket the KDC issues it, and checks the ticket
// decrypts under the service keytab.
//
// With DITTOFS_AD_DC_PAC_OUT naming a directory, it also writes the captures
// there with their keys: tgt.pac with krbtgt.keytab, service.pac with
// service.keytab, and alice.apreq with alice.json, the authenticator's time
// and alice's and her groups' SIDs as samba-tool reads them.
// capture-pacs.sh runs it so.
func TestADPACCapture(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping AD-DC integration test in short mode")
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not found, skipping AD-DC integration test")
	}

	dcIP := setupADDC(t)
	serviceKeytab := copyADKeytab(t)

	// alice signs in and fetches her cifs/ ticket inside the DC, with MIT's
	// tools, so the ccache keeps her TGT too; the krbtgt key is exported
	// beside it.
	script := `set -eu
printf '[libdefaults]\n default_realm = ` + adRealm + `\n dns_lookup_kdc = false\n[realms]\n ` + adRealm + ` = {\n  kdc = 127.0.0.1\n }\n' > /tmp/pac-krb5.conf
export KRB5_CONFIG=/tmp/pac-krb5.conf KRB5CCNAME=/tmp/pac.ccache
echo '` + adUserPass + `' | kinit ` + adUserAlice + `@` + adRealm + ` >/dev/null
kvno ` + adSMBSPNShort + ` >/dev/null
rm -f /tmp/krbtgt.keytab
samba-tool domain exportkeytab /tmp/krbtgt.keytab --principal=krbtgt/` + adRealm + `@` + adRealm + ` >/dev/null`
	if out, err := adDockerExec("sh", "-c", script); err != nil {
		t.Fatalf("capture alice's tickets in the DC: %v\n%s", err, out)
	}
	dir := t.TempDir()
	for _, f := range []string{"pac.ccache", "krbtgt.keytab"} {
		if out, err := adDockerCombined("cp", adContainerName+":/tmp/"+f, filepath.Join(dir, f)); err != nil {
			t.Fatalf("copy %s out of the DC: %v\n%s", f, err, out)
		}
	}

	cc, err := credentials.LoadCCache(filepath.Join(dir, "pac.ccache"))
	if err != nil {
		t.Fatalf("load alice's ccache: %v", err)
	}
	krbtgtKeytab := filepath.Join(dir, "krbtgt.keytab")
	svcKT := loadKeytab(t, serviceKeytab)
	captures := []struct {
		service, keytab, file string
		kt                    *keytab.Keytab
		want, absent          []uint32
	}{
		{"krbtgt", krbtgtKeytab, "tgt.pac", loadKeytab(t, krbtgtKeytab),
			[]uint32{pacLogonInfo, pacServerSignature, pacKDCSignature, pacClientInfo, pacUPNDNSInfo, pacAttributesInfo, pacRequestor}, nil},
		{"cifs", serviceKeytab, "service.pac", svcKT,
			[]uint32{pacLogonInfo, pacServerSignature, pacKDCSignature, pacClientInfo, pacUPNDNSInfo}, []uint32{pacAttributesInfo, pacRequestor}},
	}
	// files collects every capture, written only once all of them are made,
	// so a failed capture leaves the old testdata whole rather than mixed
	// with a new domain's.
	files := map[string][]byte{}
	for _, c := range captures {
		tkt := ccacheTicket(t, cc, c.kt, c.service)
		raw := rawPAC(t, tkt, c.service)
		isPAC, pac, err := tkt.GetPACType(c.kt, nil, log.New(io.Discard, "", 0))
		if !isPAC || err != nil {
			t.Fatalf("the %s ticket's PAC: present %v, verified under its key: %v, want present and verified", c.service, isPAC, err)
		}
		var got []uint32
		for _, b := range pac.Buffers {
			got = append(got, b.ULType)
		}
		for _, w := range c.want {
			if !slices.Contains(got, w) {
				t.Errorf("the %s ticket's PAC holds buffer types %v, want %d among them", c.service, got, w)
			}
		}
		for _, a := range c.absent {
			if slices.Contains(got, a) {
				t.Errorf("the %s ticket's PAC holds buffer types %v, want no %d", c.service, got, a)
			}
		}
		ktBytes, err := os.ReadFile(c.keytab)
		if err != nil {
			t.Fatalf("read the %s keytab: %v", c.service, err)
		}
		files[c.file] = raw
		files[map[string]string{"krbtgt": "krbtgt.keytab", "cifs": "service.keytab"}[c.service]] = ktBytes
	}

	req, ctime := aliceAPReq(t, dcIP, svcKT)
	out := os.Getenv("DITTOFS_AD_DC_PAC_OUT")
	if out == "" || t.Failed() {
		return
	}
	sids := map[string]string{"alice": sidString(objectSID(t, "user", adUserAlice))}
	for _, g := range []string{"Domain Users", "devs", "engineering"} {
		sids[g] = sidString(objectSID(t, "group", g))
	}
	meta, err := json.MarshalIndent(aliceCapture{Time: ctime, SIDs: sids}, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	files["alice.apreq"] = req
	files["alice.json"] = append(meta, '\n')
	for name, data := range files {
		if err := os.WriteFile(filepath.Join(out, name), data, 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
		t.Logf("wrote %s (%d bytes) to %s", name, len(data), out)
	}
}

// aliceCapture is alice.json: the time alice's captured AP-REQ was made, and
// her SID and her groups' by name.
type aliceCapture struct {
	Time time.Time         `json:"time"`
	SIDs map[string]string `json:"sids"`
}

// sidString formats s, which objectSID parsed from samba-tool's string, so
// a malformed objectSid fails the capture rather than being written.
func sidString(s sid.SID) string { return sid.FormatSID(&s) }

// aliceAPReq signs alice in with gokrb5's client against the DC at dcIP and
// returns her AP-REQ for cifs/, made from the service ticket the KDC issues,
// with its authenticator's time. The ticket must decrypt under kt.
func aliceAPReq(t *testing.T, dcIP string, kt *keytab.Keytab) ([]byte, time.Time) {
	t.Helper()
	cl := adLogin(t, writeADKrb5Conf(t, dcIP), adUserAlice, adUserPass)
	tkt, key := adTicket(t, cl, adSMBSPNShort)
	if err := tkt.DecryptEncPart(kt, nil); err != nil {
		t.Fatalf("decrypt alice's %s ticket under the service keytab: %v", adSMBSPNShort, err)
	}
	return adAPReq(t, cl, tkt, key)
}

// adLogin signs name in with password through gokrb5's client, under the
// krb5.conf at krb5Conf, and returns the client, destroyed at the test's end.
func adLogin(t *testing.T, krb5Conf, name, password string) *client.Client {
	t.Helper()
	cfg, err := krb5config.Load(krb5Conf)
	if err != nil {
		t.Fatal(err)
	}
	cl := client.NewWithPassword(name, adRealm, password, cfg, client.DisablePAFXFAST(true))
	if err := cl.Login(); err != nil {
		t.Fatalf("kinit as %s: %v", name, err)
	}
	t.Cleanup(cl.Destroy)
	return cl
}

// adTicket is cl's service ticket for spn and its session key.
func adTicket(t *testing.T, cl *client.Client, spn string) (messages.Ticket, types.EncryptionKey) {
	t.Helper()
	tkt, key, err := cl.GetServiceTicket(spn)
	if err != nil {
		t.Fatalf("service ticket for %s: %v", spn, err)
	}
	return tkt, key
}

// adAPReq is cl's AP-REQ presenting tkt with a fresh authenticator, and the
// authenticator's time.
func adAPReq(t *testing.T, cl *client.Client, tkt messages.Ticket, key types.EncryptionKey) ([]byte, time.Time) {
	t.Helper()
	auth, err := types.NewAuthenticator(cl.Credentials.Domain(), cl.Credentials.CName())
	if err != nil {
		t.Fatal(err)
	}
	req, err := messages.NewAPReq(tkt, key, auth)
	if err != nil {
		t.Fatalf("%s's AP-REQ: %v", cl.Credentials.UserName(), err)
	}
	b, err := req.Marshal()
	if err != nil {
		t.Fatalf("marshal %s's AP-REQ: %v", cl.Credentials.UserName(), err)
	}
	return b, auth.CTime.Truncate(time.Microsecond)
}

// loadKeytab loads the keytab at path.
func loadKeytab(t *testing.T, path string) *keytab.Keytab {
	t.Helper()
	kt, err := keytab.Load(path)
	if err != nil {
		t.Fatalf("load the keytab %s: %v", path, err)
	}
	return kt
}

// ccacheTicket returns the ccache's ticket for service, decrypted with kt.
func ccacheTicket(t *testing.T, cc *credentials.CCache, kt *keytab.Keytab, service string) *messages.Ticket {
	t.Helper()
	for _, cred := range cc.Credentials {
		if cred.Server.PrincipalName.NameString[0] != service {
			continue
		}
		var tkt messages.Ticket
		if err := tkt.Unmarshal(cred.Ticket); err != nil {
			t.Fatalf("unmarshal the %s ticket: %v", service, err)
		}
		if err := tkt.DecryptEncPart(kt, nil); err != nil {
			t.Fatalf("decrypt the %s ticket: %v", service, err)
		}
		return &tkt
	}
	t.Fatalf("alice's ccache holds no %s ticket", service)
	return nil
}

// rawPAC returns the PAC's bytes as the ticket carries them.
func rawPAC(t *testing.T, tkt *messages.Ticket, service string) []byte {
	t.Helper()
	for _, ad := range tkt.DecryptedEncPart.AuthorizationData {
		if ad.ADType != adtype.ADIfRelevant {
			continue
		}
		var inner types.AuthorizationData
		if err := inner.Unmarshal(ad.ADData); err != nil {
			t.Fatalf("unmarshal the %s ticket's AD-IF-RELEVANT: %v", service, err)
		}
		for _, e := range inner {
			if e.ADType == adtype.ADWin2KPAC {
				return e.ADData
			}
		}
	}
	t.Fatalf("the %s ticket carries no PAC", service)
	return nil
}
