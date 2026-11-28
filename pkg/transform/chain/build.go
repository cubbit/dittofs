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
	decode   *decodeSet
	steps    []transform.Transform // the configured transforms, in stage order
	require  []transform.ID        // deps.Require, resolved to IDs
	encrypts func(entity.NamespaceID) bool
}

var _ transform.Chain = (*chain)(nil)

// settingWithEncryption is the compress stage's setting that accepts
// compression beside encryption (RFC 5 §3.2), and acceptWithEncryption its one
// accepted value.
const (
	settingWithEncryption = "with_encryption"
	acceptWithEncryption  = "accept"
)

// lookup returns the ID name registered under.
func (r *registry) lookup(name string) (transform.ID, transform.Factory, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	id, ok := r.byName[name]
	return id, r.byID[id].factory, ok
}

// newChain is NewChain over registry r. Every refusal after the deps check is
// logged once, at Error, naming the transform where one is at fault.
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
	c, name, err := r.assemble(ctx, cfg, m, deps)
	if err != nil {
		attrs := []any{"reason", err}
		if name != "" {
			attrs = append(attrs, "transform", name)
		}
		deps.Logger.ErrorContext(ctx, "chain: the transform chain cannot be built", attrs...)
		return nil, err
	}
	return c, nil
}

// assemble builds the chain, or returns the reason it cannot and the name of
// the transform at fault, if one is. The list's shape is judged from the
// decode set's transforms, so no configured factory runs for a list that is
// refused.
func (r *registry) assemble(ctx context.Context, cfg []transform.Config, m keys.Provider, deps Deps) (*chain, string, error) {
	set, err := r.build(ctx, m)
	if err != nil {
		return nil, "", err
	}
	ids := make([]transform.ID, len(cfg))
	for i, c := range cfg {
		id, _, ok := r.lookup(c.Name)
		if !ok {
			return nil, c.Name, fmt.Errorf("chain: transform %q is not registered", c.Name)
		}
		if slices.Contains(ids[:i], id) {
			return nil, c.Name, fmt.Errorf("chain: transform %q is listed twice", c.Name)
		}
		ids[i] = id
		if i == 0 {
			continue
		}
		prev, prevStage, stage := cfg[i-1].Name, set.byID[ids[i-1]].Stage(), set.byID[id].Stage()
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
		if cfg[0].Settings[settingWithEncryption] != acceptWithEncryption {
			return nil, cfg[0].Name, fmt.Errorf("chain: transform %q compresses beside encryption %q without %s: %s, so sizes would leak plaintext (RFC 5 Appendix A)",
				cfg[0].Name, cfg[1].Name, settingWithEncryption, acceptWithEncryption)
		}
	}
	require := make([]transform.ID, len(deps.Require))
	for i, name := range deps.Require {
		id, _, ok := r.lookup(name)
		if !ok {
			return nil, name, fmt.Errorf("chain: required transform %q is not registered", name)
		}
		require[i] = id
	}
	steps := make([]transform.Transform, len(cfg))
	for i, c := range cfg {
		_, f, _ := r.lookup(c.Name)
		t, err := f(ctx, c.Settings, m)
		if err != nil {
			return nil, c.Name, fmt.Errorf("chain: building transform %q: %w", c.Name, err)
		}
		if t == nil {
			return nil, c.Name, fmt.Errorf("chain: transform %q's factory returned no transform", c.Name)
		}
		// A configured transform is the one its envelope ID decodes with, so
		// it reports that decoder's ID and stage, which the decode set checked.
		if d := set.byID[ids[i]]; t.ID() != d.ID() || t.Stage() != d.Stage() {
			return nil, c.Name, fmt.Errorf("chain: transform %q built from its settings reports ID %#04x stage %d, not its registered ID %#04x stage %d",
				c.Name, t.ID(), t.Stage(), d.ID(), d.Stage())
		}
		steps[i] = t
	}
	return &chain{decode: set, steps: steps, require: require, encrypts: deps.Encrypts}, "", nil
}

// MaxDecodeLen answers from the decode set, never from the configured
// transforms, so a body written under a transform since removed from the
// chain is still admitted (RFC 5 §3.1).
func (c *chain) MaxDecodeLen(n int) int { return c.decode.maxDecodeLen(n) }

// Pin is not built yet: it fails every call with errors.ErrUnsupported.
func (c *chain) Pin(context.Context, entity.NamespaceID) (transform.Attempt, error) {
	return nil, errors.ErrUnsupported
}

// Decode is not built yet: it fails every call with errors.ErrUnsupported.
func (c *chain) Decode(context.Context, entity.NamespaceID, []byte, [32]byte, []byte, int, []transform.Encoded) ([]byte, error) {
	return nil, errors.ErrUnsupported
}
