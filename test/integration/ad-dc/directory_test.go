//go:build ad_dc

package ad_dc_test

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"net"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf16"

	"github.com/go-ldap/ldap/v3"

	"github.com/cubbit/dittofs/internal/identity/directory"
	"github.com/cubbit/dittofs/pkg/identity/identitytest"
)

// The Samba domain controller passes the directory's conformance suite,
// reached through DialTLS by its host name, its certificate verified against
// the fixture's CA, over ldaps:// and over ldap:// with StartTLS. The host
// name resolves through the DC's own DNS, the record's DNS server.
func TestDirectoryConformance(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping AD-DC integration test in short mode")
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not found, skipping AD-DC integration test")
	}

	dcIP := setupADDC(t)
	ca, err := os.ReadFile(copyADCA(t))
	if err != nil {
		t.Fatalf("read the fixture CA: %v", err)
	}
	seed := &sambaSeeder{conn: adminLDAP(t, dcIP, ca)}

	for _, addr := range []string{"ldaps://" + adDCHost, "ldap://" + adDCHost} {
		t.Run(addr, func(t *testing.T) {
			rec := directory.Record{Address: addr, Base: adBaseDN, TrustRoots: string(ca), Realm: adRealm, DNSServer: dcIP}
			identitytest.DirectoryConformance(t, func(t *testing.T) (directory.Conn, identitytest.Seeder) {
				ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
				defer cancel()
				c, err := directory.DialTLS(ctx, rec)
				if err != nil {
					t.Fatalf("DialTLS %s: %v", addr, err)
				}
				return c, seed
			})
		})
	}
}

// adminLDAP binds the domain administrator over LDAPS for seeding, verifying
// the DC against the fixture's CA.
func adminLDAP(t *testing.T, dcIP string, ca []byte) *ldap.Conn {
	t.Helper()
	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM(ca)
	conn, err := ldap.DialURL("ldaps://"+net.JoinHostPort(dcIP, "636"),
		ldap.DialWithTLSConfig(&tls.Config{RootCAs: roots, ServerName: adDCHost}))
	if err != nil {
		t.Fatalf("LDAPS to the DC: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	if err := conn.Bind(adBindDN, adAdminPassword); err != nil {
		t.Fatalf("bind as the administrator: %v", err)
	}
	return conn
}

// sambaSeeder adds users to the fixture's DC over LDAP as its administrator.
type sambaSeeder struct{ conn *ldap.Conn }

func (s *sambaSeeder) AddUser(t testing.TB, name string, attrs map[string][][]byte) string {
	t.Helper()
	dn := "CN=" + name + ",CN=Users," + adBaseDN
	add := ldap.NewAddRequest(dn, nil)
	add.Attribute("objectClass", []string{"top", "person", "organizationalPerson", "user"})
	add.Attribute("sAMAccountName", []string{name})
	enabled := false
	for k, vs := range attrs {
		var ss []string
		for _, v := range vs {
			ss = append(ss, string(v))
		}
		add.Attribute(k, ss)
		if k == "userAccountControl" && len(vs) == 1 {
			uac, _ := strconv.Atoi(string(vs[0]))
			enabled = uac&2 == 0
		}
	}
	if enabled {
		add.Attribute("unicodePwd", []string{unicodePwd(adUserPass)})
	}
	if err := s.conn.Add(add); err != nil {
		t.Fatalf("add %s: %v", dn, err)
	}
	t.Cleanup(func() { _ = s.conn.Del(ldap.NewDelRequest(dn, nil)) })
	return dn
}

func (s *sambaSeeder) AddGroup(t testing.TB, name string, attrs map[string][][]byte) string {
	t.Helper()
	dn := "CN=" + name + ",CN=Users," + adBaseDN
	add := ldap.NewAddRequest(dn, nil)
	add.Attribute("objectClass", []string{"top", "group"})
	add.Attribute("sAMAccountName", []string{name})
	for k, vs := range attrs {
		var ss []string
		for _, v := range vs {
			ss = append(ss, string(v))
		}
		add.Attribute(k, ss)
	}
	if err := s.conn.Add(add); err != nil {
		t.Fatalf("add %s: %v", dn, err)
	}
	t.Cleanup(func() { _ = s.conn.Del(ldap.NewDelRequest(dn, nil)) })
	return dn
}

func (s *sambaSeeder) Account() (string, []byte) { return adBindDN, []byte(adAdminPassword) }

// unicodePwd is AD's encoding of a password set over LDAP: the quoted
// password in UTF-16LE.
func unicodePwd(password string) string {
	pw := utf16.Encode([]rune(`"` + password + `"`))
	b := make([]byte, 2*len(pw))
	for i, c := range pw {
		binary.LittleEndian.PutUint16(b[2*i:], c)
	}
	return string(b)
}

// wallClock is the host's clock, as the node hands identity one.
type wallClock struct{}

func (wallClock) Now() time.Time { return time.Now() }

func (wallClock) After(d time.Duration) <-chan struct{} {
	ch := make(chan struct{})
	time.AfterFunc(d, func() { close(ch) })
	return ch
}

// The directory client against Samba: the test call reads the base entry,
// the domain SID is the domain object's, and a user's answer carries its
// disabled bit and mail, found over a subtree of the naming context whose
// continuation references it ignores.
func TestDirectoryClient(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping AD-DC integration test in short mode")
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not found, skipping AD-DC integration test")
	}
	ctx := context.Background()
	dcIP := setupADDC(t)
	ca, err := os.ReadFile(copyADCA(t))
	if err != nil {
		t.Fatalf("read the fixture CA: %v", err)
	}
	seed := &sambaSeeder{conn: adminLDAP(t, dcIP, ca)}
	name := "t65-" + strconv.FormatInt(time.Now().UnixNano()%1e6, 10)
	dn := seed.AddUser(t, name, map[string][][]byte{"userAccountControl": {[]byte("514")}, "mail": {[]byte(name + "@corp.example")}})

	rec := directory.Record{Address: "ldaps://" + adDCHost, Base: adBaseDN, BindName: adBindDN, TrustRoots: string(ca), Realm: adRealm, DNSServer: dcIP}
	c := directory.NewClient(rec, directory.DialTLS, func(context.Context) ([]byte, error) { return []byte(adAdminPassword), nil }, wallClock{}, nil)
	defer c.Close()

	if e, err := c.Test(ctx); err != nil || !strings.EqualFold(e.DN, adBaseDN) {
		t.Errorf("Test = %q, %v; want the base %q", e.DN, err, adBaseDN)
	}
	sid, err := c.DomainSID(ctx)
	if err != nil || len(sid) == 0 {
		t.Fatalf("DomainSID = %x, %v; want the domain's", sid, err)
	}
	a, ok, err := c.Lookup(ctx, directory.ByUserName, []byte(name))
	if err != nil || !ok || !strings.EqualFold(a.DN, dn) {
		t.Fatalf("Lookup(%s) = %q, %v, %v; want %q", name, a.DN, ok, err, dn)
	}
	if a.Enabled || a.Mail != name+"@corp.example" || len(a.EntryID) != 16 || len(a.SID) != len(sid)+4 || !bytes.Equal(a.SID[8:len(sid)], sid[8:]) {
		t.Errorf("Lookup(%s) = enabled %v, mail %q, entry ID %x, SID %x; want disabled, its mail, a GUID, a SID of the domain %x",
			name, a.Enabled, a.Mail, a.EntryID, a.SID, sid)
	}
	if b, ok, err := c.Lookup(ctx, directory.BySID, a.SID); err != nil || !ok || !strings.EqualFold(b.DN, dn) {
		t.Errorf("Lookup by its SID = %q, %v, %v; want %q", b.DN, ok, err, dn)
	}
	if b, ok, err := c.Lookup(ctx, directory.ByEntryID, a.EntryID); err != nil || !ok || !strings.EqualFold(b.DN, dn) {
		t.Errorf("Lookup by its objectGUID = %q, %v, %v; want %q", b.DN, ok, err, dn)
	}

	// Its groups, nested, through tokenGroups and the SIDs looked up in
	// batches: both levels and Domain Users, its primary group.
	g1 := seed.AddGroup(t, name+"g1", map[string][][]byte{"member": {[]byte(dn)}})
	g2 := seed.AddGroup(t, name+"g2", map[string][][]byte{"member": {[]byte(g1)}})
	groups, err := c.Groups(ctx, a)
	if err != nil {
		t.Fatalf("Groups(%s): %v", name, err)
	}
	for _, want := range []string{g1, g2, "CN=Domain Users,CN=Users," + adBaseDN} {
		if !slices.ContainsFunc(groups, func(g directory.Answer) bool { return strings.EqualFold(g.DN, want) }) {
			t.Errorf("Groups(%s) lacks %s", name, want)
		}
	}
}
