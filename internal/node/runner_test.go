package node

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cubbit/dittofs/internal/node/step"
	"github.com/cubbit/dittofs/internal/prof"
	"github.com/cubbit/dittofs/pkg/health"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	merrs "github.com/cubbit/dittofs/pkg/metadata/errors"
	"github.com/cubbit/dittofs/pkg/metadata/kv"
	"github.com/cubbit/dittofs/pkg/metadata/kv/badgerkv"
	"github.com/cubbit/dittofs/pkg/metadata/kv/keys"
	"github.com/cubbit/dittofs/pkg/metadata/kv/kvtest"
	"github.com/cubbit/dittofs/pkg/metadata/kvstore"
	"github.com/cubbit/dittofs/pkg/metadata/kvstore/kvstoretest"
	"github.com/cubbit/dittofs/pkg/node/apiv1"
	"github.com/cubbit/dittofs/pkg/sim"
)

var (
	runnerNode = entity.NodeID{'r', 'u', 'n'}
	aShare     = entity.ResourceRef{Kind: entity.ResourceShare, ID: []byte{1}}
)

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
	b.must("open Badger", err)
	b.t.Cleanup(func() { _ = db.Close() })
	b.db = db
	kvs := b.wrap(db)
	if init {
		b.must("init the store", kvstore.Init(b.t.Context(), kvs, uuid.New()))
	}
	b.store, err = kvstore.Open(b.t.Context(), kvs)
	b.must("open the store", err)
}

// restart closes Badger and opens the store again over the same directory.
func (b *bench) restart() {
	b.t.Helper()
	b.must("close Badger", b.db.Close())
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

// must fails the test when step's err is set.
func (b *bench) must(step string, err error) {
	b.t.Helper()
	if err != nil {
		b.t.Fatalf("%s: %v", step, err)
	}
}

// operation records op, under a new ID, and returns it as recorded.
func (b *bench) operation(op entity.Operation) entity.Operation {
	b.t.Helper()
	op.ID = entity.OperationID(uuid.New())
	_, err := b.store.ControlPlane().PutOperation(b.t.Context(), op, 0)
	b.must("create the operation", err)
	return b.read(op.ID)
}

// markDeleted marks a principal with del and creates kind's pending
// deletion of ref in one write, as the management API's delete does.
func (b *bench) markDeleted(kind apiv1.OpKind, ref entity.ResourceRef, account entity.Principal, del func(entity.ControlPlaneTx) error) entity.Operation {
	b.t.Helper()
	op := entity.Operation{ID: entity.OperationID(uuid.New()), Kind: string(kind), Target: ref, Account: account, State: entity.OpPending}
	b.must("mark the principal and create its deletion", b.store.ControlPlane().Write(b.t.Context(), entity.WriteMeta{}, func(tx entity.ControlPlaneTx) error {
		if err := del(tx); err != nil {
			return err
		}
		_, err := tx.PutOperation(op, 0)
		return err
	}))
	return b.read(op.ID)
}

// deleteUser marks u and creates its pending deletion, started by an
// account.
func (b *bench) deleteUser(u entity.Principal, gen uint64) entity.Operation {
	b.t.Helper()
	return b.markDeleted(apiv1.OpUserDelete, entity.ResourceRef{Kind: entity.ResourceUser, ID: u.ID[:]}, entity.NewUser(),
		func(tx entity.ControlPlaneTx) error { return tx.DeleteUser(u, gen) })
}

// groupIDs allocates the gid and SID every group holds.
var groupIDs = entity.Alloc{GID: true, RID: true, Range: entity.IDRange{Lo: 1_000_000, Hi: 1_999_999_999}}

// user creates a user and returns it with its generation.
func (b *bench) user(u entity.User) (entity.Principal, uint64) {
	b.t.Helper()
	u.Principal = entity.NewUser()
	gen, err := b.store.ControlPlane().PutUser(b.t.Context(), u, 0, entity.Alloc{})
	b.must("create user "+u.Name, err)
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

// keys counts the store's keys that match.
func (b *bench) keys(match func(key []byte) bool) int {
	b.t.Helper()
	n := 0
	b.must("scan the store", b.db.View(b.t.Context(), func(r kv.Reader) error {
		for row, err := range r.Scan(nil, nil) {
			if err != nil {
				return err
			}
			if match(row.Key) {
				n++
			}
		}
		return nil
	}))
	return n
}

// keysNaming counts the store's keys holding id's bytes.
func (b *bench) keysNaming(id []byte) int {
	return b.keys(func(key []byte) bool { return bytes.Contains(key, id) })
}

// opAudits is the audit records whose target is op.
func (b *bench) opAudits(op entity.OperationID) []entity.AuditRecord {
	b.t.Helper()
	target := entity.ResourceRef{Kind: entity.ResourceOperation, ID: op[:]}
	var out []entity.AuditRecord
	for a, err := range b.store.ControlPlane().AuditRecords(b.t.Context(), entity.AuditFilter{Target: &target}, nil) {
		b.must("list the operation's audit records", err)
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
		t.Fatalf("build the fault schedule: %v", err)
	}
	return s
}

// failUpdates fails the next n store updates with err.
func failUpdates(n int, err error) sim.Rule {
	return sim.Rule{Op: kvtest.OpUpdate, Key: "*", Count: n, Action: sim.FaultAction{Err: err}}
}

// A user's deletion stopped between two batches, its store closed and
// reopened under a new runner, resumes from the records and ends succeeded
// with no row naming the user or her secret left, and another user's grant
// kept.
func TestUserDeletionResumesAcrossRestart(t *testing.T) {
	b := newBench(t, t.TempDir(), nil)
	ctx, cp := t.Context(), b.store.ControlPlane()
	secret := entity.SecretRef{15: 1}
	alice, gen := b.user(entity.User{Name: "alice", Password: secret})
	bob, _ := b.user(entity.User{Name: "bob"})
	secretPrefix := keys.ID([]byte{keys.KindSEC}, secret[:])
	b.must("write alice's secret", cp.PutSecret(ctx, entity.Secret{
		ID: secret, Kind: entity.SecretPassword, Version: 1, KeyID: "storage-1", Value: []byte{1},
		Owner: entity.UserSecretOwner(alice.ID, entity.UserPasswordKey),
	}, 0))
	g := entity.Group{Principal: entity.NewGroup(), Name: "staff"}
	_, err := cp.PutGroup(ctx, g, 0, groupIDs)
	b.must("create group staff", err)
	sh := entity.Share{ID: entity.NewShareID(), Name: "photos", Path: "/photos", Root: entity.NewFileID()}
	_, err = kvstoretest.CreateShare(ctx, cp, sh)
	b.must("create share photos", err)
	for _, p := range []entity.Principal{alice, bob} {
		_, err = cp.SetGrant(ctx, entity.ShareGrant{Share: sh.ID, Principal: p, Access: entity.AccessRead}, 0)
		b.must("grant photos", err)
	}
	b.must("add alice to staff", cp.AddMember(ctx, entity.Membership{Group: g.Principal, Member: alice}))
	_, err = cp.SetQuota(ctx, sh.ID, entity.Quota{Principal: alice, Hard: entity.Limit{Files: 10}}, 0)
	b.must("set alice's quota", err)
	expires := b.clock.Now().Add(time.Hour)
	b.must("put alice's token", cp.PutAPIToken(ctx, entity.APIToken{ID: uuid.New(), Account: alice, Hash: [32]byte{1}, Expires: expires}))
	b.must("put alice's session", cp.PutSession(ctx, entity.Session{ID: uuid.New(), Account: alice, Refresh: [32]byte{2}, Expires: expires}))
	b.must("count alice's sign-in failure", cp.Write(ctx, entity.WriteMeta{}, func(tx entity.ControlPlaneTx) error {
		return tx.PutSignInFailures(entity.SignInKey{Account: alice}, entity.SignInFailures{Count: 1})
	}))
	if n := b.keysNaming(alice.ID[:]); n < 8 {
		t.Fatalf("keys naming alice before her deletion: got %d, want her record, grant, membership, quota, token, session and count", n)
	}
	inSecret := func(key []byte) bool { return bytes.HasPrefix(key, secretPrefix) }
	if n := b.keys(inSecret); n != 1 {
		t.Fatalf("versions of alice's secret before her deletion: got %d, want 1", n)
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
	if n := b.keys(inSecret); n != 0 {
		t.Errorf("versions of alice's secret after her deletion: got %d, want none", n)
	}
	if a, ok, err := b.store.Principals().Grant(ctx, sh.ID, bob); err != nil || !ok || a != entity.AccessRead {
		t.Errorf("bob's grant after alice's deletion: got %v, %v, %v; want read, true", a, ok, err)
	}
}

// A turn whose step answers the cursor recorded commits nothing; one whose
// cursor moved commits once.
func TestUnchangedTurnCommitsNothing(t *testing.T) {
	counting := new(kv.Counting)
	b := newBench(t, "", func(db kv.KV) kv.KV { counting.KV = db; return counting })
	next := []byte("a")
	steps := map[string]stepRow{"k": {progress: noPhase, run: func(context.Context, entity.Operation) (step.StepResult, error) {
		return step.StepResult{Next: next}, nil
	}}}
	op := b.operation(entity.Operation{Kind: "k", Target: aShare, State: entity.OpPending})
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

// noPhase is the progress of a step whose phase never moves.
func noPhase(context.Context, entity.Operation) apiv1.Progress { return apiv1.Progress{} }

// A turn whose step moves the phase its progress reads, with its cursor
// unchanged, commits once with an audit record naming both phases; a turn
// whose phase stays commits nothing.
func TestPhaseChangeCommitsWithItsAudit(t *testing.T) {
	counting := new(kv.Counting)
	b := newBench(t, "", func(db kv.KV) kv.KV { counting.KV = db; return counting })
	phase, moveTo := "", ""
	steps := map[string]stepRow{"k": {
		run: func(context.Context, entity.Operation) (step.StepResult, error) {
			phase = moveTo
			return step.StepResult{Next: []byte("a")}, nil
		},
		progress: func(context.Context, entity.Operation) apiv1.Progress { return apiv1.Progress{Phase: phase} },
	}}
	op := b.operation(entity.Operation{Kind: "k", Target: aShare, State: entity.OpPending})
	r := b.runner(steps)
	b.at(r, 0)
	for i, c := range []struct {
		to      string
		commits int
		change  string
	}{{"scan", 1, `{"phase":{"old":"","new":"scan"}}`}, {"scan", 0, ""}, {"copy", 1, `{"phase":{"old":"scan","new":"copy"}}`}} {
		moveTo = c.to
		audits := len(b.opAudits(op.ID))
		counting.TakeUpdates()
		b.at(r, time.Duration(i+1)*time.Second)
		if got := counting.TakeUpdates().Updates; got != c.commits {
			t.Errorf("turn moving the phase to %q: got %d commits, want %d", c.to, got, c.commits)
		}
		got := b.opAudits(op.ID)[audits:]
		switch {
		case c.change == "" && len(got) != 0:
			t.Errorf("turn keeping phase %q: got audit records %v, want none", c.to, got)
		case c.change != "" && (len(got) != 1 || string(got[0].Change) != c.change):
			t.Errorf("turn moving the phase to %q: got audit records %v, want one with change %s", c.to, got, c.change)
		}
	}
}

// A state change whose audit record fails to commit leaves out the change,
// its audit record and its End's writes, and is retried after a wait; the
// retry commits all three, audited as the account that started the
// operation, on this node.
func TestStateChangeCommitsWithItsAudit(t *testing.T) {
	var f *kvtest.FaultKV
	b := newBench(t, "", faults(&f))
	alice, gen := b.user(entity.User{Name: "alice"})
	op := b.deleteUser(alice, gen)
	setting := entity.Setting{Scope: entity.SettingScope{Kind: entity.ScopeInstallation}, Key: "ended", Value: []byte("yes")}
	deletion := operationSteps(b.store.Nodes())[string(apiv1.OpUserDelete)]
	r := b.runner(map[string]stepRow{string(apiv1.OpUserDelete): {progress: noPhase, run: func(ctx context.Context, op entity.Operation) (step.StepResult, error) {
		res, err := deletion.run(ctx, op)
		res.End = func(tx entity.ControlPlaneTx) error {
			_, err := tx.PutSetting(setting, 0)
			return err
		}
		return res, err
	}}})
	settings := func() int {
		n := 0
		for s, err := range b.store.ControlPlane().Settings(t.Context(), setting.Scope) {
			b.must("list the settings", err)
			if s.Key == setting.Key {
				n++
			}
		}
		return n
	}
	f.SetFault(schedule(t, sim.Rule{Op: kvtest.OpSet, Key: "AU‖*", Count: 1, Action: sim.FaultAction{Err: errors.New("audit write failed")}}))

	for _, at := range []time.Duration{0, firstRetry - time.Nanosecond} {
		b.at(r, at)
		if got := b.read(op.ID); got.State != entity.OpPending || got.Generation != op.Generation {
			t.Errorf("operation at %v, after its end's audit record failed: got %v at generation %d, want pending at %d", at, got.State, got.Generation, op.Generation)
		}
		if got := b.opAudits(op.ID); len(got) != 0 {
			t.Errorf("audit records at %v, after a state change that did not commit: got %d, want none", at, len(got))
		}
		if n := settings(); n != 0 {
			t.Errorf("settings the End wrote at %v, after the end failed to commit: got %d, want none", at, n)
		}
	}

	b.at(r, firstRetry)
	ended := b.read(op.ID)
	if ended.State != entity.OpSucceeded {
		t.Errorf("operation after its end committed: got %v, want succeeded", ended.State)
	}
	if n := settings(); n != 1 {
		t.Errorf("settings the End wrote after the end committed: got %d, want 1", n)
	}
	got := b.opAudits(op.ID)
	if len(got) != 1 {
		t.Fatalf("audit records of the ended operation: got %d, want 1", len(got))
	}
	a, change := got[0], `{"state":{"old":"pending","new":"succeeded"}}`
	if a.Actor != op.Account || a.Node != runnerNode || string(a.Change) != change {
		t.Errorf("audit record of the end: got actor %v, node %v, change %s; want the starting account %v, %v, %s", a.Actor, a.Node, a.Change, op.Account, runnerNode, change)
	}
	if a.Before != op.Generation || a.After != ended.Generation || string(a.Outcome) != "applied" {
		t.Errorf("audit record of the end: got generations %d to %d, outcome %s; want %d to %d, applied", a.Before, a.After, a.Outcome, op.Generation, ended.Generation)
	}
}

// A done step whose End panics, or answers a step failure or a problem,
// fails the operation as the step's own would, without a panic reaching the
// runner's caller: the End's writes roll back, the cursor stays as recorded,
// and the failure is audited.
func TestEndPanicFailsOperation(t *testing.T) {
	gone := &apiv1.Problem{Status: 404, Code: "not_found", Title: "no such user"}
	goneBody, _ := json.Marshal(gone)
	for _, c := range []struct {
		name  string
		end   func() error
		error string
	}{
		{"a panic", func() error { panic("boom") },
			`{"status":500,"title":"the operation failed","detail":"step failed: operation end: panic: boom","retryable":false,"request_id":""}`},
		{"a step failure", func() error { return fmt.Errorf("%w: x", step.ErrStepFailed) },
			`{"status":500,"title":"the operation failed","detail":"step failed: x","retryable":false,"request_id":""}`},
		{"a problem", func() error { return gone }, string(goneBody)},
	} {
		t.Run(c.name, func(t *testing.T) {
			b := newBench(t, "", nil)
			op := b.operation(entity.Operation{Kind: "k", Target: aShare, State: entity.OpRunning, Step: []byte("s1")})
			setting := entity.Setting{Scope: entity.SettingScope{Kind: entity.ScopeInstallation}, Key: "ended", Value: []byte("yes")}
			r := b.runner(map[string]stepRow{"k": {progress: noPhase, run: func(context.Context, entity.Operation) (step.StepResult, error) {
				return step.StepResult{Next: []byte("s2"), Done: true, End: func(tx entity.ControlPlaneTx) error {
					if _, err := tx.PutSetting(setting, 0); err != nil {
						return err
					}
					return c.end()
				}}, nil
			}}})

			b.at(r, 0)

			got := b.read(op.ID)
			if got.State != entity.OpFailed || string(got.Step) != "s1" || string(got.Error) != c.error || got.Result != nil {
				t.Errorf("operation whose End answered %s: got state %v, step %q, error %s, result %s; want failed, \"s1\", %s, none", c.name, got.State, got.Step, got.Error, got.Result, c.error)
			}
			for s, err := range b.store.ControlPlane().Settings(t.Context(), setting.Scope) {
				b.must("list the settings", err)
				if s.Key == setting.Key {
					t.Errorf("setting the failing End wrote: got %q, want it rolled back", s.Value)
				}
			}
			if a, change := b.opAudits(op.ID), `{"state":{"old":"running","new":"failed"}}`; len(a) != 1 || string(a[0].Change) != change {
				t.Errorf("audit records of the failed end: got %v, want one with change %s", a, change)
			}
		})
	}
}

// A step's terminal error, its problem, wrapped or not, and its panic fail
// the operation with it as Error; a cancelled context's error is retried,
// leaving the state and cursor as recorded; a done step records its Result.
func TestStepAnswerRecorded(t *testing.T) {
	gone := &apiv1.Problem{Status: 404, Code: "not_found", Title: "no such user"}
	goneBody, _ := json.Marshal(gone)
	for _, c := range []struct {
		name   string
		res    step.StepResult
		err    error
		panics any
		state  entity.OpState
		step   string
		error  string
		result string
	}{
		{"a step failure", step.StepResult{Next: []byte("s2")}, step.ErrStepFailed, nil, entity.OpFailed, "s1",
			`{"status":500,"title":"the operation failed","detail":"step failed","retryable":false,"request_id":""}`, ""},
		{"a problem", step.StepResult{}, gone, nil, entity.OpFailed, "s1", string(goneBody), ""},
		{"a wrapped problem", step.StepResult{}, fmt.Errorf("batch: %w", gone), nil, entity.OpFailed, "s1", string(goneBody), ""},
		{"a panic", step.StepResult{}, nil, "boom", entity.OpFailed, "s1",
			`{"status":500,"title":"the operation failed","detail":"step failed: panic: boom","retryable":false,"request_id":""}`, ""},
		{"a cancelled context", step.StepResult{Next: []byte("s2")}, context.Canceled, nil, entity.OpRunning, "s1", "", ""},
		{"done", step.StepResult{Next: []byte("s2"), Done: true, Result: json.RawMessage(`{"id":7}`)}, nil, nil, entity.OpSucceeded, "s2", "", `{"id":7}`},
	} {
		t.Run(c.name, func(t *testing.T) {
			b := newBench(t, "", nil)
			op := b.operation(entity.Operation{Kind: "k", Target: aShare, State: entity.OpRunning, Step: []byte("s1")})
			b.at(b.runner(map[string]stepRow{"k": {progress: noPhase, run: func(context.Context, entity.Operation) (step.StepResult, error) {
				if c.panics != nil {
					panic(c.panics)
				}
				return c.res, c.err
			}}}), 0)
			got := b.read(op.ID)
			if got.State != c.state || string(got.Step) != c.step || string(got.Error) != c.error || string(got.Result) != c.result {
				t.Errorf("recorded: got state %v, step %q, error %s, result %s; want %v, %q, %s, %s",
					got.State, got.Step, got.Error, got.Result, c.state, c.step, c.error, c.result)
			}
		})
	}
}

// A step whose context ends while it runs commits nothing, even a failure.
func TestInterruptedStepCommitsNothing(t *testing.T) {
	b := newBench(t, "", nil)
	op := b.operation(entity.Operation{Kind: "k", Target: aShare, State: entity.OpRunning, Step: []byte("s1")})
	ctx, cancel := context.WithCancel(t.Context())
	r := b.runner(map[string]stepRow{"k": {progress: noPhase, run: func(context.Context, entity.Operation) (step.StepResult, error) {
		cancel()
		return step.StepResult{}, step.ErrStepFailed
	}}})
	r.turn(ctx)
	if got := b.read(op.ID); got.State != entity.OpRunning || got.Generation != op.Generation {
		t.Errorf("operation whose step was interrupted: got %v at generation %d, want running at %d", got.State, got.Generation, op.Generation)
	}
}

// A user's deletion fails on a user its deletion did not mark, on a target
// that names no user and on a user whose current password version does not
// decode, and ends at once on a user already gone. A
// store error, a transaction too large included, leaves it pending until a
// retry a second later runs it again.
func TestUserDeletionStepAnswers(t *testing.T) {
	userRef := func(u entity.Principal) entity.ResourceRef {
		return entity.ResourceRef{Kind: entity.ResourceUser, ID: u.ID[:]}
	}
	deletion := func(ref entity.ResourceRef) func(*bench) entity.Operation {
		return func(b *bench) entity.Operation {
			return b.operation(entity.Operation{Kind: string(apiv1.OpUserDelete), Target: ref, State: entity.OpPending})
		}
	}
	group := entity.NewGroup()
	marked := func(b *bench) entity.Operation {
		u, gen := b.user(entity.User{Name: "alice"})
		return b.deleteUser(u, gen)
	}
	for _, c := range []struct {
		name         string
		op           func(b *bench) entity.Operation
		fault        error
		first, retry entity.OpState
	}{
		{"an unmarked user", func(b *bench) entity.Operation {
			u, _ := b.user(entity.User{Name: "alice"})
			return deletion(userRef(u))(b)
		}, nil, entity.OpFailed, entity.OpFailed},
		{"a group's ID", deletion(entity.ResourceRef{Kind: entity.ResourceGroup, ID: group.ID[:]}), nil, entity.OpFailed, entity.OpFailed},
		{"a short ID", deletion(entity.ResourceRef{Kind: entity.ResourceUser, ID: []byte{1}}), nil, entity.OpFailed, entity.OpFailed},
		{"an undecodable current password", func(b *bench) entity.Operation {
			pw := entity.SecretRef{15: 1}
			u, gen := b.user(entity.User{Name: "alice", Password: pw})
			b.must("write alice's undecodable password", b.db.Update(b.t.Context(), func(tx kv.Txn) error {
				return tx.Set(keys.Uint64(keys.ID([]byte{keys.KindSEC}, pw[:]), 1), []byte("not a secret"))
			}))
			return b.deleteUser(u, gen)
		}, nil, entity.OpFailed, entity.OpFailed},
		{"a user already gone", deletion(userRef(entity.NewUser())), nil, entity.OpSucceeded, entity.OpSucceeded},
		{"a store error", marked, errors.New("store unreachable"), entity.OpPending, entity.OpSucceeded},
		{"a transaction too large", marked, kv.ErrTooLarge, entity.OpPending, entity.OpSucceeded},
	} {
		t.Run(c.name, func(t *testing.T) {
			var f *kvtest.FaultKV
			b := newBench(t, "", faults(&f))
			op := c.op(b)
			if c.fault != nil {
				f.SetFault(schedule(t, failUpdates(1, c.fault)))
			}
			r := b.runner(nil)
			for _, turn := range []struct {
				at   time.Duration
				want entity.OpState
			}{{0, c.first}, {firstRetry - time.Nanosecond, c.first}, {firstRetry, c.retry}} {
				b.at(r, turn.at)
				if got := b.read(op.ID); got.State != turn.want {
					t.Errorf("deletion of %s at %v: got %v (error %s), want %v", c.name, turn.at, got.State, got.Error, turn.want)
				}
			}
		})
	}
}

// A machine DeleteUser marked is deleted by the user deletion's step: the
// operation ends succeeded with no row naming the machine, and its Kerberos
// name finds nothing.
func TestMachineDeletionEnds(t *testing.T) {
	b := newBench(t, "", nil)
	ctx, cp := t.Context(), b.store.ControlPlane()
	name := entity.ProtocolID{Scheme: entity.SchemeKerberos, Value: "host/build01.corp.example@CORP.EXAMPLE"}
	m := entity.User{Principal: entity.Principal{Kind: entity.PrincipalMachine, ID: entity.NewPrincipalID()}, IDs: []entity.ProtocolID{name}}
	gen, err := cp.PutUser(ctx, m, 0, entity.Alloc{})
	b.must("create the machine", err)
	op := b.deleteUser(m.Principal, gen)
	r := b.runner(nil)
	for turn := time.Duration(0); turn < 10*time.Second && b.read(op.ID).State != entity.OpSucceeded; turn += time.Second {
		b.at(r, turn)
	}
	if got := b.read(op.ID); got.State != entity.OpSucceeded {
		t.Fatalf("machine deletion: got %v (error %s), want succeeded", got.State, got.Error)
	}
	if n := b.keysNaming(m.Principal.ID[:]); n != 0 {
		t.Errorf("keys naming the machine after its deletion: got %d, want none", n)
	}
	if p, err := b.store.Principals().ByProtocolID(ctx, name); !merrs.IsNotFoundError(err) {
		t.Errorf("ByProtocolID(%s) after the machine's deletion: %v, %v; want not found", name.Value, p, err)
	}
}

// A group's deletion runs past its mark to its end, leaving no row naming
// the group.
func TestGroupDeletionEnds(t *testing.T) {
	b := newBench(t, "", nil)
	ctx, cp := t.Context(), b.store.ControlPlane()
	alice, _ := b.user(entity.User{Name: "alice"})
	g := entity.Group{Principal: entity.NewGroup(), Name: "staff"}
	gen, err := cp.PutGroup(ctx, g, 0, groupIDs)
	b.must("create group staff", err)
	b.must("add alice to staff", cp.AddMember(ctx, entity.Membership{Group: g.Principal, Member: alice}))
	op := b.markDeleted(apiv1.OpGroupDelete, entity.ResourceRef{Kind: entity.ResourceGroup, ID: g.Principal.ID[:]}, entity.Principal{},
		func(tx entity.ControlPlaneTx) error { return tx.DeleteGroup(g.Principal, gen) })
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

// Both deletion kinds read their progress as an uncounted run of batches.
func TestDeletionProgress(t *testing.T) {
	steps := operationSteps(kvstore.Nodes{})
	want := apiv1.Progress{Phase: "batches", Total: -1, Unit: "batches"}
	for _, kind := range []apiv1.OpKind{apiv1.OpUserDelete, apiv1.OpGroupDelete} {
		if got := steps[string(kind)].progress(t.Context(), entity.Operation{}); got != want {
			t.Errorf("progress of %s: got %+v, want %+v", kind, got, want)
		}
	}
}

// A deletion whose batches keep failing on a store error runs again 1 s
// after its first failure, the wait doubling at each failure up to 30 s.
func TestRetriedStepBacksOff(t *testing.T) {
	var f *kvtest.FaultKV
	b := newBench(t, "", faults(&f))
	alice, gen := b.user(entity.User{Name: "alice"})
	op := b.deleteUser(alice, gen)
	f.SetFault(schedule(t, failUpdates(6, errors.New("store unreachable"))))
	r := b.runner(nil)
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

// A step that succeeds without changing its operation ends the run of
// failures: the next failure waits 1 s again, not 2.
func TestBackoffRestartsAfterASuccess(t *testing.T) {
	b := newBench(t, "", nil)
	answers := []error{errors.New("first failure"), nil, errors.New("second failure"), nil}
	r := b.runner(map[string]stepRow{"k": {progress: noPhase, run: func(_ context.Context, op entity.Operation) (step.StepResult, error) {
		err := answers[0]
		answers = answers[1:]
		return step.StepResult{Next: op.Step, Done: len(answers) == 0}, err
	}}})
	op := b.operation(entity.Operation{Kind: "k", Target: aShare, State: entity.OpRunning, Step: []byte("s1")})
	for _, at := range []time.Duration{0, time.Second, 2 * time.Second} {
		b.at(r, at)
	}
	b.at(r, 3*time.Second-time.Nanosecond)
	if got := b.read(op.ID).State; got != entity.OpRunning {
		t.Fatalf("operation during the second failure's wait: got %v, want running", got)
	}
	b.at(r, 3*time.Second)
	if got := b.read(op.ID).State; got != entity.OpSucceeded {
		t.Errorf("operation 1 s after the second failure: got %v, want succeeded", got)
	}
}

// Operations of a kind with no step stay pending, unchanged, and the kind
// is listed once among the refused.
func TestKindWithNoStepStaysPending(t *testing.T) {
	b := newBench(t, "", nil)
	kind := string(apiv1.OpRotateSigningKey)
	var ops []entity.Operation
	for i := range 2 {
		ops = append(ops, b.operation(entity.Operation{Kind: kind, Target: entity.ResourceRef{Kind: entity.ResourceInstallation, ID: []byte{byte(i)}}, State: entity.OpPending}))
	}
	r := b.runner(nil)
	b.at(r, 0)
	for _, op := range ops {
		if got := b.read(op.ID); got.State != entity.OpPending || got.Generation != op.Generation {
			t.Errorf("operation of a kind with no step: got %v at generation %d, want pending at %d", got.State, got.Generation, op.Generation)
		}
	}
	if got := r.refusedKinds(); !slices.Equal(got, []string{kind}) {
		t.Errorf("refused kinds: got %q, want %q", got, []string{kind})
	}
}

// An ended operation is not handed to its kind's step; a running one is.
func TestEndedOperationIsNotDriven(t *testing.T) {
	b := newBench(t, "", nil)
	ended := b.operation(entity.Operation{Kind: "k", Target: aShare, State: entity.OpFailed, Error: []byte(`{}`)})
	running := b.operation(entity.Operation{Kind: "k", Target: entity.ResourceRef{Kind: entity.ResourceShare, ID: []byte{2}}, State: entity.OpRunning})
	var driven []entity.OperationID
	b.at(b.runner(map[string]stepRow{"k": {progress: noPhase, run: func(_ context.Context, op entity.Operation) (step.StepResult, error) {
		driven = append(driven, op.ID)
		return step.StepResult{Done: true}, nil
	}}}), 0)
	if want := []entity.OperationID{running.ID}; !slices.Equal(driven, want) {
		t.Errorf("operations handed to the step: got %v, want only the running one %v, not the failed %v", driven, want, ended.ID)
	}
	if got := b.read(ended.ID); got.State != entity.OpFailed || got.Generation != ended.Generation {
		t.Errorf("failed operation after a turn: got %v at generation %d, want failed at %d", got.State, got.Generation, ended.Generation)
	}
}

// The running loop waits a second on its clock between turns, picks up an
// operation created meanwhile at the next turn, and returns when its context
// ends.
func TestRunTurnsOnTheClock(t *testing.T) {
	prof.Watch(t, prof.Options{})
	b := newBench(t, "", nil)
	ctx, cancel := context.WithCancel(t.Context())
	r := b.runner(map[string]stepRow{"k": {progress: noPhase, run: func(context.Context, entity.Operation) (step.StepResult, error) {
		return step.StepResult{Done: true}, nil
	}}})
	done := make(chan struct{})
	go func() {
		defer close(done)
		r.run(ctx, health.NewWatchdog(b.clock).Loop("runner"))
	}()
	waitFor(t, "the loop to wait on its clock", func() bool { return b.clock.Pending() > 0 })
	op := b.operation(entity.Operation{Kind: "k", Target: aShare, State: entity.OpPending})
	if got := b.read(op.ID).State; got != entity.OpPending {
		t.Errorf("operation created while the loop waits: got %v, want pending until the clock moves", got)
	}
	b.clock.Advance(time.Second)
	waitFor(t, "the next turn to end the operation", func() bool { return b.read(op.ID).State == entity.OpSucceeded })
	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("run after its context ended: still running 10 s later, want returned")
	}
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

// While a step reports work left the loop turns again at once, on a clock
// that never moves; each such turn still drives every operation one step, so
// one with endless work left starves neither another operation nor the
// loop's context; and once no step reports work left the loop waits on its
// clock again.
func TestRunTurnsAtOnceWhileAStepHasWorkLeft(t *testing.T) {
	b := newBench(t, "", nil)
	ctx, cancel := context.WithCancel(t.Context())
	var long, other int
	var turns atomic.Int64
	more := true
	r := b.runner(map[string]stepRow{
		"long": {progress: noPhase, run: func(context.Context, entity.Operation) (step.StepResult, error) {
			long++
			return step.StepResult{More: more}, nil
		}},
		"other": {progress: noPhase, run: func(context.Context, entity.Operation) (step.StepResult, error) {
			other++
			return step.StepResult{}, nil
		}},
	})
	b.operation(entity.Operation{Kind: "long", Target: aShare, State: entity.OpRunning})
	b.operation(entity.Operation{Kind: "other", Target: entity.ResourceRef{Kind: entity.ResourceShare, ID: []byte{2}}, State: entity.OpRunning})
	const atOnce = 50
	waited := make(chan struct{})
	r.turned = func(context.Context) {
		switch turns.Add(1) {
		case atOnce:
			more = false // the next turn reports no work left
		case atOnce + 1:
			close(waited)
		}
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		r.run(ctx, health.NewWatchdog(b.clock).Loop("runner"))
	}()
	select {
	case <-waited:
	case <-time.After(10 * time.Second):
		t.Fatalf("%d turns within 10 s on a clock that never moved, want %d, each step but the last reporting work left", turns.Load(), atOnce+1)
	}
	waitFor(t, "the loop to wait on its clock once no step reports work left", func() bool { return b.clock.Pending() > 0 })
	if long != atOnce+1 || other != atOnce+1 {
		t.Errorf("steps over %d turns: the one with work left ran %d, the other %d; want each once a turn", atOnce+1, long, other)
	}
	if e := b.clock.Elapsed(); e != 0 {
		t.Errorf("clock over the turns: %v elapsed, want 0, no turn waiting while a step had work left", e)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("run after its context ended: still running 10 s later, want returned")
	}
}

// A loop whose step never runs out of work returns once its context ends.
func TestRunWithWorkLeftReturnsWhenItsContextEnds(t *testing.T) {
	b := newBench(t, "", nil)
	ctx, cancel := context.WithCancel(t.Context())
	r := b.runner(map[string]stepRow{"k": {progress: noPhase, run: func(context.Context, entity.Operation) (step.StepResult, error) {
		return step.StepResult{More: true}, nil
	}}})
	b.operation(entity.Operation{Kind: "k", Target: aShare, State: entity.OpRunning})
	turns := 0
	r.turned = func(context.Context) {
		if turns++; turns == 3 {
			cancel()
		}
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		r.run(ctx, health.NewWatchdog(b.clock).Loop("runner"))
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("run with work left after its context ended: still running 10 s later, want returned")
	}
}
