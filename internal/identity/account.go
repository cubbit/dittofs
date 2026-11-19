package identity

import (
	"context"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"time"

	"golang.org/x/crypto/argon2"

	"github.com/cubbit/dittofs/pkg/metadata/entity"
	merrs "github.com/cubbit/dittofs/pkg/metadata/errors"
	"github.com/cubbit/dittofs/pkg/secrets"
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

// VerifyPassword checks p's password against its stored hash: ErrLogonFailure
// alike for a wrong password, the zero principal, a disabled or deleted
// account and one with no password; ErrUnavailable with no secret provider or
// when the store cannot answer, since a password it cannot read is never a
// wrong one. Each of those failures verifies password against a fixed dummy
// hash under a hashing slot, as a wrong password does, so the time taken does
// not tell them apart (RFC 18 §8.1); the comparison is constant-time.
func (s *Server) VerifyPassword(ctx context.Context, p entity.Principal, password []byte) error {
	if s.deps.Secrets == nil {
		return fmt.Errorf("%w: no secret provider", vfserr.ErrUnavailable)
	}
	phc, known, err := s.storedHash(ctx, p)
	if err != nil {
		return err
	}
	release, err := s.hashSlot(ctx)
	if err != nil {
		return err
	}
	ok, err := verifyPHC(phc, password)
	release()
	if s.verified != nil {
		s.verified(phc)
	}
	switch {
	// decision: a stored hash that does not parse is a store fault, answered
	// ErrUnavailable, so the right password never reads as a wrong one.
	// Overturned if the API ever stores a hash form a node must refuse as it
	// refuses a wrong password.
	case err != nil:
		return fmt.Errorf("%w: stored password hash: %w", vfserr.ErrUnavailable, err)
	case !ok || !known:
		return vfserr.ErrLogonFailure
	}
	return nil
}

// storedHash is p's password hash and true; the dummy hash and false for an
// account that cannot sign in or has no password.
//
// decision: the dummy hash equalises the hashing, not the store reads before
// it. An unknown name reads nothing, a disabled user or one with no password
// reads its record, and a wrong password reads its record and its sealed hash,
// so the paths differ by up to two store reads; RFC 18 §8.1's "same work" is
// held for the hashing, which is the bulk of it. Equalising the reads too
// would mean reading a dummy record and secret on every miss. Overturned if a
// backend whose reads are round trips shows the difference in a sign-in's
// latency next to the hashing's.
func (s *Server) storedHash(ctx context.Context, p entity.Principal) ([]byte, bool, error) {
	u, err := s.Account(ctx, p)
	switch {
	case errors.Is(err, vfserr.ErrLogonFailure):
		return s.dummy, false, nil
	case err != nil:
		return nil, false, err
	case u.Password == entity.SecretRef{}:
		return s.dummy, false, nil
	}
	// The hash is owned by the user's record under "password" (RFC 16 §2.3).
	owner := entity.SecretOwner{Record: entity.ResourceRef{Kind: entity.ResourceUser, ID: u.Principal.ID[:]}, Key: "password"}
	phc, err := s.deps.Secrets.Resolve(ctx, secrets.Storage, entity.SecretPassword, owner, u.Password)
	if err != nil {
		return nil, false, fmt.Errorf("%w: password hash: %w", vfserr.ErrUnavailable, err)
	}
	return phc, true, nil
}

// Account returns p's current record, read from the store on every call;
// ErrLogonFailure if it is disabled, deleted or marked deleted, or is no
// user's, a machine's included.
//
// decision: API users and file principals are one User record kept apart by
// rule: a role is never read by resolution, and an account with no file
// spelling cannot sign in over a file protocol. Overturned if an API account
// is needed that is not a User record.
func (s *Server) Account(ctx context.Context, p entity.Principal) (entity.User, error) {
	if p.Kind != entity.PrincipalUser {
		return entity.User{}, vfserr.ErrLogonFailure
	}
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

// phc is an argon2id hash's parts as its PHC string names them.
type phc struct {
	memory  uint32 // KiB
	time    uint32
	threads uint8
	salt    []byte
	key     []byte
}

// Bounds on a stored hash's cost, so a corrupt one cannot make a verification
// allocate or run without limit; NewHashes writes 19 MiB and two passes.
const (
	phcMaxMemory = 1 << 20 // KiB, 1 GiB
	phcMaxTime   = 16
)

var errPHC = errors.New("not an argon2id PHC string")

// parsePHC reads $argon2id$v=19$m=…,t=…,p=…$salt$key, salt and key in
// unpadded standard base64, the form NewHashes writes.
func parsePHC(b []byte) (phc, error) {
	f := strings.Split(string(b), "$")
	if len(f) != 6 || f[0] != "" || f[1] != "argon2id" || f[2] != fmt.Sprintf("v=%d", argon2.Version) {
		return phc{}, errPHC
	}
	var h phc
	if _, err := fmt.Sscanf(f[3], "m=%d,t=%d,p=%d", &h.memory, &h.time, &h.threads); err != nil {
		return phc{}, errPHC
	}
	if h.time == 0 || h.time > phcMaxTime || h.threads == 0 || h.memory < 8*uint32(h.threads) || h.memory > phcMaxMemory {
		return phc{}, errPHC
	}
	var err error
	if h.salt, err = base64.RawStdEncoding.Strict().DecodeString(f[4]); err != nil || len(h.salt) == 0 {
		return phc{}, errPHC
	}
	if h.key, err = base64.RawStdEncoding.Strict().DecodeString(f[5]); err != nil || len(h.key) == 0 {
		return phc{}, errPHC
	}
	return h, nil
}

// verifyPHC reports whether password derives the key of the hash b under its
// own salt and parameters, comparing in constant time.
func verifyPHC(b, password []byte) (bool, error) {
	h, err := parsePHC(b)
	if err != nil {
		return false, err
	}
	k := argon2.IDKey(password, h.salt, h.time, h.memory, h.threads, uint32(len(h.key)))
	return subtle.ConstantTimeCompare(k, h.key) == 1, nil
}
