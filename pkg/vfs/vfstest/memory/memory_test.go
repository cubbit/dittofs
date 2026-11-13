package memory_test

import (
	"context"
	"errors"
	"testing"

	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/metadata/kvstore"
	"github.com/cubbit/dittofs/pkg/vfs"
	"github.com/cubbit/dittofs/pkg/vfs/vfstest"
	"github.com/cubbit/dittofs/pkg/vfs/vfstest/memory"
	"github.com/cubbit/dittofs/pkg/vfs/vfstest/suite"
)

func TestRunPassesMemory(t *testing.T) {
	suite.Run(t, func(t *testing.T) vfs.Service { return memory.New(t) })
}

// Faults with nothing armed passes every call through unchanged, so the
// filesystem behind it still passes the suite.
func TestRunPassesFaultsOverMemory(t *testing.T) {
	suite.Run(t, func(t *testing.T) vfs.Service { return vfstest.Faults(memory.New(t)) })
}

// A chown commits the file's pending existence, after which the file's size
// and its owner's usage are read from the store alone: what the write and the
// change after it left, a removal's included, with no removal left half done.
// A clone into the file is charged its source's data, not its holes.
func TestAnExistenceCommitReachesTheStore(t *testing.T) {
	for _, c := range []struct {
		name  string
		then  func(ctx context.Context, s *memory.Service, f entity.File, h vfs.Handle) error // after the write of 10 bytes
		size  int64
		bytes int64
	}{
		{"a write", nil, 10, 10},
		{"a shrinking truncate", truncate(3), 3, 3},
		{"a growing truncate", truncate(16), 16, 10},
		{"a deallocate", func(ctx context.Context, s *memory.Service, _ entity.File, h vfs.Handle) error {
			_, err := s.Deallocate(ctx, s.Admin(), vfs.Anonymous(h), 2, 2)
			return err
		}, 10, 8},
		{"a clone of data past the end", cloneInto(func(ctx context.Context, s *memory.Service, src vfs.Handle) error {
			_, err := s.Write(ctx, s.Admin(), vfs.Anonymous(src), 0, []byte("abcd"), vfs.Unstable)
			return err
		}, 8, 4), 12, 12},
		{"a clone of a hole past the end", cloneInto(func(ctx context.Context, s *memory.Service, src vfs.Handle) error {
			return truncate(100)(ctx, s, entity.File{}, src)
		}, 10, 100), 110, 10},
		// This case pins the known difference the decision: over Memory's
		// commit names: the store keeps a hole cloned over the file's data
		// charged, where the engine's clone would free it (6 bytes). A fix
		// changes this case.
		{"a clone of a hole over data", cloneInto(func(ctx context.Context, s *memory.Service, src vfs.Handle) error {
			return truncate(4)(ctx, s, entity.File{}, src)
		}, 2, 4), 10, 10},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := memory.New(t)
			ctx, admin := t.Context(), s.Admin()
			h, f := s.Store.Create(t, s.Store.Admin, s.Store.Root, "f", kvstore.Regular(0o644))
			if _, err := s.Write(ctx, admin, vfs.Anonymous(h), 0, []byte("0123456789"), vfs.Unstable); err != nil {
				t.Fatalf("Write: %v", err)
			}
			if c.then != nil {
				if err := c.then(ctx, s, f, h); err != nil {
					t.Fatalf("%s: %v", c.name, err)
				}
			}
			chown := vfs.AttrChange{Attrs: entity.Attrs{}.WithOwner(f.Owner)}
			if _, err := s.SetAttr(ctx, admin, vfs.Target{H: h}, chown); err != nil {
				t.Fatalf("SetAttr of the owner: %v", err)
			}
			if _, pending, err := s.Content.Overlay(ctx, f.Share, f.ID); err != nil || pending {
				t.Fatalf("Overlay after the chown: pending %v, %v; want nothing pending", pending, err)
			}
			if got, err := s.GetAttr(ctx, admin, h); err != nil || got.Size != c.size {
				t.Errorf("GetAttr after the chown: size %d, %v; want %d", got.Size, err, c.size)
			}
			if u, _, err := s.Quota(ctx, admin, h, f.Owner); err != nil || u.Bytes != c.bytes {
				t.Errorf("Quota after the chown: %d bytes used, %v; want %d", u.Bytes, err, c.bytes)
			}
			for r, err := range s.Store.Store.Content().UnfinishedRemovals(ctx, f.Share, uint64(s.Store.Share.Shard)) {
				t.Errorf("after the chown: removal %+v not pruned, %v; want none", r, err)
			}
		})
	}
}

// A write moves the file's modification time past its creation's, while
// pending and once its existence commit reaches the store, so a handler's
// pre- and post-operation attributes differ.
func TestAWriteMovesTheModificationTime(t *testing.T) {
	s := memory.New(t)
	ctx, admin := t.Context(), s.Admin()
	h, f := s.Store.Create(t, s.Store.Admin, s.Store.Root, "f", kvstore.Regular(0o644))
	before, err := s.GetAttr(ctx, admin, h)
	if err != nil {
		t.Fatalf("GetAttr before the write: %v", err)
	}
	if _, err := s.Write(ctx, admin, vfs.Anonymous(h), 0, []byte("01234"), vfs.Unstable); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if got, err := s.GetAttr(ctx, admin, h); err != nil || !got.Modify.After(before.Modify) {
		t.Errorf("GetAttr after the write: mtime %v, %v; want later than %v", got.Modify, err, before.Modify)
	}
	chown := vfs.AttrChange{Attrs: entity.Attrs{}.WithOwner(f.Owner)}
	if _, err := s.SetAttr(ctx, admin, vfs.Target{H: h}, chown); err != nil {
		t.Fatalf("SetAttr of the owner: %v", err)
	}
	if got, err := s.Store.Store.Files().Get(ctx, s.Store.Admin, h); err != nil || !got.Modify.After(before.Modify) {
		t.Errorf("the store's record after the existence commit: mtime %v, %v; want later than %v", got.Modify, err, before.Modify)
	}
}

// An unlink of a file's last name, with no open left, releases it in the
// store too: its handle goes stale, and its owner's usage counts it no more.
func TestAReleaseReachesTheStore(t *testing.T) {
	s := memory.New(t)
	ctx, admin := t.Context(), s.Admin()
	h, f := s.Store.Create(t, s.Store.Admin, s.Store.Root, "f", kvstore.Regular(0o644))
	if _, err := s.Write(ctx, admin, vfs.Anonymous(h), 0, []byte("0123456789"), vfs.Unstable); err != nil {
		t.Fatalf("Write: %v", err)
	}
	before, _, err := s.Quota(ctx, admin, s.Store.Root, f.Owner)
	if err != nil {
		t.Fatalf("Quota: %v", err)
	}
	if _, err := s.Unlink(ctx, admin, vfs.Target{H: s.Store.Root}, []byte("f")); err != nil {
		t.Fatalf("Unlink: %v", err)
	}
	if _, err := s.GetAttr(ctx, admin, h); !errors.Is(err, vfs.ErrStale) {
		t.Errorf("GetAttr of the released file: %v, want ErrStale", err)
	}
	if u, _, err := s.Quota(ctx, admin, s.Store.Root, f.Owner); err != nil || u.Files != before.Files-1 || u.Bytes != 0 {
		t.Errorf("Quota after the release: %d files, %d bytes, %v; want %d, 0", u.Files, u.Bytes, err, before.Files-1)
	}
}

func truncate(n int64) func(ctx context.Context, s *memory.Service, f entity.File, h vfs.Handle) error {
	return func(ctx context.Context, s *memory.Service, _ entity.File, h vfs.Handle) error {
		_, err := s.SetAttr(ctx, s.Admin(), vfs.Target{H: h}, vfs.AttrChange{Attrs: entity.Attrs{}.WithSize(n)})
		return err
	}
}

// cloneInto clones n bytes from the start of a new file, which fill has
// written, into the file at off, as the administrator.
func cloneInto(fill func(ctx context.Context, s *memory.Service, src vfs.Handle) error, off, n int64) func(ctx context.Context, s *memory.Service, f entity.File, h vfs.Handle) error {
	return func(ctx context.Context, s *memory.Service, _ entity.File, dst vfs.Handle) error {
		h, _, err := s.Store.Store.Namespace().Create(ctx, s.Store.Admin, s.Store.Root, []byte("src"), kvstore.Regular(0o644))
		if err != nil {
			return err
		}
		if err := fill(ctx, s, h); err != nil {
			return err
		}
		_, err = s.Clone(ctx, s.Admin(), vfs.CopyArgs{Src: vfs.Anonymous(h), Dst: vfs.Anonymous(dst), Extents: []vfs.Extent{{SrcOff: 0, DstOff: off, Len: n}}})
		return err
	}
}
