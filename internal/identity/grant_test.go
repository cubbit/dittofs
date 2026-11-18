package identity_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/auth/sid"
	pid "github.com/cubbit/dittofs/pkg/identity"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/vfs/vfserr"
)

// sharing stores a share in a namespace of its own holding grants, and
// returns its ID.
func (r *resolving) sharing(t *testing.T, grants ...entity.ShareGrant) entity.ShareID {
	t.Helper()
	ctx := context.Background()
	ns := entity.Namespace{ID: entity.NewNamespaceID(), Store: [16]byte{15: 1}}
	if _, err := r.cp.PutNamespace(ctx, ns, 0); err != nil {
		t.Fatalf("PutNamespace: %v", err)
	}
	id := entity.NewShareID()
	sh := entity.Share{ID: id, Name: "s" + id.String()[:8], Path: "/s" + id.String()[:8], Root: entity.NewFileID(), Namespaces: []entity.NamespaceGen{{Generation: 1, Namespace: ns.ID}}}
	if _, err := r.cp.PutShare(ctx, sh, 0); err != nil {
		t.Fatalf("PutShare: %v", err)
	}
	for _, g := range grants {
		g.Share = id
		if _, err := r.cp.SetGrant(ctx, g, 0); err != nil {
			t.Fatalf("SetGrant %v to %v: %v", g.Access, g.Principal, err)
		}
	}
	return id
}

func grantTo(p entity.Principal, a entity.ShareAccess) entity.ShareGrant {
	return entity.ShareGrant{Principal: p, Access: a}
}

// RFC 18 §10's Unit row: resolution's five steps over proof kind, squash,
// grant, admission's narrowing and a principal with no file spelling,
// asserting every Caller field. alice is a file user in Readers and
// Writers; m is a machine principal, with no uid, in the same two groups;
// the Anonymous nobody is in Guests, which no grant names. Each case
// resolves under every protection alike.
func TestResolveTable(t *testing.T) {
	ctx := context.Background()
	r := newResolving(t)
	nobody := entity.User{Principal: entity.NewUser(), Name: "nobody", IDs: fileIDs("65534")}
	r.user(t, nobody, 0)
	guests := r.group(t, "Guests", "")
	r.member(t, guests, nobody.Principal)
	alice := entity.User{Principal: entity.NewUser(), Name: "alice", IDs: fileIDs("1000")}
	r.user(t, alice, 0)
	m := entity.User{Principal: entity.NewUser(), Name: "build01$"}
	r.user(t, m, 0)
	readers, writers := r.group(t, "Readers", ""), r.group(t, "Writers", "")
	for _, u := range []entity.Principal{alice.Principal, m.Principal} {
		r.member(t, readers, u)
		r.member(t, writers, u)
	}

	type access = entity.ShareAccess
	const none, read, rw, admin = entity.AccessNone, entity.AccessRead, entity.AccessReadWrite, entity.AccessAdmin
	// proven is what alice, signed in, finds; anon what nobody finds,
	// unauthenticated.
	grants := []struct {
		name         string
		grants       []entity.ShareGrant
		proven, anon access
	}{
		{"own read", []entity.ShareGrant{grantTo(alice.Principal, read)}, read, none},
		{"own read-write", []entity.ShareGrant{grantTo(alice.Principal, rw)}, rw, none},
		{"own admin", []entity.ShareGrant{grantTo(alice.Principal, admin)}, admin, none},
		{"own none under everyone's read-write", []entity.ShareGrant{grantTo(alice.Principal, none), grantTo(entity.Everyone, rw)}, none, rw},
		{"two groups, read and read-write", []entity.ShareGrant{grantTo(readers, read), grantTo(writers, rw)}, rw, none},
		{"two groups, read-write and read", []entity.ShareGrant{grantTo(readers, rw), grantTo(writers, read)}, rw, none},
		{"a group's read under authenticated users' read-write", []entity.ShareGrant{grantTo(readers, read), grantTo(entity.AuthenticatedUsers, rw)}, read, none},
		{"authenticated users' read", []entity.ShareGrant{grantTo(entity.AuthenticatedUsers, read)}, read, none},
		{"authenticated users' read-write", []entity.ShareGrant{grantTo(entity.AuthenticatedUsers, rw)}, rw, none},
		{"authenticated users' read under everyone's read-write", []entity.ShareGrant{grantTo(entity.AuthenticatedUsers, read), grantTo(entity.Everyone, rw)}, read, rw},
		{"everyone's read", []entity.ShareGrant{grantTo(entity.Everyone, read)}, read, read},
		{"everyone's read-write", []entity.ShareGrant{grantTo(entity.Everyone, rw)}, rw, rw},
		{"anonymous's own read", []entity.ShareGrant{grantTo(nobody.Principal, read)}, none, read},
		{"none", nil, none, none},
	}
	expires := time.Unix(1_900_000_000, 0)
	for _, g := range grants {
		share := r.sharing(t, g.grants...)
		for _, proof := range []pid.ProofKind{pid.ProofNone, pid.ProofSecret, pid.ProofTicket} {
			for _, noFile := range []bool{false, true} {
				if proof == pid.ProofNone && noFile {
					continue // a None caller names no principal at all
				}
				for _, squash := range []entity.Squash{entity.SquashNone, entity.SquashAll} {
					for _, narrowed := range []pid.ReadOnlyCause{pid.ReadOnlyNotNarrowed, pid.ReadOnlyShare, pid.ReadOnlyClient} {
						name := fmt.Sprintf("%s/proof %d/no file spelling %v/squash %v/narrowed %d", g.name, proof, noFile, squash, narrowed)
						id := pid.Identity{Proof: proof}
						if proof >= pid.ProofSecret {
							who := alice
							if noFile {
								who = m
							}
							id.Evidence = pid.Evidence{Name: who.Name, Principal: who.Principal, Expires: expires}
						}
						policy := entity.SharePolicy{Share: share, Squash: squash, Anonymous: nobody.Principal}

						// What §6.1 to §6.5 say this caller is.
						asItself := proof >= pid.ProofSecret && !noFile
						want := pid.Caller{Mapped: nobody.Principal, Principal: nobody.Principal, Groups: []entity.Principal{guests}, Access: g.anon, Fresh: id.Evidence.Expires}
						if proof >= pid.ProofSecret {
							want.Mapped = id.Evidence.Principal
						}
						if asItself {
							want.Access = g.proven
						}
						if asItself && squash == entity.SquashNone {
							want.Principal, want.Groups = alice.Principal, sorted(readers, writers)
							want.Authenticated = true
							want.Admin = g.proven == admin
							want.Privileged = want.Admin
						}
						refused := asItself && squash == entity.SquashAll && g.proven == none
						switch {
						case want.Access == none:
						case narrowed != pid.ReadOnlyNotNarrowed:
							want.Access, want.ReadOnly = read, narrowed
						case want.Access == read:
							want.ReadOnly = pid.ReadOnlyGrant
						}

						for _, prot := range []pid.Protection{pid.ProtectionNone, pid.ProtectionIntegrity, pid.ProtectionPrivacy} {
							id.Protection = prot
							c, err := r.srv.Resolve(ctx, id, policy, narrowed)
							if refused {
								if !errors.Is(err, vfserr.ErrAccess) || c != nil {
									t.Errorf("%s/protection %d: Resolve = %+v, %v; want nil, ErrAccess", name, prot, c, err)
								}
								continue
							}
							if err != nil {
								t.Errorf("%s/protection %d: Resolve: %v", name, prot, err)
								continue
							}
							want.Built = r.srv.Current()
							if !callersEqual(*c, want) {
								t.Errorf("%s/protection %d:\n got %+v\nwant %+v", name, prot, *c, want)
							}
						}
					}
				}
			}
		}
	}
}

func callersEqual(a, b pid.Caller) bool {
	return a.Principal == b.Principal && a.Mapped == b.Mapped && a.Group == b.Group &&
		slices.Equal(a.Groups, b.Groups) && slices.Equal(a.DenyOnly, b.DenyOnly) &&
		a.Access == b.Access && a.ReadOnly == b.ReadOnly &&
		a.Authenticated == b.Authenticated && a.Admin == b.Admin && a.Privileged == b.Privileged &&
		a.Built == b.Built && a.Fresh.Equal(b.Fresh)
}

// A proven caller's own grant of none decides over a group grant of
// read-write: unsquashed it resolves with Access none, which the store's
// check refuses; squashed it is refused at resolution. Removing the own
// grant lets the group's decide.
func TestResolveOwnNoneOverridesAGroupGrant(t *testing.T) {
	ctx := context.Background()
	r := newResolving(t)
	nobody := entity.User{Principal: entity.NewUser(), Name: "nobody", IDs: fileIDs("65534")}
	r.user(t, nobody, 0)
	alice := entity.User{Principal: entity.NewUser(), Name: "alice", IDs: fileIDs("1000")}
	r.user(t, alice, 0)
	writers := r.group(t, "Writers", "")
	r.member(t, writers, alice.Principal)
	share := r.sharing(t, grantTo(alice.Principal, entity.AccessNone), grantTo(writers, entity.AccessReadWrite))
	id := ticket(alice.Principal, alice.Name)

	c, err := r.srv.Resolve(ctx, id, entity.SharePolicy{Share: share, Squash: entity.SquashNone, Anonymous: nobody.Principal}, pid.ReadOnlyNotNarrowed)
	if err != nil || c.Access != entity.AccessNone {
		t.Errorf("unsquashed under an own none and a group's read-write: Access %v, %v; want none", accessOf(c), err)
	}
	c, err = r.srv.Resolve(ctx, id, entity.SharePolicy{Share: share, Squash: entity.SquashAll, Anonymous: nobody.Principal}, pid.ReadOnlyNotNarrowed)
	if !errors.Is(err, vfserr.ErrAccess) || c != nil {
		t.Errorf("squashed under an own none and a group's read-write = %+v, %v; want nil, ErrAccess", c, err)
	}

	if err := r.cp.DeleteGrant(ctx, share, alice.Principal, grantVersion(t, r, share, alice.Principal)); err != nil {
		t.Fatalf("DeleteGrant: %v", err)
	}
	c, err = r.srv.Resolve(ctx, id, entity.SharePolicy{Share: share, Squash: entity.SquashAll, Anonymous: nobody.Principal}, pid.ReadOnlyNotNarrowed)
	if err != nil || c.Access != entity.AccessReadWrite {
		t.Errorf("squashed after the own none's removal: Access %v, %v; want read-write", accessOf(c), err)
	}
}

func accessOf(c *pid.Caller) any {
	if c == nil {
		return "no caller"
	}
	return c.Access
}

func grantVersion(t *testing.T, r *resolving, share entity.ShareID, p entity.Principal) uint64 {
	t.Helper()
	g, _, err := r.cp.ShareGrant(context.Background(), share, p)
	if err != nil {
		t.Fatalf("ShareGrant: %v", err)
	}
	return g.Version
}

// Admin comes from the grant alone and only for a proven caller: an
// evidence SID S-1-5-32-544 mapping to a group no grant names leaves it
// false; the same caller's own admin grant sets it, and privilege with it;
// a None caller acting as an Anonymous granted admin holds neither; a
// proven user holding uid 0 is privileged without being admin.
func TestResolveAdminComesFromTheGrantAlone(t *testing.T) {
	ctx := context.Background()
	r := newResolving(t)
	nobody := entity.User{Principal: entity.NewUser(), Name: "nobody", IDs: fileIDs("65534")}
	r.user(t, nobody, 0)
	alice := entity.User{Principal: entity.NewUser(), Name: "alice", IDs: fileIDs("1000")}
	r.user(t, alice, 0)
	root := entity.User{Principal: entity.NewUser(), Name: "root", IDs: fileIDs("0")}
	r.user(t, root, 0)
	admins := r.group(t, "Administrators", "S-1-5-32-544")
	id := ticket(alice.Principal, alice.Name)
	id.Evidence.SIDs = []pid.SID{*sid.ParseSIDMust("S-1-5-32-544")}
	policy := func(share entity.ShareID) entity.SharePolicy {
		return entity.SharePolicy{Share: share, Squash: entity.SquashNone, Anonymous: nobody.Principal}
	}

	plain := r.sharing(t, grantTo(alice.Principal, entity.AccessReadWrite))
	c, err := r.srv.Resolve(ctx, id, policy(plain), pid.ReadOnlyNotNarrowed)
	if err != nil {
		t.Fatalf("Resolve with S-1-5-32-544: %v", err)
	}
	if !slices.Contains(c.Groups, admins) || c.Admin || c.Privileged {
		t.Errorf("S-1-5-32-544 with a read-write grant: Groups %v, Admin %v, Privileged %v; want %v listed, not admin, not privileged", c.Groups, c.Admin, c.Privileged, admins)
	}

	granted := r.sharing(t, grantTo(alice.Principal, entity.AccessAdmin), grantTo(nobody.Principal, entity.AccessAdmin), grantTo(root.Principal, entity.AccessRead))
	c, err = r.srv.Resolve(ctx, id, policy(granted), pid.ReadOnlyNotNarrowed)
	if err != nil || !c.Admin || !c.Privileged || c.Access != entity.AccessAdmin {
		t.Errorf("an own admin grant: %+v, %v; want admin, privileged, Access admin", c, err)
	}
	c, err = r.srv.Resolve(ctx, pid.Identity{Proof: pid.ProofNone}, policy(granted), pid.ReadOnlyNotNarrowed)
	if err != nil || c.Admin || c.Privileged || c.Authenticated || c.Access != entity.AccessAdmin {
		t.Errorf("a None caller as an Anonymous granted admin: %+v, %v; want Access admin and no flag", c, err)
	}
	c, err = r.srv.Resolve(ctx, ticket(root.Principal, root.Name), policy(granted), pid.ReadOnlyNotNarrowed)
	if err != nil || c.Admin || !c.Privileged {
		t.Errorf("a proven uid 0 with a read grant: %+v, %v; want privileged, not admin", c, err)
	}
	// A root squash replaces a proven uid 0's principal, groups and flags with
	// Anonymous's, while its Access stays its own grant, read before the squash.
	rootSquash := policy(granted)
	rootSquash.Squash = entity.SquashRoot
	c, err = r.srv.Resolve(ctx, ticket(root.Principal, root.Name), rootSquash, pid.ReadOnlyNotNarrowed)
	if err != nil || c.Principal != nobody.Principal || c.Mapped != root.Principal || c.Admin || c.Privileged || c.Authenticated || c.Access != entity.AccessRead {
		t.Errorf("a proven uid 0 on a root-squash share: %+v, %v; want Principal nobody, Mapped root, Access read, no flag", c, err)
	}
}

// A deny-only principal never matches a share grant: a share whose only
// grant names Finance gives a caller holding Finance deny-only nothing, and
// refuses it squashed, while one holding it as a group is admitted.
func TestResolveDenyOnlyMatchesNoGrant(t *testing.T) {
	ctx := context.Background()
	r := newResolving(t)
	nobody := entity.User{Principal: entity.NewUser(), Name: "nobody", IDs: fileIDs("65534")}
	r.user(t, nobody, 0)
	alice := entity.User{Principal: entity.NewUser(), Name: "alice", IDs: fileIDs("1000")}
	r.user(t, alice, 0)
	finance := r.group(t, "Finance", "S-1-5-21-1-2-3-2001")
	share := r.sharing(t, grantTo(finance, entity.AccessReadWrite))
	financeSID := *sid.ParseSIDMust("S-1-5-21-1-2-3-2001")

	denied := ticket(alice.Principal, alice.Name)
	denied.Evidence.DenyOnly = []pid.SID{financeSID}
	c, err := r.srv.Resolve(ctx, denied, entity.SharePolicy{Share: share, Squash: entity.SquashNone, Anonymous: nobody.Principal}, pid.ReadOnlyNotNarrowed)
	if err != nil || c.Access != entity.AccessNone || !slices.Equal(c.DenyOnly, []entity.Principal{finance}) {
		t.Errorf("Finance deny-only: %+v, %v; want Access none, DenyOnly [%v]", c, err, finance)
	}
	c, err = r.srv.Resolve(ctx, denied, entity.SharePolicy{Share: share, Squash: entity.SquashAll, Anonymous: nobody.Principal}, pid.ReadOnlyNotNarrowed)
	if !errors.Is(err, vfserr.ErrAccess) || c != nil {
		t.Errorf("Finance deny-only, squashed = %+v, %v; want nil, ErrAccess", c, err)
	}

	member := ticket(alice.Principal, alice.Name)
	member.Evidence.SIDs = []pid.SID{financeSID}
	c, err = r.srv.Resolve(ctx, member, entity.SharePolicy{Share: share, Squash: entity.SquashNone, Anonymous: nobody.Principal}, pid.ReadOnlyNotNarrowed)
	if err != nil || c.Access != entity.AccessReadWrite {
		t.Errorf("Finance as a group: Access %v, %v; want read-write", accessOf(c), err)
	}
}
