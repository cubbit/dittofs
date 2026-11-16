//go:build ad_dc

package ad_dc_test

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/oiweiwei/go-msrpc/dcerpc"
	"github.com/oiweiwei/go-msrpc/msrpc/dcetypes"
	"github.com/oiweiwei/go-msrpc/msrpc/dtyp"
	epm "github.com/oiweiwei/go-msrpc/msrpc/epm/epm/v3"
	logon "github.com/oiweiwei/go-msrpc/msrpc/nrpc/logon/v1"

	"github.com/cubbit/dittofs/internal/auth/netlogon"
)

// The DC's dynamic RPC port range, which setupADDC hands entrypoint.sh as
// RPC_PORT_RANGE.
const (
	adRPCPortLow  = 50000
	adRPCPortHigh = 50019
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

	// The endpoint mapper hands out NETLOGON on ncacn_ip_tcp from the pinned
	// range, the one a firewall in front of the DC opens.
	t.Run("netlogon on a pinned rpc port", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		cc, err := dcerpc.Dial(ctx, "ncacn_ip_tcp:"+dcIP+"[135]")
		if err != nil {
			t.Fatalf("dial the endpoint mapper: %v", err)
		}
		defer func() { _ = cc.Close(ctx) }()
		cli, err := epm.NewEpmClient(ctx, cc, dcerpc.WithInsecure())
		if err != nil {
			t.Fatalf("bind the endpoint mapper: %v", err)
		}
		// Samba answers with every endpoint it holds whatever the inquiry, so
		// NETLOGON's are picked out by the interface each tower names.
		resp, err := cli.Lookup(ctx, &epm.LookupRequest{
			InquiryType: 1, // RPC_C_EP_MATCH_BY_IF
			InterfaceID: &dcetypes.InterfaceID{UUID: dtyp.GUIDFromUUID(logon.LogonSyntaxUUID), VersMajor: 1},
			MaxEntries:  500,
		})
		if err != nil {
			t.Fatalf("endpoint mapper lookup: %v", err)
		}
		found := 0
		for _, e := range resp.Entries {
			b := e.Tower.Binding()
			if b.SyntaxID.IfUUID.String() != logon.LogonSyntaxUUID.String() ||
				b.StringBinding.ProtocolSequence != dcerpc.ProtocolSequenceIPTCP {
				continue
			}
			found++
			port, err := strconv.Atoi(b.StringBinding.Endpoint)
			if err != nil || port < adRPCPortLow || port > adRPCPortHigh {
				t.Errorf("NETLOGON on ncacn_ip_tcp port %q, want one in %d-%d",
					b.StringBinding.Endpoint, adRPCPortLow, adRPCPortHigh)
			}
		}
		if found == 0 {
			t.Errorf("the endpoint mapper holds no ncacn_ip_tcp endpoint for NETLOGON")
		}
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
		rotateMachinePassword(t, rotated)
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

// waitForMachinePassword waits up to a minute for the DC to accept a NETLOGON
// secure channel for the computer account name$ with password.
func waitForMachinePassword(t *testing.T, dcIP, name, password string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
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
	for {
		err := a.Probe(ctx)
		if err == nil {
			return
		}
		if ctx.Err() != nil {
			dumpADLogs(t)
			t.Fatalf("the DC did not accept a secure channel for %s$ within a minute: %v", name, err)
		}
		time.Sleep(250 * time.Millisecond)
	}
}
