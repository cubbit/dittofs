package chain_test

import (
	"context"

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

func (p pad) LengthKey() []byte { return nil }

// padByte fills pad's padding, which Decode checks.
const padByte = 0xa5

// Encode appends extra(n) padding bytes to plain.
func (p pad) Encode(_ context.Context, _ entity.NamespaceID, _ []keys.MaterialID, dst []byte, _ [32]byte, plain []byte) (transform.Encoded, error) {
	body := append(dst[:0], plain...)
	for range p.extra(len(plain)) {
		body = append(body, padByte)
	}
	return transform.Encoded{Body: body, Applied: true, Version: p.Version()}, nil
}

// Decode finds the n with n + extra(n) = len(body), checks the padding, and
// returns the first n bytes.
func (p pad) Decode(_ context.Context, _ entity.NamespaceID, dst []byte, _ [32]byte, body []byte, limit int, _ keys.Provider) ([]byte, error) {
	l := len(body) - p.fixed
	if l < 0 {
		return nil, transform.ErrMalformed
	}
	n := l
	if p.half {
		// n + n/2 is 3k for n = 2k and 3k+1 for n = 2k+1, never 3k+2.
		if l%3 == 2 {
			return nil, transform.ErrMalformed
		}
		n = l/3*2 + l%3
	}
	for _, b := range body[n:] {
		if b != padByte {
			return nil, transform.ErrMalformed
		}
	}
	if n > limit {
		return nil, transform.ErrTooLarge
	}
	return append(dst, body[:n]...), nil
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
