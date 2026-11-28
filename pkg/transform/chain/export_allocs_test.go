package chain

import (
	"context"

	"github.com/cubbit/dittofs/pkg/transform/keys"
)

// EnvelopeCounter builds this registry's decode set for provider m and returns
// its envelope parse, answering only how many transforms a body lists, so a
// caller measures the parse's own allocations and none of its answer's.
func (r Registry) EnvelopeCounter(ctx context.Context, m keys.Provider) (func(body []byte) (int, error), error) {
	s, _, err := r.r.build(ctx, m)
	if err != nil {
		return nil, err
	}
	return func(body []byte) (int, error) {
		p, err := s.parseEnvelope(body)
		return p.n, err
	}, nil
}
