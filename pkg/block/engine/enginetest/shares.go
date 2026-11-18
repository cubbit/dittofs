package enginetest

import (
	"testing"

	"github.com/cubbit/dittofs/pkg/block/engine"
	"github.com/cubbit/dittofs/pkg/block/engine/enginewire"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/metadata/kvstore"
	"github.com/cubbit/dittofs/pkg/metadata/kvstore/kvstoretest"
)

// ShareOf is share id of s as the engine binds it, as the node binds it,
// with limit as its journal limit.
func ShareOf(tb testing.TB, s *kvstore.Store, id entity.ShareID, limit int64) engine.Share {
	tb.Helper()

	sh, _, err := s.ShareIndex().Share(tb.Context(), id)
	if err != nil {
		tb.Fatalf("enginetest: read share %v: %v", id, err)
	}

	return enginewire.Binding(sh, limit)
}

// AddShare creates a share named name in s, at the path /name, and returns
// it as ShareOf does.
func AddShare(tb testing.TB, s *kvstore.Store, name string, limit int64) engine.Share {
	tb.Helper()

	sh := entity.Share{
		ID:   entity.NewShareID(),
		Name: name,
		Path: "/" + name,
		Root: entity.NewFileID(),
	}

	if _, err := kvstoretest.CreateShare(tb.Context(), s.ControlPlane(), sh); err != nil {
		tb.Fatalf("enginetest: create share %s: %v", name, err)
	}

	return ShareOf(tb, s, sh.ID, limit)
}
