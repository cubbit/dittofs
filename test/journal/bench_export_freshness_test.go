//go:build linux

package journal

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestFreshnessFreshDirExitsClean(t *testing.T) {
	dir := t.TempDir()
	run := filepath.Join(dir, "run-now")
	if err := os.MkdirAll(run, 0o755); err != nil {
		t.Fatalf("MkdirAll %s: %v", run, err)
	}
	code, _, stderr := benchExport(t, nil, "--freshness", dir)
	if code != 0 {
		t.Errorf("--freshness with a run folder created now: exit status got %d, want 0 (stderr %q)", code, stderr)
	}
	if stderr != "" {
		// gh is never reached on this path, so even a machine with no gh must stay quiet.
		t.Errorf("--freshness with a run folder created now: stderr got %q, want it empty — gh is never required for a fresh collection", stderr)
	}
}

func TestFreshnessStaleDirRaisesAlertThroughGh(t *testing.T) {
	dir := t.TempDir()
	run := filepath.Join(dir, "run-old")
	if err := os.MkdirAll(run, 0o755); err != nil {
		t.Fatalf("MkdirAll %s: %v", run, err)
	}
	// Three nights back with BENCH_FRESHNESS_DAYS unset: the two-night default is what runs.
	stale := time.Now().Add(-3 * 24 * time.Hour)
	if err := os.Chtimes(run, stale, stale); err != nil {
		t.Fatalf("Chtimes %s: %v", run, err)
	}
	// The cut env leaves gh missing or unauthenticated, exactly as on a machine
	// that never logged in, and the refusal must name it either way.
	code, _, stderr := benchExport(t, nil, "--freshness", dir)
	if code != 2 {
		t.Errorf("--freshness with a 3-night-old run folder: exit status got %d, want 2", code)
	}
	if !strings.Contains(stderr, "gh") {
		t.Errorf("--freshness with a 3-night-old run folder: stderr got %q, want it to name \"gh\"", stderr)
	}
}

func TestFreshnessEmptyDirIsStale(t *testing.T) {
	dir := t.TempDir()
	code, _, stderr := benchExport(t, nil, "--freshness", dir)
	if code != 2 {
		t.Errorf("--freshness on an empty dir: exit status got %d, want 2 — a dead collection must never read as healthy", code)
	}
	if !strings.Contains(stderr, "gh") {
		t.Errorf("--freshness on an empty dir: stderr got %q, want it to name \"gh\"", stderr)
	}
}
