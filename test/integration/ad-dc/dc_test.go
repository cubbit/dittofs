//go:build ad_dc

package ad_dc_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-ldap/ldap/v3"
	"github.com/oiweiwei/go-msrpc/dcerpc"
	"github.com/oiweiwei/go-msrpc/msrpc/dtyp"
	logon "github.com/oiweiwei/go-msrpc/msrpc/nrpc/logon/v1"
	"github.com/oiweiwei/go-msrpc/ssp"
	"github.com/oiweiwei/go-msrpc/ssp/credential"
	nlssp "github.com/oiweiwei/go-msrpc/ssp/netlogon"

	"github.com/cubbit/dittofs/internal/identity/netlogon"
	"github.com/cubbit/dittofs/pkg/auth/sid"
	"github.com/cubbit/dittofs/pkg/identity"
	"github.com/cubbit/dittofs/pkg/identity/identitytest"
)

// The Samba domain controller passes the domain controller's conformance
// suite, so the in-memory one stands in for it only where both answer alike.
//
// The netlogon.DC it runs against is built here from the spike's NETLOGON
// calls and plain LDAP, until the production one exists; that one replaces
// sambaDC and runs this same suite.
func TestDomainControllerConformance(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping AD-DC integration test in short mode")
	}
	dcIP := setupADDC(t)
	// v1's channel registers go-msrpc's mechanisms process-wide, once; let it
	// run first, as the spike does.
	waitForMachinePassword(t, dcIP, "dittofs", machineAccountPass)
	ca, err := os.ReadFile(copyADCA(t))
	if err != nil {
		t.Fatalf("read the fixture CA: %v", err)
	}
	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM(ca)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	// Samba counts badPwdCount only under a lockout threshold, which the
	// fixture's domain leaves at 0; 100 locks no row out.
	if out, err := adDockerExec("samba-tool", "domain", "passwordsettings", "set", "--account-lockout-threshold=100"); err != nil {
		t.Fatalf("set the lockout threshold: %v\n%s", err, out)
	}
	port := netlogonTCPPort(ctx, t, dcIP)
	seed := &sambaDCSeeder{conn: adminLDAP(t, dcIP, ca)}

	identitytest.DomainControllerConformance(t, func(t *testing.T) (netlogon.DC, identitytest.DCSeeder) {
		return &sambaDC{ip: dcIP, binding: "ncacn_ip_tcp:" + dcIP + "[" + port + "]", roots: roots}, seed
	})
}

// sambaDC is a netlogon.DC over the fixture's Samba: NETLOGON on
// ncacn_ip_tcp with schannel, as the spike runs it, and LDAPS as its
// administrator for the account calls.
type sambaDC struct {
	ip, binding string
	roots       *x509.CertPool

	mu     sync.Mutex
	closed bool
}

var errSambaClosed = errors.New("sambaDC: closed")

func (d *sambaDC) check() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed {
		return errSambaClosed
	}
	return nil
}

func (d *sambaDC) Close() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.closed = true
	return nil
}

func (d *sambaDC) ldap(bind identity.JoinCredential) (*ldap.Conn, error) {
	conn, err := ldap.DialURL("ldaps://"+d.ip+":636",
		ldap.DialWithTLSConfig(&tls.Config{RootCAs: d.roots, ServerName: adDCHost}))
	if err != nil {
		return nil, err
	}
	if err := conn.Bind(bind.User, string(bind.Password)); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("bind as %s: %w: %w", bind.User, err, netlogon.ErrRejected)
	}
	return conn, nil
}

func (d *sambaDC) Channel(ctx context.Context, account string, password []byte) (netlogon.Channel, error) {
	if err := d.check(); err != nil {
		return nil, err
	}
	name := strings.ToUpper(account)
	machine := credential.NewFromPassword(name+"$", string(password),
		credential.Workstation(name), credential.Domain(adRealm))
	cc1, err := dcerpc.Dial(ctx, d.binding)
	if err != nil {
		return nil, err
	}
	defer func() { _ = cc1.Close(ctx) }()
	plain, err := logon.NewLogonClient(ctx, cc1, dcerpc.WithInsecure())
	if err != nil {
		return nil, err
	}
	cfg := &nlssp.Config{
		Capabilities:    nlssp.CapAES_SHA2 | nlssp.CapStrongKey | nlssp.CapSecureRPC,
		Credential:      machine,
		ClientChallenge: make([]byte, 8),
	}
	_, _ = rand.Read(cfg.ClientChallenge)
	chal, err := plain.RequestChallenge(ctx, &logon.RequestChallengeRequest{
		PrimaryName: `\\DC1`, ComputerName: name, ClientChallenge: &logon.Credential{Data: cfg.ClientChallenge},
	})
	if chal != nil && chal.Return != 0 {
		return nil, netlogon.Status(uint32(chal.Return))
	}
	if err != nil {
		return nil, err
	}
	cfg.ServerChallenge = chal.ServerChallenge.Data
	sCred, err := nlssp.NewSecureCredential(ctx, cfg)
	if err != nil {
		return nil, err
	}
	clientCred, err := sCred.Encrypt(ctx, cfg.ClientChallenge)
	if err != nil {
		return nil, err
	}
	auth3, err := plain.Authenticate3(ctx, &logon.Authenticate3Request{
		PrimaryName: `\\DC1`, AccountName: name + "$",
		SecureChannelType: logon.SecureChannelTypeWorkstationSecureChannel,
		ComputerName:      name,
		ClientCredential:  &logon.Credential{Data: clientCred},
		NegotiateFlags:    uint32(cfg.Capabilities),
	})
	if auth3 != nil && auth3.Return != 0 {
		return nil, netlogon.Status(uint32(auth3.Return))
	}
	if err != nil {
		return nil, err
	}
	want, err := sCred.Encrypt(ctx, cfg.ServerChallenge)
	if err != nil {
		return nil, err
	}
	if !bytes.Equal(want, auth3.ServerCredential.Data) {
		return nil, errors.New("sambaDC: the server credential does not verify")
	}
	cc2, err := dcerpc.Dial(ctx, d.binding)
	if err != nil {
		return nil, err
	}
	sealed, err := logon.NewLogonClient(ctx, cc2, dcerpc.WithSeal(), dcerpc.WithSecurityConfig(cfg),
		dcerpc.WithMechanism(ssp.Netlogon), dcerpc.WithCredentials(machine))
	if err != nil {
		_ = cc2.Close(ctx)
		return nil, err
	}
	return &sambaChannel{cc: cc2, cli: sealed, cred: sCred, name: name}, nil
}

// entry reads the computer account name$ with attrs.
func entry(conn *ldap.Conn, name string, attrs ...string) (*ldap.Entry, error) {
	res, err := conn.Search(ldap.NewSearchRequest(adBaseDN, ldap.ScopeWholeSubtree, ldap.NeverDerefAliases, 0, 0, false,
		"(sAMAccountName="+ldap.EscapeFilter(name)+"$)", attrs, nil))
	if err != nil {
		return nil, err
	}
	if len(res.Entries) != 1 {
		return nil, fmt.Errorf("%s$: %w", name, netlogon.ErrNoAccount)
	}
	return res.Entries[0], nil
}

func (d *sambaDC) Account(ctx context.Context, bind identity.JoinCredential, name string) (netlogon.Account, error) {
	if err := d.check(); err != nil {
		return netlogon.Account{}, err
	}
	conn, err := d.ldap(bind)
	if err != nil {
		return netlogon.Account{}, err
	}
	defer func() { _ = conn.Close() }()
	const deleg = "msDS-AllowedToActOnBehalfOfOtherIdentity"
	e, err := entry(conn, name, "msDS-KeyVersionNumber", "msDS-SupportedEncryptionTypes",
		"servicePrincipalName", "nTSecurityDescriptor", deleg)
	if err != nil {
		return netlogon.Account{}, err
	}
	kvno, _ := strconv.ParseUint(e.GetAttributeValue("msDS-KeyVersionNumber"), 10, 32)
	enc, _ := strconv.ParseUint(e.GetAttributeValue("msDS-SupportedEncryptionTypes"), 10, 32)
	a := netlogon.Account{DN: e.DN, KeyVersion: uint32(kvno), EncTypes: uint32(enc),
		Delegation: e.GetRawAttributeValue(deleg)}
	if sd := e.GetRawAttributeValue("nTSecurityDescriptor"); len(sd) >= 20 {
		if off := binary.LittleEndian.Uint32(sd[4:]); off != 0 && int(off) < len(sd) {
			if o, _, err := sid.DecodeSID(sd[off:]); err == nil {
				a.Owner = *o
			}
		}
	}
	res, err := conn.Search(ldap.NewSearchRequest("CN=Directory Service,CN=Windows NT,CN=Services,CN=Configuration,"+adBaseDN,
		ldap.ScopeBaseObject, ldap.NeverDerefAliases, 0, 0, false, "(objectClass=*)", []string{"sPNMappings"}, nil))
	if err != nil || len(res.Entries) != 1 {
		return netlogon.Account{}, fmt.Errorf("read sPNMappings: %v", err)
	}
	var hostTo []string
	for _, m := range res.Entries[0].GetAttributeValues("sPNMappings") {
		if k, v, ok := strings.Cut(m, "="); ok && strings.EqualFold(k, "host") {
			hostTo = append(hostTo, strings.Split(v, ",")...)
		}
	}
	for _, n := range e.GetAttributeValues("servicePrincipalName") {
		a.Names = append(a.Names, n)
		if svc, host, ok := strings.Cut(n, "/"); ok && strings.EqualFold(svc, "host") {
			for _, m := range hostTo {
				a.Names = append(a.Names, m+"/"+host)
			}
		}
	}
	return a, nil
}

// computersGUID is the Computers container's well-known GUID.
const computersGUID = "AA312825768811D1ADED00C04FD8D5CD"

// wellKnownComputers reads the Computers container the domain names, as a
// whole wellKnownObjects value and its DN.
func wellKnownComputers(conn *ldap.Conn) (value, dn string, err error) {
	res, err := conn.Search(ldap.NewSearchRequest(adBaseDN, ldap.ScopeBaseObject, ldap.NeverDerefAliases, 0, 0, false,
		"(objectClass=*)", []string{"wellKnownObjects"}, nil))
	if err != nil || len(res.Entries) != 1 {
		return "", "", fmt.Errorf("read wellKnownObjects: %v", err)
	}
	for _, v := range res.Entries[0].GetAttributeValues("wellKnownObjects") {
		if parts := strings.SplitN(v, ":", 4); len(parts) == 4 && strings.EqualFold(parts[2], computersGUID) {
			return v, parts[3], nil
		}
	}
	return "", "", errors.New("wellKnownObjects names no Computers container")
}

func (d *sambaDC) Create(ctx context.Context, admin identity.JoinCredential, c netlogon.Computer, password []byte) error {
	if err := d.check(); err != nil {
		return err
	}
	conn, err := d.ldap(admin)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	ou := c.OU
	if ou == "" {
		if _, ou, err = wellKnownComputers(conn); err != nil {
			return err
		}
	}
	add := ldap.NewAddRequest("CN="+c.Name+","+ou, nil)
	add.Attribute("objectClass", []string{"top", "person", "organizationalPerson", "user", "computer"})
	add.Attribute("sAMAccountName", []string{c.Name + "$"})
	add.Attribute("userAccountControl", []string{"4096"})
	add.Attribute("unicodePwd", []string{unicodePwd(string(password))})
	add.Attribute("dNSHostName", []string{c.DNSHostName})
	add.Attribute("servicePrincipalName", []string{"HOST/" + c.Name, "HOST/" + c.DNSHostName,
		"cifs/" + c.Name, "cifs/" + c.DNSHostName})
	add.Attribute("msDS-SupportedEncryptionTypes", []string{"24"})
	if err := conn.Add(add); err != nil {
		if ldap.IsErrorWithCode(err, ldap.LDAPResultEntryAlreadyExists) {
			return fmt.Errorf("%s$: %w: %w", c.Name, err, fs.ErrExist)
		}
		return err
	}
	return nil
}

func (d *sambaDC) Reset(ctx context.Context, admin identity.JoinCredential, name string, password []byte) error {
	if err := d.check(); err != nil {
		return err
	}
	conn, err := d.ldap(admin)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	e, err := entry(conn, name)
	if err != nil {
		return err
	}
	mod := ldap.NewModifyRequest(e.DN, nil)
	mod.Replace("unicodePwd", []string{unicodePwd(string(password))})
	return conn.Modify(mod)
}

// sambaChannel is a schannel-sealed NETLOGON association.
type sambaChannel struct {
	cc   dcerpc.Conn
	cli  logon.LogonClient
	cred *nlssp.SecureCredential
	name string

	mu sync.Mutex
}

func toSID(s *dtyp.SID, rid ...uint32) sid.SID {
	if s == nil {
		return sid.SID{}
	}
	b, err := s.Bytes()
	if err != nil {
		return sid.SID{}
	}
	out, _, err := sid.DecodeSID(b)
	if err != nil {
		return sid.SID{}
	}
	for _, r := range rid {
		out.SubAuthorityCount++
		out.SubAuthorities = append(out.SubAuthorities, r)
	}
	return *out
}

func (c *sambaChannel) Logon(ctx context.Context, r netlogon.LogonRequest) (netlogon.Reply, error) {
	out, err := c.cli.SAMLogonEx(ctx, &logon.SAMLogonExRequest{
		LogonServer: `\\DC1`, ComputerName: c.name,
		LogonLevel: logon.LogonInfoClassNetworkTransitiveInformation,
		LogonInformation: &logon.Level{Value: &logon.Level_LogonNetworkTransitive{
			LogonNetworkTransitive: &logon.NetworkInfo{
				Identity: &logon.LogonIdentityInfo{
					LogonDomainName: &dtyp.UnicodeString{Buffer: r.Domain},
					UserName:        &dtyp.UnicodeString{Buffer: r.User},
					Workstation:     &dtyp.UnicodeString{Buffer: r.Workstation},
				},
				LMChallenge:         &logon.LMChallenge{Data: r.Challenge[:]},
				NTChallengeResponse: &logon.String{Buffer: r.NTResponse},
				LMChallengeResponse: &logon.String{Buffer: r.LMResponse},
			},
		}},
		ValidationLevel: logon.ValidationInfoClassSAMInfo4,
	})
	if out != nil && out.Return != 0 {
		return netlogon.Reply{}, netlogon.Status(uint32(out.Return))
	}
	if err != nil {
		return netlogon.Reply{}, err
	}
	v, ok := out.ValidationInformation.GetValue().(*logon.ValidationSAMInfo4)
	if !ok || v == nil {
		return netlogon.Reply{}, fmt.Errorf("validation %T, want SamInfo4", out.ValidationInformation.GetValue())
	}
	rep := netlogon.Reply{User: toSID(v.LogonDomainID, v.UserID), UserFlags: v.UserFlags, LogonDomain: toSID(v.LogonDomainID)}
	for _, g := range v.GroupIDs {
		rep.Groups = append(rep.Groups, netlogon.SIDAttr{SID: toSID(v.LogonDomainID, g.RelativeID), Attrs: g.Attributes})
	}
	for _, x := range v.ExtraSIDs {
		rep.Groups = append(rep.Groups, netlogon.SIDAttr{SID: toSID(x.SID), Attrs: x.Attributes})
	}
	if v.UserSessionKey != nil {
		for _, b := range v.UserSessionKey.Data {
			rep.SessionKey = append(rep.SessionKey, b.Data...)
		}
	}
	return rep, nil
}

func (c *sambaChannel) SetPassword(ctx context.Context, password []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	ts := uint32(time.Now().Unix())
	auth, err := c.cred.Next(ctx, ts)
	if err != nil {
		return err
	}
	tp, err := sealTrustPassword(ctx, c.cred, string(password))
	if err != nil {
		return err
	}
	set, err := c.cli.PasswordSet2(ctx, &logon.PasswordSet2Request{
		PrimaryName: `\\DC1`, AccountName: c.name + "$",
		SecureChannelType: logon.SecureChannelTypeWorkstationSecureChannel,
		ComputerName:      c.name,
		Authenticator:     &logon.Authenticator{Credential: &logon.Credential{Data: auth}, Timestamp: ts},
		ClearNewPassword:  tp,
	})
	if set != nil && set.Return != 0 {
		return netlogon.Status(uint32(set.Return))
	}
	if err != nil {
		return err
	}
	return c.cred.Verify(ctx, 1, set.ReturnAuthenticator.Credential.Data)
}

func (c *sambaChannel) Close() error { return c.cc.Close(context.Background()) }

// sealTrustPassword is the spike's trustPassword without its *testing.T.
func sealTrustPassword(ctx context.Context, sCred *nlssp.SecureCredential, password string) (*logon.TrustPassword, error) {
	pw := []byte(unicodePwd(password))
	pw = pw[2 : len(pw)-2] // unicodePwd quotes it
	clear := make([]byte, 516)
	_, _ = rand.Read(clear[:512])
	copy(clear[512-len(pw):], pw)
	binary.LittleEndian.PutUint32(clear[512:], uint32(len(pw)))
	cipher, err := sCred.Encrypt(ctx, clear)
	if err != nil {
		return nil, err
	}
	buf := make([]uint16, 256)
	for i := range buf {
		buf[i] = binary.LittleEndian.Uint16(cipher[2*i:])
	}
	return &logon.TrustPassword{Buffer: buf, Length: binary.LittleEndian.Uint32(cipher[512:])}, nil
}

// sambaDCSeeder seeds the fixture's Samba over LDAPS as its administrator.
type sambaDCSeeder struct {
	conn *ldap.Conn

	once      sync.Once
	domainSID sid.SID
}

func (s *sambaDCSeeder) Domain() identitytest.DCDomain {
	s.once.Do(func() {
		res, err := s.conn.Search(ldap.NewSearchRequest(adBaseDN, ldap.ScopeBaseObject, ldap.NeverDerefAliases, 0, 0, false,
			"(objectClass=*)", []string{"objectSid"}, nil))
		if err == nil && len(res.Entries) == 1 {
			if d, _, err := sid.DecodeSID(res.Entries[0].GetRawAttributeValue("objectSid")); err == nil {
				s.domainSID = *d
			}
		}
	})
	return identitytest.DCDomain{NetBIOS: adDomain, Realm: adRealm, Base: adBaseDN, SID: s.domainSID}
}

func (s *sambaDCSeeder) Admin() identity.JoinCredential {
	return identity.JoinCredential{User: adBindDN, Password: []byte(adAdminPassword)}
}

func (s *sambaDCSeeder) AddUser(t testing.TB, name string, password []byte) sid.SID {
	t.Helper()
	dn := "CN=" + name + ",CN=Users," + adBaseDN
	add := ldap.NewAddRequest(dn, nil)
	add.Attribute("objectClass", []string{"top", "person", "organizationalPerson", "user"})
	add.Attribute("sAMAccountName", []string{name})
	add.Attribute("userAccountControl", []string{"512"})
	add.Attribute("unicodePwd", []string{unicodePwd(string(password))})
	if err := s.conn.Add(add); err != nil {
		t.Fatalf("add %s: %v", dn, err)
	}
	t.Cleanup(func() { _ = s.conn.Del(ldap.NewDelRequest(dn, nil)) })
	res, err := s.conn.Search(ldap.NewSearchRequest(dn, ldap.ScopeBaseObject, ldap.NeverDerefAliases, 0, 0, false,
		"(objectClass=*)", []string{"objectSid"}, nil))
	if err != nil || len(res.Entries) != 1 {
		t.Fatalf("read %s's objectSid: %v", dn, err)
	}
	u, _, err := sid.DecodeSID(res.Entries[0].GetRawAttributeValue("objectSid"))
	if err != nil {
		t.Fatalf("decode %s's objectSid: %v", dn, err)
	}
	return *u
}

func (s *sambaDCSeeder) BadPwdCount(t testing.TB, name string) int {
	t.Helper()
	res, err := s.conn.Search(ldap.NewSearchRequest(adBaseDN, ldap.ScopeWholeSubtree, ldap.NeverDerefAliases, 0, 0, false,
		"(sAMAccountName="+ldap.EscapeFilter(name)+")", []string{"badPwdCount"}, nil))
	if err != nil || len(res.Entries) != 1 {
		t.Fatalf("read %s's badPwdCount: %v", name, err)
	}
	n, _ := strconv.Atoi(res.Entries[0].GetAttributeValue("badPwdCount"))
	return n
}

func (s *sambaDCSeeder) StageComputer(t testing.TB, c identitytest.StagedComputer) {
	t.Helper()
	dn := "CN=" + c.Name + ",CN=Computers," + adBaseDN
	add := ldap.NewAddRequest(dn, nil)
	add.Attribute("objectClass", []string{"top", "person", "organizationalPerson", "user", "computer"})
	add.Attribute("sAMAccountName", []string{c.Name + "$"})
	add.Attribute("userAccountControl", []string{"4096"})
	add.Attribute("unicodePwd", []string{unicodePwd(string(c.Password))})
	if c.EncTypes != 0 {
		add.Attribute("msDS-SupportedEncryptionTypes", []string{strconv.FormatUint(uint64(c.EncTypes), 10)})
	}
	if len(c.SPNs) > 0 {
		add.Attribute("servicePrincipalName", c.SPNs)
	}
	if err := s.conn.Add(add); err != nil {
		t.Fatalf("stage %s: %v", dn, err)
	}
}

func (s *sambaDCSeeder) SetComputerPassword(t testing.TB, name string, password []byte) {
	t.Helper()
	e, err := entry(s.conn, name)
	if err != nil {
		t.Fatal(err)
	}
	mod := ldap.NewModifyRequest(e.DN, nil)
	mod.Replace("unicodePwd", []string{unicodePwd(string(password))})
	if err := s.conn.Modify(mod); err != nil {
		t.Fatalf("set %s$'s password: %v", name, err)
	}
}

// redirect makes dn the Computers container wellKnownObjects names, as
// redircmp does.
func (s *sambaDCSeeder) redirect(t testing.TB, dn string) {
	t.Helper()
	old, cur, err := wellKnownComputers(s.conn)
	if err != nil {
		t.Fatal(err)
	}
	if strings.EqualFold(cur, dn) {
		return
	}
	mod := ldap.NewModifyRequest(adBaseDN, nil)
	mod.Delete("wellKnownObjects", []string{old})
	mod.Add("wellKnownObjects", []string{"B:32:" + computersGUID + ":" + dn})
	if err := s.conn.Modify(mod); err != nil {
		t.Fatalf("redirect the Computers container to %s: %v", dn, err)
	}
}

func (s *sambaDCSeeder) MoveComputersContainer(t testing.TB, ou string) {
	t.Helper()
	add := ldap.NewAddRequest(ou, nil)
	add.Attribute("objectClass", []string{"top", "organizationalUnit"})
	if err := s.conn.Add(add); err != nil && !ldap.IsErrorWithCode(err, ldap.LDAPResultEntryAlreadyExists) {
		t.Fatalf("add %s: %v", ou, err)
	}
	s.redirect(t, ou)
}

func (s *sambaDCSeeder) Reset(t testing.TB) {
	t.Helper()
	s.redirect(t, "CN=Computers,"+adBaseDN)
	res, err := s.conn.Search(ldap.NewSearchRequest(adBaseDN, ldap.ScopeWholeSubtree, ldap.NeverDerefAliases, 0, 0, false,
		"(&(objectClass=computer)(sAMAccountName=DCT*))", []string{"cn"}, nil))
	if err != nil {
		t.Fatalf("find the suite's computer accounts: %v", err)
	}
	for _, e := range res.Entries {
		if err := s.conn.Del(ldap.NewDelRequest(e.DN, nil)); err != nil {
			t.Fatalf("delete %s: %v", e.DN, err)
		}
	}
	if err := s.conn.Del(ldap.NewDelRequest("OU=Servers,"+adBaseDN, nil)); err != nil &&
		!ldap.IsErrorWithCode(err, ldap.LDAPResultNoSuchObject) {
		t.Fatalf("delete OU=Servers: %v", err)
	}
}
