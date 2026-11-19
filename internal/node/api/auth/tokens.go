package auth

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/cubbit/dittofs/pkg/metadata/entity"
)

// accessLife is how long an access token verifies after its issue time.
const accessLife = 15 * time.Minute

// signingKeyLen is the signing key's length: HS256's block-sized key.
const signingKeyLen = 32

// refreshEvery is the least time between two reloads of the key's versions
// that an unknown kid triggers, so a stream of forged kids reads the store
// at most once a second.
const refreshEvery = time.Second

// errToken refuses an access token: malformed, signed with another key or
// algorithm, naming a version no longer held, or expired.
var errToken = errors.New("auth: access token refused")

// signingKeyOwner is the owner of the installation's signing key: the
// installation record's signing_key field.
func signingKeyOwner(installation [16]byte) entity.SecretOwner {
	return entity.SecretOwner{
		Record: entity.ResourceRef{Kind: entity.ResourceInstallation, ID: installation[:]},
		Key:    "signing_key",
	}
}

// sealer seals a value as the next version of ref under the role of its kind.
type sealer interface {
	Seal(ctx context.Context, kind entity.SecretKind, owner entity.SecretOwner, ref entity.SecretRef, value []byte) (entity.Secret, error)
}

// SealSigningKey seals a fresh random key as the next version of ref, owned
// by installation's signing_key. The caller commits it with PutSecret, and
// for a first version SetSigningKey, in its own Write.
func SealSigningKey(ctx context.Context, s sealer, installation [16]byte, ref entity.SecretRef) (entity.Secret, error) {
	key := make([]byte, signingKeyLen)
	_, _ = rand.Read(key) // crypto/rand.Read never fails
	defer clear(key)
	return s.Seal(ctx, entity.SecretKey, signingKeyOwner(installation), ref, key)
}

// Key is one opened version of the signing key.
type Key struct {
	Version uint64
	Value   []byte
}

// keyVersions opens every version of ref still held, current first, checked
// against kind and owner.
type keyVersions interface {
	Versions(ctx context.Context, kind entity.SecretKind, owner entity.SecretOwner, ref entity.SecretRef) ([]Key, error)
}

// Tokens issues and verifies access tokens: HS256 JWTs whose kid names the
// signing key's version, so a token signed before a rotation verifies while
// its version is held.
//
// ponytail: the opened versions are kept in memory, reloaded when the
// rotation step calls refresh and on an unknown kid at most once a second,
// so a rotation switched on another node is seen here up to a second late,
// and a version pruned there verifies here until a reload. Upgrade: reload
// on the node's settings generation, which every secret write raises.
type Tokens struct {
	keys  keyVersions
	owner entity.SecretOwner
	ref   entity.SecretRef
	clk   clock

	mu     sync.Mutex
	held   []Key // current first
	loaded time.Time
}

// NewTokens returns the tokens of the signing key ref, owned by
// installation's signing_key, opened through keys. Nothing is read until
// the first issue or verify.
func NewTokens(keys keyVersions, installation [16]byte, ref entity.SecretRef, clk clock) *Tokens {
	return &Tokens{keys: keys, owner: signingKeyOwner(installation), ref: ref, clk: clk}
}

// refresh reloads every held version of the signing key.
func (t *Tokens) refresh(ctx context.Context) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.reload(ctx)
}

// reload reads the versions; t.mu is held.
func (t *Tokens) reload(ctx context.Context) error {
	ks, err := t.keys.Versions(ctx, entity.SecretKey, t.owner, t.ref)
	if err != nil {
		return fmt.Errorf("auth: open the signing key: %w", err)
	}
	if len(ks) == 0 {
		return errors.New("auth: the signing key holds no version")
	}
	t.held, t.loaded = ks, t.clk.Now()
	return nil
}

// current is the version that signs now, loaded on first use.
func (t *Tokens) current(ctx context.Context) (Key, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.held == nil {
		if err := t.reload(ctx); err != nil {
			return Key{}, err
		}
	}
	return t.held[0], nil
}

// byVersion is the held version v, reloaded once when it is not held and
// the last load is a second old or more.
func (t *Tokens) byVersion(ctx context.Context, v uint64) (Key, bool, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	find := func() (Key, bool) {
		for _, k := range t.held {
			if k.Version == v {
				return k, true
			}
		}
		return Key{}, false
	}
	if k, ok := find(); ok {
		return k, true, nil
	}
	if t.held != nil && t.clk.Now().Sub(t.loaded) < refreshEvery {
		return Key{}, false, nil
	}
	if err := t.reload(ctx); err != nil {
		return Key{}, false, err
	}
	k, ok := find()
	return k, ok, nil
}

// accessClaims are an access token's claims: exactly sub, sid, iat and exp,
// every other registered claim left empty and so omitted.
type accessClaims struct {
	Session string `json:"sid"`
	jwt.RegisteredClaims
}

// claims is what a verified access token names.
type claims struct {
	Principal entity.Principal
	Session   [16]byte
	IssuedAt  time.Time
}

// issue signs an access token for principal's session, issued at iat in
// whole seconds and expiring accessLife later, with the current version.
func (t *Tokens) issue(ctx context.Context, principal entity.Principal, session [16]byte, iat time.Time) (string, error) {
	k, err := t.current(ctx)
	if err != nil {
		return "", err
	}
	iat = iat.Truncate(time.Second)
	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, accessClaims{
		Session: hex.EncodeToString(session[:]),
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   principal.String(),
			IssuedAt:  jwt.NewNumericDate(iat),
			ExpiresAt: jwt.NewNumericDate(iat.Add(accessLife)),
		},
	})
	tok.Header["kid"] = strconv.FormatUint(k.Version, 10)
	return tok.SignedString(k.Value)
}

// verify checks raw's HS256 signature under the version its kid names and
// its expiry at the node's clock, with no leeway, and returns its claims.
// Every refusal wraps errToken; a key that cannot be read is returned as is.
//
// decision: iat is not checked against the clock, since the store time it
// was issued at runs up to the store's ceiling ahead of a node's clock, and
// a token issued at it would be refused on a node behind. Its expiry bounds
// it. Withdraw if issue times move to the node's clock.
func (t *Tokens) verify(ctx context.Context, raw string) (claims, error) {
	var readErr error
	var c accessClaims
	_, err := jwt.ParseWithClaims(raw, &c, func(tok *jwt.Token) (any, error) {
		kid, _ := tok.Header["kid"].(string)
		v, err := strconv.ParseUint(kid, 10, 64)
		if err != nil {
			return nil, errors.New("the kid names no version")
		}
		k, ok, err := t.byVersion(ctx, v)
		if err != nil {
			readErr = err
			return nil, err
		}
		if !ok {
			return nil, fmt.Errorf("version %d of the signing key is not held", v)
		}
		return k.Value, nil
	},
		jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Alg()}),
		jwt.WithTimeFunc(t.clk.Now),
		jwt.WithExpirationRequired(),
	)
	if readErr != nil {
		return claims{}, readErr
	}
	if err != nil {
		return claims{}, fmt.Errorf("%w: %v", errToken, err)
	}
	p, err := entity.ParsePrincipal(c.Subject)
	sid, serr := hex.DecodeString(c.Session)
	if err != nil || serr != nil || len(sid) != 16 || c.IssuedAt == nil {
		return claims{}, fmt.Errorf("%w: its sub, sid or iat is malformed", errToken)
	}
	return claims{Principal: p, Session: [16]byte(sid), IssuedAt: c.IssuedAt.Time}, nil
}
