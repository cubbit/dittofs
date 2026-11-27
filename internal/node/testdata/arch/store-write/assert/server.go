package api

import (
	"context"

	"github.com/cubbit/dittofs/pkg/metadata/entity"
)

// Deps holds only Write, as the api's does.
type Deps struct {
	Store interface {
		Write(ctx context.Context, meta entity.WriteMeta, fn func(entity.ControlPlaneTx) error) error
	}
}

// promote reaches a user write through a type assertion on the store.
func promote(ctx context.Context, d Deps, u entity.User) error {
	w, ok := d.Store.(interface {
		PutUser(ctx context.Context, u entity.User, expect uint64, a entity.Alloc) (uint64, error)
	})
	if !ok {
		return nil
	}
	_, err := w.PutUser(ctx, u, 0, entity.Alloc{})
	return err
}
