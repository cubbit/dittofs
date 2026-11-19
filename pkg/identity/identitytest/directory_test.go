package identitytest_test

import (
	"context"
	"errors"
	"slices"
	"testing"
	"testing/synctest"
	"time"

	"github.com/cubbit/dittofs/internal/identity/directory"
	"github.com/cubbit/dittofs/pkg/identity/identitytest"
	"github.com/cubbit/dittofs/pkg/sim"
)

const base = "DC=corp,DC=example"

// The in-memory directory answers as the suite holds a real one to.
func TestDirectoryConformance(t *testing.T) {
	identitytest.DirectoryConformance(t, func(t *testing.T) (directory.Conn, identitytest.Seeder) {
		d := identitytest.NewDirectory(base)
		c, err := d.Dial(context.Background(), directory.Record{})
		if err != nil {
			t.Fatal(err)
		}
		return c, d
	})
}

// The filters, paging and attributes the in-memory directory cannot answer
// are refused, not answered wrongly.
func TestDirectoryRefusesWhatItCannotRead(t *testing.T) {
	ctx := context.Background()
	d := identitytest.NewDirectory(base)
	c, _ := d.Dial(ctx, directory.Record{})
	name, pw := d.Account()
	if err := c.Bind(ctx, name, pw); err != nil {
		t.Fatal(err)
	}
	for _, filter := range []string{
		"(cn~=x)", "(uidNumber>=1)", "(uidNumber<=1)",
		"(member:1.2.840.113556.1.4.1941:=CN=x)",
		"(cn=x", "cn=x", "(cn=a**b)", "(cn=x)(cn=y)", `(cn=\4)`,
	} {
		if _, err := c.Search(ctx, directory.Request{Base: base, Scope: directory.ScopeSubtree, Filter: filter}); err == nil {
			t.Errorf("filter %q answered, want it refused", filter)
		}
	}
	if _, err := c.Search(ctx, directory.Request{Base: base, Scope: directory.ScopeSubtree,
		Filter: "(cn=*)", PageSize: 10}); err == nil {
		t.Error("a paged search answered, want it refused")
	}
	for _, attr := range []string{"tokenGroups", "TOKENGROUPS", "tokenGroupsGlobalAndUniversal", "tokenGroupsNoGCAcceptable", "*", "+", "1.1"} {
		if res, err := c.Search(ctx, directory.Request{Base: name, Scope: directory.ScopeBase,
			Filter: "(objectClass=*)", Attrs: []string{"cn", attr}}); err == nil {
			t.Errorf("a search asking for %q answered %v, want it refused", attr, res.Entries)
		}
	}
}

// The fault wrapper sends each call on, records the search, then applies the
// fault: an error, a hold until the context ends, a torn attribute.
func TestFaultyDirectory(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx := context.Background()
		d := identitytest.NewDirectory(base)
		user := d.AddUser(t, "alice", map[string][][]byte{"member;range=0-1499": {[]byte("x")}})
		errDown := errors.New("down")
		clock := sim.NewTestClock(time.Unix(0, 0))
		sched, err := sim.NewSchedule(1, clock,
			sim.Rule{Op: "search", Key: "(cn=alice)", From: 1, Count: 1, Action: sim.FaultAction{Err: errDown}},
			sim.Rule{Op: "search", Key: "(cn=alice)", From: 2, Count: 1, Action: sim.FaultAction{TearAfter: 1}},
			sim.Rule{Op: "search", Key: "(cn=alice)", From: 3, Count: 1, Action: sim.FaultAction{Err: identitytest.ErrHold}},
		)
		if err != nil {
			t.Fatal(err)
		}
		f := identitytest.NewFaultyDirectory(d.Dial, sched)
		// Another case than the directory's, as a real one may return it.
		f.TearAttr("MEMBER;RANGE=0-1499", "member;range=1500-1499")
		c, err := f.Dial(ctx, directory.Record{})
		if err != nil {
			t.Fatal(err)
		}
		name, pw := d.Account()
		if err := c.Bind(ctx, name, pw); err != nil {
			t.Fatal(err)
		}
		req := directory.Request{Base: base, Scope: directory.ScopeSubtree, Filter: "(cn=alice)"}

		if _, err := c.Search(ctx, req); !errors.Is(err, errDown) {
			t.Errorf("first search: err %v, want %v", err, errDown)
		}
		res, err := c.Search(ctx, req)
		if err != nil || len(res.Entries) != 1 || res.Entries[0].DN != user {
			t.Fatalf("second search: %v, %v, want %s", res, err, user)
		}
		if _, ok := res.Entries[0].Attrs["member;range=1500-1499"]; !ok {
			t.Errorf("second search's attributes %v, want member;range=0-1499 torn to member;range=1500-1499", res.Entries[0].Attrs)
		}
		hctx, cancel := context.WithCancel(ctx)
		done := make(chan error, 1)
		go func() { _, err := c.Search(hctx, req); done <- err }()
		synctest.Wait()
		select {
		case err := <-done:
			t.Fatalf("held search answered %v before its context ended, want it held", err)
		default:
		}
		cancel()
		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Errorf("held search: err %v, want %v once its context ended", err, context.Canceled)
		}
		if got := f.Queries(); len(got) != 3 || !slices.ContainsFunc(got, func(r directory.Request) bool { return r.Filter == req.Filter }) {
			t.Errorf("queries recorded %v, want the three searches sent", got)
		}
	})
}

// A bind the fault fails leaves nothing bound, though the directory accepted
// it: a search after it, by a caller that ignored the error, finds nothing.
func TestFaultyDirectoryFailedBindLeavesNothingBound(t *testing.T) {
	ctx := context.Background()
	d := identitytest.NewDirectory(base)
	errDown := errors.New("down")
	sched, err := sim.NewSchedule(1, sim.NewTestClock(time.Unix(0, 0)),
		sim.Rule{Op: "bind", Key: "*", Count: 1, Action: sim.FaultAction{Err: errDown}})
	if err != nil {
		t.Fatal(err)
	}
	c, err := identitytest.NewFaultyDirectory(d.Dial, sched).Dial(ctx, directory.Record{})
	if err != nil {
		t.Fatal(err)
	}
	name, pw := d.Account()
	if err := c.Bind(ctx, name, pw); !errors.Is(err, errDown) {
		t.Fatalf("bind: err %v, want %v", err, errDown)
	}
	res, err := c.Search(ctx, directory.Request{Base: base, Scope: directory.ScopeSubtree, Filter: "(objectClass=user)"})
	if err == nil {
		t.Errorf("search after a failed bind answered %d entries, want it refused", len(res.Entries))
	}
}

// A tear before TearAttr names the attribute to tear is a mistake in the
// test, and panics rather than answering untorn.
func TestFaultyDirectoryTearNeedsTearAttr(t *testing.T) {
	ctx := context.Background()
	d := identitytest.NewDirectory(base)
	d.AddUser(t, "alice", nil)
	sched, err := sim.NewSchedule(1, sim.NewTestClock(time.Unix(0, 0)),
		sim.Rule{Op: "search", Key: "*", Count: 1, Action: sim.FaultAction{TearAfter: 1}})
	if err != nil {
		t.Fatal(err)
	}
	c, _ := identitytest.NewFaultyDirectory(d.Dial, sched).Dial(ctx, directory.Record{})
	name, pw := d.Account()
	if err := c.Bind(ctx, name, pw); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if recover() == nil {
			t.Error("a torn search with no TearAttr answered, want a panic")
		}
	}()
	_, _ = c.Search(ctx, directory.Request{Base: base, Scope: directory.ScopeSubtree, Filter: "(cn=alice)"})
}

// objectGUID is compared byte for byte, as an octet string: a GUID whose
// bytes spell a letter's other case is another GUID.
func TestDirectoryComparesBinaryExactly(t *testing.T) {
	ctx := context.Background()
	d := identitytest.NewDirectory(base)
	upper := d.AddUser(t, "upper", map[string][][]byte{"objectGUID": {[]byte("ABCDEFGHIJKLMNOP")}})
	d.AddUser(t, "lower", map[string][][]byte{"objectGUID": {[]byte("abcdefghijklmnop")}})
	c, _ := d.Dial(ctx, directory.Record{})
	name, pw := d.Account()
	if err := c.Bind(ctx, name, pw); err != nil {
		t.Fatal(err)
	}
	res, err := c.Search(ctx, directory.Request{Base: base, Scope: directory.ScopeSubtree, Filter: "(objectGUID=ABCDEFGHIJKLMNOP)"})
	if err != nil || len(res.Entries) != 1 || res.Entries[0].DN != upper {
		t.Errorf("(objectGUID=ABCDEFGHIJKLMNOP): %v, %v, want only %s", res.Entries, err, upper)
	}
}
