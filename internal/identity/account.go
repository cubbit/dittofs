package identity

import (
	"context"
	"fmt"
	"time"

	"github.com/cubbit/dittofs/pkg/metadata/entity"
	merrs "github.com/cubbit/dittofs/pkg/metadata/errors"
	"github.com/cubbit/dittofs/pkg/vfs/vfserr"
)

// Lookup resolves a name as presented to its account's principal by the
// name's user part, so every spelling of one account gives one principal; the
// zero principal for a user part with no account. It reads the store only,
// and returns an API-only account too, which no file protocol may present.
func (s *Server) Lookup(ctx context.Context, name string) (entity.Principal, error) {
	p, err := s.deps.Store.Principals().ByName(ctx, entity.PrincipalUser, entity.PrincipalUserPart(name))
	switch {
	case merrs.IsNotFoundError(err):
		return entity.Principal{}, nil
	case err != nil:
		return entity.Principal{}, fmt.Errorf("%w: %w", vfserr.ErrUnavailable, err)
	}
	return p, nil
}

// VerifyPassword checks p's password against its stored hash. With no
// secret provider it answers ErrUnavailable: a password it cannot read is
// never a wrong one.
//
// ponytail: past that check it answers ErrUnavailable too, verifying nothing,
// so a caller wired to it before the secret provider resolves anything signs
// no one in. Replace it with the record's read, the hash's Resolve and the
// argon2id verification under hashSlot once the provider resolves sealed
// secrets.
func (s *Server) VerifyPassword(ctx context.Context, p entity.Principal, password []byte) error {
	if s.deps.Secrets == nil {
		return fmt.Errorf("%w: no secret provider", vfserr.ErrUnavailable)
	}
	return vfserr.ErrUnavailable
}

// Account returns p's current record, read from the store on every call;
// ErrLogonFailure if it is disabled, deleted or marked deleted, or is no
// user's.
func (s *Server) Account(ctx context.Context, p entity.Principal) (entity.User, error) {
	u, err := s.deps.Store.Principals().User(ctx, p)
	switch {
	case merrs.IsNotFoundError(err):
		return entity.User{}, vfserr.ErrLogonFailure
	case err != nil:
		return entity.User{}, fmt.Errorf("%w: %w", vfserr.ErrUnavailable, err)
	case u.Disabled || u.Deleted:
		return entity.User{}, vfserr.ErrLogonFailure
	}
	return u, nil
}

// hashWait is how long a password verification waits for a hashing slot.
const hashWait = 5 * time.Second

// hashSlot takes one of the password verifications' slots, waiting at most
// hashWait on the clock, and returns its release.
//
// decision: concurrent argon2id verifications are bounded to GOMAXPROCS per
// node, and a call that waits hashWait for a slot answers ErrUnavailable,
// because sign-in is the path an unauthenticated burst reaches and each
// verification holds 19 MiB and a core. NewHashes is not bounded: it runs on
// an authenticated password set, which the API's throttle and roles bound.
// Overturned if a node must verify more passwords at once than it has cores.
func (s *Server) hashSlot(ctx context.Context) (release func(), err error) {
	release = func() { <-s.hashing }
	select {
	case s.hashing <- struct{}{}:
		return release, nil
	default:
	}
	select {
	case s.hashing <- struct{}{}:
		return release, nil
	case <-s.deps.Clock.After(hashWait):
		return nil, fmt.Errorf("%w: no hashing slot within %v", vfserr.ErrUnavailable, hashWait)
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
