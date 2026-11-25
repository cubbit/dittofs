package vfs

import (
	"errors"
	"strconv"
	"testing"

	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/metadata/kv/kvtest"
	"github.com/cubbit/dittofs/pkg/sim"
	"github.com/cubbit/dittofs/pkg/vfs"
)

// claimedShare is the fixture's share granting everyone read-write, with
// nobody, a user, to be its Anonymous, alice and bob, users granted
// read-write, each with a file of its own at mode 0600, and a directory
// everyone may create in.
type claimedShare struct {
	*attrs
	nobody    entity.User
	aliceFile vfs.Handle
	bobFile   vfs.Handle
	dir       vfs.Handle
}

func newClaimedShare(t *testing.T) *claimedShare {
	t.Helper()
	a := newAttrs(t)
	c := &claimedShare{attrs: a, nobody: a.user(t, "nobody", "", a.share, entity.AccessReadWrite)}
	c.must(t, errOf(c.cp.SetGrant(t.Context(), entity.ShareGrant{Share: c.share, Principal: entity.Everyone, Access: entity.AccessReadWrite}, 0)))
	c.aliceFile = a.file(t, "alice-file", a.alice.Principal, 0o600)
	c.bobFile = a.file(t, "bob-file", a.bob.Principal, 0o600)
	d, err := c.Create(t.Context(), proven(c.fs.Admin.Principal, inLab), c.fs.Root, []byte("d"), vfs.CreateArgs{Kind: vfs.CreateDir, Attrs: entity.Attrs{}.WithMode(0o777)})
	c.must(t, err)
	c.dir = d.Handle
	return c
}

// squash sets the share's policy to squash, with nobody as its Anonymous.
func (c *claimedShare) squash(t *testing.T, sq entity.Squash) {
	t.Helper()
	c.must(t, errOf(c.cp.PutExportPolicy(t.Context(), entity.ExportPolicy{Share: c.share, Squash: sq, Anonymous: c.nobody.Principal}, policyVersion(t, c.cp, c.share))))
}

// uid is the uid u holds.
func uid(t *testing.T, u entity.User) uint32 {
	t.Helper()
	for _, id := range u.IDs {
		if id.Scheme == entity.SchemeUID {
			n, err := strconv.ParseUint(id.Value, 10, 32)
			if err != nil {
				t.Fatal(err)
			}
			return uint32(n)
		}
	}
	t.Fatalf("%s holds no uid", u.Name)
	return 0
}

// RFC 18 §10's "root" and "admin needs proof" rows and RFC 21 §8.1's
// privilege steps, over AUTH_SYS, at the operations whose permission check
// reads the caller. uid 0, with no user record, unsquashed is privileged: it
// may chown another user's file and owns what it creates. Squashed by a root
// squash it is nobody: it may not chown, nor read a 0600 file, and what it
// creates is nobody's. A claimed uid holding an admin grant is not
// privileged, squashed by root or not: it may not read another user's 0600
// file, take another user's file (refused by the file's ACL, ErrAccess), nor
// give its own file away (ErrPerm, as chown(2) refuses an unprivileged
// owner); signed in, the same user may do all three.
func TestClaimedPrivilegeFollowsTheProof(t *testing.T) {
	c := newClaimedShare(t)
	ctx := t.Context()
	ops := c.user(t, "ops", "1500", c.share, entity.AccessAdmin)
	opsFile := c.file(t, "ops-file", ops.Principal, 0o600)
	chown := func(call vfs.Call, h vfs.Handle, to entity.Principal) error {
		_, err := c.SetAttr(ctx, call, vfs.Target{H: h}, vfs.AttrChange{Attrs: entity.Attrs{}.WithOwner(to)})
		return err
	}
	read := func(call vfs.Call, h vfs.Handle) error {
		_, _, err := c.Read(ctx, call, vfs.Anonymous(h), 0, make([]byte, 1))
		return err
	}
	n := 0
	creates := func(call vfs.Call, want entity.Principal) error {
		n++
		r, err := c.Create(ctx, call, c.dir, []byte("f"+strconv.Itoa(n)), vfs.CreateArgs{Kind: vfs.CreateRegular, Attrs: entity.Attrs{}.WithMode(0o644)})
		if err == nil && r.File.Owner != want {
			return errors.New("the created file is owned by " + r.File.Owner.String() + ", want " + want.String())
		}
		return err
	}
	root := claimed(inLab, 0, 0)
	asOps := claimed(inLab, 1500, 1500)
	for _, row := range []struct {
		name   string
		squash entity.Squash
		do     func() error
		want   error
	}{
		{"root chowns bob's file to alice, unsquashed", entity.SquashNone, func() error { return chown(root, c.bobFile, c.alice.Principal) }, nil},
		{"root creates, unsquashed", entity.SquashNone, func() error { return creates(root, entity.RootPrincipal) }, nil},
		{"root chowns alice's file to bob, root-squashed", entity.SquashRoot, func() error { return chown(root, c.aliceFile, c.bob.Principal) }, vfs.ErrAccess},
		{"root reads alice's 0600 file, root-squashed", entity.SquashRoot, func() error { return read(root, c.aliceFile) }, vfs.ErrAccess},
		{"root creates, root-squashed", entity.SquashRoot, func() error { return creates(root, c.nobody.Principal) }, nil},
		{"admin uid reads alice's 0600 file, root-squashed", entity.SquashRoot, func() error { return read(asOps, c.aliceFile) }, vfs.ErrAccess},
		{"admin uid reads alice's 0600 file, unsquashed", entity.SquashNone, func() error { return read(asOps, c.aliceFile) }, vfs.ErrAccess},
		{"admin uid takes alice's file, root-squashed", entity.SquashRoot, func() error { return chown(asOps, c.aliceFile, ops.Principal) }, vfs.ErrAccess},
		{"admin uid gives its file to bob, root-squashed", entity.SquashRoot, func() error { return chown(asOps, opsFile, c.bob.Principal) }, vfs.ErrPerm},
		{"admin uid gives its file to bob, unsquashed", entity.SquashNone, func() error { return chown(asOps, opsFile, c.bob.Principal) }, vfs.ErrPerm},
		{"admin signed in reads alice's 0600 file", entity.SquashRoot, func() error { return read(proven(ops.Principal, inLab), c.aliceFile) }, nil},
		{"admin signed in gives its file to bob", entity.SquashRoot, func() error { return chown(proven(ops.Principal, inLab), opsFile, c.bob.Principal) }, nil},
	} {
		c.squash(t, row.squash)
		if err := row.do(); row.want == nil && err != nil || row.want != nil && !errors.Is(err, row.want) {
			t.Errorf("%s: %v, want %v", row.name, err, row.want)
		}
	}
	if got := c.record(t, c.bobFile).Owner; got != c.alice.Principal {
		t.Errorf("bob's file after root's chown is owned by %v, want alice", got)
	}
}

// RFC 18 §10's "squash order follows the proof", over AUTH_SYS: on an
// all-squash share granting alice read-write and everyone read, a write
// claiming alice's uid is refused, the share's grant read for the squashed
// principal; alice signed in writes, her grant read before the squash.
func TestClaimedSquashOrderFollowsTheProof(t *testing.T) {
	s := newShares(t, nil, nil)
	ctx := t.Context()
	alice := s.user(t, "alice", "", s.share, entity.AccessReadWrite)
	nobody := entity.User{Principal: entity.NewUser(), Name: "nobody", IDs: entity.ProtocolIDs{{Scheme: entity.SchemeUID, Value: "60999"}, {Scheme: entity.SchemeSID, Value: "sid-60999"}}}
	s.must(t, errOf(s.cp.PutUser(ctx, nobody, 0, entity.Alloc{})))
	s.must(t, errOf(s.cp.SetGrant(ctx, entity.ShareGrant{Share: s.share, Principal: entity.Everyone, Access: entity.AccessRead}, 0)))
	dir, err := s.Create(ctx, proven(s.fs.Admin.Principal, inLab), s.fs.Root, []byte("d"), vfs.CreateArgs{Kind: vfs.CreateDir, Attrs: entity.Attrs{}.WithMode(0o777)})
	s.must(t, err)
	s.must(t, errOf(s.cp.PutExportPolicy(ctx, entity.ExportPolicy{Share: s.share, Squash: entity.SquashAll, Anonymous: nobody.Principal}, policyVersion(t, s.cp, s.share))))
	mkdir := func(call vfs.Call, name string) error {
		_, err := s.Create(ctx, call, dir.Handle, []byte(name), vfs.CreateArgs{Kind: vfs.CreateDir, Attrs: entity.Attrs{}.WithMode(0o777)})
		return err
	}
	if err := mkdir(claimed(inLab, uid(t, alice), 1000), "claimed"); !errors.Is(err, vfs.ErrAccess) {
		t.Errorf("a create claiming alice's uid on an all-squash share: %v, want ErrAccess", err)
	}
	if err := mkdir(proven(alice.Principal, inLab), "proven"); err != nil {
		t.Errorf("alice's create signed in, squashed: %v, want admitted by her read-write grant", err)
	}
}

// A squashed claimed caller carries nothing of the principal it replaced: a
// file whose ACL grants read to alice alone is refused to a claim of alice's
// uid on an all-squash share, and read by it on a share squashing nobody.
func TestClaimedSquashDropsTheReplacedPrincipal(t *testing.T) {
	c := newClaimedShare(t)
	ctx := t.Context()
	acl := entity.ACL{Entries: []entity.ACE{{Type: entity.ACEAllow, Mask: entity.ReadData | entity.ReadAttributes, Who: c.alice.Principal}}}
	_, err := c.SetACL(ctx, proven(c.fs.Admin.Principal, inLab), vfs.Target{H: c.bobFile}, acl)
	c.must(t, err)
	alice := claimed(inLab, uid(t, c.alice), 1000)
	for _, row := range []struct {
		squash entity.Squash
		want   error
	}{{entity.SquashAll, vfs.ErrAccess}, {entity.SquashNone, nil}} {
		c.squash(t, row.squash)
		_, _, err := c.Read(ctx, alice, vfs.Anonymous(c.bobFile), 0, make([]byte, 1))
		if row.want == nil && err != nil || row.want != nil && !errors.Is(err, row.want) {
			t.Errorf("a claim of alice's uid reading a file only alice may read, squash %v: %v, want %v", row.squash, err, row.want)
		}
	}
}

// A file a claimed caller creates takes the group its claimed gid maps to;
// with a gid no principal holds, the user's primary group, never a group
// for that number.
func TestClaimedCreateTakesTheMappedGroup(t *testing.T) {
	s := newShares(t, nil, nil)
	ctx := t.Context()
	staff := entity.Group{Principal: entity.NewGroup(), Name: "staff", IDs: entity.ProtocolIDs{{Scheme: entity.SchemeGID, Value: "3000"}, {Scheme: entity.SchemeSID, Value: "sid-staff"}}}
	users := entity.Group{Principal: entity.NewGroup(), Name: "users", IDs: entity.ProtocolIDs{{Scheme: entity.SchemeGID, Value: "3001"}, {Scheme: entity.SchemeSID, Value: "sid-users"}}}
	for _, g := range []entity.Group{staff, users} {
		s.must(t, errOf(s.cp.PutGroup(ctx, g, 0, entity.Alloc{})))
	}
	alice := entity.User{Principal: entity.NewUser(), Name: "alice", Primary: users.Principal,
		IDs: entity.ProtocolIDs{{Scheme: entity.SchemeUID, Value: "1000"}, {Scheme: entity.SchemeSID, Value: "sid-1000"}}}
	s.must(t, errOf(s.cp.PutUser(ctx, alice, 0, entity.Alloc{})))
	s.must(t, errOf(s.cp.SetGrant(ctx, entity.ShareGrant{Share: s.share, Principal: alice.Principal, Access: entity.AccessReadWrite}, 0)))
	s.must(t, errOf(s.cp.PutExportPolicy(ctx, entity.ExportPolicy{Share: s.share, Squash: entity.SquashNone}, policyVersion(t, s.cp, s.share))))
	dir, err := s.Create(ctx, proven(s.fs.Admin.Principal, inLab), s.fs.Root, []byte("d"), vfs.CreateArgs{Kind: vfs.CreateDir, Attrs: entity.Attrs{}.WithMode(0o777)})
	s.must(t, err)
	for _, row := range []struct {
		name string
		gid  uint32
		want entity.Principal
	}{{"mapped", 3000, staff.Principal}, {"unmapped", 4242, users.Principal}} {
		r, err := s.Create(ctx, claimed(inLab, 1000, row.gid), dir.Handle, []byte(row.name), vfs.CreateArgs{Kind: vfs.CreateRegular, Attrs: entity.Attrs{}.WithMode(0o644)})
		if err != nil {
			t.Fatalf("alice's create claiming gid %d: %v", row.gid, err)
		}
		if r.File.Group != row.want {
			t.Errorf("a file created claiming the %s gid %d has group %v, want %v", row.name, row.gid, r.File.Group, row.want)
		}
	}
}

// RFC 18 §10's "error is not "unknown"", over AUTH_SYS: on a share whose
// everyone grant is read-write, a store error on the uid index is a
// retryable error and nothing is created in a directory anonymous could
// create in, its listing after the call unchanged; never a create as
// anonymous.
func TestClaimedStoreErrorCreatesNothing(t *testing.T) {
	c := newCaching(t)
	ctx := t.Context()
	c.must(t, errOf(c.cp.SetGrant(ctx, entity.ShareGrant{Share: c.share, Principal: entity.Everyone, Access: entity.AccessReadWrite}, 0)))
	c.must(t, errOf(c.cp.PutExportPolicy(ctx, entity.ExportPolicy{Share: c.share, Squash: entity.SquashNone}, policyVersion(t, c.cp, c.share))))
	admin := proven(c.fs.Admin.Principal, inLab)
	dir, err := c.Create(ctx, admin, c.fs.Root, []byte("d"), vfs.CreateArgs{Kind: vfs.CreateDir, Attrs: entity.Attrs{}.WithMode(0o777)})
	c.must(t, err)
	list := func() int {
		p, err := c.ReadDir(ctx, admin, dir.Handle, 0, 100, 0)
		c.must(t, err)
		return len(p.Entries)
	}
	before := list()
	s, err := sim.NewSchedule(1, c.clock, sim.Rule{Op: kvtest.OpGet, Key: "PX‖01*", Action: sim.FaultAction{Err: errors.New("injected uid index fault")}})
	c.must(t, err)
	c.fault.SetFault(s)
	_, err = c.Create(ctx, claimed(inLab, 4242, 4242), dir.Handle, []byte("x"), vfs.CreateArgs{Kind: vfs.CreateRegular, Attrs: entity.Attrs{}.WithMode(0o666)})
	c.fault.SetFault(nil)
	if !errors.Is(err, vfs.ErrUnavailable) {
		t.Errorf("a claimed create with the uid index failing: %v, want ErrUnavailable", err)
	}
	if _, _, err := c.Lookup(ctx, admin, dir.Handle, []byte("x")); !errors.Is(err, vfs.ErrNotFound) {
		t.Errorf("the name after the refused create: %v, want ErrNotFound", err)
	}
	if got := list(); got != before {
		t.Errorf("the directory lists %d entries after the refused create, want %d", got, before)
	}
}
