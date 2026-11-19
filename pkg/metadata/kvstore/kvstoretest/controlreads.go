package kvstoretest

import (
	"slices"
	"testing"

	"github.com/cubbit/dittofs/pkg/metadata/entity"
)

// The control plane's reads of one record, in a write's transaction and in
// the view, find what is stored, with the version a write expects, and
// nothing for a record absent, marked deleted or of another kind; the
// listings of a share's grants and principal quotas resume after a cursor.
func testControlPlaneReads(t *testing.T, kvo Opener) {
	s, _ := newStore(t, kvo)
	cp := s.ControlPlane()
	u, g, gone := user("alice"), group("staff"), group("gone")
	sh := share("photos", "/photos")
	ugen, err := cp.PutUser(ctx, u, 0, entity.Alloc{})
	must(t, err)
	ggen, err := cp.PutGroup(ctx, g, 0, entity.Alloc{})
	must(t, err)
	ngen, err := cp.PutGroup(ctx, gone, 0, entity.Alloc{})
	must(t, err)
	must(t, cp.DeleteGroup(ctx, gone.Principal, ngen))
	must(t, cp.AddMember(ctx, entity.Membership{Group: g.Principal, Member: u.Principal}))
	must(t, errOf(cp.PutNetgroup(ctx, entity.Netgroup{Name: "labs", Members: []string{"10.0.0.0/8"}}, 0)))
	must(t, errOf(CreateShare(ctx, cp, sh)))
	gv, err := cp.SetGrant(ctx, entity.ShareGrant{Share: sh.ID, Principal: u.Principal, Access: entity.AccessNone}, 0)
	must(t, err)
	must(t, errOf(cp.SetGrant(ctx, entity.ShareGrant{Share: sh.ID, Principal: g.Principal, Access: entity.AccessRead}, 0)))
	qv, err := cp.SetQuota(ctx, sh.ID, entity.Quota{Principal: u.Principal, Hard: entity.Limit{Bytes: 10}}, 0)
	must(t, err)
	must(t, errOf(cp.SetQuota(ctx, sh.ID, entity.Quota{Principal: g.Principal, Hard: entity.Limit{Files: 3}}, 0)))
	sv, err := cp.SetQuota(ctx, sh.ID, entity.Quota{Hard: entity.Limit{Files: 9}}, 0)
	must(t, err)
	must(t, errOf(cp.SetQuota(ctx, sh.ID, entity.Quota{Project: 4, Hard: entity.Limit{Files: 1}}, 0)))

	if got, ok, err := cp.User(ctx, u.Principal); err != nil || !ok || got.Generation != ugen || got.Name != "alice" {
		t.Errorf("view User(alice) = %+v, %v, %v; want generation %d", got, ok, err, ugen)
	}
	if _, ok, err := cp.User(ctx, entity.Principal{Kind: entity.PrincipalMachine, ID: u.Principal.ID}); err != nil || ok {
		t.Errorf("view User(alice as a machine) found %v, %v; want not found", ok, err)
	}
	if got, ok, err := cp.UserNamed(ctx, "ALICE"); err != nil || !ok || got.Principal != u.Principal {
		t.Errorf("view UserNamed(ALICE) = %+v, %v, %v; want alice, the name folded", got, ok, err)
	}
	if got, ok, err := cp.Group(ctx, g.Principal); err != nil || !ok || got.Generation != ggen {
		t.Errorf("view Group(staff) = %+v, %v, %v; want generation %d", got, ok, err, ggen)
	}
	if got, ok, err := cp.GroupNamed(ctx, "staff"); err != nil || !ok || got.Principal != g.Principal {
		t.Errorf("view GroupNamed(staff) = %+v, %v, %v; want staff", got, ok, err)
	}
	for _, name := range []string{"gone", "nobody"} {
		if _, ok, err := cp.GroupNamed(ctx, name); err != nil || ok {
			t.Errorf("view GroupNamed(%s) found %v, %v; want not found", name, ok, err)
		}
	}
	if _, ok, err := cp.Group(ctx, gone.Principal); err != nil || ok {
		t.Errorf("view Group(gone) found %v, %v; want a marked group not found", ok, err)
	}
	if got, ok, err := cp.Netgroup(ctx, "labs"); err != nil || !ok || got.Version == 0 || len(got.Members) != 1 {
		t.Errorf("view Netgroup(labs) = %+v, %v, %v; want it with its member and a version", got, ok, err)
	}
	if _, ok, err := cp.Netgroup(ctx, "none"); err != nil || ok {
		t.Errorf("view Netgroup(none) found %v, %v; want not found", ok, err)
	}
	if q, ok, err := cp.Quota(ctx, sh.ID, u.Principal); err != nil || !ok || q.Version != qv || q.Hard.Bytes != 10 {
		t.Errorf("view Quota(alice) = %+v, %v, %v; want 10 bytes at version %d", q, ok, err, qv)
	}
	if q, ok, err := cp.Quota(ctx, sh.ID, entity.Principal{}); err != nil || !ok || q.Version != sv {
		t.Errorf("view Quota(the share) = %+v, %v, %v; want version %d", q, ok, err, sv)
	}

	byID := func(a, b entity.Principal) int { return slices.Compare(a.ID[:], b.ID[:]) }
	want := []entity.Principal{u.Principal, g.Principal}
	slices.SortFunc(want, byID)
	var grants, quotas []entity.Principal
	for gr, err := range cp.ShareGrants(ctx, sh.ID, entity.PrincipalID{}) {
		must(t, err)
		grants = append(grants, gr.Principal)
	}
	for q, err := range cp.PrincipalQuotas(ctx, sh.ID, entity.PrincipalID{}) {
		must(t, err)
		quotas = append(quotas, q.Principal)
	}
	if !slices.Equal(grants, want) || !slices.Equal(quotas, want) {
		t.Errorf("ShareGrants %v, PrincipalQuotas %v; want each %v in ID order, no share or project quota", grants, quotas, want)
	}
	rest := all(t, cp.ShareGrants(ctx, sh.ID, want[0].ID))
	restQ := all(t, cp.PrincipalQuotas(ctx, sh.ID, want[0].ID))
	if len(rest) != 1 || rest[0].Principal != want[1] || len(restQ) != 1 || restQ[0].Principal != want[1] {
		t.Errorf("after %v: grants %+v, quotas %+v; want %v's alone", want[0], rest, restQ, want[1])
	}

	err = cp.Write(ctx, entity.WriteMeta{}, func(tx entity.ControlPlaneTx) error {
		if got, ok, err := tx.Group(g.Principal); err != nil || !ok || got.Generation != ggen {
			t.Errorf("tx Group(staff) = %+v, %v, %v; want generation %d", got, ok, err, ggen)
		}
		for _, p := range []entity.Principal{gone.Principal, u.Principal} {
			if _, ok, err := tx.Group(p); err != nil || ok {
				t.Errorf("tx Group(%v) found %v, %v; want not found", p, ok, err)
			}
		}
		if got, ok, err := tx.Grant(sh.ID, u.Principal); err != nil || !ok || got.Version != gv || got.Access != entity.AccessNone {
			t.Errorf("tx Grant(alice) = %+v, %v, %v; want the none grant at version %d", got, ok, err, gv)
		}
		if _, ok, err := tx.Grant(sh.ID, gone.Principal); err != nil || ok {
			t.Errorf("tx Grant(gone) found %v, %v; want no grant", ok, err)
		}
		if ok, err := tx.Member(entity.Membership{Group: g.Principal, Member: u.Principal}); err != nil || !ok {
			t.Errorf("tx Member(alice in staff) = %v, %v; want true", ok, err)
		}
		if ok, err := tx.Member(entity.Membership{Group: gone.Principal, Member: u.Principal}); err != nil || ok {
			t.Errorf("tx Member(alice in gone) = %v, %v; want false", ok, err)
		}
		if got, ok, err := tx.Netgroup("labs"); err != nil || !ok || got.Version == 0 {
			t.Errorf("tx Netgroup(labs) = %+v, %v, %v; want it", got, ok, err)
		}
		if _, ok, err := tx.Netgroup("none"); err != nil || ok {
			t.Errorf("tx Netgroup(none) found %v, %v; want not found", ok, err)
		}
		if got, ok, err := tx.Quota(sh.ID, u.Principal); err != nil || !ok || got.Version != qv {
			t.Errorf("tx Quota(alice) = %+v, %v, %v; want version %d", got, ok, err, qv)
		}
		if got, ok, err := tx.Quota(sh.ID, entity.Principal{}); err != nil || !ok || got.Version != sv {
			t.Errorf("tx Quota(the share) = %+v, %v, %v; want version %d", got, ok, err, sv)
		}
		if _, ok, err := tx.Quota(sh.ID, gone.Principal); err != nil || ok {
			t.Errorf("tx Quota(gone) found %v, %v; want none", ok, err)
		}
		return nil
	})
	must(t, err)
}
