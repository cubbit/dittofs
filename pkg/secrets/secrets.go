// Package secrets is the built-in secret provider of RFC 13 §7: sealed
// Secret records in the control-plane store, each sealed under the wrapping
// key of its kind's role. A value is never listed and never returned through
// any path but Resolve and Versions.
package secrets

import (
	"context"
	"errors"
	"fmt"

	"github.com/cubbit/dittofs/pkg/keyprovider"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/metadata/kvstore"
)

// Provider resolves secret references to their values, under the two roles'
// wrapping keys. It holds nothing until its methods are built, which add the
// view and the keys New takes.
type Provider struct{}

// unbuilt is the error of a method whose body is not built yet; the message
// names the work that fills it.
func unbuilt(method string) error {
	return fmt.Errorf(
		"secrets: %s is filled by configuration epic 4: %w",
		method,
		errors.ErrUnsupported,
	)
}

// New builds a provider over the control-plane view and each role's
// wrapping key.
func New(cp kvstore.ControlPlane, keys map[Role]keyprovider.KeyProvider) (*Provider, error) {
	return nil, unbuilt("New")
}

// Seal wraps value with role's wrapping key as the next version of ref and
// returns it unwritten; a new version of an existing ref keeps its kind and
// owner, and one naming others is refused. The caller commits it with
// ControlPlaneTx.PutSecret(s, s.Version-1) inside its own Write, beside the
// record naming ref, so a sealed version commits with its change or not at
// all.
func (p *Provider) Seal(
	ctx context.Context,
	role Role,
	kind entity.SecretKind,
	owner entity.SecretOwner,
	ref entity.SecretRef,
	value []byte,
	m Meta,
) (entity.Secret, error) {
	return entity.Secret{}, unbuilt("Seal")
}

// Resolve returns the current value of ref, unsealed with role's wrapping
// key. It fails if the record's kind or owner is not the caller's, so a
// reference cannot be pointed at another holder's secret.
func (p *Provider) Resolve(
	ctx context.Context,
	role Role,
	kind entity.SecretKind,
	owner entity.SecretOwner,
	ref entity.SecretRef,
) ([]byte, error) {
	return nil, unbuilt("Resolve")
}

// Versions opens every version still held, current first, checked against
// the caller's kind and owner as Resolve checks the current one.
func (p *Provider) Versions(
	ctx context.Context,
	role Role,
	kind entity.SecretKind,
	owner entity.SecretOwner,
	ref entity.SecretRef,
) ([]Version, error) {
	return nil, unbuilt("Versions")
}

// Prune drops the versions below the lowest one any node reports holding,
// never the current one, after checking the caller's kind and owner.
func (p *Provider) Prune(
	ctx context.Context,
	kind entity.SecretKind,
	owner entity.SecretOwner,
	ref entity.SecretRef,
) error {
	return unbuilt("Prune")
}

// Delete destroys every version of the secret; a later Resolve of ref
// fails. Provisional: it gains the caller's transaction when it is filled,
// so that it can refuse a credential a store record still names.
func (p *Provider) Delete(
	ctx context.Context,
	kind entity.SecretKind,
	owner entity.SecretOwner,
	ref entity.SecretRef,
) error {
	return unbuilt("Delete")
}
