package engine

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/block/journal/journaltest"
	remotememory "github.com/cubbit/dittofs/pkg/block/remote/memory"
	"github.com/cubbit/dittofs/pkg/block/remote/s3"
)

// probeTestConfig is a syncer config whose probes are bounded by bound and
// whose periodic probe runs every interval.
func probeTestConfig(bound, interval time.Duration) RemoteSyncConfig {
	cfg := DefaultConfig()
	cfg.HealthCheckTimeout = bound
	cfg.HealthCheckInterval = interval

	return cfg
}

// The first probe runs inside Start, and Start runs inside share creation and
// server startup. Without a bound of its own the probe waits as long as the
// remote client is willing to, which against a stalled endpoint is minutes.
func TestHealthMonitor_FirstProbeHasItsOwnDeadline(t *testing.T) {
	const bound = 50 * time.Millisecond
	rem := &probeHookRemote{
		RemoteStore: remotememory.New(),
		probe: func(ctx context.Context) error {
			deadline, ok := ctx.Deadline()
			if !ok {
				t.Errorf("probe ran without a deadline")

				return errors.New("probe ran without a deadline")
			}
			if left := time.Until(deadline); left > bound {
				t.Errorf("probe deadline is %v away, want at most %v", left, bound)
			}
			<-ctx.Done()

			return ctx.Err()
		},
	}
	m := NewRemoteSync(
		journaltest.New(t),
		rem,
		newStubFileChunkStore(),
		probeTestConfig(bound, time.Hour),
	)
	t.Cleanup(func() { _ = m.Close() })

	m.Start(context.Background())

	if m.IsRemoteHealthy() {
		t.Fatal("IsRemoteHealthy() = true after the first probe reached its deadline, want false")
	}
}

// The periodic probes run on the syncer's background context, which never
// ends while the share serves, so they need the same bound as the first one.
func TestHealthMonitor_PeriodicProbeHasItsOwnDeadline(t *testing.T) {
	entered := make(chan struct{}, 2)
	rem := &probeHookRemote{
		RemoteStore: remotememory.New(),
		probe: func(ctx context.Context) error {
			if _, ok := ctx.Deadline(); !ok {
				t.Errorf("probe ran without a deadline")
			}
			select {
			case entered <- struct{}{}:
			default:
			}

			return nil
		},
	}
	cfg := probeTestConfig(time.Second, time.Millisecond)
	m := NewRemoteSync(journaltest.New(t), rem, newStubFileChunkStore(), cfg)
	t.Cleanup(func() { _ = m.Close() })

	m.Start(context.Background())

	<-entered // the first probe, run by Start
	<-entered // a periodic probe, run by the monitor loop
}

// Reproduces a stalled object store: the endpoint accepts the connection and
// never answers. Start must come back on the probe's own bound, with the
// remote marked unhealthy, long before the caller's context ends.
func TestRemoteSyncStart_StalledS3EndpointDoesNotHoldStart(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(release) })

	store, err := s3.NewFromConfig(context.Background(), s3.Config{
		Bucket:         "stalled",
		Region:         "us-east-1",
		Endpoint:       srv.URL,
		AccessKey:      "test",
		SecretKey:      "test",
		ForcePathStyle: true,
		AllowPrivate:   true,
	})
	if err != nil {
		t.Fatalf("s3.NewFromConfig: %v", err)
	}
	cfg := probeTestConfig(100*time.Millisecond, time.Hour)
	m := NewRemoteSync(journaltest.New(t), store, newStubFileChunkStore(), cfg)
	t.Cleanup(func() { _ = m.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	m.Start(ctx)

	if ctx.Err() != nil {
		t.Fatal(
			"Start returned only when the caller's context ended: the probe has no bound of its own",
		)
	}
	if m.IsRemoteHealthy() {
		t.Fatal("IsRemoteHealthy() = true with a stalled endpoint, want false")
	}
}
