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

// chain is a built transform.Chain: the configured transforms in stage order,
// and the store's decode set.
type chain struct {
	decode *decodeSet
	steps  []transform.Transform // the configured transforms, in stage order
}

var _ transform.Chain = (*chain)(nil)

// newChain is NewChain over registry r. Every refusal after the deps check is
// logged once, at Error, with the reason and the transform at fault.
func (r *registry) newChain(ctx context.Context, cfg []transform.Config, m keys.Provider, deps Deps) (transform.Chain, error) {
	for _, d := range []struct {
		name    string
		missing bool
	}{
		{"Deps.Logger", deps.Logger == nil},
		{"Deps.Recorder", deps.Recorder == nil},
		{"Deps.Encrypts", deps.Encrypts == nil},
		{"the keys provider", m == nil},
	} {
		if d.missing {
			return nil, fmt.Errorf("chain: %s is missing", d.name)
		}
	}
	c, name, err := r.assemble(ctx, cfg, m, deps.Require)
	if err != nil {
		deps.Logger.ErrorContext(ctx, "chain: the transform chain cannot be built", "transform", name, "reason", err)
		return nil, err
	}
	return c, nil
}

// resolve looks up the IDs and factories names registered under, in one hold
// of the lock, or returns the first name not registered.
func (r *registry) resolve(names []string) ([]transform.ID, []transform.Factory, string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	ids, fs := make([]transform.ID, len(names)), make([]transform.Factory, len(names))
	for i, n := range names {
		id, ok := r.byName[n]
		if !ok {
			return nil, nil, n
		}
		ids[i], fs[i] = id, r.byID[id].factory
	}
	return ids, fs, ""
}

// assemble builds the chain, or returns the reason it cannot and the name of
// the transform at fault. Names are resolved before the decode set is built,
// and a registry only grows, so every resolved ID is in the set. The list's
// shape is judged from the decode set's transforms, so no configured factory
// runs for a list that is refused.
func (r *registry) assemble(ctx context.Context, cfg []transform.Config, m keys.Provider, require []string) (*chain, string, error) {
	names := make([]string, len(cfg))
	for i, c := range cfg {
		names[i] = c.Name
	}
	ids, fs, unknown := r.resolve(names)
	if unknown != "" {
		return nil, unknown, fmt.Errorf("chain: transform %q is not registered", unknown)
	}
	if _, _, unknown := r.resolve(require); unknown != "" {
		return nil, unknown, fmt.Errorf("chain: required transform %q is not registered", unknown)
	}
	set, name, err := r.build(ctx, m)
	if err != nil {
		return nil, name, err
	}
	for i, c := range cfg {
		if slices.Contains(ids[:i], ids[i]) {
			return nil, c.Name, fmt.Errorf("chain: transform %q is listed twice", c.Name)
		}
		if i == 0 {
			continue
		}
		prev, prevStage, stage := cfg[i-1].Name, set.byID[ids[i-1]].Stage(), set.byID[ids[i]].Stage()
		switch {
		case prevStage == stage:
			return nil, c.Name, fmt.Errorf("chain: transforms %q and %q are both of stage %d", prev, c.Name, stage)
		case prevStage > stage:
			return nil, c.Name, fmt.Errorf("chain: transform %q (stage %d) is listed after %q (stage %d), out of stage order", c.Name, stage, prev, prevStage)
		}
	}
	// In stage order with one per stage, an encrypt stage beside a compress
	// stage is the second of the list, after it.
	if len(cfg) > 1 && set.byID[ids[0]].Stage() == transform.StageCompress && set.byID[ids[1]].Stage() == transform.StageEncrypt {
		// decision: compression is off by default where the namespace
		// encrypts, rather than padded. Padding to fixed sizes costs space on
		// every chunk and still leaks the size class; turning compression off
		// costs space only where data compresses. Specify a padding
		// transform, in the compress stage's place, if a deployment needs
		// compression and encryption together against writers it does not
		// trust (RFC 5 §3.2, §9 item 3).
		if cfg[0].Settings["with_encryption"] != "accept" {
			return nil, cfg[0].Name, fmt.Errorf("chain: transform %q compresses beside encryption %q without with_encryption: accept, "+
				"so compressed sizes would leak content to a writer who places data beside it (RFC 5 Appendix A)", cfg[0].Name, cfg[1].Name)
		}
	}
	steps := make([]transform.Transform, len(cfg))
	for i, c := range cfg {
		t, err := fs[i](ctx, c.Settings, m)
		if err != nil {
			return nil, c.Name, fmt.Errorf("chain: building transform %q: %w", c.Name, err)
		}
		if t == nil {
			return nil, c.Name, fmt.Errorf("chain: transform %q's factory returned no transform", c.Name)
		}
		// decision: the list's order is judged on each transform's decoder,
		// built with empty settings, so the one its settings build must
		// report the same ID and stage: otherwise a stage that moves with a
		// setting would slip an order past the check, and its bodies would
		// carry an ID that decodes with another transform.
		if d := set.byID[ids[i]]; t.ID() != d.ID() || t.Stage() != d.Stage() {
			return nil, c.Name, fmt.Errorf("chain: transform %q built from its settings reports ID %#04x stage %d, not its registered ID %#04x stage %d",
				c.Name, t.ID(), t.Stage(), d.ID(), d.Stage())
		}
		steps[i] = t
	}
	return &chain{decode: set, steps: steps}, "", nil
}

// MaxDecodeLen answers from the decode set, never from the configured
// transforms, so a body written under a transform since removed from the
// chain is still admitted (RFC 5 §3.1).
func (c *chain) MaxDecodeLen(n int) int { return c.decode.maxDecodeLen(n) }

// ponytail: Pin and Decode fail every call with errors.ErrUnsupported, so a
// chain that builds cannot yet encode or decode. Replace them with the pin and
// the envelope's decode.

// Pin fails every call with errors.ErrUnsupported.
func (c *chain) Pin(context.Context, entity.NamespaceID) (transform.Attempt, error) {
	return nil, errors.ErrUnsupported
}

// Decode fails every call with errors.ErrUnsupported.
func (c *chain) Decode(context.Context, entity.NamespaceID, []byte, [32]byte, []byte, int, []transform.Encoded) ([]byte, error) {
	return nil, errors.ErrUnsupported
}
