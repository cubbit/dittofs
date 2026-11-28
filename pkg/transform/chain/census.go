package chain

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/transform"
	"github.com/cubbit/dittofs/pkg/transform/keys"
)

// census is the keys.Provider each step's Decode receives (RFC 5 §2.5, §3.1):
// it answers Get only for material the block record's census lists, of the
// namespace being read, and refuses every other ID, and Current, with
// transform.ErrMalformed before the provider is asked. So a material ID a
// bucket writer plants in a body never reaches the key service. It keeps the
// first error it gave during a step, which Decode reports over whatever the
// step returned, so a transform that rewrites the error cannot turn a planted
// ID into an outage or a destroyed key into corruption (RFC 5 §2.7).
//
// decision: an ID is matched against every census entry, not only the one of
// the transform asking, as RFC 5 §2.5 states the check and as transformtest's
// view does; one step can so read material the census records for another of
// the same kind. Bind each step to its own entries, by Encoded.ID, if two
// transforms of one chain ever share a kind.
type census struct {
	p      keys.Provider
	ns     entity.NamespaceID
	listed []transform.Encoded

	mu      sync.Mutex
	verdict error
}

var _ keys.Provider = (*census)(nil)

// Current is refused as ErrMalformed, the error a decode's material request
// outside the census takes: a decode reads the material its body names,
// never the current one.
func (c *census) Current(_ context.Context, ns entity.NamespaceID, kind string) (keys.MaterialID, error) {
	return keys.MaterialID{}, c.note(fmt.Errorf("%w: a decode asked for the current %s of namespace %s", transform.ErrMalformed, kind, ns))
}

// Get matches id to a census entry on namespace, kind and ID, and asks the
// provider with that entry, so the fingerprint the provider checks comes from
// block metadata: a body names its material by ID alone. A provider error
// that is none of keys' three material errors is keys.ErrMaterialUnavailable,
// carrying the cause as text only: the ID passed the census, so the body is
// not at fault, and an error the provider wraps cannot make it so. decodeErr
// maps a step's errors the same way.
func (c *census) Get(ctx context.Context, id keys.MaterialID) ([]byte, error) {
	if id.Namespace == c.ns {
		for _, e := range c.listed {
			for _, l := range e.Material {
				if l.Namespace != c.ns || l.Kind != id.Kind || l.ID != id.ID {
					continue
				}
				b, err := c.p.Get(ctx, l)
				if err != nil && !errors.Is(err, keys.ErrUnknownMaterial) && !errors.Is(err, keys.ErrMaterialUnavailable) && !errors.Is(err, keys.ErrMaterialDestroyed) {
					err = fmt.Errorf("%w: reading %s %q of namespace %s: %s", keys.ErrMaterialUnavailable, l.Kind, l.ID, l.Namespace, err)
				}
				if err != nil {
					return nil, c.note(err)
				}
				return b, nil
			}
		}
	}
	return nil, c.note(fmt.Errorf("%w: %s %q of namespace %s is not in the block's census", transform.ErrMalformed, id.Kind, id.ID, id.Namespace))
}

// note keeps err as the step's verdict unless one is kept, and returns it.
func (c *census) note(err error) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.verdict == nil {
		c.verdict = err
	}
	return err
}

// take returns the step's verdict, if any, and clears it for the next step.
func (c *census) take() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	v := c.verdict
	c.verdict = nil
	return v
}
