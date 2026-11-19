//go:build ad_dc

package ad_dc_test

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"net"
	"os"
	"os/exec"
	"strconv"
	"testing"
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
				c, err := directory.DialTLS(context.Background(), rec)
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
