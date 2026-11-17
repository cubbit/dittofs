package gc

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"testing"
	"time"

	"lukechampine.com/blake3"

	"github.com/cubbit/dittofs/pkg/block"
	"github.com/cubbit/dittofs/pkg/block/remote"
	remotememory "github.com/cubbit/dittofs/pkg/block/remote/memory"
	"github.com/cubbit/dittofs/pkg/metadata"

	"github.com/cubbit/dittofs/pkg/metadata/store/badger/badgertest"
)

// errUnbounded is what a stalled call answers when its context has no
// deadline: such a call would hang forever against a real stalled store.
var errUnbounded = errors.New("remote call made without a deadline")

// stalledStore is a fault wrapper over the memory remote: GetBlock (get),
// PutBlock (put) or DeleteBlock (del) stops answering, the way a stalled store
// does, until the call's own context ends.
type stalledStore struct {
	*remotememory.Store
	get bool
	put bool
	del bool
}

func stall(ctx context.Context) error {
	if _, ok := ctx.Deadline(); !ok {
		return errUnbounded
	}

	<-ctx.Done()

	return ctx.Err()
}

func (s stalledStore) GetBlock(ctx context.Context, blockID string) ([]byte, error) {
	if s.get {
		return nil, fmt.Errorf("get block %s: %w", blockID, stall(ctx))
	}

	return s.Store.GetBlock(ctx, blockID)
}

func (s stalledStore) PutBlock(ctx context.Context, blockID string, r io.Reader) error {
	if s.put {
		return fmt.Errorf("put block %s: %w", blockID, stall(ctx))
	}

	return s.Store.PutBlock(ctx, blockID, r)
}

func (s stalledStore) DeleteBlock(ctx context.Context, blockID string) error {
	if s.del {
		return fmt.Errorf("delete block %s: %w", blockID, stall(ctx))
	}

	return s.Store.DeleteBlock(ctx, blockID)
}

// failingGetStore is a fault wrapper over the memory remote whose GetBlock
// fails at once with an error that is not a timeout.
type failingGetStore struct {
	*remotememory.Store
}

func (s failingGetStore) GetBlock(_ context.Context, blockID string) ([]byte, error) {
	return nil, fmt.Errorf("get block %s: connection reset", blockID)
}

// seedCompactionCandidate writes a four-chunk block with three chunks dead, so
// it is 25% live and a candidate at LiveRatio 0.5. A husk has all four chunks
// dead, and a synced marker for a chunk the block does not hold keeps it a
// candidate: compaction then deletes the whole block.
func seedCompactionCandidate(
	t *testing.T,
	st metadata.Store,
	rbs remote.RemoteBlockStore,
	blockID string,
	husk bool,
) {
	t.Helper()

	chunks := make([][]byte, 4)
	for i := range chunks {
		chunks[i] = bytes.Repeat([]byte(fmt.Sprintf("%s-%d|", blockID, i)), 8)
	}

	hashes := seedRealPackedBlock(t, st, rbs, blockID, chunks)
	if !husk {
		hashes = hashes[1:]
	}

	for _, h := range hashes {
		killChunk(t, st, blockID, h)
	}

	if husk {
		foreign := block.ContentHash(blake3.Sum256([]byte(blockID + "-not-in-block")))

		loc := block.ChunkLocator{BlockID: blockID, WireLength: 8}
		if err := st.MarkSynced(t.Context(), foreign, loc); err != nil {
			t.Fatalf("MarkSynced(%s): %v", foreign, err)
		}
	}
}

// A remote call that reaches its deadline means the store is not answering:
// compaction stops at the first one, instead of paying one more call per
// candidate while it holds the remote's GC lock, and every block keeps its
// object and its record.
func TestCompactBlocks_TimedOutCallEndsThePass(t *testing.T) {
	cases := []struct {
		name  string
		husk  bool
		store func(*remotememory.Store) remote.RemoteBlockStore
	}{
		{"delete stalls", false, func(m *remotememory.Store) remote.RemoteBlockStore {
			return stalledStore{Store: m, del: true}
		}},
		{"husk delete stalls", true, func(m *remotememory.Store) remote.RemoteBlockStore {
			return stalledStore{Store: m, del: true}
		}},
		{"get stalls", false, func(m *remotememory.Store) remote.RemoteBlockStore {
			return stalledStore{Store: m, get: true}
		}},
		{"put stalls", false, func(m *remotememory.Store) remote.RemoteBlockStore {
			return stalledStore{Store: m, put: true}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := badgertest.NewInMemory(t)

			rbs := remotememory.New()
			defer func() { _ = rbs.Close() }()

			ids := []string{"blk-cand-1", "blk-cand-2"}
			for _, id := range ids {
				seedCompactionCandidate(t, st, rbs, id, tc.husk)
			}

			// The deadline is real time: a context deadline takes no injected
			// clock. One nanosecond makes a stalled call end at once.
			rep, err := CompactBlocks(t.Context(), []CompactMetaView{st}, tc.store(rbs),
				CompactOptions{LiveRatio: 0.5, callTimeout: time.Nanosecond})

			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("CompactBlocks err = %v, want the call's DeadlineExceeded", err)
			}

			if rep.Errors != 1 {
				t.Fatalf(
					"CompactBlocks Errors = %d, want 1 (the pass stops at the first timeout)",
					rep.Errors,
				)
			}

			for _, id := range ids {
				if _, ok, err := st.GetBlockRecord(t.Context(), id); err != nil || !ok {
					t.Fatalf("record of %s: ok = %v, err = %v, want kept", id, ok, err)
				}

				if _, err := rbs.GetBlock(t.Context(), id); err != nil {
					t.Fatalf("object of %s: %v, want kept", id, err)
				}
			}
		})
	}
}

// A remote failure that is not a timeout says nothing about the store as a
// whole: it is counted and the pass goes on to the next candidate.
func TestCompactBlocks_OtherCallErrorGoesOn(t *testing.T) {
	st := badgertest.NewInMemory(t)

	rbs := remotememory.New()
	defer func() { _ = rbs.Close() }()

	seedCompactionCandidate(t, st, rbs, "blk-cand-1", false)
	seedCompactionCandidate(t, st, rbs, "blk-cand-2", false)

	rep, err := CompactBlocks(t.Context(), []CompactMetaView{st}, failingGetStore{rbs},
		CompactOptions{LiveRatio: 0.5})
	if err != nil {
		t.Fatalf("CompactBlocks err = %v, want nil (a failed get is not a timeout)", err)
	}

	if rep.Errors != 2 {
		t.Fatalf("CompactBlocks Errors = %d, want 2 (one per candidate)", rep.Errors)
	}
}

// Every remote call of compaction runs under a deadline: the one the options
// set, or compactCallTimeout when they set none.
func TestCallDeadline(t *testing.T) {
	cases := []struct {
		name string
		set  time.Duration
		want time.Duration
	}{
		{"unset uses the default", 0, compactCallTimeout},
		{"set is used", time.Second, time.Second},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := callDeadline(t.Context(), CompactOptions{callTimeout: tc.set})
			defer cancel()

			deadline, ok := ctx.Deadline()
			if !ok {
				t.Fatalf("call context has no deadline, want one %v away", tc.want)
			}

			if left := time.Until(deadline); left > tc.want || left <= tc.want/2 {
				t.Fatalf("call deadline %v away, want at most %v and more than %v",
					left, tc.want, tc.want/2)
			}
		})
	}
}
