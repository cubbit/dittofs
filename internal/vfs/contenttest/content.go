// Package contenttest is the conformance suite of the filesystem's Content,
// the engine's content methods, and the in-memory Content it holds. Run drives
// any Content through what RFC 8 §12.1 and RFC 17 §3.5 ask of it; Memory
// passes it, and stands in for the engine where a test needs content but not
// the engine itself.
package contenttest

import (
	"context"

	"github.com/cubbit/dittofs/pkg/block/engine"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/vfs"
)

// Content is internal/vfs.Content, method for method: that declaration
// documents each method.
//
// decision: the interface is declared twice, here and in internal/vfs, so
// that this package imports nothing of internal/vfs and the filesystem's own
// tests, in package vfs, can run over Memory. The two are held identical by
// assignments both ways in this package's tests; drop this copy if the
// filesystem's tests ever move out of package vfs.
type Content interface {
	Draw(ctx context.Context, s entity.ShareID, f entity.FileID) (entity.JournalVersion, error)
	Write(ctx context.Context, s entity.ShareID, f entity.FileID, off int64, p []byte, o engine.WriteOpts) (vfs.Verifier, error)
	Commit(ctx context.Context, s entity.ShareID, f entity.FileID, off, n int64) (v vfs.Verifier, lossSeq uint64, err error)
	CommitExistence(ctx context.Context, s entity.ShareID, f entity.FileID) error
	LossSeq(s entity.ShareID, f entity.FileID) uint64
	Read(ctx context.Context, s entity.ShareID, f entity.FileID, off int64, dst []byte) (int, error)
	Truncate(ctx context.Context, s entity.ShareID, f entity.FileID, size int64) error
	Deallocate(ctx context.Context, s entity.ShareID, f entity.FileID, off, n int64) error
	Delete(ctx context.Context, s entity.ShareID, f entity.FileID) (retry <-chan error, err error)
	Clone(ctx context.Context, srcShare entity.ShareID, src entity.FileID, dstShare entity.ShareID, dst entity.FileID, x vfs.Extent) error
	Copy(ctx context.Context, srcShare entity.ShareID, src entity.FileID, dstShare entity.ShareID, dst entity.FileID, xs []vfs.Extent) (int64, error)
	Overlay(ctx context.Context, s entity.ShareID, f entity.FileID) (o engine.Overlay, pending bool, err error)
	Allocation(ctx context.Context, s entity.ShareID, f entity.FileID, off int64) (entity.Span, error)
}
