// Package gss is the GSS-API mechanisms both adapters drive: each turns a
// peer's tokens into a Context through the service's SignIn. It holds no
// secret: the keytab, the replay cache and every check of a ticket are the
// authenticator's behind SignIn, which no adapter package imports.
package gss

import (
	"context"

	"github.com/cubbit/dittofs/pkg/vfs"
)

// Mechanism is one GSS mechanism. Each Step consumes the peer's token and
// returns the next, until done.
type Mechanism interface {
	Step(ctx context.Context, in []byte) (out []byte, done bool, err error)
	Context() *Context // once done; nil before, and after a failed Step
}

// Context is what a finished exchange leaves: what it proved, and the keys.
type Context struct {
	Evidence vfs.Evidence
	// Keys are what SignIn returned, Session the context key, kept whole.
	// They key the context's per-message tokens and are never copied into an
	// Identity.
	Keys vfs.Keys
}
