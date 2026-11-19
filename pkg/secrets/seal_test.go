package secrets

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/cubbit/dittofs/internal/prof"
	"github.com/cubbit/dittofs/pkg/keyprovider"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	merrs "github.com/cubbit/dittofs/pkg/metadata/errors"
	"github.com/cubbit/dittofs/pkg/metadata/kv"
	"github.com/cubbit/dittofs/pkg/metadata/kv/badgerkv"
	"github.com/cubbit/dittofs/pkg/metadata/kv/kvtest"
	"github.com/cubbit/dittofs/pkg/metadata/kvstore"
)

var errSealInjected = errors.New("secrets test: injected store fault")

// sealKeys writes one real key file per role in a temporary directory and
// opens both, closed when the test ends.
func sealKeys(t testing.TB) map[Role]keyprovider.KeyProvider {
	t.Helper()

	const passphrase = "seal test passphrase"

	t.Setenv("DITTOFS_ENCRYPTION_PASSPHRASE", passphrase)

	dir := t.TempDir()
	keys := map[Role]keyprovider.KeyProvider{}

	for _, role := range []Role{Protocol, Storage} {
		b, err := keyprovider.GenerateKeyFile(passphrase)
		if err != nil {
			t.Fatalf("generate the %s key file: %v", role, err)
		}

		path := filepath.Join(dir, role.String()+".key")
		if err := os.WriteFile(path, b, 0o600); err != nil {
			t.Fatalf("write the %s key file: %v", role, err)
		}

		kp, err := keyprovider.NewProvider(
			context.Background(),
			keyprovider.Config{
				Kind: keyprovider.KindLocal,
				File: path,
			},
		)
		if err != nil {
			t.Fatalf("open the %s key file: %v", role, err)
		}

		t.Cleanup(func() {
			_ = kp.Close()
		})

		keys[role] = kp
	}

	return keys
}

// sealStore initialises a control-plane store over db, closed when the test
// ends. An empty dir keeps the store in memory.
func sealStore(t testing.TB, dir string) kv.KV {
	t.Helper()

	db, err := badgerkv.Open(badgerkv.Options{Dir: dir})
	if err != nil {
		t.Fatalf("open the store: %v", err)
	}

	t.Cleanup(func() {
		_ = db.Close()
	})

	return db
}

func sealControlPlane(t testing.TB, db kv.KV) kvstore.ControlPlane {
	t.Helper()

	ctx := context.Background()
	if err := kvstore.Init(ctx, db, [16]byte{1}); err != nil {
		t.Fatalf("init the store: %v", err)
	}

	st, err := kvstore.Open(ctx, db)
	if err != nil {
		t.Fatalf("open the control plane: %v", err)
	}

	return st.ControlPlane()
}

func sealProvider(
	t testing.TB,
	cp kvstore.ControlPlane,
	keys map[Role]keyprovider.KeyProvider,
) *Provider {
	t.Helper()

	p, err := New(cp, keys)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	return p
}

func sealRef(t testing.TB) entity.SecretRef {
	t.Helper()

	var ref entity.SecretRef
	if _, err := rand.Read(ref[:]); err != nil {
		t.Fatalf("draw a reference: %v", err)
	}

	return ref
}

// sealOwner has every part of the owner set, so a binding that leaves one
// out cannot match it by accident.
func sealOwner() entity.SecretOwner {
	return entity.SecretOwner{
		Record: entity.ResourceRef{
			Kind: entity.ResourceUser,
			ID:   []byte{0x0a, 0x0b, 0x0c},
		},
		Scope: entity.SettingScope{
			Kind: entity.ScopeNode,
			ID:   [16]byte{0x5a, 15: 0x5b},
		},
		Key: "nt_hash",
	}
}

// sealCommit commits s as its caller does: PutSecret inside an audited Write.
func sealCommit(ctx context.Context, cp kvstore.ControlPlane, s entity.Secret) error {
	meta := entity.WriteMeta{
		Audit: &entity.AuditRecord{
			Target: entity.ResourceRef{
				Kind: entity.ResourceSecret,
				ID:   s.ID[:],
			},
			Change: []byte("set"),
		},
	}

	return cp.Write(ctx, meta, func(tx entity.ControlPlaneTx) error {
		return tx.PutSecret(s, s.Version-1)
	})
}

// sealOpen opens a sealed record with kp and the binding b.
func sealOpen(
	ctx context.Context,
	kp keyprovider.KeyProvider,
	s entity.Secret,
	b binding,
) ([]byte, error) {
	wrapped, sealed, err := parseEnvelope(s.Value)
	if err != nil {
		return nil, err
	}

	dataKey, err := kp.Unwrap(ctx, wrapped, s.KeyID)
	if err != nil {
		return nil, err
	}

	return openValue(dataKey, sealed, b.additionalData())
}

// sealDataKey unwraps the data key of a sealed record.
func sealDataKey(
	t *testing.T,
	kp keyprovider.KeyProvider,
	s entity.Secret,
) []byte {
	t.Helper()

	wrapped, _, err := parseEnvelope(s.Value)
	if err != nil {
		t.Fatalf("parse the envelope: %v", err)
	}

	dataKey, err := kp.Unwrap(context.Background(), wrapped, s.KeyID)
	if err != nil {
		t.Fatalf("unwrap the data key: %v", err)
	}

	return dataKey
}

func otherRole(r Role) Role {
	if r == Protocol {
		return Storage
	}

	return Protocol
}

// firstUndeclaredKind is the first kind with no name, so a kind declared
// later moves it rather than breaking the test.
func firstUndeclaredKind() entity.SecretKind {
	k := entity.SecretKind(1)
	for k.String() != "" {
		k++
	}

	return k
}

// faultOn fails every op named op on a key whose description starts with
// prefix.
type faultOn struct {
	op     string
	prefix string
}

func (f faultOn) At(op string, key []byte) (time.Duration, int, error) {
	if op == f.op && bytes.HasPrefix(key, []byte(f.prefix)) {
		return 0, 0, errSealInjected
	}

	return 0, 0, nil
}

func TestSealRoleFollowsKind(t *testing.T) {
	prof.Watch(t, prof.Options{})

	ctx := context.Background()
	p := sealProvider(t, sealControlPlane(t, sealStore(t, "")), sealKeys(t))
	owner := sealOwner()

	kinds := slices.Sorted(func(yield func(entity.SecretKind) bool) {
		for k := range kindRoles {
			if !yield(k) {
				return
			}
		}
	})
	for _, kind := range kinds {
		role := kindRoles[kind]
		ref := sealRef(t)

		s, err := p.Seal(ctx, role, kind, owner, ref, []byte("v"), Meta{})
		if err != nil {
			t.Errorf("%s under %s: %v, want sealed", kind, role, err)
		} else if s.Version != 1 || s.Kind != kind || s.ID != ref || !s.Owner.Equal(owner) {
			t.Errorf(
				"%s under %s: version %d, kind %s, id %x, owner %+v; want 1, %s, %x, %+v",
				kind, role, s.Version, s.Kind, s.ID, s.Owner, kind, ref, owner,
			)
		}

		other := otherRole(role)
		if _, err := p.Seal(ctx, other, kind, owner, sealRef(t), []byte("v"), Meta{}); !errors.Is(
			err,
			ErrWrongRole,
		) {
			t.Errorf("%s under %s: got %v, want ErrWrongRole", kind, other, err)
		}
	}

	refused := []struct {
		role Role
		kind entity.SecretKind
	}{
		{Protocol, 0},
		{Storage, 0},
		{0, 0},
		{Protocol, firstUndeclaredKind()},
		{Storage, firstUndeclaredKind()},
		{0, entity.SecretNTHash},
		{3, entity.SecretNTHash},
		{3, entity.SecretPassword},
	}

	for _, c := range refused {
		_, err := p.Seal(ctx, c.role, c.kind, owner, sealRef(t), []byte("v"), Meta{})
		if !errors.Is(err, ErrWrongRole) {
			t.Errorf("kind %d under role %d: got %v, want ErrWrongRole", c.kind, c.role, err)
		}
	}
}

func TestSealOpensOnlyUnderItsRoleKey(t *testing.T) {
	prof.Watch(t, prof.Options{})

	ctx := context.Background()
	keys := sealKeys(t)
	p := sealProvider(t, sealControlPlane(t, sealStore(t, "")), keys)
	owner := sealOwner()
	m := Meta{
		KeyVersion: 7,
		HandedOver: true,
	}

	for _, kind := range []entity.SecretKind{
		entity.SecretMachinePassword,
		entity.SecretRemoteCredential,
	} {
		role := kindRoles[kind]
		other := otherRole(role)
		ref := sealRef(t)
		value := []byte("the opened value of " + kind.String())

		s, err := p.Seal(ctx, role, kind, owner, ref, value, m)
		if err != nil {
			t.Fatalf("%s: %v", kind, err)
		}

		if s.KeyID != keys[role].CurrentMasterKeyID() {
			t.Errorf("%s: key ID %q, want the %s key's %q",
				kind, s.KeyID, role, keys[role].CurrentMasterKeyID())
		}

		if s.KeyID == keys[other].CurrentMasterKeyID() {
			t.Errorf("%s: key ID %q is the %s key's", kind, s.KeyID, other)
		}

		if s.KeyVersion != 7 || !s.HandedOver {
			t.Errorf("%s: KeyVersion %d, HandedOver %t; want 7, true",
				kind, s.KeyVersion, s.HandedOver)
		}

		dataKey := sealDataKey(t, keys[role], s)
		if bytes.Contains(s.Value, value) || bytes.Contains(s.Value, dataKey) {
			t.Errorf("%s: the stored value holds the value or the data key in clear", kind)
		}

		b := binding{
			role:       role,
			kind:       kind,
			owner:      owner,
			ref:        ref,
			version:    1,
			handedOver: true,
		}

		got, err := sealOpen(ctx, keys[role], s, b)
		if err != nil || !bytes.Equal(got, value) {
			t.Errorf("%s: opened %q, %v; want %q", kind, got, err, value)
		}

		flipped := b
		flipped.handedOver = false

		if _, err := sealOpen(ctx, keys[role], s, flipped); !errors.Is(err, errOpen) {
			t.Errorf("%s: opened with HandedOver false: got %v, want errOpen", kind, err)
		}

		wrapped, _, _ := parseEnvelope(s.Value)
		if _, err := keys[other].Unwrap(ctx, wrapped, s.KeyID); !errors.Is(
			err,
			keyprovider.ErrWrongMasterKey,
		) {
			t.Errorf("%s: unwrapped by the %s key: got %v, want ErrWrongMasterKey",
				kind, other, err)
		}
	}
}

func TestSealFreshDataKeyPerSeal(t *testing.T) {
	prof.Watch(t, prof.Options{})

	ctx := context.Background()
	keys := sealKeys(t)
	p := sealProvider(t, sealControlPlane(t, sealStore(t, "")), keys)
	ref := sealRef(t)

	var sealed [2]entity.Secret

	for i := range sealed {
		s, err := p.Seal(ctx, Protocol, entity.SecretNTHash, sealOwner(), ref, []byte("v"), Meta{})
		if err != nil {
			t.Fatalf("seal %d: %v", i, err)
		}

		sealed[i] = s
	}

	first, second := sealDataKey(t, keys[Protocol], sealed[0]),
		sealDataKey(t, keys[Protocol], sealed[1])
	if bytes.Equal(first, second) {
		t.Errorf("two seals share the data key %x", first)
	}

	if bytes.Equal(sealed[0].Value, sealed[1].Value) {
		t.Errorf("two seals gave the same envelope")
	}
}

func TestSealRefusesKindOrOwnerChange(t *testing.T) {
	prof.Watch(t, prof.Options{})

	ctx := context.Background()
	cp := sealControlPlane(t, sealStore(t, ""))
	p := sealProvider(t, cp, sealKeys(t))
	ref, owner := sealRef(t), sealOwner()

	first, err := p.Seal(ctx, Protocol, entity.SecretNTHash, owner, ref, []byte("v1"), Meta{})
	if err != nil {
		t.Fatalf("first version: %v", err)
	}

	if err := sealCommit(ctx, cp, first); err != nil {
		t.Fatalf("commit the first version: %v", err)
	}

	_, err = p.Seal(ctx, Protocol, entity.SecretKeytab, owner, ref, []byte("v2"), Meta{})
	if !errors.Is(err, ErrKindChanged) {
		t.Errorf("a keytab over an NT hash: got %v, want ErrKindChanged", err)
	}

	owners := []struct {
		name   string
		change func(*entity.SecretOwner)
	}{
		{"record kind", func(o *entity.SecretOwner) {
			o.Record.Kind = entity.ResourceGroup
		}},
		{"record ID", func(o *entity.SecretOwner) {
			o.Record.ID = []byte{0x0a, 0x0b, 0x0d}
		}},
		{"scope kind", func(o *entity.SecretOwner) {
			o.Scope.Kind = entity.ScopeShare
		}},
		{"scope ID", func(o *entity.SecretOwner) {
			o.Scope.ID[15] = 0x5c
		}},
		{"key", func(o *entity.SecretOwner) {
			o.Key = "password"
		}},
	}

	for _, c := range owners {
		o := sealOwner()
		c.change(&o)

		_, err := p.Seal(ctx, Protocol, entity.SecretNTHash, o, ref, []byte("v2"), Meta{})
		if !errors.Is(err, ErrKindChanged) {
			t.Errorf("another %s: got %v, want ErrKindChanged", c.name, err)
		}
	}

	s, err := p.Seal(ctx, Protocol, entity.SecretNTHash, owner, ref, []byte("v2"), Meta{})
	if err != nil || s.Version != 2 {
		t.Errorf("the same kind and owner: version %d, %v; want 2, nil", s.Version, err)
	}
}

func TestSealRefusesUnprovenCredential(t *testing.T) {
	prof.Watch(t, prof.Options{})

	ctx := context.Background()
	cp := sealControlPlane(t, sealStore(t, ""))
	p := sealProvider(t, cp, sealKeys(t))
	owner := sealOwner()

	cred := sealRef(t)

	first, err := p.Seal(
		ctx,
		Storage,
		entity.SecretRemoteCredential,
		owner,
		cred,
		[]byte("c1"),
		Meta{},
	)
	if err != nil || first.Version != 1 {
		t.Fatalf("a credential's first version: version %d, %v; want 1, nil", first.Version, err)
	}

	if err := sealCommit(ctx, cp, first); err != nil {
		t.Fatalf("commit the credential: %v", err)
	}

	_, err = p.Seal(ctx, Storage, entity.SecretRemoteCredential, owner, cred, []byte("c2"), Meta{})
	if !errors.Is(err, ErrUnproven) {
		t.Errorf("a credential's second version: got %v, want ErrUnproven", err)
	}

	_, err = p.Seal(ctx, Storage, entity.SecretKey, owner, cred, []byte("k"), Meta{})
	if !errors.Is(err, ErrKindChanged) {
		t.Errorf("a key over a credential: got %v, want ErrKindChanged", err)
	}

	key := sealRef(t)

	stored, err := p.Seal(ctx, Storage, entity.SecretKey, owner, key, []byte("k1"), Meta{})
	if err != nil {
		t.Fatalf("a key's first version: %v", err)
	}

	if err := sealCommit(ctx, cp, stored); err != nil {
		t.Fatalf("commit the key: %v", err)
	}

	_, err = p.Seal(ctx, Storage, entity.SecretRemoteCredential, owner, key, []byte("c"), Meta{})
	if !errors.Is(err, ErrKindChanged) {
		t.Errorf("a credential over a key: got %v, want ErrKindChanged", err)
	}

	s, err := p.Seal(ctx, Storage, entity.SecretKey, owner, key, []byte("k2"), Meta{})
	if err != nil || s.Version != 2 {
		t.Errorf("a key's second version: version %d, %v; want 2, nil", s.Version, err)
	}
}

func TestSealWritesNothing(t *testing.T) {
	prof.Watch(t, prof.Options{})

	ctx := context.Background()
	db := kvtest.NewFaultKV(sealStore(t, t.TempDir()))
	cp := sealControlPlane(t, db)
	p := sealProvider(t, cp, sealKeys(t))
	ref := sealRef(t)

	s, err := p.Seal(ctx, Protocol, entity.SecretNTHash, sealOwner(), ref, []byte("v"), Meta{})
	if err != nil {
		t.Fatalf("seal: %v", err)
	}

	if _, err := cp.Secret(ctx, ref, 0); !merrs.IsNotFoundError(err) {
		t.Errorf("Secret after Seal: got %v, want not found", err)
	}

	db.SetFault(faultOn{
		op:     kvtest.OpSet,
		prefix: "AU‖",
	})

	if err := sealCommit(ctx, cp, s); !errors.Is(err, errSealInjected) {
		t.Errorf("the commit whose audit record fails: got %v, want the injected fault", err)
	}

	db.SetFault(nil)

	if _, err := cp.Secret(ctx, ref, 0); !merrs.IsNotFoundError(err) {
		t.Errorf("Secret after the failed commit: got %v, want not found", err)
	}
}

func TestSealConcurrentSealsOneCommits(t *testing.T) {
	prof.Watch(t, prof.Options{})

	ctx := context.Background()
	cp := sealControlPlane(t, sealStore(t, ""))
	p := sealProvider(t, cp, sealKeys(t))
	ref, owner := sealRef(t), sealOwner()

	first, err := p.Seal(ctx, Protocol, entity.SecretNTHash, owner, ref, []byte("v1"), Meta{})
	if err != nil {
		t.Fatalf("first version: %v", err)
	}

	if err := sealCommit(ctx, cp, first); err != nil {
		t.Fatalf("commit the first version: %v", err)
	}

	a, errA := p.Seal(ctx, Protocol, entity.SecretNTHash, owner, ref, []byte("a"), Meta{})
	b, errB := p.Seal(ctx, Protocol, entity.SecretNTHash, owner, ref, []byte("b"), Meta{})

	if errA != nil || errB != nil || a.Version != 2 || b.Version != 2 {
		t.Fatalf("two seals from version 1: versions %d, %d, errors %v, %v; want 2, 2",
			a.Version, b.Version, errA, errB)
	}

	if err := sealCommit(ctx, cp, a); err != nil {
		t.Fatalf("the first commit: %v", err)
	}

	if err := sealCommit(ctx, cp, b); !errors.Is(err, entity.ErrGenerationMismatch) {
		t.Errorf("the second commit: got %v, want ErrGenerationMismatch", err)
	}

	cur, err := cp.Secret(ctx, ref, 0)
	if err != nil || cur.Version != 2 || !bytes.Equal(cur.Value, a.Value) {
		t.Errorf("current: version %d, %v, the first seal's value %t; want 2, nil, true",
			cur.Version, err, bytes.Equal(cur.Value, a.Value))
	}
}

func TestSealNeedsTheRoleKey(t *testing.T) {
	prof.Watch(t, prof.Options{})

	ctx := context.Background()
	cp := sealControlPlane(t, sealStore(t, ""))
	keys := sealKeys(t)
	storageOnly := sealProvider(t, cp, map[Role]keyprovider.KeyProvider{
		Storage: keys[Storage],
	})
	ref := sealRef(t)

	s, err := storageOnly.Seal(
		ctx,
		Protocol,
		entity.SecretNTHash,
		sealOwner(),
		ref,
		[]byte("v"),
		Meta{},
	)
	if !errors.Is(err, errNoRoleKey) || errors.Is(err, ErrWrongRole) {
		t.Errorf(
			"an NT hash with no protocol key: got %v, want errNoRoleKey and not ErrWrongRole",
			err,
		)
	}

	if !reflect.DeepEqual(s, entity.Secret{}) {
		t.Errorf("an NT hash with no protocol key: got %+v, want the zero Secret", s)
	}

	if _, err := cp.Secret(ctx, ref, 0); !merrs.IsNotFoundError(err) {
		t.Errorf("Secret after the refusal: got %v, want not found", err)
	}

	if _, err := storageOnly.Seal(
		ctx, Storage, entity.SecretRemoteCredential, sealOwner(), sealRef(t), []byte("c"), Meta{},
	); err != nil {
		t.Errorf("a credential with the storage key: %v", err)
	}

	// The provider keeps its own map: a caller reusing its map does not take
	// a role's key away.
	given := map[Role]keyprovider.KeyProvider{
		Protocol: keys[Protocol],
		Storage:  keys[Storage],
	}

	p := sealProvider(t, cp, given)
	delete(given, Protocol)

	_, err = p.Seal(
		ctx,
		Protocol,
		entity.SecretNTHash,
		sealOwner(),
		sealRef(t),
		[]byte("v"),
		Meta{},
	)
	if err != nil {
		t.Errorf("an NT hash after the caller's map lost its protocol key: %v", err)
	}
}

func TestSealErrorsCarryNoValue(t *testing.T) {
	prof.Watch(t, prof.Options{})

	ctx := context.Background()
	db := kvtest.NewFaultKV(sealStore(t, ""))
	cp := sealControlPlane(t, db)
	keys := sealKeys(t)
	p := sealProvider(t, cp, keys)
	storageOnly := sealProvider(t, cp, map[Role]keyprovider.KeyProvider{
		Storage: keys[Storage],
	})
	owner := sealOwner()
	value := []byte(canary)

	nt := sealRef(t)

	s, err := p.Seal(ctx, Protocol, entity.SecretNTHash, owner, nt, []byte("nt"), Meta{})
	if err != nil {
		t.Fatalf("seal the NT hash: %v", err)
	}

	if err := sealCommit(ctx, cp, s); err != nil {
		t.Fatalf("commit the NT hash: %v", err)
	}

	cred := sealRef(t)

	s, err = p.Seal(ctx, Storage, entity.SecretRemoteCredential, owner, cred, []byte("c"), Meta{})
	if err != nil {
		t.Fatalf("seal the credential: %v", err)
	}

	if err := sealCommit(ctx, cp, s); err != nil {
		t.Fatalf("commit the credential: %v", err)
	}

	other := sealOwner()
	other.Key = "password"

	cases := []struct {
		name string
		seal func() (entity.Secret, error)
	}{
		{"wrong role", func() (entity.Secret, error) {
			return p.Seal(ctx, Storage, entity.SecretNTHash, owner, sealRef(t), value, Meta{})
		}},
		{"undeclared kind", func() (entity.Secret, error) {
			return p.Seal(ctx, Storage, firstUndeclaredKind(), owner, sealRef(t), value, Meta{})
		}},
		{"no key", func() (entity.Secret, error) {
			return storageOnly.Seal(
				ctx,
				Protocol,
				entity.SecretNTHash,
				owner,
				sealRef(t),
				value,
				Meta{},
			)
		}},
		{"kind changed", func() (entity.Secret, error) {
			return p.Seal(ctx, Protocol, entity.SecretKeytab, owner, nt, value, Meta{})
		}},
		{"owner changed", func() (entity.Secret, error) {
			return p.Seal(ctx, Protocol, entity.SecretNTHash, other, nt, value, Meta{})
		}},
		{"unproven", func() (entity.Secret, error) {
			return p.Seal(ctx, Storage, entity.SecretRemoteCredential, owner, cred, value, Meta{})
		}},
		{"store read failed", func() (entity.Secret, error) {
			db.SetFault(faultOn{
				op:     kvtest.OpBefore,
				prefix: "SEC‖",
			})

			defer db.SetFault(nil)

			return p.Seal(ctx, Protocol, entity.SecretNTHash, owner, nt, value, Meta{})
		}},
		{"wrap failed", func() (entity.Secret, error) {
			_ = keys[Protocol].Close()

			return p.Seal(ctx, Protocol, entity.SecretNTHash, owner, sealRef(t), value, Meta{})
		}},
	}

	forms := []string{
		canary,
		hex.EncodeToString(value),
		base64.StdEncoding.EncodeToString(value),
	}

	for _, c := range cases {
		s, err := c.seal()
		if err == nil {
			t.Errorf("%s: got nil, want a refusal", c.name)

			continue
		}

		if !reflect.DeepEqual(s, entity.Secret{}) {
			t.Errorf("%s: got %+v, want the zero Secret", c.name, s)
		}

		for _, text := range []string{err.Error(), fmt.Sprintf("%+v", err)} {
			for _, form := range forms {
				if strings.Contains(text, form) {
					t.Errorf("%s: the error %q carries the value", c.name, text)
				}
			}
		}
	}
}

// keptKey passes every call to the real provider and keeps the slice Seal
// handed to Wrap, so a test can look at it once Seal returns.
type keptKey struct {
	keyprovider.KeyProvider
	given []byte
}

func (k *keptKey) Wrap(ctx context.Context, dataKey []byte) ([]byte, string, error) {
	k.given = dataKey

	return k.KeyProvider.Wrap(ctx, dataKey)
}

func TestSealClearsDataKey(t *testing.T) {
	prof.Watch(t, prof.Options{})

	ctx := context.Background()
	cp := sealControlPlane(t, sealStore(t, ""))
	keys := sealKeys(t)
	protocol := &keptKey{KeyProvider: keys[Protocol]}
	storage := &keptKey{KeyProvider: keys[Storage]}
	p := sealProvider(t, cp, map[Role]keyprovider.KeyProvider{
		Protocol: protocol,
		Storage:  storage,
	})
	owner, ref := sealOwner(), sealRef(t)

	s, err := p.Seal(ctx, Protocol, entity.SecretNTHash, owner, ref, []byte("v"), Meta{})
	if err != nil {
		t.Fatalf("seal: %v", err)
	}

	zero := make([]byte, dataKeySize)
	if !bytes.Equal(protocol.given, zero) {
		t.Errorf("the data key after a seal: %x, want %d zero bytes", protocol.given, dataKeySize)
	}

	b := binding{
		role:    Protocol,
		kind:    entity.SecretNTHash,
		owner:   owner,
		ref:     ref,
		version: 1,
	}

	if got, err := sealOpen(ctx, keys[Protocol], s, b); err != nil || string(got) != "v" {
		t.Errorf("the record after the clear: opened %q, %v; want \"v\"", got, err)
	}

	_ = keys[Storage].Close()

	_, err = p.Seal(ctx, Storage, entity.SecretPassword, owner, sealRef(t), []byte("v"), Meta{})
	if err == nil {
		t.Fatalf("a seal under a closed key: got nil, want the wrap's error")
	}

	if !bytes.Equal(storage.given, zero) {
		t.Errorf(
			"the data key after a failed wrap: %x, want %d zero bytes",
			storage.given,
			dataKeySize,
		)
	}
}
