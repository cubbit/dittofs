package api

import (
	"context"

	"github.com/cubbit/dittofs/pkg/metadata/entity"
)

// Deps holds the store's Write, as the api's does.
type Deps struct {
	Store interface {
		Write(ctx context.Context, meta entity.WriteMeta, fn func(entity.ControlPlaneTx) error) error
	}
}

// write is the write adapter, which may call Write.
func write(ctx context.Context, d Deps, rec entity.AuditRecord) error {
	return d.Store.Write(ctx, entity.WriteMeta{Audit: &rec}, func(entity.ControlPlaneTx) error { return nil })
}

// putShare is a resource handler calling Write itself, with its audit
// record set.
func putShare(ctx context.Context, d Deps, rec entity.AuditRecord) error {
	return d.Store.Write(ctx, entity.WriteMeta{Audit: &rec}, func(entity.ControlPlaneTx) error { return nil })
}
