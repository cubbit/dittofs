package node_test

import (
	"testing"

	"github.com/cubbit/dittofs/internal/node/nodetest"
	"github.com/cubbit/dittofs/pkg/identity"
	"github.com/cubbit/dittofs/pkg/keyprovider"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/secrets"
	"github.com/google/uuid"
)

// A user whose password was derived with NewHashes and sealed as the API
// stores it signs in through the identity the node hands the API, once the
// node has started: the API is handed the identity the node built, over the
// node's secret provider.
func TestAccountsVerifyASeededPassword(t *testing.T) {
	h := nodetest.New(t, nodetest.Watch(), nodetest.InMemory())
	ctx := t.Context()
	const password = "correct horse battery staple"
	u := entity.User{Principal: entity.NewUser(), Name: "alice", Password: entity.SecretRef(uuid.New())}
	hashes, err := identity.NewHashes(u, []byte(password), identity.NTLMSetting{})
	if err != nil {
		t.Fatalf("NewHashes: %v", err)
	}
	kp, err := keyprovider.NewProvider(ctx, keyprovider.Config{Kind: keyprovider.KindLocal, File: h.StorageKeyFile()})
	if err != nil {
		t.Fatalf("open the storage key: %v", err)
	}
	defer func() { _ = kp.Close() }()
	cp := h.Store().ControlPlane()
	p, err := secrets.New(cp, map[secrets.Role]keyprovider.KeyProvider{secrets.Storage: kp})
	if err != nil {
		t.Fatalf("secret provider: %v", err)
	}
	owner := entity.SecretOwner{Record: entity.ResourceRef{Kind: entity.ResourceUser, ID: u.Principal.ID[:]}, Key: "password"}
	sec, err := p.Seal(ctx, secrets.Storage, entity.SecretPassword, owner, u.Password, hashes.Password, secrets.Meta{})
	if err != nil {
		t.Fatalf("seal the password hash: %v", err)
	}
	err = cp.Write(ctx, entity.WriteMeta{}, func(tx entity.ControlPlaneTx) error {
		if _, err := tx.PutUser(u, 0, entity.Alloc{}); err != nil {
			return err
		}
		return tx.PutSecret(sec, 0)
	})
	if err != nil {
		t.Fatalf("write alice: %v", err)
	}

	n := h.Node()
	if err := n.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() {
		if err := n.Stop(ctx); err != nil {
			t.Errorf("Stop: %v", err)
		}
	}()
	got, err := n.Accounts().Lookup(ctx, "alice")
	if err != nil || got != u.Principal {
		t.Fatalf("Accounts().Lookup(alice): got %v, %v, want %v, nil", got, err, u.Principal)
	}
	if err := n.Accounts().VerifyPassword(ctx, u.Principal, []byte(password)); err != nil {
		t.Fatalf("Accounts().VerifyPassword with alice's password: got %v, want nil", err)
	}
}
