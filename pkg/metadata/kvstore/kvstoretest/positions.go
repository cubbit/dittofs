package kvstoretest

import (
	"testing"

	"github.com/google/uuid"

	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/metadata/kv"
	"github.com/cubbit/dittofs/pkg/metadata/kv/kvtest"
	"github.com/cubbit/dittofs/pkg/metadata/kvstore"
	"github.com/cubbit/dittofs/pkg/sim"
)

// counters reads every reconciled kind's counter.
func counters(t *testing.T, s *kvstore.Store) kvstore.Counters {
	t.Helper()
	c, err := s.Nodes().Counters(ctx)
	must(t, err)
	return c
}

// Each change of a setting, a share or a principal leaves the record at the
// position its kind's counter was raised to by that change.
func testPositions(t *testing.T, kvo Opener) {
	s, _ := newStore(t, kvo)
	cp := s.ControlPlane()
	sh, u, g := inNamespace(t, cp, share("photos", "/photos")), user("alice"), group("staff")
	var shareGen, userGen uint64
	set := func(v string, expect uint64) func() error {
		return func() error {
			return errOf(cp.PutSetting(ctx, entity.Setting{Scope: instScope, Key: "k", Value: []byte(v)}, expect))
		}
	}
	setting := func() uint64 { return all(t, cp.Settings(ctx, instScope))[0].Position }
	shareAt := func() uint64 { return all(t, cp.Shares(ctx))[0].Position }
	userAt := func() uint64 { return all(t, cp.Users(ctx, ""))[0].Position }
	groupAt := func() uint64 { return all(t, cp.Groups(ctx, ""))[0].Position }
	settings := func(c kvstore.Counters) uint64 { return c.Settings }
	shares := func(c kvstore.Counters) uint64 { return c.ShareList }
	principals := func(c kvstore.Counters) uint64 { return c.Principals }
	for _, c := range []struct {
		name     string
		write    func() error
		position func() uint64
		counter  func(kvstore.Counters) uint64
	}{
		{"setting created", set("1", 0), setting, settings},
		{"setting changed", set("2", 1), setting, settings},
		{"share created", func() (err error) { shareGen, err = cp.PutShare(ctx, sh, 0); return err }, shareAt, shares},
		{"share renamed", func() (err error) {
			sh.Name = "pictures"
			shareGen, err = cp.PutShare(ctx, sh, shareGen)
			return err
		}, shareAt, shares},
		{"share quiesced", func() error {
			return cp.SetShareState(ctx, sh.ID, entity.ShareEnabled, entity.ShareQuiesced)
		}, shareAt, shares},
		{"user created", func() (err error) { userGen, err = cp.PutUser(ctx, u, 0, entity.Alloc{}); return err }, userAt, principals},
		{"group created", func() error { return errOf(cp.PutGroup(ctx, g, 0, entity.Alloc{})) }, groupAt, principals},
		{"user changed", func() error {
			u.Email = "a@example"
			return errOf(cp.PutUser(ctx, u, userGen, entity.Alloc{}))
		}, userAt, principals},
	} {
		must(t, c.write())
		want := c.counter(counters(t, s))
		if got := c.position(); got != want || want == 0 {
			t.Errorf("%s: position %d, want its counter's value %d, above zero", c.name, got, want)
		}
	}
}

// One write of a user with a protocol ID, a group and two memberships
// raises the principals generation once, and both principals it writes
// carry that one position.
func testPrincipalsRaisedOncePerWrite(t *testing.T, kvo Opener) {
	s, _ := newStore(t, kvo)
	cp := s.ControlPlane()
	other := group("other")
	must(t, errOf(cp.PutGroup(ctx, other, 0, entity.Alloc{})))
	before := counters(t, s).Principals
	u, g := user("alice", uid("1000")), group("staff")
	must(t, cp.Write(ctx, entity.WriteMeta{}, func(tx entity.ControlPlaneTx) error {
		if _, err := tx.PutUser(u, 0, entity.Alloc{}); err != nil {
			return err
		}
		if _, err := tx.PutGroup(g, 0, entity.Alloc{}); err != nil {
			return err
		}
		if err := tx.AddMember(entity.Membership{Group: g.Principal, Member: u.Principal}); err != nil {
			return err
		}
		return tx.AddMember(entity.Membership{Group: other.Principal, Member: u.Principal})
	}))
	after := counters(t, s).Principals
	if after != before+1 {
		t.Errorf("principals generation after one write: got %d, want %d", after, before+1)
	}
	if got := all(t, cp.Users(ctx, ""))[0].Position; got != after {
		t.Errorf("user's position: got %d, want %d", got, after)
	}
	for _, got := range all(t, cp.Groups(ctx, "")) {
		if got.Principal == g.Principal && got.Position != after {
			t.Errorf("group's position: got %d, want %d", got.Position, after)
		}
	}
}

// A grant, an export policy, a netgroup and a quota take effect through
// their versions, so writing one moves no counter.
func testVersionedKindsMoveNoCounter(t *testing.T, kvo Opener) {
	s, _ := newStore(t, kvo)
	cp := s.ControlPlane()
	sh, u := share("photos", "/photos"), user("alice")
	must(t, errOf(CreateShare(ctx, cp, sh)))
	must(t, errOf(cp.PutUser(ctx, u, 0, entity.Alloc{})))
	for _, c := range []struct {
		name  string
		write func() error
	}{
		{"grant", func() error {
			return errOf(cp.SetGrant(ctx, entity.ShareGrant{Share: sh.ID, Principal: u.Principal, Access: entity.AccessRead}, 0))
		}},
		{"export policy", func() error { return errOf(cp.PutExportPolicy(ctx, entity.ExportPolicy{Share: sh.ID}, 0)) }},
		{"netgroup", func() error { return errOf(cp.PutNetgroup(ctx, entity.Netgroup{Name: "lab"}, 0)) }},
		{"quota", func() error {
			return errOf(cp.SetQuota(ctx, sh.ID, entity.Quota{Share: sh.ID, Principal: u.Principal, Hard: entity.Limit{Files: 9}}, 0))
		}},
	} {
		before := counters(t, s)
		must(t, c.write())
		if after := counters(t, s); after != before {
			t.Errorf("%s written: counters %+v, want unchanged %+v", c.name, after, before)
		}
	}
}

// Removing a member, deleting a user and deleting a group each raise the
// principals generation by one.
func testPrincipalRemovalsRaise(t *testing.T, kvo Opener) {
	s, _ := newStore(t, kvo)
	cp := s.ControlPlane()
	u, g := user("alice"), group("staff")
	userGen, err := cp.PutUser(ctx, u, 0, entity.Alloc{})
	must(t, err)
	groupGen, err := cp.PutGroup(ctx, g, 0, entity.Alloc{})
	must(t, err)
	m := entity.Membership{Group: g.Principal, Member: u.Principal}
	must(t, cp.AddMember(ctx, m))
	for _, c := range []struct {
		name  string
		write func() error
	}{
		{"member removed", func() error { return cp.RemoveMember(ctx, m) }},
		{"user deleted", func() error { return cp.DeleteUser(ctx, u.Principal, userGen) }},
		{"group deleted", func() error { return cp.DeleteGroup(ctx, g.Principal, groupGen) }},
	} {
		before := counters(t, s).Principals
		must(t, c.write())
		if after := counters(t, s).Principals; after != before+1 {
			t.Errorf("%s: principals generation %d, want %d", c.name, after, before+1)
		}
	}
}

// A write retried after a conflict raises the principals generation again
// in its new attempt, so the group it writes holds the counter's value.
func testPrincipalsRaisedPerAttempt(t *testing.T, kvo Opener) {
	f := kvtest.NewFaultKV(kvo(t))
	must(t, kvstore.Init(ctx, f, uuid.New()))
	s, err := kvstore.Open(ctx, f)
	must(t, err)
	cp := s.ControlPlane()
	sched, err := sim.NewSchedule(1, nil, sim.Rule{Op: kvtest.OpSet, Key: "G‖*", Count: 1, Action: sim.FaultAction{Err: kv.ErrConflict}})
	must(t, err)
	f.SetFault(sched)
	g := group("staff")
	must(t, errOf(cp.PutGroup(ctx, g, 0, entity.Alloc{})))
	f.SetFault(nil)
	got, want := all(t, cp.Groups(ctx, ""))[0].Position, counters(t, s).Principals
	if got != want || want == 0 {
		t.Errorf("group written on a retried attempt: position %d, want the principals generation %d, above zero", got, want)
	}
}
