package runtime

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"slices"
	"sync"
	"testing"

	"github.com/cubbit/dittofs/pkg/block"
	blockgc "github.com/cubbit/dittofs/pkg/block/gc"
	"github.com/cubbit/dittofs/pkg/block/remote"
	remotememory "github.com/cubbit/dittofs/pkg/block/remote/memory"
)

// callLogRemote passes every block call through to the memory remote and keeps
// the name of each one, since the requirement is which calls reach the store
// under the GC lock. getErr, when set, is a fault: GetBlock answers it instead.
type callLogRemote struct {
	*remotememory.Store
	getErr error

	mu    sync.Mutex
	calls []string
}

func (c *callLogRemote) log(op string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.calls = append(c.calls, op)
}

func (c *callLogRemote) logged() []string {
	c.mu.Lock()
	defer c.mu.Unlock()

	return slices.Clone(c.calls)
}

func (c *callLogRemote) GetBlock(ctx context.Context, blockID string) ([]byte, error) {
	c.log("get")

	if c.getErr != nil {
		return nil, c.getErr
	}

	return c.Store.GetBlock(ctx, blockID)
}

func (c *callLogRemote) PutBlock(ctx context.Context, blockID string, r io.Reader) error {
	c.log("put")

	return c.Store.PutBlock(ctx, blockID, r)
}

func (c *callLogRemote) DeleteBlock(ctx context.Context, blockID string) error {
	c.log("delete")

	return c.Store.DeleteBlock(ctx, blockID)
}

// newCompactionCandidate builds a runtime with one share on rs and seeds a
// compaction candidate there: one 80-byte live chunk in a 4 KiB block.
func newCompactionCandidate(t *testing.T, rs *callLogRemote) (*Runtime, *GCDefaults) {
	t.Helper()

	ctx := context.Background()
	rt := newRuntimeForGC(t, map[string]remote.RemoteStore{"/export": rs})

	mds, err := rt.GetMetadataStoreForShare("/export")
	if err != nil {
		t.Fatalf("GetMetadataStoreForShare: %v", err)
	}

	data := bytes.Repeat([]byte("x"), 4096)
	if err := rs.Store.PutBlock(ctx, "blk-sparse", bytes.NewReader(data)); err != nil {
		t.Fatalf("PutBlock: %v", err)
	}

	if err := mds.PutBlockRecord(ctx, block.BlockRecord{
		BlockID:        "blk-sparse",
		Length:         int64(len(data)),
		LiveChunkCount: 1,
		SyncState:      block.BlockStateRemote,
	}); err != nil {
		t.Fatalf("PutBlockRecord: %v", err)
	}

	loc := block.ChunkLocator{BlockID: "blk-sparse", WireLength: 80}
	if err := mds.MarkSynced(ctx, testHash("sparse-live-chunk"), loc); err != nil {
		t.Fatalf("MarkSynced: %v", err)
	}

	return rt, &GCDefaults{CompactionLiveRatio: 0.5}
}

// A sweep that ended on a deadline found the remote not answering: compaction
// must not call it again under the same lock. The control case shows the same
// candidate does reach the store when the sweep ended normally.
func TestCompactRemoteForEntry_SkippedAfterSweepDeadline(t *testing.T) {
	cases := []struct {
		name      string
		sweep     blockgc.GCStats
		wantCalls []string
	}{
		{"sweep ended on a deadline", blockgc.GCStats{EndedOnDeadline: true}, nil},
		{"sweep ended normally", blockgc.GCStats{}, []string{"get"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rs := &callLogRemote{Store: remotememory.New()}
			defer func() { _ = rs.Close() }()

			rt, defaults := newCompactionCandidate(t, rs)

			var total blockgc.GCStats
			rt.compactRemoteForEntry(context.Background(), remoteEntryForShare(t, rt, "/export"),
				false, defaults, tc.sweep, &total)

			if got := rs.logged(); !slices.Equal(got, tc.wantCalls) {
				t.Fatalf("remote calls = %v, want %v", got, tc.wantCalls)
			}
		})
	}
}

// A compaction call that times out ends the pass, and the run's total says so:
// the error is counted and EndedOnDeadline is set, so gc-status shows it.
func TestCompactRemoteForEntry_DeadlineShowsInTotal(t *testing.T) {
	rs := &callLogRemote{
		Store:  remotememory.New(),
		getErr: fmt.Errorf("get block blk-sparse: %w", context.DeadlineExceeded),
	}
	defer func() { _ = rs.Close() }()

	rt, defaults := newCompactionCandidate(t, rs)

	var total blockgc.GCStats
	rt.compactRemoteForEntry(context.Background(), remoteEntryForShare(t, rt, "/export"),
		false, defaults, blockgc.GCStats{}, &total)

	if total.ErrorCount != 1 {
		t.Fatalf("total ErrorCount = %d, want 1 (the timed-out get)", total.ErrorCount)
	}

	if !total.EndedOnDeadline {
		t.Fatalf("total EndedOnDeadline = false, want true (compaction ended on a deadline)")
	}
}

// One remote whose pass ended on a deadline marks the whole run, whatever the
// remotes after it report.
func TestAccumulateGCStats_CarriesEndedOnDeadline(t *testing.T) {
	var total blockgc.GCStats

	accumulateGCStats(&total, &blockgc.GCStats{EndedOnDeadline: true})
	accumulateGCStats(&total, &blockgc.GCStats{})

	if !total.EndedOnDeadline {
		t.Fatalf("total EndedOnDeadline = false, want true (the first remote ended on a deadline)")
	}
}
