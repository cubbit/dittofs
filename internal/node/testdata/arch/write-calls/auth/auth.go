package auth

import (
	"context"

	"github.com/cubbit/dittofs/pkg/metadata/entity"
)

type store interface {
	Write(ctx context.Context, meta entity.WriteMeta, fn func(entity.ControlPlaneTx) error) error
}

// settle writes with its audit record, as auth may.
func settle(ctx context.Context, s store, rec entity.AuditRecord) error {
	return s.Write(ctx, entity.WriteMeta{Audit: &rec}, func(entity.ControlPlaneTx) error { return nil })
}

// revoke writes with no audit record.
func revoke(ctx context.Context, s store) error {
	return s.Write(ctx, entity.WriteMeta{}, func(entity.ControlPlaneTx) error { return nil })
}

// forget writes with a nil audit record.
func forget(ctx context.Context, s store) error {
	return s.Write(ctx, entity.WriteMeta{Audit: nil}, func(entity.ControlPlaneTx) error { return nil })
}
