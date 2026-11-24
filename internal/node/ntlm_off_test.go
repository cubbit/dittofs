package node_test

import (
	"context"
	"net/http"
	"testing"

	"github.com/google/uuid"

	"github.com/cubbit/dittofs/internal/node"
	"github.com/cubbit/dittofs/internal/node/nodetest"
	"github.com/cubbit/dittofs/pkg/keyprovider"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	merrs "github.com/cubbit/dittofs/pkg/metadata/errors"
	"github.com/cubbit/dittofs/pkg/node/apiv1"
	"github.com/cubbit/dittofs/pkg/secrets"
)

// Turning identity.ntlm off through the API starts the deletion of the
// stored NT hashes, and the node's own runner carries it to completion:
// afterwards no user's NT hash reads back, and their password hashes are
// untouched. Catches the deletion's kind missing from the node's step table,
// which leaves it pending and NTLM refused for good.
func TestNTLMOffDeletesTheSealedNTHashesOnTheNodesRunner(t *testing.T) {
	// An idempotent write is keyed by the signing key, so the node holds one.
	signing := func(context.Context) ([]byte, error) { return []byte("installation"), nil }
	h := nodetest.New(t, nodetest.Watch(), nodetest.InMemory(), nodetest.WithNew(node.NewSigning(admin, signing)))
	ctx := t.Context()
	kp, err := keyprovider.NewProvider(ctx, keyprovider.Config{Kind: keyprovider.KindLocal, File: h.StorageKeyFile()})
	if err != nil {
		t.Fatalf("open the storage key: %v", err)
	}
	defer func() { _ = kp.Close() }()
	cp := h.Store().ControlPlane()
	p, err := secrets.New(cp, map[secrets.Role]keyprovider.KeyProvider{secrets.Storage: kp, secrets.Protocol: kp})
	if err != nil {
		t.Fatalf("secret provider: %v", err)
	}
	var nt, pw []entity.SecretRef
	for _, name := range []string{"alice", "bob", "carol"} {
		u := entity.User{Principal: entity.NewUser(), Name: name,
			Password: entity.SecretRef(uuid.New()), NTHash: entity.SecretRef(uuid.New())}
		seal := func(role secrets.Role, kind entity.SecretKind, field string, ref entity.SecretRef) entity.Secret {
			s, err := p.Seal(ctx, role, kind, entity.UserSecretOwner(u.Principal.ID, field), ref, []byte("0123456789abcdef"), secrets.Meta{})
			if err != nil {
				t.Fatalf("seal %s's %s: %v", name, field, err)
			}
			return s
		}
		hash := seal(secrets.Protocol, entity.SecretNTHash, "nt_hash", u.NTHash)
		pass := seal(secrets.Storage, entity.SecretPassword, "password", u.Password)
		if err := cp.Write(ctx, entity.WriteMeta{}, func(tx entity.ControlPlaneTx) error {
			if _, err := tx.PutUser(u, 0, entity.Alloc{}); err != nil {
				return err
			}
			if err := tx.PutSecret(pass, 0); err != nil {
				return err
			}
			return tx.PutSecret(hash, 0)
		}); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
		nt, pw = append(nt, u.NTHash), append(pw, u.Password)
	}
	started(t, h)
	n := h.Node()
	addr, _ := n.Addrs()

	r := answeredIn(t, "PUT identity.ntlm off", send(ctx, addr, http.MethodPut, "/api/v1/settings/identity.ntlm", `{"spec":{"value":false}}`, "If-None-Match", "*", "Idempotency-Key", "ntlm-off"))
	if r.status < 200 || r.status > 299 {
		t.Fatalf("PUT identity.ntlm off: answered %d %s, want it accepted", r.status, r.body)
	}
	var del entity.Operation
	for op, err := range cp.Operations(ctx, entity.OperationID{}) {
		if err != nil {
			t.Fatalf("list operations: %v", err)
		}
		if op.Kind == string(apiv1.OpNTHashDelete) {
			del = op
		}
	}
	if del.Kind == "" {
		t.Fatalf("no %s operation after turning identity.ntlm off", apiv1.OpNTHashDelete)
	}

	// A turn runs synchronously, and one batch holds every hash here, so a
	// few turns are a generous bound.
	for range 5 {
		if operation(t, h, del.ID).State == entity.OpSucceeded {
			break
		}
		n.RunnerTurn(ctx)
	}
	if got := operation(t, h, del.ID); got.State != entity.OpSucceeded {
		t.Fatalf("the NT hashes' deletion on the node's runner: state %v (%s), want succeeded", got.State, got.Error)
	}
	for i, ref := range nt {
		if s, err := cp.Secret(ctx, ref, 0); !merrs.IsNotFoundError(err) {
			t.Errorf("user %d's NT hash after the deletion: got a %v secret, %v, want not found", i, s.Kind, err)
		}
		if _, err := cp.Secret(ctx, pw[i], 0); err != nil {
			t.Errorf("user %d's password hash after the deletion: %v, want it kept", i, err)
		}
	}
}
