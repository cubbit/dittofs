// Package identitytest holds the doubles identity's tests share: the
// in-memory directory and domain controller, which DirectoryConformance and
// DomainControllerConformance hold to real ones, and the fault wrapper around
// each.
package identitytest

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"
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
// the naming context itself. It pages a search as RFC 2696 says, refusing a
// cookie on a connection other than the one that handed it out, matches
// LDAP_MATCHING_RULE_BIT_AND, constructs
// tokenGroups, of security groups only, on a base-scope read, and hands back
// a range of a multi-valued attribute asked for as attr;range=<lo>-<hi|*>,
// at most MaxValRange values of it.
//
// It reads no other extensible match, none of the other constructed
// tokenGroups attributes and none of the selectors *, + and 1.1: a request
// asking for one is refused, where a domain controller would answer it. An
// attribute asked for without a range is answered whole, as Samba answers
// it, where Active Directory would hand back its first range.
type Directory struct {
	base      string // the naming context
	domainSID []byte // the naming context's objectSid

	mu          sync.Mutex
	entries     map[string]*entry // by DN, lower-cased
	passwords   map[string][]byte // by DN, lower-cased
	nextRID     uint32
	maxValRange int  // the most values of one range
	maxPageSize int  // the most entries of one page
	noTokens    bool // tokenGroups answered empty
}

// domainUsersRID is the RID of Domain Users, every user's primary group
// unless its primaryGroupID names another.
const domainUsersRID = 513

// globalSecurity is the groupType of a global security group (MS-ADTS
// 2.2.12), as a signed integer: the security bit and the global scope.
const globalSecurity = "-2147483646"

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
		// Active Directory's default MaxValRange and MaxPageSize.
		maxValRange: 1500,
		maxPageSize: 1000,
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
	d.add("Domain Users", []string{"top", "group"}, domainUsersRID, map[string][][]byte{"groupType": {[]byte(globalSecurity)}})
	admin := d.AddUser(nil, "Administrator", nil)
	d.passwords[strings.ToLower(admin)] = []byte(adminPassword)
	return d
}

func (d *Directory) put(dn string, attrs map[string][][]byte) {
	d.entries[strings.ToLower(dn)] = &entry{dn: dn, attrs: attrs}
}

// AddUser adds a user named name under the Users container, its
// sAMAccountName and cn name, with an objectSid of the domain's, a random
// objectGUID and Domain Users its primaryGroupID, then attrs over those, and
// returns its DN.
func (d *Directory) AddUser(_ testing.TB, name string, attrs map[string][][]byte) string {
	d.mu.Lock()
	defer d.mu.Unlock()
	pg := map[string][][]byte{"primaryGroupID": {[]byte(strconv.Itoa(domainUsersRID))}}
	maps.Copy(pg, attrs)
	return d.add(name, []string{"top", "person", "organizationalPerson", "user"}, d.rid(), pg)
}

// AddGroup adds a group named name under the Users container, its
// sAMAccountName and cn name, with an objectSid of the domain's, a random
// objectGUID and the groupType of a global security group, then attrs over
// those, member among them, and returns its DN.
func (d *Directory) AddGroup(_ testing.TB, name string, attrs map[string][][]byte) string {
	d.mu.Lock()
	defer d.mu.Unlock()
	gt := map[string][][]byte{"groupType": {[]byte(globalSecurity)}}
	maps.Copy(gt, attrs)
	return d.add(name, []string{"top", "group"}, d.rid(), gt)
}

// SetMaxValRange sets the most values one range of an attribute holds, 1 500
// unless set, as Active Directory's MaxValRange.
func (d *Directory) SetMaxValRange(n int) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.maxValRange = n
}

// SetMaxPageSize sets the most entries one page of a paged search holds,
// whatever page size the search asks for, 1 000 unless set, as Active
// Directory's MaxPageSize.
func (d *Directory) SetMaxPageSize(n int) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.maxPageSize = n
}

// HideTokenGroups answers tokenGroups empty from then on, as Active
// Directory answers a bind that may not read it.
func (d *Directory) HideTokenGroups() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.noTokens = true
}

// rid hands out the next RID; d.mu is held.
func (d *Directory) rid() uint32 {
	d.nextRID++
	return d.nextRID - 1
}

// sid is the domain's SID with rid appended.
func (d *Directory) sid(rid uint32) []byte { return directory.WithRID(d.domainSID, rid) }

// add puts an entry named name under the Users container, of classes, with
// rid's SID and a random objectGUID, then attrs over those, and returns its
// DN; d.mu is held, or d is not yet shared.
func (d *Directory) add(name string, classes []string, rid uint32, attrs map[string][][]byte) string {
	guid := make([]byte, 16)
	_, _ = rand.Read(guid)
	var oc [][]byte
	for _, c := range classes {
		oc = append(oc, []byte(c))
	}

	dn := "CN=" + name + ",CN=Users," + d.base
	all := map[string][][]byte{
		"objectClass":    oc,
		"cn":             {[]byte(name)},
		"sAMAccountName": {[]byte(name)},
		"objectSid":      {d.sid(rid)},
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
// in-memory directory cannot: the constructed token groups but tokenGroups,
// which it does not compute, and the selectors, which selected would read as
// names no entry has.
var unread = []string{"tokenGroupsGlobalAndUniversal", "tokenGroupsNoGCAcceptable", "*", "+", "1.1"}

// errClosed is a call on a closed connection, as a real one fails it.
var errClosed = fmt.Errorf("%w: identitytest", directory.ErrClosed)

// memConn is one connection to a Directory.
type memConn struct {
	d *Directory

	mu      sync.Mutex
	bound   bool
	closed  bool
	cookies map[string]bool // the paged searches' cookies this connection handed out
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
	for _, a := range req.Attrs {
		if strings.EqualFold(a, "tokenGroups") && req.Scope != directory.ScopeBase {
			return directory.Result{}, errors.New("identitytest: tokenGroups is read with base scope only")
		}
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
	// A directory holds a paged search's state per connection, so a cookie
	// another connection handed out names nothing here.
	known := req.Cookie == nil || c.cookies[string(req.Cookie)]
	c.mu.Unlock()
	if req.PageSize > 0 && !known {
		return directory.Result{}, fmt.Errorf("identitytest: a paged search's cookie %q this connection did not hand out", req.Cookie)
	}

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
		if inScope(e.dn, req.Base, req.Scope) && f.match(e, d) {
			out, err := d.selected(e, req.Attrs)
			if err != nil {
				return directory.Result{}, err
			}
			res.Entries = append(res.Entries, out)
		}
	}
	if req.Scope == directory.ScopeSubtree && strings.EqualFold(req.Base, d.base) {
		host, _ := dnsName(d.base)
		for _, p := range []string{"CN=Configuration,", "DC=DomainDnsZones,", "DC=ForestDnsZones,"} {
			res.Referrals = append(res.Referrals, "ldap://"+host+"/"+p+d.base)
		}
	}
	slices.SortFunc(res.Entries, func(a, b directory.Entry) int { return strings.Compare(a.DN, b.DN) })
	if req.PageSize == 0 {
		return res, nil
	}
	res, err = page(res, req, d.maxPageSize)
	if err == nil && res.Cookie != nil {
		c.mu.Lock()
		if c.cookies == nil {
			c.cookies = map[string]bool{}
		}
		c.cookies[string(res.Cookie)] = true
		c.mu.Unlock()
	}
	return res, err
}

// page is the page of res req asks for, of at most most entries. The cookie
// is the offset of the page's first entry in the whole answer, which is
// sorted by DN.
//
// ponytail: the cookie holds no state, so an entry added or removed between
// two pages shifts the rest by one, where a domain controller pages a
// snapshot. Keep a per-connection snapshot when a test pages a directory it
// changes.
func page(res directory.Result, req directory.Request, most int) (directory.Result, error) {
	off := 0
	if req.Cookie != nil {
		n, err := strconv.Atoi(string(req.Cookie))
		if err != nil || n <= 0 || n > len(res.Entries) {
			return directory.Result{}, fmt.Errorf("identitytest: a paged search's cookie %q is not one it handed out", req.Cookie)
		}
		off = n
	}
	all := res.Entries
	end := min(off+min(req.PageSize, most), len(all))
	res.Entries, res.Cookie = all[off:end], nil
	if end < len(all) {
		res.Cookie = []byte(strconv.Itoa(end))
	}
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
// when it names none: a stored one whole, one asked as attr;range=<lo>-<hi|*>
// as that range, and tokenGroups constructed; d.mu is held.
func (d *Directory) selected(e *entry, attrs []string) (directory.Entry, error) {
	out := directory.Entry{DN: e.dn, Attrs: map[string][][]byte{}}
	for k, vs := range e.attrs {
		if len(attrs) == 0 || slices.ContainsFunc(attrs, func(a string) bool { return strings.EqualFold(a, k) }) {
			out.Attrs[k] = clone(vs)
		}
	}
	for _, a := range attrs {
		if strings.EqualFold(a, "tokenGroups") {
			if sids := d.tokenGroups(e); len(sids) > 0 {
				out.Attrs[a] = sids
			}
			continue
		}
		if !strings.Contains(strings.ToLower(a), ";range=") {
			continue
		}
		r, ok := directory.ParseRange(a)
		if !ok {
			return directory.Entry{}, fmt.Errorf("identitytest: the range %q does not parse", a)
		}
		vs := e.get(r.Attr)
		if r.Lo >= len(vs) {
			continue
		}
		last := r.Lo + d.maxValRange - 1
		if r.Hi >= 0 {
			last = min(last, r.Hi)
		}
		got := directory.Range{Attr: r.Attr, Lo: r.Lo, Hi: last}
		if last >= len(vs)-1 {
			got.Hi, last = -1, len(vs)-1
		}
		out.Attrs[got.String()] = clone(vs[r.Lo : last+1])
	}
	return out, nil
}

func clone(vs [][]byte) [][]byte {
	cp := make([][]byte, len(vs))
	for i, v := range vs {
		cp[i] = slices.Clone(v)
	}
	return cp
}

// tokenGroups is the SIDs of every security group e belongs to, through
// member links or its primaryGroupID, directly or through other security
// groups, each once; none once HideTokenGroups was called; d.mu is held. A
// distribution group is neither held nor followed, as a security token
// holds no distribution group.
func (d *Directory) tokenGroups(e *entry) [][]byte {
	if d.noTokens {
		return nil
	}
	var start []*entry
	if v := e.get("primaryGroupID"); len(v) == 1 {
		if rid, err := strconv.ParseUint(string(v[0]), 10, 32); err == nil {
			sid := d.sid(uint32(rid))
			for _, g := range d.entries {
				if slices.ContainsFunc(g.get("objectSid"), func(s []byte) bool { return bytes.Equal(s, sid) }) {
					start = append(start, g)
				}
			}
		}
	}
	seen := map[*entry]bool{}
	var sids [][]byte
	var visit func(m *entry)
	visit = func(m *entry) {
		if seen[m] || m != e && !security(m) {
			return
		}
		seen[m] = true
		if m != e {
			sids = append(sids, clone(m.get("objectSid"))...)
		}
		for _, g := range d.entries {
			if slices.ContainsFunc(g.get("member"), func(v []byte) bool { return strings.EqualFold(string(v), m.dn) }) {
				visit(g)
			}
		}
	}
	visit(e)
	for _, g := range start {
		visit(g)
	}
	return sids
}

// security reports whether e's groupType holds the security bit.
func security(e *entry) bool {
	v := e.get("groupType")
	if len(v) != 1 {
		return false
	}
	n, err := strconv.ParseInt(string(v[0]), 10, 64)
	return err == nil && n&0x80000000 != 0
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
