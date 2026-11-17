package node

import (
	"context"
	"errors"
	"fmt"
	"net/netip"

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
		return step.StepResult{Done: done}, failInvalid(err)
	}
}

// deletionProgress is a principal deletion's progress: batches, as many as
// its rows need, which no cursor counts.
func deletionProgress(context.Context, entity.Operation) apiv1.Progress {
	return apiv1.Progress{Phase: "batches", Total: -1, Unit: "batches"}
}

// shareDeletion is the step of a share's deletion past its mark, its
// operation's target the share's ID. Each turn runs one batch of the removal
// with Nodes.ContinueRemoval and acts on the step it answers: at 3 it ends
// the share's live state on this node, at 4 it drops one batch of the share's
// files, and done ends the operation. It keeps no cursor: the removal's own
// records hold where it stands.
//
// decision: a share that is not being removed, and a target that names no
// share, fail the operation rather than retry it, as a principal's deletion
// does. kvstore.ErrRemovalOrder, a step asked for before the removal reached
// it, is retried, as is every other error.
func (n *Node) shareDeletion(ctx context.Context, op entity.Operation) (step.StepResult, error) {
	if op.Target.Kind != entity.ResourceShare || len(op.Target.ID) != len(entity.ShareID{}) {
		return step.StepResult{}, fmt.Errorf("%w: %s's target %s %x names no share", step.ErrStepFailed, op.Kind, op.Target.Kind, op.Target.ID)
	}
	id := entity.ShareID(op.Target.ID)
	at, done, err := n.meta.Nodes().ContinueRemoval(ctx, id)
	switch {
	case err != nil:
		return step.StepResult{}, failInvalid(err)
	case done:
		return step.StepResult{Done: true}, nil
	case at == 3:
		return step.StepResult{}, n.removeShare(ctx, id)
	case at == 4:
		return step.StepResult{}, n.dropFiles(ctx, id)
	}
	return step.StepResult{}, nil
}

// failInvalid has an invalid argument from the store fail the operation.
func failInvalid(err error) error {
	if se, ok := errors.AsType[*merrs.StoreError](err); ok && se.Code == merrs.ErrInvalidArgument {
		return fmt.Errorf("%w: %w", step.ErrStepFailed, err)
	}
	return err
}

// removeShare is step 3 of share id's removal on this node: its storage
// binding unbound, the filesystem's calls into it joined, and its open state
// dropped, each holder told; then the share leaves reconcile's running set
// and its refusals, and its shard's removal is recorded offloaded. Each part
// is idempotent, so a removal interrupted here runs it again.
//
// decision: removeShare has two callers, the share deletion's step while
// serving and Start's filesystem step after a crash; reconcile never calls it
// and never binds a share whose removal is marked. Overturned if a share can
// leave a node other than by its removal, such as a move to another node.
//
// ponytail: a share has one shard, its root's, and this node is its primary.
// Run it for each shard of the share this node is primary of once shares
// have more than one, or nodes more than one.
func (n *Node) removeShare(ctx context.Context, id entity.ShareID) error {
	sh, _, err := n.meta.ShareIndex().Share(ctx, id)
	if err != nil {
		return err
	}
	if err := n.engine.Unbind(ctx, id, false); err != nil {
		return err
	}
	if err := n.fs.JoinShare(ctx, id); err != nil {
		return err
	}
	if err := n.open.Recovery().DropShare(ctx, id, netip.Addr{}); err != nil {
		return err
	}
	n.rec.unbound(id)
	return n.meta.Nodes().RecordShardRemoval(ctx, id, sh.Shard, entity.TagOffloaded)
}

// dropFiles is one batch of step 4 of share id's removal: its files dropped
// with Nodes.ContinueShardRemoval. While the shard has not forgotten the
// share's tag, which that refuses with kvstore.ErrRemovalOrder, it first has
// the engine drop the tag and records the shard forgotten, in that order, so
// the record never stands for a tag the journal still holds.
func (n *Node) dropFiles(ctx context.Context, id entity.ShareID) error {
	sh, _, err := n.meta.ShareIndex().Share(ctx, id)
	if err != nil {
		return err
	}
	nodes := n.meta.Nodes()
	_, err = nodes.ContinueShardRemoval(ctx, id, sh.Shard)
	if !errors.Is(err, kvstore.ErrRemovalOrder) {
		return err
	}
	if err := n.engine.Drop(ctx, id); err != nil {
		return err
	}
	if err := nodes.RecordShardRemoval(ctx, id, sh.Shard, entity.TagForgotten); err != nil {
		return err
	}
	_, err = nodes.ContinueShardRemoval(ctx, id, sh.Shard)
	return err
}

// removalPhases names the steps of a share's removal, as its progress reads.
var removalPhases = [...]string{1: "mark", 2: "snapshots", 3: "live state", 4: "files", 5: "settings and grants", 6: "records"}

// removalProgress is a share deletion's progress, read from the removal's
// own record: the step it stands at, of six.
func (n *Node) removalProgress(ctx context.Context, op entity.Operation) apiv1.Progress {
	at, done, err := n.meta.Nodes().RemovalStep(ctx, entity.ShareID(op.Target.ID))
	switch {
	case err != nil:
		return apiv1.Progress{Phase: "unknown", Total: int64(len(removalPhases) - 1), Unit: "steps"}
	case done:
		return apiv1.Progress{Phase: "done", Done: int64(len(removalPhases) - 1), Total: int64(len(removalPhases) - 1), Unit: "steps"}
	}
	return apiv1.Progress{Phase: removalPhases[at], Done: int64(at) - 1, Total: int64(len(removalPhases) - 1), Unit: "steps"}
}

// joinRemovals is step 3 of each share removal a crash interrupted, run at
// start once the filesystem serves and before any call is admitted: each
// removal marked is moved on to step 3, its record's next step, and the
// share's live state ended there. A removal already past step 3 has its open
// state dropped again, which the served shard installed anew, and records
// nothing new.
func (n *Node) joinRemovals(ctx context.Context) error {
	for _, sh := range n.life.removing {
		at, done := uint8(0), false
		for at < 3 && !done {
			var err error
			if at, done, err = n.meta.Nodes().ContinueRemoval(ctx, sh.ID); err != nil {
				return fmt.Errorf("share %s's removal: %w", sh.ID, err)
			}
		}
		if done {
			continue
		}
		if err := n.removeShare(ctx, sh.ID); err != nil {
			return fmt.Errorf("share %s's removal: %w", sh.ID, err)
		}
	}
	return nil
}

// readRemoving reads every share being removed, bounded.
func (n *Node) readRemoving(ctx context.Context) ([]entity.Share, error) {
	var got []entity.Share
	_, err := n.bounded(ctx, func(ctx context.Context) error {
		ix := n.meta.ShareIndex()
		ids, err := ix.Removing(ctx)
		if err != nil {
			return err
		}
		shares := make([]entity.Share, 0, len(ids))
		for _, id := range ids {
			sh, _, err := ix.Share(ctx, id)
			if err != nil {
				return err
			}
			shares = append(shares, sh)
		}
		got = shares
		return nil
	})
	return got, err
}
