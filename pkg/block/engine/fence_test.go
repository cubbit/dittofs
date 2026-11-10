package engine_test

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/block/engine"
	"github.com/cubbit/dittofs/pkg/block/engine/enginetest"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/metadata/kv"
	"github.com/cubbit/dittofs/pkg/metadata/kv/keys"
	"github.com/cubbit/dittofs/pkg/metadata/kv/kvtest"
	"github.com/cubbit/dittofs/pkg/metadata/kvstore"
	"github.com/cubbit/dittofs/pkg/sim"
	"github.com/cubbit/dittofs/pkg/vfs/vfserr"
)

// fenceUnit is the unit over a fault-injecting kv.KV, its log recorded.
// No real-time bound cuts a store call, so a stall lasts exactly as long as
// the test clock says.
type fenceUnit struct {
	*unit
	faults *kvtest.FaultKV
	logs   logRecorder
}

func newFenceUnit(t *testing.T) *fenceUnit {
	t.Helper()
	fu := &fenceUnit{logs: newLogRecorder()}
	fu.unit = newUnitWith(t, unitConfig{
		kv: func(k kv.KV) kv.KV {
			fu.faults = kvtest.NewFaultKV(k)
			return fu.faults
		},
		deps: []func(*engine.Deps){func(d *engine.Deps) { d.Logger = slog.New(fu.logs) }},
	})
	fu.e.SetConflictBound(time.Hour)
	// A test that fails while a store call is held releases it, so the
	// engine's close, which joins that call, does not wait on the clock.
	t.Cleanup(func() { fu.clock.Advance(24 * time.Hour) })
	return fu
}

// fault replaces the store's fault schedule with rules, delays on the test
// clock.
func (u *fenceUnit) fault(t *testing.T, rules ...sim.Rule) {
	t.Helper()
	s, err := sim.NewSchedule(1, u.clock, rules...)
	if err != nil {
		t.Fatal(err)
	}
	u.faults.SetFault(s)
}

// everyUpdate is a rule acting on every write transaction the store runs.
func everyUpdate(a sim.FaultAction) sim.Rule {
	return sim.Rule{Op: kvtest.OpUpdate, Key: "*", Action: a}
}

// nextUpdate is a rule acting on the next write transaction only.
func nextUpdate(a sim.FaultAction) sim.Rule {
	r := everyUpdate(a)
	r.Count = 1
	return r
}

// tryWrite writes a byte to f, answering what Write answers.
func (u *fenceUnit) tryWrite(f entity.FileID) (vfserr.Verifier, error) {
	return u.e.Write(context.Background(), u.share.ID, f, 0, []byte("x"), engine.WriteOpts{Done: func() {}})
}

// fenced reports that err is the fence's answer, ErrDelay with DelayCommit.
func fenced(err error) bool {
	var d *vfserr.Delay
	return errors.As(err, &d) && d.Cause == vfserr.DelayCommit
}

// nodeFenced is node_fenced as Stats reports it.
func (u *fenceUnit) nodeFenced() (engine.Condition, bool) {
	for _, c := range u.e.Stats().Node.Conditions {
		if c.Name == "node_fenced" {
			return c, true
		}
	}
	return engine.Condition{}, false
}

// commitInBackground asks f's existence committed now, in a goroutine, and
// answers its result; the call is cancelled and joined when the test ends.
func (u *fenceUnit) commitInBackground(t *testing.T, f entity.FileID) <-chan error {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	var wg sync.WaitGroup
	wg.Go(func() { done <- u.e.CommitNow(ctx, u.share.ID, f) })
	t.Cleanup(func() { cancel(); wg.Wait() })
	return done
}

// outstanding waits until a write transaction is outstanding.
func (u *fenceUnit) outstanding(t *testing.T) {
	t.Helper()
	eventually(t, "a write transaction outstanding", func() bool { n, _ := u.e.FenceBusy(); return n > 0 })
}

// advanceInSteps moves the clock by d a second at a time, letting the
// engine's timers run between steps.
func (u *fenceUnit) advanceInSteps(d time.Duration) {
	for range int(d / time.Second) {
		u.clock.Advance(time.Second)
		time.Sleep(time.Millisecond)
	}
}

// logged answers the attributes of the first record at level carrying msg.
func (u *fenceUnit) logged(level slog.Level, msg string) (map[string]slog.Value, bool) {
	recs := u.logs.records()
	i := slices.IndexFunc(recs, func(r slog.Record) bool { return r.Level == level && r.Message == msg })
	if i < 0 {
		return nil, false
	}
	attrs := map[string]slog.Value{}
	recs[i].Attrs(func(a slog.Attr) bool { attrs[a.Key] = a.Value; return true })
	return attrs, true
}

// conditionLogged reports that attrs name node_fenced's condition, subject c
// and a cause, and carry duration.
func conditionLogged(attrs map[string]slog.Value, c engine.Condition, duration time.Duration) bool {
	return attrs["condition"].String() == "node_fenced" && attrs["subject"].String() == c.Subject.ID &&
		attrs["cause"].Any() != nil && attrs["duration"].Duration() == duration
}

// A store stalled for 10 s fences nothing; at 31 s writes and stability
// points answer ErrDelay with DelayCommit, reads are served, node_fenced names
// the store, the fence is counted once however many writes see it, and its
// entry is logged at Error. Once the stalled transaction commits, the next
// write is acknowledged under the same verifier, the lift is logged at Info,
// and the shard's incarnation and grace instance are as they were. Both logs
// carry the condition's name, subject and cause; the entry's duration is the
// time since the last commit, the lift's how long the fence held.
func TestSelfFenceThreshold(t *testing.T) {
	u := newFenceUnit(t)
	ctx := t.Context()
	a, b := u.create(t, "a"), u.create(t, "b")
	before, err := u.tryWrite(a)
	if err != nil {
		t.Fatal(err)
	}
	shard, err := u.fs.Store.Shard(ctx, u.share.Shard)
	if err != nil {
		t.Fatal(err)
	}
	u.fault(t, nextUpdate(sim.FaultAction{Delay: 40 * time.Second}))
	done := u.commitInBackground(t, a)
	u.outstanding(t)

	u.clock.Advance(10 * time.Second)
	if _, err := u.tryWrite(b); err != nil {
		t.Fatalf("a write 10s into the stall: %v, want acknowledged", err)
	}

	u.clock.Advance(21 * time.Second)
	for range 10 {
		if _, err := u.tryWrite(b); !fenced(err) {
			t.Fatalf("a write 31s into the stall: %v, want ErrDelay(DelayCommit)", err)
		}
	}
	if _, _, err := u.e.Commit(ctx, u.share.ID, b, 0, 0); !fenced(err) {
		t.Errorf("a stability point 31s into the stall: %v, want ErrDelay(DelayCommit)", err)
	}
	p := make([]byte, 1)
	if n, err := u.e.Read(ctx, u.share.ID, a, 0, p); err != nil || n != 1 || p[0] != 'x' {
		t.Errorf("a read while fenced: %d %q %v, want the byte written", n, p[:n], err)
	}
	c, ok := u.nodeFenced()
	if !ok || c.Subject.Scope != engine.ScopeNode || c.Cause == nil {
		t.Errorf("Stats while fenced: node_fenced %v %+v, want it raised for the node, naming a cause", ok, c)
	}
	if n := u.e.Stats().SelfFences; n != 1 {
		t.Errorf("engine_self_fences_total %d after one fence seen by many writes, want 1", n)
	}
	if attrs, ok := u.logged(slog.LevelError, "node_fenced: the metadata store committed no write transaction"); !ok || !conditionLogged(attrs, c, 31*time.Second) {
		t.Errorf("entering the fence logged %v %v at Error, want node_fenced, the node, a cause and 31s", ok, attrs)
	}

	u.clock.Advance(9 * time.Second) // the stall ends and its transaction commits
	if err := <-done; err != nil {
		t.Fatalf("the stalled commit: %v", err)
	}
	after, err := u.tryWrite(b)
	if err != nil {
		t.Fatalf("a write once the stall committed: %v, want acknowledged", err)
	}
	if after != before {
		t.Error("the verifier changed across the fence")
	}
	if _, ok := u.nodeFenced(); ok {
		t.Error("node_fenced still raised after a commit")
	}
	if attrs, ok := u.logged(slog.LevelInfo, "node_fenced cleared"); !ok || !conditionLogged(attrs, c, 9*time.Second) {
		t.Errorf("leaving the fence logged %v %v at Info, want node_fenced, the node, a cause and 9s", ok, attrs)
	}
	if got, err := u.fs.Store.Shard(ctx, u.share.Shard); err != nil || got.Incarnation != shard.Incarnation || got.GraceInstance != shard.GraceInstance {
		t.Errorf("the shard after the fence: %+v %v, want incarnation %d and grace %d as before", got, err, shard.Incarnation, shard.GraceInstance)
	}
}

// Every write transaction failing at once, reads served: the node fences 30 s
// after its first failed attempt began, and clears once one commits. A fence
// that triggers on no transaction completing never fires here.
func TestSelfFenceOnAnErroringStore(t *testing.T) {
	u := newFenceUnit(t)
	a, b := u.create(t, "a"), u.create(t, "b")
	if _, err := u.tryWrite(a); err != nil {
		t.Fatal(err)
	}
	u.fault(t, everyUpdate(sim.FaultAction{Err: errInjected}))
	done := u.commitInBackground(t, a)
	eventually(t, "the commit failing", func() bool { _, failing := u.e.FenceBusy(); return failing })

	// In steps, so the group commit retries throughout, each attempt failing.
	u.advanceInSteps(29 * time.Second)
	if _, err := u.tryWrite(b); err != nil {
		t.Fatalf("a write 29s into the failures: %v, want acknowledged", err)
	}
	u.advanceInSteps(2 * time.Second)
	if _, err := u.tryWrite(b); !fenced(err) {
		t.Fatalf("a write 31s into the failures: %v, want ErrDelay(DelayCommit)", err)
	}
	if c, ok := u.nodeFenced(); !ok || !errors.Is(c.Cause, errInjected) {
		t.Errorf("node_fenced %v naming %v, want the store's error", ok, c.Cause)
	}

	u.faults.SetFault(nil)
	u.until(t, "a write acknowledged once the store commits", func() bool { _, err := u.tryWrite(b); return err == nil })
	if err := <-done; err != nil {
		t.Errorf("the commit once the store answers: %v", err)
	}
}

// A node idle for a minute, with no write transaction outstanding or
// failing, does not fence.
func TestSelfFenceNotOnAnIdleNode(t *testing.T) {
	u := newFenceUnit(t)
	a := u.create(t, "a")
	u.clock.Advance(time.Minute)
	if _, err := u.tryWrite(a); err != nil {
		t.Errorf("a write after an idle minute: %v, want acknowledged", err)
	}
	if n := u.e.Stats().SelfFences; n != 0 {
		t.Errorf("engine_self_fences_total %d, want 0", n)
	}
}

// After an idle minute, a write transaction held for 1 s starts a fresh 30 s:
// a second write in that second is acknowledged. A fence timed from the last
// commit alone fires on it.
func TestSelfFenceTimesAFirstTransactionFromItsStart(t *testing.T) {
	u := newFenceUnit(t)
	a, b := u.create(t, "a"), u.create(t, "b")
	u.clock.Advance(time.Minute)
	if _, err := u.tryWrite(a); err != nil {
		t.Fatal(err)
	}
	u.fault(t, nextUpdate(sim.FaultAction{Delay: time.Second}))
	done := u.commitInBackground(t, a)
	u.outstanding(t)
	if _, err := u.tryWrite(b); err != nil {
		t.Errorf("a second write while the first's commit is held: %v, want acknowledged", err)
	}
	u.clock.Advance(time.Second)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if n := u.e.Stats().SelfFences; n != 0 {
		t.Errorf("engine_self_fences_total %d, want 0", n)
	}
}

// A one-shot call that failed, Bind's raise, and then an idle minute: no
// fence, as nothing retries it.
func TestSelfFenceNotAfterAFailedOneShot(t *testing.T) {
	u := newFenceUnit(t)
	a := u.create(t, "a")
	second := enginetest.AddShare(t, u.fs.Store, "second", 1<<30)
	u.fault(t, everyUpdate(sim.FaultAction{Err: errInjected}))
	if err := u.e.Bind(t.Context(), second); err == nil {
		t.Fatal("Bind over a failing store succeeded")
	}
	u.clock.Advance(time.Minute)
	if _, err := u.tryWrite(a); err != nil {
		t.Errorf("a write an idle minute after the failed Bind: %v, want acknowledged", err)
	}
}

// A one-shot call held past 30 s fences the node, and once it ends by its
// caller's context, no transaction outstanding or failing, the fence lifts
// with no commit: Bind with a 35 s context while the store stalls 40 s.
func TestSelfFenceLiftsWhenAOneShotEnds(t *testing.T) {
	u := newFenceUnit(t)
	a := u.create(t, "a")
	second := enginetest.AddShare(t, u.fs.Store, "second", 1<<30)
	u.fault(t, nextUpdate(sim.FaultAction{Delay: 40 * time.Second, Err: context.DeadlineExceeded}))
	ctx := &endingCtx{Context: context.Background(), done: make(chan struct{})}
	bound := make(chan error, 1)
	go func() { bound <- u.e.Bind(ctx, second) }()
	u.outstanding(t)

	u.clock.Advance(31 * time.Second)
	if _, err := u.tryWrite(a); !fenced(err) {
		t.Fatalf("a write with Bind's raise stalled 31s: %v, want ErrDelay(DelayCommit)", err)
	}
	u.clock.Advance(4 * time.Second)
	ctx.end(context.DeadlineExceeded)
	u.clock.Advance(5 * time.Second) // the store answers
	if err := <-bound; err == nil {
		t.Fatal("Bind past its deadline succeeded")
	}
	if _, err := u.tryWrite(a); err != nil {
		t.Errorf("a write once the one-shot ended: %v, want acknowledged", err)
	}
}

// The version limit's raise, a retrying owner's, failing at once for 31 s
// with no write pending fences the node, and its commit clears it. A recorder
// that sorts by method, counting RaiseVersionLimits as Bind's one-shot, never
// fences.
func TestSelfFenceOnARetriedRaise(t *testing.T) {
	u := newFenceUnit(t)
	a := u.create(t, "a")
	_, _, lc := u.e.StoreViews()
	ctx := engine.WithRetryingOwner(t.Context())
	to, err := lc.VersionLimits(ctx, []entity.ShareID{u.share.ID})
	if err != nil {
		t.Fatal(err)
	}
	u.fault(t, everyUpdate(sim.FaultAction{Err: errInjected}))
	if err := lc.RaiseVersionLimits(ctx, to); !errors.Is(err, errInjected) {
		t.Fatalf("the raise over a failing store: %v", err)
	}
	u.clock.Advance(31 * time.Second)
	if _, err := u.tryWrite(a); !fenced(err) {
		t.Fatalf("a write 31s into the raise failing: %v, want ErrDelay(DelayCommit)", err)
	}
	u.faults.SetFault(nil)
	if err := lc.RaiseVersionLimits(ctx, to); err != nil {
		t.Fatal(err)
	}
	if _, err := u.tryWrite(a); err != nil {
		t.Errorf("a write once the raise committed: %v, want acknowledged", err)
	}
}

// The removal loop's prune, a retrying owner's, failing for 31 s fences the
// node: the content view New wires counts toward the fence.
func TestSelfFenceOnAFailingPrune(t *testing.T) {
	u := newFenceUnit(t)
	a := u.create(t, "a")
	_, ct, _ := u.e.StoreViews()
	ctx := engine.WithRetryingOwner(t.Context())
	u.fault(t, everyUpdate(sim.FaultAction{Err: errInjected}))
	if err := ct.PruneRemovals(ctx, u.share.ID, a, entity.JournalVersion{15: 1}); !errors.Is(err, errInjected) {
		t.Fatalf("the prune over a failing store: %v", err)
	}
	u.clock.Advance(31 * time.Second)
	if _, err := u.tryWrite(a); !fenced(err) {
		t.Fatalf("a write 31s into the prune failing: %v, want ErrDelay(DelayCommit)", err)
	}
}

// fence fences the node: the version limit's raise, a retrying owner's,
// failing for 31 s.
func (u *fenceUnit) fence(t *testing.T) {
	t.Helper()
	_, _, lc := u.e.StoreViews()
	ctx := engine.WithRetryingOwner(t.Context())
	to, err := lc.VersionLimits(ctx, []entity.ShareID{u.share.ID})
	if err != nil {
		t.Fatal(err)
	}
	u.fault(t, everyUpdate(sim.FaultAction{Err: errInjected}))
	if err := lc.RaiseVersionLimits(ctx, to); !errors.Is(err, errInjected) {
		t.Fatalf("the raise over a failing store: %v", err)
	}
	u.clock.Advance(31 * time.Second)
}

// On a fenced node, a write and a stable Commit over a read-only journal
// answer as the journal does, ErrReadOnly for EROFS and retry-later on the
// journal for any other cause, ahead of the fence's own answer; a share not
// attached answers its own refusal ahead of both.
func TestSelfFenceAnswersAfterAReadOnlyJournal(t *testing.T) {
	for _, c := range []struct {
		name  string
		cause error // nil leaves the journal writable
		want  func(error) bool
	}{
		{"EROFS", fmt.Errorf("write segment: %w", syscall.EROFS), func(err error) bool { return errors.Is(err, vfserr.ErrReadOnly) }},
		{"EIO", syscall.EIO, func(err error) bool { return errors.Is(err, &vfserr.Delay{Cause: vfserr.DelayJournal}) }},
		{"writable", nil, fenced},
	} {
		t.Run(c.name, func(t *testing.T) {
			u := newFenceUnit(t)
			a := u.create(t, "a")
			u.fence(t)
			if c.cause != nil {
				u.j.ReadOnly(c.cause)
			}
			if _, err := u.tryWrite(a); !c.want(err) {
				t.Errorf("Write: %v", err)
			}
			if _, _, err := u.e.Commit(t.Context(), u.share.ID, a, 0, 1); !c.want(err) {
				t.Errorf("Commit: %v", err)
			}
			frozen := &vfserr.Delay{Cause: vfserr.DelayFrozen}
			if _, err := u.e.Write(t.Context(), entity.ShareID{0xff}, a, 0, []byte("x"), engine.WriteOpts{Done: func() {}}); !errors.Is(err, frozen) {
				t.Errorf("Write to a share not attached: %v, want ErrDelay(DelayFrozen)", err)
			}
		})
	}
}

// fileRecord is file's record key as the fault schedule names it.
func fileRecord(share entity.ShareID, file entity.FileID) string {
	return keys.Describe(keys.ID(keys.ID([]byte{keys.KindF}, share[:]), file[:]))
}

// One file's existence commit failing alone at every attempt, its record's
// read answering not found, and no other write for a minute: no fence, and
// existence_commit_failing names the file. Counting a file's own failure as
// the store's fences here.
func TestSelfFenceNotOnAFilesOwnFailure(t *testing.T) {
	u := newFenceUnit(t)
	a, b := u.create(t, "a"), u.create(t, "b")
	if _, err := u.tryWrite(a); err != nil {
		t.Fatal(err)
	}
	u.fault(t, sim.Rule{Op: kvtest.OpGet, Key: fileRecord(u.share.ID, a), Action: sim.FaultAction{Err: kv.ErrNotFound}})
	u.commitInBackground(t, a)
	eventually(t, "existence_commit_failing names the file", func() bool { return u.e.Failing(u.rec, a) })
	u.advanceInSteps(time.Minute)
	if _, err := u.tryWrite(b); err != nil {
		t.Errorf("a write after a minute of one file failing: %v, want acknowledged", err)
	}
	if !u.e.Failing(u.rec, a) {
		t.Error("existence_commit_failing no longer names the file")
	}
}

// One file's existence commit conflicting at every attempt, and no other
// write for a minute: no fence. Counting a conflict as the store's failure
// fences here. The journal's marks conflict too, so the group commit's marks
// written alone commit nothing that would restart the fence's clock.
func TestSelfFenceNotOnAConflict(t *testing.T) {
	u := newFenceUnit(t)
	u.e.SetConflictBound(20 * time.Millisecond)
	a, b := u.create(t, "a"), u.create(t, "b")
	if _, err := u.tryWrite(a); err != nil {
		t.Fatal(err)
	}
	u.fault(t,
		sim.Rule{Op: kvtest.OpGet, Key: fileRecord(u.share.ID, a), Action: sim.FaultAction{Err: kv.ErrConflict}},
		sim.Rule{Op: kvtest.OpGet, Key: "J‖*", Action: sim.FaultAction{Err: kv.ErrConflict}},
	)
	u.commitInBackground(t, a)
	eventually(t, "the conflicting file named failing", func() bool { return u.e.Failing(u.rec, a) })
	u.advanceInSteps(time.Minute)
	if _, err := u.tryWrite(b); err != nil {
		t.Errorf("a write after a minute of one file conflicting: %v, want acknowledged", err)
	}
}

// A store error ahead of a long conflict does not fence the node: the
// conflict is the latest attempt, and it failed on its own.
func TestSelfFenceNotOnAConflictAfterAStoreError(t *testing.T) {
	u := newFenceUnit(t)
	u.e.SetConflictBound(20 * time.Millisecond)
	a, b := u.create(t, "a"), u.create(t, "b")
	if _, err := u.tryWrite(a); err != nil {
		t.Fatal(err)
	}
	u.fault(t,
		nextUpdate(sim.FaultAction{Err: errInjected}),
		sim.Rule{Op: kvtest.OpGet, Key: fileRecord(u.share.ID, a), Action: sim.FaultAction{Err: kv.ErrConflict}},
		sim.Rule{Op: kvtest.OpGet, Key: "J‖*", Action: sim.FaultAction{Err: kv.ErrConflict}},
	)
	u.commitInBackground(t, a)
	eventually(t, "the conflicting file named failing", func() bool { return u.e.Failing(u.rec, a) })
	u.advanceInSteps(time.Minute)
	if _, err := u.tryWrite(b); err != nil {
		t.Errorf("a write after one store error then a minute of conflicts: %v, want acknowledged", err)
	}
}

// One owner's conflicts do not hide another's store failure: the raise
// failing at the store for 140 s, a prune that conflicts every 15 s beside
// it, fences the node. A failing flag shared by every owner fences nothing
// here, as each conflict clears it and each failed raise then starts a fresh
// 30 s.
func TestSelfFenceNotHiddenByAnotherOwnersConflict(t *testing.T) {
	u := newFenceUnit(t)
	a := u.create(t, "a")
	_, ct, lc := u.e.StoreViews()
	raise, prune := engine.WithRetryingOwner(t.Context()), engine.WithRetryingOwner(t.Context())
	to, err := lc.VersionLimits(raise, []entity.ShareID{u.share.ID})
	if err != nil {
		t.Fatal(err)
	}
	raiseFails := func() {
		t.Helper()
		u.fault(t, everyUpdate(sim.FaultAction{Err: errInjected}))
		if err := lc.RaiseVersionLimits(raise, to); !errors.Is(err, errInjected) {
			t.Fatalf("the raise over a failing store: %v", err)
		}
	}
	for range 8 {
		raiseFails()
		u.clock.Advance(10 * time.Second)
		u.fault(t, sim.Rule{Op: kvtest.OpGet, Key: "*", Action: sim.FaultAction{Err: kv.ErrConflict}})
		bctx, cancel := context.WithTimeout(prune, 50*time.Millisecond)
		if err := ct.PruneRemovals(bctx, u.share.ID, a, entity.JournalVersion{15: 1}); !errors.Is(err, kvstore.ErrConflict) {
			cancel()
			t.Fatalf("the prune over a conflicting store: %v", err)
		}
		cancel()
		u.clock.Advance(5 * time.Second)
	}
	raiseFails()
	u.clock.Advance(20 * time.Second)
	if _, err := u.tryWrite(a); !fenced(err) {
		t.Errorf("a write 140s into the raise failing, a prune conflicting beside it: %v, want ErrDelay(DelayCommit)", err)
	}
}

// A group commit that stops while failing at the store leaves no failing
// behind: a minute later the idle node is not fenced.
func TestSelfFenceNotLeftByAStoppedOwner(t *testing.T) {
	u := newFenceUnit(t)
	a := u.create(t, "a")
	if _, err := u.tryWrite(a); err != nil {
		t.Fatal(err)
	}
	u.fault(t, everyUpdate(sim.FaultAction{Err: errInjected}))
	u.commitInBackground(t, a)
	eventually(t, "the group commit failing at the store", func() bool { _, failing := u.e.FenceBusy(); return failing })
	u.e.CloseGroupCommits()
	u.clock.Advance(time.Minute)
	if c, ok := u.nodeFenced(); ok {
		t.Errorf("node_fenced %v a minute after the failing group commit stopped, want none", c)
	}
}

// A commit the store reached through conflicts it retried counts as a
// commit: it restarts the 30 s of a retrying owner failing beside it.
func TestSelfFenceCountsAConflictRetriedToSuccess(t *testing.T) {
	u := newFenceUnit(t)
	a, b := u.create(t, "a"), u.create(t, "b")
	ex, _, lc := u.e.StoreViews()
	ctx := engine.WithRetryingOwner(t.Context())
	to, err := lc.VersionLimits(ctx, []entity.ShareID{u.share.ID})
	if err != nil {
		t.Fatal(err)
	}
	raiseFails := func() {
		t.Helper()
		u.fault(t, everyUpdate(sim.FaultAction{Err: errInjected}))
		if err := lc.RaiseVersionLimits(ctx, to); !errors.Is(err, errInjected) {
			t.Fatalf("the raise over a failing store: %v", err)
		}
	}
	raiseFails()
	u.clock.Advance(20 * time.Second)
	if _, err := u.tryWrite(a); err != nil {
		t.Fatal(err)
	}
	var ops []kvstore.PendingOp
	for _, op := range u.e.Pending(a) {
		ops = append(ops, kvstore.PendingOp{Version: entity.JournalVersion(op.Version.Bytes()), Kind: kvstore.OpWrite, Extent: entity.Extent{Offset: op.Off, Length: op.Len}, Mtime: op.At, Ctime: op.At})
	}
	u.fault(t, sim.Rule{Op: kvtest.OpGet, Key: "*", Count: 2, Action: sim.FaultAction{Err: kv.ErrConflict}})
	if failed, err := ex.Commit(ctx, []kvstore.PendingExistence{{Share: u.share.ID, File: a, Ops: ops}}, nil); err != nil || len(failed) > 0 {
		t.Fatalf("the commit through two conflicts: %v %v", failed, err)
	}
	raiseFails()

	u.clock.Advance(25 * time.Second)
	if _, err := u.tryWrite(b); err != nil {
		t.Fatalf("a write 25s after the commit: %v, want acknowledged", err)
	}
	u.clock.Advance(6 * time.Second)
	if _, err := u.tryWrite(b); !fenced(err) {
		t.Errorf("a write 31s after the commit, the raise failing: %v, want ErrDelay(DelayCommit)", err)
	}
}

// Commits landing while 100 goroutines read the gate, a one-shot call held
// outstanding throughout: no reader is fenced, since each commit restarts the
// 30 s, and the race detector sees the fence's state read and written at once.
func TestSelfFenceGateUnderRace(t *testing.T) {
	u := newFenceUnit(t)
	a := u.create(t, "a")
	ex, _, _ := u.e.StoreViews()
	second := enginetest.AddShare(t, u.fs.Store, "second", 1<<30)
	u.fault(t, nextUpdate(sim.FaultAction{Delay: time.Hour}))
	bound := make(chan error, 1)
	go func() { bound <- u.e.Bind(context.Background(), second) }()
	u.outstanding(t)

	stop := make(chan struct{})
	var wg sync.WaitGroup
	var mu sync.Mutex
	var seen error
	for range 100 {
		wg.Go(func() {
			for {
				select {
				case <-stop:
					return
				default:
				}
				if _, err := u.e.Write(context.Background(), u.share.ID, a, 0, nil, engine.WriteOpts{Done: func() {}}); err != nil {
					mu.Lock()
					seen = err
					mu.Unlock()
				}
			}
		})
	}
	for range 100 {
		if _, err := ex.Commit(t.Context(), nil, nil); err != nil {
			t.Error(err)
		}
		u.clock.Advance(time.Second)
	}
	close(stop)
	wg.Wait()
	if seen != nil {
		t.Errorf("a gate read while commits landed answered %v", seen)
	}
	u.clock.Advance(time.Hour)
	if err := <-bound; err != nil {
		t.Fatal(err)
	}
}
