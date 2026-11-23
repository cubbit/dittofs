package auth

import (
	"context"
	"crypto/rand"
	"fmt"
	"time"

	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/vfs/vfserr"
)

// CreateAPIToken creates, in tx, an API token for u, read tracked in tx,
// expiring at expires, and returns its record and the token itself, which is
// answered this once and kept only as its hash. An API token is made as a
// refresh token is: opaque, random, and no JWT, so Authenticate tells the two
// apart by its form. Its creation time is tx's store time raised to u's
// not-before, as a session's is. An account holding no role, whom a token
// would admit to nothing, and an expiry not after the creation time or later
// than api.token_max_lifetime after it, are refused ErrInvalid.
func (s *Service) CreateAPIToken(tx entity.ControlPlaneTx, u entity.User, expires time.Time) (entity.APIToken, string, error) {
	if u.Role == entity.RoleNone {
		return entity.APIToken{}, "", fmt.Errorf("%w: an API token is created for an account holding a role", vfserr.ErrInvalid)
	}
	created := issuedAt(tx.Now(), u.NotBefore)
	maxLife := s.d.Settings().TokenMaxLifetime
	if !expires.After(created) || expires.After(created.Add(maxLife)) {
		return entity.APIToken{}, "", fmt.Errorf("%w: spec.expires: %s is not after the token's creation at %s and within api.token_max_lifetime, %s, of it",
			vfserr.ErrInvalid, expires.Format(time.RFC3339), created.Format(time.RFC3339), maxLife)
	}
	raw, hash := newRefresh()
	t := entity.APIToken{Account: u.Principal, Hash: hash, Created: created, Expires: expires}
	_, _ = rand.Read(t.ID[:]) // crypto/rand.Read never fails
	return t, raw, tx.PutAPIToken(t)
}

// apiToken resolves raw as an API token: refused when it is no token
// newRefresh could have made, when no token's hash is its, and when it has
// expired, by the node's clock as a session's expiry is (see session).
func (s *Service) apiToken(ctx context.Context, raw string) (credential, error) {
	hash, ok := refreshHash(raw)
	if !ok {
		return credential{}, s.Refused("api_token")
	}
	t, found, err := s.d.Store.APITokenByHash(ctx, hash)
	switch {
	case err != nil:
		return credential{}, unavailable(err)
	case !found || !s.d.Clock.Now().Before(t.Expires):
		return credential{}, s.Refused("api_token")
	}
	return credential{principal: t.Account, kind: CredentialAPIToken, id: t.ID, issued: t.Created}, nil
}
