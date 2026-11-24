package node

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/cubbit/dittofs/internal/node/step"
	"github.com/cubbit/dittofs/pkg/block/engine"
	"github.com/cubbit/dittofs/pkg/health"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/metadata/kvstore"
	"github.com/cubbit/dittofs/pkg/node/apiv1"
)

const (
	// runInterval is the runner's wait after a turn in which no step reported
	// work left.
	runInterval = time.Second
	// A retried step waits firstRetry, doubling at each failure in a row up
	// to lastRetry.
	firstRetry = time.Second
	lastRetry  = 30 * time.Second
)

// stepRow is one operation kind's step, how its progress reads, and what
// the runner writes beside the record of an operation of the kind it fails,
// nil for nothing.
type stepRow struct {
	run      step.Step
	progress step.Progress
	failed   func(tx entity.ControlPlaneTx) error
}

// runner drives the operations no component drives on its own, one step of
// each per turn, recording each turn's answer in the operation's record.
// A full turn drives every operation listed, every runInterval; between
// them, turns follow at once while a step reports work left, each driving
// only the operations whose last step did.
//
// ponytail: the runner keeps no claim and resumes, at every turn, every
// operation whose record has not ended, so a second node would drive the
// same operations; and an operation whose state write fails stays as
// recorded until a later turn. Add the claim and its lapse-takeover with the
// cluster: a record with no claim reads as lapsed, so nothing migrates.
type runner struct {
	cp      kvstore.ControlPlane
	clock   engine.Clock
	node    entity.NodeID
	log     *slog.Logger
	steps   map[string]stepRow
	retries map[entity.OperationID]retry
	busy    map[entity.OperationID]bool // the operations whose last committed step answered More

	// turned, when set, is called with run's context after each turn, once
	// the wait for the next, if any, is set on the clock; waits says whether
	// one was, and turned may read it.
	turned func(ctx context.Context)
	waits  bool

	mu      sync.Mutex
	refused map[string]bool // the kinds found with no step
}

// retry is a step that failed with a retried error: when it runs again, on
// the clock's Elapsed, and the wait that set it.
type retry struct{ at, wait time.Duration }

func newRunner(cp kvstore.ControlPlane, clock engine.Clock, node entity.NodeID, log *slog.Logger, steps map[string]stepRow) *runner {
	return &runner{cp: cp, clock: clock, node: node, log: log, steps: steps,
		retries: map[entity.OperationID]retry{}, busy: map[entity.OperationID]bool{}, refused: map[string]bool{}}
}

// progressOf reads op's progress by its kind's row: unknown for a kind this
// node has no step for.
func (r *runner) progressOf(ctx context.Context, op entity.Operation) apiv1.Progress {
	if row, ok := r.steps[op.Kind]; ok {
		return row.progress(ctx, op)
	}
	return apiv1.Progress{Total: -1}
}

// run takes turns until ctx ends, reporting each to loop: the next at once
// while a step of the last reported work left, and otherwise the next full
// turn, runInterval on the runner's clock after the last one began, or at
// once when that has passed; a turn runInterval or more after the last full
// one is full. A step ctx interrupts commits nothing, so each operation is
// left at its last durable step.
//
// decision: a turn that follows at once drives, one step each, every
// operation whose last step reported work left, so two long deletions
// alternate and neither starves the other; the others, waiting on their own
// schedule, a remote peer or a retry, are driven only by the full turn every
// runInterval, so a step that probes something remote while it waits is not
// called once per batch of a deletion. No turn starts once ctx has ended, so
// a Stop ends the runner after the step it interrupts, however much work is
// left.
func (r *runner) run(ctx context.Context, loop *health.Loop) {
	now := make(chan struct{})
	close(now)
	full := r.clock.Elapsed() - runInterval
	for ctx.Err() == nil {
		all := r.clock.Elapsed()-full >= runInterval
		if all {
			full = r.clock.Elapsed()
		}
		next := (<-chan struct{})(now)
		if r.waits = !r.turn(ctx, all); r.waits {
			next = r.clock.After(full + runInterval - r.clock.Elapsed())
		}
		loop.Turn()
		if r.turned != nil {
			r.turned(ctx)
		}
		select {
		case <-ctx.Done():
			return
		case <-next:
		}
	}
}

// turn pages through the operations not yet ended and drives each one, or
// unless all is set only those whose last step reported work left, so an
// operation created since the last full turn is picked up by the next, then
// forgets the retry waits of those no longer listed. It reports whether a
// step reported work left.
//
// ponytail: a turn pages through the whole running index, and a deletion
// with work left runs turns back to back, so the page-through repeats once
// per batch, as do drive's two reads of the operation's progress, which for
// a share's deletion are two reads of the removal's record. Keep a cursor or
// a wake-up per operation when that page-through shows in a profile. Back to
// back, a large share's drop also runs its quadratic rescan (the ponytail on
// kvstore's ContinueShardRemoval) with no pause, its store transactions
// competing with foreground writes, and each batch's bounded store calls
// leave two storeBound timers that cannot be stopped (the ponytail on the
// system clock's After): cap the back-to-back time per runInterval when
// foreground latency during a large drop, or those timers in a heap
// profile, show it.
func (r *runner) turn(ctx context.Context, all bool) bool {
	listed := map[entity.OperationID]bool{}
	for op, err := range r.cp.RunningOperations(ctx, entity.Operation{}) {
		if err != nil {
			if ctx.Err() == nil {
				r.log.Warn("operation runner: list the operations", "error", err)
			}
			return false
		}
		listed[op.ID] = true
		if all || r.busy[op.ID] {
			r.busy[op.ID] = r.drive(ctx, op)
		}
	}
	maps.DeleteFunc(r.retries, func(id entity.OperationID, _ retry) bool { return !listed[id] })
	maps.DeleteFunc(r.busy, func(id entity.OperationID, more bool) bool { return !more || !listed[id] })
	return len(r.busy) > 0
}

// drive runs one step of op and commits its answer when the step's cursor,
// the operation's state or the phase its progress reads changed, audited
// when the state or the phase did: a step's error that failed names, or its
// panic, fails the operation, any other is retried with the same cursor
// after a wait, and a done step ends it with its Result and its End's
// writes, an End's error or panic answered as a step's is. An operation
// failed is written with its kind's failed writes; when those writes are
// what failed the commit, the failure is recorded without them. A step ctx
// interrupted commits nothing, whatever it answered. It reports whether the
// step answered More and its answer was committed.
//
// decision: an operation whose kind has no step stays pending, so a binary
// that gains the step later runs it. Its kind is logged once and kept for
// refusedKinds, which the node status reports among its refused records.
func (r *runner) drive(ctx context.Context, op entity.Operation) bool {
	row, ok := r.steps[op.Kind]
	if !ok {
		r.mu.Lock()
		first := !r.refused[op.Kind]
		r.refused[op.Kind] = true
		r.mu.Unlock()
		if first {
			r.log.Warn("operation runner: no step for the operation's kind; it stays pending", "operation", op.ID, "kind", op.Kind)
		}
		return false
	}
	now := r.clock.Elapsed()
	if w, waiting := r.retries[op.ID]; waiting && now < w.at {
		return false
	}
	before := row.progress(ctx, op).Phase
	res, err := step.Run(ctx, row.run, op)
	if ctx.Err() != nil {
		return false
	}
	next := op
	problem, fails := step.Failure(err)
	end := res.End
	switch {
	case fails:
		next.State, next.Error, end = entity.OpFailed, problem, row.failed
	case err != nil:
		r.retry(ctx, op, now, err)
		return false
	case res.Done:
		next.State, next.Step, next.Result = entity.OpSucceeded, res.Next, res.Result
	default:
		next.State, next.Step = entity.OpRunning, res.Next
	}
	err = r.commit(ctx, op, next, before, row.progress(ctx, next).Phase, end)
	if problem, refused := step.Failure(err); refused && !fails {
		next = op
		next.State, next.Error = entity.OpFailed, problem
		err = r.commit(ctx, op, next, before, row.progress(ctx, next).Phase, row.failed)
		fails = true
	}
	if fails && err != nil {
		// row.failed, or the commit carrying it, did not land: a failure must
		// always be recorded, or the step that answered it runs again at
		// every turn, so it is recorded bare.
		//
		// decision: a plain store error here is not retried either, so a
		// one-off blip loses row.failed's writes; for a rotation, the failed
		// LastRotation, and with it that one back-off, costing one extra
		// rotation rather than a step rerun at every turn. Retry it once the
		// failed write of some kind carries more than a schedule's back-off.
		err = r.commit(ctx, op, next, before, row.progress(ctx, next).Phase, nil)
	}
	if err != nil {
		r.retry(ctx, op, now, err)
		return false
	}
	delete(r.retries, op.ID)
	return res.More && !fails && !res.Done
}

// commit writes next over op, audited when its state or phase moved, with
// end's writes in the same transaction when next ended, succeeded or failed;
// a next that changes none of the cursor, the state and the phase commits
// nothing.
//
// decision: end runs code from outside the node, so its panic is recovered
// and, like a step's, fails the operation: end's writes roll back with the
// transaction, and drive records the failure in a second one. A panic
// retried would panic again at every turn and every start.
func (r *runner) commit(ctx context.Context, op, next entity.Operation, before, after string, end func(entity.ControlPlaneTx) error) error {
	if next.State == op.State && bytes.Equal(next.Step, op.Step) && after == before {
		return nil
	}
	var meta entity.WriteMeta
	if next.State != op.State || after != before {
		meta.Audit = r.audit(op, next.State, before, after)
	}
	return r.cp.Write(ctx, meta, func(tx entity.ControlPlaneTx) (err error) {
		if (next.State == entity.OpSucceeded || next.State == entity.OpFailed) && end != nil {
			defer func() {
				if p := recover(); p != nil {
					err = fmt.Errorf("%w: operation end: panic: %v", step.ErrStepFailed, p)
				}
			}()
			if err := end(tx); err != nil {
				return err
			}
		}
		_, err = tx.PutOperation(next, op.Generation)
		return err
	})
}

// refusedKinds is the operation kinds the runner found with no step.
func (r *runner) refusedKinds() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Sorted(maps.Keys(r.refused))
}

// retry has op's step run again after a wait, doubled from the last one.
func (r *runner) retry(ctx context.Context, op entity.Operation, now time.Duration, err error) {
	w := backoff(r.retries[op.ID].wait, true)
	r.retries[op.ID] = retry{at: now + w, wait: w}
	if ctx.Err() == nil {
		r.log.Warn("operation runner: step retried", "operation", op.ID, "kind", op.Kind, "in", w, "error", err)
	}
}

// audit is the record of op's state changing to to and its phase from
// before to after, naming only what changed.
//
// decision: the runner's state changes are audited as the account that
// started the operation, on this node, and as the schedule, the zero actor,
// only for an operation no call started, whose Account is zero.
func (r *runner) audit(op entity.Operation, to entity.OpState, before, after string) *entity.AuditRecord {
	type change struct {
		Old string `json:"old"`
		New string `json:"new"`
	}
	c := map[string]change{}
	if to != op.State {
		c["state"] = change{op.State.String(), to.String()}
	}
	if after != before {
		c["phase"] = change{before, after}
	}
	b, _ := json.Marshal(c) // strings always marshal
	return &entity.AuditRecord{
		Actor:   op.Account,
		Node:    r.node,
		Target:  entity.ResourceRef{Kind: entity.ResourceOperation, ID: op.ID[:]},
		Before:  op.Generation,
		After:   op.Generation + 1,
		Change:  b,
		Outcome: []byte("applied"),
	}
}
