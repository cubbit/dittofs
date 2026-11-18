package identity_test

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/metadata/entity"
	merrs "github.com/cubbit/dittofs/pkg/metadata/errors"
	"github.com/cubbit/dittofs/pkg/metadata/kv/kvtest"
	"github.com/cubbit/dittofs/pkg/metadata/kvstore"
	"github.com/cubbit/dittofs/pkg/sim"
	"github.com/cubbit/dittofs/pkg/vfs/vfserr"
)

// dirGroup is a group named name with gid n and a SID ending in n.
func dirGroup(name, n string) entity.Group {
	return entity.Group{Principal: entity.NewGroup(), Name: name, IDs: entity.ProtocolIDs{
		{Scheme: entity.SchemeGID, Value: n}, sidID("S-1-5-21-7-7-7-" + n),
	}}
}

// minted is g as another call minted it: the same spellings, its own
// PrincipalID.
func minted(g entity.Group) entity.Group {
	g.Principal = entity.NewGroup()
	return g
}

// planOf is a plan minting want, as a lookup would: a group whose SID a
// principal holds is bound, the rest are the batch. runs counts its runs.
func planOf(st *kvstore.Store, runs *atomic.Int32, want ...entity.Group) func(context.Context) (entity.MintBatch, int, error) {
	return func(ctx context.Context) (entity.MintBatch, int, error) {
		runs.Add(1)
		var b entity.MintBatch
		bound := 0
		for _, g := range want {
			_, err := st.Principals().ByProtocolID(ctx, g.IDs[1])
			switch {
			case err == nil:
				bound++
			case merrs.IsNotFoundError(err):
				b.Groups = append(b.Groups, entity.MintGroup{Group: g})
			default:
				return entity.MintBatch{}, 0, err
			}
		}
		return b, bound, nil
	}
}

// fixed is a plan answering b, binding nothing, on every run.
func fixed(runs *atomic.Int32, b entity.MintBatch) func(context.Context) (entity.MintBatch, int, error) {
	return func(context.Context) (entity.MintBatch, int, error) {
		runs.Add(1)
		return b, 0, nil
	}
}

func (r *resolving) pgen(t *testing.T) uint64 {
	t.Helper()
	c, err := r.st.Nodes().Counters(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return c.Principals
}

// holder is the principal holding id; the zero one for none.
func (r *resolving) holder(t *testing.T, id entity.ProtocolID) entity.Principal {
	t.Helper()
	p, err := r.st.Principals().ByProtocolID(context.Background(), id)
	if err != nil && !merrs.IsNotFoundError(err) {
		t.Fatal(err)
	}
	return p
}

// A mint whose commit another mint overtakes re-runs its plan: it binds the
// two groups the other minted and mints only the third, and each commit
// raises the principals generation once. A path that gave up at the first
// ErrExist fails the call.
func TestMintReRunsAPlanAnotherMintOvertook(t *testing.T) {
	r := newResolving(t)
	ctx := context.Background()
	g1, g2, g3 := dirGroup("g1", "3001"), dirGroup("g2", "3002"), dirGroup("g3", "3003")
	clock := sim.NewTestClock(time.Unix(0, 0))
	s, err := sim.NewSchedule(1, clock, sim.Rule{Op: kvtest.OpUpdate, Key: "*", From: 1, Count: 1, Action: sim.FaultAction{Delay: time.Second}})
	if err != nil {
		t.Fatal(err)
	}
	r.fault.SetFault(s)
	before := r.pgen(t)

	var runs atomic.Int32
	done := make(chan error, 1)
	go func() { done <- r.srv.Mint(ctx, planOf(r.st, &runs, g1, g2, g3)) }()
	backstop := time.After(10 * time.Second)
	for clock.Pending() == 0 { // the first mint's commit waits on the clock
		select {
		case err := <-done:
			t.Fatalf("first mint returned %v before its commit was delayed", err)
		case <-backstop:
			t.Fatal("the first mint's commit never reached the delay")
		case <-time.After(time.Millisecond):
		}
	}

	b1, b2 := minted(g1), minted(g2)
	var otherRuns atomic.Int32
	if err := r.srv.Mint(ctx, planOf(r.st, &otherRuns, b1, b2)); err != nil {
		t.Fatalf("overtaking mint: %v", err)
	}
	clock.Advance(time.Second)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("overtaken mint: got %v, want nil", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("overtaken mint never returned")
	}

	if got := runs.Load(); got != 2 {
		t.Errorf("overtaken mint's plan runs: got %d, want 2", got)
	}
	for _, c := range []struct {
		name string
		id   entity.ProtocolID
		want entity.Principal
	}{{"g1", g1.IDs[1], b1.Principal}, {"g2", g2.IDs[1], b2.Principal}, {"g3", g3.IDs[1], g3.Principal}} {
		if got := r.holder(t, c.id); got != c.want {
			t.Errorf("%s's SID: held by %v, want %v", c.name, got, c.want)
		}
	}
	if got := r.pgen(t); got != before+2 {
		t.Errorf("principals generation after two commits: got %d, want %d", got, before+2)
	}
}

// A name another mint takes between the check and the commit fails the
// commit ErrExist with nothing to bind: the re-run marks the record Unnamed,
// which is progress, and mints it. A rule counting only bound principals
// fails the call.
func TestMintReRunUnnamesANameTakenBeforeItsCommit(t *testing.T) {
	r := newResolving(t)
	ctx := context.Background()
	clock := sim.NewTestClock(time.Unix(0, 0))
	s, err := sim.NewSchedule(1, clock, sim.Rule{Op: kvtest.OpUpdate, Key: "*", From: 1, Count: 1, Action: sim.FaultAction{Delay: time.Second}})
	if err != nil {
		t.Fatal(err)
	}
	r.fault.SetFault(s)

	ours := dirGroup("finance", "3401")
	var runs atomic.Int32
	done := make(chan error, 1)
	go func() { done <- r.srv.Mint(ctx, planOf(r.st, &runs, ours)) }()
	backstop := time.After(10 * time.Second)
	for clock.Pending() == 0 { // our commit waits on the clock, its name check passed
		select {
		case err := <-done:
			t.Fatalf("mint returned %v before its commit was delayed", err)
		case <-backstop:
			t.Fatal("the mint's commit never reached the delay")
		case <-time.After(time.Millisecond):
		}
	}

	theirs := dirGroup("Finance", "3402")
	var otherRuns atomic.Int32
	if err := r.srv.Mint(ctx, planOf(r.st, &otherRuns, theirs)); err != nil {
		t.Fatalf("mint taking the name: %v", err)
	}
	clock.Advance(time.Second)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("mint whose name was taken: got %v, want nil", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("mint whose name was taken never returned")
	}

	if got := runs.Load(); got != 2 {
		t.Errorf("plan runs: got %d, want 2", got)
	}
	if got := r.holder(t, ours.IDs[1]); got != ours.Principal {
		t.Errorf("our SID: held by %v, want %v", got, ours.Principal)
	}
	if got, err := r.st.Principals().ByName(ctx, entity.PrincipalGroup, "finance"); err != nil || got != theirs.Principal {
		t.Errorf("ByName finance: got %v, %v, want the group that took it %v", got, err, theirs.Principal)
	}
	if got := r.counter(t, "identity_mints_unnamed_total", "kind", "group"); got != 1 {
		t.Errorf("identity_mints_unnamed_total{kind=group}: got %v, want 1", got)
	}
}

// A re-run that binds a principal the run before did not has progressed,
// though the only record it unnames is one the first run already unnamed.
// Summing the two steps would fail the call.
func TestMintReRunBindsBesideAHeldName(t *testing.T) {
	r := newResolving(t)
	ctx := context.Background()
	local := dirGroup("Finance", "3501")
	if _, err := r.cp.PutGroup(ctx, local, 0, entity.Alloc{}); err != nil {
		t.Fatal(err)
	}
	clock := sim.NewTestClock(time.Unix(0, 0))
	s, err := sim.NewSchedule(1, clock, sim.Rule{Op: kvtest.OpUpdate, Key: "*", From: 1, Count: 1, Action: sim.FaultAction{Delay: time.Second}})
	if err != nil {
		t.Fatal(err)
	}
	r.fault.SetFault(s)

	x, y := dirGroup("finance", "3502"), dirGroup("y", "3503")
	var runs atomic.Int32
	done := make(chan error, 1)
	go func() { done <- r.srv.Mint(ctx, planOf(r.st, &runs, x, y)) }()
	backstop := time.After(10 * time.Second)
	for clock.Pending() == 0 { // the first run's commit waits on the clock
		select {
		case err := <-done:
			t.Fatalf("mint returned %v before its commit was delayed", err)
		case <-backstop:
			t.Fatal("the mint's commit never reached the delay")
		case <-time.After(time.Millisecond):
		}
	}
	other := minted(y)
	var otherRuns atomic.Int32
	if err := r.srv.Mint(ctx, planOf(r.st, &otherRuns, other)); err != nil {
		t.Fatalf("overtaking mint: %v", err)
	}
	clock.Advance(time.Second)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("overtaken mint: got %v, want nil", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("overtaken mint never returned")
	}

	if got := runs.Load(); got != 2 {
		t.Errorf("plan runs: got %d, want 2", got)
	}
	if got := r.holder(t, x.IDs[1]); got != x.Principal {
		t.Errorf("x's SID: held by %v, want %v", got, x.Principal)
	}
	if got := r.holder(t, y.IDs[1]); got != other.Principal {
		t.Errorf("y's SID: held by %v, want the overtaking mint's %v", got, other.Principal)
	}
	if got := r.counter(t, "identity_mints_unnamed_total", "kind", "group"); got != 1 {
		t.Errorf("identity_mints_unnamed_total{kind=group}: got %v, want 1", got)
	}
}

// A re-run whose only step is re-marking a record this call already unnamed
// has not progressed: it fails ErrUnavailable at the second run, not at the
// deadline. Counting the mark per run would re-run until the deadline.
func TestMintFailsARunThatOnlyReMarksARecord(t *testing.T) {
	for _, c := range []struct {
		name  string
		alloc entity.Alloc
		ids   func(local entity.Group) entity.ProtocolIDs
	}{
		// Its name and its SID are both the local group's: unnaming it
		// never clears the SID's clash.
		{"known by its SID", entity.Alloc{}, func(local entity.Group) entity.ProtocolIDs { return local.IDs }},
		// With no SID, its RID still to allocate, the local group's gid
		// clashes instead, and its mark is known by its name.
		{"known by its name", entity.Alloc{RID: true}, func(local entity.Group) entity.ProtocolIDs { return local.IDs[:1] }},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := newResolving(t)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			local := dirGroup("Finance", "3601")
			if _, err := r.cp.PutGroup(ctx, local, 0, entity.Alloc{}); err != nil {
				t.Fatal(err)
			}
			clash := entity.Group{Name: "FINANCE", IDs: c.ids(local)}

			// A fresh batch each run, as a lookup builds one, so no record
			// comes back already marked, and a fresh PrincipalID for it each
			// run, so the PrincipalID never tells a re-mark from a new one.
			var runs atomic.Int32
			err := r.srv.Mint(ctx, func(context.Context) (entity.MintBatch, int, error) {
				runs.Add(1)
				return entity.MintBatch{Groups: []entity.MintGroup{{Group: minted(clash), Alloc: c.alloc}}}, 0, nil
			})
			if !errors.Is(err, vfserr.ErrUnavailable) {
				t.Fatalf("mint: got %v, want ErrUnavailable", err)
			}
			if ctx.Err() != nil {
				t.Errorf("mint failed at the deadline, want at its second run")
			}
			if got := runs.Load(); got != 2 {
				t.Errorf("plan runs: got %d, want 2", got)
			}
		})
	}
}

// A plan that keeps answering a batch whose SID another principal holds
// binds nothing and unnames nothing, so it fails ErrUnavailable at its
// second run, writes nothing, and logs both principals. A path with no
// progress rule would run it until the deadline.
func TestMintFailsARunThatBindsNothingNew(t *testing.T) {
	r := newResolving(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	held := dirGroup("local", "3101")
	if _, err := r.cp.PutGroup(ctx, held, 0, entity.Alloc{}); err != nil {
		t.Fatal(err)
	}
	clash := minted(held)
	clash.Name = "dir"
	y := dirGroup("y", "3102")
	before := r.pgen(t)

	var runs atomic.Int32
	err := r.srv.Mint(ctx, fixed(&runs, entity.MintBatch{Groups: []entity.MintGroup{{Group: clash}, {Group: y}}}))
	if !errors.Is(err, vfserr.ErrUnavailable) {
		t.Fatalf("mint: got %v, want ErrUnavailable", err)
	}
	if got := runs.Load(); got != 2 {
		t.Errorf("plan runs: got %d, want 2", got)
	}
	if got := r.holder(t, y.IDs[1]); got != (entity.Principal{}) {
		t.Errorf("y's SID: held by %v after a failed mint, want no principal", got)
	}
	if got := r.pgen(t); got != before {
		t.Errorf("principals generation: got %d, want %d unchanged", got, before)
	}
	for _, p := range []entity.Principal{clash.Principal, held.Principal} {
		if !strings.Contains(r.logs.String(), p.ID.String()) {
			t.Errorf("log does not name principal %s: %s", p.ID, r.logs)
		}
	}
}

// A group whose name a local group holds under the principal fold is minted
// without it: found by its SID, never by the name, and counted.
func TestMintUnnamesANameTheIndexHolds(t *testing.T) {
	r := newResolving(t)
	ctx := context.Background()
	local := dirGroup("Finance", "3201")
	if _, err := r.cp.PutGroup(ctx, local, 0, entity.Alloc{}); err != nil {
		t.Fatal(err)
	}
	dir := dirGroup("finance", "3202")
	var runs atomic.Int32
	if err := r.srv.Mint(ctx, fixed(&runs, entity.MintBatch{Groups: []entity.MintGroup{{Group: dir}}})); err != nil {
		t.Fatalf("mint: got %v, want nil", err)
	}
	if got := r.holder(t, dir.IDs[1]); got != dir.Principal {
		t.Errorf("minted group's SID: held by %v, want %v", got, dir.Principal)
	}
	if got, err := r.st.Principals().ByName(ctx, entity.PrincipalGroup, "finance"); err != nil || got != local.Principal {
		t.Errorf("ByName finance: got %v, %v, want the local group %v", got, err, local.Principal)
	}
	if got := r.counter(t, "identity_mints_unnamed_total", "kind", "group"); got != 1 {
		t.Errorf("identity_mints_unnamed_total{kind=group}: got %v, want 1", got)
	}
	for _, p := range []entity.Principal{dir.Principal, local.Principal} {
		if !strings.Contains(r.logs.String(), p.ID.String()) {
			t.Errorf("log does not name principal %s: %s", p.ID, r.logs)
		}
	}
}

// Two groups of one batch whose names fold alike are both minted, the second
// without its name. Left to the store, the clash would fail every run.
func TestMintUnnamesAClashInsideOneBatch(t *testing.T) {
	r := newResolving(t)
	ctx := context.Background()
	first, second := dirGroup("Finance", "3301"), dirGroup("FINANCE", "3302")
	var runs atomic.Int32
	if err := r.srv.Mint(ctx, fixed(&runs, entity.MintBatch{Groups: []entity.MintGroup{{Group: first}, {Group: second}}})); err != nil {
		t.Fatalf("mint: got %v, want nil", err)
	}
	for _, g := range []entity.Group{first, second} {
		if got := r.holder(t, g.IDs[1]); got != g.Principal {
			t.Errorf("%s's SID: held by %v, want %v", g.Name, got, g.Principal)
		}
	}
	if got, err := r.st.Principals().ByName(ctx, entity.PrincipalGroup, "finance"); err != nil || got != first.Principal {
		t.Errorf("ByName finance: got %v, %v, want the first group %v", got, err, first.Principal)
	}
	if got := r.counter(t, "identity_mints_unnamed_total", "kind", "group"); got != 1 {
		t.Errorf("identity_mints_unnamed_total{kind=group}: got %v, want 1", got)
	}
}

// Every record a committed batch mints is counted by its kind.
func TestMintCountsEachRecordByKind(t *testing.T) {
	r := newResolving(t)
	u := entity.User{Principal: entity.NewUser(), Name: "alice", IDs: fileIDs("4001")}
	b := entity.MintBatch{
		Users:  []entity.MintUser{{User: u}},
		Groups: []entity.MintGroup{{Group: dirGroup("g1", "4002")}, {Group: dirGroup("g2", "4003")}},
	}
	var runs atomic.Int32
	if err := r.srv.Mint(context.Background(), fixed(&runs, b)); err != nil {
		t.Fatalf("mint: got %v, want nil", err)
	}
	for kind, want := range map[string]float64{"user": 1, "group": 2} {
		if got := r.counter(t, "identity_mints_total", "kind", kind); got != want {
			t.Errorf("identity_mints_total{kind=%s}: got %v, want %v", kind, got, want)
		}
	}
}

// A refusal of the store's that no retry clears keeps its own class, so a
// sign-in is not retried against a state only an operator or the caller can
// change. Wrapping it ErrUnavailable sends every caller back to retry.
func TestMintKeepsTheClassOfARefusalNoRetryClears(t *testing.T) {
	one := entity.IDRange{Lo: 5000, Hi: 5000}
	for _, c := range []struct {
		name  string
		setup func(t *testing.T, r *resolving)
		b     entity.MintBatch
		want  error
	}{
		{"an exhausted ID range", func(t *testing.T, r *resolving) {
			g := entity.Group{Principal: entity.NewGroup(), Name: "first"}
			if _, err := r.cp.PutGroup(context.Background(), g, 0, entity.Alloc{GID: true, RID: true, Range: one}); err != nil {
				t.Fatal(err)
			}
		}, entity.MintBatch{Groups: []entity.MintGroup{{Group: entity.Group{Principal: entity.NewGroup(), Name: "second"}, Alloc: entity.Alloc{GID: true, RID: true, Range: one}}}}, vfserr.ErrNoSpace},
		{"a user holding an API role", func(*testing.T, *resolving) {},
			entity.MintBatch{Users: []entity.MintUser{{User: entity.User{Principal: entity.NewUser(), Name: "alice", Role: entity.RoleAdmin, IDs: fileIDs("4101")}}}}, vfserr.ErrInvalid},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := newResolving(t)
			c.setup(t, r)
			var runs atomic.Int32
			err := r.srv.Mint(context.Background(), fixed(&runs, c.b))
			if !errors.Is(err, c.want) || errors.Is(err, vfserr.ErrUnavailable) {
				t.Fatalf("mint: got %v, want %v and not ErrUnavailable", err, c.want)
			}
			if got := runs.Load(); got != 1 {
				t.Errorf("plan runs: got %d, want 1", got)
			}
		})
	}
}

// A commit the store fails is the store's failure, which a retry may clear:
// ErrUnavailable.
func TestMintAnswersAStoreFailureUnavailable(t *testing.T) {
	r := newResolving(t)
	r.fail(t, kvtest.OpUpdate, "*")
	var runs atomic.Int32
	err := r.srv.Mint(context.Background(), fixed(&runs, entity.MintBatch{Groups: []entity.MintGroup{{Group: dirGroup("g", "4201")}}}))
	if !errors.Is(err, vfserr.ErrUnavailable) {
		t.Fatalf("mint: got %v, want ErrUnavailable", err)
	}
}
