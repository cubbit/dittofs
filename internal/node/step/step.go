// Package step is what an operation's step function speaks to the node's
// operation runner: what one turn of the work answers, and how its progress
// reads. A step defined outside the node imports this package, never the node.
package step

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/node/apiv1"
)

// ErrStepFailed, wrapped by a step's error, fails the operation with that
// error. A step returns a *apiv1.Problem instead to fail it with a coded
// problem. Any other error is retried.
var ErrStepFailed = errors.New("step failed")

// StepResult is what one turn of an operation's work answers.
type StepResult struct {
	// Next is the step's durable cursor, recorded as Operation.Step and
	// handed back on the next turn.
	Next []byte
	// Done says the work has ended.
	Done bool
	// Result, set with Done, is recorded as Operation.Result.
	Result json.RawMessage
	// End, set with Done, writes in the transaction that ends the operation:
	// both commit or neither does.
	End func(tx entity.ControlPlaneTx) error
}

// Step runs one bounded turn of op's work from op.Step, the cursor its last
// committed turn answered, and must be safe to run again from that cursor:
// a turn whose answer was not committed runs again.
type Step func(ctx context.Context, op entity.Operation) (StepResult, error)

// Progress reads an operation's progress from its cursor.
type Progress func(next []byte) apiv1.Progress

// Failed reports whether err fails the operation a step returned it for,
// and the problem body recorded as its Error: a *apiv1.Problem as it is, any
// other error wrapping ErrStepFailed as an internal problem carrying its
// text. An error neither is retried, and Failed answers false.
//
// ponytail: an error wrapping ErrStepFailed is recorded with no code. Map the
// owner's error values to codes once the management API's problem table
// exists, and use it here.
func Failed(err error) ([]byte, bool) {
	p, ok := errors.AsType[*apiv1.Problem](err)
	if !ok {
		if !errors.Is(err, ErrStepFailed) {
			return nil, false
		}
		p = &apiv1.Problem{Status: http.StatusInternalServerError, Title: "the operation failed", Detail: err.Error()}
	}
	b, _ := json.Marshal(p) // a Problem holds strings, ints and bools, which always marshal
	return b, true
}
