package kvstoretest

import (
	"errors"
	"testing"

	"github.com/cubbit/dittofs/pkg/metadata/entity"
	merrs "github.com/cubbit/dittofs/pkg/metadata/errors"
	"github.com/cubbit/dittofs/pkg/metadata/kvstore"
)

// versioned is one control-plane record kind as a write's expect sees it:
// put writes the record, gen reads its stored generation or version, zero
// when there is none, and del, when the kind has one, deletes it.
type versioned struct {
	name string
	put  func(expect uint64) error
	gen  func() uint64
	del  func(expect uint64) error
}

// versionedKinds is every record kind whose writes take expect, each over
// records of its own in s.
func versionedKinds(t *testing.T, s *kvstore.Store) []versioned {
	cp, ix, c := s.ControlPlane(), s.ShareIndex(), s.Capacity()
	sh, u, g := share("photos", "/photos"), user("alice"), group("staff")
	inst := entity.SettingScope{Kind: entity.ScopeInstallation}
	q := entity.Quota{Principal: u.Principal, Hard: entity.Limit{Files: 10}}
	return []versioned{
		{"user",
			func(e uint64) error { return cp.PutUser(ctx, u, e, entity.Alloc{}) },
			func() uint64 {
				got, err := s.Principals().User(ctx, u.Principal)
				if merrs.IsNotFoundError(err) {
					return 0
				}
				must(t, err)
				return got.Generation
			},
			func(e uint64) error { return cp.DeleteUser(ctx, u.Principal, e) }},
		{"group",
			func(e uint64) error { return cp.PutGroup(ctx, g, e, entity.Alloc{}) },
			func() uint64 {
				for got, err := range cp.Groups(ctx, "") {
					must(t, err)
					if got.Principal == g.Principal {
						return got.Generation
					}
				}
				return 0
			},
			func(e uint64) error { return cp.DeleteGroup(ctx, g.Principal, e) }},
		{"share",
			func(e uint64) error { return cp.PutShare(ctx, sh, e) },
			func() uint64 {
				for got, err := range cp.Shares(ctx) {
					must(t, err)
					if got.ID == sh.ID {
						return got.Generation
					}
				}
				return 0
			},
			func(e uint64) error { return cp.DeleteShare(ctx, sh.ID, entity.SnapshotsRefuse, e) }},
		{"setting",
			func(e uint64) error { return cp.PutSetting(ctx, entity.Setting{Scope: inst, Key: "k"}, e) },
			func() uint64 {
				for got, err := range cp.Settings(ctx, inst) {
					must(t, err)
					return got.Generation
				}
				return 0
			},
			nil},
		{"netgroup",
			func(e uint64) error { return cp.PutNetgroup(ctx, entity.Netgroup{Name: "lab"}, e) },
			func() uint64 {
				got, _, err := ix.Netgroup(ctx, "lab")
				must(t, err)
				return got.Version
			},
			func(e uint64) error { return cp.DeleteNetgroup(ctx, "lab", e) }},
		{"export policy",
			func(e uint64) error {
				return cp.PutExportPolicy(ctx, entity.ExportPolicy{Share: sh.ID, Squash: entity.SquashAll}, e)
			},
			func() uint64 { return policyVersion(t, cp, sh.ID) },
			nil},
		{"grant",
			func(e uint64) error {
				return cp.SetGrant(ctx, entity.ShareGrant{Share: sh.ID, Principal: u.Principal, Access: entity.AccessRead}, e)
			},
			func() uint64 { return grantVersion(t, cp, sh.ID, u.Principal) },
			func(e uint64) error { return cp.DeleteGrant(ctx, sh.ID, u.Principal, e) }},
		{"quota",
			func(e uint64) error { return cp.SetQuota(ctx, sh.ID, q, e) },
			func() uint64 { return quotaVersion(t, c, sh.ID, kvstore.QuotaTarget{Principal: u.Principal}) },
			func(e uint64) error { return cp.SetQuota(ctx, sh.ID, entity.Quota{Principal: u.Principal}, e) }},
	}
}

// versionedFixture is the share and principals the grant, policy and quota
// kinds write against, put under expect zero.
func versionedFixture(t *testing.T, kinds []versioned) {
	for _, k := range kinds[:3] {
		must(t, k.put(0))
	}
}

func refused(err error) bool { return errors.Is(err, entity.ErrGenerationMismatch) }

// Every write that takes expect commits only over the generation or version
// it names, raising it; zero names no record, so a write under it only
// creates; and a refused write changes nothing.
func testWritesCheckExpect(t *testing.T, kvo Opener) {
	s, _ := newStore(t, kvo)
	kinds := versionedKinds(t, s)
	for _, k := range kinds {
		t.Run(k.name, func(t *testing.T) {
			if k.gen() == 0 {
				must(t, k.put(0))
			}
			v1 := k.gen()
			if v1 == 0 {
				t.Fatalf("%s after a create: stored generation 0", k.name)
			}
			if err := k.put(0); !refused(err) {
				t.Errorf("%s created again over an existing one: %v, want %v", k.name, err, entity.ErrGenerationMismatch)
			}
			if err := k.put(v1 + 1); !refused(err) {
				t.Errorf("%s written under another generation: %v, want %v", k.name, err, entity.ErrGenerationMismatch)
			}
			if got := k.gen(); got != v1 {
				t.Errorf("%s after refused writes: generation %d, want %d", k.name, got, v1)
			}
			must(t, k.put(v1))
			v2 := k.gen()
			if v2 <= v1 {
				t.Errorf("%s written under its generation: generation %d, want above %d", k.name, v2, v1)
			}
			if k.del == nil {
				return
			}
			for _, e := range []uint64{0, v1} {
				if err := k.del(e); !refused(err) {
					t.Errorf("%s deleted under %d, stored %d: %v, want %v", k.name, e, v2, err, entity.ErrGenerationMismatch)
				}
			}
			if got := k.gen(); got != v2 {
				t.Errorf("%s after refused deletes: generation %d, want %d", k.name, got, v2)
			}
		})
	}
	// The deletes last, the share's after the records that name it.
	for i := len(kinds) - 1; i >= 0; i-- {
		if k := kinds[i]; k.del != nil {
			must(t, k.del(k.gen()))
			if got := k.gen(); got != 0 {
				t.Errorf("%s deleted under its generation: still found at %d", k.name, got)
			}
		}
	}
}

// Two writes from one read: the second to commit is refused, though its
// read passed when it was made, because the check is inside its
// transaction.
func testExpectRace(t *testing.T, kvo Opener) {
	for _, b := range kvo.backends() {
		for i, name := range []string{"user", "group", "share", "setting", "netgroup", "export policy", "grant", "quota"} {
			t.Run(b.name+"/"+name, func(t *testing.T) {
				s, h := heldStore(t, b)
				kinds := versionedKinds(t, s)
				versionedFixture(t, kinds)
				k := kinds[i]
				if k.name != name {
					t.Fatalf("kind %d is %s, want %s", i, k.name, name)
				}
				if k.gen() == 0 {
					must(t, k.put(0))
				}
				read := k.gen()
				outer, inner := race(h, func() error { return k.put(read) }, func() error { return k.put(read) })
				if inner != nil {
					t.Fatalf("%s write committing first: %v", k.name, inner)
				}
				if !refused(outer) {
					t.Errorf("%s write committing second from the same read: %v, want %v", k.name, outer, entity.ErrGenerationMismatch)
				}
			})
		}
	}
}

// An Alloc naming an ID is refused: the store keeps no uid, gid or RID
// counter to draw it from.
func testAllocRefused(t *testing.T, kvo Opener) {
	s, _ := newStore(t, kvo)
	cp := s.ControlPlane()
	for name, err := range map[string]error{
		"user uid": cp.PutUser(ctx, user("alice"), 0, entity.Alloc{UID: true}),
		"user rid": cp.PutUser(ctx, user("bob"), 0, entity.Alloc{RID: true}),
		"group":    cp.PutGroup(ctx, group("staff"), 0, entity.Alloc{GID: true}),
	} {
		if !isCode(err, merrs.ErrInvalidArgument) {
			t.Errorf("%s allocation: %v, want invalid argument", name, err)
		}
	}
}

// A grant's delete and a share's setting name the share, and are stale once
// it is being removed.
func testWritesToRemovedShare(t *testing.T, kvo Opener) {
	s, _ := newStore(t, kvo)
	cp := s.ControlPlane()
	sh, u := share("photos", "/photos"), user("alice")
	must(t, cp.PutShare(ctx, sh, 0))
	must(t, cp.PutUser(ctx, u, 0, entity.Alloc{}))
	must(t, cp.SetGrant(ctx, entity.ShareGrant{Share: sh.ID, Principal: u.Principal, Access: entity.AccessRead}, 0))
	v := grantVersion(t, cp, sh.ID, u.Principal)
	must(t, cp.DeleteShare(ctx, sh.ID, entity.SnapshotsRefuse, 1))
	if err := cp.DeleteGrant(ctx, sh.ID, u.Principal, v); !errors.Is(err, entity.ErrStale) {
		t.Errorf("grant deleted on a share being removed: %v, want %v", err, entity.ErrStale)
	}
	scope := entity.SettingScope{Kind: entity.ScopeShare, ID: sh.ID}
	if err := cp.PutSetting(ctx, entity.Setting{Scope: scope, Key: "k"}, 0); !errors.Is(err, entity.ErrStale) {
		t.Errorf("setting put on a share being removed: %v, want %v", err, entity.ErrStale)
	}
}

func policyVersion(t *testing.T, cp kvstore.ControlPlane, share entity.ShareID) uint64 {
	t.Helper()
	p, err := cp.ExportPolicy(ctx, share)
	must(t, err)
	return p.Version
}

func quotaVersion(t *testing.T, c kvstore.Capacity, share entity.ShareID, target kvstore.QuotaTarget) uint64 {
	t.Helper()
	q, _, err := c.Quota(ctx, share, target)
	must(t, err)
	return q.Version
}

func netgroupVersion(t *testing.T, s *kvstore.Store, name string) uint64 {
	t.Helper()
	g, _, err := s.ShareIndex().Netgroup(ctx, name)
	must(t, err)
	return g.Version
}
