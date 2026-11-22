package nodetest

import (
	"crypto/rand"
	"testing"

	"github.com/google/uuid"

	"github.com/cubbit/dittofs/pkg/identity"
	"github.com/cubbit/dittofs/pkg/keyprovider"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/node/apiv1"
	apiclient "github.com/cubbit/dittofs/pkg/node/apiv1/client"
	"github.com/cubbit/dittofs/pkg/secrets"
)

// Secrets is a secret provider over the harness's store and storage key, as
// another node's would be.
func (h *Harness) Secrets(t testing.TB) *secrets.Provider {
	t.Helper()
	kp, err := keyprovider.NewProvider(t.Context(), keyprovider.Config{Kind: keyprovider.KindLocal, File: h.StorageKeyFile()})
	if err != nil {
		t.Fatalf("nodetest: open the storage key: %v", err)
	}
	t.Cleanup(func() { _ = kp.Close() })
	p, err := secrets.New(h.store.ControlPlane(), map[secrets.Role]keyprovider.KeyProvider{secrets.Storage: kp})
	if err != nil {
		t.Fatalf("nodetest: %v", err)
	}
	return p
}

// Account stores an account named name holding role and password, its
// password sealed as the initialisation step seals the administrator's, and
// returns its principal. It holds no spelling, as an API-only account does.
func (h *Harness) Account(t testing.TB, name, password string, role entity.APIRole) entity.Principal {
	t.Helper()
	ctx := t.Context()
	u := entity.User{Principal: entity.NewUser(), Name: name, Role: role, Password: entity.SecretRef(uuid.New())}
	hs, err := identity.NewHashes(u, []byte(password), identity.NTLMSetting{})
	if err != nil {
		t.Fatalf("nodetest: hash %s's password: %v", name, err)
	}
	owner := entity.SecretOwner{Record: entity.ResourceRef{Kind: entity.ResourceUser, ID: u.Principal.ID[:]}, Key: "password"}
	sec, err := h.Secrets(t).Seal(ctx, secrets.Storage, entity.SecretPassword, owner, u.Password, hs.Password, secrets.Meta{})
	if err != nil {
		t.Fatalf("nodetest: seal %s's password: %v", name, err)
	}
	err = h.store.ControlPlane().Write(ctx, entity.WriteMeta{}, func(tx entity.ControlPlaneTx) error {
		if _, err := tx.PutUser(u, 0, entity.Alloc{}); err != nil {
			return err
		}
		return tx.PutSecret(sec, 0)
	})
	if err != nil {
		t.Fatalf("nodetest: store %s: %v", name, err)
	}
	return u.Principal
}

// SignedIn stores an account named name holding role, with a password of
// its own, and signs it in through the API the started node serves at addr:
// a caller of that role, its principal and its tokens.
func (h *Harness) SignedIn(t testing.TB, addr, name string, role entity.APIRole) (entity.Principal, apiv1.Tokens) {
	t.Helper()
	password := rand.Text()
	p := h.Account(t, name, password, role)
	c, err := apiclient.New("http://" + addr)
	if err != nil {
		t.Fatalf("nodetest: %v", err)
	}
	tk, err := c.SignIn(t.Context(), name, []byte(password))
	if err != nil {
		t.Fatalf("nodetest: sign %s in: %v", name, err)
	}
	return p, tk
}
