package engine

import (
	"context"
	"strings"
	"testing"

	"github.com/cubbit/dittofs/pkg/block/chunker"
	"github.com/cubbit/dittofs/pkg/block/journal"
	remotememory "github.com/cubbit/dittofs/pkg/block/remote/memory"
	"github.com/cubbit/dittofs/pkg/metadata"

	"github.com/cubbit/dittofs/pkg/metadata/store/badger/badgertest"
)

// syncedOnly exposes only the synced-hash methods of a real store, the shape of
// a store that keeps marks but no block records.
type syncedOnly struct {
	metadata.SyncedHashStore
}

// The dedup oracle needs a block's record to trust its mark. A store that has
// no block records must fail the flush, never let the oracle answer "not
// durable" for every chunk and so turn the dedup off without a word.
func TestFlush_DedupOnAStoreWithoutBlockRecordsFails(t *testing.T) {
	ctx := context.Background()
	ms := badgertest.NewInMemory(t)
	mem := remotememory.New()

	t.Cleanup(func() { _ = mem.Close() })

	j, err := journal.Open(t.TempDir(), journal.Config{CarveBlockSize: 1 << 20})
	if err != nil {
		t.Fatalf("journal.Open: %v", err)
	}

	t.Cleanup(func() { _ = j.Close() })

	if err := j.WriteAt(ctx, "f", 0, seamRandBytes(64<<10, 0x4386_0010)); err != nil {
		t.Fatalf("WriteAt: %v", err)
	}

	fn, reap := newFlushClosure(j, chunker.Params{}, 1<<20, engineDeduper{synced: syncedOnly{ms}},
		realSink(ms, mem), nil)

	err = j.Flush(ctx, "f", journal.FlushOptions{Force: true, AfterFile: reap}, fn)
	if err == nil {
		t.Fatal("Flush on a store without block records = nil, want the dedup's error")
	}

	if !strings.Contains(err.Error(), "has no block records to check") {
		t.Fatalf("Flush on a store without block records = %v, want the dedup's error", err)
	}
}
