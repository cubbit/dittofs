// Package secrets is the built-in secret provider of RFC 13 §7: sealed
// Secret records in the control-plane store, each sealed under the wrapping
// key of its kind's role. A value is never listed and never returned through
// any path but Resolve and Versions.
package secrets

import (
	"maps"

	"github.com/cubbit/dittofs/pkg/keyprovider"
	"github.com/cubbit/dittofs/pkg/metadata/kvstore"
)

// Provider resolves secret references to their values, under the two roles'
// wrapping keys. It reads through the control-plane view and writes nothing:
// a sealed version is committed by its caller.
//
// ponytail: no reseal pass. A role key rotates by making the next key current
// and keeping the old one among keyprovider's retired keys, so every sealed
// version still opens, and an old role key can never leave the bootstrap.
// Upgrade: RFC 13 §7's reseal pass, when an operator must remove a role key.
type Provider struct {
	cp   kvstore.ControlPlane
	keys map[Role]keyprovider.KeyProvider
}

// New builds a provider over the control-plane view and each role's
// wrapping key. It keeps its own copy of keys, so a caller reusing its map
// cannot take a role's key away. A role missing from keys is refused when a
// call needs it, not here.
//
// ponytail: one passphrase variable for every key file. keyprovider's local
// provider reads DITTOFS_ENCRYPTION_PASSPHRASE, so two roles kept in
// passphrase-protected files share one passphrase. Upgrade: a per-role
// passphrase variable in the provider's configuration.
func New(cp kvstore.ControlPlane, keys map[Role]keyprovider.KeyProvider) (*Provider, error) {
	return &Provider{
		cp:   cp,
		keys: maps.Clone(keys),
	}, nil
}
