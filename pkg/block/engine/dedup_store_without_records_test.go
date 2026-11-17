package engine

import (
	"testing"

	"github.com/cubbit/dittofs/pkg/block/journal/journaltest"
	remotememory "github.com/cubbit/dittofs/pkg/block/remote/memory"
	"github.com/cubbit/dittofs/pkg/metadata"
	"github.com/cubbit/dittofs/pkg/metadata/store/badger/badgertest"
)

// syncedOnly exposes only the synced-hash methods of a real store, the shape of
// a store that keeps marks but no block records.
type syncedOnly struct {
	metadata.SyncedHashStore
}

// The dedup oracle needs a block's record to trust its mark. A store without
// block records is refused at wiring, so the carve stays off for it and no flush
// runs the oracle against it. A store with block records is accepted.
func TestSetSyncedHashStore_NeedsBlockRecordsForTheCarve(t *testing.T) {
	local := journaltest.New(t)
	rs := remotememory.New()
	t.Cleanup(func() { _ = rs.Close() })

	m := NewRemoteSync(local, rs, newStubFileChunkStore(), RemoteSyncConfig{})
	ms := badgertest.NewInMemory(t)

	m.SetSyncedHashStore(syncedOnly{ms})
	if _, _, committer, _ := m.wiring(); committer != nil {
		t.Fatal("a store without block records became the committer, want none")
	}

	m.SetSyncedHashStore(ms)
	if _, _, committer, _ := m.wiring(); committer == nil {
		t.Fatal("a store with block records was refused as committer")
	}
}
