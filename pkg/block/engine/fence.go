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
	failing     atomic.Bool  // the latest attempt of a retrying owner failed at the store as a whole
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
		if n == 0 && !f.failing.Load() {
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

// end brackets the write transaction begin started, ctx and err being the
// call's. A nil err is a commit, and clears the fence. An error is failing
// only under the retrying-owner mark and only when it is the store's
// (storeFailed); a one-shot call's error, or one ended by its caller's
// context, is neither a commit nor a failure.
//
// ponytail: one failing flag for the node rather than one per owner, so any
// commit clears it while another owner is still failing; that owner's next
// attempt sets it again and starts a fresh fenceAfter, so a node whose only
// commits come from one owner fences up to one backoff of another late. Keep a
// flag per owner, carried by the mark, if that lag is seen to matter.
func (f *fence) end(ctx context.Context, err error) {
	switch {
	case err == nil:
		f.committed()
	case isRetryingOwner(ctx) && storeFailed(err):
		f.lastErr.Store(&err)
		f.failing.Store(true) // before the count leaves zero, so no reader sees the node idle meanwhile
	}
	f.outstanding.Add(-1)
}

// committed records a commit: it restarts the fence's clock and lifts it.
func (f *fence) committed() {
	f.lastCommit.Store(int64(f.clock.Elapsed()))
	f.failing.Store(false)
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
// file's own failure the store reports as an error of the whole call, not one
// listed here, counts as the store's: name it here once the store gives it a
// value of its own.
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
	busy := f.outstanding.Load() > 0 || f.failing.Load()
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
// decision: only Write and Commit read the gate. Truncate, Deallocate,
// Delete and Clone each issue their own write transaction, which a stalled
// or erroring store fails at the caller's deadline, and Draw and the reads
// make none, so none of them is refused up front. Gate them too if a fenced
// node is seen to admit one that then holds a caller past its deadline.
func (f *fence) gate() error {
	if f.holds() {
		return &vfserr.Delay{Cause: vfserr.DelayCommit}
	}
	return nil
}

// cause is the store's error a retrying owner's latest attempt failed with,
// while one is failing, or errNoCommit. An error a commit since followed is
// not the cause of a later stall.
func (f *fence) cause() error {
	if p := f.lastErr.Load(); p != nil && f.failing.Load() {
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
	x.f.begin()
	m, err := x.Existence.Commit(ctx, pending, marks)
	x.f.end(ctx, err)
	return m, err
}

func (x fencedExistence) Remove(ctx context.Context, share entity.ShareID, file entity.FileID, epoch uint64, op kvstore.RemoveOp, pending []kvstore.PendingOp) error {
	x.f.begin()
	err := x.Existence.Remove(ctx, share, file, epoch, op, pending)
	x.f.end(ctx, err)
	return err
}

func (x fencedExistence) Clone(ctx context.Context, op kvstore.CloneOp, pending []kvstore.PendingOp) error {
	x.f.begin()
	err := x.Existence.Clone(ctx, op, pending)
	x.f.end(ctx, err)
	return err
}

// fencedContent is the content view with its write transactions recorded.
type fencedContent struct {
	Content
	f *fence
}

// Resume's batches that committed are a commit even when a later one failed.
func (c fencedContent) Resume(ctx context.Context, share entity.ShareID, file entity.FileID) (bool, error) {
	c.f.begin()
	advanced, err := c.Content.Resume(ctx, share, file)
	if advanced && err != nil {
		c.f.committed()
	}
	c.f.end(ctx, err)
	return advanced, err
}

func (c fencedContent) PruneRemovals(ctx context.Context, share entity.ShareID, file entity.FileID, atOrBelow entity.JournalVersion) error {
	c.f.begin()
	err := c.Content.PruneRemovals(ctx, share, file, atOrBelow)
	c.f.end(ctx, err)
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
