package node

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cubbit/dittofs/internal/node/step"
	"github.com/cubbit/dittofs/internal/prof"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/metadata/kv"
	"github.com/cubbit/dittofs/pkg/metadata/kv/badgerkv"
	"github.com/cubbit/dittofs/pkg/metadata/kv/kvtest"
	"github.com/cubbit/dittofs/pkg/metadata/kvstore"
	"github.com/cubbit/dittofs/pkg/metadata/kvstore/kvstoretest"
	"github.com/cubbit/dittofs/pkg/node/apiv1"
	"github.com/cubbit/dittofs/pkg/sim"
)

var runnerNode = entity.NodeID{'r', 'u', 'n'}

// bench is a store over Badger under dir, on a test clock, and runners
// built over it with the node's step table.
type bench struct {
	t     *testing.T
	dir   string // empty keeps Badger in memory
	clock *sim.TestClock
	wrap  func(kv.KV) kv.KV // what the store opens on, over Badger
	db    *badgerkv.Store
	store *kvstore.Store
}

func newBench(t *testing.T, dir string, wrap func(kv.KV) kv.KV) *bench {
	t.Helper()
	b := &bench{t: t, dir: dir, clock: sim.NewTestClock(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)), wrap: wrap}
	if b.wrap == nil {
		b.wrap = func(db kv.KV) kv.KV { return db }
	}
	b.open(true)
	return b
}

// open opens Badger and the store over it, initialising the store when
// init is set, and closes Badger when the test ends.
func (b *bench) open(init bool) {
	b.t.Helper()
	db, err := badgerkv.Open(badgerkv.Options{Dir: b.dir, NodeID: runnerNode, Clock: b.clock.Now})
	if err != nil {
		b.t.Fatalf("open Badger: %v", err)
	}
	b.t.Cleanup(func() { _ = db.Close() })
	b.db = db
	kvs := b.wrap(db)
	if init {
		if err := kvstore.Init(b.t.Context(), kvs, uuid.New()); err != nil {
			b.t.Fatalf("init the store: %v", err)
		}
	}
	if b.store, err = kvstore.Open(b.t.Context(), kvs); err != nil {
		b.t.Fatalf("open the store: %v", err)
	}
}

// restart closes Badger and opens the store again over the same directory.
func (b *bench) restart() {
	b.t.Helper()
	if err := b.db.Close(); err != nil {
		b.t.Fatalf("close Badger: %v", err)
	}
	b.open(false)
}

// runner is a new runner over the store, with steps or, when nil, the
// node's own table.
func (b *bench) runner(steps map[string]stepRow) *runner {
	if steps == nil {
		steps = operationSteps(b.store.Nodes())
	}
	return newRunner(b.store.ControlPlane(), b.clock, runnerNode, slog.New(slog.NewTextHandler(b.t.Output(), nil)), steps)
}

// at advances the clock to d past its start and has r take a turn.
func (b *bench) at(r *runner, d time.Duration) {
	b.clock.Advance(d - b.clock.Elapsed())
	r.turn(b.t.Context())
}

func (b *bench) must(err error) {
	b.t.Helper()
	if err != nil {
		b.t.Fatal(err)
	}
}

// operation creates a pending operation of kind on target.
func (b *bench) operation(kind string, target entity.ResourceRef) entity.Operation {
	b.t.Helper()
	op := entity.Operation{ID: entity.OperationID(uuid.New()), Kind: kind, Target: target, State: entity.OpPending}
	_, err := b.store.ControlPlane().PutOperation(b.t.Context(), op, 0)
	b.must(err)
	return op
}

// deleteUser marks u and creates its pending deletion in one write, as the
// management API's delete does.
func (b *bench) deleteUser(u entity.Principal, gen uint64) entity.Operation {
	b.t.Helper()
	op := entity.Operation{ID: entity.OperationID(uuid.New()), Kind: string(apiv1.OpUserDelete),
		Target: entity.ResourceRef{Kind: entity.ResourceUser, ID: u.ID[:]}, State: entity.OpPending}
	b.must(b.store.ControlPlane().Write(b.t.Context(), entity.WriteMeta{}, func(tx entity.ControlPlaneTx) error {
		if err := tx.DeleteUser(u, gen); err != nil {
			return err
		}
		_, err := tx.PutOperation(op, 0)
		return err
	}))
	return op
}

// user creates a user and returns it with its generation.
func (b *bench) user(name string) (entity.Principal, uint64) {
	b.t.Helper()
	u := entity.User{Principal: entity.NewUser(), Name: name}
	gen, err := b.store.ControlPlane().PutUser(b.t.Context(), u, 0, entity.Alloc{})
	b.must(err)
	return u.Principal, gen
}

func (b *bench) read(id entity.OperationID) entity.Operation {
	b.t.Helper()
	op, found, err := b.store.ControlPlane().Operation(b.t.Context(), id)
	if err != nil || !found {
		b.t.Fatalf("read operation %v: found %v, %v", id, found, err)
	}
	return op
}

// keysNaming counts the store's keys holding id's bytes.
func (b *bench) keysNaming(id []byte) int {
	b.t.Helper()
	n := 0
	b.must(b.db.View(b.t.Context(), func(r kv.Reader) error {
		for row, err := range r.Scan(nil, nil) {
			if err != nil {
				return err
			}
			if bytes.Contains(row.Key, id) {
				n++
			}
		}
		return nil
	}))
	return n
}

// opAudits is the audit records whose target is op.
func (b *bench) opAudits(op entity.OperationID) []entity.AuditRecord {
	b.t.Helper()
	target := entity.ResourceRef{Kind: entity.ResourceOperation, ID: op[:]}
	var out []entity.AuditRecord
	for a, err := range b.store.ControlPlane().AuditRecords(b.t.Context(), entity.AuditFilter{Target: &target}, nil) {
		b.must(err)
		out = append(out, a)
	}
	return out
}

// faults is a FaultKV over Badger, kept so a test can set its fault.
func faults(f **kvtest.FaultKV) func(kv.KV) kv.KV {
	return func(db kv.KV) kv.KV {
		*f = kvtest.NewFaultKV(db)
		return *f
	}
}

func schedule(t *testing.T, rules ...sim.Rule) *sim.Schedule {
	t.Helper()
	s, err := sim.NewSchedule(1, nil, rules...)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// A user's deletion stopped between two batches, its store closed and
// reopened under a new runner, resumes from the records and ends succeeded
// with no row naming the user left, and another user's grant kept.
func TestUserDeletionResumesAcrossRestart(t *testing.T) {
	b := newBench(t, t.TempDir(), nil)
	ctx, cp := t.Context(), b.store.ControlPlane()
	alice, gen := b.user("alice")
	bob, _ := b.user("bob")
	g := entity.Group{Principal: entity.NewGroup(), Name: "staff"}
	_, err := cp.PutGroup(ctx, g, 0, entity.Alloc{})
	b.must(err)
	sh := entity.Share{ID: entity.NewShareID(), Name: "photos", Path: "/photos", Root: entity.NewFileID()}
	_, err = kvstoretest.CreateShare(ctx, cp, sh)
	b.must(err)
	for _, p := range []entity.Principal{alice, bob} {
		_, err = cp.SetGrant(ctx, entity.ShareGrant{Share: sh.ID, Principal: p, Access: entity.AccessRead}, 0)
		b.must(err)
	}
	b.must(cp.AddMember(ctx, entity.Membership{Group: g.Principal, Member: alice}))
	_, err = cp.SetQuota(ctx, sh.ID, entity.Quota{Principal: alice, Hard: entity.Limit{Files: 10}}, 0)
	b.must(err)
	expires := b.clock.Now().Add(time.Hour)
	b.must(cp.PutAPIToken(ctx, entity.APIToken{ID: uuid.New(), Account: alice, Hash: [32]byte{1}, Expires: expires}))
	b.must(cp.PutSession(ctx, entity.Session{ID: uuid.New(), Account: alice, Refresh: [32]byte{2}, Expires: expires}))
	b.must(cp.Write(ctx, entity.WriteMeta{}, func(tx entity.ControlPlaneTx) error {
		return tx.PutSignInFailures(entity.SignInKey{Account: alice}, entity.SignInFailures{Count: 1})
	}))
	if n := b.keysNaming(alice.ID[:]); n < 8 {
		t.Fatalf("keys naming alice before her deletion: got %d, want her record, grant, membership, quota, token, session and count", n)
	}
	op := b.deleteUser(alice, gen)

	b.at(b.runner(nil), 0)
	if got := b.read(op.ID).State; got != entity.OpRunning {
		t.Fatalf("deletion after its first batch: got %v, want running", got)
	}
	if n := b.keysNaming(alice.ID[:]); n == 0 {
		t.Fatalf("keys naming alice after the first batch: got none, want her record kept for the last batch")
	}

	b.restart()
	b.at(b.runner(nil), time.Second)
	if got := b.read(op.ID); got.State != entity.OpSucceeded {
		t.Fatalf("deletion resumed after a restart: got %v, want succeeded", got.State)
	}
	if n := b.keysNaming(alice.ID[:]); n != 0 {
		t.Errorf("keys naming alice after her deletion: got %d, want none", n)
	}
	if a, err := b.store.Principals().Grant(ctx, sh.ID, bob); err != nil || a != entity.AccessRead {
		t.Errorf("bob's grant after alice's deletion: got %v, %v; want read", a, err)
	}
}

// A turn whose step answers the cursor recorded commits nothing; one whose
// cursor moved commits once.
func TestUnchangedTurnCommitsNothing(t *testing.T) {
	counting := new(kv.Counting)
	b := newBench(t, "", func(db kv.KV) kv.KV { counting.KV = db; return counting })
	next := []byte("a")
	steps := map[string]stepRow{"k": {run: func(context.Context, entity.Operation) (step.StepResult, error) {
		return step.StepResult{Next: next}, nil
	}}}
	op := b.operation("k", entity.ResourceRef{Kind: entity.ResourceShare, ID: []byte{1}})
	r := b.runner(steps)
	for i, c := range []struct {
		next    string
		commits int
	}{{"a", 1}, {"a", 0}, {"a", 0}, {"b", 1}} {
		next = []byte(c.next)
		counting.TakeUpdates()
		b.at(r, time.Duration(i)*time.Second)
		if got := counting.TakeUpdates().Updates; got != c.commits {
			t.Errorf("turn %d answering cursor %q: got %d commits, want %d", i+1, c.next, got, c.commits)
		}
	}
	if got := b.read(op.ID).Step; string(got) != "b" {
		t.Errorf("recorded cursor: got %q, want %q", got, "b")
	}
}

// A state change whose audit record fails to commit leaves neither: the
// operation keeps its state and has no audit record, until a later turn
// commits both, audited as the schedule's on this node.
func TestStateChangeCommitsWithItsAudit(t *testing.T) {
	var f *kvtest.FaultKV
	b := newBench(t, "", faults(&f))
	alice, gen := b.user("alice")
	op := b.deleteUser(alice, gen)
	r := b.runner(nil)
	failed := errors.New("audit write failed")
	f.SetFault(schedule(t, sim.Rule{Op: kvtest.OpSet, Key: "AU‖*", Count: 1, Action: sim.FaultAction{Err: failed}}))

	b.at(r, 0)
	if got := b.read(op.ID).State; got != entity.OpPending {
		t.Errorf("operation after its end's audit record failed: got %v, want pending", got)
	}
	if got := b.opAudits(op.ID); len(got) != 0 {
		t.Errorf("audit records after a state change that did not commit: got %d, want none", len(got))
	}

	b.at(r, time.Second)
	if got := b.read(op.ID).State; got != entity.OpSucceeded {
		t.Errorf("operation after its end committed: got %v, want succeeded", got)
	}
	got := b.opAudits(op.ID)
	if len(got) != 1 {
		t.Fatalf("audit records of the ended operation: got %d, want 1", len(got))
	}
	want := `{"state":{"old":"pending","new":"succeeded"}}`
	if a := got[0]; !a.Actor.IsZero() || a.Node != runnerNode || string(a.Change) != want {
		t.Errorf("audit record of the end: got actor %v, node %v, change %s; want the schedule, %v, %s", a.Actor, a.Node, a.Change, runnerNode, want)
	}
}

// An end whose writes and the operation's end fail to commit leaves both
// out: the operation is not ended and the setting its End writes is absent,
// until a later turn commits both.
func TestEndCommitsWithTheOperation(t *testing.T) {
	var f *kvtest.FaultKV
	b := newBench(t, "", faults(&f))
	alice, gen := b.user("alice")
	op := b.deleteUser(alice, gen)
	setting := entity.Setting{Scope: entity.SettingScope{Kind: entity.ScopeInstallation}, Key: "ended", Value: []byte("yes")}
	deletion := operationSteps(b.store.Nodes())[string(apiv1.OpUserDelete)]
	steps := map[string]stepRow{string(apiv1.OpUserDelete): {run: func(ctx context.Context, op entity.Operation) (step.StepResult, error) {
		res, err := deletion.run(ctx, op)
		res.End = func(tx entity.ControlPlaneTx) error {
			_, err := tx.PutSetting(setting, 0)
			return err
		}
		return res, err
	}}}
	r := b.runner(steps)
	settings := func() int {
		n := 0
		for s, err := range b.store.ControlPlane().Settings(t.Context(), setting.Scope) {
			b.must(err)
			if s.Key == setting.Key {
				n++
			}
		}
		return n
	}
	failed := errors.New("operation write failed")
	f.SetFault(schedule(t, sim.Rule{Op: kvtest.OpSet, Key: "OP‖*", Count: 1, Action: sim.FaultAction{Err: failed}}))

	b.at(r, 0)
	if got := b.read(op.ID).State; got.Ended() {
		t.Errorf("operation after its end failed to commit: got %v, want not ended", got)
	}
	if n := settings(); n != 0 {
		t.Errorf("settings the End wrote after the end failed to commit: got %d, want none", n)
	}

	b.at(r, time.Second)
	if got := b.read(op.ID).State; got != entity.OpSucceeded {
		t.Errorf("operation after its end committed: got %v, want succeeded", got)
	}
	if n := settings(); n != 1 {
		t.Errorf("settings the End wrote after the end committed: got %d, want 1", n)
	}
}

// A step's terminal error fails the operation with it as Error; a retried
// one leaves the state and cursor as recorded; a done step records its
// Result.
func TestStepAnswerRecorded(t *testing.T) {
	gone := &apiv1.Problem{Status: 404, Code: "not_found", Title: "no such user"}
	goneBody, _ := json.Marshal(gone)
	for _, c := range []struct {
		name   string
		res    step.StepResult
		err    error
		state  entity.OpState
		step   string
		error  string
		result string
	}{
		{"a step failure", step.StepResult{Next: []byte("s2")}, step.ErrStepFailed, entity.OpFailed, "s1",
			`{"status":500,"code":"","title":"the operation failed","detail":"step failed","retryable":false,"request_id":""}`, ""},
		{"a problem", step.StepResult{}, gone, entity.OpFailed, "s1", string(goneBody), ""},
		{"a retried error", step.StepResult{Next: []byte("s2")}, errors.New("store unreachable"), entity.OpRunning, "s1", "", ""},
		{"done", step.StepResult{Next: []byte("s2"), Done: true, Result: json.RawMessage(`{"id":7}`)}, nil, entity.OpSucceeded, "s2", "", `{"id":7}`},
	} {
		t.Run(c.name, func(t *testing.T) {
			b := newBench(t, "", nil)
			op := entity.Operation{ID: entity.OperationID(uuid.New()), Kind: "k", Target: entity.ResourceRef{Kind: entity.ResourceShare, ID: []byte{1}},
				State: entity.OpRunning, Step: []byte("s1")}
			_, err := b.store.ControlPlane().PutOperation(t.Context(), op, 0)
			b.must(err)
			b.at(b.runner(map[string]stepRow{"k": {run: func(context.Context, entity.Operation) (step.StepResult, error) { return c.res, c.err }}}), 0)
			got := b.read(op.ID)
			if got.State != c.state || string(got.Step) != c.step || string(got.Error) != c.error || string(got.Result) != c.result {
				t.Errorf("recorded: got state %v, step %q, error %s, result %s; want %v, %q, %s, %s",
					got.State, got.Step, got.Error, got.Result, c.state, c.step, c.error, c.result)
			}
		})
	}
}

// A user's deletion fails, rather than retrying, on a user its deletion did
// not mark and on a target that names no user; a user already gone ends it.
func TestUserDeletionStepAnswers(t *testing.T) {
	for _, c := range []struct {
		name   string
		target func(b *bench) entity.ResourceRef
		state  entity.OpState
	}{
		{"an unmarked user", func(b *bench) entity.ResourceRef {
			u, _ := b.user("alice")
			return entity.ResourceRef{Kind: entity.ResourceUser, ID: u.ID[:]}
		}, entity.OpFailed},
		{"a group's ID", func(b *bench) entity.ResourceRef {
			g := entity.NewGroup()
			return entity.ResourceRef{Kind: entity.ResourceGroup, ID: g.ID[:]}
		}, entity.OpFailed},
		{"a short ID", func(*bench) entity.ResourceRef { return entity.ResourceRef{Kind: entity.ResourceUser, ID: []byte{1}} }, entity.OpFailed},
		{"a user already gone", func(*bench) entity.ResourceRef {
			u := entity.NewUser()
			return entity.ResourceRef{Kind: entity.ResourceUser, ID: u.ID[:]}
		}, entity.OpSucceeded},
	} {
		t.Run(c.name, func(t *testing.T) {
			b := newBench(t, "", nil)
			op := b.operation(string(apiv1.OpUserDelete), c.target(b))
			b.at(b.runner(nil), 0)
			if got := b.read(op.ID); got.State != c.state {
				t.Errorf("deletion of %s: got %v (error %s), want %v", c.name, got.State, got.Error, c.state)
			}
		})
	}
}

// A group's deletion runs past its mark to its end, leaving no row naming
// the group.
func TestGroupDeletionEnds(t *testing.T) {
	b := newBench(t, "", nil)
	ctx, cp := t.Context(), b.store.ControlPlane()
	alice, _ := b.user("alice")
	g := entity.Group{Principal: entity.NewGroup(), Name: "staff"}
	gen, err := cp.PutGroup(ctx, g, 0, entity.Alloc{})
	b.must(err)
	b.must(cp.AddMember(ctx, entity.Membership{Group: g.Principal, Member: alice}))
	op := entity.Operation{ID: entity.OperationID(uuid.New()), Kind: string(apiv1.OpGroupDelete),
		Target: entity.ResourceRef{Kind: entity.ResourceGroup, ID: g.Principal.ID[:]}, State: entity.OpPending}
	b.must(cp.Write(ctx, entity.WriteMeta{}, func(tx entity.ControlPlaneTx) error {
		if err := tx.DeleteGroup(g.Principal, gen); err != nil {
			return err
		}
		_, err := tx.PutOperation(op, 0)
		return err
	}))
	r := b.runner(nil)
	b.at(r, 0)
	b.at(r, time.Second)
	if got := b.read(op.ID); got.State != entity.OpSucceeded {
		t.Errorf("group deletion after two batches: got %v (error %s), want succeeded", got.State, got.Error)
	}
	if n := b.keysNaming(g.Principal.ID[:]); n != 0 {
		t.Errorf("keys naming the group after its deletion: got %d, want none", n)
	}
}

// A step that keeps failing with a retried error runs again 1 s after its
// first failure, the wait doubling at each failure up to 30 s.
func TestRetriedStepBacksOff(t *testing.T) {
	b := newBench(t, "", nil)
	failures := 6
	steps := map[string]stepRow{"k": {run: func(context.Context, entity.Operation) (step.StepResult, error) {
		if failures > 0 {
			failures--
			return step.StepResult{}, errors.New("store unreachable")
		}
		return step.StepResult{Done: true}, nil
	}}}
	op := b.operation("k", entity.ResourceRef{Kind: entity.ResourceShare, ID: []byte{1}})
	r := b.runner(steps)
	// Failures at 0, 1, 3, 7, 15 and 31 s, waiting 1, 2, 4, 8, 16 then 30 s.
	for _, at := range []time.Duration{0, time.Second, 3 * time.Second, 7 * time.Second, 15 * time.Second, 31 * time.Second} {
		b.at(r, at)
	}
	b.at(r, 61*time.Second-time.Nanosecond)
	if got := b.read(op.ID).State; got != entity.OpPending {
		t.Fatalf("operation before its last wait ended: got %v, want pending", got)
	}
	b.at(r, 61*time.Second)
	if got := b.read(op.ID).State; got != entity.OpSucceeded {
		t.Errorf("operation once its last wait ended: got %v, want succeeded", got)
	}
}

// An operation of a kind with no step stays pending, unchanged.
func TestKindWithNoStepStaysPending(t *testing.T) {
	b := newBench(t, "", nil)
	op := b.operation(string(apiv1.OpRotateSigningKey), entity.ResourceRef{Kind: entity.ResourceInstallation, ID: []byte{1}})
	b.at(b.runner(nil), 0)
	if got := b.read(op.ID); got.State != entity.OpPending || got.Generation != 1 {
		t.Errorf("operation of a kind with no step: got %v at generation %d, want pending at 1", got.State, got.Generation)
	}
}

// The running loop waits a second on its clock between turns, picks up an
// operation created meanwhile at the next turn, and returns when its context
// ends.
func TestRunTurnsOnTheClock(t *testing.T) {
	prof.Watch(t, prof.Options{})
	b := newBench(t, "", nil)
	ctx, cancel := context.WithCancel(t.Context())
	r := b.runner(map[string]stepRow{"k": {run: func(context.Context, entity.Operation) (step.StepResult, error) {
		return step.StepResult{Done: true}, nil
	}}})
	done := make(chan struct{})
	go func() {
		defer close(done)
		r.run(ctx)
	}()
	waitFor(t, "the loop to wait on its clock", func() bool { return b.clock.Pending() > 0 })
	op := b.operation("k", entity.ResourceRef{Kind: entity.ResourceShare, ID: []byte{1}})
	if got := b.read(op.ID).State; got != entity.OpPending {
		t.Errorf("operation created while the loop waits: got %v, want pending until the clock moves", got)
	}
	b.clock.Advance(runInterval)
	waitFor(t, "the next turn to end the operation", func() bool { return b.read(op.ID).State == entity.OpSucceeded })
	cancel()
	<-done
}

// waitFor polls cond until it holds, failing after a backstop.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}
