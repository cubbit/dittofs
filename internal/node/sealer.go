package node

import (
	"context"

	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/secrets"
)

// providerSealer is the secret provider as the API seals with it: under the
// role a secret's kind is sealed under.
type providerSealer struct{ p *secrets.Provider }

// roleOfKind is the role a secret's kind is sealed under: protocol for
// authentication's secrets, storage for every other.
//
// decision: the role is read off the kind here as RFC 13 §7's table gives
// it, since the provider exports no lookup; a drift from the provider's own
// table fails closed, since its Seal and its reads refuse a role a kind is
// not sealed under. Overturned if the provider exports its table.
func roleOfKind(kind entity.SecretKind) secrets.Role {
	switch kind {
	case entity.SecretNTHash,
		entity.SecretKeytab,
		entity.SecretMachinePassword,
		entity.SecretDirectoryCredential:
		return secrets.Protocol
	}

	return secrets.Storage
}

// Seal seals value under the role of kind, handedOver marking an offline
// join's machine password.
func (s providerSealer) Seal(
	ctx context.Context,
	kind entity.SecretKind,
	owner entity.SecretOwner,
	ref entity.SecretRef,
	value []byte,
	handedOver bool,
) (entity.Secret, error) {
	return s.p.Seal(
		ctx,
		roleOfKind(kind),
		kind,
		owner,
		ref,
		value,
		secrets.Meta{HandedOver: handedOver},
	)
}

// authSealer is providerSealer as auth seals with it, the signing key's
// rotation and a password's hashes: neither is ever handed over.
type authSealer struct{ providerSealer }

// newAuthSealer is p as auth seals with it.
func newAuthSealer(p *secrets.Provider) authSealer { return authSealer{providerSealer{p}} }

// Seal seals value as providerSealer does, not handed over.
func (s authSealer) Seal(ctx context.Context, kind entity.SecretKind, owner entity.SecretOwner, ref entity.SecretRef, value []byte) (entity.Secret, error) {
	return s.providerSealer.Seal(ctx, kind, owner, ref, value, false)
}
