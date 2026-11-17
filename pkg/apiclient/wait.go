package apiclient

import (
	"context"
	"net/url"

	"github.com/cubbit/dittofs/pkg/node/apiv1"
)

// pollWait is how long one poll of an operation parks on the server.
const pollWait = "30s"

// Wait follows op to its end, succeeded, failed or cancelled, polling it
// with ?wait= so the server answers once it changes, and returns it as it
// ended; or, when a poll fails, the error and the operation as last read.
func (c *Client) Wait(ctx context.Context, op apiv1.Operation) (apiv1.Operation, error) {
	for op.State != apiv1.OpSucceeded && op.State != apiv1.OpFailed && op.State != apiv1.OpCancelled {
		next, err := get[apiv1.Operation](ctx, c, "/api/v1/operations/"+url.PathEscape(op.ID)+"?wait="+pollWait)
		if err != nil {
			return op, err
		}
		op = next
	}
	return op, nil
}
