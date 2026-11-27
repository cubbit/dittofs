//go:build ad_dc

package ad_dc_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf16"

	"github.com/go-ldap/ldap/v3"
	"github.com/jcmturner/gokrb5/v8/client"
	krb5config "github.com/jcmturner/gokrb5/v8/config"
	"github.com/oiweiwei/go-msrpc/dcerpc"
	"github.com/oiweiwei/go-msrpc/msrpc/dcetypes"
	"github.com/oiweiwei/go-msrpc/msrpc/dtyp"
	epm "github.com/oiweiwei/go-msrpc/msrpc/epm/epm/v3"
	logon "github.com/oiweiwei/go-msrpc/msrpc/nrpc/logon/v1"
	"github.com/oiweiwei/go-msrpc/ssp"
	"github.com/oiweiwei/go-msrpc/ssp/credential"
	"github.com/oiweiwei/go-msrpc/ssp/gssapi"
	nlssp "github.com/oiweiwei/go-msrpc/ssp/netlogon"
)

// TestNetlogonTCPSchannel asks the DC whether NETLOGON works on ncacn_ip_tcp
// with a schannel-sealed binding and no SMB session: the endpoint mapper names
// the port, an unauthenticated association runs the challenge and
// NetrServerAuthenticate3 with AES and no RC4, and a second association binds
// with the Netlogon credential, sealed, for NetrLogonSamLogonEx and
// NetrServerPasswordSet2. Every dial runs from the host, outside the fixture's
// Docker network, so ports 135 and the pinned dynamic range are the ones a
// firewall in front of the DC opens.
func TestNetlogonTCPSchannel(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping AD-DC integration test in short mode")
	}
	dcIP := setupADDC(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	waitForMachinePassword(t, dcIP, "dittofs", machineAccountPass)

	port := netlogonTCPPort(ctx, t, dcIP)
	binding := "ncacn_ip_tcp:" + dcIP + "[" + port + "]"
	t.Logf("NETLOGON on %s", binding)
	machine := credential.NewFromPassword(machineAccountName, machineAccountPass,
		credential.Workstation("DITTOFS"), credential.Domain(adRealm))
	const logonServer = `\\DC1`

	logonReq := func(t *testing.T) *logon.SAMLogonExRequest {
		challenge := [8]byte{1, 2, 3, 4, 5, 6, 7, 8}
		nt, lm := ntlmResponseFor(ctx, t, strings.TrimSuffix(machineAccountName, "$"), adUserAlice, adUserPass, challenge)
		return &logon.SAMLogonExRequest{
			LogonServer: logonServer, ComputerName: "DITTOFS",
			LogonLevel: logon.LogonInfoClassNetworkTransitiveInformation,
			LogonInformation: &logon.Level{Value: &logon.Level_LogonNetworkTransitive{
				LogonNetworkTransitive: &logon.NetworkInfo{
					Identity: &logon.LogonIdentityInfo{
						ParameterControl: logon.IdentityAllowWorkstationTrustAccount,
						LogonDomainName:  &dtyp.UnicodeString{Buffer: adDomain},
						UserName:         &dtyp.UnicodeString{Buffer: adUserAlice},
						Workstation:      &dtyp.UnicodeString{Buffer: "DITTOFS"},
					},
					LMChallenge:         &logon.LMChallenge{Data: challenge[:]},
					NTChallengeResponse: &logon.String{Buffer: nt},
					LMChallengeResponse: &logon.String{Buffer: lm},
				},
			}},
			ValidationLevel: logon.ValidationInfoClassSAMInfo4,
		}
	}

	// go-msrpc's own helper: an NTLM-sealed bind, then an AlterContext to
	// schannel on the same association, the sequence securechannel.go records
	// Samba refusing on ncacn_ip_tcp; this Samba accepts it. The helper asks
	// for RC4 among its capabilities, which D1 takes out, so the channel below
	// is built from ssp/netlogon's parts.
	t.Run("one association upgraded by alter context", func(t *testing.T) {
		// The helper finds its mechanisms in go-msrpc's process-wide store,
		// which panics on a second registration; nothing else registers them.
		registerMechanisms.Do(func() {
			gssapi.AddMechanism(ssp.SPNEGO)
			gssapi.AddMechanism(ssp.NTLM)
			gssapi.AddMechanism(ssp.Netlogon)
		})
		gctx := gssapi.NewSecurityContext(ctx)
		cc, err := dcerpc.Dial(gctx, binding, dcerpc.WithCredentials(machine))
		if err != nil {
			t.Fatalf("dial %s: %v", binding, err)
		}
		defer func() { _ = cc.Close(gctx) }()
		cli, err := logon.NewSecureChannelClient(gctx, cc, dcerpc.WithSeal(), dcerpc.WithCredentials(machine))
		if err != nil {
			t.Fatalf("logon.NewSecureChannelClient on ncacn_ip_tcp: %v", err)
		}
		out, err := cli.SAMLogonEx(gctx, logonReq(t))
		if err != nil || out.Return != 0 {
			t.Fatalf("NetrLogonSamLogonEx over the upgraded association: err=%v status=0x%08x", err, uint32(samReturn(out)))
		}
		t.Log("logon.NewSecureChannelClient on ncacn_ip_tcp: bound, upgraded and signed alice in")
	})

	// Before the channel below, so the channels these set up are superseded
	// by its challenge, and before the password rotation at its end.
	t.Run("zeroed reply refused", func(t *testing.T) { secureChannelRefusesZeroReply(ctx, t, dcIP) })

	// The first association carries no authentication: the challenge and the
	// server's credential need none.
	cc1, err := dcerpc.Dial(ctx, binding)
	if err != nil {
		t.Fatalf("dial %s: %v", binding, err)
	}
	defer func() { _ = cc1.Close(ctx) }()
	plain, err := logon.NewLogonClient(ctx, cc1, dcerpc.WithInsecure())
	if err != nil {
		t.Fatalf("bind NETLOGON without authentication: %v", err)
	}
	cfg := &nlssp.Config{
		Capabilities:    nlssp.CapAES_SHA2 | nlssp.CapStrongKey | nlssp.CapSecureRPC,
		Credential:      machine,
		ClientChallenge: make([]byte, 8),
	}
	if _, err := rand.Read(cfg.ClientChallenge); err != nil {
		t.Fatal(err)
	}
	chal, err := plain.RequestChallenge(ctx, &logon.RequestChallengeRequest{
		PrimaryName: logonServer, ComputerName: "DITTOFS",
		ClientChallenge: &logon.Credential{Data: cfg.ClientChallenge},
	})
	if err != nil || chal.Return != 0 {
		t.Fatalf("NetrServerReqChallenge: err=%v status=0x%08x", err, uint32(chalReturn(chal)))
	}
	cfg.ServerChallenge = chal.ServerChallenge.Data
	sCred, err := nlssp.NewSecureCredential(ctx, cfg)
	if err != nil {
		t.Fatalf("session key: %v", err)
	}
	clientCred, err := sCred.Encrypt(ctx, cfg.ClientChallenge)
	if err != nil {
		t.Fatal(err)
	}
	auth3, err := plain.Authenticate3(ctx, &logon.Authenticate3Request{
		PrimaryName: logonServer, AccountName: machineAccountName,
		SecureChannelType: logon.SecureChannelTypeWorkstationSecureChannel,
		ComputerName:      "DITTOFS",
		ClientCredential:  &logon.Credential{Data: clientCred},
		NegotiateFlags:    uint32(cfg.Capabilities),
	})
	if err != nil || auth3.Return != 0 {
		t.Fatalf("NetrServerAuthenticate3 with AES and no RC4: err=%v status=0x%08x", err, uint32(auth3Return(auth3)))
	}
	t.Logf("NetrServerAuthenticate3: negotiated flags 0x%08x (asked 0x%08x), RC4 granted=%v",
		auth3.NegotiateFlags, uint32(cfg.Capabilities), nlssp.Cap(auth3.NegotiateFlags).IsSet(nlssp.CapRC4))
	if nlssp.Cap(auth3.NegotiateFlags).IsSet(nlssp.CapRC4) || !nlssp.Cap(auth3.NegotiateFlags).IsSet(nlssp.CapAES_SHA2) {
		t.Errorf("negotiated flags 0x%08x, want AES and no RC4", auth3.NegotiateFlags)
	}
	// Built from ssp/netlogon's parts, the channel checks the server's
	// credential only if its caller does.
	wantServer, err := sCred.Encrypt(ctx, cfg.ServerChallenge)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(wantServer, auth3.ServerCredential.Data) {
		t.Fatalf("server credential %x, want %x", auth3.ServerCredential.Data, wantServer)
	}

	// The unauthenticated association may not sign anyone in.
	if out, err := plain.SAMLogonEx(ctx, logonReq(t)); err == nil && out.Return == 0 {
		t.Errorf("NetrLogonSamLogonEx on the unauthenticated association succeeded, want it refused")
	} else {
		t.Logf("NetrLogonSamLogonEx unsealed: err=%v status=0x%08x", err, uint32(samReturn(out)))
	}

	// The second association binds with the Netlogon credential, sealed.
	cc2, err := dcerpc.Dial(ctx, binding)
	if err != nil {
		t.Fatalf("dial %s: %v", binding, err)
	}
	defer func() { _ = cc2.Close(ctx) }()
	sealed, err := logon.NewLogonClient(ctx, cc2, dcerpc.WithSeal(), dcerpc.WithSecurityConfig(cfg),
		dcerpc.WithMechanism(ssp.Netlogon), dcerpc.WithCredentials(machine))
	if err != nil {
		t.Fatalf("schannel-sealed bind on ncacn_ip_tcp: %v", err)
	}

	out, err := sealed.SAMLogonEx(ctx, logonReq(t))
	if err != nil || out.Return != 0 {
		t.Fatalf("NetrLogonSamLogonEx for alice over schannel: err=%v status=0x%08x", err, uint32(samReturn(out)))
	}
	v, ok := out.ValidationInformation.GetValue().(*logon.ValidationSAMInfo4)
	if !ok || v == nil || v.EffectiveName == nil || !strings.EqualFold(v.EffectiveName.Buffer, adUserAlice) {
		t.Fatalf("NetrLogonSamLogonEx validation %#v, want SamInfo4 for %s", out.ValidationInformation.GetValue(), adUserAlice)
	}
	t.Logf("NetrLogonSamLogonEx: alice signed in, rid %d", v.UserID)

	const rotated = "SpikeRotated01!"
	ts := uint32(time.Now().Unix())
	authCred, err := sCred.Next(ctx, ts)
	if err != nil {
		t.Fatal(err)
	}
	set, err := sealed.PasswordSet2(ctx, &logon.PasswordSet2Request{
		PrimaryName: logonServer, AccountName: machineAccountName,
		SecureChannelType: logon.SecureChannelTypeWorkstationSecureChannel,
		ComputerName:      "DITTOFS",
		Authenticator:     &logon.Authenticator{Credential: &logon.Credential{Data: authCred}, Timestamp: ts},
		ClearNewPassword:  trustPassword(ctx, t, sCred, rotated),
	})
	if err != nil || set.Return != 0 {
		t.Fatalf("NetrServerPasswordSet2 over schannel: err=%v status=0x%08x", err, uint32(setReturn(set)))
	}
	if err := sCred.Verify(ctx, 1, set.ReturnAuthenticator.Credential.Data); err != nil {
		t.Errorf("NetrServerPasswordSet2 return authenticator: %v", err)
	}
	waitForMachinePassword(t, dcIP, "dittofs", rotated)
	t.Log("NetrServerPasswordSet2: the DC accepts DITTOFS$ with the new password")

	t.Run("cifs ticket for a host holding only host names", func(t *testing.T) {
		mappings, _ := adDockerExec("ldbsearch", "-H", "/var/lib/samba/private/sam.ldb",
			"-b", "CN=Directory Service,CN=Windows NT,CN=Services,CN=Configuration,"+adBaseDN,
			"-s", "base", "sPNMappings")
		t.Logf("sPNMappings:\n%s", mappings)
		cfgK, err := krb5config.Load(writeADKrb5Conf(t, dcIP))
		if err != nil {
			t.Fatal(err)
		}
		cl := client.NewWithPassword(adUserAlice, adRealm, adUserPass, cfgK, client.DisablePAFXFAST(true))
		if err := cl.Login(); err != nil {
			t.Fatalf("kinit as alice: %v", err)
		}
		defer cl.Destroy()
		for _, host := range []string{"dc1", "offjoinlegacy"} {
			spns, err := adDockerExec("samba-tool", "spn", "list", host+"$")
			if err != nil {
				t.Fatalf("samba-tool spn list %s$: %v\n%s", host, err, spns)
			}
			if strings.Contains(strings.ToLower(string(spns)), "cifs/") {
				t.Errorf("%s$ holds a cifs/ name, want host/ names only:\n%s", host, spns)
			}
			spn := "cifs/" + host + ".dittofs.ad"
			_, key, err := cl.GetServiceTicket(spn)
			if err != nil {
				t.Errorf("service ticket for %s (account holds only host/ names): %v\n%s", spn, err, spns)
				continue
			}
			t.Logf("service ticket for %s issued, etype %d; %s$ names:\n%s", spn, key.KeyType, host, spns)
		}
		// A name no account holds is refused, so the issues above are not a
		// KDC answering every cifs/ name.
		if _, _, err := cl.GetServiceTicket("cifs/nosuchhost.dittofs.ad"); err == nil {
			t.Errorf("service ticket for cifs/nosuchhost.dittofs.ad issued, want it refused")
		} else {
			t.Logf("service ticket for cifs/nosuchhost.dittofs.ad: %v", err)
		}
	})

	t.Run("key version of a created computer account", func(t *testing.T) {
		pem, err := os.ReadFile(copyADCA(t))
		if err != nil {
			t.Fatal(err)
		}
		roots := x509.NewCertPool()
		roots.AppendCertsFromPEM(pem)
		conn, err := ldap.DialURL("ldaps://"+dcIP+":636",
			ldap.DialWithTLSConfig(&tls.Config{RootCAs: roots, ServerName: adDCHost}))
		if err != nil {
			t.Fatalf("LDAPS: %v", err)
		}
		defer conn.Close()
		if err := conn.Bind(adBindDN, adAdminPassword); err != nil {
			t.Fatalf("bind: %v", err)
		}
		unicodePwd := func(password string) string {
			pw := utf16.Encode([]rune(`"` + password + `"`))
			b := make([]byte, 2*len(pw))
			for i, c := range pw {
				binary.LittleEndian.PutUint16(b[2*i:], c)
			}
			return string(b)
		}
		kvno := func(dn string) string {
			res, err := conn.Search(ldap.NewSearchRequest(dn, ldap.ScopeBaseObject, ldap.NeverDerefAliases,
				0, 0, false, "(objectClass=*)", []string{"msDS-KeyVersionNumber"}, nil))
			if err != nil || len(res.Entries) != 1 {
				t.Fatalf("read msDS-KeyVersionNumber of %s: %v", dn, err)
			}
			return res.Entries[0].GetAttributeValue("msDS-KeyVersionNumber")
		}
		addComputer := func(name string, uac int, password string) string {
			dn := "CN=" + name + ",CN=Computers," + adBaseDN
			add := ldap.NewAddRequest(dn, nil)
			add.Attribute("objectClass", []string{"top", "person", "organizationalPerson", "user", "computer"})
			add.Attribute("sAMAccountName", []string{name + "$"})
			add.Attribute("userAccountControl", []string{strconv.Itoa(uac)})
			if password != "" {
				add.Attribute("unicodePwd", []string{unicodePwd(password)})
			}
			if err := conn.Add(add); err != nil {
				t.Fatalf("add %s: %v", dn, err)
			}
			return dn
		}

		// As v1's online join creates one: disabled, no password, then unicodePwd.
		dn := addComputer("spikekvno", 0x1000|0x2, "")
		created := kvno(dn)
		mod := ldap.NewModifyRequest(dn, nil)
		mod.Replace("unicodePwd", []string{unicodePwd("SpikeKvno01!")})
		if err := conn.Modify(mod); err != nil {
			t.Fatalf("set unicodePwd: %v", err)
		}
		// These pin the answer the join's key version rests on: only an account
		// given its password in the add starts at key version 1.
		set := kvno(dn)
		t.Logf("msDS-KeyVersionNumber, added then given a password: %q after the add, %q after the password set", created, set)
		if created != "1" {
			t.Errorf("msDS-KeyVersionNumber after the add, before the password set = %q, want 1", created)
		}
		if set != "2" {
			t.Errorf("msDS-KeyVersionNumber after add-then-set = %q, want 2 (v1's join)", set)
		}

		// The password in the add itself.
		dn = addComputer("spikekvnopw", 0x1000, "SpikeKvno02!")
		if got := kvno(dn); got != "1" {
			t.Errorf("msDS-KeyVersionNumber with the password in the add = %q, want 1", got)
		}
	})
}

var registerMechanisms sync.Once

// netlogonTCPPort asks the endpoint mapper on port 135 for NETLOGON's
// ncacn_ip_tcp port.
func netlogonTCPPort(ctx context.Context, t *testing.T, dcIP string) string {
	t.Helper()
	cc, err := dcerpc.Dial(ctx, "ncacn_ip_tcp:"+dcIP+"[135]")
	if err != nil {
		t.Fatalf("dial the endpoint mapper: %v", err)
	}
	defer func() { _ = cc.Close(ctx) }()
	cli, err := epm.NewEpmClient(ctx, cc, dcerpc.WithInsecure())
	if err != nil {
		t.Fatalf("bind the endpoint mapper: %v", err)
	}
	resp, err := cli.Lookup(ctx, &epm.LookupRequest{
		InquiryType: 1,
		InterfaceID: &dcetypes.InterfaceID{UUID: dtyp.GUIDFromUUID(logon.LogonSyntaxUUID), VersMajor: 1},
		MaxEntries:  500,
	})
	if err != nil {
		t.Fatalf("endpoint mapper lookup: %v", err)
	}
	for _, e := range resp.Entries {
		b := e.Tower.Binding()
		if b.SyntaxID.IfUUID.String() == logon.LogonSyntaxUUID.String() &&
			b.StringBinding.ProtocolSequence == dcerpc.ProtocolSequenceIPTCP {
			return b.StringBinding.Endpoint
		}
	}
	t.Fatal("the endpoint mapper holds no ncacn_ip_tcp endpoint for NETLOGON")
	return ""
}

// trustPassword builds NL_TRUST_PASSWORD for password, encrypted under the
// channel's AES session key.
func trustPassword(ctx context.Context, t *testing.T, sCred *nlssp.SecureCredential, password string) *logon.TrustPassword {
	t.Helper()
	codes := utf16.Encode([]rune(password))
	clear := make([]byte, 516)
	if _, err := rand.Read(clear[:512]); err != nil {
		t.Fatal(err)
	}
	off := 512 - 2*len(codes)
	for i, c := range codes {
		binary.LittleEndian.PutUint16(clear[off+2*i:], c)
	}
	binary.LittleEndian.PutUint32(clear[512:], uint32(2*len(codes)))
	cipher, err := sCred.Encrypt(ctx, clear)
	if err != nil || len(cipher) != len(clear) {
		t.Fatalf("encrypt the trust password: %v (len %d)", err, len(cipher))
	}
	buf := make([]uint16, 256)
	for i := range buf {
		buf[i] = binary.LittleEndian.Uint16(cipher[2*i:])
	}
	return &logon.TrustPassword{Buffer: buf, Length: binary.LittleEndian.Uint32(cipher[512:])}
}

func chalReturn(r *logon.RequestChallengeResponse) int32 {
	if r == nil {
		return 0
	}
	return r.Return
}

func auth3Return(r *logon.Authenticate3Response) int32 {
	if r == nil {
		return 0
	}
	return r.Return
}

func samReturn(r *logon.SAMLogonExResponse) int32 {
	if r == nil {
		return 0
	}
	return r.Return
}

func setReturn(r *logon.PasswordSet2Response) int32 {
	if r == nil {
		return 0
	}
	return r.Return
}
