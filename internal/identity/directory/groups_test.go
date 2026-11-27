package directory_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/cubbit/dittofs/internal/identity/directory"
	"github.com/cubbit/dittofs/pkg/sim"
	"github.com/cubbit/dittofs/pkg/vfs/vfserr"
)

// dnsOf is the sorted, lower-cased DNs of answers.
func dnsOf(as []directory.Answer) []string {
	var out []string
	for _, a := range as {
		out = append(out, strings.ToLower(a.DN))
	}
	slices.Sort(out)
	return out
}

func lowered(dns ...string) []string {
	out := make([]string, len(dns))
	for i, dn := range dns {
		out[i] = strings.ToLower(dn)
	}
	slices.Sort(out)
	return out
}

// walked reports whether q is a level of the walk over security groups.
func walked(q directory.Request) bool { return strings.Contains(q.Filter, directory.BitAnd) }

// An ad user's closure is every level of its nesting, its primary group and
// that group's own groups, a cycle visited once, through tokenGroups; and the
// same through a level-by-level walk from the user and primaryGroupID for a
// bind that cannot read tokenGroups, which member alone would answer without
// the primary group. A distribution group is in neither, at the first level
// or any above it, nor is a security group reached only through one, as
// tokenGroups holds neither (MS-ADTS 2.2.12).
func TestGroupsAD(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	user := e.d.AddUser(t, "alice", nil)
	g1 := e.d.AddGroup(t, "g1", map[string][][]byte{"member": {[]byte("CN=g3,CN=Users," + base), []byte(user)}}) // g3 in g1: a cycle
	g2 := e.d.AddGroup(t, "g2", map[string][][]byte{"member": {[]byte(g1)}})
	g3 := e.d.AddGroup(t, "g3", map[string][][]byte{"member": {[]byte(g2)}})
	du := "CN=Domain Users,CN=Users," + base
	pgParent := e.d.AddGroup(t, "staff", map[string][][]byte{"member": {[]byte(du)}})
	e.d.AddGroup(t, "other", nil)
	list := e.d.AddGroup(t, "list", map[string][][]byte{"member": {[]byte(user)}, "groupType": {[]byte("2")}})
	e.d.AddGroup(t, "overlist", map[string][][]byte{"member": {[]byte(list)}})
	g1list := e.d.AddGroup(t, "g1list", map[string][][]byte{"member": {[]byte(g1)}, "groupType": {[]byte("2")}})
	e.d.AddGroup(t, "overg1list", map[string][][]byte{"member": {[]byte(g1list)}})
	want := lowered(g1, g2, g3, du, pgParent)

	for _, hidden := range []bool{false, true} {
		if hidden {
			e.d.HideTokenGroups()
		}
		f := e.faulty(t)
		c := e.client(f.Dial)
		a, _, err := c.Lookup(ctx, directory.ByUserName, []byte("alice"))
		if err != nil {
			t.Fatal(err)
		}
		got, err := c.Groups(ctx, a)
		if err != nil || !slices.Equal(dnsOf(got), want) {
			t.Errorf("Groups(alice), tokenGroups hidden %v = %v, %v; want %v", hidden, dnsOf(got), err, want)
		}
		if w := slices.ContainsFunc(f.Queries(), walked); w != hidden {
			t.Errorf("tokenGroups hidden %v: the walk asked %v, want %v", hidden, w, hidden)
		}
		for _, g := range got {
			if !g.Group || len(g.SID) == 0 {
				t.Errorf("Groups(alice) answered %s as group %v with SID %x, want a group with its SID", g.DN, g.Group, g.SID)
			}
		}
	}
}

// A page that fails fails the closure: nothing of the first page is
// answered.
func TestGroupsFailedPageFailsWhole(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	user := e.d.AddUser(t, "alice", nil)
	for i := range 1000 {
		e.d.AddGroup(t, fmt.Sprintf("g%04d", i), map[string][][]byte{"member": {[]byte(user)}})
	}
	e.d.HideTokenGroups()
	e.d.SetMaxPageSize(600)

	c := e.client(e.d.Dial)
	a, _, err := c.Lookup(ctx, directory.ByUserName, []byte("alice"))
	if err != nil {
		t.Fatal(err)
	}
	filter := walkFilter(t, e, a)

	f := e.faulty(t, sim.Rule{Op: "search", Key: filter, From: 2, Count: 1, Action: sim.FaultAction{Err: errors.New("page lost")}})
	c = e.client(f.Dial)
	got, err := c.Groups(ctx, a)
	if !errors.Is(err, vfserr.ErrUnavailable) || got != nil {
		t.Errorf("Groups(alice) with its second page failed = %d groups, %v; want none, ErrUnavailable", len(got), err)
	}
	if n := len(slices.DeleteFunc(f.Queries(), func(r directory.Request) bool { return r.Filter != filter })); n != 2 {
		t.Errorf("%d pages of the walk's first level asked, want the first and the failed second", n)
	}
}

// walkFilter is the filter of the first level of the walk of a Groups(user)
// that walks, read from a run on a fault wrapper with no faults.
func walkFilter(t *testing.T, e *env, user directory.Answer) string {
	t.Helper()
	f := e.faulty(t)
	if _, err := e.client(f.Dial).Groups(context.Background(), user); err != nil {
		t.Fatal(err)
	}
	for _, q := range f.Queries() {
		if walked(q) {
			return q.Filter
		}
	}
	t.Fatal("Groups asked no level of the walk")
	return ""
}

// A paged search's later pages go on the connection that answered its first,
// as a directory refuses a cookie on another; one that finds that connection
// closed restarts the search, once, from its first page on a new one.
func TestGroupsPagesStayOnOneConnection(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	user := e.d.AddUser(t, "alice", nil)
	for i := range 1000 {
		e.d.AddGroup(t, fmt.Sprintf("g%04d", i), map[string][][]byte{"member": {[]byte(user)}})
	}
	e.d.HideTokenGroups()
	e.d.SetMaxPageSize(600)
	a, _, err := e.client(e.d.Dial).Lookup(ctx, directory.ByUserName, []byte("alice"))
	if err != nil {
		t.Fatal(err)
	}
	filter := walkFilter(t, e, a)

	closed := fmt.Errorf("%w: closed under the search", directory.ErrClosed)
	f := e.faulty(t, sim.Rule{Op: "search", Key: filter, From: 2, Count: 1, Action: sim.FaultAction{Err: closed}})
	got, err := e.client(f.Dial).Groups(ctx, a)
	if err != nil || len(got) != 1001 {
		t.Fatalf("Groups(alice) with its second page's connection closed = %d groups, %v; want its 1 000 and Domain Users", len(got), err)
	}
	if n := len(f.Dials()); n != 2 {
		t.Errorf("%d dials, want 2: the first and the restart's, none for a lone second page", n)
	}
	if n := len(slices.DeleteFunc(f.Queries(), func(r directory.Request) bool { return r.Filter != filter })); n != 4 {
		t.Errorf("%d pages of the walk's first level asked, want 4: two, the second closed, then two again", n)
	}
}

// A paged search of more than 64 pages fails, and the connection it was left
// open on is closed: the next question dials again.
func TestGroupsMorePagesThanTheBound(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	user := e.d.AddUser(t, "alice", nil)
	for i := range 65 {
		e.d.AddGroup(t, fmt.Sprintf("g%02d", i), map[string][][]byte{"member": {[]byte(user)}})
	}
	e.d.HideTokenGroups()
	e.d.SetMaxPageSize(1)
	f := e.faulty(t)
	c := e.client(f.Dial)
	a, _, err := c.Lookup(ctx, directory.ByUserName, []byte("alice"))
	if err != nil {
		t.Fatal(err)
	}
	got, err := c.Groups(ctx, a)
	if !errors.Is(err, vfserr.ErrUnavailable) || got != nil {
		t.Fatalf("Groups(alice) in 65 groups, a page each = %d groups, %v; want none, ErrUnavailable", len(got), err)
	}
	if n := len(slices.DeleteFunc(f.Queries(), func(r directory.Request) bool { return !walked(r) })); n != 64 {
		t.Errorf("%d pages of the walk asked, want 64", n)
	}
	dials := len(f.Dials())
	if _, _, err := c.Lookup(ctx, directory.ByUserName, []byte("alice")); err != nil {
		t.Fatal(err)
	}
	if n := len(f.Dials()); n != dials+1 {
		t.Errorf("%d dials after the next lookup, want %d: the connection left mid-search closed", n, dials+1)
	}
}

// A caller whose context ends while a later page is out gets an error, and
// the connection the paged search was left open on is closed: the next
// question dials again.
func TestGroupsCallerGoneMidPages(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newEnv(t)
		user := e.d.AddUser(t, "alice", nil)
		for i := range 2 {
			e.d.AddGroup(t, fmt.Sprintf("g%d", i), map[string][][]byte{"member": {[]byte(user)}})
		}
		e.d.HideTokenGroups()
		e.d.SetMaxPageSize(1)
		a, _, err := e.client(e.d.Dial).Lookup(context.Background(), directory.ByUserName, []byte("alice"))
		if err != nil {
			t.Fatal(err)
		}
		filter := walkFilter(t, e, a)

		// The walk's second page is answered, then held a second.
		f := e.faulty(t, sim.Rule{Op: "search", Key: filter, From: 2, Count: 1, Action: sim.FaultAction{Delay: time.Second}})
		c := e.client(f.Dial)
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { _, err := c.Groups(ctx, a); done <- err }()
		synctest.Wait()
		cancel()
		if err := <-done; !errors.Is(err, vfserr.ErrUnavailable) || !errors.Is(err, context.Canceled) {
			t.Fatalf("Groups(alice), its context ended on the second page = %v; want ErrUnavailable for the canceled context", err)
		}
		e.clock.Advance(time.Second)
		synctest.Wait()
		dials := len(f.Dials())
		if _, _, err := c.Lookup(context.Background(), directory.ByUserName, []byte("alice")); err != nil {
			t.Fatal(err)
		}
		if n := len(f.Dials()); n != dials+1 {
			t.Errorf("%d dials after the next lookup, want %d: the connection left mid-search closed", n, dials+1)
		}
	})
}

// A caller whose context ends while the first page is out gets an error,
// and the connection the paged search is left open on once that page is
// answered is closed: the next question dials again.
func TestGroupsCallerGoneOnFirstPage(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newEnv(t)
		user := e.d.AddUser(t, "alice", nil)
		for i := range 2 {
			e.d.AddGroup(t, fmt.Sprintf("g%d", i), map[string][][]byte{"member": {[]byte(user)}})
		}
		e.d.HideTokenGroups()
		e.d.SetMaxPageSize(1)
		a, _, err := e.client(e.d.Dial).Lookup(context.Background(), directory.ByUserName, []byte("alice"))
		if err != nil {
			t.Fatal(err)
		}
		filter := walkFilter(t, e, a)

		// The walk's first page is answered, cookie and all, then held a
		// second.
		f := e.faulty(t, sim.Rule{Op: "search", Key: filter, From: 1, Count: 1, Action: sim.FaultAction{Delay: time.Second}})
		c := e.client(f.Dial)
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { _, err := c.Groups(ctx, a); done <- err }()
		synctest.Wait()
		cancel()
		if err := <-done; !errors.Is(err, vfserr.ErrUnavailable) || !errors.Is(err, context.Canceled) {
			t.Fatalf("Groups(alice), its context ended on the first page = %v; want ErrUnavailable for the canceled context", err)
		}
		e.clock.Advance(time.Second)
		synctest.Wait()
		dials := len(f.Dials())
		if _, _, err := c.Lookup(context.Background(), directory.ByUserName, []byte("alice")); err != nil {
			t.Fatal(err)
		}
		if n := len(f.Dials()); n != dials+1 {
			t.Errorf("%d dials after the next lookup, want %d: the connection left with the search open closed", n, dials+1)
		}
	})
}

// A tokenGroups SID the lookup finds no entry for, as when the bind may not
// read the group, is kept in the closure by its SID alone.
func TestGroupsKeepsASIDWithNoEntry(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	user := e.d.AddUser(t, "alice", nil)
	e.d.AddGroup(t, "g1", map[string][][]byte{"member": {[]byte(user)}})
	c0 := e.client(e.d.Dial)
	var want []string
	for _, name := range []string{"g1", "Domain Users"} {
		g, ok, err := c0.Lookup(ctx, directory.ByGroupName, []byte(name))
		if err != nil || !ok {
			t.Fatalf("Lookup(%s) = %v, %v", name, ok, err)
		}
		want = append(want, string(g.SID))
	}
	slices.Sort(want)
	a, _, err := c0.Lookup(ctx, directory.ByUserName, []byte("alice"))
	if err != nil {
		t.Fatal(err)
	}

	// The SID lookup's first entry comes back without its objectSid.
	f := e.faulty(t, sim.Rule{Op: "search", Key: "(|(objectSid=*", Action: sim.FaultAction{TearAfter: 1}})
	f.TearAttr("objectSid", "objectSidUnread")
	got, err := e.client(f.Dial).Groups(ctx, a)
	var sids []string
	bare := 0
	for _, g := range got {
		sids = append(sids, string(g.SID))
		if g.DN == "" {
			bare++
		}
	}
	slices.Sort(sids)
	if err != nil || !slices.Equal(sids, want) || bare != 1 {
		t.Errorf("Groups(alice), one group's entry unread = SIDs %x (%d by SID alone), %v; want %x, one by SID alone", sids, bare, err, want)
	}
}

// A bind that reads neither tokenGroups nor the user's primaryGroupID, which
// every ad user holds, fails the closure rather than answer it without the
// primary group.
func TestGroupsADUnreadPrimaryGroupIsUnavailable(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	user := e.d.AddUser(t, "alice", nil)
	e.d.AddGroup(t, "g1", map[string][][]byte{"member": {[]byte(user)}})
	e.d.HideTokenGroups()
	f := e.faulty(t, sim.Rule{Op: "search", Key: "(objectClass=user)", Action: sim.FaultAction{TearAfter: 1}})
	f.TearAttr("primaryGroupID", "primaryGroupIDUnread")
	got, err := e.client(f.Dial).Groups(ctx, directory.Answer{DN: user})
	if !errors.Is(err, vfserr.ErrUnavailable) || got != nil {
		t.Errorf("Groups(alice), primaryGroupID unread = %v, %v; want none, ErrUnavailable", dnsOf(got), err)
	}
}

// A record whose base holds none of the user's groups still answers the
// whole closure: tokenGroups covers the domain, so its SIDs, the primary
// group and the walk are looked up under the naming context.
func TestGroupsADUnderTheNamingContext(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	e.rec.Base = "OU=People," + base
	user := e.d.AddUser(t, "alice", nil)
	g1 := e.d.AddGroup(t, "g1", map[string][][]byte{"member": {[]byte(user)}})
	g2 := e.d.AddGroup(t, "g2", map[string][][]byte{"member": {[]byte(g1)}})
	du := "CN=Domain Users,CN=Users," + base
	staff := e.d.AddGroup(t, "staff", map[string][][]byte{"member": {[]byte(du)}})
	want := lowered(g1, g2, du, staff)
	for _, hidden := range []bool{false, true} {
		if hidden {
			e.d.HideTokenGroups()
		}
		got, err := e.client(e.d.Dial).Groups(ctx, directory.Answer{DN: user})
		if err != nil || !slices.Equal(dnsOf(got), want) {
			t.Errorf("Groups(alice) under base %s, tokenGroups hidden %v = %v, %v; want %v", e.rec.Base, hidden, dnsOf(got), err, want)
		}
	}
}

// A closure of exactly 1 024 groups is answered whole, and one of more
// fails: from tokenGroups before any of its SIDs is looked up, and from the
// walk at the level that passes the cap.
func TestGroupsADBound(t *testing.T) {
	for _, n := range []int{1024, 1025} {
		for _, hidden := range []bool{false, true} {
			t.Run(fmt.Sprintf("%d groups, tokenGroups hidden %v", n, hidden), func(t *testing.T) {
				ctx := context.Background()
				e := newEnv(t)
				user := e.d.AddUser(t, "alice", nil)
				for i := range n - 1 { // and Domain Users
					e.d.AddGroup(t, fmt.Sprintf("g%04d", i), map[string][][]byte{"member": {[]byte(user)}})
				}
				if hidden {
					e.d.HideTokenGroups()
				}
				f := e.faulty(t)
				got, err := e.client(f.Dial).Groups(ctx, directory.Answer{DN: user})
				if n == 1024 {
					if err != nil || len(got) != n {
						t.Errorf("Groups(alice) in 1 024 groups = %d groups, %v; want all 1 024", len(got), err)
					}
					return
				}
				if !errors.Is(err, vfserr.ErrUnavailable) || got != nil {
					t.Errorf("Groups(alice) in 1 025 groups = %d groups, %v; want none, ErrUnavailable", len(got), err)
				}
				lookups, levels := 0, 0
				for _, q := range f.Queries() {
					if strings.HasPrefix(q.Filter, "(|(objectSid=") {
						lookups++
					}
					if walked(q) {
						levels++
					}
				}
				// Hidden, the one lookup is the primary group's, and the walk's
				// first level is two pages of 1 000.
				want := map[bool][2]int{false: {0, 0}, true: {1, 2}}[hidden]
				if lookups != want[0] || levels != want[1] {
					t.Errorf("Groups(alice) in 1 025 groups asked %d SID lookups and %d walk queries, want %d and %d", lookups, levels, want[0], want[1])
				}
			})
		}
	}
}

// An rfc2307 user's closure is walked level by level through memberUid and
// member, a cycle visited once.
func TestGroupsRFC2307(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	e.rec.Schema = directory.SchemaRFC2307
	posix := func(name string, attrs map[string][][]byte) string {
		attrs["objectClass"] = [][]byte{[]byte("top"), []byte("posixGroup")}
		attrs["cn"] = [][]byte{[]byte(name)}
		return e.d.AddGroup(t, name, attrs)
	}
	user := e.d.AddUser(t, "bob", map[string][][]byte{
		"objectClass": {[]byte("top"), []byte("posixAccount")}, "uid": {[]byte("bob")}})
	// A cycle: byuid holds top, which holds up, which holds byuid.
	byUID := posix("byuid", map[string][][]byte{"memberUid": {[]byte("bob")}, "member": {[]byte("CN=top,CN=Users," + base)}})
	byDN := posix("bydn", map[string][][]byte{"member": {[]byte(user)}})
	up := posix("up", map[string][][]byte{"member": {[]byte(byUID)}})
	top := posix("top", map[string][][]byte{"member": {[]byte(up)}})
	posix("unrelated", map[string][][]byte{"memberUid": {[]byte("carol")}})

	c := e.client(e.d.Dial)
	a, ok, err := c.Lookup(ctx, directory.ByUserName, []byte("bob"))
	if err != nil || !ok {
		t.Fatalf("Lookup(bob) = %v, %v", ok, err)
	}
	got, err := c.Groups(ctx, a)
	if want := lowered(byUID, byDN, up, top); err != nil || !slices.Equal(dnsOf(got), want) {
		t.Errorf("Groups(bob) = %v, %v; want %v", dnsOf(got), err, want)
	}
}

// An rfc2307 closure deeper than 64 levels, or of more than 1 024 groups,
// fails after a bounded number of queries.
func TestGroupsRFC2307Bounds(t *testing.T) {
	for _, tc := range []struct {
		name    string
		groups  int
		nested  bool
		queries int
	}{
		{"70 levels deep", 70, true, 64},
		{"in 1 025 groups", 1025, false, 2}, // one level, two pages
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			e := newEnv(t)
			e.rec.Schema = directory.SchemaRFC2307
			e.d.AddUser(t, "bob", map[string][][]byte{
				"objectClass": {[]byte("top"), []byte("posixAccount")}, "uid": {[]byte("bob")}})
			prev := ""
			for i := range tc.groups {
				attrs := map[string][][]byte{"objectClass": {[]byte("top"), []byte("posixGroup")}}
				if tc.nested && prev != "" {
					attrs["member"] = [][]byte{[]byte(prev)}
				} else {
					attrs["memberUid"] = [][]byte{[]byte("bob")}
				}
				prev = e.d.AddGroup(t, fmt.Sprintf("p%04d", i), attrs)
			}
			f := e.faulty(t)
			c := e.client(f.Dial)
			a, ok, err := c.Lookup(ctx, directory.ByUserName, []byte("bob"))
			if err != nil || !ok {
				t.Fatalf("Lookup(bob) = %v, %v", ok, err)
			}
			got, err := c.Groups(ctx, a)
			if !errors.Is(err, vfserr.ErrUnavailable) || got != nil {
				t.Errorf("Groups(bob) %s = %d groups, %v; want none, ErrUnavailable", tc.name, len(got), err)
			}
			walk := func(r directory.Request) bool { return !strings.Contains(r.Filter, "(objectClass=posixGroup)") }
			if n := len(slices.DeleteFunc(f.Queries(), walk)); n != tc.queries {
				t.Errorf("Groups(bob) %s asked %d queries, want %d", tc.name, n, tc.queries)
			}
		})
	}
}

// bigGroup adds a group of n members to e's directory.
func bigGroup(t *testing.T, e *env, n int) (string, []string) {
	var members []string
	var vals [][]byte
	for i := range n {
		m := fmt.Sprintf("CN=m%04d,CN=Users,%s", i, base)
		members = append(members, m)
		vals = append(vals, []byte(m))
	}
	return e.d.AddGroup(t, "big", map[string][][]byte{"member": vals}), members
}

// A group of 3 000 members is read in two ranges of 1 500.
func TestMembersInRanges(t *testing.T) {
	e := newEnv(t)
	dn, members := bigGroup(t, e, 3000)
	f := e.faulty(t)
	got, err := directory.Members(context.Background(), e.client(f.Dial), dn)
	if err != nil || !slices.Equal(got, members) {
		t.Errorf("Members(big) = %d values, %v; want its 3 000 in order", len(got), err)
	}
	if n := len(memberQueries(f.Queries())); n != 2 {
		t.Errorf("%d range queries, want 2", n)
	}
}

func memberQueries(qs []directory.Request) []directory.Request {
	return slices.DeleteFunc(qs, func(q directory.Request) bool {
		return !slices.ContainsFunc(q.Attrs, func(a string) bool { return strings.HasPrefix(a, "member;range=") })
	})
}

// A hostile range fails the read without asking another: a second range
// whose bounds are reversed, that starts over at 0, or holds other than its
// count of values, and a first range whose name does not parse, which must
// not read as a group with no members.
func TestMembersHostileRange(t *testing.T) {
	for _, tc := range []struct {
		from       int
		torn, into string
	}{
		{2, "member;range=1500-*", "member;range=1500-1499"},
		{2, "member;range=1500-*", "member;range=0-1499"},
		{2, "member;range=1500-*", "member;range=1500-1600"},
		{2, "member;range=1500-*", "member;range=x-*"},
		{1, "member;range=0-1499", "member;range=0-x"},
		{2, "member;range=1500-*", "member"},
	} {
		t.Run(tc.into, func(t *testing.T) {
			e := newEnv(t)
			dn, _ := bigGroup(t, e, 3000)
			f := e.faulty(t, sim.Rule{Op: "search", Key: "(objectClass=group)", From: tc.from, Count: 1, Action: sim.FaultAction{TearAfter: 1}})
			f.TearAttr(tc.torn, tc.into)
			got, err := directory.Members(context.Background(), e.client(f.Dial), dn)
			if !errors.Is(err, vfserr.ErrUnavailable) || got != nil {
				t.Errorf("Members(big) with range %d torn to %s = %d values, %v; want none, ErrUnavailable", tc.from, tc.into, len(got), err)
			}
			if n := len(memberQueries(f.Queries())); n != tc.from {
				t.Errorf("%d range queries, want %d: none after the torn one", n, tc.from)
			}
		})
	}

	// Ranges of one value each: a group of 100 members takes more parts than
	// the bound, and the read stops at it.
	t.Run("more than 64 parts", func(t *testing.T) {
		e := newEnv(t)
		dn, _ := bigGroup(t, e, 100)
		e.d.SetMaxValRange(1)
		f := e.faulty(t)
		got, err := directory.Members(context.Background(), e.client(f.Dial), dn)
		if !errors.Is(err, vfserr.ErrUnavailable) || got != nil {
			t.Errorf("Members(big) in ranges of 1 = %d values, %v; want none, ErrUnavailable", len(got), err)
		}
		if n := len(memberQueries(f.Queries())); n != 64 {
			t.Errorf("%d range queries, want 64", n)
		}
	})
}

// SIDs are looked up at most 64 to a query, each found once; a SID two
// entries hold fails the whole lookup.
func TestLookupSIDs(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	var sids [][]byte
	c0 := e.client(e.d.Dial)
	for i := range 100 {
		name := fmt.Sprintf("u%03d", i)
		e.d.AddUser(t, name, nil)
		a, _, err := c0.Lookup(ctx, directory.ByUserName, []byte(name))
		if err != nil {
			t.Fatal(err)
		}
		sids = append(sids, a.SID)
	}
	f := e.faulty(t)
	got, err := e.client(f.Dial).LookupSIDs(ctx, append(sids, sids[0], []byte("no such SID")))
	if err != nil || len(got) != 100 {
		t.Fatalf("LookupSIDs(100 SIDs, a repeat and an unknown) = %d answers, %v; want 100", len(got), err)
	}
	for i, a := range got {
		if string(a.SID) != string(sids[i]) {
			t.Errorf("answer %d holds SID %x, want %x", i, a.SID, sids[i])
		}
	}
	var batches []int
	for _, q := range f.Queries() {
		if n := strings.Count(q.Filter, "(objectSid="); strings.HasPrefix(q.Filter, "(|") {
			batches = append(batches, n)
		}
	}
	if !slices.Equal(batches, []int{64, 37}) {
		t.Errorf("SID batches %v, want [64 37]", batches)
	}

	e.d.AddUser(t, "twin", map[string][][]byte{"objectSid": {sids[5]}})
	if got, err := e.client(e.d.Dial).LookupSIDs(ctx, sids[:10]); !errors.Is(err, vfserr.ErrUnavailable) || got != nil {
		t.Errorf("LookupSIDs with a SID two entries hold = %d answers, %v; want none, ErrUnavailable", len(got), err)
	}
}
