package kvstoretest

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/cubbit/dittofs/internal/prof"
	"github.com/cubbit/dittofs/pkg/keyprovider"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	merrs "github.com/cubbit/dittofs/pkg/metadata/errors"
	"github.com/cubbit/dittofs/pkg/secrets"
)

// providerKeys writes one local key file per role and opens it.
func providerKeys(t *testing.T) map[secrets.Role]keyprovider.KeyProvider {
	t.Helper()
	const passphrase = "conformance passphrase"
	t.Setenv("DITTOFS_ENCRYPTION_PASSPHRASE", passphrase)
	keys := map[secrets.Role]keyprovider.KeyProvider{}
	for _, role := range []secrets.Role{secrets.Protocol, secrets.Storage} {
		b, err := keyprovider.GenerateKeyFile(passphrase)
		must(t, err)
		path := filepath.Join(t.TempDir(), role.String()+".key")
		must(t, os.WriteFile(path, b, 0o600))
		kp, err := keyprovider.NewProvider(context.Background(), keyprovider.Config{Kind: keyprovider.KindLocal, File: path})
		must(t, err)
		t.Cleanup(func() { _ = kp.Close() })
		keys[role] = kp
	}
	return keys
}

// A user's password hash and NT hash, each sealed through the secret
// provider at two versions, leave no version of either once the user's
// deletion has run to its end.
func testDeletedUserLeavesNoSecret(t *testing.T, kvo Opener) {
	s, _ := newStore(t, kvo)
	cp := s.ControlPlane()
	p, err := secrets.New(cp, providerKeys(t))
	must(t, err)
	u := user("alice")
	u.Password, u.NTHash = newSecretRef(), newSecretRef()
	gen, err := cp.PutUser(ctx, u, 0, entity.Alloc{})
	must(t, err)
	refs := []struct {
		field string
		role  secrets.Role
		kind  entity.SecretKind
		ref   entity.SecretRef
	}{
		{entity.UserPasswordKey, secrets.Storage, entity.SecretPassword, u.Password},
		{entity.UserNTHashKey, secrets.Protocol, entity.SecretNTHash, u.NTHash},
	}
	for v := range uint64(2) {
		for _, r := range refs {
			owner := entity.UserSecretOwner(u.Principal.ID, r.field)
			sec, err := p.Seal(ctx, r.role, r.kind, owner, r.ref, fmt.Appendf(nil, "%s %d", r.field, v+1), secrets.Meta{})
			must(t, err)
			must(t, cp.PutSecret(ctx, sec, v))
		}
	}
	for _, r := range refs {
		if _, err := cp.Secret(ctx, r.ref, 1); err != nil {
			t.Fatalf("%s version 1 before the deletion: %v", r.field, err)
		}
	}

	must(t, deleted(s, cp.DeleteUser(ctx, u.Principal, gen), u.Principal))

	for _, r := range refs {
		for _, v := range []uint64{0, 1, 2} {
			if _, err := cp.Secret(ctx, r.ref, v); !merrs.IsNotFoundError(err) {
				t.Errorf("%s version %d after the deletion: error %v, want not found", r.field, v, err)
			}
		}
	}
}

// A user re-pointed from one sealed NT hash to another keeps the second
// while a batch drops only the first, and loses it only to the batch that
// drops it: zeroing by ownership alone would leave the second unreferenced.
func testRepointedNTHashSurvivesItsPredecessor(t *testing.T, kvo Opener) {
	s, _ := newStore(t, kvo)
	cp := s.ControlPlane()
	p, err := secrets.New(cp, providerKeys(t))
	must(t, err)
	// The first sorts before the second, so a one-secret batch drops it alone.
	first, second := entity.SecretRef{0: 1}, entity.SecretRef{0: 2}
	u := user("alice")
	u.NTHash = first
	gen, err := cp.PutUser(ctx, u, 0, entity.Alloc{})
	must(t, err)
	owner := entity.UserSecretOwner(u.Principal.ID, entity.UserNTHashKey)
	for _, ref := range []entity.SecretRef{first, second} {
		sec, err := p.Seal(ctx, secrets.Protocol, entity.SecretNTHash, owner, ref, ref[:], secrets.Meta{})
		must(t, err)
		must(t, cp.PutSecret(ctx, sec, 0))
	}
	u.NTHash = second
	gen, err = cp.PutUser(ctx, u, gen, entity.Alloc{})
	must(t, err)

	next, dropped, done, err := cp.DropNTHashes(ctx, entity.SecretRef{}, 1)
	must(t, err)
	if next != first || dropped != 1 || done {
		t.Fatalf("first batch: next %x, dropped %d, done %v; want %x, 1, false", next, dropped, done, first)
	}
	got, found, err := cp.User(ctx, u.Principal)
	must(t, err)
	if !found || got.NTHash != second || got.Generation != gen {
		t.Errorf("after the first batch: found %v, NTHash %x, generation %d; want true, %x, %d", found, got.NTHash, got.Generation, second, gen)
	}
	if _, err := cp.Secret(ctx, first, 1); !merrs.IsNotFoundError(err) {
		t.Errorf("first after the first batch: error %v, want not found", err)
	}
	if _, err := cp.Secret(ctx, second, 1); err != nil {
		t.Errorf("second after the first batch: %v", err)
	}

	for !done {
		next, _, done, err = cp.DropNTHashes(ctx, next, 1)
		must(t, err)
	}
	got, found, err = cp.User(ctx, u.Principal)
	must(t, err)
	if !found || got.NTHash != (entity.SecretRef{}) || got.Generation <= gen {
		t.Errorf("after the deletion: found %v, NTHash %x, generation %d; want true, zero, above %d", found, got.NTHash, got.Generation, gen)
	}
	if _, err := cp.Secret(ctx, second, 1); !merrs.IsNotFoundError(err) {
		t.Errorf("second after the deletion: error %v, want not found", err)
	}
}

// PutUser refuses a user whose password or NT hash reference names a held
// secret sealed for another holder or as another kind than the field's, or
// whose two references are one, on a new record and on a changed one, and
// accepts the user's own.
func testPutUserRefusesAForeignSecret(t *testing.T, kvo Opener) {
	prof.Watch(t, prof.Options{})

	s, _ := newStore(t, kvo)
	cp := s.ControlPlane()
	bob, bobPw := user("bob"), newSecretRef()
	must(t, cp.PutSecret(ctx, secretOf(bobPw, bob.Principal.ID, 1, "bob"), 0))

	// Each refused create is its own principal, so none is refused as a
	// generation conflict with another.
	others := user("carol")
	others.Password = bobPw
	wrongKind := user("dave")
	wrongKind.NTHash = newSecretRef()
	nt := secretOf(wrongKind.NTHash, wrongKind.Principal.ID, 1, "dave")
	nt.Owner = entity.UserSecretOwner(wrongKind.Principal.ID, entity.UserNTHashKey)
	must(t, cp.PutSecret(ctx, nt, 0))
	oneSecret := user("erin")
	oneSecret.Password = newSecretRef()
	oneSecret.NTHash = oneSecret.Password

	for _, c := range []struct {
		name string
		u    entity.User
	}{
		{"naming bob's password", others},
		{"naming an NT hash of the password kind", wrongKind},
		{"naming one secret as both", oneSecret},
	} {
		if _, err := cp.PutUser(ctx, c.u, 0, entity.Alloc{}); !secretInvalid(err) {
			t.Errorf("PutUser %s: got %v, want an invalid argument", c.name, err)
		}
	}

	alice := user("alice")
	alice.Password = newSecretRef()
	must(t, cp.PutSecret(ctx, secretOf(alice.Password, alice.Principal.ID, 1, "alice"), 0))
	gen, err := cp.PutUser(ctx, alice, 0, entity.Alloc{})
	if err != nil {
		t.Fatalf("PutUser naming alice's own password: %v", err)
	}
	alice.Password = bobPw
	if _, err := cp.PutUser(ctx, alice, gen, entity.Alloc{}); !secretInvalid(err) {
		t.Errorf("PutUser changed to name bob's password: got %v, want an invalid argument", err)
	}
}
