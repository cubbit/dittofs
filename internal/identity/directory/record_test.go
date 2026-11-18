package directory_test

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"testing"
	"time"

	"github.com/cubbit/dittofs/internal/identity/directory"
)

// selfSigned is the PEM of a fresh self-signed certificate.
func selfSigned(t *testing.T) string {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "test CA"},
		NotBefore:    time.Unix(0, 0),
		NotAfter:     time.Unix(1<<32, 0),
		IsCA:         true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &k.PublicKey, k)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
}

func TestRecordValidate(t *testing.T) {
	ca := selfSigned(t)
	for _, tc := range []struct {
		name   string
		edit   func(*directory.Record)
		refuse bool
	}{
		{"ldaps", func(r *directory.Record) {}, false},
		{"ldap, upgraded with StartTLS", func(r *directory.Record) { r.Address = "ldap://dc1.corp.example" }, false},
		{"http", func(r *directory.Record) { r.Address = "http://dc1.corp.example" }, true},
		{"ldapi", func(r *directory.Record) { r.Address = "ldapi:///var/run/ldapi" }, true},
		{"empty address", func(r *directory.Record) { r.Address = "" }, true},
		{"no host", func(r *directory.Record) { r.Address = "ldaps://:636" }, true},
		{"trailing slash", func(r *directory.Record) { r.Address = "ldaps://dc1.corp.example/" }, false},
		{"userinfo with a password", func(r *directory.Record) { r.Address = "ldaps://binduser:secret@dc1.corp.example" }, true},
		{"userinfo, name only", func(r *directory.Record) { r.Address = "ldaps://binduser@dc1.corp.example" }, true},
		{"path", func(r *directory.Record) { r.Address = "ldaps://dc1.corp.example/x" }, true},
		{"query", func(r *directory.Record) { r.Address = "ldaps://dc1.corp.example?y" }, true},
		{"empty query", func(r *directory.Record) { r.Address = "ldaps://dc1.corp.example?" }, true},
		{"fragment", func(r *directory.Record) { r.Address = "ldaps://dc1.corp.example#z" }, true},
		{"unknown schema", func(r *directory.Record) { r.Schema = "nis" }, true},
		{"empty schema reads as ad", func(r *directory.Record) { r.Schema = "" }, false},
		{"lower-case ad realm", func(r *directory.Record) { r.Realm = "corp.example" }, true},
		{"one lower-case letter in an ad realm", func(r *directory.Record) { r.Realm = "CORP.EXAMPLe" }, true},
		{"lower-case realm, empty schema", func(r *directory.Record) { r.Schema, r.Realm = "", "corp.example" }, true},
		{"lower-case rfc2307 realm", func(r *directory.Record) { r.Schema, r.Realm = directory.SchemaRFC2307, "corp.example" }, false},
		{"trust roots", func(r *directory.Record) { r.TrustRoots = ca }, false},
		{"two trust roots", func(r *directory.Record) { r.TrustRoots = ca + ca }, false},
		{"trust roots not PEM", func(r *directory.Record) { r.TrustRoots = "not a certificate" }, true},
		{"trust roots with trailing text", func(r *directory.Record) { r.TrustRoots = ca + "junk" }, true},
		{"trust roots holding a non-certificate block", func(r *directory.Record) {
			r.TrustRoots = string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: []byte{1}}))
		}, true},
		{"trust roots holding a corrupt certificate", func(r *directory.Record) {
			r.TrustRoots = string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: []byte{1}}))
		}, true},
	} {
		r := directory.Record{
			Schema:        directory.SchemaAD,
			Address:       "ldaps://dc1.corp.example:636",
			Realm:         "CORP.EXAMPLE",
			NetBIOSDomain: "CORP",
		}
		tc.edit(&r)
		if err := r.Validate(); (err != nil) != tc.refuse {
			t.Errorf("%s: Validate(%+v) = %v, want refused %v", tc.name, r, err, tc.refuse)
		}
	}
}

// An ad record, or one naming no schema, names users and groups by
// sAMAccountName; an rfc2307 one by uid and cn; a set attribute wins.
func TestRecordNameAttrDefaults(t *testing.T) {
	for _, tc := range []struct {
		name        string
		r           directory.Record
		user, group string
		ad          bool
	}{
		{"no schema", directory.Record{}, "sAMAccountName", "sAMAccountName", true},
		{"ad", directory.Record{Schema: directory.SchemaAD}, "sAMAccountName", "sAMAccountName", true},
		{"rfc2307", directory.Record{Schema: directory.SchemaRFC2307}, "uid", "cn", false},
		{"set", directory.Record{Schema: directory.SchemaRFC2307, UserNameAttr: "mailNickname", GroupNameAttr: "displayName"}, "mailNickname", "displayName", false},
	} {
		if got := tc.r.UserAttr(); got != tc.user {
			t.Errorf("%s: UserAttr = %q, want %q", tc.name, got, tc.user)
		}
		if got := tc.r.GroupAttr(); got != tc.group {
			t.Errorf("%s: GroupAttr = %q, want %q", tc.name, got, tc.group)
		}
		if got := tc.r.IsAD(); got != tc.ad {
			t.Errorf("%s: IsAD = %t, want %t", tc.name, got, tc.ad)
		}
	}
}
