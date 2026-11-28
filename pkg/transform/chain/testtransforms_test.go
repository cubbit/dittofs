package chain_test

import (
	"context"
	"encoding/binary"

	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/transform"
	"github.com/cubbit/dittofs/pkg/transform/keys"
)

// pad is a test transform that appends zero bytes to its input: fixed of them,
// plus half the input's length when half is set, so with neither it is an
// identity. Its ID, stage and kinds are parameters, so one type stands in any
// stage and under any ID, a wrong one included. It uses no material, even when
// it declares a kind.
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

func (p pad) LengthKey() []byte {
	k := binary.BigEndian.AppendUint64(nil, uint64(p.fixed))
	if p.half {
		return append(k, 1)
	}
	return append(k, 0)
}

func (p pad) Encode(_ context.Context, _ entity.NamespaceID, _ []keys.MaterialID, dst []byte, _ [32]byte, plain []byte) (transform.Encoded, error) {
	body := append(dst[:0], plain...)
	body = append(body, make([]byte, p.extra(len(plain)))...)
	return transform.Encoded{Body: body, Applied: true, Version: 1}, nil
}

func (p pad) Decode(_ context.Context, _ entity.NamespaceID, dst []byte, _ [32]byte, body []byte, max int, _ keys.Provider) ([]byte, error) {
	n := len(body) - p.fixed
	if p.half {
		n = (2*n + 1) / 3 // the n for which n + n/2 is len(body) - fixed
	}
	if n < 0 || n+p.extra(n) != len(body) {
		return nil, transform.ErrMalformed
	}
	for _, b := range body[n:] {
		if b != 0 {
			return nil, transform.ErrMalformed
		}
	}
	if n > max {
		return nil, transform.ErrTooLarge
	}
	return append(dst[:0], body[:n]...), nil
}

// factory builds p, whatever the settings.
func (p pad) factory() transform.Factory {
	return func(context.Context, map[string]any, keys.Provider) (transform.Transform, error) {
		return p, nil
	}
}

// failing is a factory that fails with err, as a transform that cannot start
// does.
func failing(err error) transform.Factory {
	return func(context.Context, map[string]any, keys.Provider) (transform.Transform, error) {
		return nil, err
	}
}
