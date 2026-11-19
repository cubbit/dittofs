package secrets

import (
	"context"
	"fmt"

	"github.com/cubbit/dittofs/pkg/keyprovider"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	merrs "github.com/cubbit/dittofs/pkg/metadata/errors"
)

// Resolve returns the current value of ref, unsealed with role's wrapping
// key. It fails if this node does not hold that role's key, and if the
// record's kind or owner is not the caller's, so a reference cannot be
// pointed at another holder's secret.
func (p *Provider) Resolve(
	ctx context.Context,
	role Role,
	kind entity.SecretKind,
	owner entity.SecretOwner,
	ref entity.SecretRef,
) ([]byte, error) {
	key, err := p.roleKey(role, kind)
	if err != nil {
		return nil, err
	}

	// ponytail: no cache. Each call reads the record, one store round trip,
	// and unwraps its data key locally, since keyprovider holds its keys in
	// memory. Ceiling: that read on a per-sign-in path, once it passes a tenth
	// of a sign-in's time. Upgrade: a cache of opened values keyed by
	// reference, which reconcile replaces for each reference a secret write
	// moved.
	s, err := p.cp.Secret(ctx, ref, 0)
	if err != nil {
		return nil, fmt.Errorf("secrets: read current version: %w", err)
	}

	return openRecord(ctx, key, role, kind, owner, ref, s)
}

// Versions opens every version still held, current first, checked against
// the caller's kind and owner as Resolve checks the current one. It walks
// down from the current version to the first missing one. A version that
// does not open fails the whole call, so a holder never acts on a list that
// lacks a version it must keep. Run beside a prune or a delete, it can return
// a shorter list.
func (p *Provider) Versions(
	ctx context.Context,
	role Role,
	kind entity.SecretKind,
	owner entity.SecretOwner,
	ref entity.SecretRef,
) ([]Version, error) {
	key, err := p.roleKey(role, kind)
	if err != nil {
		return nil, err
	}

	s, err := p.cp.Secret(ctx, ref, 0)
	if err != nil {
		return nil, fmt.Errorf("secrets: read current version: %w", err)
	}

	var out []Version

	for {
		value, err := openRecord(ctx, key, role, kind, owner, ref, s)
		if err != nil {
			return nil, err
		}

		out = append(out, Version{
			Number: s.Version,
			Value:  value,
			Meta: Meta{
				KeyVersion: s.KeyVersion,
				HandedOver: s.HandedOver,
			},
		})

		if s.Version == 1 {
			return out, nil
		}

		below := s.Version - 1

		// A prune removes only the lowest versions, so the first missing one
		// ends those still held.
		s, err = p.cp.Secret(ctx, ref, below)
		if merrs.IsNotFoundError(err) {
			return out, nil
		}

		if err != nil {
			return nil, fmt.Errorf("secrets: read version %d: %w", below, err)
		}
	}
}

// roleKey is the wrapping key that opens kind under role: the role must be
// the one its kind is sealed under, and this node must hold its key.
func (p *Provider) roleKey(role Role, kind entity.SecretKind) (keyprovider.KeyProvider, error) {
	if r, ok := roleOf(kind); !ok || r != role {
		return nil, ErrWrongRole
	}

	key := p.keys[role]
	if key == nil {
		return nil, fmt.Errorf("%w: %s", errNoRoleKey, role)
	}

	return key, nil
}

// openRecord opens one stored version with the caller's role, kind, owner and
// reference in the additional data, so the open proves the caller's
// expectation rather than the record's own. A record of another kind or
// owner is refused before its data key is unwrapped.
func openRecord(
	ctx context.Context,
	key keyprovider.KeyProvider,
	role Role,
	kind entity.SecretKind,
	owner entity.SecretOwner,
	ref entity.SecretRef,
	s entity.Secret,
) ([]byte, error) {
	if s.Kind != kind || !owner.Equal(s.Owner) {
		return nil, ErrWrongKind
	}

	wrapped, sealed, err := parseEnvelope(s.Value)
	if err != nil {
		return nil, err
	}

	// A retired key's ID still resolves: keyprovider keeps every retired key
	// for opening.
	dataKey, err := key.Unwrap(ctx, wrapped, s.KeyID)
	if err != nil {
		return nil, fmt.Errorf("secrets: unwrap data key under the %s key: %w", role, err)
	}

	// Best effort: the AES key schedule openValue builds from dataKey stays
	// on the heap until it is collected, and cannot be cleared from here.
	defer clear(dataKey)

	b := binding{
		role:       role,
		kind:       kind,
		owner:      owner,
		ref:        ref,
		version:    s.Version,
		handedOver: s.HandedOver,
	}

	return openValue(dataKey, sealed, b.additionalData())
}
