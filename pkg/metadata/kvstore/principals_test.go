package kvstore

import (
	"context"
	"github.com/google/uuid"
	"slices"
	"testing"

	"github.com/cubbit/dittofs/pkg/metadata/entity"
	merrs "github.com/cubbit/dittofs/pkg/metadata/errors"
	"github.com/cubbit/dittofs/pkg/metadata/kv"
	"github.com/cubbit/dittofs/pkg/metadata/kv/keys"
)

func openStore(t *testing.T) *Store {
	t.Helper()
	db := memKV(t)
	if err := Init(context.Background(), db, uuid.New()); err != nil {
		t.Fatal(err)
	}
	s, err := Open(context.Background(), db)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// rows writes raw records, standing in for the control plane's writers.
func rows(t *testing.T, s *Store, kvs ...[]byte) {
	t.Helper()
	set := func(tx kv.Txn) error {
		for i := 0; i < len(kvs); i += 2 {
			if err := tx.Set(kvs[i], kvs[i+1]); err != nil {
				return err
			}
		}
		return nil
	}
	if err := s.kv.Update(context.Background(), set); err != nil {
		t.Fatal(err)
	}
}

// member is m's membership of g, with g's record: a membership of a group
// that has none grants nothing.
func member(g, m entity.Principal) [][]byte {
	return [][]byte{memberKey(g.ID, m.ID), encodeKind(m.Kind), memberRevKey(m.ID, g.ID), encodeKind(m.Kind),
		groupKey(g.ID), encodeGroup(entity.Group{Principal: g})}
}

func grantRow(share entity.ShareID, p entity.Principal, a entity.ShareAccess) [][]byte {
	return [][]byte{grantKey(share, p.ID), encodeGrant(entity.ShareGrant{Principal: p, Access: a})}
}

func collectGroups(t *testing.T, s *Store, p entity.Principal) []entity.Principal {
	t.Helper()
	var out []entity.Principal
	for g, err := range s.Principals().Groups(context.Background(), p) {
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, g)
	}
	return out
}

func TestGroupsTransitiveWithCycle(t *testing.T) {
	s := openStore(t)
	u, g1, g2, g3 := entity.NewUser(), entity.NewGroup(), entity.NewGroup(), entity.NewGroup()
	// u in g1, g1 in g2, g2 in g1 (a cycle), g3 unrelated.
	rows(t, s, slices.Concat(member(g1, u), member(g2, g1), member(g1, g2), member(g3, entity.NewUser()))...)
	got := collectGroups(t, s, u)
	if !slices.Equal(got, []entity.Principal{g1, g2}) {
		t.Fatalf("Groups(u) = %v, want [%v %v]", got, g1, g2)
	}
}

// Grant answers the grant naming the principal itself, never one reached
// through its groups, and tells a stored none from no grant.
func TestGrantIsTheOwnGrantAlone(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)
	share := entity.NewShareID()
	u, v, g1 := entity.NewUser(), entity.NewUser(), entity.NewGroup()
	rows(t, s, slices.Concat(
		member(g1, u), member(g1, v),
		grantRow(share, g1, entity.AccessReadWrite),
		grantRow(share, v, entity.AccessNone),
	)...)
	for _, c := range []struct {
		name   string
		share  entity.ShareID
		p      entity.Principal
		want   entity.ShareAccess
		wantOK bool
	}{
		{"a group's grant", share, g1, entity.AccessReadWrite, true},
		{"a member with no grant of its own", share, u, entity.AccessNone, false},
		{"an own grant of none", share, v, entity.AccessNone, true},
		{"another share", entity.NewShareID(), g1, entity.AccessNone, false},
	} {
		got, ok, err := s.Principals().Grant(ctx, c.share, c.p)
		if err != nil || got != c.want || ok != c.wantOK {
			t.Errorf("Grant of %s = %v, %v, %v; want %v, %v", c.name, got, ok, err, c.want, c.wantOK)
		}
	}
}

func TestLookups(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)
	u, g := entity.NewUser(), entity.NewGroup()
	sidID := entity.ProtocolID{Scheme: entity.SchemeSID, Value: "\x01\x02"}
	rows(t, s,
		nameKey(keys.NXUser, "alice"), encodeNameRow(u.ID, "alice"),
		nameKey(keys.NXGroup, "alice"), encodeNameRow(g.ID, "alice"),
		nameKey(keys.NXGroup, "staff"), encodeNameRow(g.ID, "staff"),
		protocolKey(sidID), encodePrincipal(g),
	)
	p := s.Principals()
	// A user and a group named alice are two principals, told apart by kind.
	if got, err := p.ByName(ctx, entity.PrincipalUser, "alice"); err != nil || got != u {
		t.Errorf("ByName(user, alice) = %v, %v", got, err)
	}
	if got, err := p.ByName(ctx, entity.PrincipalGroup, "alice"); err != nil || got != g {
		t.Errorf("ByName(group, alice) = %v, %v", got, err)
	}
	if got, err := p.ByName(ctx, entity.PrincipalGroup, "staff"); err != nil || got != g {
		t.Errorf("ByName(group, staff) = %v, %v", got, err)
	}
	if _, err := p.ByName(ctx, entity.PrincipalUser, "staff"); !merrs.IsNotFoundError(err) {
		t.Errorf("ByName(user, staff): %v", err)
	}
	if got, err := p.ByProtocolID(ctx, sidID); err != nil || got != g {
		t.Errorf("ByProtocolID(sid) = %v, %v", got, err)
	}
	if _, err := p.ByName(ctx, entity.PrincipalUser, "nobody"); !merrs.IsNotFoundError(err) {
		t.Errorf("ByName(nobody): %v", err)
	}
	if _, err := p.ByProtocolID(ctx, entity.ProtocolID{Scheme: entity.SchemeUID, Value: "7"}); !merrs.IsNotFoundError(err) {
		t.Errorf("ByProtocolID(uid 7): %v", err)
	}
}
