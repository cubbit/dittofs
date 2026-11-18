package kvstoretest

import (
	"fmt"
	"slices"
	"strconv"
	"testing"

	"github.com/cubbit/dittofs/pkg/metadata/entity"
	merrs "github.com/cubbit/dittofs/pkg/metadata/errors"
	"github.com/cubbit/dittofs/pkg/metadata/kvstore"
)

// mintUser and mintGroup are batch records drawing every ID from fileRange.
func mintUser(name string) entity.MintUser {
	return entity.MintUser{User: user(name), Alloc: entity.Alloc{UID: true, RID: true, Range: fileRange}}
}

func mintGroup(name string) entity.MintGroup {
	g := group(name)
	g.IDs = nil
	return entity.MintGroup{Group: g, Alloc: entity.Alloc{GID: true, RID: true, Range: fileRange}}
}

func groupOf(t *testing.T, s *kvstore.Store, p entity.Principal) (entity.Group, bool) {
	t.Helper()
	for g, err := range s.ControlPlane().Groups(ctx, "") {
		must(t, err)
		if g.Principal == p {
			return g, true
		}
	}
	return entity.Group{}, false
}

// A batch draws each record's IDs inside its transaction: two users and two
// groups get distinct uids and gids in the range, and four distinct SIDs.
func testMintAllocates(t *testing.T, kvo Opener) {
	s, _ := newStore(t, kvo)
	b := entity.MintBatch{
		Users:  []entity.MintUser{mintUser("alice"), mintUser("bob")},
		Groups: []entity.MintGroup{mintGroup("staff"), mintGroup("dev")},
	}
	must(t, s.Minter().Create(ctx, b))
	var uids, gids, sids []string
	for _, r := range b.Users {
		u, err := s.Principals().User(ctx, r.User.Principal)
		must(t, err)
		uids, sids = append(uids, spelling(u.IDs, entity.SchemeUID)), append(sids, spelling(u.IDs, entity.SchemeSID))
	}
	for _, r := range b.Groups {
		g, ok := groupOf(t, s, r.Group.Principal)
		if !ok {
			t.Fatalf("minted group %s not found", r.Group.Name)
		}
		gids, sids = append(gids, spelling(g.IDs, entity.SchemeGID)), append(sids, spelling(g.IDs, entity.SchemeSID))
	}
	inRange := func(ids []string) bool {
		for _, v := range ids {
			n, err := strconv.ParseUint(v, 10, 32)
			if err != nil || uint32(n) < fileRange.Lo || uint32(n) > fileRange.Hi {
				return false
			}
		}
		return len(ids) == 2 && ids[0] != ids[1]
	}
	if !inRange(uids) || !inRange(gids) {
		t.Errorf("minted uids %v and gids %v; want two distinct of each in %v", uids, gids, fileRange)
	}
	slices.Sort(sids)
	if len(slices.Compact(sids)) != 4 || sids[0] == "" {
		t.Errorf("minted SIDs %x; want four distinct", sids)
	}
}

// A batch one of whose rows another principal holds fails whole: none of
// its records, name or ID rows, counters or the principals generation is
// written.
func testMintAllOrNothing(t *testing.T, kvo Opener) {
	s, _ := newStore(t, kvo)
	held := group("held")
	must(t, errOf(s.ControlPlane().PutGroup(ctx, held, 0, entity.Alloc{})))
	before := counters(t, s).Principals

	last := group("last")
	last.IDs[1] = held.IDs[1]
	b := entity.MintBatch{
		Users:  []entity.MintUser{mintUser("alice")},
		Groups: []entity.MintGroup{mintGroup("staff"), {Group: last}},
	}
	if err := s.Minter().Create(ctx, b); !merrs.IsAlreadyExistsError(err) {
		t.Fatalf("a batch whose last group holds another's SID: %v, want already exists", err)
	}
	if _, err := s.Principals().User(ctx, b.Users[0].User.Principal); !merrs.IsNotFoundError(err) {
		t.Errorf("the refused batch's user: %v, want not found", err)
	}
	for _, r := range b.Groups {
		if _, ok := groupOf(t, s, r.Group.Principal); ok {
			t.Errorf("the refused batch's group %s is listed", r.Group.Name)
		}
	}
	for _, n := range []struct {
		kind entity.PrincipalKind
		name string
	}{{entity.PrincipalUser, "alice"}, {entity.PrincipalGroup, "staff"}, {entity.PrincipalGroup, "last"}} {
		if got, err := s.Principals().ByName(ctx, n.kind, n.name); !merrs.IsNotFoundError(err) {
			t.Errorf("ByName(%v, %s) after the refused batch = %v, %v; want not found", n.kind, n.name, got, err)
		}
	}
	if got, err := s.Principals().ByProtocolID(ctx, last.IDs[0]); !merrs.IsNotFoundError(err) {
		t.Errorf("the refused group's gid row = %v, %v; want not found", got, err)
	}
	if after := counters(t, s).Principals; after != before {
		t.Errorf("principals generation across the refused batch: %d, want %d", after, before)
	}

	// The counters did not move: the next batch draws the first IDs.
	ok := entity.MintBatch{Users: []entity.MintUser{mintUser("alice")}, Groups: []entity.MintGroup{mintGroup("staff")}}
	must(t, s.Minter().Create(ctx, ok))
	u, err := s.Principals().User(ctx, ok.Users[0].User.Principal)
	must(t, err)
	g, _ := groupOf(t, s, ok.Groups[0].Group.Principal)
	if uid, gid, sid := spelling(u.IDs, entity.SchemeUID), spelling(g.IDs, entity.SchemeGID), spelling(u.IDs, entity.SchemeSID); uid != "1000000" || gid != "1000000" || sid != ridSID(t, s, 1000) {
		t.Errorf("after the refused batch: uid %s, gid %s, SID %x; want 1000000, 1000000 and RID 1000's", uid, gid, sid)
	}
}

// A batch never overwrites: one naming an existing user or group's
// PrincipalID fails ErrExist and leaves the record as it was.
func testMintNeverOverwrites(t *testing.T, kvo Opener) {
	s, _ := newStore(t, kvo)
	cp := s.ControlPlane()
	alice, staff := user("alice"), group("staff")
	must(t, errOf(cp.PutUser(ctx, alice, 0, entity.Alloc{})))
	must(t, errOf(cp.PutGroup(ctx, staff, 0, entity.Alloc{})))
	wasU, err := s.Principals().User(ctx, alice.Principal)
	must(t, err)
	wasG, _ := groupOf(t, s, staff.Principal)

	u, g := alice, staff
	u.Name, g.Name = "mallory", "other"
	for what, b := range map[string]entity.MintBatch{
		"user":  {Users: []entity.MintUser{{User: u}}},
		"group": {Groups: []entity.MintGroup{{Group: g}}},
	} {
		if err := s.Minter().Create(ctx, b); !merrs.IsAlreadyExistsError(err) {
			t.Errorf("minting an existing %s's ID: %v, want already exists", what, err)
		}
	}
	if got, err := s.Principals().User(ctx, alice.Principal); err != nil || got.Name != wasU.Name || got.Generation != wasU.Generation {
		t.Errorf("alice after the refused mint: %+v, %v; want %+v", got, err, wasU)
	}
	if got, _ := groupOf(t, s, staff.Principal); got.Name != wasG.Name || got.Generation != wasG.Generation {
		t.Errorf("staff after the refused mint: %+v; want %+v", got, wasG)
	}
}

// A batch raises the principals generation once however many records it
// holds, and every record it writes takes the raised value as its position.
func testMintRaisesPrincipalsOnce(t *testing.T, kvo Opener) {
	s, _ := newStore(t, kvo)
	before := counters(t, s).Principals
	b := entity.MintBatch{Users: []entity.MintUser{mintUser("alice")}}
	for i := range 300 {
		b.Groups = append(b.Groups, mintGroup(fmt.Sprint("g", i)))
	}
	must(t, s.Minter().Create(ctx, b))
	after := counters(t, s).Principals
	if after != before+1 {
		t.Errorf("principals generation across a 301-record batch: %d to %d, want a raise of one", before, after)
	}
	u, err := s.Principals().User(ctx, b.Users[0].User.Principal)
	must(t, err)
	if u.Position != after {
		t.Errorf("minted user's position: %d, want %d", u.Position, after)
	}
	groups := all(t, s.ControlPlane().Groups(ctx, ""))
	if len(groups) != 300 {
		t.Fatalf("groups after the batch: %d, want 300", len(groups))
	}
	for _, g := range groups {
		if g.Position != after {
			t.Errorf("minted group %s's position: %d, want %d", g.Name, g.Position, after)
		}
	}
}

// A record marked Unnamed is minted without a name, so a name a local
// principal holds never fails the batch: found by its SID, not by name.
func testMintUnnamed(t *testing.T, kvo Opener) {
	s, _ := newStore(t, kvo)
	cp, p := s.ControlPlane(), s.Principals()
	localU, localG := user("alice"), group("finance")
	must(t, errOf(cp.PutUser(ctx, localU, 0, entity.Alloc{})))
	must(t, errOf(cp.PutGroup(ctx, localG, 0, entity.Alloc{})))

	// No read lists an unnamed group, so this one carries its IDs.
	u, g := mintUser("Alice"), entity.MintGroup{Group: group("Finance"), Unnamed: true}
	u.Unnamed = true
	if err := s.Minter().Create(ctx, entity.MintBatch{Users: []entity.MintUser{u}, Groups: []entity.MintGroup{g}}); err != nil {
		t.Fatalf("minting unnamed records whose names are held: %v", err)
	}
	mu, err := p.User(ctx, u.User.Principal)
	must(t, err)
	for _, c := range []struct {
		what string
		ids  entity.ProtocolIDs
		want entity.Principal
		name string
		kind entity.PrincipalKind
		held entity.Principal
	}{
		{"user", mu.IDs, u.User.Principal, "alice", entity.PrincipalUser, localU.Principal},
		{"group", g.Group.IDs, g.Group.Principal, "finance", entity.PrincipalGroup, localG.Principal},
	} {
		if got, err := p.ByProtocolID(ctx, entity.ProtocolID{Scheme: entity.SchemeSID, Value: spelling(c.ids, entity.SchemeSID)}); err != nil || got != c.want {
			t.Errorf("unnamed %s by SID = %v, %v; want %v", c.what, got, err, c.want)
		}
		if got, err := p.ByName(ctx, c.kind, c.name); err != nil || got != c.held {
			t.Errorf("ByName(%s) = %v, %v; want the local %s %v", c.name, got, err, c.what, c.held)
		}
	}
}

// A batch is held to the checks every write is: uid 0, a machine with a
// name and a well-known principal are each refused, and a uid 0 the record
// holds with no Alloc.UID is refused rather than allocated.
func testMintShapes(t *testing.T, kvo Opener) {
	s, _ := newStore(t, kvo)
	named := machine(krb("host/a.corp.example@CORP.EXAMPLE"))
	named.Name = "a"
	for what, u := range map[string]entity.User{
		"uid 0":                   user("root0", uid("0")),
		"a machine with a name":   named,
		"a Special-kind record":   {Principal: entity.Principal{Kind: entity.PrincipalSpecial, ID: entity.NewPrincipalID()}, Name: "s"},
		"root's well-known ID":    {Principal: entity.RootPrincipal, Name: "root"},
		"uid 0 with a RID to add": {Principal: entity.NewUser(), Name: "z", IDs: entity.ProtocolIDs{uid("0")}},
	} {
		a := entity.Alloc{}
		if what == "uid 0 with a RID to add" {
			a = entity.Alloc{RID: true}
		}
		if err := s.Minter().Create(ctx, entity.MintBatch{Users: []entity.MintUser{{User: u, Alloc: a}}}); !isCode(err, merrs.ErrInvalidArgument) {
			t.Errorf("minting %s: %v, want invalid argument", what, err)
		}
	}
	if got, _ := allocUser(t, s, "after", fileRange); got != "1000000" {
		t.Errorf("uid after the refused mints: %s, want 1000000: a zero uid was allocated", got)
	}
}

// A minted user holds no API role: one naming admin is refused, and the
// enabled-admin index is unchanged.
func testMintNoAPIRole(t *testing.T, kvo Opener) {
	s, _ := newStore(t, kvo)
	admins := func() (n int) {
		must(t, s.ControlPlane().Write(ctx, entity.WriteMeta{}, func(tx entity.ControlPlaneTx) (err error) {
			n, err = tx.EnabledAdmins()
			return err
		}))
		return n
	}
	before := admins()
	r := mintUser("alice")
	r.User.Role = entity.RoleAdmin
	if err := s.Minter().Create(ctx, entity.MintBatch{Users: []entity.MintUser{r}}); !isCode(err, merrs.ErrInvalidArgument) {
		t.Errorf("minting an admin: %v, want invalid argument", err)
	}
	if after := admins(); after != before {
		t.Errorf("enabled admins after the refused mint: %d, want %d", after, before)
	}
}
