//go:build ad_dc

package node_test

import (
	"crypto/tls"
	"crypto/x509"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/go-ldap/ldap/v3"
	"github.com/google/uuid"
	krbclient "github.com/jcmturner/gokrb5/v8/client"
	krb5config "github.com/jcmturner/gokrb5/v8/config"
	"github.com/jcmturner/gokrb5/v8/messages"
	"github.com/jcmturner/gokrb5/v8/types"

	"github.com/cubbit/dittofs/internal/node/nodetest"
	"github.com/cubbit/dittofs/pkg/auth/sid"
	"github.com/cubbit/dittofs/pkg/identity"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/secrets"
	vfs "github.com/cubbit/dittofs/pkg/vfs"
)

// The Samba domain controller test/integration/ad-dc's fixture provisions
// (its entrypoint.sh): its realm, host name and base, its administrator,
// whose password binds the directory, and alice, a member of devs.
const (
	sambaRealm     = "DITTOFS.AD"
	sambaDomain    = "DITTOFS"
	sambaHost      = "dc1.dittofs.ad"
	sambaBase      = "DC=dittofs,DC=ad"
	sambaAdmin     = "CN=Administrator,CN=Users," + sambaBase
	sambaAdminPass = "Passw0rd!2024"
	sambaAlicePass = "TestPassword01!"
	sambaSPN       = "cifs/dittofs.dittofs.ad"
)

// sambaDC is the fixture's domain controller as test/integration/ad-dc's
// TestNodeSignIn hands it over: its address, the CA its certificate is
// verified against and the service keytab it exported.
type sambaDC struct {
	ip     string
	ca     []byte
	keytab []byte
}

// sambaDCFromEnv is the domain controller the environment names, skipping
// t when it names none.
func sambaDCFromEnv(t *testing.T) sambaDC {
	t.Helper()
	ip := os.Getenv("DITTOFS_AD_DC_IP")
	if ip == "" {
		t.Skip("DITTOFS_AD_DC_IP not set: run test/integration/ad-dc's TestNodeSignIn, which starts the Samba domain " +
			"controller and sets it with DITTOFS_AD_DC_CA and DITTOFS_AD_DC_KEYTAB")
	}
	read := func(env string) []byte {
		b, err := os.ReadFile(os.Getenv(env))
		if err != nil {
			t.Fatalf("read %s: %v", env, err)
		}
		return b
	}
	return sambaDC{ip: ip, ca: read("DITTOFS_AD_DC_CA"), keytab: read("DITTOFS_AD_DC_KEYTAB")}
}

// admin binds the domain administrator over LDAPS, verifying the DC against
// the fixture's CA, to read SIDs and add a group.
func (dc sambaDC) admin(t *testing.T) *ldap.Conn {
	t.Helper()
	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM(dc.ca)
	conn, err := ldap.DialURL("ldaps://"+net.JoinHostPort(dc.ip, "636"), ldap.DialWithTLSConfig(&tls.Config{RootCAs: roots, ServerName: sambaHost}))
	if err != nil {
		t.Fatalf("LDAPS to the DC: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	if err := conn.Bind(sambaAdmin, sambaAdminPass); err != nil {
		t.Fatalf("bind as the administrator: %v", err)
	}
	return conn
}

// entry is the DN and objectSid of the entry whose sAMAccountName is name.
func entry(t *testing.T, conn *ldap.Conn, name string) (string, sid.SID) {
	t.Helper()
	res, err := conn.Search(ldap.NewSearchRequest(sambaBase, ldap.ScopeWholeSubtree, ldap.NeverDerefAliases, 0, 0, false,
		"(sAMAccountName="+ldap.EscapeFilter(name)+")", []string{"objectSid"}, nil))
	if err != nil || len(res.Entries) != 1 {
		t.Fatalf("find %s in the directory: %v", name, err)
	}
	s, _, err := sid.DecodeSID(res.Entries[0].GetRawAttributeValue("objectSid"))
	if err != nil {
		t.Fatalf("%s's objectSid: %v", name, err)
	}
	return res.Entries[0].DN, *s
}

// login is alice's Kerberos client, signed in to the DC's KDC over TCP.
func (dc sambaDC) login(t *testing.T) *krbclient.Client {
	t.Helper()
	path := filepath.Join(t.TempDir(), "krb5.conf")
	conf := "[libdefaults]\n default_realm = " + sambaRealm + "\n dns_lookup_kdc = false\n udp_preference_limit = 1\n" +
		"[realms]\n " + sambaRealm + " = {\n  kdc = " + net.JoinHostPort(dc.ip, "88") + "\n }\n"
	if err := os.WriteFile(path, []byte(conf), 0o600); err != nil {
		t.Fatalf("write krb5.conf: %v", err)
	}
	cfg, err := krb5config.Load(path)
	if err != nil {
		t.Fatalf("load krb5.conf: %v", err)
	}
	cl := krbclient.NewWithPassword("alice", sambaRealm, sambaAlicePass, cfg, krbclient.DisablePAFXFAST(true))
	if err := cl.Login(); err != nil {
		t.Fatalf("kinit as alice: %v", err)
	}
	t.Cleanup(cl.Destroy)
	return cl
}

// apReq is a step presenting tkt with an authenticator made at now, the
// node's clock, which the node checks it against.
func apReq(t *testing.T, cl *krbclient.Client, tkt messages.Ticket, key types.EncryptionKey, now time.Time) identity.SignInStep {
	t.Helper()
	auth, err := types.NewAuthenticator(cl.Credentials.Domain(), cl.Credentials.CName())
	if err != nil {
		t.Fatal(err)
	}
	auth.CTime, auth.Cusec = now.Truncate(time.Second), now.Nanosecond()/1000
	req, err := messages.NewAPReq(tkt, key, auth)
	if err != nil {
		t.Fatalf("alice's AP-REQ: %v", err)
	}
	b, err := req.Marshal()
	if err != nil {
		t.Fatalf("marshal alice's AP-REQ: %v", err)
	}
	return identity.SignInStep{Kerberos: b, Service: identity.ServiceCIFS, Transport: identity.TransportSMB, Client: signInClient}
}

// sidStrings formats each SID.
func sidStrings(sids []identity.SID) []string {
	out := make([]string, len(sids))
	for i := range sids {
		out[i] = sid.FormatSID(&sids[i])
	}
	return out
}

// RFC 18 §3.3 and §3.4 on the composed node: the keytab the Samba domain
// controller exported is sealed as identity.keytab, and the directory record
// names the controller over LDAPS verified against the fixture's CA, its DNS
// server the controller. alice's ticket from the controller's KDC signs in
// through the node's filesystem service, her SID first, and her caller is
// admitted to a share granting only devs, which her PAC carries (catches a
// verifier or binding that agrees only with the test realm). A group created
// after her ticket was issued, alice added to it, is in no PAC of hers; past
// the directory cache's TTL her next sign-in, the same ticket, is admitted to
// a share granting only that group, which only the directory gives (catches a
// node whose identity never reads the directory).
//
// The records are written through the store, not the API's settings and
// keytab routes: the node hands the API neither its settings table nor its
// secret provider yet, so those routes refuse every write.
func TestSambaTicketThroughTheNode(t *testing.T) {
	dc := sambaDCFromEnv(t)
	h := nodetest.New(t, nodetest.Watch(), nodetest.InMemory())
	// The node's clock starts at the harness's date; the KDC's tickets are
	// valid at the host's.
	h.Clock().Jump(time.Since(h.Clock().Now()))
	configure(t, h, storeA)
	shares := createShares(t, h, storeA, "devs", "late")
	started(t, h)
	p := sealer(t, h)
	keytabRef, bindRef := entity.SecretRef(uuid.New()), entity.SecretRef(uuid.New())
	seal(t, h, p, secrets.Protocol, entity.SecretKeytab, entity.SecretOwner{Scope: installationScope, Key: "identity.keytab"}, keytabRef, dc.keytab)
	seal(t, h, p, secrets.Protocol, entity.SecretDirectoryCredential, entity.SecretOwner{Scope: installationScope, Key: "identity.directory"}, bindRef, []byte(sambaAdminPass))
	rows := map[string]string{
		// The default NetBIOS name, DITTOFS, is the controller's domain,
		// which identity refuses a node to share.
		"identity.netbios_name":              `"NAS"`,
		"identity.keytab":                    refJSON(keytabRef),
		"identity.directory.bind_credential": refJSON(bindRef),
	}
	for k, v := range map[string]string{
		"address": "ldaps://" + sambaHost, "base": sambaBase, "bind_name": sambaAdmin, "trust_roots": string(dc.ca),
		"realm": sambaRealm, "netbios_domain": sambaDomain, "dns_server": dc.ip,
	} {
		rows["identity.directory."+k] = strconv.Quote(v)
	}
	putSettings(t, h, rows)
	caughtUp(t, h)
	if refused := h.Node().Refused(); len(refused) != 0 {
		t.Fatalf("records refused once the keytab and the directory record were written: %q, want none", refusedIDs(refused))
	}

	conn := dc.admin(t)
	aliceDN, aliceSID := entry(t, conn, "alice")
	_, devsSID := entry(t, conn, "devs")
	cl := dc.login(t)
	tkt, key, err := cl.GetServiceTicket(sambaSPN)
	if err != nil {
		t.Fatalf("alice's service ticket for %s: %v", sambaSPN, err)
	}
	fs := h.Node().Service()
	signIn := func(what string) vfs.Call {
		t.Helper()
		res, err := fs.SignIn(t.Context(), apReq(t, cl, tkt, key, h.Clock().Now()))
		if err != nil {
			t.Fatalf("%s: alice's ticket signed in: %v, want signed in", what, err)
		}
		if got := sidStrings(res.Identity.Evidence.SIDs); len(got) == 0 || got[0] != sid.FormatSID(&aliceSID) {
			t.Fatalf("%s: SIDs %v, want alice's %s first", what, got, sid.FormatSID(&aliceSID))
		}
		return vfs.Call{Identity: res.Identity}
	}
	// grant grants share the principal holding s, which a proven caller's
	// Find mints from the directory as a grant by directory name does.
	grant := func(call vfs.Call, share entity.Share, s sid.SID) {
		t.Helper()
		p, err := fs.Find(t.Context(), call, vfs.Spelling{Scheme: vfs.SchemeSID, SID: s})
		if err != nil || p.IsZero() {
			t.Fatalf("Find of %s: %v, %v; want the group's principal", sid.FormatSID(&s), p, err)
		}
		if _, err := h.Store().ControlPlane().SetGrant(t.Context(), entity.ShareGrant{Share: share.ID, Principal: p, Access: entity.AccessRead}, 0); err != nil {
			t.Fatalf("grant %s: %v", share.Name, err)
		}
	}
	admitted := func(what string, call vfs.Call, share entity.Share) {
		t.Helper()
		if _, err := fs.StatFS(t.Context(), call, entity.NewHandle(share.ID, share.Root)); err != nil {
			t.Errorf("%s: alice's call on share %s: %v, want admitted", what, share.Name, err)
		}
	}

	call := signIn("first sign-in")
	grant(call, shares[0], devsSID)
	admitted("devs, which her PAC carries", call, shares[0])

	// A group created after her ticket was issued, alice in it.
	late := "late" + strconv.FormatInt(time.Now().UnixNano()%1e9, 10)
	lateDN := "CN=" + late + ",CN=Users," + sambaBase
	add := ldap.NewAddRequest(lateDN, nil)
	add.Attribute("objectClass", []string{"top", "group"})
	add.Attribute("sAMAccountName", []string{late})
	add.Attribute("member", []string{aliceDN})
	if err := conn.Add(add); err != nil {
		t.Fatalf("add %s: %v", lateDN, err)
	}
	t.Cleanup(func() { _ = conn.Del(ldap.NewDelRequest(lateDN, nil)) })
	_, lateSID := entry(t, conn, late)
	h.Clock().Advance(6 * time.Minute) // past the directory cache's five-minute TTL

	call = signIn("sign-in past the TTL")
	if got := sidStrings(call.Identity.Evidence.SIDs); slices.Contains(got, sid.FormatSID(&lateSID)) {
		t.Fatalf("SIDs %v carry %s's %s, which her ticket predates", got, late, sid.FormatSID(&lateSID))
	}
	grant(call, shares[1], lateSID)
	admitted(late+", which only the directory gives", call, shares[1])
}
