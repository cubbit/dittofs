// Package auth is who calls the management API: the caller a request
// authenticated as, which the API's handlers read from their context.
package auth

import (
	"context"

	"github.com/cubbit/dittofs/pkg/metadata/entity"
)

// Caller is the account a request authenticated as: its principal, its name
// as it is now, the session or API token it presented, its role, and whether
// it must change its password before anything else. Its audit records name
// it by all of the first three.
type Caller struct {
	Principal entity.Principal
	Name      string
	// Kind and ID are the credential the caller presented, set by
	// Authenticate; Credential is its ID as the audit records it.
	Kind       CredentialKind
	ID         [16]byte
	Credential string
	Role       entity.APIRole
	MustChange bool
}

// CredentialKind is what a caller presented: no credential, or a session's
// access token.
type CredentialKind uint8

const (
	CredentialNone CredentialKind = iota
	CredentialSession
)

// Session is the session c authenticated under, false for a caller that
// presented none.
func (c Caller) Session() ([16]byte, bool) {
	return c.ID, c.Kind == CredentialSession
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

type localKey struct{}

// WithLocal returns ctx marked as a request that arrived on the node's local
// socket. The API's socket listener is its one caller.
func WithLocal(ctx context.Context) context.Context {
	return context.WithValue(ctx, localKey{}, true)
}

// Local reports whether ctx's request arrived on the node's local socket,
// whose caller is a process on the host holding the server's user's rights.
func Local(ctx context.Context) bool {
	local, _ := ctx.Value(localKey{}).(bool)
	return local
}
