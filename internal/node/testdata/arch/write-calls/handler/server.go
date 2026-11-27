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

// shares is a resource whose method is named like the write adapter.
type shares struct{ d Deps }

// write calls Write with its audit record set, from a method, not the
// adapter.
func (s shares) write(ctx context.Context, rec entity.AuditRecord) error {
	return s.d.Store.Write(ctx, entity.WriteMeta{Audit: &rec}, func(entity.ControlPlaneTx) error { return nil })
}

// hand takes Write as a method value, which a caller could call with no
// audit record.
func hand(d Deps) any { return d.Store.Write }
