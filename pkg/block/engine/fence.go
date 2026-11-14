package engine

import (
	"context"
	"encoding/hex"
	"errors"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/metadata/kvstore"
	"github.com/cubbit/dittofs/pkg/vfs/vfserr"
)

// fenceAfter is how long a write transaction may be outstanding or failing
// with none committed before the node fences: the default request deadline.
// It is fixed, not a setting.
const fenceAfter = 30 * time.Second

// conditionNodeFenced is the condition the self-fence raises: unhealthy while
// it holds, naming the metadata store and its last error.
const conditionNodeFenced = "node_fenced"

// errNoCommit is node_fenced's cause when no write transaction failed: the
// store answered none within the fence's bound.
var errNoCommit = errors.New("engine: the metadata store committed no write transaction")

// fence records every write transaction the engine's store views issue and
// fences the node once one has been outstanding or failing for fenceAfter
// with none committed. It is all atomics and runs nothing of its own: the
// fence is computed where it is read, at the gate of Write and Commit and in
// Stats. Reads never consult it.
type fence struct {
	clock  Clock
	logger *slog.Logger
	node   entity.NodeID // node_fenced's subject

	lastCommit  atomic.Int64 // the clock's Elapsed at the last commit, in nanoseconds
	since       atomic.Int64 // the Elapsed at which the node last went from no transaction outstanding or failing to one
	outstanding atomic.Int64 // write transactions running now
	failing     atomic.Int64 // retrying owners whose latest attempt failed at the store as a whole
	lastErr     atomic.Pointer[error]
	entered     atomic.Pointer[fenceEntry] // the fence's entry while it holds, nil while lifted
	fences      atomic.Int64               // engine_self_fences_total{reason="store"}
}

// begin brackets one write transaction's start. A start on a node with none
// outstanding or failing starts a fresh fenceAfter: a failing owner's count
// leaves zero at each retry, so it is the node's state, not the count, that
// times the fence.
//
// since is stored before the count leaves zero, so no reader sees the node
// busy with the since of an earlier busy spell. It is only ever raised: a
// start that read zero and then lost the count to another cannot lower the
// since that one stored, and moves it no later than its own reading.
func (f *fence) begin() {
	for {
		n := f.outstanding.Load()
		if n == 0 && f.failing.Load() == 0 {
			raise(&f.since, int64(f.clock.Elapsed()))
		}
		if f.outstanding.CompareAndSwap(n, n+1) {
			return
		}
	}
}

// raise sets a to v unless it already holds a later value.
func raise(a *atomic.Int64, v int64) {
	for {
		c := a.Load()
		if c >= v || a.CompareAndSwap(c, v) {
			return
		}
	}
}

// end brackets the write transaction begin started: ctx and err are the
// call's, items what it carried (its files, marksItem for marks, wholeItem
// for neither). A nil err is a commit: it restarts the fence's clock and
// clears ctx's retrying owner and every item of it. Failing is per owner
// (RFC 8 §11.2) and, within one, per item, each timed from its latest
// attempt. A call its caller's own context ended, by a cancel or a deadline
// other than the conflict bound's, is neither a commit nor a failure: it
// leaves the owner and its items as they were. An error the store's
// (storeFailed) fails the owner and marks the call's items failing at the
// store, but a record of a file's own that does not decode marks none: the
// split names its file. Any other error, that record in a call carrying its
// file alone included, fails on its own: it clears the call's items, and the
// owner once none of its items is left failing at the store. No call clears
// another owner. A one-shot call's error is neither a commit nor a failure.
//
// An item also leaves when its owner stops retrying it without either, as
// forget says.
func (f *fence) end(ctx context.Context, err error, items ...any) {
	o := ownerOf(ctx)
	if len(items) == 0 {
		items = []any{wholeItem{}}
	}
	switch {
	case o == nil:
	case err == nil:
		f.forget(o)
	case callerEnded(ctx):
	case storeFailed(err) && !ownRecord(err, items):
		var failed []any
		if !kvstore.FileRecordUndecodable(err) {
			failed = items
		}
		f.lastErr.Store(&err)
		// Counted before outstanding leaves zero, so no reader sees the
		// node idle meanwhile.
		if o.fail(failed) {
			f.failing.Add(1)
		}
	default:
		f.forget(o, items...)
	}
	if err == nil {
		f.committed()
	}
	f.outstanding.Add(-1)
}

// callerEnded reports that ctx, a store call's, ended by its caller's cancel
// or deadline. The conflict bound's cut is not the caller's: it is how a
// store that answers nothing fails a call (storeFailed).
func callerEnded(ctx context.Context) bool {
	return ctx.Err() != nil && !errors.Is(context.Cause(ctx), errBoundCut)
}

// ownRecord reports that err is a record of the one file items carry that
// does not decode: that file's failure, not the store's. A call carrying
// several files leaves it to the split to name the file.
func ownRecord(err error, items []any) bool {
	n := 0
	for _, it := range items {
		if _, ok := it.(entity.FileID); ok {
			n++
		}
	}
	return n == 1 && kvstore.FileRecordUndecodable(err)
}

// marksItem is a marks write's item, failing at the store apart from the
// files beside it; wholeItem is the item of a call that carries none.
type (
	marksItem struct{}
	wholeItem struct{}
)

// release ends o's failing, if it is, so an owner that stops leaves no
// failing behind. A retrying owner's loop calls it as it returns, after its
// last store call, so an owner that stops while failing does not keep an idle
// node fenced. f is nil only in a test that builds its engine without New.
//
// decision: runClone and unstage carry the removal loop's mark off its
// goroutine, so a store error of theirs after the loop's release marks the
// owner failing again, and nothing releases it. That holds only while such an
// error comes after Close halted the loops; release from cloning's join too
// if one is seen fencing a live node.
func (f *fence) release(o *ownerState) { f.forget(o) }

// forget clears o's items, every item when none is named, and ends o's
// failing once none is left failing at the store. Besides a commit and a
// failure of the item's own, an item leaves this way when its owner stops
// retrying it: the group commit's file whose writes left another way, was
// taken off by an unbind or lost with its journal, and the removal loop's
// item that finished or was ended. Kept, it would hold the owner failing
// and fence a quiet node on a healthy store.
func (f *fence) forget(o *ownerState, items ...any) {
	if f != nil && o != nil && o.settle(items) {
		f.failing.Add(-1)
	}
}

// committed records a commit: it restarts the fence's clock and lifts it. It
// clears no owner's failing; a node another owner is failing on fences
// fenceAfter after this commit.
func (f *fence) committed() {
	f.lastCommit.Store(int64(f.clock.Elapsed()))
	f.lift()
}

// storeFailed reports that err, a write transaction's, fails the store as a
// whole.
//
// decision: a conflict, a call its context cancelled, the store's refusal of
// the operations themselves, a file gone stale and a marks write a newer open
// replaced each fail one file or one call, which the group commit's split or
// the owner's own condition answers, so none is the store's; a per-file entry
// in Existence.Commit's map never reaches here, as that call committed. Every
// other error is the store's, a deadline the engine's own bound cut included,
// since an erroring store must fence as one that answers nothing does. A
// record of a file's own that does not decode, which the store reports as an
// error of the whole call, counts here as the store's; end counts it as the
// file's once a call carries that file alone.
func storeFailed(err error) bool {
	return !errors.Is(err, kvstore.ErrConflict) &&
		!errors.Is(err, context.Canceled) &&
		!errors.Is(err, entity.ErrStale) &&
		!errors.Is(err, kvstore.ErrOpenGenMoved) &&
		!kvstore.Refused(err)
}

// holds evaluates the fence now: it holds while a write transaction is
// outstanding or failing and fenceAfter has passed since the later of the
// last commit and the moment the node last went busy. The first reader to
// find it entered counts and logs it; one finding it lifted logs that.
//
// The clock is read before the state: a commit or a start landing between
// the two then only moves from past now, which reads as not yet due, where
// the other order pairs an old from with a later now and fences.
func (f *fence) holds() bool {
	now := int64(f.clock.Elapsed())
	busy := f.outstanding.Load() > 0 || f.failing.Load() > 0
	from := max(f.lastCommit.Load(), f.since.Load())
	if !busy || now-from < int64(fenceAfter) {
		f.lift()
		return false
	}
	if f.entered.Load() == nil {
		e := &fenceEntry{at: now, cause: f.cause()}
		if f.entered.CompareAndSwap(nil, e) {
			f.fences.Add(1)
			// The duration is the time since the last commit, which an idle
			// spell before the stall lengthens past fenceAfter.
			f.logger.Error("node_fenced: the metadata store committed no write transaction",
				"condition", conditionNodeFenced, "subject", f.subject(), "cause", e.cause,
				"duration", time.Duration(now-f.lastCommit.Load()))
		}
	}
	return true
}

// fenceEntry is when the fence was entered and why.
type fenceEntry struct {
	at    int64 // the clock's Elapsed, in nanoseconds
	cause error
}

// lift ends the fence if it holds, logging it once with how long it held.
func (f *fence) lift() {
	if f.entered.Load() == nil {
		return
	}
	if e := f.entered.Swap(nil); e != nil {
		f.logger.Info("node_fenced cleared",
			"condition", conditionNodeFenced, "subject", f.subject(), "cause", e.cause,
			"duration", time.Duration(int64(f.clock.Elapsed())-e.at))
	}
}

// subject is the node node_fenced is raised for, as its condition names it.
func (f *fence) subject() string { return hex.EncodeToString(f.node[:]) }

// gate is a write's or a stability point's answer while the fence holds:
// ErrDelay with DelayCommit, else nil.
//
// decision: only Write, Commit and CommitExistence read the gate. Truncate,
// Deallocate, Delete and Clone each issue their own write transaction, which
// a stalled or erroring store fails at the caller's deadline, and Draw and
// the reads make none, so none of them is refused up front. Gate them too if
// a fenced node is seen to admit one that then holds a caller past its
// deadline.
func (f *fence) gate() error {
	if f.holds() {
		return &vfserr.Delay{Cause: vfserr.DelayCommit}
	}
	return nil
}

// fenceGate is Write's, Commit's and CommitExistence's answer on b while the
// fence holds. The self-fence answers after every refusal of a write that
// never reaches the store: b's journal turned read-only answers as journalErr
// does (RFC 8 §11.2), and so would any capacity refusal of the journal the
// RFC places before it. Those are read only once the gate refuses, so a call
// on an unfenced node pays nothing for them; checks for them go here, not
// ahead of the gate. A share's own refusal, from bindingOf, comes before this.
func (e *Engine) fenceGate(b *binding) error {
	err := e.fence.gate()
	if err == nil {
		return nil
	}
	if ro := b.j.Stats().ReadOnly; ro.ReadOnly {
		return e.journalErr(readOnly(ro))
	}
	return err
}

// cause is the store's error a retrying owner's latest attempt failed with,
// while one is failing, or errNoCommit. An error no owner is failing with
// any more is not the cause of a later stall.
func (f *fence) cause() error {
	if p := f.lastErr.Load(); p != nil && f.failing.Load() > 0 {
		return *p
	}
	return errNoCommit
}

// condition is node_fenced while the fence holds.
func (f *fence) condition() (Condition, bool) {
	if !f.holds() {
		return Condition{}, false
	}
	return Condition{
		Name:    conditionNodeFenced,
		Subject: Subject{Scope: ScopeNode, ID: f.subject()},
		Cause:   f.cause(),
		Detail:  "the metadata store committed no write transaction for " + fenceAfter.String(),
	}, true
}

// fencedExistence is the existence view with its write transactions recorded.
type fencedExistence struct {
	Existence
	f *fence
}

func (x fencedExistence) Commit(ctx context.Context, pending []kvstore.PendingExistence, marks *kvstore.MarksWrite) (map[entity.FileID]error, error) {
	// decision: a marks write carrying no marks sets no key, and a store
	// commits it without writing, so it is no write transaction: counted,
	// it would restart the fence's clock on a store that refuses every write.
	// Its failure is not counted either, as the files' calls of its batch
	// report the store. Count it again if a marks write with no marks is ever
	// seen to write.
	if len(pending) == 0 && (marks == nil || len(marks.Marks) == 0) {
		return x.Existence.Commit(ctx, pending, marks)
	}
	items := make([]any, 0, len(pending)+1)
	for _, p := range pending {
		items = append(items, p.File)
	}
	if marks != nil && len(marks.Marks) > 0 {
		items = append(items, marksItem{})
	}
	x.f.begin()
	m, err := x.Existence.Commit(ctx, pending, marks)
	x.f.end(ctx, err, items...)
	return m, err
}

func (x fencedExistence) Remove(ctx context.Context, share entity.ShareID, file entity.FileID, epoch uint64, op kvstore.RemoveOp, pending []kvstore.PendingOp) error {
	x.f.begin()
	err := x.Existence.Remove(ctx, share, file, epoch, op, pending)
	x.f.end(ctx, err, file)
	return err
}

func (x fencedExistence) Clone(ctx context.Context, op kvstore.CloneOp, pending []kvstore.PendingOp) error {
	x.f.begin()
	err := x.Existence.Clone(ctx, op, pending)
	x.f.end(ctx, err, op.File)
	return err
}

// fencedContent is the content view with its write transactions recorded.
type fencedContent struct {
	Content
	f *fence
}

// Resume's batches that committed are a commit even when a later one failed.
//
// decision: a Resume that committed no batch and failed nothing found no
// removal left, and wrote nothing, so it is neither a commit nor a failure.
// Counted as a commit, the removal loop's retry of a file whose removal ran
// and whose prune keeps failing would restart the fence's clock and clear the
// owner every second, on a store that refuses every write. Count it again if
// such a Resume is ever seen to write.
func (c fencedContent) Resume(ctx context.Context, share entity.ShareID, file entity.FileID) (bool, error) {
	c.f.begin()
	advanced, err := c.Content.Resume(ctx, share, file)
	switch {
	case !advanced && err == nil:
		c.f.outstanding.Add(-1)
		return advanced, err
	case advanced && err != nil:
		c.f.committed()
	}
	c.f.end(ctx, err, file)
	return advanced, err
}

func (c fencedContent) PruneRemovals(ctx context.Context, share entity.ShareID, file entity.FileID, atOrBelow entity.JournalVersion) error {
	c.f.begin()
	err := c.Content.PruneRemovals(ctx, share, file, atOrBelow)
	c.f.end(ctx, err, file)
	return err
}

// fencedLifecycle is the lifecycle view with its write transactions recorded:
// the version limits' raise and the incarnations' raises.
type fencedLifecycle struct {
	Lifecycle
	f *fence
}

func (l fencedLifecycle) RaiseVersionLimits(ctx context.Context, to map[entity.ShareID]entity.JournalVersion) error {
	l.f.begin()
	err := l.Lifecycle.RaiseVersionLimits(ctx, to)
	l.f.end(ctx, err)
	return err
}

func (l fencedLifecycle) RaiseAtStart(ctx context.Context, shards []entity.ShardID) (map[entity.ShardID]uint64, error) {
	l.f.begin()
	incs, err := l.Lifecycle.RaiseAtStart(ctx, shards)
	l.f.end(ctx, err)
	return incs, err
}

func (l fencedLifecycle) RaiseAtAttach(ctx context.Context, shard entity.ShardID) (uint64, error) {
	l.f.begin()
	inc, err := l.Lifecycle.RaiseAtAttach(ctx, shard)
	l.f.end(ctx, err)
	return inc, err
}
