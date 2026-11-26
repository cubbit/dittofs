package gss

import (
	"context"
	"errors"
	"fmt"

	"github.com/cubbit/dittofs/pkg/vfs"
)

// Key is the protocol's handle for an exchange: an RPCSEC_GSS context
// handle, an SMB session ID.
type Key string

// ErrNoExchange is a continuation naming no exchange in progress. It is a
// logon failure, as every error of an exchange is.
var ErrNoExchange = fmt.Errorf("%w: gss: no exchange in progress", vfs.ErrLogonFailure)

// Exchanges runs the exchanges of one protocol's sign-ins, each keyed by
// the protocol's handle for it.
type Exchanges struct{}

// NewExchanges is an Exchanges with no exchange in progress.
func NewExchanges() *Exchanges { return &Exchanges{} }

// Step runs one leg of the exchange under key. With newMech it starts an
// exchange on the mechanism newMech makes and steps it on in; without, it
// continues the exchange in progress under key, and with none is
// ErrNoExchange. Once done it answers the mechanism's last token and its
// Context, and keeps nothing. A mechanism's error comes back as it is, and
// nothing is kept.
//
// ponytail: an exchange completes in one leg, so none is ever in progress
// and a mechanism not done after its first leg is refused. Hold it under its
// key, bounded by the handshake deadline and charged to the
// connection-state pool, and clean it up on close or at the deadline, when a
// mechanism that takes a second leg, NTLM or SPNEGO, is added.
func (x *Exchanges) Step(ctx context.Context, key Key, newMech func() Mechanism, in []byte) (out []byte, done *Context, err error) {
	if newMech == nil {
		return nil, nil, ErrNoExchange
	}
	m := newMech()
	out, ok, err := m.Step(ctx, in)
	if err != nil {
		return nil, nil, err
	}
	if !ok {
		return nil, nil, logonFailure(errors.New("a mechanism that takes a second leg"))
	}
	return out, m.Context(), nil
}
