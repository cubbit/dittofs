package chain

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/transform"
	"github.com/cubbit/dittofs/pkg/transform/keys"
)

// census is the keys.Provider each step's Decode receives (RFC 5 §2.5, §3.1):
// it answers Get only for material the block record's census lists, of the
// namespace being read, and refuses every other ID, and Current, with
// transform.ErrMalformed before the provider is asked. So a material ID a
// bucket writer plants in a body never reaches the key service.
type census struct {
	p      keys.Provider
	ns     entity.NamespaceID
	listed []transform.Encoded
}

var _ keys.Provider = (*census)(nil)

// Current is refused: a decode reads the material its body names, never the
// current one.
func (c *census) Current(_ context.Context, ns entity.NamespaceID, kind string) (keys.MaterialID, error) {
	return keys.MaterialID{}, fmt.Errorf("%w: a decode asked for the current %s of namespace %s", transform.ErrMalformed, kind, ns)
}

// Get matches id to a census entry on namespace, kind and ID, and asks the
// provider with that entry, so the fingerprint the provider checks comes from
// block metadata: a body names its material by ID alone. A provider error
// that is none of keys' three material errors is keys.ErrMaterialUnavailable,
// carrying the cause as text only: the ID passed the census, so the body is
// not at fault, and an error the provider wraps cannot make it so.
func (c *census) Get(ctx context.Context, id keys.MaterialID) ([]byte, error) {
	if id.Namespace == c.ns {
		for _, e := range c.listed {
			for _, l := range e.Material {
				if l.Namespace != c.ns || l.Kind != id.Kind || l.ID != id.ID {
					continue
				}
				b, err := c.p.Get(ctx, l)
				if err != nil && !errors.Is(err, keys.ErrUnknownMaterial) && !errors.Is(err, keys.ErrMaterialUnavailable) && !errors.Is(err, keys.ErrMaterialDestroyed) {
					return nil, fmt.Errorf("%w: reading %s %q of namespace %s: %s", keys.ErrMaterialUnavailable, l.Kind, l.ID, l.Namespace, err)
				}
				return b, err
			}
		}
	}
	return nil, fmt.Errorf("%w: %s %q of namespace %s is not in the block's census", transform.ErrMalformed, id.Kind, id.ID, id.Namespace)
}

// required refuses, with transform.ErrMalformed, a parsed envelope that lacks
// a transform the store configuration's require names, or, in a namespace
// that encrypts, any transform of the encrypt stage (RFC 5 §2.5, §3.2). It
// runs before any step decodes and only refuses: it never changes how a body
// decodes. Any encrypt-stage transform satisfies an encrypting namespace, so
// replacing one encryption transform by another keeps older bodies readable.
func (c *chain) required(ns entity.NamespaceID, p *parsed) error {
	listed := p.steps[:p.n]
	for _, id := range c.require {
		if !slices.ContainsFunc(listed, func(t transform.Transform) bool { return t.ID() == id }) {
			return fmt.Errorf("%w: the body lacks required transform %#04x", transform.ErrMalformed, id)
		}
	}
	if c.encrypts(ns) && !slices.ContainsFunc(listed, func(t transform.Transform) bool { return t.Stage() == transform.StageEncrypt }) {
		return fmt.Errorf("%w: a body of namespace %s, which encrypts, has no encrypt stage", transform.ErrMalformed, ns)
	}
	return nil
}
