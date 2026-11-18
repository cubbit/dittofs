package directory

import (
	"context"
	"encoding/binary"
	"fmt"
	"io/fs"
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

// dialBound dials rec over DialTLS and binds as name with password.
func dialBound(ctx context.Context, rec Record, name string, password []byte) (*ldapConn, error) {
	c, err := DialTLS(ctx, rec)
	if err != nil {
		return nil, err
	}
	lc := c.(*ldapConn)
	if err := lc.Bind(ctx, name, password); err != nil {
		_ = lc.Close()
		return nil, fmt.Errorf("identity.directory: bind as %s: %w", name, err)
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
			// msDS-SupportedEncryptionTypes is a signed 32-bit integer.
			n, err := strconv.ParseInt(v, 10, 64)
			if err != nil {
				return ComputerAccount{}, fmt.Errorf("identity.directory: %s$'s %s %q: %w", name, attr, v, err)
			}
			*dst = uint32(n)
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
	if err != nil || len(root.Entries) != 1 {
		return nil, fmt.Errorf("identity.directory: read the root DSE's configurationNamingContext: %v", err)
	}
	config := root.Entries[0].GetAttributeValue("configurationNamingContext")
	res, err := c.search(ctx, ldap.NewSearchRequest("CN=Directory Service,CN=Windows NT,CN=Services,"+config,
		ldap.ScopeBaseObject, ldap.NeverDerefAliases, 0, 0, false, "(objectClass=*)", []string{"sPNMappings"}, nil))
	if err != nil || len(res.Entries) != 1 {
		return nil, fmt.Errorf("identity.directory: read sPNMappings under %s: %v", config, err)
	}
	var hostTo []string
	for _, m := range res.Entries[0].GetAttributeValues("sPNMappings") {
		if k, v, ok := strings.Cut(m, "="); ok && strings.EqualFold(k, "host") {
			hostTo = append(hostTo, strings.Split(v, ",")...)
		}
	}
	return hostTo, nil
}

// computersContainer is the DN of the Computers container the domain at base
// names in its wellKnownObjects, which redircmp rewrites.
func (c *ldapConn) computersContainer(ctx context.Context, base string) (string, error) {
	res, err := c.search(ctx, ldap.NewSearchRequest(base, ldap.ScopeBaseObject, ldap.NeverDerefAliases, 0, 0, false,
		"(objectClass=*)", []string{"wellKnownObjects"}, nil))
	if err != nil || len(res.Entries) != 1 {
		return "", fmt.Errorf("identity.directory: read %s's wellKnownObjects: %v", base, err)
	}
	// Each value is B:32:<GUID>:<DN>.
	for _, v := range res.Entries[0].GetAttributeValues("wellKnownObjects") {
		if parts := strings.SplitN(v, ":", 4); len(parts) == 4 && strings.EqualFold(parts[2], computersGUID) {
			return parts[3], nil
		}
	}
	return "", fmt.Errorf("identity.directory: %s's wellKnownObjects names no Computers container", base)
}

// CreateComputer creates the computer account c as adminName: its host/ and
// cifs/ names for the short and the DNS host name, msDS-SupportedEncryptionTypes
// 0x18, and password. fs.ErrExist, changing nothing, when the name is taken.
// An empty c.OU creates it in the Computers container the domain names.
//
// decision: the password goes in the add itself, so Active Directory numbers
// the account's first key version 1; an add without a password and a set
// after it, as adcli and v1 did, numbers it 2. Whoever seals the version reads
// the number back with ReadComputer rather than assuming either, so this line
// alone changes if the add-then-set order is chosen.
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
		return fmt.Errorf("identity.directory: create computer account %s$ in %s: %w", c.Name, ou, err)
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
		return fmt.Errorf("identity.directory: reset %s$'s password: %w", name, err)
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
