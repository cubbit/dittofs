package node

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/netip"
	"time"

	"github.com/cubbit/dittofs/internal/node/step"
	"github.com/cubbit/dittofs/pkg/block/journal"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	merrs "github.com/cubbit/dittofs/pkg/metadata/errors"
	"github.com/cubbit/dittofs/pkg/metadata/kvstore"
	"github.com/cubbit/dittofs/pkg/node/apiv1"
)

// operationSteps is the runner's step table: the kinds this node drives.
func operationSteps(nodes kvstore.Nodes) map[string]stepRow {
	return map[string]stepRow{
		string(apiv1.OpUserDelete):  {run: principalDeletion(nodes, entity.PrincipalUser), progress: deletionProgress},
		string(apiv1.OpGroupDelete): {run: principalDeletion(nodes, entity.PrincipalGroup), progress: deletionProgress},
	}
}

// principalDeletion is the step of a user's or a group's deletion past its
// mark, kind naming which, its operation's target the principal's ID: one
// batch of Nodes.ContinueDelete a turn, done when it answers done. It keeps
// no cursor, since each batch finds what is left from the rows, and a record
// already gone answers done.
//
// decision: a principal that is not marked, a target that names no
// principal of kind, and a user whose record names a secret sealed for
// another holder or as another kind, or one whose current version does not
// decode (kvstore.ErrSecretUndecodable), fail the operation rather than
// retry it: only the mark marks, the running operation holds its target,
// refusing the deletion that could mark it, a secret keeps the kind and
// owner of its current version, and nothing but a direct edit of the store
// repairs a version that does not decode, so none changes on a retry. The
// user stays marked, as kvstore's ownSecrets decision says. Every other
// error, kv.ErrTooLarge included, is retried on the runner's backoff, since
// a failed deletion is never resumed and would leave the principal marked
// and half-deleted; and a batch's size can change between turns, as its
// quota drop reads every share. Fail a batch too large for one transaction
// only if a batch is ever bounded by its key budget alone.
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
		return step.StepResult{Done: done, More: !done}, failInvalid(err)
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
// the share's live state on this node, at 4 it forgets the share's tag and
// drops a batch of its files, and done ends the operation. The removal's own
// records hold where it stands; its cursor is the step the last batch
// answered, so a batch can tell whether it moved.
//
// decision: a batch answers More only when it moved, so a removal held where
// no batch moves it waits the runner's interval between batches rather than
// running them back to back: at 3, when it reached the step; at 4, when it
// reached the step, forgot the tag or left files to drop, so the batch that
// drops the last files answers none and the removal waits one interval
// before step 5; past 4, always, since those batches walk rows that never
// hold the walk, so each moves the removal's cursor or its step.
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
	reached := !bytes.Equal(op.Step, []byte{at})
	switch {
	case err != nil:
		return step.StepResult{}, failInvalid(err)
	case done:
		return step.StepResult{Done: true}, nil
	case at == 3:
		return step.StepResult{Next: []byte{at}, More: reached}, n.removeShare(ctx, id)
	case at == 4:
		moved, err := n.dropFiles(ctx, id)
		return step.StepResult{Next: []byte{at}, More: reached || moved}, failInvalid(err)
	}
	return step.StepResult{Next: []byte{at}, More: true}, nil
}

// dropFiles is a turn of step 4 of share id's removal on this node. Until
// the store records the share's tag forgotten, the turn drops the tag
// through the engine, its handle closed and the tag the store names
// forgotten in the journal, and only then records the shard forgotten, so a
// record that says forgotten never stands for a tag the journal still holds.
// Each later turn drops one batch of the shard's files with
// Nodes.ContinueShardRemoval. Each part is idempotent: a crash between the
// drop and the record drops again, and a record already made changes
// nothing. It reports whether the turn moved the removal: the tag forgotten,
// or files left to drop after its batch.
//
// decision: the engine's Drop is not bounded on the node's clock, as the
// store calls are: it waits only for the share's own work, which step 3's
// unbind has already joined, and for the journal's forget, and a Drop that
// hangs holds this one runner turn, which the watchdog reports as a stall.
// Bound it as step 3 bounds its unbind if a Drop is ever seen to hang.
//
// ponytail: step 4 runs once per share, on this node's journal alone: a share
// has one shard, its root's, this node is its primary, and no replica holds
// the tag. A whole-tag forget is safe here only because the removal reaches
// step 4 once every shard is offloaded. RFC 16 §2.3.2 step 3 (cluster) needs a
// replicated per-shard forget sent to every replica, and the tag kept while
// any other shard of the share the node serves has not recorded one; build
// those once a share has more than one shard or a shard more than one node.
func (n *Node) dropFiles(ctx context.Context, id entity.ShareID) (moved bool, err error) {
	var (
		sh entity.Share
		st entity.TagState
	)
	_, err = n.bounded(ctx, func(ctx context.Context) (err error) {
		if sh, _, err = n.meta.ShareIndex().Share(ctx, id); err != nil {
			return err
		}
		st, err = n.meta.Lifecycle().TagState(ctx, id)
		return err
	})
	if err != nil {
		return false, err
	}
	nodes := n.meta.Nodes()
	if st == entity.TagForgotten {
		var done bool
		_, err = n.bounded(ctx, func(ctx context.Context) (err error) {
			done, err = nodes.ContinueShardRemoval(ctx, id, sh.Shard)
			return err
		})
		if err != nil {
			return false, err
		}
		return !done, nil
	}
	if err := n.engine.Drop(ctx, id, journal.ShareTag(sh.Tag)); err != nil {
		return false, err
	}
	_, err = n.bounded(ctx, func(ctx context.Context) error {
		return nodes.RecordShardRemoval(ctx, id, sh.Shard, entity.TagForgotten)
	})
	return true, err
}

// failInvalid has an invalid argument from the store, or a secret version
// that does not decode, fail the operation.
func failInvalid(err error) error {
	if se, ok := errors.AsType[*merrs.StoreError](err); (ok && se.Code == merrs.ErrInvalidArgument) || errors.Is(err, kvstore.ErrSecretUndecodable) {
		return fmt.Errorf("%w: %w", step.ErrStepFailed, err)
	}
	return err
}

// liveStateBound is how long step 3 of a share's removal waits for the
// share's calls and work to end, on the node's clock.
//
// ponytail: one fixed bound, as RFC 23 sets none. Make it a setting once an
// operator needs another.
const liveStateBound = 30 * time.Second

// removeShare is step 3 of share id's removal on this node: its storage
// binding unbound, the filesystem's calls into it joined, and its open state
// dropped, each holder told; then the share leaves reconcile's running set
// and its refusals, and its shard's removal is recorded offloaded. Each part
// is idempotent, so a removal interrupted here runs it again. The parts wait
// at most liveStateBound on the node's clock, so a call or an engine call
// that never ends holds neither the runner nor a start: the step answers an
// error and is retried.
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
	var sh entity.Share
	_, err := n.bounded(ctx, func(ctx context.Context) (err error) {
		sh, _, err = n.meta.ShareIndex().Share(ctx, id)
		return err
	})
	if err != nil {
		return err
	}
	bounded, cancel := context.WithCancel(ctx)
	defer cancel()
	go func(bound <-chan struct{}) {
		select {
		case <-bound:
			cancel()
		case <-bounded.Done():
		}
	}(n.cfg.Clock.After(liveStateBound))
	if n.ending != nil {
		n.ending(id)
	}
	if err := n.endLiveState(bounded, id); err != nil {
		if ctx.Err() == nil && bounded.Err() != nil {
			return fmt.Errorf("share %s's live state did not end within %v: %w", id, liveStateBound, err)
		}
		return err
	}
	n.rec.unbound(id)
	_, err = n.bounded(ctx, func(ctx context.Context) error {
		return n.meta.Nodes().RecordShardRemoval(ctx, id, sh.Shard, entity.TagOffloaded)
	})
	return err
}

// endLiveState unbinds share id in the engine, joins the filesystem's calls
// into it, then drops its open state, each holder told.
func (n *Node) endLiveState(ctx context.Context, id entity.ShareID) error {
	if err := n.engine.Unbind(ctx, id, false); err != nil {
		return err
	}
	if err := n.fs.JoinShare(ctx, id); err != nil {
		return err
	}
	return n.open.Recovery().DropShare(ctx, id, netip.Addr{})
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
			_, err = n.bounded(ctx, func(ctx context.Context) (err error) {
				at, done, err = n.meta.Nodes().ContinueRemoval(ctx, sh.ID)
				return err
			})
			if err != nil {
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
	if err != nil {
		return nil, err
	}
	return got, nil
}
