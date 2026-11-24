package identity

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/cubbit/dittofs/internal/node/step"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/metadata/kvstore"
	"github.com/cubbit/dittofs/pkg/node/apiv1"
)

// ntHashBatch is how many NT hashes one turn of the deletion drops.
//
// ponytail: a fixed 256, under every backend's per-transaction limit for 256
// secrets of a few versions each and their users' records. Size it from the
// KV's limits when a backend's limit falls below that or a secret keeps many
// versions.
const ntHashBatch = 256

// ntHashCursor is the NT hashes' deletion's durable cursor, recorded as the
// operation's Step after each turn: the last reference a batch passed, and
// how many NT hashes the turns have dropped.
type ntHashCursor struct {
	After   entity.SecretRef `json:"after"`
	Dropped int64            `json:"dropped"`
}

// ntHashCursorOf decodes op's cursor; no cursor yet is the first turn's.
func ntHashCursorOf(op entity.Operation) (ntHashCursor, error) {
	var c ntHashCursor
	if len(op.Step) == 0 {
		return c, nil
	}

	return c, json.Unmarshal(op.Step, &c)
}

// NTHashDeleteStep runs one turn of an identity.nt-hash-delete operation: it
// deletes every version of the next batch of NT hashes after the cursor
// op.Step holds, zeroing each user's reference to one, and once none is left
// answers Done with an End that records the deletion completed on the
// installation. A secret or user record the store cannot decode fails the
// operation; every other error is retried with the same cursor.
//
// decision: a record that does not decode fails the operation rather than
// being retried, since every later turn would stop on it the same way, and
// the failure leaves NTLMOff.Deleted zero, so NTLM stays refused until a
// deletion started after the repair completes (RFC 23 §4.1). Overturned if
// the store ever skips a record it cannot decode.
//
// ponytail: the count is advisory. A turn adds what its batch dropped, so a
// batch committed before a crash, its turn's cursor never recorded, is not
// counted when the turn runs again and drops nothing. Count inside the
// batch's transaction if an exact total is ever needed.
func (s *Server) NTHashDeleteStep(ctx context.Context, op entity.Operation) (step.StepResult, error) {
	c, err := ntHashCursorOf(op)
	if err != nil {
		return step.StepResult{}, fmt.Errorf("%w: the NT hashes' deletion's cursor: %w", step.ErrStepFailed, err)
	}

	if s.deps.ControlPlane == nil {
		return step.StepResult{}, fmt.Errorf("%w: no control plane", step.ErrStepFailed)
	}

	next, dropped, done, err := s.deps.ControlPlane.DropNTHashes(ctx, c.After, ntHashBatch)
	switch {
	case kvstore.Undecodable(err):
		return step.StepResult{}, fmt.Errorf("%w: %w", step.ErrStepFailed, err)
	case err != nil:
		return step.StepResult{}, err
	}

	c.After, c.Dropped = next, c.Dropped+int64(dropped)
	b, _ := json.Marshal(c) // a reference and an integer always marshal

	res := step.StepResult{Next: b, Done: done, More: !done}
	if done {
		// The one write of NTLMOff in identity: it commits with the
		// operation's end or not at all.
		res.End = func(tx entity.ControlPlaneTx) error { return tx.SetNTHashDeleted(op.ID) }
	}

	return res, nil
}

// NTHashDeleteProgress is the NT hashes' deletion's progress, read from its
// cursor: how many it has dropped, out of a total no record keeps.
func (s *Server) NTHashDeleteProgress(_ context.Context, op entity.Operation) apiv1.Progress {
	c, _ := ntHashCursorOf(op) // a cursor that does not decode reads as none dropped
	return apiv1.Progress{Phase: "nt hashes", Done: c.Dropped, Total: -1, Unit: "nt hashes"}
}
