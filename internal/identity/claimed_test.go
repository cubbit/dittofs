package identity_test

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	pid "github.com/cubbit/dittofs/pkg/identity"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/metadata/kv/kvtest"
	"github.com/cubbit/dittofs/pkg/vfs/vfserr"
)

func claim(uid, gid uint32, gids ...uint32) pid.Identity {
	return pid.Identity{Proof: pid.ProofClaimed, Claims: pid.Claims{UID: uid, GID: gid, GIDs: gids}}
}

// A claimed uid 0 maps to root and a claimed gid 0 to root's group: on a
// share squashing nobody it is root, privileged, unauthenticated and no
// admin; on a root-squashed share it acts as the Anonymous with no flag; on
// no share it is mapped alone. A non-root uid claiming gid 0 holds root as
// its group, a root squash leaving it, as RFC 18 §6.2 squashes only a caller
// mapped to root.
func TestClaimedRootByUIDAndGIDZero(t *testing.T) {
	ctx := context.Background()
	r := newResolving(t)
	nobody := entity.User{Principal: entity.NewUser(), Name: "nobody", IDs: fileIDs("60000")}
	r.user(t, nobody, 0)
	alice := entity.User{Principal: entity.NewUser(), Name: "alice", IDs: fileIDs("1000")}
	r.user(t, alice, 0)
	grants := []entity.ShareGrant{grantTo(entity.Everyone, entity.AccessReadWrite)}
	none := entity.SharePolicy{Share: r.sharing(t, grants...), Squash: entity.SquashNone, Anonymous: nobody.Principal}
	rootSq := entity.SharePolicy{Share: r.sharing(t, grants...), Squash: entity.SquashRoot, Anonymous: nobody.Principal}

	c, err := r.srv.Resolve(ctx, claim(0, 0), none, pid.ReadOnlyNotNarrowed)
	if err != nil {
		t.Fatalf("uid 0 on a share squashing nobody: %v", err)
	}
	if c.Mapped != entity.RootPrincipal || c.Principal != entity.RootPrincipal || c.Group != entity.RootPrincipal || !slices.Equal(c.Groups, []entity.Principal{entity.RootPrincipal}) {
		t.Errorf("uid 0, gid 0 unsquashed: Mapped %v, Principal %v, Group %v, Groups %v; want root for each", c.Mapped, c.Principal, c.Group, c.Groups)
	}
	if !c.Privileged || c.Authenticated || c.Admin {
		t.Errorf("uid 0 unsquashed: Privileged %v, Authenticated %v, Admin %v; want true, false, false", c.Privileged, c.Authenticated, c.Admin)
	}

	c, err = r.srv.Resolve(ctx, claim(0, 0), rootSq, pid.ReadOnlyNotNarrowed)
	if err != nil {
		t.Fatalf("uid 0 on a root-squashed share: %v", err)
	}
	if c.Mapped != entity.RootPrincipal || c.Principal != nobody.Principal || !c.Group.IsZero() || c.Privileged || c.Authenticated {
		t.Errorf("uid 0 root-squashed: Mapped %v, Principal %v, Group %v, Privileged %v, Authenticated %v; want root, nobody, none, false, false",
			c.Mapped, c.Principal, c.Group, c.Privileged, c.Authenticated)
	}

	if c, err = r.srv.Resolve(ctx, claim(0, 0), entity.SharePolicy{}, pid.ReadOnlyNotNarrowed); err != nil || c.Mapped != entity.RootPrincipal {
		t.Errorf("uid 0 on no share: %+v, %v; want Mapped root", c, err)
	}

	c, err = r.srv.Resolve(ctx, claim(1000, 0), rootSq, pid.ReadOnlyNotNarrowed)
	if err != nil {
		t.Fatalf("alice claiming gid 0 on a root-squashed share: %v", err)
	}
	if c.Principal != alice.Principal || c.Group != entity.RootPrincipal || c.Privileged {
		t.Errorf("alice claiming gid 0, root-squashed: Principal %v, Group %v, Privileged %v; want alice, root, false", c.Principal, c.Group, c.Privileged)
	}
}

// A claimed uid no principal holds maps to the well-known anonymous before
// any squash and acts as the share's Anonymous, counted under uid; a claimed
// gid no principal holds is in neither Group nor Groups, counted under gid.
// Neither is ever defaulted to a principal.
func TestClaimedUnmappedUIDAndGID(t *testing.T) {
	ctx := context.Background()
	r := newResolving(t)
	nobody := entity.User{Principal: entity.NewUser(), Name: "nobody", IDs: fileIDs("60000")}
	r.user(t, nobody, 0)
	alice := entity.User{Principal: entity.NewUser(), Name: "alice", IDs: fileIDs("1000")}
	r.user(t, alice, 0)
	eng := r.group(t, "eng", "")
	engGID := gidOf(t, r, eng)
	share := entity.SharePolicy{Share: r.sharing(t, grantTo(entity.Everyone, entity.AccessRead)), Squash: entity.SquashNone, Anonymous: nobody.Principal}

	c, err := r.srv.Resolve(ctx, claim(4242, engGID), share, pid.ReadOnlyNotNarrowed)
	if err != nil {
		t.Fatalf("an unmapped uid: %v", err)
	}
	if c.Mapped != entity.AnonymousPrincipal || c.Principal != nobody.Principal || !c.Group.IsZero() || len(c.Groups) != 0 || c.Access != entity.AccessRead {
		t.Errorf("an unmapped uid: Mapped %v, Principal %v, Group %v, Groups %v, Access %v; want anonymous, nobody, none, none, read",
			c.Mapped, c.Principal, c.Group, c.Groups, c.Access)
	}
	if got := r.counter(t, "identity_unmapped_total", "scheme", "uid"); got != 1 {
		t.Errorf("identity_unmapped_total{scheme=uid} = %v, want 1", got)
	}

	c, err = r.srv.Resolve(ctx, claim(1000, 4343, engGID, 4344), share, pid.ReadOnlyNotNarrowed)
	if err != nil {
		t.Fatalf("alice with unmapped gids: %v", err)
	}
	if c.Principal != alice.Principal || !c.Group.IsZero() || !slices.Equal(c.Groups, []entity.Principal{eng}) {
		t.Errorf("alice with an unmapped primary gid: Principal %v, Group %v, Groups %v; want alice, none, [eng]", c.Principal, c.Group, c.Groups)
	}
	if got := r.counter(t, "identity_unmapped_total", "scheme", "gid"); got != 2 {
		t.Errorf("identity_unmapped_total{scheme=gid} = %v, want 2", got)
	}
}

// A claimed uid held by a disabled user is refused ErrAccess, counted
// disabled, on a share and on none; never acted on as anonymous, which
// everyone's read would admit.
func TestClaimedDisabledUserIsRefused(t *testing.T) {
	ctx := context.Background()
	r := newResolving(t)
	bob := entity.User{Principal: entity.NewUser(), Name: "bob", IDs: fileIDs("1001"), Disabled: true}
	r.user(t, bob, 0)
	share := entity.SharePolicy{Share: r.sharing(t, grantTo(entity.Everyone, entity.AccessRead)), Squash: entity.SquashAll, Anonymous: entity.AnonymousPrincipal}
	for i, p := range []entity.SharePolicy{share, {}} {
		if c, err := r.srv.Resolve(ctx, claim(1001, 1001), p, pid.ReadOnlyNotNarrowed); !errors.Is(err, vfserr.ErrAccess) || c != nil {
			t.Errorf("disabled bob's uid on share %v = %+v, %v; want nil, ErrAccess", p.Share, c, err)
		}
		if got := r.refusals(t, "disabled"); got != float64(i+1) {
			t.Errorf("refusals{disabled} = %v, want %d", got, i+1)
		}
	}
}

// A claimed call naming no share runs the map step alone: Mapped is the
// uid's holder, and no gid, group or grant is read, which all fail here.
func TestClaimedOnNoShareMapsAlone(t *testing.T) {
	r := newResolving(t)
	alice := entity.User{Principal: entity.NewUser(), Name: "alice", IDs: fileIDs("1000")}
	r.user(t, alice, 0)
	r.fail(t, "*", "G*", "M*", "PX‖02*")
	c, err := r.srv.Resolve(context.Background(), claim(1000, 1000, 1001), entity.SharePolicy{}, pid.ReadOnlyNotNarrowed)
	if err != nil {
		t.Fatalf("a claim on no share: %v", err)
	}
	if c.Mapped != alice.Principal || !c.Principal.IsZero() || !c.Group.IsZero() || c.Groups != nil || c.Access != entity.AccessNone || c.Privileged {
		t.Errorf("a claim on no share = %+v; want Mapped alice and nothing else", *c)
	}
}

// A store error on the uid index, for a held uid and an unmapped one, or on
// the gid index, is ErrUnavailable, counted store_unavailable: never "no such
// principal", never anonymous, though everyone's read-write would admit
// anonymous.
func TestClaimedStoreErrorIsUnavailable(t *testing.T) {
	ctx := context.Background()
	for key, uids := range map[string][]uint32{"PX‖01*": {1000, 4242}, "PX‖02*": {1000}} {
		t.Run(key, func(t *testing.T) {
			r := newResolving(t)
			alice := entity.User{Principal: entity.NewUser(), Name: "alice", IDs: fileIDs("1000")}
			r.user(t, alice, 0)
			share := entity.SharePolicy{Share: r.sharing(t, grantTo(entity.Everyone, entity.AccessReadWrite)), Squash: entity.SquashNone, Anonymous: entity.AnonymousPrincipal}
			r.fail(t, kvtest.OpGet, key)
			for _, uid := range uids {
				if c, err := r.srv.Resolve(ctx, claim(uid, 1000), share, pid.ReadOnlyNotNarrowed); !errors.Is(err, vfserr.ErrUnavailable) || c != nil {
					t.Errorf("uid %d with %s failing = %+v, %v; want nil, ErrUnavailable", uid, key, c, err)
				}
			}
			if got := r.refusals(t, "store_unavailable"); got != float64(len(uids)) {
				t.Errorf("refusals{store_unavailable} = %v, want %d", got, len(uids))
			}
		})
	}
}

// On an all-squash share whose grants to alice, to its Anonymous and to
// everyone all differ, a claim of alice's uid gets the Anonymous's grant;
// with that grant removed, everyone's; never alice's admin. It is never
// authenticated, admin or privileged.
func TestClaimedSquashedTakesTheAnonymousGrant(t *testing.T) {
	ctx := context.Background()
	r := newResolving(t)
	nobody := entity.User{Principal: entity.NewUser(), Name: "nobody", IDs: fileIDs("60000")}
	r.user(t, nobody, 0)
	alice := entity.User{Principal: entity.NewUser(), Name: "alice", IDs: fileIDs("1000")}
	r.user(t, alice, 0)
	share := entity.SharePolicy{Squash: entity.SquashAll, Anonymous: nobody.Principal,
		Share: r.sharing(t, grantTo(alice.Principal, entity.AccessAdmin), grantTo(nobody.Principal, entity.AccessRead), grantTo(entity.Everyone, entity.AccessReadWrite))}
	check := func(when string, want entity.ShareAccess) {
		t.Helper()
		c, err := r.srv.Resolve(ctx, claim(1000, 1000), share, pid.ReadOnlyNotNarrowed)
		if err != nil {
			t.Fatalf("alice's claim %s: %v", when, err)
		}
		if c.Access != want || c.Principal != nobody.Principal || c.Authenticated || c.Admin || c.Privileged {
			t.Errorf("alice's claim %s: Access %v, Principal %v, flags %v %v %v; want %v as nobody, no flag",
				when, c.Access, c.Principal, c.Authenticated, c.Admin, c.Privileged, want)
		}
	}
	check("with the Anonymous granted read", entity.AccessRead)
	g, _, err := r.cp.ShareGrant(ctx, share.Share, nobody.Principal)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.cp.DeleteGrant(ctx, share.Share, nobody.Principal, g.Version); err != nil {
		t.Fatalf("DeleteGrant: %v", err)
	}
	check("with the Anonymous's grant removed", entity.AccessReadWrite)
}

// A claimed uid of a minted directory principal goes through the directory
// step: its caller is fresh for the TTL, and once the TTL has passed with
// the account disabled in the directory it is refused ErrAccess.
func TestClaimedDirectoryPrincipalFollowsTheDirectory(t *testing.T) {
	e := newAD(t)
	bobDN, bob := e.minted(t, "bob", nil)
	sp, err := e.s.Spell(context.Background(), bob, pid.SchemeUID)
	if err != nil {
		t.Fatalf("Spell(bob, uid): %v", err)
	}
	resolve := func() (*pid.Caller, error) {
		return e.s.Resolve(context.Background(), claim(sp.Num, sp.Num), fileShare(), pid.ReadOnlyNotNarrowed)
	}
	t0 := e.clock.Now()
	c, err := resolve()
	if err != nil {
		t.Fatalf("bob's claimed uid: %v", err)
	}
	if c.Principal != bob || c.Authenticated || !c.Fresh.Equal(t0.Add(5*time.Minute)) {
		t.Errorf("bob's claimed caller: Principal %v, Authenticated %v, Fresh %v; want bob, false, %v", c.Principal, c.Authenticated, c.Fresh, t0.Add(5*time.Minute))
	}
	e.d.Modify(bobDN, map[string][][]byte{"userAccountControl": attr("514")})
	e.clock.Advance(5*time.Minute + time.Second)
	if c, err := resolve(); !errors.Is(err, vfserr.ErrAccess) || c != nil {
		t.Errorf("bob's claimed uid, disabled in the directory past the TTL = %+v, %v; want nil, ErrAccess", c, err)
	}
}
