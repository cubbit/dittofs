package kvstoretest

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

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
		{"password", secrets.Storage, entity.SecretPassword, u.Password},
		{"nt_hash", secrets.Protocol, entity.SecretNTHash, u.NTHash},
	}
	for v := range uint64(2) {
		for _, r := range refs {
			owner := entity.SecretOwner{Record: entity.ResourceRef{Kind: entity.ResourceUser, ID: bytes.Clone(u.Principal.ID[:])}, Key: r.field}
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
