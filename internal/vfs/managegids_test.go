package vfs

import (
	"context"
	"encoding/hex"
	"errors"
	"net/netip"
	"strconv"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/cubbit/dittofs/pkg/identity"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/metadata/kv/kvtest"
	"github.com/cubbit/dittofs/pkg/sim"
	"github.com/cubbit/dittofs/pkg/vfs"
)

// manageGIDs writes the share's policy with ManageGIDs set to on, squashing
// root.
func (c *caching) manageGIDs(t *testing.T, on bool) {
	t.Helper()
	c.must(t, errOf(c.cp.PutExportPolicy(t.Context(), entity.ExportPolicy{Share: c.share, Squash: entity.SquashRoot, ManageGIDs: on}, policyVersion(t, c.cp, c.share))))
}

// readOnly is a file in dir, created by the admin, whose ACL lets only who
// read it.
func (c *caching) readOnly(t *testing.T, dir vfs.Handle, name string, who entity.Principal) vfs.Handle {
	t.Helper()
	admin := proven(c.fs.Admin.Principal, inLab)
	f, err := c.Create(t.Context(), admin, dir, []byte(name), vfs.CreateArgs{Kind: vfs.CreateRegular, Attrs: entity.Attrs{}.WithMode(0o600)})
	c.must(t, err)
	_, err = c.SetACL(t.Context(), admin, vfs.Target{H: f.Handle}, entity.ACL{Entries: []entity.ACE{{Type: entity.ACEAllow, Mask: entity.ReadData | entity.ReadAttributes, Who: who}}})
	c.must(t, err)
	return f.Handle
}

// read reads one byte of h as call.
func (c *caching) read(call vfs.Call, h vfs.Handle) error {
	_, _, err := c.Read(context.Background(), call, vfs.Anonymous(h), 0, make([]byte, 1))
	return err
}

// RFC 18 §10's "ManageGIDs": alice, a local user in 20 store groups, claims
// her uid and the first group's gid alone, and reads a file whose ACL grants
// only the 20th group. Refused with ManageGIDs off, her groups being the
// gid she sent; granted with it on, her groups being her closure. With it
// on, a file she creates claiming her second group's gid still takes that
// group, the credential's gid naming the group a create stamps; and with her
// membership read failing, the call is refused ErrUnavailable, never
// resolved with fewer groups. Catches the credential's gids still read under
// ManageGIDs, the credential's gid dropped from a create, and a closure that
// drops a failed read.
func TestClaimedManageGIDsReadsTheClosure(t *testing.T) {
	c := newCaching(t)
	ctx := t.Context()
	alice := c.user(t, "alice", "", c.share, entity.AccessReadWrite)
	var groups []entity.Group
	for i := range 20 {
		gid := strconv.Itoa(7001 + i)
		g := entity.Group{Principal: entity.NewGroup(), Name: "g" + gid, IDs: entity.ProtocolIDs{{Scheme: entity.SchemeGID, Value: gid}, {Scheme: entity.SchemeSID, Value: "sid-g" + gid}}}
		c.must(t, errOf(c.cp.PutGroup(ctx, g, 0, entity.Alloc{})))
		c.must(t, c.cp.AddMember(ctx, entity.Membership{Group: g.Principal, Member: alice.Principal}))
		groups = append(groups, g)
	}
	f := c.readOnly(t, c.fs.Root, "ledger", groups[19].Principal)
	call := claimed(inLab, uid(t, alice), 7001)
	for _, row := range []struct {
		on   bool
		want error
	}{{false, vfs.ErrAccess}, {true, nil}} {
		c.manageGIDs(t, row.on)
		if err := c.read(call, f); row.want == nil && err != nil || row.want != nil && !errors.Is(err, row.want) {
			t.Errorf("alice's claimed read of a file only her 20th group may read, ManageGIDs %v: %v, want %v", row.on, err, row.want)
		}
	}
	dir, err := c.Create(ctx, proven(c.fs.Admin.Principal, inLab), c.fs.Root, []byte("d"), vfs.CreateArgs{Kind: vfs.CreateDir, Attrs: entity.Attrs{}.WithMode(0o777)})
	c.must(t, err)
	r, err := c.Create(ctx, claimed(inLab, uid(t, alice), 7002), dir.Handle, []byte("f"), vfs.CreateArgs{Kind: vfs.CreateRegular, Attrs: entity.Attrs{}.WithMode(0o644)})
	if err != nil || r.File.Group != groups[1].Principal {
		t.Errorf("alice's create under ManageGIDs claiming gid 7002: group %v, err %v, want %v", r.File.Group, err, groups[1].Principal)
	}
	s, err := sim.NewSchedule(1, c.clock, sim.Rule{Op: kvtest.OpScan, Key: "MR‖" + hex.EncodeToString(alice.Principal.ID[:]) + "*", Action: sim.FaultAction{Err: errors.New("injected membership read fault")}})
	c.must(t, err)
	c.fault.SetFault(s)
	err = c.read(call, f)
	c.fault.SetFault(nil)
	if !errors.Is(err, vfs.ErrUnavailable) {
		t.Errorf("alice's claimed read under ManageGIDs with her membership read failing: %v, want ErrUnavailable", err)
	}
}

// A claimed directory principal's directory group grants the read once
// ManageGIDs is on, and turning it on reaches a caller already cached: bob,
// a directory user in Finance, claims his uid with a gid no principal holds
// and is refused a file only Finance may read, twice, the second call served
// from the cache; with ManageGIDs then turned on, his next call misses the
// cache and is granted. Catches a directory principal's directory groups
// left out of the closure, and a cache that serves the caller built before
// the change.
func TestClaimedManageGIDsReachesTheDirectoryAndTheCache(t *testing.T) {
	c := newDirCaching(t)
	ctx := t.Context()
	bobDN := c.numbered(t, "bob", 5000)
	c.d.AddGroup(t, "Finance", map[string][][]byte{"member": {[]byte(bobDN)}})
	finance, err := c.srv.Find(ctx, identity.Spelling{Scheme: identity.SchemeQualified, Text: `KORP\Finance`}, identity.FindOpts{Mint: true})
	c.must(t, err)
	f := c.readOnly(t, c.dir, "ledger", finance)
	call := claimed(netip.MustParseAddr("192.0.2.1"), 5000, 4999)
	c.manageGIDs(t, false)
	hits := testutil.ToFloat64(c.callers.hits)
	for i := range 2 {
		if err := c.read(call, f); !errors.Is(err, vfs.ErrAccess) {
			t.Errorf("bob's claimed read %d of a Finance-only file, ManageGIDs off: %v, want ErrAccess", i+1, err)
		}
	}
	if got := testutil.ToFloat64(c.callers.hits) - hits; got != 1 {
		t.Fatalf("caller cache hits for bob's two claimed reads %v, want 1", got)
	}
	c.manageGIDs(t, true)
	misses := testutil.ToFloat64(c.callers.misses)
	if err := c.read(call, f); err != nil {
		t.Errorf("bob's claimed read of a Finance-only file, ManageGIDs on: %v, want granted", err)
	}
	if got := testutil.ToFloat64(c.callers.misses) - misses; got != 1 {
		t.Errorf("caller cache misses for bob's read after ManageGIDs was turned on %v, want 1", got)
	}
}

// Under ManageGIDs, the directory groups a claimed directory principal has
// gained count against the node's mint cap, and the caller is never resolved
// with part of them: bob, minted with no group, gains 150 directory groups.
// Past the TTL, his next call mints 100 of them and is refused
// ErrUnavailable, the one after mints none and is refused too, and once the
// minute has passed his call mints the other 50 and is admitted, with the
// last group's grant. Catches the groups the directory step mints left
// uncharged, which mints all 150 at once, and a caller resolved with the
// groups minted so far.
func TestClaimedManageGIDsMintsUnderTheCap(t *testing.T) {
	c := newDirCaching(t)
	ctx := t.Context()
	c.manageGIDs(t, true)
	bobDN := c.numbered(t, "bob", 5000)
	call := claimed(netip.MustParseAddr("192.0.2.1"), 5000, 4999)
	if err := c.create(call, "first"); err != nil {
		t.Fatalf("bob's first claimed call: %v", err)
	}
	names := make([]string, 150)
	for i := range names {
		names[i] = "g" + strconv.Itoa(i)
		c.d.AddGroup(t, names[i], map[string][][]byte{"member": {[]byte(bobDN)}})
	}
	held := func() int {
		n := 0
		for _, name := range names {
			_, err := c.srv.Find(ctx, identity.Spelling{Scheme: identity.SchemeQualified, Text: `KORP\` + name}, identity.FindOpts{})
			switch {
			case err == nil:
				n++
			case !errors.Is(err, vfs.ErrUnknownPrincipal):
				t.Fatalf("find group %s: %v", name, err)
			}
		}
		return n
	}
	c.clock.Advance(5*time.Minute + time.Second)
	for _, row := range []struct {
		what string
		want int
	}{{"past the TTL", 100}, {"again, the cap's room spent", 100}} {
		if err := c.create(call, "refused"); !errors.Is(err, vfs.ErrUnavailable) {
			t.Errorf("bob's claimed call %s, his groups not all minted: %v, want ErrUnavailable", row.what, err)
		}
		if got := held(); got != row.want {
			t.Errorf("bob's directory groups held after his call %s: %d, want %d", row.what, got, row.want)
		}
	}
	c.clock.Advance(time.Minute)
	if err := c.create(call, "last"); err != nil {
		t.Fatalf("bob's claimed call once the cap has room for his remaining groups: %v, want admitted", err)
	}
	if got := held(); got != 150 {
		t.Errorf("bob's directory groups held after his admitted call: %d, want 150", got)
	}
	last, err := c.srv.Find(ctx, identity.Spelling{Scheme: identity.SchemeQualified, Text: `KORP\` + names[149]}, identity.FindOpts{})
	c.must(t, err)
	if err := c.read(call, c.readOnly(t, c.dir, "ledger", last)); err != nil {
		t.Errorf("bob's claimed read of a file only his last directory group may read: %v, want granted", err)
	}
}
