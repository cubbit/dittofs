package auth

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/cubbit/dittofs/internal/node/step"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/node/apiv1"
)

// The rotation's phases, as its progress reads them: none before the switch,
// switched while the old version waits out the last token it signed, pruned
// once it is gone.
const (
	phaseSwitched = "switched"
	phasePruned   = "pruned"
)

// rotateCursor is the rotation's durable cursor: the version the switch
// sealed and the switch's store time.
type rotateCursor struct {
	Phase   string    `json:"phase"`
	Version uint64    `json:"version"`
	At      time.Time `json:"at"`
}

// writer is the control plane's write.
type writer interface {
	Write(ctx context.Context, meta entity.WriteMeta, fn func(entity.ControlPlaneTx) error) error
}

// Rotation is the signing key's rotation, the step of an
// installation.rotate-signing-key operation, on the signing key tk verifies
// with.
type Rotation struct {
	store writer
	seal  sealer
	tk    *Tokens
}

// NewRotation returns the rotation of tk's signing key, sealed through s and
// written to store.
func NewRotation(store writer, s sealer, tk *Tokens) *Rotation {
	return &Rotation{store: store, seal: s, tk: tk}
}

// Step runs one turn of the rotation. The first seals the key's next
// version and switches signing to it, then refreshes the verifier, and
// records the version and the switch's store time. Each later turn returns
// the cursor unchanged, so the runner commits nothing, until every token the
// old versions signed has expired at the node's clock; then the operation
// ends, pruning every version below the new one in the transaction that
// ends it.
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
// ponytail: the wait counts from the switch's store time on this node,
// which refreshes its verifier at once. Another node signs with the old
// version for up to the second its verifier re-reads in, and a token this
// node signs between the switch's commit and the refresh is signed with it
// too, so either can be pruned up to that gap before it expires. Ceiling: a
// second node. Upgrade: wait for every node's status to report the new
// version before the 15 min start.
func (r *Rotation) Step(ctx context.Context, op entity.Operation) (step.StepResult, error) {
	if op.Target.Kind != entity.ResourceInstallation {
		return step.StepResult{}, fmt.Errorf("%w: %s's target %s names no installation", step.ErrStepFailed, op.Kind, op.Target.Kind)
	}
	if len(op.Step) == 0 {
		return r.switchKey(ctx)
	}
	var c rotateCursor
	if err := json.Unmarshal(op.Step, &c); err != nil {
		return step.StepResult{}, fmt.Errorf("%w: the rotation's cursor: %w", step.ErrStepFailed, err)
	}
	// A token's expiry is its issue time in whole seconds plus accessLife,
	// and a token the old versions signed was issued at the switch or
	// before, so this is the last expiry any of them carries.
	if r.tk.clk.Now().Before(jwt.NewNumericDate(c.At).Add(accessLife)) {
		return step.StepResult{Next: op.Step}, nil
	}
	c.Phase = phasePruned
	next, _ := json.Marshal(c) // a cursor always encodes
	ref := r.tk.ref
	return step.StepResult{Done: true, Next: next, End: func(tx entity.ControlPlaneTx) error {
		return tx.PruneSecret(ref, c.Version)
	}}, nil
}

// switchKey seals the key's next version, commits it as the current one,
// and refreshes the verifier, answering the cursor that records the switch.
func (r *Rotation) switchKey(ctx context.Context) (step.StepResult, error) {
	s, err := SealSigningKey(ctx, r.seal, [16]byte(r.tk.owner.Record.ID), r.tk.ref)
	if err != nil {
		return step.StepResult{}, err
	}
	var at time.Time
	err = r.store.Write(ctx, entity.WriteMeta{}, func(tx entity.ControlPlaneTx) error {
		at = tx.Now()
		return tx.PutSecret(s, s.Version-1)
	})
	if err != nil {
		return step.StepResult{}, err
	}
	// decision: a failed refresh does not fail the turn, which would seal
	// again; the verifier reads the versions again within a second anyway,
	// and the ponytail above covers that second.
	_ = r.tk.refresh(ctx)
	next, _ := json.Marshal(rotateCursor{Phase: phaseSwitched, Version: s.Version, At: at}) // a cursor always encodes
	return step.StepResult{Next: next}, nil
}

// RotationProgress is the rotation's progress, read from its cursor: no
// phase before the switch, then switched, then pruned.
func RotationProgress(_ context.Context, op entity.Operation) apiv1.Progress {
	var c rotateCursor
	_ = json.Unmarshal(op.Step, &c) // no cursor yet reads as before the switch
	done := map[string]int64{phaseSwitched: 1, phasePruned: 2}[c.Phase]
	return apiv1.Progress{Phase: c.Phase, Done: done, Total: 2, Unit: "steps"}
}
