package chain

import (
	"context"

	"github.com/cubbit/dittofs/pkg/transform"
	"github.com/cubbit/dittofs/pkg/transform/keys"
)

// EnvelopeParser builds this registry's decode set for provider m and returns
// its envelope parse: the IDs a body lists and the offset of the last
// transform's output.
func (r Registry) EnvelopeParser(ctx context.Context, m keys.Provider) (func(body []byte) ([]transform.ID, int, error), error) {
	s, _, err := r.r.build(ctx, m)
	if err != nil {
		return nil, err
	}
	return func(body []byte) ([]transform.ID, int, error) {
		p, err := s.parseEnvelope(body)
		if err != nil {
			return nil, 0, err
		}
		ids := make([]transform.ID, p.n)
		for i := range p.n {
			ids[i] = p.steps[i].ID()
		}
		return ids, p.off, nil
	}, nil
}

// AppendEnvelope is appendEnvelope.
func AppendEnvelope(b []byte, ids []transform.ID) []byte { return appendEnvelope(b, ids) }
