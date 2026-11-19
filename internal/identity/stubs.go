package identity

import (
	"context"

	"github.com/cubbit/dittofs/pkg/identity"
	"github.com/cubbit/dittofs/pkg/vfs/vfserr"
)

// ponytail: a Kerberos sign-in step answers ErrUnavailable, one of the errors
// SignIn may answer and never a session, so a client offering a ticket fails
// closed. Replace it with the AP-REQ's verification and the PAC (RFC 18 §3.4).
func (s *Server) signInKerberos(context.Context, identity.SignInStep) (identity.SignInResult, outcome, error) {
	return identity.SignInResult{}, outcomeUnavailable, vfserr.ErrUnavailable
}
