package secrets

import (
	"context"
	"errors"
	"fmt"

	"github.com/cubbit/dittofs/pkg/metadata/entity"
	merrs "github.com/cubbit/dittofs/pkg/metadata/errors"
)

// errNoRoleKey is a call naming a role whose wrapping key this node does not
// hold. RFC 13 §7 has Resolve fail this way and names no error; Seal refuses
// the same, and none of the exported errors means it, since the role is the
// right one for the kind.
var errNoRoleKey = errors.New("secrets: this node holds no key of the role")

// Seal wraps value with role's wrapping key as the next version of ref and
// returns it unwritten; a new version of an existing ref keeps its kind and
// owner, and one naming others is refused. The caller commits it with
// ControlPlaneTx.PutSecret(s, s.Version-1) inside its own Write, beside the
// record naming ref and its audit record, so a sealed version commits with
// its change or not at all. Wrapping runs before that Write, so a rerun of
// its function does not wrap again, and a concurrent seal since fails the
// commit's expect.
func (p *Provider) Seal(
	ctx context.Context,
	role Role,
	kind entity.SecretKind,
	owner entity.SecretOwner,
	ref entity.SecretRef,
	value []byte,
	m Meta,
) (entity.Secret, error) {
	// The role follows from the kind, never from the caller, so an NT hash
	// cannot be sealed under the storage key.
	if r, ok := roleOf(kind); !ok || r != role {
		return entity.Secret{}, ErrWrongRole
	}

	key := p.keys[role]
	if key == nil {
		return entity.Secret{}, fmt.Errorf("%w: %s", errNoRoleKey, role)
	}

	// The current version is read through the view, outside the caller's
	// Write, since wrapping runs before it (RFC 13 §7).
	cur, err := p.cp.Secret(ctx, ref, 0)
	if err != nil && !merrs.IsNotFoundError(err) {
		return entity.Secret{}, fmt.Errorf("secrets: read current version: %w", err)
	}

	if cur.Version > 0 && (kind != cur.Kind || !owner.Equal(cur.Owner)) {
		return entity.Secret{}, ErrKindChanged
	}

	// decision: the stored kind decides, never the caller's. A remote
	// store's credential changes only once the store's capability check finds
	// the namespace's claim at the location it reaches (RFC 13 §5.2); neither
	// is built, so a new version of one is refused and fails closed, while its
	// first version passes. The API seals each set at a fresh reference, so
	// it runs the check itself, and this gate covers a reseal of one
	// reference. Withdraw when both pieces are built.
	if cur.Version > 0 && cur.Kind == entity.SecretRemoteCredential {
		return entity.Secret{}, ErrUnproven
	}

	next := cur.Version + 1
	b := binding{
		role:       role,
		kind:       kind,
		owner:      owner,
		ref:        ref,
		version:    next,
		handedOver: m.HandedOver,
	}

	dataKey, sealed, err := sealValue(value, b.additionalData())
	if err != nil {
		return entity.Secret{}, err
	}

	// Best effort: the AES key schedule sealValue built from dataKey stays
	// on the heap until it is collected, and cannot be cleared from here.
	defer clear(dataKey)

	wrapped, keyID, err := key.Wrap(ctx, dataKey)
	if err != nil {
		return entity.Secret{}, fmt.Errorf("secrets: wrap data key under the %s key: %w", role, err)
	}

	return entity.Secret{
		ID:         ref,
		Kind:       kind,
		Version:    next,
		KeyID:      keyID,
		Owner:      owner,
		Value:      buildEnvelope(wrapped, sealed),
		KeyVersion: m.KeyVersion,
		HandedOver: m.HandedOver,
	}, nil
}
