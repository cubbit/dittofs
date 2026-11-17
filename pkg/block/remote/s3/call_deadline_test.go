package s3

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/block"
	blockgc "github.com/cubbit/dittofs/pkg/block/gc"
)

// stallingS3 is a fault wrapper over the S3 emulator. A listing or a delete it
// is told to hold gets no answer until the client gives up or the test ends; a
// listing page it is told to delay is answered after listDelay. Every other
// request goes to the emulator unchanged.
type stallingS3 struct {
	inner      http.Handler
	holdList   bool
	holdDelete bool
	listDelay  time.Duration
	released   chan struct{}
}

func (s *stallingS3) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	isList := r.Method == http.MethodGet && r.URL.Query().Get("list-type") == "2"
	if (isList && s.holdList) || (r.Method == http.MethodDelete && s.holdDelete) {
		select {
		case <-r.Context().Done():
		case <-s.released:
		}

		return
	}

	if isList && s.listDelay > 0 {
		select {
		case <-time.After(s.listDelay):
		case <-r.Context().Done():
			return
		}
	}

	s.inner.ServeHTTP(w, r)
}

// newStallingStore builds a Store the way production does (NewFromConfig: its
// HTTP client and retryer) against the emulator behind the stalling wrapper.
func newStallingStore(t *testing.T, stall *stallingS3) (*Store, *mockS3) {
	t.Helper()

	mock := newMockS3("deadline")
	stall.inner = mock
	stall.released = make(chan struct{})
	srv := httptest.NewServer(stall)
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(stall.released) }) // runs first: frees held handlers

	store, err := NewFromConfig(context.Background(), Config{
		Bucket:         "deadline",
		Region:         "us-east-1",
		Endpoint:       srv.URL,
		AccessKey:      "test",
		SecretKey:      "test",
		ForcePathStyle: true,
		AllowPrivate:   true,
	})
	if err != nil {
		t.Fatalf("NewFromConfig: %v", err)
	}

	t.Cleanup(func() { _ = store.Close() })

	return store, mock
}

func seedAgedBlocks(mock *mockS3, ids ...string) {
	mock.mu.Lock()
	defer mock.mu.Unlock()

	for _, id := range ids {
		mock.objects[blockObjectPrefix+id] = mockObject{
			data:         []byte(id),
			lastModified: time.Now().Add(-2 * time.Hour),
		}
	}
}

// backstop is the caller's own deadline in these tests. No clock can be
// injected into the HTTP client, so this bounded wait is part of the
// assertion: a call that returns only when it ends had no deadline of its own.
const backstop = 10 * time.Second

func TestWalkBlocks_StalledPageEndsOnTheCallDeadline(t *testing.T) {
	store, mock := newStallingStore(t, &stallingS3{holdList: true})
	seedAgedBlocks(mock, "blk-1")

	store.callTimeout = 100 * time.Millisecond

	ctx, cancel := context.WithTimeout(t.Context(), backstop)
	defer cancel()

	err := store.WalkBlocks(ctx, func(string, block.Meta) error { return nil })

	if ctx.Err() != nil {
		t.Fatalf(
			"WalkBlocks ended only on the caller's own deadline (err %v), want the page's deadline",
			err,
		)
	}

	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("WalkBlocks err = %v, want DeadlineExceeded from the page's deadline", err)
	}
}

// The deadline is per page, not per walk: a listing that grows with the bucket
// must finish when every page answers in time. Five pages of 400 ms each take
// 2 s in all, against a 1 s deadline per call.
func TestWalkBlocks_DeadlineIsPerPage(t *testing.T) {
	store, mock := newStallingStore(t, &stallingS3{listDelay: 400 * time.Millisecond})
	seedAgedBlocks(mock, "blk-1", "blk-2", "blk-3", "blk-4", "blk-5")
	mock.listPageSize = 1
	store.callTimeout = time.Second

	var got []string

	err := store.WalkBlocks(t.Context(), func(id string, _ block.Meta) error {
		got = append(got, id)
		return nil
	})
	if err != nil {
		t.Fatalf(
			"WalkBlocks err = %v after %d of 5 blocks, want every page within its own deadline",
			err,
			len(got),
		)
	}

	if len(got) != 5 {
		t.Fatalf("WalkBlocks yielded %d blocks %v, want 5", len(got), got)
	}
}

func TestDeleteBlock_StalledDeleteEndsOnTheCallDeadline(t *testing.T) {
	store, mock := newStallingStore(t, &stallingS3{holdDelete: true})
	seedAgedBlocks(mock, "blk-1")

	store.callTimeout = 100 * time.Millisecond

	ctx, cancel := context.WithTimeout(t.Context(), backstop)
	defer cancel()

	err := store.DeleteBlock(ctx, "blk-1")

	if ctx.Err() != nil {
		t.Fatalf(
			"DeleteBlock ended only on the caller's deadline (err %v), want the call's deadline",
			err,
		)
	}

	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("DeleteBlock err = %v, want DeadlineExceeded from the call's deadline", err)
	}
}

func TestNew_BoundsEachCallAt30s(t *testing.T) {
	if got := New(nil, Config{Bucket: "b"}).callTimeout; got != 30*time.Second {
		t.Fatalf("New callTimeout = %v, want 30s", got)
	}
}

// The issue's scenario end to end: the orphan sweep that runs under the
// per-remote GC lock lists three aged orphans from a store whose every delete
// stalls. It must give the lock back after one call's deadline, with the other
// orphans left for the next run.
func TestReclaimOrphanObjects_StalledEndpointReleasesWithinOneDeadline(t *testing.T) {
	store, mock := newStallingStore(t, &stallingS3{holdDelete: true})
	seedAgedBlocks(mock, "blk-orphan-1", "blk-orphan-2", "blk-orphan-3")

	store.callTimeout = 100 * time.Millisecond

	ctx, cancel := context.WithTimeout(t.Context(), backstop)
	defer cancel()

	rep, err := blockgc.ReclaimOrphanObjects(ctx, map[string]struct{}{}, store,
		blockgc.ReclaimOptions{GracePeriod: time.Hour})

	if ctx.Err() != nil {
		t.Fatalf(
			"the sweep ended only on the caller's own deadline (err %v, %d errors), "+
				"want one call's deadline",
			err,
			rep.Errors,
		)
	}

	if !errors.Is(err, context.DeadlineExceeded) || rep.Errors != 1 {
		t.Fatalf(
			"the sweep returned err %v with %d errors, want DeadlineExceeded after 1 timeout",
			err,
			rep.Errors,
		)
	}

	if rep.RemoteObjectsScanned != 3 {
		t.Fatalf("RemoteObjectsScanned = %d, want 3", rep.RemoteObjectsScanned)
	}
}
