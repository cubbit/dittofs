//go:build ad_dc

package ad_dc_test

import (
	"crypto/tls"
	"crypto/x509"
	"net"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/jcmturner/gokrb5/v8/client"
	krb5config "github.com/jcmturner/gokrb5/v8/config"
)

// TestADFixture checks the fixture's own promises, the ones the identity
// tests build on rather than test.
func TestADFixture(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping AD-DC integration test in short mode")
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not found, skipping AD-DC integration test")
	}

	dcIP, _, krb5ConfPath, cleanup := setupADDC(t)
	defer cleanup()

	// A client reaching the DC by its host name verifies its certificate
	// against the exported CA alone, with Go's default verification.
	t.Run("ldaps verified by host name", func(t *testing.T) {
		pem, err := os.ReadFile(copyADCA(t))
		if err != nil {
			t.Fatalf("read the fixture CA: %v", err)
		}
		roots := x509.NewCertPool()
		if !roots.AppendCertsFromPEM(pem) {
			t.Fatal("the fixture CA holds no PEM certificate")
		}
		conn, err := tls.DialWithDialer(&net.Dialer{Timeout: 30 * time.Second}, "tcp",
			net.JoinHostPort(dcIP, "636"), &tls.Config{RootCAs: roots, ServerName: adDCHost})
		if err != nil {
			t.Fatalf("LDAPS handshake verifying %s against the fixture CA: %v", adDCHost, err)
		}
		_ = conn.Close()
	})

	// The imported service names sit on an account of their own, not on
	// Administrator, and that account is AES only: the KDC refuses a service
	// ticket for them under RC4 and issues one under AES. DES is not tried:
	// Samba refuses it whatever the account allows, so that leg could not fail.
	t.Run("service names on an AES-only account", func(t *testing.T) {
		out, err := adDockerExec("samba-tool", "spn", "list", "Administrator")
		if err != nil {
			t.Fatalf("samba-tool spn list Administrator: %v\n%s", err, out)
		}
		if strings.Contains(strings.ToLower(string(out)), adSMBSPNShort) {
			t.Errorf("Administrator holds %s, want it on the service account only:\n%s", adSMBSPNShort, out)
		}

		for _, tc := range []struct {
			name    string
			etype   int32
			refused bool
		}{
			{"rc4-hmac", 23, true},
			{"aes256-cts", 18, false},
		} {
			cfg, err := krb5config.Load(krb5ConfPath)
			if err != nil {
				t.Fatalf("load krb5.conf: %v", err)
			}
			cl := client.NewWithPassword(adUserAlice, adRealm, adUserPass, cfg, client.DisablePAFXFAST(true))
			if err := cl.Login(); err != nil {
				t.Fatalf("kinit as alice: %v", err)
			}
			// The TGT is AES; only the service ticket's request names the etype.
			cfg.LibDefaults.DefaultTGSEnctypeIDs = []int32{tc.etype}
			_, key, err := cl.GetServiceTicket(adSMBSPNShort)
			cl.Destroy()
			switch {
			case tc.refused && err == nil:
				t.Errorf("%s service ticket for %s: issued with a session key of etype %d, want KDC_ERR_ETYPE_NOSUPP",
					tc.name, adSMBSPNShort, key.KeyType)
			case tc.refused && !strings.Contains(err.Error(), "KDC_ERR_ETYPE_NOSUPP"):
				t.Errorf("%s service ticket for %s: %v, want KDC_ERR_ETYPE_NOSUPP", tc.name, adSMBSPNShort, err)
			case !tc.refused && err != nil:
				t.Errorf("%s service ticket for %s: %v, want one issued", tc.name, adSMBSPNShort, err)
			case !tc.refused && key.KeyType != tc.etype:
				t.Errorf("%s service ticket for %s: session key etype %d, want %d", tc.name, adSMBSPNShort, key.KeyType, tc.etype)
			}
		}
	})
}
