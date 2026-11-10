package engine

import (
	"context"
	"maps"
	"runtime"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/block/journal"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/vfs/vfserr"
)

// ErrNotStarted is the answer of a Bind while no journal is open.
var ErrNotStarted = errNotStarted

// The errors start answers and refuses shares with, for a test to match.
var (
	ErrUnbuilt         = errUnbuilt
	ErrSourceNotServed = errSourceNotServed
	ErrStarted         = errStarted
	ErrJournalLost     = errJournalLost
	ErrNotRecorded     = errNotRecorded
	ErrUnidentified    = errUnidentified
	ErrNotRaised       = errNotRaised
)

// SetJournalsForTest sets the journals the engine has open, as Start will,
// each with the limit last passed read from the journal itself, so the engine
// and the journal agree on it.
func (e *Engine) SetJournalsForTest(js ...Journal) {
	vls := make([]*versionLimit, 0, len(js))

	for _, j := range js {
		vl := newVersionLimit(j)
		vl.last.Store(j.Stats().VersionLimit.Counter)
		vls = append(vls, vl)
	}

	e.mu.Lock()
	e.journals = vls
	e.mu.Unlock()
}

// SetBlockForTest sets the versions one raise of the version limit covers,
// before Start or Bind.
func (e *Engine) SetBlockForTest(n uint64) {
	e.block = n
}

// TrackRaisesForTest counts the version limit raise goroutines alive and
// keeps the clock channels they wait on to retry, for WaitRaises, and counts
// every one started, for RaisesStarted. Call it before Start or Bind.
func (e *Engine) TrackRaisesForTest() {
	e.raising.count = &atomic.Int32{}
	e.raising.started = &atomic.Int64{}
	e.raising.timers = &sync.Map{}
}

// RaisesStarted is how many raise goroutines started since
// TrackRaisesForTest.
func (e *Engine) RaisesStarted() int64 {
	return e.raising.started.Load()
}

// WaitRaises returns once every version limit raise has ended or waits on
// the test clock to retry, so a test reads what the raises did. It fails the
// test when a raise does neither within 5 s, which only wrong code reaches.
// A raise counts as waiting while the clock channel it waits on is still
// open: one the clock has closed but whose raise has not run yet is not, so a
// test that advances the clock then waits for the retry. Other waits on the
// clock, as the group commit's, are not counted.
func (e *Engine) WaitRaises(t testing.TB) {
	t.Helper()

	count, timers := e.raising.count, e.raising.timers

	if count == nil {
		t.Fatal("WaitRaises: TrackRaisesForTest was not called")
	}

	// Correct raises settle at once; the deadline is met only by a raise
	// stuck on something other than the clock.
	deadline := time.Now().Add(5 * time.Second)

	for {
		n := count.Load()

		if n == 0 || int(n) <= openTimers(timers) {
			return
		}

		if time.Now().After(deadline) {
			t.Fatal("WaitRaises: a raise neither ended nor waits to retry")
		}

		runtime.Gosched()
	}
}

// MarkRaisedForTest records shard as raised by this process, as Start does.
func (e *Engine) MarkRaisedForTest(shard entity.ShardID) {
	e.mu.Lock()
	e.raised[shard] = struct{}{}
	e.mu.Unlock()
}

// RaisedForTest reports whether shard is recorded as raised by this process.
func (e *Engine) RaisedForTest(shard entity.ShardID) bool {
	e.mu.Lock()

	defer e.mu.Unlock()

	_, ok := e.raised[shard]

	return ok
}

// IncarnationForTest is the incarnation on share s's binding, zero when it
// has no binding.
func (e *Engine) IncarnationForTest(s entity.ShareID) uint64 {
	b := e.table()[s]

	if b == nil {
		return 0
	}

	return b.incarnation
}

// TableForTest is the share table's pointer, compared for identity.
func (e *Engine) TableForTest() any {
	return e.shares.Load()
}

// StateOf is share s's binding state, zero when it has no binding.
func (e *Engine) StateOf(s entity.ShareID) ShareState {
	b := e.table()[s]

	if b == nil {
		return 0
	}

	return ShareState(b.state.Load())
}

// LifecycleFree reports whether the lifecycle mutex is free now.
func (e *Engine) LifecycleFree() bool {
	if !e.mu.TryLock() {
		return false
	}

	e.mu.Unlock()

	return true
}

// HoldLifecycle takes the lifecycle mutex and returns the function that
// releases it.
func (e *Engine) HoldLifecycle() (release func()) {
	e.mu.Lock()

	return e.mu.Unlock
}

// BindingOf reports whether share s is bound, through the content path's
// own lookup.
func (e *Engine) BindingOf(s entity.ShareID) error {
	_, err := e.bindingOf(s)
	return err
}

// FileEntries is the count of files holding an entry.
func (e *Engine) FileEntries() int {
	n := 0
	for i := range e.files.shards {
		sh := &e.files.shards[i]
		sh.mu.Lock()
		n += len(sh.m)
		sh.mu.Unlock()
	}
	return n
}

// HasEntry reports whether f has an entry, through entryOf.
func (e *Engine) HasEntry(f entity.FileID) bool { return e.files.entryOf(f) != nil }

// SameShard reports whether a and b share a shard of the entry map.
func (e *Engine) SameShard(a, b entity.FileID) bool { return e.files.shardOf(a) == e.files.shardOf(b) }

// HoldEntry takes f's entry lock, inserting the entry, and returns the
// function that releases it, as a call adding to it would.
func (e *Engine) HoldEntry(f entity.FileID) func() {
	fe := e.files.lockFor(f)
	return func() { e.files.unlock(f, fe) }
}

// AddPending adds a pending write of [off, off+n) to f's entry, and returns
// the function that drops it again, as a commit would, bumping the removal
// counter.
func (e *Engine) AddPending(f entity.FileID, off, n int64) (drop func()) {
	fe := e.files.lockFor(f)
	fe.ops = append(fe.ops, pendingOp{kind: opWrite})
	fe.ops[len(fe.ops)-1].extent.Off, fe.ops[len(fe.ops)-1].extent.Len = off, n
	e.files.unlock(f, fe)
	return func() {
		fe := e.files.lockOf(f)
		if fe == nil {
			return
		}
		if len(fe.ops) > 0 {
			fe.ops = fe.ops[1:]
			e.files.removals.Add(1)
		}
		e.files.unlock(f, fe)
	}
}

// FileStateWaits is the file_state lock-wait series: its count and total.
func (e *Engine) FileStateWaits() (int64, time.Duration) {
	return e.files.waits.count.Load(), time.Duration(e.files.waits.total.Load())
}

// FileStateWaiting is the callers waiting for an entry lock now.
func (e *Engine) FileStateWaiting() int64 { return e.files.waits.waiting.Load() }

// PendingOpType is the pending operation's type, for a check of its fields.
type PendingOpType = pendingOp

// SetSample replaces how a write samples its verifier.
func (e *Engine) SetSample(f func() vfserr.Verifier) {
	e.sample = func(*binding) vfserr.Verifier { return f() }
}

// SetPrimary sets share s's verifier inputs from its shard's primary, as
// Bind will from the start transaction, copying the share table as a
// binding's change does.
func (e *Engine) SetPrimary(s entity.ShareID, node entity.NodeID, nodeEpoch, incarnation uint64) {
	t := bindings{}
	for k, v := range *e.shares.Load() {
		t[k] = v
	}
	old := t[s]
	b := &binding{
		share:       old.share,
		j:           old.j,
		vl:          old.vl,
		h:           old.h,
		incarnation: incarnation,
		primary:     primary{node, nodeEpoch},
		gc:          old.gc,
	}
	b.state.Store(old.state.Load())
	t[s] = b
	e.shares.Store(&t)
}

// NewInstance draws the process instance again, as a new process would.
func (e *Engine) NewInstance() { e.instance = instanceID() }

// PendingView is one pending operation as a test reads it.
type PendingView struct {
	Version   journal.Version
	Off, Len  int64
	At        time.Time
	SetsMtime bool
	HasDone   bool
}

// Pending is f's pending operations, in the entry's order.
func (e *Engine) Pending(f entity.FileID) []PendingView {
	fe := e.files.lockOf(f)
	if fe == nil {
		return nil
	}
	defer e.files.unlock(f, fe)
	var out []PendingView
	for _, op := range fe.ops {
		out = append(out, PendingView{op.version, op.extent.Off, op.extent.Len, op.at, op.setsMtime, op.done != nil})
	}
	return out
}

// AddOp adds a pending deallocate of [off, off+n), or with truncate a pending
// truncate to off, to f's entry at version v, as those calls will once built.
func (e *Engine) AddOp(f entity.FileID, v uint64, truncate bool, off, n int64) {
	op := pendingOp{version: journal.Version{Counter: v}, kind: opDeallocate, extent: journal.Extent{Off: off, Len: n}}
	if truncate {
		op.kind, op.extent.Len = opTruncate, 0
	}
	fe := e.files.lockFor(f)
	fe.insert(op)
	e.files.unlock(f, fe)
}

// AddWriteOp adds a pending write of [off, off+n) to f's entry at version v.
func (e *Engine) AddWriteOp(f entity.FileID, v uint64, off, n int64) {
	fe := e.files.lockFor(f)
	fe.insert(pendingOp{version: journal.Version{Counter: v}, kind: opWrite, extent: journal.Extent{Off: off, Len: n}})
	e.files.unlock(f, fe)
}

// ClearOps drops f's pending operations, as a commit of their existence does.
func (e *Engine) ClearOps(f entity.FileID) {
	fe := e.files.lockFor(f)
	fe.ops = nil
	e.files.removals.Add(1)
	e.files.unlock(f, fe)
}

// Reads is engine_reads_total of class, by its label.
func (e *Engine) Reads(class string) int64 {
	for c, name := range readClassNames {
		if name == class {
			return e.reads.byClass[c].Load()
		}
	}
	panic("no read class " + class)
}

// ReadSeconds is engine_read_seconds: its count and total.
func (e *Engine) ReadSeconds() (int64, time.Duration) {
	return e.reads.count.Load(), time.Duration(e.reads.total.Load())
}

// OnCapture runs f after each batch's capture, before its sync.
func (e *Engine) OnCapture(f func()) { e.hooks.captured.Store(&f) }

// CommitNow is commitNow on share s's file f.
func (e *Engine) CommitNow(ctx context.Context, s entity.ShareID, f entity.FileID) error {
	b, err := e.bindingOf(s)
	if err != nil {
		return err
	}
	return e.commitNow(ctx, b, f)
}

// WaitBatch is waitBatch on f.
func (e *Engine) WaitBatch(ctx context.Context, f entity.FileID) error { return e.waitBatch(ctx, f) }

// PendingApplied is engine_pending_existence_ops.
func (e *Engine) PendingApplied() int64 { return e.pendingApplied.Load() }

// OnLookup runs f in lockFor between its lookup and its lock.
func (e *Engine) OnLookup(f func()) { e.files.afterLookup = f }

// OnTake runs f after each batch's take, before its capture.
func (e *Engine) OnTake(f func()) { e.hooks.taken.Store(&f) }

// Due reports whether f is on journal j's due set.
func (e *Engine) Due(j Journal, f entity.FileID) bool {
	g := e.groupCommitFor(j)
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.due[f] != nil
}

// LostWhole is lostWhole on journal j.
func (e *Engine) LostWhole(j Journal) { e.lostWhole(j) }

// OnWritten runs f in each Write between its journal's WriteAt and its insert.
func (e *Engine) OnWritten(f func()) { e.hooks.written.Store(&f) }

// OnDrain runs f in each drain that read new losses, before it marks writes.
func (e *Engine) OnDrain(f func()) { e.hooks.drained.Store(&f) }

// SyncFile syncs file f of share s through the engine's one sync function.
func (e *Engine) SyncFile(ctx context.Context, s entity.ShareID, f entity.FileID) error {
	b, err := e.bindingOf(s)
	if err != nil {
		return err
	}
	return e.sync(ctx, b, fileID(f))
}

// StabilityPoints is engine_stability_points_total.
func (e *Engine) StabilityPoints() int64 { return e.stabilityPoints.Load() }

// Failing reports whether existence_commit_failing names f on journal j.
func (e *Engine) Failing(j Journal, f entity.FileID) bool {
	g := e.groupCommitFor(j)
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.failing[f] != nil && g.failing[f].raised
}

// SetConflictBound sets how long one store call the engine retries may run.
func (e *Engine) SetConflictBound(d time.Duration) { e.conflictBound.Store(int64(d)) }

// RetryingOwner reports whether ctx carries the retrying-owner mark.
func RetryingOwner(ctx context.Context) bool { return isRetryingOwner(ctx) }

// SetAfterRegister makes h run once each clone's registrations are
// installed; the clone then stops, its registrations undone, answering
// ErrUnbuilt: a test of admission alone.
func (e *Engine) SetAfterRegister(h func()) { e.afterRegister = h }

// SetCloneMarkerHook makes h run for each clone marker start step 3 finds,
// standing in for the clone's rebuild.
func (e *Engine) SetCloneMarkerHook(h cloneMarkerFunc) {
	e.cloneMarker = h
}

// SetCloneRemovalHook makes h run for each clone removal start step 4 finds,
// standing in for the clone's rebuild.
func (e *Engine) SetCloneRemovalHook(h cloneRemovalFunc) {
	e.cloneRemoval = h
}

// FakeCloneDone lets a clone run past its registrations to its done signal,
// standing in for the joins a served engine does not have yet.
func (e *Engine) FakeCloneDone() { e.fakeCloneDone.Store(true) }

// LoopSteps is the count of items the share's journal's removal loop ran.
func (e *Engine) LoopSteps(s entity.ShareID) int64 {
	b, err := e.bindingOf(s)
	if err != nil {
		return 0
	}
	return b.gc.removals.steps.Load()
}

// RemovalRunsDone is engine_removal_runs_total{result="done"} on the share's
// journal's loop.
func (e *Engine) RemovalRunsDone(s entity.ShareID) int64 {
	b, err := e.bindingOf(s)
	if err != nil {
		return 0
	}
	return b.gc.removals.runs[runDone].Load()
}

// RemovalRunsError is engine_removal_runs_total{result="error"} on the
// share's journal's loop.
func (e *Engine) RemovalRunsError(s entity.ShareID) int64 {
	b, err := e.bindingOf(s)
	if err != nil {
		return 0
	}

	return b.gc.removals.runs[runError].Load()
}

// RemovalRunsConflict is engine_removal_runs_total{result="conflict"} on the
// share's journal's loop.
func (e *Engine) RemovalRunsConflict(s entity.ShareID) int64 {
	b, err := e.bindingOf(s)
	if err != nil {
		return 0
	}

	return b.gc.removals.runs[runConflict].Load()
}

// Registrations is the count of f's source and destination registrations.
func (e *Engine) Registrations(f entity.FileID) (src, dst int) {
	fe := e.files.lockOf(f)
	if fe == nil {
		return 0, 0
	}
	defer e.files.unlock(f, fe)
	for _, r := range fe.regs {
		if r.dst {
			dst++
		} else {
			src++
		}
	}
	return src, dst
}

// PendingWrites is the count of f's pending writes overlapping [off, end).
func (e *Engine) PendingWrites(f entity.FileID, off, end int64) int {
	fe := e.files.lockOf(f)
	if fe == nil {
		return 0
	}
	defer e.files.unlock(f, fe)
	n := 0
	for _, op := range fe.ops {
		if op.kind == opWrite && op.extent.Off < end && off < op.extent.Off+op.extent.Len {
			n++
		}
	}
	return n
}

// OnStaged runs f in each removal between its journal step and its phase 1.
func (e *Engine) OnStaged(f func()) { e.hooks.staged.Store(&f) }

// OnIssued runs f with each version the engine counts as issued toward its
// version limit.
func (e *Engine) OnIssued(f func(journal.Version)) { e.onIssued.Store(&f) }

// GuardHeld reports whether a removal holds f's guard.
func (e *Engine) GuardHeld(f entity.FileID) bool {
	fe := e.files.entryOf(f)
	return fe != nil && fe.guarded.Load()
}

// RemovalStalled reports whether f has an item removal_stalled on a loop.
func (e *Engine) RemovalStalled(f entity.FileID) bool {
	e.gcMu.Lock()
	defer e.gcMu.Unlock()
	for _, g := range e.gcs {
		if g.removals.stalledOn(f) {
			return true
		}
	}
	return false
}

// RemovalsPending is engine_removals_pending over every loop.
func (e *Engine) RemovalsPending() int {
	e.gcMu.Lock()
	defer e.gcMu.Unlock()
	n := 0
	for _, g := range e.gcs {
		k, _ := g.removals.pending()
		n += k
	}
	return n
}

// NoGuardSkip has every capture take a file whose guard a removal holds,
// as one would with the guard's skip removed.
func (e *Engine) NoGuardSkip() { e.noGuardSkip.Store(true) }

// StopRemovalLoops ends every journal's removal loop, as Close's stop of
// the work queue does before its last group commit, and leaves the group
// commits running.
func (e *Engine) StopRemovalLoops() {
	e.gcMu.Lock()
	gs := make([]*groupCommit, 0, len(e.gcs))
	for _, g := range e.gcs {
		gs = append(gs, g)
	}
	e.gcMu.Unlock()
	for _, g := range gs {
		g.removals.close()
	}
}

// StageGrow stages on f, at the journal's next version, a grow to size,
// which the store refuses as a truncate down when size is below the file's
// committed size: a pending operation the store will not commit.
func (e *Engine) StageGrow(s entity.ShareID, f entity.FileID, size int64) error {
	b, err := e.bindingOf(s)
	if err != nil {
		return err
	}
	v, err := b.j.Draw()
	if err != nil {
		return err
	}
	fe := e.files.lockFor(f)
	defer e.files.unlock(f, fe)
	fe.insert(pendingOp{version: v, kind: opTruncate, extent: journal.Extent{Off: size}, grow: true})
	return nil
}

// The calls below are what Close and Drop will run once built; until then
// only tests call them, so they live here rather than in the engine.

// HoldRemovalLoop builds j's group commit and removal loop as the first Start
// would, and runs the group commit but not the loop, so what Start posts to
// the loop stays on it, counted by RemovalsPending. The function returned
// starts the loop, once.
//
// It copies how groupCommitFor assembles a group commit, so the two must
// change together: a field groupCommitFor sets that this does not leaves the
// held one different from the one Start builds.
func (e *Engine) HoldRemovalLoop(j Journal) (run func()) {
	e.gcMu.Lock()
	defer e.gcMu.Unlock()

	g := &groupCommit{
		e:        e,
		j:        j,
		due:      map[entity.FileID]*dueFile{},
		returned: make(chan struct{}),
		wake:     make(chan struct{}, 1),
		stop:     make(chan struct{}),
		done:     make(chan struct{}),
	}
	g.ctx, g.cancel = context.WithCancel(withRetryingOwner(context.Background()))
	g.removals = newRemovalLoop(e)

	if e.gcs == nil {
		e.gcs = map[Journal]*groupCommit{}
	}

	e.gcs[j] = g

	go g.run()

	var once sync.Once

	return func() {
		once.Do(func() {
			go g.removals.run()
		})
	}
}

// CloseGroupCommits stops every journal's group commit and joins its
// goroutine, as Close will: a batch in flight has its context cancelled, and
// no batch starts after it.
func (e *Engine) CloseGroupCommits() {
	e.gcMu.Lock()
	gs := e.gcs
	e.gcs = nil
	e.gcsClosed = true
	e.gcMu.Unlock()
	for _, g := range gs {
		g.mu.Lock()
		g.closed = true
		g.mu.Unlock()
		g.cancel()
		close(g.stop)
		<-g.done
		g.mu.Lock()
		g.broadcastLocked() // every waiter then finds the group commit closed
		g.mu.Unlock()
		g.removals.close()
	}
}

// CommitAll commits the pending existence of every file of journal j, as
// commitNow does for one, and as Close's last group commit will: it marks
// every due file due now and returns once the due set is empty and no batch
// is in flight, or at ctx's end.
func (e *Engine) CommitAll(ctx context.Context, j Journal) error {
	g := e.groupCommitFor(j)
	for {
		g.mu.Lock()
		if g.closed {
			g.mu.Unlock()
			return errClosed
		}
		files := slices.Collect(maps.Keys(g.due))
		done := len(files) == 0 && !g.inFlight
		returned := g.returned
		g.mu.Unlock()
		if done {
			return nil
		}
		// A file a removal's guard holds is committed by that removal's own
		// phase 1, and a batch would only skip it: wait for the guard rather
		// than ask for batch after batch. Under the test seam a batch takes
		// it, so it is asked for like any other.
		var guard <-chan struct{}
		asked := false
		for _, f := range files {
			if ch := e.guardOf(f); ch != nil && !e.noGuardSkip.Load() {
				guard = ch
				continue
			}
			g.mu.Lock()
			if d := g.due[f]; d != nil {
				d.now, asked = true, true
			}
			g.mu.Unlock()
		}
		if asked {
			g.poke()
		}
		select {
		case <-returned:
		case <-guard:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// DropShare takes share s's files off the due set of journal j and its
// items off the journal's removal loop, ending each, as Drop will, and
// returns once no batch is in flight there, so none of them is committed
// after.
func (e *Engine) DropShare(ctx context.Context, j Journal, s entity.ShareID) error {
	g := e.groupCommitFor(j)
	g.removals.dropShare(s)
	for {
		g.mu.Lock()
		for f, d := range g.due {
			if d.b.share.ID == s {
				delete(g.due, f)
				g.clearFailingLocked(f)
			}
		}
		inFlight, returned := g.inFlight && !g.closed, g.returned
		g.mu.Unlock()
		if !inFlight {
			return nil
		}
		select {
		case <-returned:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// dropShare takes share s's items off the loop, ending each; one running
// now is ended when its run returns, and none of the share's is enqueued
// after.
func (l *removalLoop) dropShare(s entity.ShareID) {
	l.mu.Lock()
	if l.dropped == nil {
		l.dropped = map[entity.ShareID]bool{}
	}
	l.dropped[s] = true
	var dropped []*loopItem
	l.items = slices.DeleteFunc(l.items, func(it *loopItem) bool {
		if it.b.share.ID == s {
			dropped = append(dropped, it)
			return true
		}
		return false
	})
	l.mu.Unlock()
	for _, it := range dropped {
		if it.end != nil {
			it.end()
		}
	}
}

// EntryLocked reports whether f's entry lock is held: a probe that takes and
// gives back the lock when it is free.
func (e *Engine) EntryLocked(f entity.FileID) bool {
	fe := e.files.entryOf(f)
	if fe == nil || !fe.mu.TryLock() {
		return fe != nil
	}
	fe.mu.Unlock()
	return false
}

// openTimers is how many of the raises' clock channels the clock has not
// closed yet.
func openTimers(timers *sync.Map) int {
	n := 0

	timers.Range(func(_, v any) bool {
		select {
		case <-v.(<-chan struct{}):
		default:
			n++
		}

		return true
	})

	return n
}
