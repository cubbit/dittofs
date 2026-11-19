package kvstore

import (
	"context"
	"testing"

	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/metadata/kv/keys"
)

// The control plane's reads of one record, in a write's transaction and in
// the view, find what is stored, with the version a write expects, and
// nothing for a record absent, marked deleted or of another kind.
func TestControlPlaneReadsOneRecord(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)
	cp := s.ControlPlane()
	u, g, gone := entity.NewUser(), entity.NewGroup(), entity.NewGroup()
	share := entity.NewShareID()
	ugen, err := cp.PutUser(ctx, entity.User{Principal: u, Name: "alice"}, 0, entity.Alloc{})
	if err != nil {
		t.Fatalf("put user: %v", err)
	}
	r := entity.IDRange{Lo: 1_000_000, Hi: 1_999_999}
	ggen, err := cp.PutGroup(ctx, entity.Group{Principal: g, Name: "staff"}, 0, entity.Alloc{GID: true, RID: true, Range: r})
	if err != nil {
		t.Fatalf("put group: %v", err)
	}
	ngen, err := cp.PutGroup(ctx, entity.Group{Principal: gone, Name: "gone"}, 0, entity.Alloc{GID: true, RID: true, Range: r})
	if err != nil {
		t.Fatalf("put group gone: %v", err)
	}
	if err := cp.DeleteGroup(ctx, gone, ngen); err != nil {
		t.Fatalf("mark group gone deleted: %v", err)
	}
	if err := cp.AddMember(ctx, entity.Membership{Group: g, Member: u}); err != nil {
		t.Fatalf("add member: %v", err)
	}
	if _, err := cp.PutNetgroup(ctx, entity.Netgroup{Name: "labs", Members: []string{"10.0.0.0/8"}}, 0); err != nil {
		t.Fatalf("put netgroup: %v", err)
	}
	grant := entity.ShareGrant{Share: share, Principal: u, Access: entity.AccessNone, Version: 7}
	suffix, _ := quotaSuffix(QuotaTarget{Principal: u})
	quota := entity.Quota{Share: share, Principal: u, Hard: entity.Limit{Bytes: 10}, Version: 4}
	rows(t, s, grantKey(share, u.ID), encodeGrant(grant), quotaKey(share, keys.SQt, suffix), encodeQuota(quota))

	if got, ok, err := cp.User(ctx, u); err != nil || !ok || got.Generation != ugen || got.Name != "alice" {
		t.Errorf("view User(alice) = %+v, %v, %v; want generation %d", got, ok, err, ugen)
	}
	if _, ok, err := cp.User(ctx, entity.Principal{Kind: entity.PrincipalMachine, ID: u.ID}); err != nil || ok {
		t.Errorf("view User(alice as a machine) found %v, %v; want not found", ok, err)
	}
	if got, ok, err := cp.Group(ctx, g); err != nil || !ok || got.Generation != ggen {
		t.Errorf("view Group(staff) = %+v, %v, %v; want generation %d", got, ok, err, ggen)
	}
	if _, ok, err := cp.Group(ctx, gone); err != nil || ok {
		t.Errorf("view Group(gone) found %v, %v; want a marked group not found", ok, err)
	}
	if got, ok, err := cp.Netgroup(ctx, "labs"); err != nil || !ok || got.Version == 0 || len(got.Members) != 1 {
		t.Errorf("view Netgroup(labs) = %+v, %v, %v; want it with its member and a version", got, ok, err)
	}
	if _, ok, err := cp.Netgroup(ctx, "none"); err != nil || ok {
		t.Errorf("view Netgroup(none) found %v, %v; want not found", ok, err)
	}

	err = cp.Write(ctx, entity.WriteMeta{}, func(tx entity.ControlPlaneTx) error {
		if got, ok, err := tx.Group(g); err != nil || !ok || got.Generation != ggen {
			t.Errorf("tx Group(staff) = %+v, %v, %v; want generation %d", got, ok, err, ggen)
		}
		for _, p := range []entity.Principal{gone, u} {
			if _, ok, err := tx.Group(p); err != nil || ok {
				t.Errorf("tx Group(%v) found %v, %v; want not found", p, ok, err)
			}
		}
		if got, ok, err := tx.Grant(share, u); err != nil || !ok || got.Version != 7 || got.Access != entity.AccessNone {
			t.Errorf("tx Grant(alice) = %+v, %v, %v; want the none grant at version 7", got, ok, err)
		}
		if _, ok, err := tx.Grant(share, g); err != nil || ok {
			t.Errorf("tx Grant(staff) found %v, %v; want no grant", ok, err)
		}
		if ok, err := tx.Member(entity.Membership{Group: g, Member: u}); err != nil || !ok {
			t.Errorf("tx Member(alice in staff) = %v, %v; want true", ok, err)
		}
		if ok, err := tx.Member(entity.Membership{Group: gone, Member: u}); err != nil || ok {
			t.Errorf("tx Member(alice in gone) = %v, %v; want false", ok, err)
		}
		if got, ok, err := tx.Netgroup("labs"); err != nil || !ok || got.Version == 0 {
			t.Errorf("tx Netgroup(labs) = %+v, %v, %v; want it", got, ok, err)
		}
		if _, ok, err := tx.Netgroup("none"); err != nil || ok {
			t.Errorf("tx Netgroup(none) found %v, %v; want not found", ok, err)
		}
		if got, ok, err := tx.Quota(share, u); err != nil || !ok || got.Version != 4 || got.Hard.Bytes != 10 {
			t.Errorf("tx Quota(alice) = %+v, %v, %v; want the hard limit of 10 bytes at version 4", got, ok, err)
		}
		if _, ok, err := tx.Quota(share, g); err != nil || ok {
			t.Errorf("tx Quota(staff) found %v, %v; want none", ok, err)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
}
