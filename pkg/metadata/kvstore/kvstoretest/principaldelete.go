package kvstoretest

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cubbit/dittofs/pkg/metadata/entity"
	merrs "github.com/cubbit/dittofs/pkg/metadata/errors"
)

// A put naming Deleted stores the principal unmarked: only a deletion
// marks. From its mark, a deleted group grants nothing to its members, and
// a primary naming it reads as none; a deleted user is found by no name or
// ID, nor are its tokens and sessions; a write naming either is refused as
// not found; a new principal may take the name and the ID the mark freed;
// and the deletion then runs to its end.
func testMarkedPrincipalReads(t *testing.T, kvo Opener) {
	s, _ := newStore(t, kvo)
	cp, p := s.ControlPlane(), s.Principals()
	gid := entity.ProtocolID{Scheme: entity.SchemeGID, Value: "50"}
	staff, outer := group("staff"), group("all")
	staff.IDs = entity.ProtocolIDs{gid}
	u := user("alice", uid("1000"))
	u.Primary = staff.Principal
	sh := share("photos", "/photos")
	staff.Deleted, u.Deleted = true, true
	must(t, errOf(cp.PutGroup(ctx, staff, 0, entity.Alloc{})))
	must(t, errOf(cp.PutGroup(ctx, outer, 0, entity.Alloc{})))
	must(t, errOf(cp.PutUser(ctx, u, 0, entity.Alloc{})))
	if got, err := p.User(ctx, u.Principal); err != nil || got.Primary != staff.Principal {
		t.Errorf("a user and its primary group put with Deleted set: got primary %v, %v; want %v, unmarked", got.Primary, err, staff.Principal)
	}
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
	must(t, cp.Write(ctx, entity.WriteMeta{}, func(tx entity.ControlPlaneTx) error {
		got, _, err := tx.User(u.Principal)
		if err == nil && !got.Primary.IsZero() {
			t.Errorf("alice's primary naming a marked group, read in a write: got %v, want zero", got.Primary)
		}
		return err
	}))
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

	e := time.Unix(1_900_000_000, 0).UTC()
	sess := entity.Session{ID: uuid.New(), Account: u.Principal, Refresh: [32]byte{1}, Expires: e}
	tok := entity.APIToken{ID: uuid.New(), Account: u.Principal, Hash: [32]byte{2}, Expires: e}
	must(t, cp.PutSession(ctx, sess))
	must(t, cp.PutAPIToken(ctx, tok))
	userGen := userGeneration(t, s, u.Principal)
	must(t, cp.DeleteUser(ctx, u.Principal, userGen))
	for name, err := range map[string]error{
		"APITokenByHash":   absent(cp.APITokenByHash(ctx, tok.Hash)),
		"APIToken":         absent(cp.APIToken(ctx, tok.ID)),
		"SessionByRefresh": absent(cp.SessionByRefresh(ctx, sess.Refresh)),
		"Session":          absent(cp.Session(ctx, sess.ID)),
	} {
		if err != nil {
			t.Errorf("%s of a marked user's: %v, want absent", name, err)
		}
	}
	must(t, cp.Write(ctx, entity.WriteMeta{}, func(tx entity.ControlPlaneTx) error {
		for name, err := range map[string]error{
			"User":             absent(tx.User(u.Principal)),
			"Session":          absent(tx.Session(sess.ID)),
			"APIToken":         absent(tx.APIToken(tok.ID)),
			"SessionByRefresh": absent(tx.SessionByRefresh(sess.Refresh)),
			"APITokenByHash":   absent(tx.APITokenByHash(tok.Hash)),
		} {
			if err != nil {
				t.Errorf("%s of a marked user's, read in a write: %v, want absent", name, err)
			}
		}
		return nil
	}))
	if got := all(t, cp.Sessions(ctx, u.Principal)); len(got) != 0 {
		t.Errorf("a marked user's sessions: got %v, want none", got)
	}
	if got := all(t, cp.APITokens(ctx, u.Principal)); len(got) != 0 {
		t.Errorf("a marked user's tokens: got %v, want none", got)
	}
	for name, err := range map[string]error{
		"User":          errOf(p.User(ctx, u.Principal)),
		"ByName":        errOf(p.ByName(ctx, entity.PrincipalUser, "alice")),
		"ByProtocolID":  errOf(p.ByProtocolID(ctx, uid("1000"))),
		"PutUser":       errOf(cp.PutUser(ctx, u, userGen, entity.Alloc{})),
		"DeleteUser":    cp.DeleteUser(ctx, u.Principal, userGen),
		"SetQuota":      errOf(cp.SetQuota(ctx, sh.ID, entity.Quota{Principal: u.Principal, Hard: entity.Limit{Files: 1}}, 0)),
		"PutAPIToken":   cp.PutAPIToken(ctx, entity.APIToken{ID: uuid.New(), Account: u.Principal, Hash: [32]byte{3}, Expires: e}),
		"PutSession":    cp.PutSession(ctx, entity.Session{ID: uuid.New(), Account: u.Principal, Refresh: [32]byte{4}, Expires: e}),
		"UpdateSession": cp.UpdateSession(ctx, sess),
		"PutSignInFailures": cp.Write(ctx, entity.WriteMeta{}, func(tx entity.ControlPlaneTx) error {
			return tx.PutSignInFailures(entity.SignInKey{Account: u.Principal}, entity.SignInFailures{Count: 1})
		}),
	} {
		if !merrs.IsNotFoundError(err) {
			t.Errorf("%s naming a marked user: got %v, want not found", name, err)
		}
	}
	must(t, deleted(s, nil, u.Principal))
}

// absent is nil for a read that found nothing.
func absent[T any](_ T, found bool, err error) error {
	if err == nil && found {
		err = errors.New("found")
	}
	return err
}

// ContinueDelete ends a deletion whose record is gone, as after its last
// batch, and refuses a principal no deletion marked.
func testContinueDeleteEnds(t *testing.T, kvo Opener) {
	s, _ := newStore(t, kvo)
	cp, n := s.ControlPlane(), s.Nodes()
	if done, err := n.ContinueDelete(ctx, entity.NewUser()); err != nil || !done {
		t.Errorf("ContinueDelete on a user with no record: got done %v, %v; want done", done, err)
	}
	u := user("alice")
	must(t, errOf(cp.PutUser(ctx, u, 0, entity.Alloc{})))
	if done, err := n.ContinueDelete(ctx, u.Principal); err == nil || done {
		t.Errorf("ContinueDelete on an unmarked user: got done %v, %v; want refused", done, err)
	}
}
