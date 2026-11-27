// Package directory is identity's directory: the installation's one LDAP
// directory, Active Directory or RFC 2307, and the record that names it. It
// is the only package of the module that speaks LDAP.
package directory

import (
	"bytes"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"net/url"
	"regexp"

	"github.com/cubbit/dittofs/pkg/metadata/entity"
)

// The schemas a record may name.
const (
	SchemaAD      = "ad"      // Active Directory's attributes (MS-ADTS)
	SchemaRFC2307 = "rfc2307" // RFC 2307's attributes
)

// Record is the identity.directory section: the one directory, nil in
// identity's State when the installation has none.
type Record struct {
	// Schema is SchemaAD or SchemaRFC2307; empty reads as SchemaAD.
	Schema string `json:"schema" class:"live"`
	// Address is ldaps://host[:port] or ldap://host[:port]; the latter is
	// always upgraded with StartTLS before the bind.
	Address string `json:"address" class:"live"`
	// Base is the distinguished name every search starts from.
	Base string `json:"base" class:"live"`
	// BindName is the distinguished name or UPN the simple bind sends.
	BindName string `json:"bind_name" class:"live"`
	// BindCredential names the bind password, a Secret of kind directory
	// credential, never the password itself.
	BindCredential entity.SecretRef `json:"bind_credential" class:"live"`
	// TrustRoots is the PEM of the certificates the directory's chain must
	// end in; empty trusts the host's roots.
	TrustRoots string `json:"trust_roots" class:"live"`
	// Realm is the Kerberos realm the directory serves.
	Realm string `json:"realm" class:"live"`
	// NetBIOSDomain is the domain's NetBIOS name.
	NetBIOSDomain string `json:"netbios_domain" class:"live"`
	// UserNameAttr and GroupNameAttr are the attributes holding a user's and
	// a group's name; empty reads as the schema's default (UserAttr,
	// GroupAttr).
	UserNameAttr  string `json:"user_name_attr" class:"live"`
	GroupNameAttr string `json:"group_name_attr" class:"live"`
	// DCAddress and DNSServer, when set, are used instead of the domain's
	// DNS service records.
	DCAddress string `json:"dc_address" class:"live"`
	DNSServer string `json:"dns_server" class:"live"`
	// RequirePAC is whether a Kerberos ticket in Realm must carry a PAC;
	// read it through PACRequired.
	//
	// decision: the field is a pointer, nil reading as true, so a record
	// with no require_pac row, and one built without the field, requires
	// the PAC: a missing row read as false would let a PAC-less ticket in
	// the directory's realm reach a same-named local user, failing open.
	// Overturned if the settings read ever hands every record its default
	// whole, which would let a plain bool defaulting to true stand.
	RequirePAC *bool `json:"require_pac" class:"live"`
}

// PACRequired reports whether a Kerberos ticket in the record's realm must
// carry a PAC: true unless RequirePAC is set false.
func (r Record) PACRequired() bool { return r.RequirePAC == nil || *r.RequirePAC }

// IsAD reports whether the record names an Active Directory, the schema an
// empty Schema reads as.
func (r Record) IsAD() bool { return r.Schema == "" || r.Schema == SchemaAD }

// UserAttr is the attribute holding a user's name: UserNameAttr, else
// sAMAccountName for an ad directory and uid for an rfc2307 one.
func (r Record) UserAttr() string {
	switch {
	case r.UserNameAttr != "":
		return r.UserNameAttr
	case r.IsAD():
		return "sAMAccountName"
	}
	return "uid"
}

// GroupAttr is the attribute holding a group's name: GroupNameAttr, else
// sAMAccountName for an ad directory and cn for an rfc2307 one.
func (r Record) GroupAttr() string {
	switch {
	case r.GroupNameAttr != "":
		return r.GroupNameAttr
	case r.IsAD():
		return "sAMAccountName"
	}
	return "cn"
}

// Validate refuses an unknown schema, an address that is not ldaps:// or
// ldap:// with a host and an optional port and nothing more (no userinfo,
// path, query or fragment), trust roots that are not PEM certificates, an
// ad realm holding a lower-case ASCII letter, since a ticket's realm is
// compared exactly, and a name attribute that is neither an RFC 4512 descr
// nor a numeric OID, since it is placed in a filter unescaped.
//
// decision: TLS is the address's scheme and nothing turns it off: ldaps://
// dials TLS, ldap:// always upgrades with StartTLS before the bind, and the
// record has no field that skips certificate verification or allows plain
// LDAP, so a record asking for either names a field the strict decode
// refuses. A bind password crossing the network in clear, or to an
// unverified peer, hands the directory account to whoever is on the path.
// Overturned only if a directory is ever reached over a channel already
// authenticated and encrypted below LDAP.
func (r Record) Validate() error {
	if !r.IsAD() && r.Schema != SchemaRFC2307 {
		return fmt.Errorf("schema %q is neither %q nor %q", r.Schema, SchemaAD, SchemaRFC2307)
	}
	u, err := url.Parse(r.Address)
	if err != nil {
		return fmt.Errorf("address: %w", err)
	}
	if u.Scheme != "ldaps" && u.Scheme != "ldap" {
		return fmt.Errorf("address %q is not ldaps:// or ldap://", r.Address)
	}
	if u.Hostname() == "" {
		return fmt.Errorf("address %q names no host", r.Address)
	}
	// A password in the address would sit in a plain setting, past the
	// BindCredential reference meant to hold it.
	if u.User != nil || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return fmt.Errorf("address %q holds more than ldaps:// or ldap://, a host and a port", r.Address)
	}
	if err := checkPEM(r.TrustRoots); err != nil {
		return err
	}
	for field, attr := range map[string]string{"user_name_attr": r.UserNameAttr, "group_name_attr": r.GroupNameAttr} {
		if attr != "" && !attrName.MatchString(attr) {
			return fmt.Errorf("%s %q is neither an attribute name nor a numeric OID", field, attr)
		}
	}
	if r.IsAD() {
		for i := range len(r.Realm) {
			if c := r.Realm[i]; 'a' <= c && c <= 'z' {
				return fmt.Errorf("ad realm %q holds the lower-case letter %q", r.Realm, c)
			}
		}
	}
	return nil
}

// attrName is an RFC 4512 oid: a descr, ALPHA *(ALPHA / DIGIT / "-"), or a
// numericoid, numbers with no leading zero joined by dots.
var attrName = regexp.MustCompile(`^(?:[A-Za-z][A-Za-z0-9-]*|(?:0|[1-9][0-9]*)(?:\.(?:0|[1-9][0-9]*))+)$`)

// checkPEM refuses trust roots holding anything but PEM certificates, or
// none at all when set.
func checkPEM(s string) error {
	if s == "" {
		return nil
	}
	rest, n := []byte(s), 0
	for {
		var b *pem.Block
		b, rest = pem.Decode(rest)
		if b == nil {
			break
		}
		if _, err := x509.ParseCertificate(b.Bytes); err != nil {
			return fmt.Errorf("trust_roots: %w", err)
		}
		n++
	}
	if n == 0 || len(bytes.TrimSpace(rest)) != 0 {
		return errors.New("trust_roots is not a list of PEM certificates")
	}
	return nil
}
