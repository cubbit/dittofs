// Package auth is who calls the management API: the caller a request
// authenticated as, which the API's handlers read from their context.
package auth

import (
	"context"

	"github.com/cubbit/dittofs/pkg/metadata/entity"
)

// Caller is the account a request authenticated as: its principal, its
// role, whether it must change its password before anything else, and
// whether it called on the node's local socket.
type Caller struct {
	Principal  entity.Principal
	Role       entity.APIRole
	MustChange bool
	Local      bool
}

type callerKey struct{}

// With returns ctx carrying c. The API's authentication step is its one
// caller: a request's caller is the one Authenticate answered.
func With(ctx context.Context, c Caller) context.Context {
	return context.WithValue(ctx, callerKey{}, c)
}

// CallerFrom returns the caller ctx carries, and false for a request that
// authenticated as no one, as a public route's does.
func CallerFrom(ctx context.Context) (Caller, bool) {
	c, ok := ctx.Value(callerKey{}).(Caller)
	return c, ok
}
