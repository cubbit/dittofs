package auth

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/cubbit/dittofs/internal/node/step"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/node/apiv1"
)

// pruneWait is how long after the switch the old versions are kept: an
// access token's life, plus the two seconds a node that did not run the
// switch may still sign with the old version, its verifier re-read at most a
// second late on either side of the switch's commit.
const pruneWait = accessLife + 2*refreshEvery

// rotateCursor is the rotation's durable cursor, recorded at the switch:
// the version it sealed and its store time.
type rotateCursor struct {
	Version uint64    `json:"version"`
	At      time.Time `json:"at"`
}

// Writer is the control plane's write.
type Writer interface {
	Write(ctx context.Context, meta entity.WriteMeta, fn func(entity.ControlPlaneTx) error) error
}

// RotationStep is the step of an installation.rotate-signing-key operation
// on the signing key tk verifies with: sealed through s, written to store,
// its switch audited as on node.
//
// The first turn seals the key's next version and switches signing to it,
// then refreshes the verifier, and records the version and the switch's
// store time. Each later turn returns the cursor unchanged, so the runner
// commits nothing, until pruneWait has passed at the node's clock; then the
// operation ends, pruning every version below the new one in the
// transaction that ends it. A target other than the installation, and an
// installation that names no signing key, fail the operation: neither
// changes on a retry.
//
// A switch whose cursor was not committed runs again and seals one more
// version. That is safe: the cursor names the last switch, whose store time
// is past every token an earlier version signed.
//
// decision: the prune calls tx.PruneSecret, not the provider's Prune, so it
// skips the floor of the versions node statuses report. The retention is
// time-based: no token an old version signed verifies once its 15 min have
// passed, so nothing still needs the version, wherever it is held.
//
// ponytail: the wait counts from the switch's store time on this node, with
// two seconds' margin for a node whose verifier re-reads the versions up to
// a second late. Its ceilings: a node whose clock runs behind this node's
// store time by more than the margin signs with the old version after the
// window and has such a token refused before it expires; a node that has
// not yet re-read refuses a token the new version signed for up to a
// second; and a node down across the whole wait keeps nothing it needs.
// Ceiling: a second node. Upgrade: wait for every node's status to report
// the new version before the 15 min start.
func RotationStep(store Writer, s Sealer, tk *Tokens, node entity.NodeID) step.Step {
	return func(ctx context.Context, op entity.Operation) (step.StepResult, error) {
		inst := tk.owner.Record.ID
		if op.Target.Kind != entity.ResourceInstallation || !bytes.Equal(op.Target.ID, inst) {
			return step.StepResult{}, fmt.Errorf("%w: %s's target %s %x names no installation but %x", step.ErrStepFailed, op.Kind, op.Target.Kind, op.Target.ID, inst)
		}
		if tk.ref == (entity.SecretRef{}) {
			return step.StepResult{}, fmt.Errorf("%w: the installation names no signing key", step.ErrStepFailed)
		}
		if len(op.Step) == 0 {
			return switchKey(ctx, store, s, tk, node, op)
		}
		var c rotateCursor
		if err := json.Unmarshal(op.Step, &c); err != nil {
			return step.StepResult{}, fmt.Errorf("%w: the rotation's cursor: %w", step.ErrStepFailed, err)
		}
		// A token's expiry is its issue time in whole seconds plus accessLife,
		// and a token the old versions signed was issued by the switch, give or
		// take the margin, so this is past the last expiry any of them carries.
		if tk.clk.Now().Before(jwt.NewNumericDate(c.At).Add(pruneWait)) {
			return step.StepResult{Next: op.Step}, nil
		}
		ref := tk.ref
		return step.StepResult{Done: true, Next: op.Step, End: func(tx entity.ControlPlaneTx) error {
			return tx.PruneSecret(ref, c.Version)
		}}, nil
	}
}

// switchKey seals the key's next version, commits it as the current one,
// audited as op's account on node, and refreshes the verifier, answering
// the cursor that records the switch.
func switchKey(ctx context.Context, store Writer, s Sealer, tk *Tokens, node entity.NodeID, op entity.Operation) (step.StepResult, error) {
	sealed, err := SealSigningKey(ctx, s, [16]byte(tk.owner.Record.ID), tk.ref)
	if err != nil {
		return step.StepResult{}, err
	}
	var at time.Time
	err = store.Write(ctx, entity.WriteMeta{Audit: switchAudit(op, node)}, func(tx entity.ControlPlaneTx) error {
		at = tx.Now()
		return tx.PutSecret(sealed, sealed.Version-1)
	})
	if err != nil {
		return step.StepResult{}, err
	}
	// decision: a failed refresh does not fail the turn, which would seal
	// again; the verifier reads the versions again within a second anyway,
	// and pruneWait's margin covers that second.
	_ = tk.refresh(ctx)
	next, _ := json.Marshal(rotateCursor{Version: sealed.Version, At: at}) // a cursor always encodes
	return step.StepResult{Next: next}, nil
}

// switchAudit is the record of the switch: the signing key set, as a secret
// is recorded, never its value, as op's account on node.
func switchAudit(op entity.Operation, node entity.NodeID) *entity.AuditRecord {
	return &entity.AuditRecord{
		Actor:   op.Account,
		Node:    node,
		Target:  op.Target,
		Change:  []byte(`{"signing_key":"set"}`),
		Outcome: []byte("applied"),
	}
}

// RotationProgress is the rotation's progress, read from what the runner
// holds: no cursor before the switch, a cursor once it is switched, and the
// operation succeeded once the old versions are pruned.
func RotationProgress(_ context.Context, op entity.Operation) apiv1.Progress {
	switch {
	case op.State == entity.OpSucceeded:
		return apiv1.Progress{Phase: "pruned", Done: 2, Total: 2, Unit: "steps"}
	case len(op.Step) > 0:
		return apiv1.Progress{Phase: "switched", Done: 1, Total: 2, Unit: "steps"}
	}
	return apiv1.Progress{Total: 2, Unit: "steps"}
}
