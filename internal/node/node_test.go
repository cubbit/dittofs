package node_test

import (
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/cubbit/dittofs/internal/node"
	"github.com/cubbit/dittofs/internal/node/nodetest"
	"github.com/cubbit/dittofs/internal/prof"
	"github.com/cubbit/dittofs/pkg/metadata/kv"
	"github.com/cubbit/dittofs/pkg/metadata/kv/badgerkv"
	"github.com/cubbit/dittofs/pkg/metadata/kvstore"
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
	_, err = node.New(node.Config{KV: db, Clock: sim.NewTestClock(time.Unix(0, 0)), Logger: slog.New(slog.NewTextHandler(t.Output(), nil))})
	if !errors.Is(err, kvstore.ErrNotInitialised) {
		t.Fatalf("New over an empty KV: got %v, want ErrNotInitialised", err)
	}
	if err := db.View(t.Context(), func(kv.Reader) error { return nil }); err == nil {
		t.Error("the KV after a refused New: a View succeeded, want it closed")
	}
}
