// Package identitytest holds the doubles identity's tests share: the
// in-memory directory, which DirectoryConformance holds to a real one, and
// the fault wrapper around a directory.
package identitytest

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/cubbit/dittofs/internal/identity/directory"
)

// Directory is an in-memory directory: entries by distinguished name under
// one naming context, served through Dial as a directory.Conn. It answers as
// Active Directory does where DirectoryConformance looks: a simple bind
// against a stored password, no search but the root DSE's before it, base,
// one-level and subtree scopes, RFC 4515 filters with string values compared
// case-insensitively and objectSid and objectGUID byte for byte, no such
// object for a search based outside its naming context, and continuation
// references to the configuration and DNS partitions for a subtree search of
// the naming context itself.
//
// It reads no paging, no extensible match, none of the constructed
// tokenGroups attributes and none of the selectors *, + and 1.1: a request
// asking for one is refused, where a domain controller would answer it.
type Directory struct {
	base      string // the naming context
	domainSID []byte // the naming context's objectSid

	mu        sync.Mutex
	entries   map[string]*entry // by DN, lower-cased
	passwords map[string][]byte // by DN, lower-cased
	nextRID   uint32
}

// entry is one entry. Its attributes are keyed by the name they were stored
// under and looked up case-insensitively.
type entry struct {
	dn    string
	attrs map[string][][]byte
}

func (e *entry) get(name string) [][]byte {
	for k, v := range e.attrs {
		if strings.EqualFold(k, name) {
			return v
		}
	}
	return nil
}

// binaryAttr reports whether the attribute's values are compared byte for
// byte, as a directory compares an octet string, rather than as text.
func binaryAttr(name string) bool {
	return strings.EqualFold(name, "objectSid") || strings.EqualFold(name, "objectGUID")
}

// adminPassword is the in-memory directory's administrator's password.
const adminPassword = "in-memory directory administrator"

// NewDirectory returns a directory whose naming context is base, a DN of DC=
// components, with a random domain SID, the Users container, and an
// administrator whose name and password Account returns.
func NewDirectory(base string) *Directory {
	sid := make([]byte, 24)
	sid[0], sid[1], sid[7] = 1, 4, 5 // revision 1, four sub-authorities, authority 5
	binary.LittleEndian.PutUint32(sid[8:], 21)
	_, _ = rand.Read(sid[12:])

	d := &Directory{
		base:      base,
		domainSID: sid,
		entries:   map[string]*entry{},
		passwords: map[string][]byte{},
		nextRID:   1100,
	}
	d.put("", map[string][][]byte{
		"objectClass":          {[]byte("top")},
		"defaultNamingContext": {[]byte(base)},
		"namingContexts":       {[]byte(base)},
	})
	d.put(base, map[string][][]byte{
		"objectClass": {[]byte("top"), []byte("domain"), []byte("domainDNS")},
		"objectSid":   {sid},
	})
	d.put("CN=Users,"+base, map[string][][]byte{
		"objectClass": {[]byte("top"), []byte("container")},
		"cn":          {[]byte("Users")},
	})
	admin := d.AddUser(nil, "Administrator", nil)
	d.passwords[strings.ToLower(admin)] = []byte(adminPassword)
	return d
}

func (d *Directory) put(dn string, attrs map[string][][]byte) {
	d.entries[strings.ToLower(dn)] = &entry{dn: dn, attrs: attrs}
}

// AddUser adds a user named name under the Users container, its
// sAMAccountName and cn name, with an objectSid of the domain's and a random
// objectGUID, then attrs over those, and returns its DN.
func (d *Directory) AddUser(_ testing.TB, name string, attrs map[string][][]byte) string {
	d.mu.Lock()
	defer d.mu.Unlock()

	sid := append(slices.Clone(d.domainSID), 0, 0, 0, 0)
	sid[1]++
	binary.LittleEndian.PutUint32(sid[24:], d.nextRID)
	d.nextRID++
	guid := make([]byte, 16)
	_, _ = rand.Read(guid)

	dn := "CN=" + name + ",CN=Users," + d.base
	all := map[string][][]byte{
		"objectClass":    {[]byte("top"), []byte("person"), []byte("organizationalPerson"), []byte("user")},
		"cn":             {[]byte(name)},
		"sAMAccountName": {[]byte(name)},
		"objectSid":      {sid},
		"objectGUID":     {guid},
	}
	for k, v := range attrs {
		for have := range all {
			if strings.EqualFold(have, k) {
				delete(all, have)
			}
		}
		all[k] = v
	}
	d.put(dn, all)
	return dn
}

// Account returns the administrator's DN and password.
func (d *Directory) Account() (name string, password []byte) {
	return "CN=Administrator,CN=Users," + d.base, []byte(adminPassword)
}

// Dial returns a new connection to the directory; it is a directory.Dialer,
// reading nothing of rec.
func (d *Directory) Dial(ctx context.Context, _ directory.Record) (directory.Conn, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return &memConn{d: d}, nil
}

var _ directory.Dialer = (*Directory)(nil).Dial

// unread are the attribute names a domain controller answers and the
// in-memory directory cannot: the constructed token groups, which it does not
// compute, and the selectors, which selected would read as names no entry has.
var unread = []string{"tokenGroups", "tokenGroupsGlobalAndUniversal", "tokenGroupsNoGCAcceptable", "*", "+", "1.1"}

// errClosed is a call on a closed connection.
var errClosed = errors.New("identitytest: the connection is closed")

// memConn is one connection to a Directory.
type memConn struct {
	d *Directory

	mu     sync.Mutex
	bound  bool
	closed bool
}

func (c *memConn) Bind(ctx context.Context, name string, password []byte) error {
	if err := c.check(ctx); err != nil {
		return err
	}
	c.d.mu.Lock()
	want, ok := c.d.passwords[strings.ToLower(name)]
	c.d.mu.Unlock()

	c.mu.Lock()
	defer c.mu.Unlock()
	c.bound = ok && len(password) > 0 && string(want) == string(password)
	if !c.bound {
		return fmt.Errorf("identitytest: bind as %q: invalid credentials", name)
	}
	return nil
}

func (c *memConn) Search(ctx context.Context, req directory.Request) (directory.Result, error) {
	if err := c.check(ctx); err != nil {
		return directory.Result{}, err
	}
	if req.PageSize > 0 || req.Cookie != nil {
		return directory.Result{}, errors.New("identitytest: the in-memory directory reads no paged search")
	}
	for _, a := range req.Attrs {
		if slices.ContainsFunc(unread, func(u string) bool { return strings.EqualFold(u, a) }) {
			return directory.Result{}, fmt.Errorf("identitytest: the in-memory directory reads no attribute %q", a)
		}
	}
	f, err := parseFilter(req.Filter)
	if err != nil {
		return directory.Result{}, err
	}
	c.mu.Lock()
	bound := c.bound
	c.mu.Unlock()

	d := c.d
	if req.Base == "" && req.Scope == directory.ScopeBase {
		bound = true // the root DSE is read before a bind
	}
	if !bound {
		return directory.Result{}, errors.New("identitytest: a search before a successful bind")
	}
	if !under(req.Base, d.base) && req.Base != "" {
		return directory.Result{}, fmt.Errorf("identitytest: no such object %q", req.Base)
	}

	d.mu.Lock()
	defer d.mu.Unlock()
	if _, ok := d.entries[strings.ToLower(req.Base)]; !ok {
		return directory.Result{}, fmt.Errorf("identitytest: no such object %q", req.Base)
	}
	var res directory.Result
	for _, e := range d.entries {
		if inScope(e.dn, req.Base, req.Scope) && f.match(e) {
			res.Entries = append(res.Entries, e.selected(req.Attrs))
		}
	}
	if req.Scope == directory.ScopeSubtree && strings.EqualFold(req.Base, d.base) {
		host, _ := dnsName(d.base)
		for _, p := range []string{"CN=Configuration,", "DC=DomainDnsZones,", "DC=ForestDnsZones,"} {
			res.Referrals = append(res.Referrals, "ldap://"+host+"/"+p+d.base)
		}
	}
	slices.SortFunc(res.Entries, func(a, b directory.Entry) int { return strings.Compare(a.DN, b.DN) })
	return res, nil
}

func (c *memConn) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed = true
	return nil
}

func (c *memConn) check(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return errClosed
	}
	return nil
}

// selected copies the entry with the attributes attrs names, or all of them
// when it names none.
func (e *entry) selected(attrs []string) directory.Entry {
	out := directory.Entry{DN: e.dn, Attrs: map[string][][]byte{}}
	for k, vs := range e.attrs {
		if len(attrs) == 0 || slices.ContainsFunc(attrs, func(a string) bool { return strings.EqualFold(a, k) }) {
			cp := make([][]byte, len(vs))
			for i, v := range vs {
				cp[i] = slices.Clone(v)
			}
			out.Attrs[k] = cp
		}
	}
	return out
}

// under reports whether dn is base or below it.
func under(dn, base string) bool {
	dn, base = strings.ToLower(dn), strings.ToLower(base)
	return dn == base || strings.HasSuffix(dn, ","+base)
}

// inScope reports whether dn is within scope of base.
func inScope(dn, base string, scope directory.Scope) bool {
	switch scope {
	case directory.ScopeBase:
		return strings.EqualFold(dn, base)
	case directory.ScopeOne:
		_, parent, ok := strings.Cut(dn, ",")
		return ok && strings.EqualFold(parent, base)
	}
	return base != "" && under(dn, base)
}

// dnsName is the DNS name a DN of DC= components stands for, as a directory
// names the host of a continuation reference; false when any component is
// not DC=.
func dnsName(dn string) (string, bool) {
	var labels []string
	for rdn := range strings.SplitSeq(dn, ",") {
		k, v, ok := strings.Cut(strings.TrimSpace(rdn), "=")
		if !ok || !strings.EqualFold(k, "DC") {
			return "", false
		}
		labels = append(labels, v)
	}
	return strings.Join(labels, "."), true
}
