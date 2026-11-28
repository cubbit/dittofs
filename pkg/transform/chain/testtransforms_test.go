package chain_test

import (
	"context"
	"errors"

	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/transform"
	"github.com/cubbit/dittofs/pkg/transform/keys"
)

// pad is a test transform whose bound adds fixed bytes to its input, plus half
// the input's length when half is set, so with neither it is an identity. Its
// ID, stage and kinds are parameters, so one type stands in any stage and
// under any ID, a wrong one included. It uses no material, even when it
// declares a kind.
type pad struct {
	id    transform.ID
	stage transform.Stage
	fixed int
	half  bool
	kinds []string
}

// The test transforms' IDs, from the range reserved for custom transforms.
const (
	idA transform.ID = 0x8001 + iota
	idB
	idC
)

func (p pad) extra(n int) int {
	e := p.fixed
	if p.half {
		e += n / 2
	}
	return e
}

func (p pad) ID() transform.ID                                { return p.id }
func (p pad) Version() uint8                                  { return 1 }
func (p pad) Stage() transform.Stage                          { return p.stage }
func (p pad) Kinds() []string                                 { return p.kinds }
func (p pad) MaxEncodedLen(n int, _ []keys.MaterialID) int    { return n + p.extra(n) }
func (p pad) LengthOf(n int, _ []keys.MaterialID) (int, bool) { return n + p.extra(n), true }

// The encoding half lands with the chain's encode and decode, and its tests.
func (p pad) LengthKey() []byte { return nil }
func (p pad) Encode(context.Context, entity.NamespaceID, []keys.MaterialID, []byte, [32]byte, []byte) (transform.Encoded, error) {
	return transform.Encoded{}, errors.ErrUnsupported
}
func (p pad) Decode(context.Context, entity.NamespaceID, []byte, [32]byte, []byte, int, keys.Provider) ([]byte, error) {
	return nil, errors.ErrUnsupported
}

// factory builds p, whatever the settings.
func (p pad) factory() transform.Factory {
	return func(context.Context, map[string]any, keys.Provider) (transform.Transform, error) {
		return p, nil
	}
}

// failing is a factory that fails with err, as a transform that cannot start
// does; nilTransform is one that returns neither a transform nor an error.
func failing(err error) transform.Factory {
	return func(context.Context, map[string]any, keys.Provider) (transform.Transform, error) {
		return nil, err
	}
}

func nilTransform(context.Context, map[string]any, keys.Provider) (transform.Transform, error) {
	return nil, nil
}

// onSetting is a fault wrapper over factory f: a build whose settings carry
// key runs fault instead, so a transform builds for decoding, with empty
// settings, and fails, or builds another transform, when configured with key.
func onSetting(key string, f, fault transform.Factory) transform.Factory {
	return func(ctx context.Context, s map[string]any, m keys.Provider) (transform.Transform, error) {
		if _, ok := s[key]; ok {
			return fault(ctx, s, m)
		}
		return f(ctx, s, m)
	}
}
