package node

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/cubbit/dittofs/internal/node/step"
	"github.com/cubbit/dittofs/pkg/block/engine"
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
	refused map[entity.OperationID]bool
}

// retry is a step that failed with a retried error: when it runs again, on
// the clock's Elapsed, and the wait that set it.
type retry struct{ at, wait time.Duration }

func newRunner(cp kvstore.ControlPlane, clock engine.Clock, node entity.NodeID, log *slog.Logger, steps map[string]stepRow) *runner {
	return &runner{cp: cp, clock: clock, node: node, log: log, steps: steps,
		retries: map[entity.OperationID]retry{}, refused: map[entity.OperationID]bool{}}
}

// run takes a turn every runInterval on the runner's clock until ctx ends.
// A step ctx interrupts commits nothing, so each operation is left at its
// last durable step.
//
// ponytail: an operation moves one step a turn, so a deletion of n batches
// takes n seconds. Run a step again at once while it reports work left when
// an operation's batches make that wait show.
func (r *runner) run(ctx context.Context) {
	for {
		r.turn(ctx)
		select {
		case <-ctx.Done():
			return
		case <-r.clock.After(runInterval):
		}
	}
}

// turn pages through every operation and drives each one pending or
// running, so an operation created since the last turn is picked up.
//
// ponytail: a turn reads every operation record, ended ones included,
// bounded by the seven days an ended one is kept. List the running index
// instead when that page-through shows in a profile.
func (r *runner) turn(ctx context.Context) {
	for op, err := range r.cp.Operations(ctx, entity.OperationID{}) {
		if err != nil {
			if ctx.Err() == nil {
				r.log.Warn("operation runner: list the operations", "error", err)
			}
			return
		}
		if op.State == entity.OpPending || op.State == entity.OpRunning {
			r.drive(ctx, op)
		}
	}
}

// drive runs one step of op and commits its answer when the step's cursor
// or the operation's state changed: a step's error that step.Failed names
// fails the operation, any other is retried with the same cursor after a
// wait, and a done step ends it with its Result and its End's writes.
//
// decision: an operation whose kind has no step stays pending, so a binary
// that gains the step later runs it. It is logged once, not yet reported
// among the node status's refused records, which the status writer adds.
func (r *runner) drive(ctx context.Context, op entity.Operation) {
	row, ok := r.steps[op.Kind]
	if !ok {
		if !r.refused[op.ID] {
			r.refused[op.ID] = true
			r.log.Warn("operation runner: no step for the operation's kind; it stays pending", "operation", op.ID, "kind", op.Kind)
		}
		return
	}
	now := r.clock.Elapsed()
	if w, waiting := r.retries[op.ID]; waiting && now < w.at {
		return
	}
	res, err := row.run(ctx, op)
	next := op
	problem, failed := step.Failed(err)
	switch {
	case failed:
		next.State, next.Error = entity.OpFailed, problem
	case err != nil:
		r.retry(ctx, op, now, err)
		return
	case res.Done:
		next.State, next.Step, next.Result = entity.OpSucceeded, res.Next, res.Result
	default:
		next.State, next.Step = entity.OpRunning, res.Next
	}
	if next.State == op.State && bytes.Equal(next.Step, op.Step) {
		delete(r.retries, op.ID)
		return
	}
	var meta entity.WriteMeta
	if next.State != op.State {
		meta.Audit = r.audit(op, next.State)
	}
	err = r.cp.Write(ctx, meta, func(tx entity.ControlPlaneTx) error {
		if next.State == entity.OpSucceeded && res.End != nil {
			if err := res.End(tx); err != nil {
				return err
			}
		}
		_, err := tx.PutOperation(next, op.Generation)
		return err
	})
	if err != nil {
		r.retry(ctx, op, now, err)
		return
	}
	delete(r.retries, op.ID)
}

// retry has op's step run again after a wait, doubled from the last one.
func (r *runner) retry(ctx context.Context, op entity.Operation, now time.Duration, err error) {
	w := firstRetry
	if last, ok := r.retries[op.ID]; ok {
		w = min(2*last.wait, lastRetry)
	}
	r.retries[op.ID] = retry{at: now + w, wait: w}
	if ctx.Err() == nil {
		r.log.Warn("operation runner: step retried", "operation", op.ID, "kind", op.Kind, "in", w, "error", err)
	}
}

// audit is the record of op's state changing to to.
//
// decision: the runner's state changes are audited as the schedule's, the
// zero actor, on this node: the runner acts on no request. The account that
// started the operation is on its record, and the write that started it was
// audited as that account.
func (r *runner) audit(op entity.Operation, to entity.OpState) *entity.AuditRecord {
	return &entity.AuditRecord{
		Node:    r.node,
		Target:  entity.ResourceRef{Kind: entity.ResourceOperation, ID: op.ID[:]},
		Before:  op.Generation,
		After:   op.Generation + 1,
		Change:  fmt.Appendf(nil, `{"state":{"old":%q,"new":%q}}`, op.State, to),
		Outcome: []byte("applied"),
	}
}
