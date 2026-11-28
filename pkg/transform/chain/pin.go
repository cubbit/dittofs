package chain

import (
	"context"

	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/transform"
	"github.com/cubbit/dittofs/pkg/transform/keys"
)

// Pin fixes the chain and namespace ns's current material for one block (RFC 5
// §3.1). It reads Current once for each kind the chain's transforms declare,
// so two transforms declaring one kind see one material, and computes the
// chain ID then: the attempt's ID and bounds come from what Pin read, however
// the current material changes meanwhile. A provider error fails Pin as the
// provider returned it, with no attempt.
func (c *chain) Pin(ctx context.Context, ns entity.NamespaceID) (transform.Attempt, error) {
	current := make(map[string]keys.MaterialID)
	material := make([][]keys.MaterialID, len(c.steps))
	for i, kinds := range c.kinds {
		material[i] = make([]keys.MaterialID, len(kinds))
		for j, kind := range kinds {
			m, ok := current[kind]
			if !ok {
				var err error
				if m, err = c.keys.Current(ctx, ns, kind); err != nil {
					return nil, err
				}
				current[kind] = m
			}
			material[i][j] = m
		}
	}
	return &attempt{id: chainID(c.steps, material), c: c, ns: ns, material: material}, nil
}

// attempt is one pinned chain: its chain, the namespace it was pinned for,
// each configured step's pinned material in the order of its Kinds, and the
// chain ID they give.
type attempt struct {
	id       [32]byte
	c        *chain
	ns       entity.NamespaceID
	material [][]keys.MaterialID
}

var _ transform.Attempt = (*attempt)(nil)

// ID is the chain ID Pin computed.
func (a *attempt) ID() [32]byte { return a.id }

// MaxEncodedLen composes the steps' bounds of n bytes under their pinned
// material, in stage order, each no less than its input since a step that
// declines passes it on, plus an envelope listing every step (RFC 5 §2.4).
func (a *attempt) MaxEncodedLen(n int) int {
	for i, t := range a.c.steps {
		n = max(n, t.MaxEncodedLen(n, a.material[i]))
	}
	return n + 2 + 2*len(a.c.steps)
}

// LengthOf composes the steps' LengthOf of n bytes under their pinned
// material, false if any one is false. A length known in advance is never a
// declined chunk's, so the envelope lists every step.
func (a *attempt) LengthOf(n int) (int, bool) {
	for i, t := range a.c.steps {
		var ok bool
		if n, ok = t.LengthOf(n, a.material[i]); !ok {
			return 0, false
		}
	}
	return n + 2 + 2*len(a.c.steps), true
}

// Release does nothing.
//
// decision: a pin counts nothing yet. The count of open pins is the fence
// Provider.Remove waits on before it destroys material, so Release feeds that
// count when Remove lands, and does nothing until then. Withdraw this when
// anything waits on open pins.
func (a *attempt) Release() {}
