package directory_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

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

// An ad user's closure is every level of its nesting, its primary group and
// that group's own groups, through tokenGroups; and the same through the
// chain rule and primaryGroupID for a bind that cannot read tokenGroups,
// which member alone would answer without the primary group.
func TestGroupsAD(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	user := e.d.AddUser(t, "alice", nil)
	g1 := e.d.AddGroup(t, "g1", map[string][][]byte{"member": {[]byte(user)}})
	g2 := e.d.AddGroup(t, "g2", map[string][][]byte{"member": {[]byte(g1)}})
	g3 := e.d.AddGroup(t, "g3", map[string][][]byte{"member": {[]byte(g2)}})
	du := "CN=Domain Users,CN=Users," + base
	pgParent := e.d.AddGroup(t, "staff", map[string][][]byte{"member": {[]byte(du)}})
	e.d.AddGroup(t, "other", nil)
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
		chained := slices.ContainsFunc(f.Queries(), func(q directory.Request) bool { return strings.Contains(q.Filter, "1.2.840.113556.1.4.1941") })
		if chained != hidden {
			t.Errorf("tokenGroups hidden %v: the chain rule asked %v, want %v", hidden, chained, hidden)
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
	for i := range 1500 {
		e.d.AddGroup(t, fmt.Sprintf("g%04d", i), map[string][][]byte{"member": {[]byte(user)}})
	}
	e.d.HideTokenGroups()
	chain := func(r directory.Request) bool { return strings.Contains(r.Filter, "1.2.840.113556.1.4.1941") }

	f := e.faulty(t)
	c := e.client(f.Dial)
	a, _, err := c.Lookup(ctx, directory.ByUserName, []byte("alice"))
	if err != nil {
		t.Fatal(err)
	}
	got, err := c.Groups(ctx, a)
	if err != nil || len(got) != 1501 {
		t.Fatalf("Groups(alice) = %d groups, %v; want its 1 500 and Domain Users", len(got), err)
	}
	var filter string
	for _, q := range f.Queries() {
		if chain(q) {
			filter = q.Filter
		}
	}

	f = e.faulty(t, sim.Rule{Op: "search", Key: filter, From: 2, Count: 1, Action: sim.FaultAction{Err: errors.New("page lost")}})
	c = e.client(f.Dial)
	got, err = c.Groups(ctx, a)
	if !errors.Is(err, vfserr.ErrUnavailable) || got != nil {
		t.Errorf("Groups(alice) with its second page failed = %d groups, %v; want none, ErrUnavailable", len(got), err)
	}
	if n := len(slices.DeleteFunc(f.Queries(), func(r directory.Request) bool { return !chain(r) })); n != 2 {
		t.Errorf("%d chain-rule pages asked, want the first and the failed second", n)
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
	got, err := e.client(f.Dial).Members(context.Background(), dn)
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
	} {
		t.Run(tc.into, func(t *testing.T) {
			e := newEnv(t)
			dn, _ := bigGroup(t, e, 3000)
			f := e.faulty(t, sim.Rule{Op: "search", Key: "(objectClass=group)", From: tc.from, Count: 1, Action: sim.FaultAction{TearAfter: 1}})
			f.TearAttr(tc.torn, tc.into)
			got, err := e.client(f.Dial).Members(context.Background(), dn)
			if !errors.Is(err, vfserr.ErrUnavailable) || got != nil {
				t.Errorf("Members(big) with range %d torn to %s = %d values, %v; want none, ErrUnavailable", tc.from, tc.into, len(got), err)
			}
			if n := len(memberQueries(f.Queries())); n != tc.from {
				t.Errorf("%d range queries, want %d: none after the torn one", n, tc.from)
			}
		})
	}
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
