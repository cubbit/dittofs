package identity

import (
	"context"

	"github.com/cubbit/dittofs/pkg/identity"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/vfs/vfserr"
)

// ponytail: every method below answers ErrUnavailable, one of the errors
// SignIn may answer and never "unknown principal" or a session, so a
// consumer wired to the server before its bodies exist fails closed. Each is
// replaced by its body: SignIn by NTLMv2, Kerberos and the domain's secure
// channel; Spell and Find by the principal indexes and the directory;
// Resolve by resolution; Lookup, VerifyPassword
// and Account by the principals' password checks.

// SignIn verifies one handshake round.
func (s *Server) SignIn(ctx context.Context, step identity.SignInStep) (identity.SignInResult, error) {
	return identity.SignInResult{}, vfserr.ErrUnavailable
}

// Spell returns p's spelling in scheme.
func (s *Server) Spell(ctx context.Context, p entity.Principal, scheme identity.Scheme) (identity.Spelling, error) {
	return identity.Spelling{}, vfserr.ErrUnavailable
}

// Find returns the principal holding sp.
func (s *Server) Find(ctx context.Context, sp identity.Spelling, o identity.FindOpts) (entity.Principal, error) {
	return entity.Principal{}, vfserr.ErrUnavailable
}

// Resolve resolves id to the Caller it acts as on the share p governs.
func (s *Server) Resolve(ctx context.Context, id identity.Identity, p entity.SharePolicy, narrowed identity.ReadOnlyCause) (*identity.Caller, error) {
	return nil, vfserr.ErrUnavailable
}

// Lookup resolves a name as presented to its account's principal.
func (s *Server) Lookup(ctx context.Context, name string) (entity.Principal, error) {
	return entity.Principal{}, vfserr.ErrUnavailable
}

// VerifyPassword checks p's password against its stored hash.
func (s *Server) VerifyPassword(ctx context.Context, p entity.Principal, password []byte) error {
	return vfserr.ErrUnavailable
}

// Account returns p's current record.
func (s *Server) Account(ctx context.Context, p entity.Principal) (entity.User, error) {
	return entity.User{}, vfserr.ErrUnavailable
}
