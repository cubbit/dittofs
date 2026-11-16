package api

import (
	"context"
	"iter"

	"github.com/cubbit/dittofs/pkg/metadata/entity"
)

// Deps holds a store method that writes outside Write.
type Deps struct {
	Store interface {
		Shares(ctx context.Context) iter.Seq2[entity.Share, error]
		PutShare(ctx context.Context, s entity.Share) error
	}
}
