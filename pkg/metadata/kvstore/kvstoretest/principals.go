package kvstoretest

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/cubbit/dittofs/pkg/auth/sid"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	merrs "github.com/cubbit/dittofs/pkg/metadata/errors"
	"github.com/cubbit/dittofs/pkg/metadata/kvstore"
)

func gid(n string) entity.ProtocolID { return entity.ProtocolID{Scheme: entity.SchemeGID, Value: n} }

func sidOf(v string) entity.ProtocolID { return entity.ProtocolID{Scheme: entity.SchemeSID, Value: v} }

func krb(v string) entity.ProtocolID {
	return entity.ProtocolID{Scheme: entity.SchemeKerberos, Value: v}
}

func machine(ids ...entity.ProtocolID) entity.User {
	return entity.User{Principal: entity.Principal{Kind: entity.PrincipalMachine, ID: entity.NewPrincipalID()}, IDs: ids}
}

// A well-known principal is never created: no user record holds one.
func testWellKnownUserRefused(t *testing.T, kvo Opener) {
	s, _ := newStore(t, kvo)
	cp := s.ControlPlane()
	for _, p := range []entity.Principal{entity.RootPrincipal, {Kind: entity.PrincipalSpecial, ID: entity.NewPrincipalID()}} {
		if _, err := cp.PutUser(ctx, entity.User{Principal: p, Name: "root"}, 0, entity.Alloc{}); !isCode(err, merrs.ErrInvalidArgument) {
			t.Errorf("PutUser of %v: %v, want invalid argument", p, err)
		}
	}
}

// uid and gid 0 and 65534 are root's and anonymous's by rule: no write
// gives them to a user or group, so no index row maps them to one.
func testReservedIDsRefused(t *testing.T, kvo Opener) {
	s, _ := newStore(t, kvo)
	cp, p := s.ControlPlane(), s.Principals()
	for _, n := range []string{"0", "65534"} {
		if _, err := cp.PutUser(ctx, user("u"+n, uid(n)), 0, entity.Alloc{}); !isCode(err, merrs.ErrInvalidArgument) {
			t.Errorf("user with uid %s: %v, want invalid argument", n, err)
		}
		g := group("g" + n)
		g.IDs[0] = gid(n)
		if _, err := cp.PutGroup(ctx, g, 0, entity.Alloc{}); !isCode(err, merrs.ErrInvalidArgument) {
			t.Errorf("group with gid %s: %v, want invalid argument", n, err)
		}
		for _, id := range []entity.ProtocolID{uid(n), gid(n)} {
			if got, err := p.ByProtocolID(ctx, id); !merrs.IsNotFoundError(err) {
				t.Errorf("ByProtocolID(%s %s) = %v, %v; want not found", id.Scheme, id.Value, got, err)
			}
		}
	}
}

// A machine record holds its Kerberos name and nothing else.
func testMachineRecordShape(t *testing.T, kvo Opener) {
	s, _ := newStore(t, kvo)
	cp, p := s.ControlPlane(), s.Principals()
	name := krb("host/build01.corp.example@CORP.EXAMPLE")
	named := machine(name)
	named.Name = "build01"
	withUID := machine(name, uid("1500"))
	withPassword := machine(name)
	withPassword.Password = entity.SecretRef{15: 1}
	admin := machine(name)
	admin.Role = entity.RoleAdmin
	for what, bad := range map[string]entity.User{"a name": named, "a uid": withUID, "a password": withPassword, "an API role": admin} {
		if _, err := cp.PutUser(ctx, bad, 0, entity.Alloc{}); !isCode(err, merrs.ErrInvalidArgument) {
			t.Errorf("machine with %s: %v, want invalid argument", what, err)
		}
	}
	m := machine(name)
	gen, err := cp.PutUser(ctx, m, 0, entity.Alloc{})
	if err != nil {
		t.Fatalf("machine with its krb row only: %v", err)
	}
	if got, err := p.ByProtocolID(ctx, name); err != nil || got != m.Principal {
		t.Errorf("ByProtocolID(%s) = %v, %v; want %v", name.Value, got, err, m.Principal)
	}
	// A machine never becomes a user: a put naming its ID as a user's finds none.
	asUser := user("build01", name)
	asUser.Principal.ID = m.Principal.ID
	if _, err := cp.PutUser(ctx, asUser, gen, entity.Alloc{}); !merrs.IsNotFoundError(err) {
		t.Errorf("user put over a machine record: %v, want not found", err)
	}
}

// A machine record is read and deleted as a machine, and its Kerberos name
// is free again once it is; naming its ID as a user's finds no record.
func testMachineReadAndDelete(t *testing.T, kvo Opener) {
	s, _ := newStore(t, kvo)
	cp, p := s.ControlPlane(), s.Principals()
	name := krb("host/build01.corp.example@CORP.EXAMPLE")
	m := machine(name)
	gen, err := cp.PutUser(ctx, m, 0, entity.Alloc{})
	must(t, err)
	if got, err := p.User(ctx, m.Principal); err != nil || got.Principal != m.Principal || got.Generation != gen {
		t.Errorf("User(machine) = %+v, %v; want the record at generation %d", got, err, gen)
	}

	asUser := entity.Principal{Kind: entity.PrincipalUser, ID: m.Principal.ID}
	g := group("staff")
	must(t, errOf(cp.PutGroup(ctx, g, 0, entity.Alloc{})))
	for what, err := range map[string]error{
		"User":       errOf(p.User(ctx, asUser)),
		"DeleteUser": cp.DeleteUser(ctx, asUser, gen),
		"AddMember":  cp.AddMember(ctx, entity.Membership{Group: g.Principal, Member: asUser}),
	} {
		if !merrs.IsNotFoundError(err) {
			t.Errorf("%s naming the machine's ID as a user's: %v, want not found", what, err)
		}
	}
	if _, err := p.User(ctx, g.Principal); !merrs.IsNotFoundError(err) {
		t.Errorf("User(group): %v, want not found", err)
	}
	must(t, cp.Write(ctx, entity.WriteMeta{}, func(tx entity.ControlPlaneTx) error {
		if got, found, err := tx.User(m.Principal); err != nil || !found || got.Generation != gen {
			t.Errorf("tx.User(machine): found %v at generation %d, %v; want found at %d", found, got.Generation, err, gen)
		}
		if _, found, err := tx.User(g.Principal); err != nil || found {
			t.Errorf("tx.User(group) found %v, %v; want absent", found, err)
		}
		return nil
	}))

	must(t, deleted(s, cp.DeleteUser(ctx, m.Principal, gen), m.Principal))
	if _, err := p.User(ctx, m.Principal); !merrs.IsNotFoundError(err) {
		t.Errorf("User(machine) after its deletion: %v, want not found", err)
	}
	again := machine(name)
	if _, err := cp.PutUser(ctx, again, 0, entity.Alloc{}); err != nil {
		t.Fatalf("a second machine with the deleted one's krb name: %v", err)
	}
	if got, err := p.ByProtocolID(ctx, name); err != nil || got != again.Principal {
		t.Errorf("ByProtocolID(%s) = %v, %v; want %v", name.Value, got, err, again.Principal)
	}
}

// A user or group may have no name: no name row finds it, its protocol IDs
// do, and a rename to no name frees the old one.
// Records reads a group's record by ID, and finds neither a user under a
// group's kind nor a deleted group.
func testGroupRecord(t *testing.T, kvo Opener) {
	s, _ := newStore(t, kvo)
	cp, r := s.ControlPlane(), s.Records()
	g := group("staff")
	gen, err := cp.PutGroup(ctx, g, 0, entity.Alloc{})
	must(t, err)
	got, err := r.Group(ctx, g.Principal)
	if err != nil || got.Name != "staff" || !slices.Equal(got.IDs, g.IDs) || got.Generation != gen {
		t.Fatalf("Group(staff) = %+v, %v; want name staff, IDs %v, generation %d", got, err, g.IDs, gen)
	}
	if got, err := r.Group(ctx, entity.Principal{Kind: entity.PrincipalUser, ID: g.Principal.ID}); !merrs.IsNotFoundError(err) {
		t.Errorf("Group of a user principal with staff's ID = %+v, %v; want not found", got, err)
	}
	must(t, cp.DeleteGroup(ctx, g.Principal, gen))
	if got, err := r.Group(ctx, g.Principal); !merrs.IsNotFoundError(err) {
		t.Errorf("Group of a deleted group = %+v, %v; want not found", got, err)
	}
}

func testUnnamedPrincipals(t *testing.T, kvo Opener) {
	s, _ := newStore(t, kvo)
	cp, p := s.ControlPlane(), s.Principals()
	u := user("", uid("1500"))
	g := group("")
	alice := user("alice", uid("1501"))
	if _, err := cp.PutUser(ctx, u, 0, entity.Alloc{}); err != nil {
		t.Fatalf("creating an unnamed user: %v", err)
	}
	if _, err := cp.PutGroup(ctx, g, 0, entity.Alloc{}); err != nil {
		t.Fatalf("creating an unnamed group: %v", err)
	}
	gen, err := cp.PutUser(ctx, alice, 0, entity.Alloc{})
	must(t, err)
	alice.Name = ""
	if _, err := cp.PutUser(ctx, alice, gen, entity.Alloc{}); err != nil {
		t.Fatalf("renaming alice to no name: %v", err)
	}

	for _, c := range []struct {
		kind entity.PrincipalKind
		name string
	}{{entity.PrincipalUser, ""}, {entity.PrincipalGroup, ""}, {entity.PrincipalUser, "alice"}} {
		if got, err := p.ByName(ctx, c.kind, c.name); !merrs.IsNotFoundError(err) {
			t.Errorf("ByName(%v, %q) = %v, %v; want not found", c.kind, c.name, got, err)
		}
	}
	for _, c := range []struct {
		id   entity.ProtocolID
		want entity.Principal
	}{{u.IDs[0], u.Principal}, {g.IDs[0], g.Principal}, {alice.IDs[0], alice.Principal}} {
		if got, err := p.ByProtocolID(ctx, c.id); err != nil || got != c.want {
			t.Errorf("ByProtocolID(%s %s) = %v, %v; want %v", c.id.Scheme, c.id.Value, got, err, c.want)
		}
	}
	// The freed name can be taken.
	must(t, errOf(cp.PutUser(ctx, user("alice"), 0, entity.Alloc{})))

	// The listings walk the name index: no unnamed record, and no machine, is listed.
	must(t, errOf(cp.PutUser(ctx, machine(krb("host/nas01.corp.example@CORP.EXAMPLE")), 0, entity.Alloc{})))
	var users []string
	for _, x := range all(t, cp.Users(ctx, "")) {
		users = append(users, x.Name)
	}
	if len(users) != 1 || users[0] != "alice" {
		t.Errorf("Users = %q; want only [alice], no unnamed user and no machine", users)
	}
	if got := all(t, cp.Groups(ctx, "")); len(got) != 0 {
		t.Errorf("Groups = %v; want none, the unnamed group unlisted", got)
	}
}

// A user's two credential references are read back as written, each in its
// own place.
func testUserCredentialRefs(t *testing.T, kvo Opener) {
	s, _ := newStore(t, kvo)
	cp, p := s.ControlPlane(), s.Principals()
	both := user("alice")
	both.Password, both.NTHash = entity.SecretRef{15: 1}, entity.SecretRef{15: 2}
	apiOnly := user("admin")
	apiOnly.Password = entity.SecretRef{15: 3}
	for _, u := range []entity.User{both, apiOnly} {
		must(t, errOf(cp.PutUser(ctx, u, 0, entity.Alloc{})))
		got, err := p.User(ctx, u.Principal)
		must(t, err)
		if got.Password != u.Password || got.NTHash != u.NTHash {
			t.Errorf("%s: Password %x, NTHash %x; want %x, %x", u.Name, got.Password, got.NTHash, u.Password, u.NTHash)
		}
	}
}

// A stored spelling is unique in its scheme: a SID among users and groups
// together, a Kerberos name among users. A number's scheme is its kind's.
func testSpellingUniqueAcrossKinds(t *testing.T, kvo Opener) {
	s, _ := newStore(t, kvo)
	cp := s.ControlPlane()
	must(t, errOf(cp.PutUser(ctx, user("alice", uid("1500"), sidOf("sid-a"), krb("alice@CORP.EXAMPLE")), 0, entity.Alloc{})))
	g := group("staff")
	g.IDs[1] = sidOf("sid-a")
	if _, err := cp.PutGroup(ctx, g, 0, entity.Alloc{}); !merrs.IsAlreadyExistsError(err) {
		t.Errorf("group holding alice's SID: %v, want already exists", err)
	}
	if _, err := cp.PutUser(ctx, user("bob", krb("alice@CORP.EXAMPLE")), 0, entity.Alloc{}); !merrs.IsAlreadyExistsError(err) {
		t.Errorf("bob holding alice's krb name: %v, want already exists", err)
	}
	g.IDs[0], g.IDs[1] = gid("1500"), freshSID()
	if _, err := cp.PutGroup(ctx, g, 0, entity.Alloc{}); err != nil {
		t.Errorf("gid 1500 beside uid 1500: %v", err)
	}
}

// Names are unique under simple case folding within their kind, are found
// under any case, keep the case they were written in, and never hold @ or \.
func testPrincipalNameCase(t *testing.T, kvo Opener) {
	s, _ := newStore(t, kvo)
	cp, p := s.ControlPlane(), s.Principals()
	alice := user("Alice")
	gen, err := cp.PutUser(ctx, alice, 0, entity.Alloc{})
	must(t, err)
	if _, err := cp.PutUser(ctx, user("aLICE"), 0, entity.Alloc{}); !merrs.IsAlreadyExistsError(err) {
		t.Errorf("aLICE beside Alice: %v, want already exists", err)
	}
	if got, err := p.ByName(ctx, entity.PrincipalUser, "ALICE"); err != nil || got != alice.Principal {
		t.Errorf("ByName(ALICE) = %v, %v; want %v", got, err, alice.Principal)
	}
	if users := all(t, cp.Users(ctx, "")); len(users) != 1 || users[0].Name != "Alice" {
		t.Errorf("Users = %+v, want Alice as written", users)
	}
	// A change of case only keeps the row, now in the new case.
	alice.Name = "ALICE"
	gen, err = cp.PutUser(ctx, alice, gen, entity.Alloc{})
	must(t, err)
	if got, err := p.ByName(ctx, entity.PrincipalUser, "alice"); err != nil || got != alice.Principal {
		t.Errorf("after a case change, ByName(alice) = %v, %v; want %v", got, err, alice.Principal)
	}

	// A local CORP\bob would be a second spelling of bob, whose DOMAIN\
	// prefix a lookup strips.
	for _, bad := range []string{"bob@example.com", `CORP\bob`} {
		if _, err := cp.PutUser(ctx, user(bad), 0, entity.Alloc{}); !isCode(err, merrs.ErrInvalidArgument) {
			t.Errorf("creating %s: %v, want invalid argument", bad, err)
		}
		renamed := alice
		renamed.Name = bad
		if _, err := cp.PutUser(ctx, renamed, gen, entity.Alloc{}); !isCode(err, merrs.ErrInvalidArgument) {
			t.Errorf("renaming to %s: %v, want invalid argument", bad, err)
		}
	}

	// The Kelvin sign folds with k, which an ASCII-only fold misses, and the
	// long s with s, which a lower-casing fold misses.
	for _, c := range []struct{ held, other string }{{"kelvin", "\u212Aelvin"}, {"sam", "\u017Fam"}} {
		must(t, errOf(cp.PutUser(ctx, user(c.held), 0, entity.Alloc{})))
		if _, err := cp.PutUser(ctx, user(c.other), 0, entity.Alloc{}); !merrs.IsAlreadyExistsError(err) {
			t.Errorf("%+q beside %s: %v, want already exists", c.other, c.held, err)
		}
	}

	// A user and a group may share a name.
	must(t, errOf(cp.PutUser(ctx, user("finance"), 0, entity.Alloc{})))
	if _, err := cp.PutGroup(ctx, group("finance"), 0, entity.Alloc{}); err != nil {
		t.Errorf("group finance beside user finance: %v", err)
	}
}

// A user's or group's name no protocol carries is refused invalid argument
// on create, on rename and on a mint, and nothing is written: the rule is
// the store's, so every path writing a name holds it. A name of exactly 255
// bytes is accepted, on create and on rename.
func testPrincipalNameChars(t *testing.T, kvo Opener) {
	s, _ := newStore(t, kvo)
	cp, p := s.ControlPlane(), s.Principals()
	alice, staff := user("alice"), group("staff")
	ugen, err := cp.PutUser(ctx, alice, 0, entity.Alloc{})
	must(t, err)
	ggen, err := cp.PutGroup(ctx, staff, 0, entity.Alloc{})
	must(t, err)
	bad := map[string]string{
		"invalid UTF-8": "bad\xffname",
		"NUL":           "nul\x00z",
		"BEL":           "bell\x07",
		"newline":       "x\ny",
		"leading space": " lead",
		"trailing tab":  "trail\t",
		"256 bytes":     strings.Repeat("a", 256),
		"at":            "bob@corp",
		"backslash":     `CORP\bob`,
	}
	refused := func(what, name string, err error) {
		t.Helper()
		if !isCode(err, merrs.ErrInvalidArgument) || !errors.Is(err, entity.ErrLocalName) {
			t.Errorf("%s named %s: %v, want invalid argument wrapping ErrLocalName", what, name, err)
		}
	}
	for why, name := range bad {
		_, err := cp.PutUser(ctx, user(name), 0, entity.Alloc{})
		refused("creating a user", why, err)
		_, err = cp.PutGroup(ctx, group(name), 0, entity.Alloc{})
		refused("creating a group", why, err)
		ru, rg := alice, staff
		ru.Name, rg.Name = name, name
		_, err = cp.PutUser(ctx, ru, ugen, entity.Alloc{})
		refused("renaming a user", why, err)
		_, err = cp.PutGroup(ctx, rg, ggen, entity.Alloc{})
		refused("renaming a group", why, err)
		err = s.Minter().Create(ctx, entity.MintBatch{Users: []entity.MintUser{{User: user(name)}}})
		refused("minting a user", why, err)
		err = s.Minter().Create(ctx, entity.MintBatch{Groups: []entity.MintGroup{{Group: group(name)}}})
		refused("minting a group", why, err)
		for _, kind := range []entity.PrincipalKind{entity.PrincipalUser, entity.PrincipalGroup} {
			if got, err := p.ByName(ctx, kind, name); !merrs.IsNotFoundError(err) {
				t.Errorf("ByName(%s, %s) after the refusals = %v, %v; want not found", kind, why, got, err)
			}
		}
	}
	if users := all(t, cp.Users(ctx, "")); len(users) != 1 || users[0].Name != "alice" || users[0].Generation != ugen {
		t.Errorf("users after the refusals: %+v, want alice alone, unchanged", users)
	}
	if groups := all(t, cp.Groups(ctx, "")); len(groups) != 1 || groups[0].Name != "staff" || groups[0].Generation != ggen {
		t.Errorf("groups after the refusals: %+v, want staff alone, unchanged", groups)
	}
	long := strings.Repeat("a", entity.MaxLocalName)
	if _, err := cp.PutUser(ctx, user(long), 0, entity.Alloc{}); err != nil {
		t.Errorf("creating a user named %d bytes: %v, want accepted", len(long), err)
	}
	if _, err := cp.PutGroup(ctx, group(long), 0, entity.Alloc{}); err != nil {
		t.Errorf("creating a group named %d bytes: %v, want accepted", len(long), err)
	}
	long = strings.Repeat("b", entity.MaxLocalName)
	alice.Name, staff.Name = long, long
	if _, err := cp.PutUser(ctx, alice, ugen, entity.Alloc{}); err != nil {
		t.Errorf("renaming a user to %d bytes: %v, want accepted", len(long), err)
	}
	if _, err := cp.PutGroup(ctx, staff, ggen, entity.Alloc{}); err != nil {
		t.Errorf("renaming a group to %d bytes: %v, want accepted", len(long), err)
	}
}

// fileRange is the default allocation range.
var fileRange = entity.IDRange{Lo: 1_000_000, Hi: 1_999_999_999}

// allocUser creates a user named name drawing a uid and a RID from r, and
// returns the uid and SID it was given.
func allocUser(t *testing.T, s *kvstore.Store, name string, r entity.IDRange) (uid, sid string) {
	t.Helper()
	u := user(name)
	must(t, errOf(s.ControlPlane().PutUser(ctx, u, 0, entity.Alloc{UID: true, RID: true, Range: r})))
	got, err := s.Principals().User(ctx, u.Principal)
	must(t, err)
	return spelling(got.IDs, entity.SchemeUID), spelling(got.IDs, entity.SchemeSID)
}

// allocGroup is allocUser for a group's gid and RID.
func allocGroup(t *testing.T, s *kvstore.Store, name string, r entity.IDRange) (gid string) {
	t.Helper()
	g := group(name)
	g.IDs = nil
	must(t, errOf(s.ControlPlane().PutGroup(ctx, g, 0, entity.Alloc{GID: true, RID: true, Range: r})))
	for got, err := range s.ControlPlane().Groups(ctx, "") {
		must(t, err)
		if got.Principal == g.Principal {
			return spelling(got.IDs, entity.SchemeGID)
		}
	}
	t.Fatalf("group %s not found", name)
	return ""
}

func spelling(ids entity.ProtocolIDs, scheme entity.ProtocolScheme) string {
	for _, id := range ids {
		if id.Scheme == scheme {
			return id.Value
		}
	}
	return ""
}

// An allocated uid or RID is never given again, even once its principal is
// deleted and no live record holds it.
func testAllocNeverRepeats(t *testing.T, kvo Opener) {
	s, _ := newStore(t, kvo)
	uid1, sid1 := allocUser(t, s, "alice", fileRange)
	alice, err := s.Principals().ByName(ctx, entity.PrincipalUser, "alice")
	must(t, err)
	must(t, s.ControlPlane().DeleteUser(ctx, alice, userGeneration(t, s, alice)))
	uid2, sid2 := allocUser(t, s, "bob", fileRange)
	if uid1 != "1000000" || uid2 != "1000001" {
		t.Errorf("uids allocated across a deletion: %s, %s; want 1000000, 1000001", uid1, uid2)
	}
	if sid1 == sid2 {
		t.Errorf("SID %x allocated twice across a deletion", sid1)
	}
}

// A uid the operator assigned at the counter's next value is skipped, while
// a gid of the same value is not: each scheme has its own rows. A range
// spanning 65534 allocates past it.
func testAllocSkipsHeldIDs(t *testing.T, kvo Opener) {
	s, _ := newStore(t, kvo)
	must(t, errOf(s.ControlPlane().PutUser(ctx, user("op", uid("1000000")), 0, entity.Alloc{})))
	if got, _ := allocUser(t, s, "alice", fileRange); got != "1000001" {
		t.Errorf("uid allocated beside an assigned 1000000: %s, want 1000001", got)
	}
	if got := allocGroup(t, s, "staff", fileRange); got != "1000000" {
		t.Errorf("gid allocated beside uid 1000000: %s, want 1000000", got)
	}

	s, _ = newStore(t, kvo)
	r := entity.IDRange{Lo: 65533, Hi: 65535}
	for i, want := range []string{"65533", "65535"} {
		if got, _ := allocUser(t, s, fmt.Sprint("u", i), r); got != want {
			t.Errorf("uid %d of a range spanning 65534: %s, want %s", i, got, want)
		}
	}
}

// A block of uids an operator assigned at the start of the range, as an
// import writes them, is passed in a few reads: an allocation probing them
// one read each would outlast a remote store's transaction age.
func testAllocPastHeldBlock(t *testing.T, kvo Opener) {
	const held = 3000
	r := entity.IDRange{Lo: 10_000, Hi: 60_000}

	s, c := newStore(t, kvo)
	allocUser(t, s, "first", r)
	empty := c.TakeReads()

	s, c = newStore(t, kvo)
	for n := r.Lo; n < r.Lo+held; n++ {
		must(t, errOf(s.ControlPlane().PutUser(ctx, user(fmt.Sprint("op", n), uid(fmt.Sprint(n))), 0, entity.Alloc{})))
	}
	c.TakeReads()
	if got, _ := allocUser(t, s, "alice", r); got != fmt.Sprint(r.Lo+held) {
		t.Errorf("uid allocated past %d held uids: %s, want %d", held, got, r.Lo+held)
	}
	if got := c.TakeReads() - empty; got > held/100 {
		t.Errorf("reads past %d held uids: %d more than an allocation in an empty range, want at most %d", held, got, held/100)
	}
}

// A change of range applies to the next allocation and moves no ID already
// given; a range with no ID left refuses the write, which writes nothing.
func testAllocRange(t *testing.T, kvo Opener) {
	s, _ := newStore(t, kvo)
	first, _ := allocUser(t, s, "alice", fileRange)
	if got, _ := allocUser(t, s, "bob", entity.IDRange{Lo: 3_000_000, Hi: 3_999_999}); got != "3000000" {
		t.Errorf("uid after a range change: %s, want 3000000", got)
	}
	alice, err := s.Principals().ByName(ctx, entity.PrincipalUser, "alice")
	must(t, err)
	got, err := s.Principals().User(ctx, alice)
	must(t, err)
	if uid := spelling(got.IDs, entity.SchemeUID); uid != first || first != "1000000" {
		t.Errorf("alice's uid after the range change: %s, want %s, as allocated at 1000000", uid, first)
	}

	s, _ = newStore(t, kvo)
	r := entity.IDRange{Lo: 1_000_000, Hi: 1_000_001}
	for _, name := range []string{"a", "b"} {
		allocUser(t, s, name, r)
	}
	c := user("c")
	if _, err := s.ControlPlane().PutUser(ctx, c, 0, entity.Alloc{UID: true, RID: true, Range: r}); !isCode(err, merrs.ErrNoSpace) {
		t.Errorf("a third create from a two-ID range: %v, want no space", err)
	}
	if got := all(t, s.ControlPlane().Users(ctx, "")); len(got) != 2 {
		t.Errorf("users after the refused create: %d, want 2", len(got))
	}
}

// A write refused after drawing its IDs raises no counter: the next create
// gets the IDs the refused one would have had.
func testAllocInsideTheWrite(t *testing.T, kvo Opener) {
	s, _ := newStore(t, kvo)
	uid1, _ := allocUser(t, s, "alice", fileRange)
	if _, err := s.ControlPlane().PutUser(ctx, user("alice"), 0, entity.Alloc{UID: true, RID: true, Range: fileRange}); !merrs.IsAlreadyExistsError(err) {
		t.Fatalf("a second alice: %v, want already exists", err)
	}
	if uid2, _ := allocUser(t, s, "bob", fileRange); uid1 != "1000000" || uid2 != "1000001" {
		t.Errorf("uids around a refused create: %s, %s; want 1000000, 1000001", uid1, uid2)
	}
}

// A user's uid and SID, and a group's gid and SID, are held together; a
// principal with no file spelling holds none, so an API-only account and a
// machine are written as they are. An allocation is a create's alone, a SID
// never changes, and no principal holds two spellings of one scheme.
func testFileSpellingsTogether(t *testing.T, kvo Opener) {
	s, _ := newStore(t, kvo)
	cp := s.ControlPlane()
	noSID, noGID := group("g1"), group("g2")
	noSID.IDs, noGID.IDs = noSID.IDs[:1], noGID.IDs[1:]
	for what, err := range map[string]error{
		"a user with a uid and no SID": errOf(cp.PutUser(ctx, entity.User{Principal: entity.NewUser(), Name: "u1", IDs: entity.ProtocolIDs{uid("1500")}}, 0, entity.Alloc{})),
		"a user with a SID and no uid": errOf(cp.PutUser(ctx, user("u2", sidOf("sid-u2")), 0, entity.Alloc{})),
		"a group with no SID":          errOf(cp.PutGroup(ctx, noSID, 0, entity.Alloc{})),
		"a group with no gid":          errOf(cp.PutGroup(ctx, noGID, 0, entity.Alloc{})),
	} {
		if !isCode(err, merrs.ErrInvalidArgument) {
			t.Errorf("%s: %v, want invalid argument", what, err)
		}
	}
	for what, err := range map[string]error{
		"an API-only account":             errOf(cp.PutUser(ctx, user("admin"), 0, entity.Alloc{})),
		"a machine with its krb row only": errOf(cp.PutUser(ctx, machine(krb("host/a.corp.example@CORP.EXAMPLE")), 0, entity.Alloc{})),
	} {
		if err != nil {
			t.Errorf("%s: %v, want accepted", what, err)
		}
	}

	api := user("api")
	gen, err := cp.PutUser(ctx, api, 0, entity.Alloc{})
	must(t, err)
	if _, err := cp.PutUser(ctx, api, gen, entity.Alloc{UID: true, RID: true, Range: fileRange}); !isCode(err, merrs.ErrInvalidArgument) {
		t.Errorf("an update allocating a uid and a RID: %v, want invalid argument", err)
	}
	u := user("alice", uid("1500"))
	aliceGen, err := cp.PutUser(ctx, u, 0, entity.Alloc{})
	must(t, err)
	changed := u
	changed.IDs = entity.ProtocolIDs{uid("1500"), sidOf("sid-other")}
	if _, err := cp.PutUser(ctx, changed, aliceGen, entity.Alloc{}); !isCode(err, merrs.ErrInvalidArgument) {
		t.Errorf("an update changing the SID: %v, want invalid argument", err)
	}
	g := group("staff")
	gen, err = cp.PutGroup(ctx, g, 0, entity.Alloc{})
	must(t, err)
	g.IDs[1] = freshSID()
	if _, err := cp.PutGroup(ctx, g, gen, entity.Alloc{}); !isCode(err, merrs.ErrInvalidArgument) {
		t.Errorf("a group update changing the SID: %v, want invalid argument", err)
	}

	// At most one spelling per scheme: a second uid, gid or SID is refused,
	// and the refused write files no row for it.
	// bob takes the second uid, so each update meets a record no other case
	// has changed.
	bob := user("bob", uid("1510"))
	bobGen, err := cp.PutUser(ctx, bob, 0, entity.Alloc{})
	must(t, err)
	withExtra := func(name string, extra entity.ProtocolID) entity.Group {
		g := group(name)
		g.IDs = append(g.IDs, extra)
		return g
	}
	for _, c := range []struct {
		what  string
		err   error
		added entity.ProtocolID
	}{
		{"an update adding a second SID to alice", errOf(cp.PutUser(ctx, entity.User{Principal: u.Principal, Name: u.Name, IDs: entity.ProtocolIDs{uid("1500"), u.IDs[1], sidOf("sid-extra")}}, aliceGen, entity.Alloc{})), sidOf("sid-extra")},
		{"an update adding a second uid to bob", errOf(cp.PutUser(ctx, entity.User{Principal: bob.Principal, Name: bob.Name, IDs: entity.ProtocolIDs{uid("1510"), uid("1511"), bob.IDs[1]}}, bobGen, entity.Alloc{})), uid("1511")},
		{"a group created with two gids", errOf(cp.PutGroup(ctx, withExtra("twogids", gid("1600")), 0, entity.Alloc{})), gid("1600")},
		{"a group created with two SIDs", errOf(cp.PutGroup(ctx, withExtra("twosids", sidOf("sid-second")), 0, entity.Alloc{})), sidOf("sid-second")},
	} {
		if !isCode(c.err, merrs.ErrInvalidArgument) {
			t.Errorf("%s: %v, want invalid argument", c.what, c.err)
		}
		if got, err := s.Principals().ByProtocolID(ctx, c.added); !merrs.IsNotFoundError(err) {
			t.Errorf("%s: ByProtocolID(%s %s) = %v, %v; want not found", c.what, c.added.Scheme, c.added.Value, got, err)
		}
	}
}

// A uid or gid allocation with no SID held or allocated beside it is refused
// naming the RID, before any counter moves.
func testAllocNeedsRID(t *testing.T, kvo Opener) {
	s, _ := newStore(t, kvo)
	cp := s.ControlPlane()
	gr := group("staff")
	gr.IDs = nil
	for what, err := range map[string]error{
		"a uid": errOf(cp.PutUser(ctx, user("alice"), 0, entity.Alloc{UID: true, Range: fileRange})),
		"a gid": errOf(cp.PutGroup(ctx, gr, 0, entity.Alloc{GID: true, Range: fileRange})),
	} {
		if !isCode(err, merrs.ErrInvalidArgument) || !strings.Contains(err.Error(), "RID") {
			t.Errorf("allocating %s without a RID: %v, want invalid argument naming the RID", what, err)
		}
	}
	// A SID the record holds stands in for the RID.
	if _, err := cp.PutUser(ctx, user("bob", freshSID()), 0, entity.Alloc{UID: true, Range: fileRange}); err != nil {
		t.Errorf("allocating a uid beside a held SID: %v", err)
	}
	if got := allocGroup(t, s, "staff", fileRange); got != "1000000" {
		t.Errorf("gid after a refused allocation: %s, want 1000000", got)
	}
}

// An allocated SID is the installation's domain SID, S-1-5-21-a-b-c with a,
// b and c the installation ID's first twelve bytes as little-endian 32-bit
// numbers, followed by the RID, from 1000.
func testAllocatedSID(t *testing.T, kvo Opener) {
	s, _ := newStore(t, kvo)
	_, got := allocUser(t, s, "alice", fileRange)
	if want := ridSID(t, s, 1000); got != want {
		t.Errorf("allocated SID %x, want %x", got, want)
	}
}

// ridSID is the encoded SID the installation's domain SID and rid make,
// computed here from the installation ID rather than by the store.
func ridSID(t *testing.T, s *kvstore.Store, rid uint32) string {
	t.Helper()
	in, err := s.Installation(ctx)
	must(t, err)
	le := binary.LittleEndian
	want, err := sid.ParseSIDString(fmt.Sprintf("S-1-5-21-%d-%d-%d-%d", le.Uint32(in.ID[0:]), le.Uint32(in.ID[4:]), le.Uint32(in.ID[8:]), rid))
	must(t, err)
	var b bytes.Buffer
	sid.EncodeSID(&b, want)
	return b.String()
}
