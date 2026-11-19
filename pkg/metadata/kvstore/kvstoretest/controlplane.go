package kvstoretest

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"github.com/google/uuid"
	"iter"
	"net/netip"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/auth/sid"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	merrs "github.com/cubbit/dittofs/pkg/metadata/errors"
	"github.com/cubbit/dittofs/pkg/metadata/kv"
	"github.com/cubbit/dittofs/pkg/metadata/kv/keys"
	"github.com/cubbit/dittofs/pkg/metadata/kvstore"
)

var ctx = context.Background()

// newStore initialises and opens a store over a fresh KV from o, behind a
// counting wrapper so a case can assert an operation's cost.
func newStore(t testing.TB, kvo Opener) (*kvstore.Store, *kv.Counting) {
	t.Helper()
	c := &kv.Counting{KV: kvo(t)}
	if err := kvstore.Init(ctx, c, uuid.New()); err != nil {
		t.Fatal(err)
	}
	s, err := kvstore.Open(ctx, c)
	if err != nil {
		t.Fatal(err)
	}
	c.Take()
	return s, c
}

func must(t testing.TB, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func all[T any](t testing.TB, seq iter.Seq2[T, error]) []T {
	t.Helper()
	var out []T
	for v, err := range seq {
		must(t, err)
		out = append(out, v)
	}
	return out
}

// user is a user holding ids, and a fresh SID beside a uid that has none,
// as the store keeps a user's file spellings together.
func user(name string, ids ...entity.ProtocolID) entity.User {
	if slices.ContainsFunc(ids, func(id entity.ProtocolID) bool { return id.Scheme == entity.SchemeUID }) &&
		!slices.ContainsFunc(ids, func(id entity.ProtocolID) bool { return id.Scheme == entity.SchemeSID }) {
		ids = append(ids, freshSID())
	}
	return entity.User{Principal: entity.NewUser(), Name: name, IDs: ids}
}

// fixtureIDs numbers the gids and SIDs fixtures hand out, so no two collide.
var fixtureIDs atomic.Uint32

// freshSID is a SID no other fixture holds.
func freshSID() entity.ProtocolID { return sidOf(fmt.Sprintf("fixture-sid-%d", fixtureIDs.Add(1))) }

// grantVersion is the version of the grant naming p on share, zero for none:
// what a write replacing or deleting it expects.
func grantVersion(t *testing.T, cp kvstore.ControlPlane, share entity.ShareID, p entity.Principal) uint64 {
	t.Helper()
	g, _, err := cp.ShareGrant(ctx, share, p)
	must(t, err)
	return g.Version
}

// effective is the share access the store's check gives p, signed in, with
// its store groups: none when the share's root is refused, read when a write
// to it is, else read-write. The caller is privileged, so no ACL decides and
// the grants alone do.
func effective(t *testing.T, s *kvstore.Store, share entity.ShareID, p entity.Principal) entity.ShareAccess {
	t.Helper()
	id := entity.Identity{Principal: p, Groups: all(t, s.Principals().Groups(ctx, p)), Authenticated: true, Privileged: true}
	root, _, err := s.Files().Root(ctx, id, share)
	if isDenied(err) {
		return entity.AccessNone
	}
	must(t, err)
	err = s.Files().Authorize(ctx, id, root, nil, entity.WriteData)
	if isDenied(err) {
		return entity.AccessRead
	}
	must(t, err)
	return entity.AccessReadWrite
}

// group is a group holding a fresh gid, above the default allocation range,
// and a fresh SID.
func group(name string) entity.Group {
	n := fixtureIDs.Add(1)
	return entity.Group{Principal: entity.NewGroup(), Name: name, IDs: entity.ProtocolIDs{gid(fmt.Sprint(2_000_000_000 + n)), freshSID()}}
}

func uid(n string) entity.ProtocolID { return entity.ProtocolID{Scheme: entity.SchemeUID, Value: n} }

func share(name, path string) entity.Share {
	return entity.Share{ID: entity.NewShareID(), Name: name, Path: path, Root: entity.NewFileID()}
}

func testUserRenameAndIDsMoveTheirRows(t *testing.T, kvo Opener) {
	s, _ := newStore(t, kvo)
	cp, p := s.ControlPlane(), s.Principals()
	u := user("alice", uid("1000"))
	must(t, errOf(cp.PutUser(ctx, u, 0, entity.Alloc{})))

	u.Name, u.IDs = "alicia", entity.ProtocolIDs{uid("1001"), u.IDs[1]}
	must(t, errOf(cp.PutUser(ctx, u, userGeneration(t, s, u.Principal), entity.Alloc{})))
	if _, err := p.ByName(ctx, entity.PrincipalUser, "alice"); !merrs.IsNotFoundError(err) {
		t.Errorf("old name still resolves: %v", err)
	}
	if got, err := p.ByName(ctx, entity.PrincipalUser, "alicia"); err != nil || got != u.Principal {
		t.Errorf("new name: %v, %v", got, err)
	}
	if _, err := p.ByProtocolID(ctx, uid("1000")); !merrs.IsNotFoundError(err) {
		t.Errorf("old uid still resolves: %v", err)
	}
	if got, err := p.ByProtocolID(ctx, uid("1001")); err != nil || got != u.Principal {
		t.Errorf("new uid: %v, %v", got, err)
	}
	// Both are free for another user now.
	must(t, errOf(cp.PutUser(ctx, user("alice", uid("1000")), 0, entity.Alloc{})))

	users := all(t, cp.Users(ctx, ""))
	if len(users) != 2 || users[0].Name != "alice" || users[1].Name != "alicia" {
		t.Fatalf("Users = %+v", users)
	}
	if !slices.Equal(users[1].IDs, u.IDs) {
		t.Errorf("IDs = %v", users[1].IDs)
	}
	if after := all(t, cp.Users(ctx, "alice")); len(after) != 1 || after[0].Name != "alicia" {
		t.Errorf("Users after alice = %+v", after)
	}
}

func testPrincipalUniqueness(t *testing.T, kvo Opener) {
	s, _ := newStore(t, kvo)
	cp := s.ControlPlane()
	must(t, errOf(cp.PutUser(ctx, user("alice", uid("1000")), 0, entity.Alloc{})))
	if _, err := cp.PutUser(ctx, user("alice"), 0, entity.Alloc{}); !merrs.IsAlreadyExistsError(err) {
		t.Errorf("second alice: %v", err)
	}
	if _, err := cp.PutUser(ctx, user("bob", uid("1000")), 0, entity.Alloc{}); !merrs.IsAlreadyExistsError(err) {
		t.Errorf("uid 1000 twice: %v", err)
	}
	// A user and a group may share a name.
	must(t, errOf(cp.PutGroup(ctx, group("alice"), 0, entity.Alloc{})))
	for _, bad := range []entity.User{
		user("carol", uid("01000")),
		user("carol", uid("x")),
		user("carol", entity.ProtocolID{Scheme: entity.SchemeGID, Value: "5"}),
		user("carol", uid("5"), uid("5")),
	} {
		if _, err := cp.PutUser(ctx, bad, 0, entity.Alloc{}); err == nil {
			t.Errorf("accepted %+v", bad)
		}
	}
}

// deleted runs the batches of p's deletion, which err, the mark's outcome,
// started, to the end.
func deleted(s *kvstore.Store, err error, p entity.Principal) error {
	for range 1000 {
		var done bool
		if err == nil {
			done, err = s.Nodes().ContinueDelete(ctx, p)
		}
		if err != nil || done {
			return err
		}
	}
	return fmt.Errorf("the deletion of %v not done after 1000 batches", p)
}

func testDeleteRemovesMembershipsAndGrants(t *testing.T, kvo Opener) {
	s, _ := newStore(t, kvo)
	cp, p := s.ControlPlane(), s.Principals()
	u, g, outer := user("alice"), group("staff"), group("all")
	sh := share("photos", "/photos")
	must(t, errOf(cp.PutUser(ctx, u, 0, entity.Alloc{})))
	must(t, errOf(cp.PutGroup(ctx, g, 0, entity.Alloc{})))
	must(t, errOf(cp.PutGroup(ctx, outer, 0, entity.Alloc{})))
	must(t, errOf(CreateShare(ctx, cp, sh)))
	must(t, cp.AddMember(ctx, entity.Membership{Group: g.Principal, Member: u.Principal}))
	must(t, cp.AddMember(ctx, entity.Membership{Group: outer.Principal, Member: g.Principal}))
	must(t, errOf(cp.SetGrant(ctx, entity.ShareGrant{Share: sh.ID, Principal: outer.Principal, Access: entity.AccessRead}, 0)))
	if got := all(t, p.Groups(ctx, u.Principal)); len(got) != 2 {
		t.Fatalf("Groups = %v", got)
	}
	if a := effective(t, s, sh.ID, u.Principal); a != entity.AccessRead {
		t.Fatalf("access through nested groups = %v, want read", a)
	}

	// Deleting the middle group cuts u off from outer and its grant.
	must(t, cp.DeleteGroup(ctx, g.Principal, groupGeneration(t, cp, g.Principal)))
	if got := all(t, p.Groups(ctx, u.Principal)); len(got) != 0 {
		t.Errorf("Groups after DeleteGroup = %v", got)
	}
	if a := effective(t, s, sh.ID, u.Principal); a != entity.AccessNone {
		t.Errorf("access after DeleteGroup = %v, want none", a)
	}
	// Deleting outer drops its grant; a new group can take the name.
	must(t, cp.DeleteGroup(ctx, outer.Principal, groupGeneration(t, cp, outer.Principal)))
	again := group("all")
	must(t, errOf(cp.PutGroup(ctx, again, 0, entity.Alloc{})))
	if a, ok, err := p.Grant(ctx, sh.ID, again.Principal); err != nil || ok {
		t.Errorf("a new group inherited a deleted one's grant: %v, %v, %v; want none, false", a, ok, err)
	}
	must(t, cp.DeleteUser(ctx, u.Principal, userGeneration(t, s, u.Principal)))
	if _, err := p.ByName(ctx, entity.PrincipalUser, "alice"); !merrs.IsNotFoundError(err) {
		t.Errorf("deleted user resolves: %v", err)
	}
	if err := cp.DeleteUser(ctx, u.Principal, 0); !merrs.IsNotFoundError(err) {
		t.Errorf("second delete: %v", err)
	}
	if err := cp.AddMember(ctx, entity.Membership{Group: again.Principal, Member: u.Principal}); !merrs.IsNotFoundError(err) {
		t.Errorf("membership of a deleted user: %v", err)
	}
}

func testShares(t *testing.T, kvo Opener) {
	s, _ := newStore(t, kvo)
	cp := s.ControlPlane()
	list := func() uint64 { return counters(t, s).ShareList }
	home := share("Home", "/home")
	must(t, errOf(CreateShare(ctx, cp, home)))
	if list() != 1 {
		t.Errorf("ShareList after create = %d", list())
	}
	for name, sh := range map[string]entity.Share{
		"name under the fold": share("HOME", "/other"),
		"same path":           share("other", "/home"),
		"descendant path":     share("alice", "/home/alice"),
		"ancestor path":       share("top", "/srv"),
	} {
		if name == "ancestor path" {
			must(t, errOf(CreateShare(ctx, cp, share("deep", "/srv/a/b"))))
			v := list()
			if _, err := CreateShare(ctx, cp, sh); !merrs.IsAlreadyExistsError(err) {
				t.Errorf("%s: %v", name, err)
			}
			if list() != v {
				t.Errorf("%s: a refused put raised ShareList", name)
			}
			continue
		}
		if _, err := CreateShare(ctx, cp, sh); !merrs.IsAlreadyExistsError(err) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := CreateShare(ctx, cp, share("deeper", "/srv/a/b/c")); !merrs.IsAlreadyExistsError(err) {
		t.Errorf("below a share: %v", err)
	}
	// /homes is not below /home.
	must(t, errOf(CreateShare(ctx, cp, share("homes", "/homes"))))

	// Rename and re-path: the old name and path are free, ShareList moves.
	v := list()
	home.Name, home.Path = "house", "/house"
	must(t, errOf(cp.PutShare(ctx, home, 1)))
	if list() != v+1 {
		t.Errorf("ShareList after rename = %d, want %d", list(), v+1)
	}
	must(t, errOf(CreateShare(ctx, cp, share("home", "/home"))))
	// A put that changes nothing raises nothing.
	v = list()
	must(t, errOf(cp.PutShare(ctx, home, 2)))
	if list() != v {
		t.Error("an unchanged put raised ShareList")
	}
	moved := home
	moved.Root = entity.NewFileID()
	if _, err := cp.PutShare(ctx, moved, 3); err == nil {
		t.Error("a share's root changed")
	}

	got := all(t, cp.Shares(ctx))
	if len(got) != 4 {
		t.Fatalf("Shares = %+v", got)
	}
	for _, sh := range got {
		if sh.State != entity.ShareEnabled {
			t.Errorf("%s starts %v", sh.Name, sh.State)
		}
	}
}

func testSetShareState(t *testing.T, kvo Opener) {
	s, _ := newStore(t, kvo)
	cp := s.ControlPlane()
	sh := share("photos", "/photos")
	must(t, errOf(CreateShare(ctx, cp, sh)))
	v := counters(t, s).ShareList
	must(t, cp.SetShareState(ctx, sh.ID, entity.ShareEnabled, entity.ShareQuiesced))
	if err := cp.SetShareState(ctx, sh.ID, entity.ShareEnabled, entity.ShareQuiesced); !merrs.IsConflictError(err) {
		t.Errorf("stale from: %v", err)
	}
	if w := counters(t, s).ShareList; w != v+1 {
		t.Errorf("ShareList %d, want %d", w, v+1)
	}
	if got := all(t, cp.Shares(ctx)); got[0].State != entity.ShareQuiesced {
		t.Errorf("state %v", got[0].State)
	}
	if err := cp.SetShareState(ctx, entity.NewShareID(), entity.ShareEnabled, entity.ShareQuiesced); !merrs.IsNotFoundError(err) {
		t.Errorf("unknown share: %v", err)
	}
}

// A grant write takes its version as expect, and every grant set or deleted
// on a share moves the share's grant-list version, read with the share and
// its policy in one snapshot.
func testGrantVersions(t *testing.T, kvo Opener) {
	s, c := newStore(t, kvo)
	cp, p, ix := s.ControlPlane(), s.Principals(), s.ShareIndex()
	u, g := user("alice"), group("staff")
	sh := share("photos", "/photos")
	must(t, errOf(cp.PutUser(ctx, u, 0, entity.Alloc{})))
	must(t, errOf(cp.PutGroup(ctx, g, 0, entity.Alloc{})))
	must(t, errOf(CreateShare(ctx, cp, sh)))
	must(t, cp.AddMember(ctx, entity.Membership{Group: g.Principal, Member: u.Principal}))
	listVersion := func() uint64 {
		t.Helper()
		got, _, err := ix.Share(ctx, sh.ID)
		must(t, err)
		return got.GrantVersion
	}
	moved := func(what string, write func() error) {
		t.Helper()
		before := listVersion()
		must(t, write())
		if after := listVersion(); after <= before {
			t.Errorf("grant-list version after %s: %d, want above %d", what, after, before)
		}
	}
	moved("a group's grant", func() error {
		return errOf(cp.SetGrant(ctx, entity.ShareGrant{Share: sh.ID, Principal: g.Principal, Access: entity.AccessReadWrite}, 0))
	})
	moved("a user's grant", func() error {
		return errOf(cp.SetGrant(ctx, entity.ShareGrant{Share: sh.ID, Principal: u.Principal, Access: entity.AccessRead}, 0))
	})
	if a, ok, err := p.Grant(ctx, sh.ID, u.Principal); err != nil || !ok || a != entity.AccessRead {
		t.Errorf("own grant = %v, %v, %v; want read, true", a, ok, err)
	}
	// Setting none is a grant of its own that blocks; only deleting it lets
	// the group's decide again.
	moved("a grant changed to none", func() error {
		return errOf(cp.SetGrant(ctx, entity.ShareGrant{Share: sh.ID, Principal: u.Principal, Access: entity.AccessNone}, grantVersion(t, cp, sh.ID, u.Principal)))
	})
	if a, ok, err := p.Grant(ctx, sh.ID, u.Principal); err != nil || !ok || a != entity.AccessNone {
		t.Errorf("own grant of none = %v, %v, %v; want none, true", a, ok, err)
	}
	if a := effective(t, s, sh.ID, u.Principal); a != entity.AccessNone {
		t.Errorf("access under an own grant of none in a read-write group = %v, want none", a)
	}
	moved("a grant's delete", func() error {
		return cp.DeleteGrant(ctx, sh.ID, u.Principal, grantVersion(t, cp, sh.ID, u.Principal))
	})
	if _, ok, err := cp.ShareGrant(ctx, sh.ID, u.Principal); err != nil || ok {
		t.Errorf("a deleted grant: found %v, %v; want none", ok, err)
	}
	if a, ok, err := p.Grant(ctx, sh.ID, u.Principal); err != nil || ok {
		t.Errorf("Grant after the own grant's delete = %v, %v, %v; want none, false", a, ok, err)
	}
	if a := effective(t, s, sh.ID, u.Principal); a != entity.AccessReadWrite {
		t.Errorf("a group member after its own grant's delete: %v, want the group's %v", a, entity.AccessReadWrite)
	}
	// The grant's reverse row goes with it.
	must(t, c.View(ctx, func(r kv.Reader) error {
		for kvp, err := range r.Scan(keys.ID([]byte{keys.KindGR}, u.Principal.ID[:]), nil) {
			if err != nil {
				return err
			}
			t.Errorf("reverse row left after DeleteGrant: %x", kvp.Key)
		}
		return nil
	}))
	// Deleting a grant that is not there changes nothing.
	before := listVersion()
	must(t, cp.DeleteGrant(ctx, sh.ID, u.Principal, 0))
	if after := listVersion(); after != before {
		t.Errorf("grant-list version after deleting no grant: %d, want %d", after, before)
	}
	if _, err := cp.SetGrant(ctx, entity.ShareGrant{Share: entity.NewShareID(), Principal: u.Principal, Access: entity.AccessRead}, 0); !merrs.IsNotFoundError(err) {
		t.Errorf("grant on a missing share: %v", err)
	}
	// Deleting a principal deletes its grants, so it moves the version too.
	must(t, errOf(cp.SetGrant(ctx, entity.ShareGrant{Share: sh.ID, Principal: u.Principal, Access: entity.AccessRead}, 0)))
	moved("a granted user's delete", func() error {
		return deleted(s, cp.DeleteUser(ctx, u.Principal, userGeneration(t, s, u.Principal)), u.Principal)
	})
	moved("a granted group's delete", func() error {
		return deleted(s, cp.DeleteGroup(ctx, g.Principal, groupGeneration(t, cp, g.Principal)), g.Principal)
	})
	if _, err := cp.SetGrant(ctx, entity.ShareGrant{Share: sh.ID, Principal: entity.NewUser(), Access: entity.AccessRead}, 0); !merrs.IsNotFoundError(err) {
		t.Errorf("grant to a missing user: %v", err)
	}
}

// A share's policy and its grant-list version are read from one snapshot: a
// read across the commits of a policy write and a grant write finds both or
// neither.
func testShareReadsPolicyAndGrantsTogether(t *testing.T, kvo Opener) {
	for _, b := range kvo.backends() {
		t.Run(b.name, func(t *testing.T) {
			s, h := heldStore(t, b)
			cp, ix := s.ControlPlane(), s.ShareIndex()
			sh, u := share("photos", "/photos"), user("alice")
			must(t, errOf(CreateShare(ctx, cp, sh)))
			must(t, errOf(cp.PutUser(ctx, u, 0, entity.Alloc{})))
			must(t, errOf(cp.PutExportPolicy(ctx, entity.ExportPolicy{Share: sh.ID, Squash: entity.SquashAll}, 0)))
			must(t, errOf(cp.SetGrant(ctx, entity.ShareGrant{Share: sh.ID, Principal: u.Principal, Access: entity.AccessRead}, 0)))
			before, beforePol, err := ix.Share(ctx, sh.ID)
			must(t, err)

			var writeErr error
			h.mu.Lock()
			h.viewHook = func() {
				writeErr = errors.Join(
					errOf(cp.PutExportPolicy(ctx, entity.ExportPolicy{Share: sh.ID, Squash: entity.SquashRoot}, beforePol.Version)),
					errOf(cp.SetGrant(ctx, entity.ShareGrant{Share: sh.ID, Principal: u.Principal, Access: entity.AccessReadWrite}, grantVersion(t, cp, sh.ID, u.Principal))))
			}
			h.mu.Unlock()
			got, pol, err := ix.Share(ctx, sh.ID)
			must(t, err)
			must(t, writeErr)
			grantMoved, policyMoved := got.GrantVersion != before.GrantVersion, pol.Version != beforePol.Version
			if grantMoved != policyMoved {
				t.Errorf("share read across a policy and a grant write: grant-list version moved %v, policy version moved %v; want both or neither", grantMoved, policyMoved)
			}
			after, afterPol, err := ix.Share(ctx, sh.ID)
			must(t, err)
			if after.GrantVersion == before.GrantVersion || afterPol.Squash != entity.SquashRoot {
				t.Errorf("share read after both writes: grant-list version %d, squash %v; want above %d, %v", after.GrantVersion, afterPol.Squash, before.GrantVersion, entity.SquashRoot)
			}
		})
	}
}

// The share's identity policy comes from one ShareIndex.Share read through
// ExportPolicy.SharePolicy: the stored squash, the anonymous principal a
// policy naming none reads back as, the policy's version and the grant-list
// version the share's last grant write set.
func testSharePolicyFromOneRead(t *testing.T, kvo Opener) {
	s, _ := newStore(t, kvo)
	cp, ix := s.ControlPlane(), s.ShareIndex()
	sh, u := share("photos", "/photos"), user("alice")
	must(t, errOf(CreateShare(ctx, cp, sh)))
	must(t, errOf(cp.PutUser(ctx, u, 0, entity.Alloc{})))
	polVersion, err := cp.PutExportPolicy(ctx, entity.ExportPolicy{Share: sh.ID, Squash: entity.SquashRoot}, 0)
	must(t, err)
	v, err := cp.SetGrant(ctx, entity.ShareGrant{Share: sh.ID, Principal: u.Principal, Access: entity.AccessRead}, 0)
	must(t, err)

	got, pol, err := ix.Share(ctx, sh.ID)
	must(t, err)
	sp := pol.SharePolicy(got.GrantVersion)
	if sp.Share != sh.ID {
		t.Errorf("SharePolicy.Share = %v, want %v", sp.Share, sh.ID)
	}
	if sp.Squash != entity.SquashRoot {
		t.Errorf("SharePolicy.Squash = %v, want %v", sp.Squash, entity.SquashRoot)
	}
	if sp.Anonymous == (entity.Principal{}) {
		t.Errorf("SharePolicy.Anonymous is zero, want the anonymous principal")
	}
	if sp.PolicyVersion != polVersion {
		t.Errorf("SharePolicy.PolicyVersion = %d, want %d", sp.PolicyVersion, polVersion)
	}
	if sp.GrantVersion != v {
		t.Errorf("SharePolicy.GrantVersion = %d, want the grant write's %d", sp.GrantVersion, v)
	}
}

// RFC 16 §4.2's quota records (qt, qx) and RFC 17 §5.6: limits on a user,
// a group or a project; when usage first went above the soft limit, kept
// until it falls back, restarted by a new soft limit; a principal's
// records go with it.
func testQuotas(t *testing.T, kvo Opener) {
	s, _ := newStore(t, kvo)
	cp, c := s.ControlPlane(), s.Capacity()
	u, g := user("alice"), group("staff")
	sh := share("photos", "/photos")
	must(t, errOf(cp.PutUser(ctx, u, 0, entity.Alloc{})))
	must(t, errOf(cp.PutGroup(ctx, g, 0, entity.Alloc{})))
	must(t, errOf(CreateShare(ctx, cp, sh)))
	uq := entity.Quota{Share: sh.ID, Principal: u.Principal, Hard: entity.Limit{Bytes: 10 << 30}, Soft: entity.Limit{Bytes: 8 << 30},
		Grace: 7 * 24 * time.Hour, Advisory: entity.Limit{Files: 5000}}
	pq := entity.Quota{Share: sh.ID, Project: 7, Hard: entity.Limit{Files: 1000}}
	gq := entity.Quota{Share: sh.ID, Principal: g.Principal, Soft: entity.Limit{Bytes: 1 << 30}, Grace: time.Hour}
	for _, q := range []entity.Quota{uq, pq, gq} {
		must(t, errOf(cp.SetQuota(ctx, sh.ID, q, 0)))
	}
	var got []entity.Quota
	for q, err := range cp.Quotas(ctx, sh.ID) {
		must(t, err)
		got = append(got, q)
	}
	if len(got) != 3 || got[2].Project != pq.Project || got[2].Hard != pq.Hard || got[2].Version == 0 {
		t.Fatalf("quotas %+v", got)
	}

	ut := kvstore.QuotaTarget{Principal: u.Principal}
	soft := func(above bool) time.Time {
		t.Helper()
		since, err := c.SoftExceeded(ctx, sh.ID, ut, above)
		must(t, err)
		return since
	}
	sinceNow := func() time.Time {
		t.Helper()
		q, since, err := c.Quota(ctx, sh.ID, ut)
		must(t, err)
		if q.Hard != uq.Hard {
			t.Errorf("quota %+v", q)
		}
		return since
	}
	if !sinceNow().IsZero() {
		t.Error("above the soft limit before anything said so")
	}
	first := soft(true)
	if first.IsZero() || !soft(true).Equal(first) || !sinceNow().Equal(first) {
		t.Errorf("the first crossing at %v is not kept", first)
	}
	uq.Hard.Bytes = 20 << 30 // another limit keeps the grace running
	must(t, errOf(cp.SetQuota(ctx, sh.ID, uq, quotaVersion(t, c, sh.ID, ut))))
	if !sinceNow().Equal(first) {
		t.Error("a new hard limit restarted the grace")
	}
	uq.Soft.Bytes = 9 << 30 // a new soft limit restarts it
	must(t, errOf(cp.SetQuota(ctx, sh.ID, uq, quotaVersion(t, c, sh.ID, ut))))
	if !sinceNow().IsZero() {
		t.Error("a new soft limit kept the old crossing")
	}
	soft(true)
	if !soft(false).IsZero() || !sinceNow().IsZero() {
		t.Error("falling back under did not clear the crossing")
	}

	// A quota that limits nothing is deleted.
	must(t, errOf(cp.SetQuota(ctx, sh.ID, entity.Quota{Project: 7}, quotaVersion(t, c, sh.ID, kvstore.QuotaTarget{Project: 7}))))
	if q, _, err := c.Quota(ctx, sh.ID, kvstore.QuotaTarget{Project: 7}); err != nil || !q.Unlimited() {
		t.Errorf("a deleted quota reads %+v, %v", q, err)
	}

	// A deleted principal's quotas go with it.
	gt := kvstore.QuotaTarget{Principal: g.Principal}
	if _, err := c.SoftExceeded(ctx, sh.ID, gt, true); err != nil {
		t.Fatal(err)
	}
	must(t, deleted(s, cp.DeleteGroup(ctx, g.Principal, groupGeneration(t, cp, g.Principal)), g.Principal))
	if q, since, err := c.Quota(ctx, sh.ID, gt); err != nil || !q.Unlimited() || !since.IsZero() {
		t.Errorf("a deleted group's quota reads %+v since %v, %v", q, since, err)
	}

	// The share's own quota names neither a principal nor a project.
	must(t, errOf(cp.SetQuota(ctx, sh.ID, entity.Quota{Hard: entity.Limit{Bytes: 1 << 40}}, 0)))
	if q, _, err := c.Quota(ctx, sh.ID, kvstore.QuotaTarget{}); err != nil || q.Hard.Bytes != 1<<40 {
		t.Errorf("the share-wide quota reads %+v, %v", q, err)
	}
	got = got[:0]
	for q, err := range cp.Quotas(ctx, sh.ID) {
		must(t, err)
		got = append(got, q)
	}
	if len(got) != 2 || got[len(got)-1].Principal != (entity.Principal{}) || got[len(got)-1].Project != 0 {
		t.Errorf("quotas with the share's own last: %+v", got)
	}

	invalid := func(err error) bool { return isCode(err, merrs.ErrInvalidArgument) }
	for name, c := range map[string]struct {
		q    entity.Quota
		want func(error) bool
	}{
		"missing user":          {entity.Quota{Share: sh.ID, Principal: entity.NewUser(), Hard: entity.Limit{Files: 1}}, merrs.IsNotFoundError},
		"missing share":         {entity.Quota{Share: entity.NewShareID(), Project: 1, Hard: entity.Limit{Files: 1}}, merrs.IsNotFoundError},
		"principal and project": {entity.Quota{Share: sh.ID, Principal: u.Principal, Project: 1, Hard: entity.Limit{Files: 1}}, invalid},
		"negative":              {entity.Quota{Share: sh.ID, Project: 1, Hard: entity.Limit{Bytes: -1}}, invalid},
	} {
		if _, err := cp.SetQuota(ctx, c.q.Share, c.q, 0); !c.want(err) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// RFC 16 §4.2's qx rule: a changed soft limit restarts its grace at the
// change's time when usage is above the new limit, and clears it otherwise.
func testQuotaSoftChange(t *testing.T, kvo Opener) {
	f := newFS(t, kvo, entity.FoldIdentity)
	cp, c := f.s.ControlPlane(), f.s.Capacity()
	alice := f.member("alice", entity.Principal{})
	pub, _ := f.create(f.admin, f.root, "pub", directory(0o777))
	for _, name := range []string{"a", "b", "c"} {
		f.create(alice, pub, name, regular(0o644))
	}
	at := kvstore.QuotaTarget{Principal: alice.Principal}
	set := func(files int64) time.Time {
		t.Helper()
		must(t, errOf(cp.SetQuota(ctx, f.share.ID, entity.Quota{Principal: alice.Principal, Soft: entity.Limit{Files: files}, Grace: time.Hour}, quotaVersion(t, c, f.share.ID, at))))
		_, since, err := c.Quota(ctx, f.share.ID, at)
		must(t, err)
		return since
	}
	if !set(5).IsZero() {
		t.Error("3 files under a soft limit of 5 recorded a crossing")
	}
	if set(2).IsZero() {
		t.Error("3 files over a new soft limit of 2 recorded no crossing")
	}
	if !set(10).IsZero() {
		t.Error("a soft limit raised above usage kept the crossing")
	}
}

func testExportPolicy(t *testing.T, kvo Opener) {
	s, _ := newStore(t, kvo)
	cp := s.ControlPlane()
	sh := share("photos", "/photos")
	must(t, errOf(CreateShare(ctx, cp, sh)))
	pol := entity.ExportPolicy{
		Share:   sh.ID,
		Flavors: []entity.AuthFlavor{entity.FlavorSys},
		Clients: []entity.ClientRule{{Match: entity.ClientMatch{CIDR: netip.MustParsePrefix("10.0.0.0/8")}, Access: entity.AccessRead}},
		Squash:  entity.SquashRoot,
	}
	must(t, errOf(cp.PutExportPolicy(ctx, pol, 0)))
	got, err := cp.ExportPolicy(ctx, sh.ID)
	must(t, err)
	must(t, errOf(cp.PutExportPolicy(ctx, pol, got.Version)))
	again, _ := cp.ExportPolicy(ctx, sh.ID)
	if got.Version == 0 || again.Version <= got.Version {
		t.Errorf("versions %d then %d", got.Version, again.Version)
	}
	got.Version = 0
	if !slices.Equal(got.Clients, pol.Clients) || got.Squash != pol.Squash {
		t.Errorf("policy %+v", got)
	}
	pol.Clients[0].Access = entity.AccessAdmin
	if _, err := cp.PutExportPolicy(ctx, pol, 0); err == nil {
		t.Error("a client rule granted admin")
	}
	pol.Clients[0].Access = entity.AccessRead

	// A protection floor with a proof below a verified one is refused: the
	// unverified caller it admits carries no protection at all.
	for _, proof := range []entity.ProofKind{entity.ProofNone, entity.ProofClaimed} {
		pol.MinProof, pol.MinProtection = proof, entity.ProtectionIntegrity
		if _, err := cp.PutExportPolicy(ctx, pol, 0); err == nil {
			t.Errorf("a policy admitting proof %v with an integrity floor was saved", proof)
		}
	}
	// A verified proof takes the floor: NTLM with sealing, and Kerberos with
	// the integrity floor refused above.
	for proof, floor := range map[entity.ProofKind]entity.Protection{
		entity.ProofSecret: entity.ProtectionPrivacy, entity.ProofTicket: entity.ProtectionIntegrity,
	} {
		pol.MinProof, pol.MinProtection, pol.ReadOnly = proof, floor, true
		must(t, errOf(cp.PutExportPolicy(ctx, pol, policyVersion(t, cp, sh.ID))))
		if got, err := cp.ExportPolicy(ctx, sh.ID); err != nil || got.MinProof != proof ||
			got.MinProtection != floor || !got.ReadOnly {
			t.Errorf("the minimums and read-only read back as %+v, %v", got, err)
		}
	}
}

func testNetgroupsAndSettings(t *testing.T, kvo Opener) {
	s, _ := newStore(t, kvo)
	cp := s.ControlPlane()
	must(t, errOf(cp.PutNetgroup(ctx, entity.Netgroup{Name: "lab", Members: []string{"10.0.0.0/8"}}, 0)))
	must(t, errOf(cp.PutNetgroup(ctx, entity.Netgroup{Name: "office"}, 0)))
	if got := all(t, cp.Netgroups(ctx, "lab")); len(got) != 1 || got[0].Name != "office" {
		t.Errorf("Netgroups after lab = %+v", got)
	}
	must(t, cp.DeleteNetgroup(ctx, "lab", netgroupVersion(t, s, "lab")))
	if got := all(t, cp.Netgroups(ctx, "")); len(got) != 1 {
		t.Errorf("Netgroups = %+v", got)
	}

	inst := entity.SettingScope{Kind: entity.ScopeInstallation}
	shareScope := entity.SettingScope{Kind: entity.ScopeShare, ID: entity.NewShareID()}
	must(t, errOf(cp.PutSetting(ctx, entity.Setting{Scope: inst, Key: "a", Value: []byte("1")}, 0)))
	must(t, errOf(cp.PutSetting(ctx, entity.Setting{Scope: inst, Key: "a", Value: []byte("2")}, 1)))
	must(t, errOf(cp.PutSetting(ctx, entity.Setting{Scope: shareScope, Key: "a", Value: []byte("3")}, 0)))
	got := all(t, cp.Settings(ctx, inst))
	if len(got) != 1 || string(got[0].Value) != "2" || got[0].Generation != 2 {
		t.Errorf("installation settings = %+v", got)
	}
	if gen := counters(t, s).Settings; gen != 3 {
		t.Errorf("settings generation = %d", gen)
	}
	if _, err := cp.PutSetting(ctx, entity.Setting{Scope: entity.SettingScope{Kind: entity.ScopeShare}, Key: "a"}, 0); err == nil {
		t.Error("share scope with no ID accepted")
	}
}

// A listing longer than one page resumes where the page ended.
func testListingPages(t *testing.T, kvo Opener) {
	s, _ := newStore(t, kvo)
	cp := s.ControlPlane()
	const n = 600
	for i := range n {
		must(t, errOf(cp.PutGroup(ctx, group(string(rune('a'+i/26/26%26))+string(rune('a'+i/26%26))+string(rune('a'+i%26))), 0, entity.Alloc{})))
	}
	got := all(t, cp.Groups(ctx, ""))
	if len(got) != n || !slices.IsSortedFunc(got, func(a, b entity.Group) int { return strings.Compare(a.Name, b.Name) }) {
		t.Fatalf("%d groups, sorted %v", len(got), slices.IsSortedFunc(got, func(a, b entity.Group) int { return strings.Compare(a.Name, b.Name) }))
	}
}

// A share's fold rule, name rule and digest key are fixed at create.
func testShareBoundSettings(t *testing.T, kvo Opener) {
	s, _ := newStore(t, kvo)
	cp := s.ControlPlane()
	ci := share("docs", "/docs")
	ci.Fold = entity.FoldUpcase
	must(t, errOf(CreateShare(ctx, cp, ci)))
	must(t, errOf(CreateShare(ctx, cp, share("src", "/src"))))
	got := all(t, cp.Shares(ctx))
	if got[0].Fold != entity.FoldUpcase || got[0].Names != entity.NamesWindows || got[0].DigestKey == ([32]byte{}) {
		t.Errorf("case-insensitive share: %v %v", got[0].Fold, got[0].Names)
	}
	if got[1].Fold != entity.FoldIdentity || got[1].Names != entity.NamesPOSIX || got[1].DigestKey == got[0].DigestKey {
		t.Errorf("default share: %v %v", got[1].Fold, got[1].Names)
	}
	// A rename that names none of them keeps them.
	ci.Name, ci.Fold = "papers", 0
	must(t, errOf(cp.PutShare(ctx, ci, 1)))
	if got := all(t, cp.Shares(ctx)); got[0].Name != "papers" || got[0].Fold != entity.FoldUpcase || got[0].DigestKey == ([32]byte{}) {
		t.Errorf("after rename: %+v", got[0])
	}
	for name, mut := range map[string]func(*entity.Share){
		"fold":  func(s *entity.Share) { s.Fold = entity.FoldIdentity },
		"names": func(s *entity.Share) { s.Names = entity.NamesPOSIX },
		"key":   func(s *entity.Share) { s.DigestKey[0] ^= 1 },
	} {
		changed := ci
		mut(&changed)
		if _, err := cp.PutShare(ctx, changed, 2); err == nil {
			t.Errorf("changing the %s was accepted", name)
		}
	}
}

// Every share gets a journal tag of its own, from 1: a put keeps it, and a
// share created after another's removal never gets the removed one's.
func testShareTags(t *testing.T, kvo Opener) {
	s, _ := newStore(t, kvo)
	cp := s.ControlPlane()
	a, b := share("a", "/a"), share("b", "/b")
	must(t, errOf(CreateShare(ctx, cp, a)))
	must(t, errOf(CreateShare(ctx, cp, b)))
	tags := func() map[entity.ShareID]uint32 {
		m := map[entity.ShareID]uint32{}
		for _, sh := range all(t, cp.Shares(ctx)) {
			m[sh.ID] = sh.Tag
		}
		return m
	}
	before := tags()
	if before[a.ID] == 0 || before[b.ID] == 0 || before[a.ID] == before[b.ID] {
		t.Fatalf("tags %v", before)
	}
	a.Name = "renamed"
	a.Tag = 77 // a put never writes the tag
	must(t, errOf(cp.PutShare(ctx, a, 1)))
	must(t, cp.DeleteShare(ctx, b.ID, entity.SnapshotsRefuse, 1))
	c := share("c", "/c")
	must(t, errOf(CreateShare(ctx, cp, c)))
	after := tags()
	if after[a.ID] != before[a.ID] {
		t.Errorf("a put changed the tag %d -> %d", before[a.ID], after[a.ID])
	}
	if after[c.ID] <= before[b.ID] {
		t.Errorf("new share's tag %d does not follow the removed one's %d", after[c.ID], before[b.ID])
	}
}

// A project is made on an empty directory by a privileged caller alone, a
// retry on its root answers it again, it does not nest, and the files made
// below it are charged to it, its root included. Making it folds the root's
// deltas, as any rewrite of a directory's record does, so its Get scans none.
func testProjects(t *testing.T, kvo Opener) {
	f := newFS(t, kvo, entity.FoldIdentity)
	files, c := f.s.Files(), f.s.Capacity()
	alice := f.member("alice", entity.Principal{})
	tree, _ := f.create(f.admin, f.root, "tree", directory(0o777))
	full, _ := f.create(f.admin, f.root, "full", directory(0o777))
	_, file := f.create(alice, full, "a", regular(0o644))
	f.create(alice, tree, "tmp", regular(0o644))
	_, err := f.s.Namespace().Unlink(ctx, alice, tree, []byte("tmp"))
	must(t, err)

	if _, err := files.SetProject(ctx, alice, tree); !isCode(err, merrs.ErrPrivilegeRequired) {
		t.Errorf("an unprivileged SetProject: %v", err)
	}
	if _, err := files.SetProject(ctx, f.admin, full); !isCode(err, merrs.ErrNotEmpty) {
		t.Errorf("SetProject on a directory holding a file: %v", err)
	}
	if _, err := files.SetProject(ctx, f.admin, entity.NewHandle(f.share.ID, file.ID)); !invalid(err) {
		t.Errorf("SetProject on a regular file: %v", err)
	}
	p, err := files.SetProject(ctx, f.admin, tree)
	must(t, err)
	f.c.Take()
	_, err = files.Get(ctx, f.admin, tree)
	must(t, err)
	if n := f.c.Take(); n.Scanned != 0 {
		t.Errorf("a new project's root's Get scanned %d deltas", n.Scanned)
	}
	if again, err := files.SetProject(ctx, f.admin, tree); err != nil || again != p || p == 0 {
		t.Errorf("SetProject on its root again: %d, %v; want %d", again, err, p)
	}
	sub, _ := f.create(alice, tree, "sub", directory(0o777))
	_, made := f.create(alice, sub, "b", regular(0o644))
	if made.Project != p {
		t.Errorf("a file below the root is in project %d, want %d", made.Project, p)
	}
	if _, err := files.SetProject(ctx, f.admin, sub); !invalid(err) {
		t.Errorf("a project nested in another: %v", err)
	}
	if u, err := c.ProjectUsage(ctx, f.share.ID, p); err != nil || u.Files != 3 {
		t.Errorf("the project's usage: %+v, %v; want its root, sub and b", u, err)
	}
	if u, err := c.ProjectUsage(ctx, f.share.ID, p+1); err != nil || u != (entity.Usage{}) {
		t.Errorf("another project's usage: %+v, %v", u, err)
	}
	// The root renames in place and moves to a directory in no project; a
	// file below it, and the root, cannot move into another project.
	ns := f.s.Namespace()
	out, _ := f.create(f.admin, f.root, "out", directory(0o777))
	other, _ := f.create(f.admin, f.root, "other", directory(0o777))
	_, err = files.SetProject(ctx, f.admin, other)
	must(t, err)
	if _, err := ns.Rename(ctx, f.admin, f.root, []byte("tree"), f.root, []byte("renamed")); err != nil {
		t.Errorf("a project's root renamed in place: %v", err)
	}
	if _, err := ns.Rename(ctx, f.admin, f.root, []byte("renamed"), out, []byte("tree")); err != nil {
		t.Errorf("a project's root moved outside every project: %v", err)
	}
	if _, err := ns.Rename(ctx, f.admin, out, []byte("tree"), other, []byte("tree")); !errors.Is(err, entity.ErrCrossProject) {
		t.Errorf("a project's root moved into another project: %v", err)
	}
	if _, err := ns.Rename(ctx, f.admin, sub, []byte("b"), out, []byte("b")); !errors.Is(err, entity.ErrCrossProject) {
		t.Errorf("a file moved out of its project: %v", err)
	}
}

// A krb spelling's linked SID is kept in the user's record: put, changed
// and cleared on the same spelling, the spelling's row still resolves and
// the record reads back what was put. No lookup returns the row's own copy
// of the SID, so this cannot see it; the package's TestKrbRowHoldsLinked
// reads the row. On a spelling of any other scheme, or a SID that is not a
// domain account's, the put is refused.
func testKrbLinkedSID(t *testing.T, kvo Opener) {
	s, _ := newStore(t, kvo)
	cp, p := s.ControlPlane(), s.Principals()
	krb := func(linked string) entity.ProtocolID { return krbAs("alice@AD.EXAMPLE", linked) }
	u := user("alice", uid("1000"))
	own := u.IDs[1] // the SID user gave alice to go with her uid
	for i, linked := range []string{"S-1-5-21-1-2-3-1104", "S-1-5-21-1-2-3-1105", ""} {
		u.IDs = entity.ProtocolIDs{uid("1000"), own, krb(linked)}
		must(t, errOf(cp.PutUser(ctx, u, userGeneration(t, s, u.Principal), entity.Alloc{})))
		if got, err := p.ByProtocolID(ctx, krb("")); err != nil || got != u.Principal {
			t.Errorf("put %d, linked %q: ByProtocolID(krb) = %v, %v; want %v", i, linked, got, err, u.Principal)
		}
		got, err := p.User(ctx, u.Principal)
		must(t, err)
		if want := (entity.ProtocolIDs{uid("1000"), own, krb(linked)}); !slices.Equal(got.IDs, want) {
			t.Errorf("put %d: record's IDs = %q, want %q", i, got.IDs, want)
		}
	}

	// Each user is otherwise valid and holds spellings no user holds, as its
	// put with every link cleared shows, so each refusal is the linked SID's.
	domain := krbAs("", "S-1-5-21-1-2-3-1104").Linked
	cut := krbAs("bob@AD.EXAMPLE", "S-1-5-21-1-2-3-1104")
	cut.Linked = cut.Linked[:len(cut.Linked)-1] // a sub-authority cut short
	linkedUID := uid("1001")
	linkedUID.Linked = domain
	linkedSID := freshSID()
	linkedSID.Linked = domain
	for _, c := range []struct {
		name string
		ids  entity.ProtocolIDs
	}{
		{"a uid spelling linked for a SID", entity.ProtocolIDs{linkedUID, freshSID()}},
		{"a sid spelling linked for a SID", entity.ProtocolIDs{uid("1002"), linkedSID}},
		{"a krb spelling linked for a malformed SID", entity.ProtocolIDs{cut}},
		{"a krb spelling linked for Everyone", entity.ProtocolIDs{krbAs("carol@AD.EXAMPLE", "S-1-1-0")}},
		{"a krb spelling linked for a builtin group", entity.ProtocolIDs{krbAs("dave@AD.EXAMPLE", "S-1-5-32-544")}},
	} {
		v := user("bob-"+strings.ReplaceAll(c.name, " ", "-"), c.ids...)
		if _, err := cp.PutUser(ctx, v, 0, entity.Alloc{}); !isCode(err, merrs.ErrInvalidArgument) {
			t.Errorf("%s: PutUser = %v, want %v", c.name, err, merrs.ErrInvalidArgument)
		}
		v.IDs = slices.Clone(c.ids)
		for i := range v.IDs {
			v.IDs[i].Linked = ""
		}
		if _, err := cp.PutUser(ctx, v, 0, entity.Alloc{}); err != nil {
			t.Errorf("%s, unlinked: PutUser = %v, want it put", c.name, err)
		}
	}
}

// Holder answers the user or machine whose record holds a spelling, its
// linked SID with it, both from the view and inside a write; a group's
// spelling, one nobody holds and a marked machine's answer none.
func testSpellingHolder(t *testing.T, kvo Opener) {
	s, _ := newStore(t, kvo)
	cp := s.ControlPlane()
	alice := user("alice", uid("1000"), krbAs("alice@AD.EXAMPLE", "S-1-5-21-1-2-3-1104"))
	must(t, errOf(cp.PutUser(ctx, alice, 0, entity.Alloc{})))
	m := entity.User{Principal: entity.Principal{Kind: entity.PrincipalMachine, ID: entity.NewPrincipalID()}, IDs: entity.ProtocolIDs{krbAs("host/build01@AD.EXAMPLE", "S-1-5-21-1-2-3-1199")}}
	must(t, errOf(cp.PutUser(ctx, m, 0, entity.Alloc{})))
	g := group("staff")
	must(t, errOf(cp.PutGroup(ctx, g, 0, entity.Alloc{})))
	inTx := func(id entity.ProtocolID) (u entity.User, ok bool) {
		must(t, cp.Write(ctx, entity.WriteMeta{}, func(tx entity.ControlPlaneTx) (err error) {
			u, ok, err = tx.Holder(id)
			return err
		}))
		return u, ok
	}
	for _, c := range []struct {
		name string
		id   entity.ProtocolID
		want entity.User
		ok   bool
	}{
		{"a user's krb", krbAs("alice@AD.EXAMPLE", ""), alice, true},
		{"a user's uid", uid("1000"), alice, true},
		{"a machine's krb", krbAs("host/build01@AD.EXAMPLE", ""), m, true},
		{"a group's gid", g.IDs[0], entity.User{}, false},
		{"nobody's", krbAs("carol@AD.EXAMPLE", ""), entity.User{}, false},
	} {
		got, ok, err := cp.Holder(ctx, c.id)
		must(t, err)
		txGot, txOK := inTx(c.id)
		for _, side := range []struct {
			how string
			u   entity.User
			ok  bool
		}{{"view", got, ok}, {"write", txGot, txOK}} {
			if side.ok != c.ok || side.u.Principal != c.want.Principal || c.ok && !slices.Equal(side.u.IDs[len(side.u.IDs)-1:], c.want.IDs[len(c.want.IDs)-1:]) {
				t.Errorf("%s, from the %s: Holder = %v %v %v, want %v %v holding %v", c.name, side.how, side.u.Principal, side.u.IDs, side.ok, c.want.Principal, c.ok, c.want.IDs)
			}
		}
	}
	mGen, _, err := cp.User(ctx, m.Principal)
	must(t, err)
	must(t, cp.DeleteUser(ctx, m.Principal, mGen.Generation))
	if _, ok, err := cp.Holder(ctx, krbAs("host/build01@AD.EXAMPLE", "")); err != nil || ok {
		t.Errorf("a marked machine's krb: Holder = %v %v, want none", ok, err)
	}
}

// krbAs is the krb spelling name, linked for the binary form of the SID
// linked spells, or for none when it is empty.
func krbAs(name, linked string) entity.ProtocolID {
	id := entity.ProtocolID{Scheme: entity.SchemeKerberos, Value: name}
	if linked != "" {
		var b bytes.Buffer
		sid.EncodeSID(&b, sid.ParseSIDMust(linked))
		id.Linked = b.String()
	}
	return id
}

// A user's management API fields read back as written, its not-before in
// whole seconds.
func testUserAPIFields(t *testing.T, kvo Opener) {
	s, _ := newStore(t, kvo)
	u := user("alice")
	u.Email, u.Role, u.MustChange = "alice@example.com", entity.RoleOperator, true
	u.NotBefore = time.Unix(1_800_000_000, 999_999_999)
	must(t, errOf(s.ControlPlane().PutUser(ctx, u, 0, entity.Alloc{})))
	got, err := s.Principals().User(ctx, u.Principal)
	must(t, err)
	if got.Email != u.Email || got.Role != u.Role || !got.MustChange {
		t.Errorf("email, role, must change: got %q %v %v, want %q %v true", got.Email, got.Role, got.MustChange, u.Email, u.Role)
	}
	if want := time.Unix(1_800_000_000, 0); !got.NotBefore.Equal(want) {
		t.Errorf("not before: got %v, want %v", got.NotBefore, want)
	}
}
