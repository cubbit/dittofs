package enginetest

import (
	"testing"

	"github.com/cubbit/dittofs/pkg/block/engine"
	"github.com/cubbit/dittofs/pkg/block/journal"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/metadata/kvstore"
)

// ShareOf is share id of s as the engine binds it: its tag and shard as the
// store drew them, limit as its journal limit, serving.
func ShareOf(tb testing.TB, s *kvstore.Store, id entity.ShareID, limit int64) engine.Share {
	tb.Helper()

	sh, _, err := s.ShareIndex().Share(tb.Context(), id)
	if err != nil {
		tb.Fatalf("enginetest: read share %v: %v", id, err)
	}

	return engine.Share{
		ID:    sh.ID,
		Tag:   journal.ShareTag(sh.Tag),
		Shard: sh.Shard,
		Limit: limit,
		State: engine.Serving,
	}
}

// AddShare creates a share named name in s, at the path /name, and returns
// it as ShareOf does.
func AddShare(tb testing.TB, s *kvstore.Store, name string, limit int64) engine.Share {
	tb.Helper()

	ns := entity.Namespace{ID: entity.NewNamespaceID(), Store: [16]byte{15: 1}}
	sh := entity.Share{
		ID:         entity.NewShareID(),
		Name:       name,
		Path:       "/" + name,
		Root:       entity.NewFileID(),
		Namespaces: []entity.NamespaceGen{{Generation: 1, Namespace: ns.ID}},
	}

	if _, err := s.ControlPlane().PutNamespace(tb.Context(), ns, 0); err != nil {
		tb.Fatalf("enginetest: create namespace of share %s: %v", name, err)
	}
	if _, err := s.ControlPlane().PutShare(tb.Context(), sh, 0); err != nil {
		tb.Fatalf("enginetest: create share %s: %v", name, err)
	}

	return ShareOf(tb, s, sh.ID, limit)
}
