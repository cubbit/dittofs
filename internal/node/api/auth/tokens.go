package auth

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
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

// refreshEvery is the least time between two reads of the key's versions,
// so a stream of forged kids reads the store at most once a second.
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

// Sealer seals a value as the next version of ref under the role of its kind.
type Sealer interface {
	Seal(ctx context.Context, kind entity.SecretKind, owner entity.SecretOwner, ref entity.SecretRef, value []byte) (entity.Secret, error)
}

// SealSigningKey seals a fresh random key as the next version of ref, owned
// by installation's signing_key. The caller commits it with PutSecret, and
// for a first version SetSigningKey, in its own Write.
func SealSigningKey(ctx context.Context, s Sealer, installation [16]byte, ref entity.SecretRef) (entity.Secret, error) {
	key := make([]byte, signingKeyLen)
	_, _ = rand.Read(key) // crypto/rand.Read never fails
	defer clear(key)
	return s.Seal(ctx, entity.SecretKey, signingKeyOwner(installation), ref, key)
}

// KeyVersion is one opened version of the signing key. Its value is the raw
// HMAC key, so every printing path shows the version alone.
type KeyVersion struct {
	Version uint64
	Value   []byte
}

// String prints the version, never the value.
func (k KeyVersion) String() string { return fmt.Sprintf("{version:%d}", k.Version) }

// Format prints String for every verb, %#v included, so no fmt verb reaches
// the value.
func (k KeyVersion) Format(f fmt.State, _ rune) { _, _ = fmt.Fprint(f, k.String()) }

// LogValue hands slog the version alone.
func (k KeyVersion) LogValue() slog.Value { return slog.Uint64Value(k.Version) }

// keyVersionView is what a KeyVersion marshals as: its number alone.
type keyVersionView struct {
	Version uint64 `json:"version" yaml:"version"`
}

// MarshalJSON writes the version, never the value.
func (k KeyVersion) MarshalJSON() ([]byte, error) { return json.Marshal(keyVersionView{k.Version}) }

// MarshalYAML writes the version, never the value.
func (k KeyVersion) MarshalYAML() (any, error) { return keyVersionView{k.Version}, nil }

// KeyVersions opens every version of ref still held, current first, checked
// against kind and owner.
type KeyVersions interface {
	Versions(ctx context.Context, kind entity.SecretKind, owner entity.SecretOwner, ref entity.SecretRef) ([]KeyVersion, error)
}

// Tokens issues and verifies access tokens: HS256 JWTs whose kid names the
// signing key's version, so a token signed before a rotation verifies while
// its version is held.
//
// ponytail: the opened versions are kept in memory and re-read at most once
// a second, on the first issue or verify after the last attempt is a second
// old, and at once when the rotation step calls refresh. So a node that did
// not run a rotation signs with the old version, and verifies a version
// pruned elsewhere, for up to a second, and each node reads the key once a
// second while tokens are in use. Upgrade: re-read on the node's settings
// generation, which every secret write raises.
type Tokens struct {
	keys  KeyVersions
	owner entity.SecretOwner
	ref   entity.SecretRef
	clk   Clock

	mu     sync.Mutex
	held   []KeyVersion // current first
	tried  time.Time    // the last read's start, failed or not
	failed error        // the last read's error while nothing is held
}

// NewTokens returns the tokens of the signing key ref, owned by
// installation's signing_key, opened through keys. Nothing is read until
// the first issue or verify.
func NewTokens(keys KeyVersions, installation [16]byte, ref entity.SecretRef, clk Clock) *Tokens {
	return &Tokens{keys: keys, owner: signingKeyOwner(installation), ref: ref, clk: clk}
}

// String prints the key's reference and held versions, never a value.
func (t *Tokens) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	vs := make([]uint64, len(t.held))
	for i, k := range t.held {
		vs[i] = k.Version
	}
	return fmt.Sprintf("{ref:%x versions:%v}", t.ref, vs)
}

// Format prints String for every verb, so no fmt verb reaches a value.
func (t *Tokens) Format(f fmt.State, _ rune) { _, _ = fmt.Fprint(f, t.String()) }

// LogValue hands slog String.
func (t *Tokens) LogValue() slog.Value { return slog.StringValue(t.String()) }

// Close clears the bytes of every version held and forgets them, so the raw
// keys do not outlive the storage key that wraps them; an issue or verify
// after it reads the versions again.
func (t *Tokens) Close() {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, k := range t.held {
		clear(k.Value)
	}
	t.held, t.tried = nil, time.Time{}
}

// refresh re-reads every held version of the signing key at once.
func (t *Tokens) refresh(ctx context.Context) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.reload(ctx)
}

// reload reads the versions, recording the attempt before the read so a
// failing store is read at most once a second too; t.mu is held.
func (t *Tokens) reload(ctx context.Context) error {
	t.tried = t.clk.Now()
	ks, err := t.keys.Versions(ctx, entity.SecretKey, t.owner, t.ref)
	if err == nil && len(ks) == 0 {
		// decision: unreachable through the real provider, whose Versions
		// returns the current version or an error; kept so held[0] cannot
		// panic.
		err = errors.New("the signing key holds no version")
	}
	if err != nil {
		t.failed = fmt.Errorf("auth: open the signing key: %w", err)
		return t.failed
	}
	t.held, t.failed = ks, nil
	return nil
}

// fresh re-reads the versions when the last attempt is a second old or
// more, and returns a read's error, the last one's while nothing is held;
// t.mu is held.
func (t *Tokens) fresh(ctx context.Context) error {
	if t.clk.Now().Sub(t.tried) >= refreshEvery {
		if err := t.reload(ctx); err != nil {
			return err
		}
	}
	if t.held == nil {
		return t.failed
	}
	return nil
}

// current is the version that signs now.
func (t *Tokens) current(ctx context.Context) (KeyVersion, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if err := t.fresh(ctx); err != nil {
		return KeyVersion{}, err
	}
	return t.held[0], nil
}

// byVersion is the held version v, and false when it is not held.
func (t *Tokens) byVersion(ctx context.Context, v uint64) (KeyVersion, bool, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if err := t.fresh(ctx); err != nil {
		return KeyVersion{}, false, err
	}
	for _, k := range t.held {
		if k.Version == v {
			return k, true, nil
		}
	}
	return KeyVersion{}, false, nil
}

// accessClaims are an access token's claims: exactly sub, sid, iat and exp,
// every other registered claim left empty and so omitted.
type accessClaims struct {
	Session string `json:"sid"`
	jwt.RegisteredClaims
}

// Claims is what a verified access token names.
type Claims struct {
	Principal entity.Principal
	Session   [16]byte
	IssuedAt  time.Time
}

// Issue signs an access token for principal's session, issued at iat in
// whole seconds and expiring accessLife later, with the current version.
func (t *Tokens) Issue(ctx context.Context, principal entity.Principal, session [16]byte, iat time.Time) (string, error) {
	k, err := t.current(ctx)
	if err != nil {
		return "", err
	}
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

// Verify checks raw's HS256 signature under the version its kid names and
// its expiry at the node's clock, with no leeway, and returns its claims.
// Every refusal wraps errToken; a key that cannot be read is returned as is.
//
// decision: iat is not checked against the clock, since the store time it
// was issued at runs up to the store's ceiling ahead of a node's clock, and
// a token issued at it would be refused on a node behind. Its expiry bounds
// it. Withdraw if issue times move to the node's clock.
func (t *Tokens) Verify(ctx context.Context, raw string) (Claims, error) {
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
		return Claims{}, readErr
	}
	if err != nil {
		return Claims{}, fmt.Errorf("%w: %v", errToken, err)
	}
	p, err := entity.ParsePrincipal(c.Subject)
	sid, serr := hex.DecodeString(c.Session)
	if err != nil || serr != nil || len(sid) != 16 || c.IssuedAt == nil {
		return Claims{}, fmt.Errorf("%w: its sub, sid or iat is malformed", errToken)
	}
	return Claims{Principal: p, Session: [16]byte(sid), IssuedAt: c.IssuedAt.Time}, nil
}
