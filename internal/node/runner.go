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
)

const (
	// runInterval is the runner's wait between turns.
	runInterval = time.Second
	// A retried step waits firstRetry, doubling at each failure in a row up
	// to lastRetry.
	firstRetry = time.Second
	lastRetry  = 30 * time.Second
)

// stepRow is one operation kind's step and how its progress reads.
type stepRow struct {
	run      step.Step
	progress step.Progress
}

// runner drives the operations no component drives on its own, one step of
// each per turn, recording each turn's answer in the operation's record.
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

	// turned, when set, is called after each turn, once the wait for the
	// next is set on the clock.
	turned func()

	mu      sync.Mutex
	refused map[string]bool // the kinds found with no step
}

// retry is a step that failed with a retried error: when it runs again, on
// the clock's Elapsed, and the wait that set it.
type retry struct{ at, wait time.Duration }

func newRunner(cp kvstore.ControlPlane, clock engine.Clock, node entity.NodeID, log *slog.Logger, steps map[string]stepRow) *runner {
	return &runner{cp: cp, clock: clock, node: node, log: log, steps: steps,
		retries: map[entity.OperationID]retry{}, refused: map[string]bool{}}
}

// run takes a turn every runInterval on the runner's clock until ctx ends,
// reporting each to loop. A step ctx interrupts commits nothing, so each
// operation is left at its last durable step.
//
// ponytail: an operation moves one step a turn, so a deletion of n batches
// takes n seconds. Run a step again at once while it reports work left when
// an operation's batches make that wait show.
func (r *runner) run(ctx context.Context, loop *health.Loop) {
	for {
		r.turn(ctx)
		loop.Turn()
		next := r.clock.After(runInterval)
		if r.turned != nil {
			r.turned()
		}
		select {
		case <-ctx.Done():
			return
		case <-next:
		}
	}
}

// turn pages through the operations not yet ended and drives each one, so
// an operation created since the last turn is picked up, then forgets the
// retry waits of those no longer listed.
//
// ponytail: a turn pages through the whole running index. Keep a cursor or
// a wake-up per operation when that page-through shows in a profile.
func (r *runner) turn(ctx context.Context) {
	listed := map[entity.OperationID]bool{}
	for op, err := range r.cp.RunningOperations(ctx, entity.Operation{}) {
		if err != nil {
			if ctx.Err() == nil {
				r.log.Warn("operation runner: list the operations", "error", err)
			}
			return
		}
		listed[op.ID] = true
		r.drive(ctx, op)
	}
	maps.DeleteFunc(r.retries, func(id entity.OperationID, _ retry) bool { return !listed[id] })
}

// drive runs one step of op and commits its answer when the step's cursor,
// the operation's state or the phase its progress reads changed, audited
// when the state or the phase did: a step's error that failed names, or its
// panic, fails the operation, any other is retried with the same cursor
// after a wait, and a done step ends it with its Result and its End's
// writes, an End's error or panic answered as a step's is. A step ctx
// interrupted commits nothing, whatever it answered.
//
// decision: an operation whose kind has no step stays pending, so a binary
// that gains the step later runs it. Its kind is logged once and kept for
// refusedKinds, which the node status reports among its refused records.
func (r *runner) drive(ctx context.Context, op entity.Operation) {
	row, ok := r.steps[op.Kind]
	if !ok {
		r.mu.Lock()
		first := !r.refused[op.Kind]
		r.refused[op.Kind] = true
		r.mu.Unlock()
		if first {
			r.log.Warn("operation runner: no step for the operation's kind; it stays pending", "operation", op.ID, "kind", op.Kind)
		}
		return
	}
	now := r.clock.Elapsed()
	if w, waiting := r.retries[op.ID]; waiting && now < w.at {
		return
	}
	before := row.progress(ctx, op).Phase
	res, err := step.Run(ctx, row.run, op)
	if ctx.Err() != nil {
		return
	}
	next := op
	problem, fails := step.Failure(err)
	switch {
	case fails:
		next.State, next.Error = entity.OpFailed, problem
	case err != nil:
		r.retry(ctx, op, now, err)
		return
	case res.Done:
		next.State, next.Step, next.Result = entity.OpSucceeded, res.Next, res.Result
	default:
		next.State, next.Step = entity.OpRunning, res.Next
	}
	err = r.commit(ctx, op, next, before, row.progress(ctx, next).Phase, res.End)
	if problem, fails := step.Failure(err); fails {
		next = op
		next.State, next.Error = entity.OpFailed, problem
		err = r.commit(ctx, op, next, before, row.progress(ctx, next).Phase, nil)
	}
	if err != nil {
		r.retry(ctx, op, now, err)
		return
	}
	delete(r.retries, op.ID)
}

// commit writes next over op, audited when its state or phase moved, with
// end's writes in the same transaction when next succeeded; a next that
// changes none of the cursor, the state and the phase commits nothing.
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
		if next.State == entity.OpSucceeded && end != nil {
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
