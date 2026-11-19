package auth

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"

	"github.com/cubbit/dittofs/pkg/keyprovider"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/metadata/kv"
	"github.com/cubbit/dittofs/pkg/metadata/kv/badgerkv"
	kvkeys "github.com/cubbit/dittofs/pkg/metadata/kv/keys"
	"github.com/cubbit/dittofs/pkg/metadata/kvstore"
	"github.com/cubbit/dittofs/pkg/secrets"
	"github.com/cubbit/dittofs/pkg/sim"
)

// provider is the real secret provider as tokens take it: the signing key's
// kind is sealed under the storage role.
type provider struct{ p *secrets.Provider }

func (p provider) Seal(ctx context.Context, kind entity.SecretKind, owner entity.SecretOwner, ref entity.SecretRef, value []byte) (entity.Secret, error) {
	return p.p.Seal(ctx, secrets.Storage, kind, owner, ref, value, secrets.Meta{})
}

func (p provider) Versions(ctx context.Context, kind entity.SecretKind, owner entity.SecretOwner, ref entity.SecretRef) ([]Key, error) {
	vs, err := p.p.Versions(ctx, secrets.Storage, kind, owner, ref)
	ks := make([]Key, len(vs))
	for i, v := range vs {
		ks[i] = Key{Version: v.Number, Value: v.Value}
	}
	return ks, err
}

// keyFix is a store on Badger in memory, the real provider over it with a
// key file per role, and the installation's signing key sealed once.
type keyFix struct {
	t     *testing.T
	clk   *sim.TestClock
	db    kv.KV
	st    *kvstore.Store
	cp    kvstore.ControlPlane
	prov  provider
	inst  [16]byte
	ref   entity.SecretRef
	alice entity.Principal
	sess  [16]byte
}

func newKeyFix(t *testing.T) *keyFix {
	t.Helper()
	const passphrase = "tokens test passphrase"
	t.Setenv("DITTOFS_ENCRYPTION_PASSPHRASE", passphrase)
	keys := map[secrets.Role]keyprovider.KeyProvider{}
	for _, role := range []secrets.Role{secrets.Protocol, secrets.Storage} {
		b, err := keyprovider.GenerateKeyFile(passphrase)
		if err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(t.TempDir(), role.String()+".key")
		if err := os.WriteFile(path, b, 0o600); err != nil {
			t.Fatal(err)
		}
		kp, err := keyprovider.NewProvider(t.Context(), keyprovider.Config{Kind: keyprovider.KindLocal, File: path})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = kp.Close() })
		keys[role] = kp
	}
	clk := sim.NewTestClock(t0)
	db, err := badgerkv.Open(badgerkv.Options{Clock: clk.Now, CeilingAhead: 48 * time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	inst := uuid.New()
	if err := kvstore.Init(t.Context(), db, inst); err != nil {
		t.Fatal(err)
	}
	s, err := kvstore.Open(t.Context(), db)
	if err != nil {
		t.Fatal(err)
	}
	p, err := secrets.New(s.ControlPlane(), keys)
	if err != nil {
		t.Fatal(err)
	}
	f := &keyFix{t: t, clk: clk, db: db, st: s, cp: s.ControlPlane(), prov: provider{p}, inst: inst,
		ref: entity.SecretRef(uuid.New()), alice: entity.NewUser(), sess: uuid.New()}
	f.seal()
	return f
}

// seal seals the signing key's next version and commits it, naming it in the
// installation record with its first.
func (f *keyFix) seal() {
	f.t.Helper()
	s, err := SealSigningKey(f.t.Context(), f.prov, f.inst, f.ref)
	if err != nil {
		f.t.Fatalf("seal the signing key: %v", err)
	}
	err = f.cp.Write(f.t.Context(), entity.WriteMeta{}, func(tx entity.ControlPlaneTx) error {
		if err := tx.PutSecret(s, s.Version-1); err != nil {
			return err
		}
		if s.Version == 1 {
			return tx.SetSigningKey(f.ref)
		}
		return nil
	})
	if err != nil {
		f.t.Fatalf("commit signing key version %d: %v", s.Version, err)
	}
}

// prune deletes version v of the signing key from the KV, as a prune does.
func (f *keyFix) prune(v uint64) {
	f.t.Helper()
	err := f.db.Update(f.t.Context(), func(tx kv.Txn) error {
		return tx.Delete(kvkeys.Uint64(kvkeys.ID([]byte{kvkeys.KindSEC}, f.ref[:]), v))
	})
	if err != nil {
		f.t.Fatal(err)
	}
}

func (f *keyFix) tokens() *Tokens { return NewTokens(f.prov, f.inst, f.ref, f.clk) }

func (f *keyFix) issue(tk *Tokens) string {
	f.t.Helper()
	raw, err := tk.issue(f.t.Context(), f.alice, f.sess, f.clk.Now())
	if err != nil {
		f.t.Fatalf("issue: %v", err)
	}
	return raw
}

// segment decodes one of raw's dot-separated parts as a JSON object.
func segment(t *testing.T, raw string, i int) map[string]any {
	t.Helper()
	b, err := base64.RawURLEncoding.DecodeString(strings.Split(raw, ".")[i])
	if err != nil {
		t.Fatalf("segment %d: %v", i, err)
	}
	m := map[string]any{}
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("segment %d: %v", i, err)
	}
	return m
}

// An access token's header is exactly alg HS256, kid the version and typ, its
// claims exactly sub, sid, iat and exp in whole seconds, 15 min apart, with
// no role; it verifies back to the principal, session and issue time.
func TestTokenShape(t *testing.T) {
	f := newKeyFix(t)
	f.clk.Advance(300 * time.Millisecond) // a fraction the token drops
	tk := f.tokens()
	raw := f.issue(tk)

	h := segment(t, raw, 0)
	if got := slices.Sorted(maps.Keys(h)); !slices.Equal(got, []string{"alg", "kid", "typ"}) {
		t.Errorf("header keys: got %q, want alg, kid, typ", got)
	}
	if h["alg"] != "HS256" || h["kid"] != "1" || h["typ"] != "JWT" {
		t.Errorf("header: got %v, want alg HS256, kid 1, typ JWT", h)
	}
	c := segment(t, raw, 1)
	if got := slices.Sorted(maps.Keys(c)); !slices.Equal(got, []string{"exp", "iat", "sid", "sub"}) {
		t.Errorf("claim keys: got %q, want exp, iat, sid, sub", got)
	}
	if c["sub"] != f.alice.String() || c["iat"] != float64(t0.Unix()) || c["exp"] != float64(t0.Add(accessLife).Unix()) {
		t.Errorf("claims: got %v, want sub %s, iat %d, exp %d", c, f.alice, t0.Unix(), t0.Add(accessLife).Unix())
	}

	got, err := tk.verify(t.Context(), raw)
	want := claims{Principal: f.alice, Session: f.sess, IssuedAt: t0}
	if err != nil || got.Principal != want.Principal || got.Session != want.Session || !got.IssuedAt.Equal(want.IssuedAt) {
		t.Errorf("verify: got %+v, %v; want %+v", got, err, want)
	}
}

// A token verifies up to its exp and is refused at it, on the node's clock.
func TestTokenExpiry(t *testing.T) {
	f := newKeyFix(t)
	tk := f.tokens()
	raw := f.issue(tk)
	f.clk.Advance(accessLife - time.Second)
	if _, err := tk.verify(t.Context(), raw); err != nil {
		t.Errorf("verify a second before exp: %v, want it to verify", err)
	}
	f.clk.Advance(time.Second)
	if _, err := tk.verify(t.Context(), raw); !errors.Is(err, errToken) {
		t.Errorf("verify at exp: %v, want errToken", err)
	}
}

// A token signed with alg none, with HS512 under the right key, under a kid
// naming no held version, with no kid, or under another key is refused.
func TestTokenRefusals(t *testing.T) {
	f := newKeyFix(t)
	tk := f.tokens()
	key, err := tk.current(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	claims := accessClaims{Session: hex.EncodeToString(f.sess[:]), RegisteredClaims: jwt.RegisteredClaims{
		Subject: f.alice.String(), IssuedAt: jwt.NewNumericDate(t0), ExpiresAt: jwt.NewNumericDate(t0.Add(accessLife)),
	}}
	sign := func(m jwt.SigningMethod, kid any, k any) string {
		tok := jwt.NewWithClaims(m, claims)
		if kid != nil {
			tok.Header["kid"] = kid
		}
		raw, err := tok.SignedString(k)
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	// The same claims, signed as an access token is, verify, so each
	// refusal below is its header's or signature's.
	if _, err := tk.verify(t.Context(), sign(jwt.SigningMethodHS256, "1", key.Value)); err != nil {
		t.Fatalf("the claims signed as an access token: %v, want them to verify", err)
	}
	for name, raw := range map[string]string{
		"alg none":    sign(jwt.SigningMethodNone, "1", jwt.UnsafeAllowNoneSignatureType),
		"HS512":       sign(jwt.SigningMethodHS512, "1", key.Value),
		"unknown kid": sign(jwt.SigningMethodHS256, "9", key.Value),
		"no kid":      sign(jwt.SigningMethodHS256, nil, key.Value),
		"numeric kid": sign(jwt.SigningMethodHS256, 1, key.Value),
		"other key":   sign(jwt.SigningMethodHS256, "1", []byte("another key of thirty-two bytes!")),
	} {
		if _, err := tk.verify(t.Context(), raw); !errors.Is(err, errToken) {
			t.Errorf("%s: verify got %v, want errToken", name, err)
		}
	}
}

// A token signed with version 1 verifies once version 2 is added and signs,
// and is refused once 1 is pruned.
func TestTokenRotation(t *testing.T) {
	f := newKeyFix(t)
	tk := f.tokens()
	old := f.issue(tk)

	f.seal()
	if err := tk.refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	cur := f.issue(tk)
	if kid := segment(t, cur, 0)["kid"]; kid != "2" {
		t.Errorf("kid after the rotation: got %v, want 2", kid)
	}
	for name, raw := range map[string]string{"version 1": old, "version 2": cur} {
		if _, err := tk.verify(t.Context(), raw); err != nil {
			t.Errorf("%s with both held: %v, want it to verify", name, err)
		}
	}

	f.prune(1)
	if err := tk.refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := tk.verify(t.Context(), old); !errors.Is(err, errToken) {
		t.Errorf("version 1 after its prune: %v, want errToken", err)
	}
	if _, err := tk.verify(t.Context(), cur); err != nil {
		t.Errorf("version 2 after the prune: %v, want it to verify", err)
	}
}

// A verifier that has not seen a rotation made elsewhere reloads on the new
// kid, but at most once a second.
func TestTokenUnknownKidReloads(t *testing.T) {
	f := newKeyFix(t)
	here := f.tokens()
	if _, err := here.verify(t.Context(), f.issue(here)); err != nil {
		t.Fatal(err)
	}
	f.seal()
	there := f.tokens()
	raw := f.issue(there) // signed with version 2, which here has not loaded

	if _, err := here.verify(t.Context(), raw); !errors.Is(err, errToken) {
		t.Errorf("within a second of the last load: %v, want errToken", err)
	}
	f.clk.Advance(refreshEvery)
	if _, err := here.verify(t.Context(), raw); err != nil {
		t.Errorf("a second after the last load: %v, want it to reload and verify", err)
	}
}

// The signing key is one Secret of kind key owned by the installation's
// signing_key, named by the installation record, which only that owner
// resolves.
func TestSigningKeySecret(t *testing.T) {
	f := newKeyFix(t)
	ctx := t.Context()
	s, err := f.cp.Secret(ctx, f.ref, 0)
	if err != nil {
		t.Fatal(err)
	}
	owner := entity.SecretOwner{Record: entity.ResourceRef{Kind: entity.ResourceInstallation, ID: f.inst[:]}, Key: "signing_key"}
	if s.Kind != entity.SecretKey || !s.Owner.Equal(owner) || s.Version != 1 {
		t.Errorf("secret: kind %v, owner %+v, version %d; want key, %+v, 1", s.Kind, s.Owner, s.Version, owner)
	}

	in, err := f.st.Installation(ctx)
	if err != nil || in.SigningKey != f.ref {
		t.Errorf("installation's signing key: got %x, %v; want %x", in.SigningKey, err, f.ref)
	}

	v, err := f.prov.p.Resolve(ctx, secrets.Storage, entity.SecretKey, signingKeyOwner(f.inst), f.ref)
	if err != nil || len(v) != signingKeyLen {
		t.Errorf("resolve as the owner: %d bytes, %v; want %d", len(v), err, signingKeyLen)
	}
	other := signingKeyOwner(f.inst)
	other.Key = "machine_password"
	if _, err := f.prov.p.Resolve(ctx, secrets.Storage, entity.SecretKey, other, f.ref); !errors.Is(err, secrets.ErrWrongKind) {
		t.Errorf("resolve as another owner: %v, want ErrWrongKind", err)
	}
}
