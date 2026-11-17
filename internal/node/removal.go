package node

import (
	"context"
	"errors"
	"fmt"

	"github.com/cubbit/dittofs/internal/node/step"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	merrs "github.com/cubbit/dittofs/pkg/metadata/errors"
	"github.com/cubbit/dittofs/pkg/metadata/kvstore"
	"github.com/cubbit/dittofs/pkg/node/apiv1"
)

// operationSteps is the runner's step table: the kinds this node drives.
func operationSteps(nodes kvstore.Nodes) map[string]stepRow {
	return map[string]stepRow{
		string(apiv1.OpUserDelete):  {principalDeletion(nodes, entity.PrincipalUser), deletionProgress},
		string(apiv1.OpGroupDelete): {principalDeletion(nodes, entity.PrincipalGroup), deletionProgress},
	}
}

// principalDeletion is the step of a user's or a group's deletion past its
// mark, kind naming which, its operation's target the principal's ID: one
// batch of Nodes.ContinueDelete a turn, done when it answers done. It keeps
// no cursor, since each batch finds what is left from the rows, and a record
// already gone answers done.
//
// decision: a principal that is not marked, and a target that names no
// principal of kind, fail the operation rather than retry it: only the mark
// marks, and the running operation holds its target, refusing the deletion
// that could mark it, so neither changes on a retry. Every other error,
// kv.ErrTooLarge included, is retried on the runner's backoff, since a
// failed deletion is never resumed and would leave the principal marked and
// half-deleted; and a batch's size can change between turns, as its quota
// drop reads every share. Fail a batch too large for one transaction only if
// a batch is ever bounded by its key budget alone.
func principalDeletion(nodes kvstore.Nodes, kind entity.PrincipalKind) step.Step {
	target := entity.ResourceUser
	if kind == entity.PrincipalGroup {
		target = entity.ResourceGroup
	}
	return func(ctx context.Context, op entity.Operation) (step.StepResult, error) {
		if op.Target.Kind != target || len(op.Target.ID) != len(entity.PrincipalID{}) {
			return step.StepResult{}, fmt.Errorf("%w: %s's target %s %x names no %s", step.ErrStepFailed, op.Kind, op.Target.Kind, op.Target.ID, target)
		}
		done, err := nodes.ContinueDelete(ctx, entity.Principal{Kind: kind, ID: entity.PrincipalID(op.Target.ID)})
		if se, ok := errors.AsType[*merrs.StoreError](err); ok && se.Code == merrs.ErrInvalidArgument {
			err = fmt.Errorf("%w: %w", step.ErrStepFailed, err)
		}
		return step.StepResult{Done: done}, err
	}
}

// deletionProgress is a principal deletion's progress: batches, as many as
// its rows need, which no cursor counts.
func deletionProgress(context.Context, entity.Operation) apiv1.Progress {
	return apiv1.Progress{Phase: "batches", Total: -1, Unit: "batches"}
}
