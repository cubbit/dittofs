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

	dcIP := setupADDC(t)

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

	// The DC's smb.conf names the certificate files the entrypoint writes, so
	// a Samba whose default TLS location moves still serves them.
	t.Run("smb.conf names the TLS files", func(t *testing.T) {
		for param, want := range map[string]string{
			"tls certfile": "/var/lib/samba/private/tls/cert.pem",
			"tls keyfile":  "/var/lib/samba/private/tls/key.pem",
			"tls cafile":   "/var/lib/samba/private/tls/ca.pem",
		} {
			out, err := adDockerExec("testparm", "-s", "--parameter-name", param)
			if err != nil {
				t.Fatalf("testparm %q: %v\n%s", param, err, out)
			}
			if got := lastLine(string(out)); got != want {
				t.Errorf("smb.conf %q = %q, want %q", param, got, want)
			}
		}
	})
}

// lastLine returns the last non-empty line of out, trimmed: testparm prints
// its warnings before the value.
func lastLine(out string) string {
	lines := strings.Split(strings.TrimSpace(out), "\n")
	return strings.TrimSpace(lines[len(lines)-1])
}
