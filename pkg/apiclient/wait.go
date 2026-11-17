package apiclient

import (
	"context"
	"fmt"
	"net/url"

	"github.com/cubbit/dittofs/pkg/node/apiv1"
)

// Wait follows op to its end, succeeded, failed or cancelled, polling it
// with ?wait= so the server answers once it changes, and returns it as it
// ended; or, when a poll fails or answers a state this client does not
// know, the error and the operation as last read.
func (c *Client) Wait(ctx context.Context, op apiv1.Operation) (apiv1.Operation, error) {
	for {
		if done, err := ended(op.State); done || err != nil {
			return op, err
		}
		next, err := get[apiv1.Operation](ctx, c, "/api/v1/operations/"+url.PathEscape(op.ID)+"?wait=30s")
		if err != nil {
			return op, err
		}
		op = next
	}
}

// ended reports whether an operation in state s has ended, refusing a state
// this client does not know.
//
// decision: an unknown state is refused rather than polled, so a poll that
// returns at once never spins. An unchanged known state is polled again
// without a wait of the client's own, since the server parks each poll
// until the operation changes or 30 s pass. Overturned if a server is seen
// answering ?wait= at once without the change.
func ended(s apiv1.OpState) (bool, error) {
	switch s {
	case apiv1.OpSucceeded, apiv1.OpFailed, apiv1.OpCancelled:
		return true, nil
	case apiv1.OpPending, apiv1.OpRunning, apiv1.OpCancelling:
		return false, nil
	}
	return false, fmt.Errorf("apiclient: an operation in state %q, which this client does not know", s)
}
