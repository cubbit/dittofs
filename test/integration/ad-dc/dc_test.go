//go:build ad_dc

package ad_dc_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-ldap/ldap/v3"

	"github.com/cubbit/dittofs/internal/identity/directory"
	"github.com/cubbit/dittofs/internal/identity/netlogon"
	"github.com/cubbit/dittofs/pkg/auth/sid"
	"github.com/cubbit/dittofs/pkg/identity"
	"github.com/cubbit/dittofs/pkg/identity/identitytest"
)

// The Samba domain controller passes the domain controller's conformance
// suite through netlogon.NewDialer, the production client, so the in-memory
// one stands in for it only where both answer alike.
func TestDomainControllerConformance(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping AD-DC integration test in short mode")
	}
	dcIP := setupADDC(t)
	waitForMachinePassword(t, dcIP, "dittofs", machineAccountPass)
	rec, ca := dcRecord(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	seed := &sambaDCSeeder{conn: adminLDAP(t, dcIP, ca), ip: dcIP}
	dialer := netlogon.NewDialer((&net.Dialer{}).DialContext)

	identitytest.DomainControllerConformance(t, func(t *testing.T) (netlogon.DC, identitytest.DCSeeder) {
		dc, err := dialer.Dial(ctx, rec, dcIP)
		if err != nil {
			t.Fatalf("dial the DC: %v", err)
		}
		return dc, seed
	})
}

// A pre-staged account reads back the owner its creator left and its
// delegation value as stored: offjoinowned$ is owned by joiner and lets
// dittofs$ act on its behalf. The read binds as joiner, a plain user as a
// directory's bind account is, who may read no security descriptor's SACL.
func TestDomainControllerAccountOwnerAndDelegation(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping AD-DC integration test in short mode")
	}
	dcIP := setupADDC(t)
	rec, _ := dcRecord(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	dc, err := netlogon.NewDialer((&net.Dialer{}).DialContext).Dial(ctx, rec, dcIP)
	if err != nil {
		t.Fatalf("dial the DC: %v", err)
	}
	defer func() { _ = dc.Close() }()
	a, err := dc.Account(ctx, identity.JoinCredential{User: "joiner@" + adRealm, Password: []byte(adUserPass)}, "offjoinowned")
	if err != nil {
		t.Fatalf("Account offjoinowned$: %v", err)
	}
	joiner := adAttrs(t, "user", "joiner", "objectSid")["objectSid"]
	if got := sid.FormatSID(&a.Owner); got != joiner {
		t.Errorf("offjoinowned$'s owner %s, want joiner's %s", got, joiner)
	}
	dittofs, err := sid.ParseSIDString(adAttrs(t, "computer", "dittofs", "objectSid")["objectSid"])
	if err != nil {
		t.Fatalf("parse dittofs$'s objectSid: %v", err)
	}
	var enc bytes.Buffer
	sid.EncodeSID(&enc, dittofs)
	if !bytes.Contains(a.Delegation, enc.Bytes()) {
		t.Errorf("offjoinowned$'s msDS-AllowedToActOnBehalfOfOtherIdentity %x, want a descriptor naming dittofs$ %s",
			a.Delegation, sid.FormatSID(dittofs))
	}
}

// dcRecord is the fixture's directory record as the domain controller's
// account calls read it, and the fixture's CA.
func dcRecord(t *testing.T) (directory.Record, []byte) {
	t.Helper()
	ca, err := os.ReadFile(copyADCA(t))
	if err != nil {
		t.Fatalf("read the fixture CA: %v", err)
	}
	rec := fixtureRecord()
	rec.Address, rec.Base, rec.TrustRoots = "ldaps://"+adDCHost, adBaseDN, string(ca)
	return rec, ca
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

// sambaDCSeeder seeds the fixture's Samba over LDAPS as its administrator.
type sambaDCSeeder struct {
	conn *ldap.Conn
	ip   string // the controller's address

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
	v := res.Entries[0].GetAttributeValue("badPwdCount")
	n, err := strconv.Atoi(v)
	if err != nil {
		t.Fatalf("%s's badPwdCount %q: %v", name, v, err)
	}
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

// Unreachable is the controller with its NETLOGON port out of reach: the
// endpoint mapper's connection reaches the real controller, and every other
// one a local port nothing listens on, so its first call fails to dial.
func (s *sambaDCSeeder) Unreachable(t testing.TB) netlogon.DC {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen for a free port: %v", err)
	}
	closed := l.Addr().String()
	_ = l.Close()
	dial := func(ctx context.Context, network, addr string) (net.Conn, error) {
		if _, port, _ := net.SplitHostPort(addr); port == "135" {
			return (&net.Dialer{}).DialContext(ctx, network, addr)
		}
		return (&net.Dialer{}).DialContext(ctx, network, closed)
	}
	dc, err := netlogon.NewDialer(dial).Dial(t.Context(), fixtureRecord(), s.ip)
	if err != nil {
		t.Fatalf("build the unreachable controller: %v", err)
	}
	return dc
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
