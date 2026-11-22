package directory

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"strconv"
	"strings"
	"unicode/utf16"

	"github.com/go-ldap/ldap/v3"

	"github.com/cubbit/dittofs/pkg/auth/sid"
)

// ComputerAccount is what Active Directory holds of a computer account.
type ComputerAccount struct {
	DN         string
	KeyVersion uint32 // msDS-KeyVersionNumber
	EncTypes   uint32 // msDS-SupportedEncryptionTypes; 0 when absent
	// Names is servicePrincipalName, each host/ name standing also for every
	// service sPNMappings maps to host, on that host name.
	Names      []string
	Owner      sid.SID // the security descriptor's owner
	Delegation []byte  // msDS-AllowedToActOnBehalfOfOtherIdentity, as stored
}

// NewComputer is a computer account to create.
type NewComputer struct {
	Name        string // without the $
	OU          string // the container's DN; empty for the domain's Computers container
	DNSHostName string // written as dNSHostName and in the fully qualified names
}

// computersGUID is the Computers container's well-known GUID, as
// wellKnownObjects names it.
const computersGUID = "AA312825768811D1ADED00C04FD8D5CD"

const delegationAttr = "msDS-AllowedToActOnBehalfOfOtherIdentity"

// sdOwnerOnly is LDAP_SERVER_SD_FLAGS_OID's value asking for the security
// descriptor's owner alone, BER SEQUENCE { INTEGER 1 }: without it Active
// Directory withholds nTSecurityDescriptor from a reader who may not read its
// SACL.
var sdOwnerOnly = ldap.NewControlString("1.2.840.113556.1.4.801", true, "\x30\x03\x02\x01\x01")

// ErrRefused is the directory's refusal of a bind's credentials, or of the
// rights the bound account holds.
var ErrRefused = errors.New("identity.directory: refused by the directory")

// refused is err, matching ErrRefused too when the directory refused the
// credentials or the rights.
func refused(err error) error {
	if ldap.IsErrorAnyOf(err, ldap.LDAPResultInvalidCredentials, ldap.LDAPResultInsufficientAccessRights, ldap.ErrorEmptyPassword) {
		return fmt.Errorf("%w: %w", err, ErrRefused)
	}
	return err
}

// dialBound dials rec over DialTLS and binds as name with password.
func dialBound(ctx context.Context, rec Record, name string, password []byte) (*ldapConn, error) {
	c, err := DialTLS(ctx, rec)
	if err != nil {
		return nil, err
	}
	lc := c.(*ldapConn)
	if err := lc.Bind(ctx, name, password); err != nil {
		_ = lc.Close()
		return nil, fmt.Errorf("identity.directory: bind as %s: %w", name, refused(err))
	}
	return lc, nil
}

// search runs one search on the connection, closed if ctx ends first.
func (c *ldapConn) search(ctx context.Context, sr *ldap.SearchRequest) (*ldap.SearchResult, error) {
	var res *ldap.SearchResult
	err := c.during(ctx, func() (err error) {
		res, err = c.l.Search(sr)
		return err
	})
	return res, err
}

// computerEntry is the computer account name$ under rec's base:
// fs.ErrNotExist when absent.
func (c *ldapConn) computerEntry(ctx context.Context, base, name string, attrs []string, controls ...ldap.Control) (*ldap.Entry, error) {
	res, err := c.search(ctx, ldap.NewSearchRequest(base, ldap.ScopeWholeSubtree, ldap.NeverDerefAliases, 0, 0, false,
		"(&(objectClass=computer)(sAMAccountName="+ldap.EscapeFilter(name)+"$))", attrs, controls))
	if err != nil {
		return nil, err
	}
	if len(res.Entries) != 1 {
		return nil, fmt.Errorf("identity.directory: computer account %s$: %w", name, fs.ErrNotExist)
	}
	return res.Entries[0], nil
}

// ReadComputer reads the computer account name$ bound as bindName, its names
// with the configuration partition's sPNMappings applied: fs.ErrNotExist
// when absent.
func ReadComputer(ctx context.Context, rec Record, bindName string, bindPassword []byte, name string) (ComputerAccount, error) {
	c, err := dialBound(ctx, rec, bindName, bindPassword)
	if err != nil {
		return ComputerAccount{}, err
	}
	defer func() { _ = c.Close() }()
	e, err := c.computerEntry(ctx, rec.Base, name, []string{"msDS-KeyVersionNumber", "msDS-SupportedEncryptionTypes",
		"servicePrincipalName", "nTSecurityDescriptor", delegationAttr}, sdOwnerOnly)
	if err != nil {
		return ComputerAccount{}, err
	}
	a := ComputerAccount{DN: e.DN, Delegation: e.GetRawAttributeValue(delegationAttr)}
	for attr, dst := range map[string]*uint32{"msDS-KeyVersionNumber": &a.KeyVersion, "msDS-SupportedEncryptionTypes": &a.EncTypes} {
		if v := e.GetAttributeValue(attr); v != "" {
			n, err := attrUint32(v)
			if err != nil {
				return ComputerAccount{}, fmt.Errorf("identity.directory: %s$'s %s %q: %w", name, attr, v, err)
			}
			*dst = n
		}
	}
	if a.Owner, err = sdOwner(e.GetRawAttributeValue("nTSecurityDescriptor")); err != nil {
		return ComputerAccount{}, fmt.Errorf("identity.directory: %s$'s nTSecurityDescriptor: %w", name, err)
	}
	hostTo, err := c.hostMappings(ctx)
	if err != nil {
		return ComputerAccount{}, err
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

// sdOwner is a self-relative security descriptor's owner (MS-DTYP 2.4.6).
func sdOwner(sd []byte) (sid.SID, error) {
	if len(sd) < 20 {
		return sid.SID{}, fmt.Errorf("%d bytes, shorter than a security descriptor's header", len(sd))
	}
	off := binary.LittleEndian.Uint32(sd[4:])
	if off == 0 || uint64(off) >= uint64(len(sd)) {
		return sid.SID{}, fmt.Errorf("owner offset %d outside its %d bytes", off, len(sd))
	}
	o, _, err := sid.DecodeSID(sd[off:])
	if err != nil {
		return sid.SID{}, err
	}
	return *o, nil
}

// hostMappings is the services the forest's sPNMappings maps to host, read
// from the configuration partition the root DSE names.
func (c *ldapConn) hostMappings(ctx context.Context) ([]string, error) {
	root, err := c.search(ctx, ldap.NewSearchRequest("", ldap.ScopeBaseObject, ldap.NeverDerefAliases, 0, 0, false,
		"(objectClass=*)", []string{"configurationNamingContext"}, nil))
	if err != nil {
		return nil, fmt.Errorf("identity.directory: read the root DSE's configurationNamingContext: %w", err)
	}
	if len(root.Entries) != 1 {
		return nil, fmt.Errorf("identity.directory: the root DSE search answered %d entries, want 1", len(root.Entries))
	}
	config := root.Entries[0].GetAttributeValue("configurationNamingContext")
	res, err := c.search(ctx, ldap.NewSearchRequest("CN=Directory Service,CN=Windows NT,CN=Services,"+config,
		ldap.ScopeBaseObject, ldap.NeverDerefAliases, 0, 0, false, "(objectClass=*)", []string{"sPNMappings"}, nil))
	if err != nil {
		return nil, fmt.Errorf("identity.directory: read sPNMappings under %s: %w", config, err)
	}
	if len(res.Entries) != 1 {
		return nil, fmt.Errorf("identity.directory: the sPNMappings search under %s answered %d entries, want 1", config, len(res.Entries))
	}
	return hostAliases(res.Entries[0].GetAttributeValues("sPNMappings")), nil
}

// hostAliases is the services the sPNMappings values map to host: each value
// is service=alias,alias,....
func hostAliases(mappings []string) []string {
	var hostTo []string
	for _, m := range mappings {
		if k, v, ok := strings.Cut(m, "="); ok && strings.EqualFold(strings.TrimSpace(k), "host") {
			for _, a := range strings.Split(v, ",") {
				if a = strings.TrimSpace(a); a != "" {
					hostTo = append(hostTo, a)
				}
			}
		}
	}
	return hostTo
}

// computersContainer is the DN of the Computers container the domain at base
// names in its wellKnownObjects, which redircmp rewrites.
func (c *ldapConn) computersContainer(ctx context.Context, base string) (string, error) {
	res, err := c.search(ctx, ldap.NewSearchRequest(base, ldap.ScopeBaseObject, ldap.NeverDerefAliases, 0, 0, false,
		"(objectClass=*)", []string{"wellKnownObjects"}, nil))
	if err != nil {
		return "", fmt.Errorf("identity.directory: read %s's wellKnownObjects: %w", base, err)
	}
	if len(res.Entries) != 1 {
		return "", fmt.Errorf("identity.directory: the wellKnownObjects search at %s answered %d entries, want 1", base, len(res.Entries))
	}
	if dn, ok := wellKnownDN(res.Entries[0].GetAttributeValues("wellKnownObjects"), computersGUID); ok {
		return dn, nil
	}
	return "", fmt.Errorf("identity.directory: %s's wellKnownObjects names no Computers container", base)
}

// wellKnownDN is the DN the wellKnownObjects values, each B:32:<GUID>:<DN>,
// name for guid, and whether one does.
func wellKnownDN(values []string, guid string) (string, bool) {
	for _, v := range values {
		if parts := strings.SplitN(v, ":", 4); len(parts) == 4 && strings.EqualFold(parts[2], guid) && parts[3] != "" {
			return parts[3], true
		}
	}
	return "", false
}

// CreateComputer creates the computer account c as adminName: its host/ and
// cifs/ names for the short and the DNS host name, msDS-SupportedEncryptionTypes
// 0x18, and password. fs.ErrExist, changing nothing, when the name is taken.
// An empty c.OU creates it in the Computers container the domain names.
//
// decision: the password goes in the add itself, so Active Directory numbers
// the account's first key version 1; an add without a password and a set
// after it, as adcli does, numbers it 2. The join seals key version 1, which
// holds only while this add carries the password, and its confirm reports a
// mismatch.
func CreateComputer(ctx context.Context, rec Record, adminName string, adminPassword []byte, c NewComputer, password []byte) error {
	conn, err := dialBound(ctx, rec, adminName, adminPassword)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	ou := c.OU
	if ou == "" {
		if ou, err = conn.computersContainer(ctx, rec.Base); err != nil {
			return err
		}
	}
	add := ldap.NewAddRequest("CN="+ldap.EscapeDN(c.Name)+","+ou, nil)
	add.Attribute("objectClass", []string{"top", "person", "organizationalPerson", "user", "computer"})
	add.Attribute("sAMAccountName", []string{c.Name + "$"})
	add.Attribute("userAccountControl", []string{"4096"}) // WORKSTATION_TRUST_ACCOUNT
	add.Attribute("dNSHostName", []string{c.DNSHostName})
	add.Attribute("servicePrincipalName", []string{"HOST/" + c.Name, "HOST/" + c.DNSHostName,
		"cifs/" + c.Name, "cifs/" + c.DNSHostName})
	add.Attribute("msDS-SupportedEncryptionTypes", []string{"24"})
	add.Attribute("unicodePwd", []string{unicodePwd(password)})
	err = conn.during(ctx, func() error { return conn.l.Add(add) })
	if ldap.IsErrorWithCode(err, ldap.LDAPResultEntryAlreadyExists) {
		return fmt.Errorf("identity.directory: computer account %s$: %w: %w", c.Name, err, fs.ErrExist)
	}
	if err != nil {
		return fmt.Errorf("identity.directory: create computer account %s$ in %s: %w", c.Name, ou, refused(err))
	}
	return nil
}

// ResetComputer sets the existing computer account name$'s password as
// adminName: fs.ErrNotExist when absent.
func ResetComputer(ctx context.Context, rec Record, adminName string, adminPassword []byte, name string, password []byte) error {
	conn, err := dialBound(ctx, rec, adminName, adminPassword)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	e, err := conn.computerEntry(ctx, rec.Base, name, []string{"cn"})
	if err != nil {
		return err
	}
	mod := ldap.NewModifyRequest(e.DN, nil)
	mod.Replace("unicodePwd", []string{unicodePwd(password)})
	if err := conn.during(ctx, func() error { return conn.l.Modify(mod) }); err != nil {
		return fmt.Errorf("identity.directory: reset %s$'s password: %w", name, refused(err))
	}
	return nil
}

// unicodePwd renders password as the value Active Directory's unicodePwd
// attribute takes: the password in double quotes, encoded UTF-16LE, written
// only over a verified TLS connection. go-ldap sends a string's bytes as the
// value, so the UTF-16LE bytes are packed into one.
func unicodePwd(password []byte) string {
	codes := utf16.Encode([]rune(`"` + string(password) + `"`))
	buf := make([]byte, 0, 2*len(codes))
	for _, c := range codes {
		buf = append(buf, byte(c), byte(c>>8))
	}
	return string(buf)
}

// attrUint32 reads a 32-bit integer attribute as its bits: AD writes
// msDS-SupportedEncryptionTypes signed and msDS-KeyVersionNumber unsigned, so
// either reading is taken and anything outside both is refused.
func attrUint32(v string) (uint32, error) {
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return 0, err
	}
	if n < math.MinInt32 || n > math.MaxUint32 {
		return 0, fmt.Errorf("%d is not a 32-bit integer", n)
	}
	return uint32(n), nil
}

// StrictlyUnder reports whether dn is a distinguished name below base, never
// base itself, compared RDN by RDN with ASCII letters folded. An empty base,
// or a dn or base that does not parse, is not.
func StrictlyUnder(dn, base string) bool {
	b, err := ldap.ParseDN(base)
	if err != nil || len(b.RDNs) == 0 {
		return false
	}
	d, err := ldap.ParseDN(dn)
	return err == nil && b.AncestorOfFold(d)
}
