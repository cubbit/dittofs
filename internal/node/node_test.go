package node_test

import (
	"errors"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/cubbit/dittofs/internal/node"
	"github.com/cubbit/dittofs/internal/node/nodetest"
	"github.com/cubbit/dittofs/internal/prof"
	"github.com/cubbit/dittofs/pkg/bootstrap"
	"github.com/cubbit/dittofs/pkg/metadata/kv"
	"github.com/cubbit/dittofs/pkg/metadata/kv/badgerkv"
	"github.com/cubbit/dittofs/pkg/metadata/kvstore"
	"github.com/cubbit/dittofs/pkg/metrics"
	"github.com/cubbit/dittofs/pkg/sim"
)

// New starts nothing: a node built and stopped without a Start leaves no
// goroutine, descriptor or heap behind, so no journal was opened and no
// component started.
func TestNewStartsNothing(t *testing.T) {
	h := nodetest.New(t, nodetest.Watch())
	if err := h.Node().Stop(t.Context()); err != nil {
		t.Fatalf("Stop after New: got %v, want nil", err)
	}
}

// New over a KV holding no installation refuses, and closes the KV it was
// handed: the watch finds nothing of it left open.
func TestNewRefusesAKVWithNoInstallation(t *testing.T) {
	prof.Watch(t, prof.Options{})
	db, err := badgerkv.Open(badgerkv.Options{})
	if err != nil {
		t.Fatal(err)
	}

	_, err = node.New(
		node.Config{
			Bootstrap: bootstrap.Bootstrap{APIAddr: "127.0.0.1:0", MetricsAddr: "127.0.0.1:0"},
			KV:        db,
			Clock:     sim.NewTestClock(time.Unix(0, 0)),
			Metrics:   metrics.New("test", "test"),
			Logger:    slog.New(slog.NewTextHandler(t.Output(), nil)),
		},
	)
	if !errors.Is(err, kvstore.ErrNotInitialised) {
		t.Fatalf("New over an empty KV: got %v, want ErrNotInitialised", err)
	}
	if err := db.View(t.Context(), func(kv.Reader) error { return nil }); err == nil {
		t.Error("the KV after a refused New: a View succeeded, want it closed")
	}
}

// New refuses an empty API or metrics address, which would bind a random
// port on every interface, and closes the KV it was handed.
func TestNewRefusesAnEmptyAddress(t *testing.T) {
	for _, tc := range []struct {
		name         string
		api, metrics string
	}{
		{"api", "", "127.0.0.1:0"},
		{"metrics", "127.0.0.1:0", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, err := badgerkv.Open(badgerkv.Options{})
			if err != nil {
				t.Fatal(err)
			}
			_, err = node.New(node.Config{
				Bootstrap: bootstrap.Bootstrap{APIAddr: tc.api, MetricsAddr: tc.metrics},
				KV:        db, Clock: sim.NewTestClock(time.Unix(0, 0)), Metrics: metrics.New("test", "test"),
				Logger: slog.New(slog.NewTextHandler(t.Output(), nil)),
			})
			if err == nil || !strings.Contains(err.Error(), "APIAddr and MetricsAddr must both be set") {
				t.Errorf("New with %s address empty: got %v, want refused for the empty address", tc.name, err)
			}
			if err := db.View(t.Context(), func(kv.Reader) error { return nil }); err == nil {
				t.Error("the KV after a refused New: a View succeeded, want it closed")
			}
		})
	}
}

// The engine's series are in the registry the node was handed from New on,
// so a scrape answers them before anything starts.
func TestEngineSeriesRegistered(t *testing.T) {
	h := nodetest.New(t)
	mfs, err := h.Metrics().Registry().Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	var got []string
	for _, mf := range mfs {
		got = append(got, mf.GetName())
	}
	for _, want := range []string{"dittofs_engine_self_fences_total", "dittofs_engine_stability_points_total", "dittofs_engine_pending_existence_ops", "dittofs_engine_read_seconds"} {
		if !slices.Contains(got, want) {
			t.Errorf("series after New: %s missing from %v", want, got)
		}
	}
}
