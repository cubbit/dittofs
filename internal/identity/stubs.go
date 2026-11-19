package identity

import (
	"context"

	"github.com/cubbit/dittofs/pkg/identity"
	"github.com/cubbit/dittofs/pkg/vfs/vfserr"
)

// ponytail: SignIn answers ErrUnavailable, one of the errors it may answer
// and never "unknown principal" or a session, so a consumer wired to the
// server before its body exists fails closed. Replace it with NTLMv2,
// Kerberos and the domain's secure channel.

// SignIn verifies one handshake round.
func (s *Server) SignIn(ctx context.Context, step identity.SignInStep) (identity.SignInResult, error) {
	return identity.SignInResult{}, vfserr.ErrUnavailable
}
