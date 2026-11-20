package kvstoretest

import (
	"errors"
	"slices"
	"testing"

	"github.com/cubbit/dittofs/pkg/metadata/entity"
	merrs "github.com/cubbit/dittofs/pkg/metadata/errors"
	"github.com/cubbit/dittofs/pkg/metadata/kvstore"
)

// versioned is one control-plane record kind as a write's expect sees it:
// put writes the record and returns the generation or version it wrote,
// gen reads the stored one, zero when there is none, and del, when the kind has one, deletes it.
type versioned struct {
	name string
	put  func(expect uint64) (uint64, error)
	gen  func() uint64
	del  func(expect uint64) error
}

// versionedKinds is every record kind whose writes take expect, each over
// records of its own in s.
func versionedKinds(t *testing.T, s *kvstore.Store) []versioned {
	cp, c := s.ControlPlane(), s.Capacity()
	sh, u, g := inNamespace(t, cp, share("photos", "/photos")), user("alice"), group("staff")
	inst := entity.SettingScope{Kind: entity.ScopeInstallation}
	q := entity.Quota{Principal: u.Principal, Hard: entity.Limit{Files: 10}}
	return []versioned{
		{"user",
			func(e uint64) (uint64, error) { return cp.PutUser(ctx, u, e, entity.Alloc{}) },
			func() uint64 { return userGeneration(t, s, u.Principal) },
			func(e uint64) error { return deleted(s, cp.DeleteUser(ctx, u.Principal, e), u.Principal) }},
		{"group",
			func(e uint64) (uint64, error) { return cp.PutGroup(ctx, g, e, entity.Alloc{}) },
			func() uint64 { return groupGeneration(t, cp, g.Principal) },
			func(e uint64) error { return deleted(s, cp.DeleteGroup(ctx, g.Principal, e), g.Principal) }},
		{"share",
			func(e uint64) (uint64, error) { return cp.PutShare(ctx, sh, e) },
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
		{
			"setting",
			func(e uint64) (uint64, error) { return cp.PutSetting(ctx, entity.Setting{Scope: inst, Key: "k"}, e) },
			func() uint64 {
				for got, err := range cp.Settings(ctx, inst) {
					must(t, err)
					return got.Generation
				}
				return 0
			},
			func(e uint64) error {
				return cp.DeleteSetting(ctx, inst, "k", e)
			},
		},
		{"netgroup",
			func(e uint64) (uint64, error) { return cp.PutNetgroup(ctx, entity.Netgroup{Name: "lab"}, e) },
			func() uint64 { return netgroupVersion(t, s, "lab") },
			func(e uint64) error { return cp.DeleteNetgroup(ctx, "lab", e) }},
		{"export policy",
			func(e uint64) (uint64, error) {
				return cp.PutExportPolicy(ctx, entity.ExportPolicy{Share: sh.ID, Squash: entity.SquashAll}, e)
			},
			func() uint64 { return policyVersion(t, cp, sh.ID) },
			nil},
		{"grant",
			func(e uint64) (uint64, error) {
				return cp.SetGrant(ctx, entity.ShareGrant{Share: sh.ID, Principal: u.Principal, Access: entity.AccessRead}, e)
			},
			func() uint64 { return grantVersion(t, cp, sh.ID, u.Principal) },
			func(e uint64) error { return cp.DeleteGrant(ctx, sh.ID, u.Principal, e) }},
		{"quota",
			func(e uint64) (uint64, error) { return cp.SetQuota(ctx, sh.ID, q, e) },
			func() uint64 { return quotaVersion(t, c, sh.ID, kvstore.QuotaTarget{Principal: u.Principal}) },
			func(e uint64) error { return errOf(cp.SetQuota(ctx, sh.ID, entity.Quota{Principal: u.Principal}, e)) }},
	}
}

// versionedFixture is the share and principals the grant, policy and quota
// kinds write against, put under expect zero.
func versionedFixture(t *testing.T, kinds []versioned) {
	for _, k := range kinds[:3] {
		must(t, errOf(k.put(0)))
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
				must(t, errOf(k.put(0)))
			}
			v1 := k.gen()
			if v1 == 0 {
				t.Fatalf("%s after a create: stored generation 0", k.name)
			}
			if _, err := k.put(0); !refused(err) {
				t.Errorf("%s created again over an existing one: %v, want %v", k.name, err, entity.ErrGenerationMismatch)
			}
			if _, err := k.put(v1 + 1); !refused(err) {
				t.Errorf("%s written under another generation: %v, want %v", k.name, err, entity.ErrGenerationMismatch)
			}
			if got := k.gen(); got != v1 {
				t.Errorf("%s after refused writes: generation %d, want %d", k.name, got, v1)
			}
			wrote, err := k.put(v1)
			must(t, err)
			v2 := k.gen()
			if wrote != v2 {
				t.Errorf("%s written under its generation: returned %d, read back %d", k.name, wrote, v2)
			}
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
					must(t, errOf(k.put(0)))
				}
				read := k.gen()
				outer, inner := race(h, func() error { return errOf(k.put(read)) }, func() error { return errOf(k.put(read)) })
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

// A user, group or setting deleted and created again under the same key takes
// a generation it never had, so a write from a read of the deleted record is
// refused rather than applied to the new one.
func testRecreatedPrincipalRefusesOldGeneration(t *testing.T, kvo Opener) {
	s, _ := newStore(t, kvo)
	for _, k := range slices.DeleteFunc(versionedKinds(t, s), func(k versioned) bool {
		return k.name != "user" && k.name != "group" && k.name != "setting"
	}) {
		t.Run(k.name, func(t *testing.T) {
			must(t, errOf(k.put(0)))
			deleted := k.gen()
			must(t, k.del(deleted))
			must(t, errOf(k.put(0)))
			stored := k.gen()
			if _, err := k.put(deleted); !refused(err) {
				t.Errorf("%s created again, stored at %d, written under the deleted record's generation %d: %v, want %v", k.name, stored, deleted, err, entity.ErrGenerationMismatch)
			}
		})
	}
}

// A grant's delete and a share's setting, written or deleted, name the
// share, and are stale once it is being removed.
func testWritesToRemovedShare(t *testing.T, kvo Opener) {
	s, _ := newStore(t, kvo)
	cp := s.ControlPlane()
	sh, u := share("photos", "/photos"), user("alice")
	must(t, errOf(CreateShare(ctx, cp, sh)))
	must(t, errOf(cp.PutUser(ctx, u, 0, entity.Alloc{})))
	must(t, errOf(cp.SetGrant(ctx, entity.ShareGrant{Share: sh.ID, Principal: u.Principal, Access: entity.AccessRead}, 0)))
	v := grantVersion(t, cp, sh.ID, u.Principal)

	scope := entity.SettingScope{Kind: entity.ScopeShare, ID: sh.ID}

	sg, err := cp.PutSetting(ctx, entity.Setting{Scope: scope, Key: "k"}, 0)
	if err != nil {
		t.Fatalf("setting put while the share lives: %v", err)
	}

	must(t, cp.DeleteShare(ctx, sh.ID, entity.SnapshotsRefuse, 1))

	if err := cp.DeleteGrant(ctx, sh.ID, u.Principal, v); !errors.Is(err, entity.ErrStale) {
		t.Errorf("grant deleted on a share being removed: %v, want %v", err, entity.ErrStale)
	}

	if err := cp.DeleteSetting(ctx, scope, "k", sg); !errors.Is(err, entity.ErrStale) {
		t.Errorf("setting deleted on a share being removed: %v, want %v", err, entity.ErrStale)
	}

	if _, err := cp.PutSetting(ctx, entity.Setting{Scope: scope, Key: "k"}, 0); !errors.Is(err, entity.ErrStale) {
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

// userGeneration is p's stored generation, zero when there is no such user.
func userGeneration(t *testing.T, s *kvstore.Store, p entity.Principal) uint64 {
	t.Helper()
	got, err := s.Principals().User(ctx, p)
	if merrs.IsNotFoundError(err) {
		return 0
	}
	must(t, err)
	return got.Generation
}

// groupGeneration is p's stored generation, zero when there is no such group.
func groupGeneration(t *testing.T, cp kvstore.ControlPlane, p entity.Principal) uint64 {
	t.Helper()
	for got, err := range cp.Groups(ctx, "") {
		must(t, err)
		if got.Principal == p {
			return got.Generation
		}
	}
	return 0
}
