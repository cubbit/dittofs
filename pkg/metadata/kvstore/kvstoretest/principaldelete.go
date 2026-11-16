package kvstoretest

import (
	"testing"

	"github.com/cubbit/dittofs/pkg/metadata/entity"
	merrs "github.com/cubbit/dittofs/pkg/metadata/errors"
)

// From its mark, a deleted group grants nothing to its members, and a
// primary naming it reads as none; a deleted user is found by no name or
// ID; a write naming either is refused as not found; and a new principal
// may take the name and the ID the mark freed.
func testMarkedPrincipalReads(t *testing.T, kvo Opener) {
	s, _ := newStore(t, kvo)
	cp, p := s.ControlPlane(), s.Principals()
	gid := entity.ProtocolID{Scheme: entity.SchemeGID, Value: "50"}
	staff, outer := group("staff"), group("all")
	staff.IDs = entity.ProtocolIDs{gid}
	u := user("alice", uid("1000"))
	u.Primary = staff.Principal
	sh := share("photos", "/photos")
	must(t, errOf(cp.PutGroup(ctx, staff, 0, entity.Alloc{})))
	must(t, errOf(cp.PutGroup(ctx, outer, 0, entity.Alloc{})))
	must(t, errOf(cp.PutUser(ctx, u, 0, entity.Alloc{})))
	must(t, errOf(CreateShare(ctx, cp, sh)))
	must(t, cp.AddMember(ctx, entity.Membership{Group: staff.Principal, Member: u.Principal}))
	must(t, cp.AddMember(ctx, entity.Membership{Group: outer.Principal, Member: staff.Principal}))
	must(t, errOf(cp.SetGrant(ctx, entity.ShareGrant{Share: sh.ID, Principal: outer.Principal, Access: entity.AccessRead}, 0)))
	staffGen := groupGeneration(t, cp, staff.Principal)

	must(t, cp.DeleteGroup(ctx, staff.Principal, staffGen))
	if got := all(t, p.Groups(ctx, u.Principal)); len(got) != 0 {
		t.Errorf("alice's groups with staff marked: got %v, want none, as all is reached only through staff", got)
	}
	if a, err := p.Grant(ctx, sh.ID, u.Principal); err != nil || a != entity.AccessNone {
		t.Errorf("alice's grant through a marked group: got %v, %v; want none", a, err)
	}
	if got, err := p.User(ctx, u.Principal); err != nil || !got.Primary.IsZero() {
		t.Errorf("alice's primary naming a marked group: got %v, %v; want zero", got.Primary, err)
	}
	for name, err := range map[string]error{
		"ByName":       errOf(p.ByName(ctx, entity.PrincipalGroup, "staff")),
		"ByProtocolID": errOf(p.ByProtocolID(ctx, gid)),
		"AddMember":    cp.AddMember(ctx, entity.Membership{Group: staff.Principal, Member: u.Principal}),
		"SetGrant":     errOf(cp.SetGrant(ctx, entity.ShareGrant{Share: sh.ID, Principal: staff.Principal, Access: entity.AccessRead}, 0)),
		"PutGroup":     errOf(cp.PutGroup(ctx, staff, staffGen, entity.Alloc{})),
		"DeleteGroup":  cp.DeleteGroup(ctx, staff.Principal, staffGen),
	} {
		if !merrs.IsNotFoundError(err) {
			t.Errorf("%s naming a marked group: got %v, want not found", name, err)
		}
	}
	again := group("staff")
	again.IDs = entity.ProtocolIDs{gid}
	if _, err := cp.PutGroup(ctx, again, 0, entity.Alloc{}); err != nil {
		t.Errorf("a new group taking a marked group's name and gid: %v", err)
	}

	userGen := userGeneration(t, s, u.Principal)
	must(t, cp.DeleteUser(ctx, u.Principal, userGen))
	var inTx bool
	must(t, cp.Write(ctx, entity.WriteMeta{}, func(tx entity.ControlPlaneTx) (err error) {
		_, inTx, err = tx.User(u.Principal)
		return err
	}))
	if inTx {
		t.Errorf("a marked user read in a write: found, want not")
	}
	for name, err := range map[string]error{
		"User":         errOf(p.User(ctx, u.Principal)),
		"ByName":       errOf(p.ByName(ctx, entity.PrincipalUser, "alice")),
		"ByProtocolID": errOf(p.ByProtocolID(ctx, uid("1000"))),
		"PutUser":      errOf(cp.PutUser(ctx, u, userGen, entity.Alloc{})),
		"DeleteUser":   cp.DeleteUser(ctx, u.Principal, userGen),
		"SetQuota":     errOf(cp.SetQuota(ctx, sh.ID, entity.Quota{Principal: u.Principal, Hard: entity.Limit{Files: 1}}, 0)),
	} {
		if !merrs.IsNotFoundError(err) {
			t.Errorf("%s naming a marked user: got %v, want not found", name, err)
		}
	}
}

// ContinueDelete ends a deletion whose record is gone, refuses a principal
// no deletion marked, and after its last batch answers done again.
func testContinueDeleteEnds(t *testing.T, kvo Opener) {
	s, _ := newStore(t, kvo)
	cp, n := s.ControlPlane(), s.Nodes()
	if done, err := n.ContinueDelete(ctx, entity.NewUser()); err != nil || !done {
		t.Errorf("ContinueDelete on a user with no record: got done %v, %v; want done", done, err)
	}
	u := user("alice")
	gen, err := cp.PutUser(ctx, u, 0, entity.Alloc{})
	must(t, err)
	if done, err := n.ContinueDelete(ctx, u.Principal); err == nil || done {
		t.Errorf("ContinueDelete on an unmarked user: got done %v, %v; want refused", done, err)
	}
	must(t, deleted(s, cp.DeleteUser(ctx, u.Principal, gen), u.Principal))
	if done, err := n.ContinueDelete(ctx, u.Principal); err != nil || !done {
		t.Errorf("ContinueDelete after the last batch: got done %v, %v; want done", done, err)
	}
}
