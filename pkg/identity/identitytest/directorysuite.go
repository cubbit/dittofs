package identitytest

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/cubbit/dittofs/internal/identity/directory"
)

// Seeder puts the entries a row of DirectoryConformance needs into the
// directory under test.
type Seeder interface {
	// AddUser adds a user named name, at most 20 characters of letters and
	// digits, its account name and cn, with attrs set on it, and returns its
	// DN. A user whose userAccountControl lacks the disable bit is given a
	// password, so that a directory that refuses an enabled account without
	// one accepts it.
	AddUser(t testing.TB, name string, attrs map[string][][]byte) string
	// AddGroup adds a global security group named name, at most 20
	// characters of letters and digits, its account name and cn, with attrs
	// set on it, member among them, and returns its DN; a groupType in attrs
	// makes it another kind of group.
	AddGroup(t testing.TB, name string, attrs map[string][][]byte) string
	// Account is a name and password the directory accepts in a simple bind,
	// whose bind may read every entry AddUser adds.
	Account() (name string, password []byte)
}

var _ Seeder = (*Directory)(nil)

// DirectoryConformance holds a directory to what identity reads of Active
// Directory. open returns a new, unbound connection to the directory and its
// seeder; the in-memory Directory runs it in unit tests and a real domain
// controller in integration tests, so the in-memory one stands in only where
// both answer alike.
func DirectoryConformance(t *testing.T, open func(t *testing.T) (directory.Conn, Seeder)) {
	// call bounds one call to the directory: a directory that stops answering
	// fails it at a minute, and the seeding before it, however long, takes
	// none of that minute.
	call := func(t *testing.T) context.Context {
		ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
		t.Cleanup(cancel)
		return ctx
	}

	// bound opens a connection and binds it as the seeder's account.
	bound := func(t *testing.T) (directory.Conn, Seeder) {
		t.Helper()
		c, s := open(t)
		t.Cleanup(func() { _ = c.Close() })
		name, pw := s.Account()
		if err := c.Bind(call(t), name, pw); err != nil {
			t.Fatalf("bind as the seeder's account %q: %v", name, err)
		}
		return c, s
	}
	search := func(t *testing.T, c directory.Conn, req directory.Request) directory.Result {
		t.Helper()
		res, err := c.Search(call(t), req)
		if err != nil {
			t.Fatalf("search %+v: %v", req, err)
		}
		return res
	}
	// naming reads the root DSE's defaultNamingContext.
	naming := func(t *testing.T, c directory.Conn) string {
		t.Helper()
		res := search(t, c, directory.Request{Scope: directory.ScopeBase, Filter: "(objectClass=*)",
			Attrs: []string{"defaultNamingContext"}})
		if len(res.Entries) != 1 {
			t.Fatalf("root DSE: %d entries, want 1", len(res.Entries))
		}
		nc := one(res.Entries[0], "defaultNamingContext")
		if len(nc) == 0 {
			t.Fatalf("root DSE: no defaultNamingContext in %v", res.Entries[0].Attrs)
		}
		return string(nc)
	}
	// dns of a subtree search under the naming context.
	dns := func(t *testing.T, c directory.Conn, filter string) []string {
		t.Helper()
		res := search(t, c, directory.Request{Base: naming(t, c), Scope: directory.ScopeSubtree,
			Filter: filter, Attrs: []string{"cn"}})
		var got []string
		for _, e := range res.Entries {
			got = append(got, strings.ToLower(e.DN))
		}
		slices.Sort(got)
		return got
	}
	lower := func(dns ...string) []string {
		for i := range dns {
			dns[i] = strings.ToLower(dns[i])
		}
		slices.Sort(dns)
		return dns
	}

	t.Run("bind checks the password", func(t *testing.T) {
		c, s := open(t)
		defer func() { _ = c.Close() }()
		name, pw := s.Account()
		if err := c.Bind(call(t), name, append(slices.Clone(pw), 'x')); err == nil {
			t.Errorf("bind as %q with a wrong password: accepted, want refused", name)
		}
		if err := c.Bind(call(t), name, pw); err != nil {
			t.Errorf("bind as %q with its password: %v", name, err)
		}
	})

	// A name with an empty password is an unauthenticated bind, which a
	// permissive directory answers with success; it is refused, and the
	// connection is left unbound.
	t.Run("a bind with an empty password is refused", func(t *testing.T) {
		c, s := open(t)
		defer func() { _ = c.Close() }()
		name, _ := s.Account()
		for _, pw := range [][]byte{nil, {}} {
			if err := c.Bind(call(t), name, pw); err == nil {
				t.Errorf("bind as %q with password %#v: accepted, want refused", name, pw)
			}
		}
		res, err := c.Search(call(t), directory.Request{Base: naming(t, c), Scope: directory.ScopeSubtree, Filter: "(objectClass=user)"})
		if err == nil && len(res.Entries) > 0 {
			t.Errorf("a subtree search after binds with an empty password returned %d entries, want it refused", len(res.Entries))
		}
	})

	t.Run("root DSE names the naming context and its domain SID", func(t *testing.T) {
		c, s := open(t)
		defer func() { _ = c.Close() }()
		nc := naming(t, c) // read before any bind, as a client finds the directory
		name, pw := s.Account()
		if err := c.Bind(call(t), name, pw); err != nil {
			t.Fatal(err)
		}
		res := search(t, c, directory.Request{Base: nc, Scope: directory.ScopeBase, Filter: "(objectClass=*)",
			Attrs: []string{"objectSid"}})
		if len(res.Entries) != 1 {
			t.Fatalf("naming context %q: %d entries, want 1", nc, len(res.Entries))
		}
		if sid := one(res.Entries[0], "objectSid"); !domainSID(sid) {
			t.Errorf("naming context's objectSid = %x, want a binary S-1-5-21-x-y-z", sid)
		}
	})

	// The client asks a question again on a new connection only when the
	// one it held fails this way.
	t.Run("a search on a closed connection fails ErrClosed", func(t *testing.T) {
		c, _ := bound(t)
		_ = c.Close()
		if _, err := c.Search(call(t), directory.Request{Scope: directory.ScopeBase, Filter: "(objectClass=*)"}); !errors.Is(err, directory.ErrClosed) {
			t.Errorf("a search on a closed connection = %v, want ErrClosed", err)
		}
	})

	t.Run("a search before the bind is refused", func(t *testing.T) {
		c, _ := open(t)
		defer func() { _ = c.Close() }()
		nc := naming(t, c)
		res, err := c.Search(call(t), directory.Request{Base: nc, Scope: directory.ScopeSubtree, Filter: "(objectClass=user)"})
		if err == nil && len(res.Entries) > 0 {
			t.Errorf("an unbound subtree search returned %d entries, want it refused", len(res.Entries))
		}
	})

	t.Run("special characters match literally", func(t *testing.T) {
		c, s := bound(t)
		tok := token()
		value := tok + "a*b(c)d\\e\x00f"
		decoy := tok + "aXb(c)d\\e\x00f"
		want := s.AddUser(t, "lit"+tok, map[string][][]byte{"description": {[]byte(value)}})
		s.AddUser(t, "dec"+tok, map[string][][]byte{"description": {[]byte(decoy)}})

		if got := dns(t, c, "(description="+escape([]byte(value))+")"); !slices.Equal(got, lower(want)) {
			t.Errorf("escaped %q matched %v, want only %s", value, got, want)
		}
		upper := strings.ToUpper(tok) + "A*B(C)D\\E\x00F"
		if got := dns(t, c, "(description="+escape([]byte(upper))+")"); !slices.Equal(got, lower(want)) {
			t.Errorf("escaped %q, the value's case changed, matched %v, want only %s", upper, got, want)
		}
		if _, err := c.Search(call(t), directory.Request{Base: naming(t, c), Scope: directory.ScopeSubtree,
			Filter: `(description=\zz)`}); err == nil {
			t.Error(`a filter holding the escape \zz was answered, want it refused`)
		}
	})

	// A value holding NUL is left out: Samba matches substrings against a
	// value only up to its first NUL.
	t.Run("substrings match initial, any and final pieces", func(t *testing.T) {
		c, s := bound(t)
		tok := token()
		a := s.AddUser(t, "suba"+tok, map[string][][]byte{"description": {[]byte(tok + "alpha-omega")}})
		b := s.AddUser(t, "subb"+tok, map[string][][]byte{"description": {[]byte(tok + "alpha-beta")}})
		for filter, want := range map[string][]string{
			"(description=" + tok + "*)":           lower(a, b),
			"(description=" + tok + "alpha*omega)": lower(a),
			"(description=" + tok + "*omega)":      lower(a),
			"(description=" + tok + "*beta)":       lower(b),
			"(description=" + tok + "*-*e*a)":      lower(a, b),
			"(description=" + tok + "beta*)":       nil,
			"(description=" + tok + "*-*x*a)":      nil,
		} {
			if got := dns(t, c, filter); !slices.Equal(got, want) {
				t.Errorf("%s matched %v, want %v", filter, got, want)
			}
		}
	})

	t.Run("two entries answering one name are both returned", func(t *testing.T) {
		c, s := bound(t)
		tok := token()
		a := s.AddUser(t, "amb1"+tok, map[string][][]byte{"uid": {[]byte("dup" + tok)}})
		b := s.AddUser(t, "amb2"+tok, map[string][][]byte{"uid": {[]byte("dup" + tok)}})
		if got := dns(t, c, "(uid=dup"+tok+")"); !slices.Equal(got, lower(a, b)) {
			t.Errorf("(uid=dup%s) matched %v, want both %s and %s", tok, got, a, b)
		}
	})

	// Active Directory answers such a search with a referral, Samba with no
	// such object; either way it is refused, never an empty answer.
	t.Run("a search based outside the naming context is refused", func(t *testing.T) {
		c, _ := bound(t)
		const base = "DC=elsewhere,DC=example"
		res, err := c.Search(call(t), directory.Request{Base: base, Scope: directory.ScopeSubtree, Filter: "(objectClass=*)"})
		if err == nil {
			t.Errorf("search based at %s: answered %d entries and referrals %q, want it refused", base, len(res.Entries), res.Referrals)
		}
	})

	// The references are handed back, never followed: a connection that
	// followed them would bind elsewhere with the same credential.
	t.Run("a subtree search of the naming context references the other partitions", func(t *testing.T) {
		c, _ := bound(t)
		nc := naming(t, c)
		for _, tc := range []struct {
			base  string
			scope directory.Scope
			want  []string
		}{
			{nc, directory.ScopeSubtree, lower("CN=Configuration,"+nc, "DC=DomainDnsZones,"+nc, "DC=ForestDnsZones,"+nc)},
			{nc, directory.ScopeBase, nil},
			{"CN=Users," + nc, directory.ScopeSubtree, nil},
		} {
			res := search(t, c, directory.Request{Base: tc.base, Scope: tc.scope, Filter: "(cn=" + token() + ")"})
			var got []string
			for _, r := range res.Referrals {
				u, err := url.Parse(r)
				if err != nil || !strings.HasPrefix(u.Scheme, "ldap") {
					t.Errorf("search of %s, scope %d: reference %q is not an LDAP URL", tc.base, tc.scope, r)
					continue
				}
				got = append(got, strings.ToLower(strings.TrimPrefix(u.Path, "/")))
			}
			slices.Sort(got)
			if !slices.Equal(got, tc.want) || len(res.Entries) != 0 {
				t.Errorf("search of %s, scope %d: %d entries, references to %q, want none and %q",
					tc.base, tc.scope, len(res.Entries), got, tc.want)
			}
		}
	})

	t.Run("disable bit and account expiry read back", func(t *testing.T) {
		c, s := bound(t)
		tok := token()
		const past, never = "129000000000000000", "9223372036854775807" // 2009-10-15 as a FILETIME; never
		off := s.AddUser(t, "off"+tok, map[string][][]byte{
			"userAccountControl": {[]byte("514")}, "accountExpires": {[]byte(past)}})
		on := s.AddUser(t, "on"+tok, map[string][][]byte{
			"userAccountControl": {[]byte("512")}, "accountExpires": {[]byte(never)}})
		for _, tc := range []struct {
			dn       string
			disabled bool
			expires  string
		}{{off, true, past}, {on, false, never}} {
			res := search(t, c, directory.Request{Base: tc.dn, Scope: directory.ScopeBase, Filter: "(objectClass=user)",
				Attrs: []string{"userAccountControl", "accountExpires"}})
			if len(res.Entries) != 1 {
				t.Fatalf("%s: %d entries, want 1", tc.dn, len(res.Entries))
			}
			uac, err := strconv.ParseUint(string(one(res.Entries[0], "userAccountControl")), 10, 32)
			if err != nil {
				t.Fatalf("%s: userAccountControl: %v", tc.dn, err)
			}
			if got := uac&2 != 0; got != tc.disabled {
				t.Errorf("%s: userAccountControl %d, disable bit %v, want %v", tc.dn, uac, got, tc.disabled)
			}
			if got := string(one(res.Entries[0], "accountExpires")); got != tc.expires {
				t.Errorf("%s: accountExpires %q, want %q", tc.dn, got, tc.expires)
			}
		}
	})

	t.Run("objectSid and objectGUID are binary and found by escaped bytes", func(t *testing.T) {
		c, s := bound(t)
		dn := s.AddUser(t, "bin"+token(), nil)
		nc := naming(t, c)
		dom := search(t, c, directory.Request{Base: nc, Scope: directory.ScopeBase, Filter: "(objectClass=*)",
			Attrs: []string{"objectSid"}})
		res := search(t, c, directory.Request{Base: dn, Scope: directory.ScopeBase, Filter: "(objectClass=*)",
			Attrs: []string{"objectSid", "objectGUID"}})
		if len(res.Entries) != 1 || len(dom.Entries) != 1 {
			t.Fatalf("%s and %s: %d and %d entries, want 1 each", dn, nc, len(res.Entries), len(dom.Entries))
		}
		if n := len(res.Entries[0].Attrs); n != 2 {
			t.Errorf("%s: asked for objectSid and objectGUID, got %d attributes: %v", dn, n, res.Entries[0].Attrs)
		}
		sid, guid := one(res.Entries[0], "objectSid"), one(res.Entries[0], "objectGUID")
		domSID := one(dom.Entries[0], "objectSid")
		if len(sid) != 28 || len(domSID) != 24 || sid[1] != 5 || !bytes.Equal(sid[2:8], domSID[2:8]) ||
			!bytes.Equal(sid[8:24], domSID[8:24]) {
			t.Errorf("%s: objectSid %x, want the domain's %x with one RID more", dn, sid, domSID)
		}
		if len(guid) != 16 {
			t.Errorf("%s: objectGUID %x, want 16 bytes", dn, guid)
		}
		for attr, v := range map[string][]byte{"objectSid": sid, "objectGUID": guid} {
			if got := dns(t, c, "("+attr+"="+escape(v)+")"); !slices.Equal(got, lower(dn)) {
				t.Errorf("(%s=%x) matched %v, want only %s", attr, v, got, dn)
			}
		}
	})

	// sidOf reads the objectSid of the entry at dn.
	sidOf := func(t *testing.T, c directory.Conn, dn string) []byte {
		t.Helper()
		res := search(t, c, directory.Request{Base: dn, Scope: directory.ScopeBase, Filter: "(objectClass=*)",
			Attrs: []string{"objectSid"}})
		if len(res.Entries) != 1 || len(one(res.Entries[0], "objectSid")) == 0 {
			t.Fatalf("%s: %d entries, want 1 holding an objectSid", dn, len(res.Entries))
		}
		return one(res.Entries[0], "objectSid")
	}
	// nested adds a user three levels deep, in g1, g1 in g2, g2 in g3, and a
	// group the user is not in; it returns the user's DN and the four groups'.
	nested := func(t *testing.T, s Seeder) (user string, g [3]string, decoy string) {
		t.Helper()
		tok := token()
		user = s.AddUser(t, "nu"+tok, nil)
		g[0] = s.AddGroup(t, "ng1"+tok, map[string][][]byte{"member": {[]byte(user)}})
		g[1] = s.AddGroup(t, "ng2"+tok, map[string][][]byte{"member": {[]byte(g[0])}})
		g[2] = s.AddGroup(t, "ng3"+tok, map[string][][]byte{"member": {[]byte(g[1])}})
		decoy = s.AddGroup(t, "ngx"+tok, nil)
		return user, g, decoy
	}

	// tokenGroupsOf reads the user's tokenGroups.
	tokenGroupsOf := func(t *testing.T, c directory.Conn, user string) []string {
		t.Helper()
		res := search(t, c, directory.Request{Base: user, Scope: directory.ScopeBase, Filter: "(objectClass=*)",
			Attrs: []string{"tokenGroups"}})
		if len(res.Entries) != 1 {
			t.Fatalf("%s: %d entries, want 1", user, len(res.Entries))
		}
		var got []string
		for k, vs := range res.Entries[0].Attrs {
			if strings.EqualFold(k, "tokenGroups") {
				for _, v := range vs {
					got = append(got, string(v))
				}
			}
		}
		return got
	}

	// tokenGroups is read with base scope; it holds the primary group, which
	// member never names, as well as every level.
	t.Run("tokenGroups holds every level of nesting and the primary group", func(t *testing.T) {
		c, s := bound(t)
		user, g, decoy := nested(t, s)
		got := tokenGroupsOf(t, c, user)
		domain := sidOf(t, c, naming(t, c))
		primary := append(slices.Clone(domain), 0x01, 0x02, 0, 0) // RID 513, Domain Users
		primary[1]++
		for _, want := range []struct {
			what string
			sid  []byte
		}{{"level 1", sidOf(t, c, g[0])}, {"level 2", sidOf(t, c, g[1])}, {"level 3", sidOf(t, c, g[2])}, {"the primary group", primary}} {
			if !slices.Contains(got, string(want.sid)) {
				t.Errorf("%s: tokenGroups %x lacks %s, %x", user, got, want.what, want.sid)
			}
		}
		if d := sidOf(t, c, decoy); slices.Contains(got, string(d)) {
			t.Errorf("%s: tokenGroups holds %x, a group it is not in", user, d)
		}
	})

	t.Run("the chain rule answers every level of nesting", func(t *testing.T) {
		c, s := bound(t)
		user, g, _ := nested(t, s)
		got := dns(t, c, "(&(objectClass=group)(member:"+directory.InChain+":="+escape([]byte(user))+"))")
		if want := lower(g[0], g[1], g[2]); !slices.Equal(got, want) {
			t.Errorf("groups %s is in by the chain rule: %v, want %v", user, got, want)
		}
	})

	// A distribution group (groupType 2, global, no security bit) is one the
	// chain rule matches and a security token never holds: tokenGroups
	// leaves it out, and so does the chain rule asked for groupType's
	// security bit. A security group the user reaches only through it is
	// left out of tokenGroups too, where the chain rule, which follows
	// member whatever the group, still answers it.
	t.Run("a distribution group is in neither tokenGroups nor the security bit's chain", func(t *testing.T) {
		c, s := bound(t)
		tok := token()
		user := s.AddUser(t, "du"+tok, nil)
		sec := s.AddGroup(t, "dsec"+tok, map[string][][]byte{"member": {[]byte(user)}})
		dist := s.AddGroup(t, "ddist"+tok, map[string][][]byte{"member": {[]byte(user)}, "groupType": {[]byte("2")}})
		up := s.AddGroup(t, "dup"+tok, map[string][][]byte{"member": {[]byte(dist)}})
		in := "(member:" + directory.InChain + ":=" + escape([]byte(user)) + ")"
		if got, want := dns(t, c, "(&(objectClass=group)"+in+")"), lower(sec, dist, up); !slices.Equal(got, want) {
			t.Errorf("groups %s is in by the chain rule: %v, want %v", user, got, want)
		}
		bit := "(groupType:" + directory.BitAnd + ":=2147483648)"
		if got, want := dns(t, c, "(&(objectClass=group)"+bit+in+")"), lower(sec, up); !slices.Equal(got, want) {
			t.Errorf("security groups %s is in by the chain rule: %v, want %v", user, got, want)
		}
		got := tokenGroupsOf(t, c, user)
		if want := sidOf(t, c, sec); !slices.Contains(got, string(want)) {
			t.Errorf("%s: tokenGroups %x lacks its security group, %x", user, got, want)
		}
		if d := sidOf(t, c, dist); slices.Contains(got, string(d)) {
			t.Errorf("%s: tokenGroups holds %x, its distribution group", user, d)
		}
		if u := sidOf(t, c, up); slices.Contains(got, string(u)) {
			t.Errorf("%s: tokenGroups holds %x, a group reached only through its distribution group", user, u)
		}
	})

	// One set of 3 000 users serves both rows: the first 2 500 are paged
	// through, and a group holds all of them as members.
	t.Run("3000 entries", func(t *testing.T) {
		c, s := bound(t)
		tok := token()
		var paged, members []string
		for i := range 3000 {
			mark := tok + "rest"
			if i < 2500 {
				mark = tok + "page"
			}
			dn := s.AddUser(t, fmt.Sprintf("p%s%04d", tok, i), map[string][][]byte{"description": {[]byte(mark)}})
			if i < 2500 {
				paged = append(paged, dn)
			}
			members = append(members, dn)
		}
		var vals [][]byte
		for _, m := range members {
			vals = append(vals, []byte(m))
		}
		big := s.AddGroup(t, "big"+tok, map[string][][]byte{"member": vals})
		nc := naming(t, c)

		t.Run("a paged search returns every entry once", func(t *testing.T) {
			req := directory.Request{Base: nc, Scope: directory.ScopeSubtree, Filter: "(description=" + tok + "page)",
				Attrs: []string{"cn"}, PageSize: 1000}
			var got []string
			pages := 0
			for {
				res := search(t, c, req)
				pages++
				if len(res.Entries) > 1000 {
					t.Errorf("page %d: %d entries, want at most 1000", pages, len(res.Entries))
				}
				for _, e := range res.Entries {
					got = append(got, strings.ToLower(e.DN))
				}
				if len(res.Cookie) == 0 || pages == 10 {
					break
				}
				req.Cookie = res.Cookie
			}
			slices.Sort(got)
			if want := lower(slices.Clone(paged)...); !slices.Equal(got, want) || pages < 3 {
				t.Errorf("paged search: %d entries in %d pages, want each of the %d once in at least 3 pages",
					len(got), pages, len(want))
			}
		})

		// A directory holds a paged search's state per connection.
		t.Run("a paged search's cookie is refused on another connection", func(t *testing.T) {
			req := directory.Request{Base: nc, Scope: directory.ScopeSubtree, Filter: "(description=" + tok + "page)",
				Attrs: []string{"cn"}, PageSize: 1000}
			res := search(t, c, req)
			if len(res.Cookie) == 0 {
				t.Fatal("the first page of 2 500 entries handed out no cookie")
			}
			other, _ := bound(t)
			req.Cookie = res.Cookie
			if res, err := other.Search(call(t), req); err == nil {
				t.Errorf("the second page asked on another connection answered %d entries, want it refused", len(res.Entries))
			}
		})

		// rng reads one range of big's member values: the attribute's name
		// as returned and its values.
		rng := func(t *testing.T, asked string) (string, []string) {
			t.Helper()
			res := search(t, c, directory.Request{Base: big, Scope: directory.ScopeBase, Filter: "(objectClass=group)",
				Attrs: []string{asked}})
			if len(res.Entries) != 1 || len(res.Entries[0].Attrs) != 1 {
				t.Fatalf("%s of %s: %d entries, want 1 holding one attribute", asked, big, len(res.Entries))
			}
			for k, vs := range res.Entries[0].Attrs {
				var got []string
				for _, v := range vs {
					got = append(got, strings.ToLower(string(v)))
				}
				return k, got
			}
			return "", nil
		}

		t.Run("member is read in ranges", func(t *testing.T) {
			var all []string
			for _, tc := range []struct{ asked, named string }{
				{"member;range=0-999", "member;range=0-999"},
				{"member;range=1000-1999", "member;range=1000-1999"},
				{"member;range=2000-2999", "member;range=2000-*"},
			} {
				name, got := rng(t, tc.asked)
				if !strings.EqualFold(name, tc.named) || len(got) != 1000 {
					t.Errorf("%s: %d values under %q, want 1000 under %q", tc.asked, len(got), name, tc.named)
				}
				all = append(all, got...)
			}
			slices.Sort(all)
			if want := lower(slices.Clone(members)...); !slices.Equal(all, want) {
				t.Errorf("the three ranges hold %d values, want each of the %d members once", len(all), len(want))
			}
		})

		// Asked to its end, a directory may answer in one part or several,
		// each starting where it was asked, the last ending in *.
		t.Run("member read to its end", func(t *testing.T) {
			var all []string
			for lo, parts := 0, 0; ; parts++ {
				if parts == 10 {
					t.Fatalf("member;range=%d-*: no last part after 10", lo)
				}
				name, got := rng(t, fmt.Sprintf("member;range=%d-*", lo))
				r, ok := directory.ParseRange(name)
				if !ok || r.Lo != lo || !strings.EqualFold(r.Attr, "member") {
					t.Fatalf("member;range=%d-*: answered under %q, want a range of member from %d", lo, name, lo)
				}
				all = append(all, got...)
				if r.Hi < 0 {
					break
				}
				if len(got) != r.Hi-r.Lo+1 {
					t.Fatalf("%s: %d values", name, len(got))
				}
				lo = r.Hi + 1
			}
			slices.Sort(all)
			if want := lower(slices.Clone(members)...); !slices.Equal(all, want) {
				t.Errorf("member read to its end: %d values, want each of the %d members once", len(all), len(want))
			}
		})
	})
}

// one is the entry's first value of attr, looked up case-insensitively.
func one(e directory.Entry, attr string) []byte {
	for k, v := range e.Attrs {
		if strings.EqualFold(k, attr) && len(v) > 0 {
			return v[0]
		}
	}
	return nil
}

// domainSID reports whether b is a binary S-1-5-21-x-y-z.
func domainSID(b []byte) bool {
	return len(b) == 24 && b[0] == 1 && b[1] == 4 && bytes.Equal(b[2:8], []byte{0, 0, 0, 0, 0, 5}) &&
		bytes.Equal(b[8:12], []byte{21, 0, 0, 0})
}

// escape writes every byte of v that is not a letter or digit as RFC 4515's
// \xx escape.
func escape(v []byte) string {
	var b strings.Builder
	for _, c := range v {
		if 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' || '0' <= c && c <= '9' {
			b.WriteByte(c)
		} else {
			fmt.Fprintf(&b, `\%02x`, c)
		}
	}
	return b.String()
}

// token is eight random lower-case letters and digits, which keep one row's
// entries apart from every other row's and run's in a shared directory.
func token() string { return strings.ToLower(rand.Text()[:8]) }
