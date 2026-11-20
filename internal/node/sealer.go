package node

import (
	"context"

	"github.com/cubbit/dittofs/internal/node/api"
	"github.com/cubbit/dittofs/internal/node/api/auth"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/secrets"
	"github.com/cubbit/dittofs/pkg/settings"
)

// providerSealer is the secret provider as the API seals with it: under the
// role a secret's kind is sealed under.
type providerSealer struct{ p *secrets.Provider }

// Seal seals value under the role of kind: protocol for authentication's
// secrets, storage for every other.
//
// decision: the role is read off the kind here as RFC 13 §7's table gives
// it, since the provider exports no lookup; a drift from the provider's own
// table fails closed, since its Seal refuses a role a kind is not sealed
// under. Overturned if the provider exports its table.
func (s providerSealer) Seal(ctx context.Context, kind entity.SecretKind, owner entity.SecretOwner, ref entity.SecretRef, value []byte) (entity.Secret, error) {
	role := secrets.Storage
	switch kind {
	case entity.SecretNTHash, entity.SecretKeytab, entity.SecretMachinePassword, entity.SecretDirectoryCredential:
		role = secrets.Protocol
	}
	return s.p.Seal(ctx, role, kind, owner, ref, value, secrets.Meta{})
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

// The secret provider, through providerSealer, and the settings table are
// what the API's Deps take, once the node builds them.
var _ = api.Deps{Sealer: providerSealer{}, Settings: settings.Table{}}
