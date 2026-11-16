//go:build ad_dc

package ad_dc_test

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"net"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/cubbit/dittofs/internal/auth/netlogon"
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

	dcIP, _, _, cleanup := setupADDC(t)
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

	// The computer accounts the joins start from exist with the passwords
	// handed over, and with the encryption types an offline join decides on.
	t.Run("pre-staged join accounts", func(t *testing.T) {
		for _, acct := range []struct {
			name, password, enctypes string
		}{
			{"dittofs", machineAccountPass, ""},
			{"offjoinaes", "OffJoinAES01!", "24"},
			{"offjoinlegacy", "OffJoinLegacy01!", ""},
		} {
			out, err := adDockerExec("samba-tool", "computer", "show", acct.name,
				"--attributes=msDS-SupportedEncryptionTypes")
			if err != nil {
				t.Fatalf("samba-tool computer show %s: %v\n%s", acct.name, err, out)
			}
			got := ""
			for _, line := range strings.Split(string(out), "\n") {
				if v, ok := strings.CutPrefix(line, "msDS-SupportedEncryptionTypes: "); ok {
					got = strings.TrimSpace(v)
				}
			}
			if got != acct.enctypes {
				t.Errorf("%s$ msDS-SupportedEncryptionTypes = %q, want %q", acct.name, got, acct.enctypes)
			}
			waitForMachinePassword(t, dcIP, acct.name, acct.password)
		}
	})

	// A restart keeps an account's password as it was last set, so a join
	// that set it is not undone by the fixture's next start.
	t.Run("restart keeps a machine password", func(t *testing.T) {
		const rotated = "RotatedMachinePass02!"
		if out, err := adDockerExec("samba-tool", "user", "setpassword", machineAccountName,
			"--newpassword="+rotated); err != nil {
			t.Fatalf("samba-tool setpassword %s: %v\n%s", machineAccountName, err, out)
		}
		// Drop the ready marker first, so the wait below sees only the one the
		// restarted entrypoint writes.
		if out, err := adDockerExec("rm", "-f", "/keytabs/.ready"); err != nil {
			t.Fatalf("drop the ready marker: %v\n%s", err, out)
		}
		if out, err := adDockerCombined("restart", adContainerName); err != nil {
			t.Fatalf("docker restart: %v\n%s", err, out)
		}
		waitForADReady(t, 5*time.Minute)
		waitForMachinePassword(t, getContainerIP(t), "dittofs", rotated)
	})
}

// waitForMachinePassword waits until the DC accepts a NETLOGON secure channel
// for the computer account name$ with password.
func waitForMachinePassword(t *testing.T, dcIP, name, password string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	a := netlogon.NewAuthenticator(netlogon.NewOfflineProvider(netlogon.MachineCredential{
		AccountName: strings.ToUpper(name) + "$",
		Password:    password,
		Workstation: strings.ToUpper(name),
		DomainName:  adDomain,
		Realm:       adRealm,
		DCAddresses: []string{dcIP},
	}))
	defer a.Close(ctx)
	waitForSecureChannel(t, ctx, a, time.Minute)
}

// waitForSecureChannel polls Probe until the DC accepts a secure channel for
// the provider's machine account, failing the test with the last error once
// timeout has passed.
func waitForSecureChannel(
	t *testing.T,
	ctx context.Context,
	auth *netlogon.Authenticator,
	timeout time.Duration,
) {
	t.Helper()

	deadline := time.Now().Add(timeout)

	for {
		err := auth.Probe(ctx)
		if err == nil {
			return
		}

		if time.Now().After(deadline) {
			dumpADLogs(t)
			t.Fatalf("the DC did not accept a secure channel for the joined account within %v: %v",
				timeout, err)
		}

		time.Sleep(250 * time.Millisecond)
	}
}
