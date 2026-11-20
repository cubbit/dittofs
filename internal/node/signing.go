package node

import (
	"context"
	"fmt"

	"github.com/cubbit/dittofs/internal/node/api/auth"
	"github.com/cubbit/dittofs/pkg/keyprovider"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/secrets"
)

// openSecrets builds the secret provider over the storage key the node
// opened, and over it the access tokens of the installation's signing key.
func (n *Node) openSecrets(ctx context.Context) (*secrets.Provider, error) {
	in, err := n.meta.Installation(ctx)
	if err != nil {
		return nil, fmt.Errorf("node: the installation: %w", err)
	}
	p, err := secrets.New(n.meta.ControlPlane(), map[secrets.Role]keyprovider.KeyProvider{secrets.Storage: n.storageKey})
	if err != nil {
		return nil, fmt.Errorf("node: the secret provider: %w", err)
	}
	n.tokens = auth.NewTokens(providerKeys{p}, in.ID, in.SigningKey, n.cfg.Clock)
	return p, nil
}

// rotationRow is the step table's row of the signing key's rotation, which
// seals through p.
func (n *Node) rotationRow(p *secrets.Provider) stepRow {
	return stepRow{auth.RotationStep(n.meta.ControlPlane(), signingSealer{providerSealer{p}}, n.tokens, n.cfg.Bootstrap.Node.ID), auth.RotationProgress}
}

// providerKeys is the secret provider as the access tokens read the signing
// key: every version still held, opened under the storage role.
type providerKeys struct{ p *secrets.Provider }

// Versions opens every version of ref still held, current first.
func (k providerKeys) Versions(ctx context.Context, kind entity.SecretKind, owner entity.SecretOwner, ref entity.SecretRef) ([]auth.KeyVersion, error) {
	vs, err := k.p.Versions(ctx, secrets.Storage, kind, owner, ref)
	ks := make([]auth.KeyVersion, len(vs))
	for i, v := range vs {
		ks[i] = auth.KeyVersion{Version: v.Number, Value: v.Value}
	}
	return ks, err
}
