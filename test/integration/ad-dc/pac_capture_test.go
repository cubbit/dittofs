//go:build ad_dc

package ad_dc_test

import (
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"

	"github.com/jcmturner/gokrb5/v8/credentials"
	"github.com/jcmturner/gokrb5/v8/iana/adtype"
	"github.com/jcmturner/gokrb5/v8/keytab"
	"github.com/jcmturner/gokrb5/v8/messages"
	"github.com/jcmturner/gokrb5/v8/types"
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
// ticket's does not.
//
// With DITTOFS_AD_DC_PAC_OUT naming a directory, it also writes the captures
// there with their keys: tgt.pac with krbtgt.keytab, service.pac with
// service.keytab. capture-pacs.sh runs it so.
func TestADPACCapture(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping AD-DC integration test in short mode")
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not found, skipping AD-DC integration test")
	}

	setupADDC(t)
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
	captures := []struct {
		service, keytab, file string
		want, absent          []uint32
	}{
		{"krbtgt", filepath.Join(dir, "krbtgt.keytab"), "tgt.pac",
			[]uint32{pacLogonInfo, pacServerSignature, pacKDCSignature, pacClientInfo, pacUPNDNSInfo, pacAttributesInfo, pacRequestor}, nil},
		{"cifs", serviceKeytab, "service.pac",
			[]uint32{pacLogonInfo, pacServerSignature, pacKDCSignature, pacClientInfo, pacUPNDNSInfo}, []uint32{pacAttributesInfo, pacRequestor}},
	}
	out := os.Getenv("DITTOFS_AD_DC_PAC_OUT")
	for _, c := range captures {
		kt, err := keytab.Load(c.keytab)
		if err != nil {
			t.Fatalf("load the %s keytab: %v", c.service, err)
		}
		tkt := ccacheTicket(t, cc, kt, c.service)
		raw := rawPAC(t, tkt, c.service)
		isPAC, pac, err := tkt.GetPACType(kt, nil, log.New(io.Discard, "", 0))
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

		if out == "" {
			continue
		}
		ktBytes, err := os.ReadFile(c.keytab)
		if err != nil {
			t.Fatalf("read the %s keytab: %v", c.service, err)
		}
		ktName := map[string]string{"krbtgt": "krbtgt.keytab", "cifs": "service.keytab"}[c.service]
		for name, data := range map[string][]byte{c.file: raw, ktName: ktBytes} {
			if err := os.WriteFile(filepath.Join(out, name), data, 0o644); err != nil {
				t.Fatalf("write %s: %v", name, err)
			}
		}
		t.Logf("wrote %s (%d bytes) and %s to %s", c.file, len(raw), ktName, out)
	}
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
