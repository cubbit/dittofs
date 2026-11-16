package engine

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/cubbit/dittofs/pkg/block/remote"
	remotememory "github.com/cubbit/dittofs/pkg/block/remote/memory"
	"github.com/cubbit/dittofs/pkg/block/remote/s3"
)

var errPutRefused = errors.New("put refused")

// putRefusingRemote is the memory remote with every PutBlock refused while
// refuse is set, with refuseWith or errPutRefused. Its health probe still
// passes, which is the case to cover: a store that answers its probe and
// refuses every upload.
type putRefusingRemote struct {
	*remotememory.Store
	refuse     atomic.Bool
	refuseWith error
}

func (r *putRefusingRemote) PutBlock(ctx context.Context, blockID string, body io.Reader) error {
	if r.refuse.Load() {
		if r.refuseWith != nil {
			return r.refuseWith
		}

		return errPutRefused
	}

	return r.Store.PutBlock(ctx, blockID, body)
}

// newUploadsFailingEngine wires rem behind a manual-sync carve fixture and an
// engine over it, so each Flush uploads the fixture's dirty bytes as one block
// and the engine's stats are the ones an operator reads.
func newUploadsFailingEngine(t *testing.T, rem remote.RemoteStore) (*Store, *carveFixture) {
	t.Helper()
	fx := newCarveFixture(t, rem, defaultTestCarveBlockSize)
	bs, err := New(BlockStoreConfig{
		Local:           fx.local,
		Remote:          rem,
		RemoteSync:      fx.syncer,
		FileChunkStore:  fx.ms,
		SyncedHashStore: fx.ms,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = bs.Close() })

	return bs, fx
}

// failPuts runs n Flush calls that each fail on a refused upload.
func failPuts(t *testing.T, fx *carveFixture, n int, want error) {
	t.Helper()
	for range n {
		if _, err := fx.syncer.Flush(context.Background(), carveFixturePayload); !errors.Is(
			err,
			want,
		) {
			t.Fatalf("Flush() = %v, want %v", err, want)
		}
	}
}

func TestStats_UploadsFailingAfterThreeFailedPuts(t *testing.T) {
	rem := &putRefusingRemote{Store: remotememory.New()}
	rem.refuse.Store(true)
	bs, fx := newUploadsFailingEngine(t, rem)
	fx.storeChunk(t, context.Background(), []byte("bytes the remote refuses"))

	failPuts(t, fx, 2, errPutRefused)
	st := bs.GetStatsLite()
	if st.UploadsFailing || st.ConsecutiveUploadFailures != 2 {
		t.Fatalf("after 2 failed puts: UploadsFailing = %v, ConsecutiveUploadFailures = %d, "+
			"want false, 2", st.UploadsFailing, st.ConsecutiveUploadFailures)
	}

	failPuts(t, fx, 1, errPutRefused)
	st = bs.GetStatsLite()
	if !st.UploadsFailing || st.ConsecutiveUploadFailures != 3 {
		t.Fatalf("after 3 failed puts: UploadsFailing = %v, ConsecutiveUploadFailures = %d, "+
			"want true, 3", st.UploadsFailing, st.ConsecutiveUploadFailures)
	}
	if !st.RemoteHealthy {
		t.Fatal("RemoteHealthy = false, want true: failed uploads must not touch the health flag")
	}
}

func TestStats_UploadsFailingClearsOnFirstGoodPut(t *testing.T) {
	rem := &putRefusingRemote{Store: remotememory.New()}
	rem.refuse.Store(true)
	bs, fx := newUploadsFailingEngine(t, rem)
	fx.storeChunk(t, context.Background(), []byte("bytes the remote refuses, then takes"))
	failPuts(t, fx, 3, errPutRefused)

	rem.refuse.Store(false)
	if _, err := fx.syncer.Flush(context.Background(), carveFixturePayload); err != nil {
		t.Fatalf("Flush() after the remote takes puts again: %v", err)
	}

	st := bs.GetStatsLite()
	if st.UploadsFailing || st.ConsecutiveUploadFailures != 0 {
		t.Fatalf("after a successful put: UploadsFailing = %v, ConsecutiveUploadFailures = %d, "+
			"want false, 0", st.UploadsFailing, st.ConsecutiveUploadFailures)
	}
}

// A cancelled upload is the syncer stopping, not the remote failing, so a
// shutdown in the middle of uploads must not read as failing uploads.
func TestStats_CancelledPutsDoNotCount(t *testing.T) {
	rem := &putRefusingRemote{Store: remotememory.New(), refuseWith: context.Canceled}
	rem.refuse.Store(true)
	bs, fx := newUploadsFailingEngine(t, rem)
	fx.storeChunk(t, context.Background(), []byte("bytes whose upload is cancelled"))

	failPuts(t, fx, 3, context.Canceled)

	st := bs.GetStatsLite()
	if st.UploadsFailing || st.ConsecutiveUploadFailures != 0 {
		t.Fatalf("after 3 cancelled puts: UploadsFailing = %v, ConsecutiveUploadFailures = %d, "+
			"want false, 0", st.UploadsFailing, st.ConsecutiveUploadFailures)
	}
}

// Reproduces a throttled object store: HeadBucket answers 200 and every
// PutObject answers 503 SlowDown. The probe keeps the remote healthy, so the
// stats are the only place the failing uploads can show.
func TestStats_UploadsFailingWithS3PutRefused(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut {
			w.Header().Set("Content-Type", "application/xml")
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = io.WriteString(
				w,
				"<Error><Code>SlowDown</Code><Message>slow down</Message></Error>",
			)

			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	store, err := s3.NewFromConfig(context.Background(), s3.Config{
		Bucket:         "throttled",
		Region:         "us-east-1",
		Endpoint:       srv.URL,
		AccessKey:      "test",
		SecretKey:      "test",
		ForcePathStyle: true,
		AllowPrivate:   true,
		MaxRetries:     1,
	})
	if err != nil {
		t.Fatalf("s3.NewFromConfig: %v", err)
	}
	bs, fx := newUploadsFailingEngine(t, store)
	if err := bs.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	fx.storeChunk(t, context.Background(), []byte("bytes the throttled remote refuses"))

	for range 3 {
		if _, err := fx.syncer.Flush(context.Background(), carveFixturePayload); err == nil {
			t.Fatal("Flush() = nil with every PUT refused, want an error")
		}
	}

	st := bs.GetStatsLite()
	if !st.UploadsFailing {
		t.Fatal("UploadsFailing = false with every PUT refused, want true")
	}
	if !st.RemoteHealthy {
		t.Fatal("RemoteHealthy = false with HeadBucket answering 200, want true")
	}
}
