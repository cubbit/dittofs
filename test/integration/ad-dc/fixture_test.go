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

	"github.com/jcmturner/gokrb5/v8/client"
	krb5config "github.com/jcmturner/gokrb5/v8/config"
	"github.com/jcmturner/gokrb5/v8/keytab"
	"github.com/oiweiwei/go-msrpc/dcerpc"
	"github.com/oiweiwei/go-msrpc/msrpc/dcetypes"
	"github.com/oiweiwei/go-msrpc/msrpc/dtyp"
	epm "github.com/oiweiwei/go-msrpc/msrpc/epm/epm/v3"
	logon "github.com/oiweiwei/go-msrpc/msrpc/nrpc/logon/v1"
	"github.com/oiweiwei/go-msrpc/ssp/credential"
	"github.com/oiweiwei/go-msrpc/ssp/ntlm"

	"github.com/cubbit/dittofs/internal/identity/directory"
	"github.com/cubbit/dittofs/internal/identity/netlogon"
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

	// The imported service names sit on an account of their own, not on
	// Administrator, and that account is AES only: the KDC refuses a service
	// ticket for them under RC4 and issues one under AES, and the exported
	// keytab holds no RC4 key for them. DES is not tried: Samba refuses it
	// whatever the account allows, so that leg could not fail.
	t.Run("service names on an AES-only account", func(t *testing.T) {
		krb5ConfPath := writeADKrb5Conf(t, dcIP)
		out, err := adDockerExec("samba-tool", "spn", "list", "Administrator")
		if err != nil {
			t.Fatalf("samba-tool spn list Administrator: %v\n%s", err, out)
		}
		for _, spn := range []string{adSMBSPNShort, adNFSSPNShort} {
			if strings.Contains(strings.ToLower(string(out)), spn) {
				t.Errorf("Administrator holds %s, want it on the service account only:\n%s", spn, out)
			}
		}

		for _, tc := range []struct {
			spn     string
			name    string
			etype   int32
			refused bool
		}{
			{adSMBSPNShort, "rc4-hmac", 23, true},
			{adSMBSPNShort, "aes256-cts", 18, false},
			{adNFSSPNShort, "rc4-hmac", 23, true},
			{adNFSSPNShort, "aes256-cts", 18, false},
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
			_, key, err := cl.GetServiceTicket(tc.spn)
			cl.Destroy()
			switch {
			case tc.refused && err == nil:
				t.Errorf("%s service ticket for %s: issued with a session key of etype %d, want KDC_ERR_ETYPE_NOSUPP",
					tc.name, tc.spn, key.KeyType)
			case tc.refused && !strings.Contains(err.Error(), "KDC_ERR_ETYPE_NOSUPP"):
				t.Errorf("%s service ticket for %s: %v, want KDC_ERR_ETYPE_NOSUPP", tc.name, tc.spn, err)
			case !tc.refused && err != nil:
				t.Errorf("%s service ticket for %s: %v, want one issued", tc.name, tc.spn, err)
			case !tc.refused && key.KeyType != tc.etype:
				t.Errorf("%s service ticket for %s: session key etype %d, want %d", tc.name, tc.spn, key.KeyType, tc.etype)
			}
		}

		// The account keeps an RC4 key, its NT hash, whatever its
		// msDS-SupportedEncryptionTypes; exportkeytab leaves it out of a
		// service name's export only while that attribute excludes RC4.
		kt, err := keytab.Load(copyADKeytab(t))
		if err != nil {
			t.Fatalf("load the exported keytab: %v", err)
		}
		seen := map[string]bool{}
		for _, e := range kt.Entries {
			spn := strings.ToLower(strings.Join(e.Principal.Components, "/"))
			seen[spn] = true
			if e.Key.KeyType != 17 && e.Key.KeyType != 18 {
				t.Errorf("keytab entry for %s has etype %d, want 17 or 18 (AES) only", spn, e.Key.KeyType)
			}
		}
		for _, spn := range []string{adSMBSPNShort, adNFSSPNShort} {
			if !seen[spn] {
				t.Errorf("keytab holds no entry for %s", spn)
			}
		}
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
	// handed over, the names, the encryption types an offline join decides on,
	// and the owner and delegation a join reports. offjoinowned$ alone is owned
	// by the user who created it, not Domain Admins, and alone lets another
	// account act on its behalf.
	t.Run("pre-staged join accounts", func(t *testing.T) {
		joinerSID := adAttrs(t, "user", "joiner", "objectSid")["objectSid"]
		if joinerSID == "" {
			t.Fatal("joiner has no objectSid")
		}
		dittofsSID := adAttrs(t, "computer", "dittofs", "objectSid")["objectSid"]
		if dittofsSID == "" {
			t.Fatal("dittofs$ has no objectSid")
		}
		for _, acct := range []struct {
			name, password, enctypes, owner string
			delegated                       bool
		}{
			{"dittofs", machineAccountPass, "", "DA", false},
			{"offjoinaes", "OffJoinAES01!", "24", "DA", false},
			{"offjoinlegacy", "OffJoinLegacy01!", "", "DA", false},
			{"offjoinowned", "OffJoinOwned01!", "24", joinerSID, true},
			{"BUILD01", "Build01Pass01!", "", "DA", false},
		} {
			const delegation = "msDS-AllowedToActOnBehalfOfOtherIdentity"
			attrs := adAttrs(t, "computer", acct.name,
				"sAMAccountName", "msDS-SupportedEncryptionTypes", delegation)
			if got, want := attrs["sAMAccountName"], acct.name+"$"; got != want {
				t.Errorf("%s$ sAMAccountName = %q, want %q", acct.name, got, want)
			}
			if got := attrs["msDS-SupportedEncryptionTypes"]; got != acct.enctypes {
				t.Errorf("%s$ msDS-SupportedEncryptionTypes = %q, want %q", acct.name, got, acct.enctypes)
			}
			if got := attrs[delegation] != ""; got != acct.delegated {
				t.Errorf("%s$ %s set = %v (%q), want %v", acct.name, delegation, got, attrs[delegation], acct.delegated)
			}
			if acct.delegated && !strings.Contains(attrs[delegation], dittofsSID) {
				t.Errorf("%s$ %s = %q, want it naming dittofs$ (%s)", acct.name, delegation, attrs[delegation], dittofsSID)
			}
			if got := adOwner(t, attrs["dn"]); got != acct.owner {
				t.Errorf("%s$ owner = %q, want %q", acct.name, got, acct.owner)
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
		dcIP = getContainerIP(t) // a restart can change the container's IP
		waitForMachinePassword(t, dcIP, "dittofs", rotated)

		// Put DITTOFS$ back, so a later subtest can sign in with machineAccountPass.
		rotateMachinePassword(t, machineAccountPass)
		waitForMachinePassword(t, dcIP, "dittofs", machineAccountPass)
	})
}

// machineAccountName is the computer account entrypoint.sh creates with
// "samba-tool computer create dittofs --computerpassword=MachinePass01!".
const (
	machineAccountName = "DITTOFS$"
	machineAccountPass = "MachinePass01!"
)

// fixtureRecord is the directory record of the fixture's domain, as the
// secure channel reads it.
func fixtureRecord() directory.Record {
	return directory.Record{Realm: adRealm, NetBIOSDomain: adDomain}
}

// waitForMachinePassword waits up to a minute for the DC to accept a NETLOGON
// secure channel for the computer account name$ with password.
func waitForMachinePassword(t *testing.T, dcIP, name, password string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	d := netlogon.NewDialer((&net.Dialer{}).DialContext)
	for {
		err := channelOnce(ctx, d, dcIP, name, password)
		if err == nil {
			return
		}
		if ctx.Err() != nil {
			dumpADLogs(t)
			t.Fatalf("the DC did not accept a secure channel for %s$ within a minute: %v", name, err)
		}
		// Polls the DC's own answer; the wait is bounded by ctx.
		time.Sleep(250 * time.Millisecond)
	}
}

// channelOnce sets up and closes one secure channel as name$.
func channelOnce(ctx context.Context, d netlogon.Dialer, dcIP, name, password string) error {
	dc, err := d.Dial(ctx, fixtureRecord(), dcIP)
	if err != nil {
		return err
	}
	defer func() { _ = dc.Close() }()
	ch, err := dc.Channel(ctx, name, []byte(password))
	if err != nil {
		return err
	}
	return ch.Close()
}

// rotateMachinePassword sets DITTOFS$'s password on the DC with samba-tool.
func rotateMachinePassword(t *testing.T, newPassword string) {
	t.Helper()
	out, err := adDockerExec("samba-tool", "user", "setpassword", machineAccountName,
		"--newpassword="+newPassword)
	if err != nil {
		t.Fatalf("samba-tool setpassword %s failed: %v\n%s", machineAccountName, err, out)
	}
}

// ntlmResponseFor computes a deterministic NTLMv2 NT/LM response for the
// AD user against a fixed 8-byte server challenge.
func ntlmResponseFor(ctx context.Context, t *testing.T, user, pass string, serverChallenge [8]byte) (nt, lm []byte) {
	t.Helper()
	clientNonce := []byte{0xAA, 0xAA, 0xAA, 0xAA, 0xAA, 0xAA, 0xAA, 0xAA}
	v2 := &ntlm.V2{Config: &ntlm.Config{}}
	cm := &ntlm.ChallengeMessage{
		ServerChallenge: serverChallenge[:],
		TargetInfo: ntlm.AttrValues{
			ntlm.AttrNetBIOSDomainName:   &ntlm.Value{NetBIOSDomainName: adDomain},
			ntlm.AttrNetBIOSComputerName: &ntlm.Value{NetBIOSComputerName: adDomain},
		},
	}
	cred := credential.NewFromPassword(adDomain+"\\"+user, pass)
	resp, err := v2.ChallengeResponse(ctx, cred, cm, clientNonce)
	if err != nil {
		t.Fatalf("compute NTLMv2 ChallengeResponse for %q: %v", user, err)
	}
	return resp.NT, resp.LM
}

// adAttrs reads attributes of a samba-tool user or computer, with "dn" among
// them, joining the LDIF's folded lines. An attribute the object lacks is "".
func adAttrs(t *testing.T, kind, name string, attrs ...string) map[string]string {
	t.Helper()
	out, err := adDockerExec("samba-tool", kind, "show", name, "--attributes="+strings.Join(attrs, ","))
	if err != nil {
		t.Fatalf("samba-tool %s show %s: %v\n%s", kind, name, err, out)
	}
	got := map[string]string{}
	last := ""
	for _, line := range strings.Split(string(out), "\n") {
		if v, ok := strings.CutPrefix(line, " "); ok && last != "" {
			got[last] += v
			continue
		}
		if k, v, ok := strings.Cut(line, ": "); ok {
			last = k
			got[k] = strings.TrimSpace(v)
		}
	}
	return got
}

// adOwner returns the owner of the object at dn as its descriptor's SDDL
// names it: an alias such as DA for a well-known group, a SID otherwise.
func adOwner(t *testing.T, dn string) string {
	t.Helper()
	out, err := adDockerExec("samba-tool", "dsacl", "get", "--objectdn="+dn)
	if err != nil {
		t.Fatalf("samba-tool dsacl get %s: %v\n%s", dn, err, out)
	}
	for _, line := range strings.Split(string(out), "\n") {
		if rest, ok := strings.CutPrefix(line, "O:"); ok {
			owner, _, _ := strings.Cut(rest, "G:")
			return owner
		}
	}
	t.Fatalf("samba-tool dsacl get %s printed no owner:\n%s", dn, out)
	return ""
}
