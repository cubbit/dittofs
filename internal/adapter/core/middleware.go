package core

import "context"

// Call is one step of the middleware chain: the handler, or what wraps it.
type Call func(ctx context.Context, r *Request) error

// Middleware wraps a Call: recovery, trace span, metrics, deadline, in that
// fixed order.
type Middleware func(next Call) Call
