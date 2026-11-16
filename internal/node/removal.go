package node

import (
	"context"
	"errors"
	"fmt"

	"github.com/cubbit/dittofs/internal/node/step"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	merrs "github.com/cubbit/dittofs/pkg/metadata/errors"
	"github.com/cubbit/dittofs/pkg/metadata/kv"
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
// decision: a principal that is not marked, and a batch too large for one
// transaction, fail the operation rather than retry it. Only the mark marks,
// and a marked record is never put again, so neither changes on a retry; and
// the running operation would hold the target, refusing the deletion that
// could mark it. Retry them if a batch's size can ever change between turns.
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
		if se, ok := errors.AsType[*merrs.StoreError](err); (ok && se.Code == merrs.ErrInvalidArgument) || errors.Is(err, kv.ErrTooLarge) {
			err = fmt.Errorf("%w: %w", step.ErrStepFailed, err)
		}
		return step.StepResult{Done: done}, err
	}
}

// deletionProgress is a principal deletion's progress: batches, as many as
// its rows need, which no cursor counts.
func deletionProgress([]byte) apiv1.Progress {
	return apiv1.Progress{Phase: "batches", Total: -1, Unit: "batches"}
}
