//go:build linux

package journal

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// plantRun makes one finished run folder under dir with the age given: it
// holds the done marker a successful run writes, and the folder's mtime is
// stamped only after the marker exists, so it is what the comparator sees.
// Each case below names what it checks on the freshness threshold and
// nothing else.
func plantRun(t *testing.T, dir, name string, age time.Duration) {
	t.Helper()
	run := filepath.Join(dir, name)
	if err := os.MkdirAll(run, 0o755); err != nil {
		t.Fatalf("MkdirAll %s: %v", run, err)
	}
	if err := os.WriteFile(filepath.Join(run, "done"), nil, 0o644); err != nil {
		t.Fatalf("WriteFile %s: %v", filepath.Join(run, "done"), err)
	}
	stamp := time.Now().Add(-age)
	if err := os.Chtimes(run, stamp, stamp); err != nil {
		t.Fatalf("Chtimes %s: %v", run, err)
	}
}

func TestFreshnessFreshDirExitsClean(t *testing.T) {
	dir := t.TempDir()
	plantRun(t, dir, "run-now", 0)
	code, _, stderr := benchExport(t, nil, "--freshness", dir)
	if code != 0 {
		t.Errorf("--freshness with a run folder created now: exit status got %d, want 0 (stderr %q)", code, stderr)
	}
	if stderr != "" {
		// gh is never reached on this path, so even a machine with no gh must stay quiet.
		t.Errorf("--freshness with a run folder created now: stderr got %q, want it empty — gh is never required for a fresh collection", stderr)
	}
}

func TestFreshnessOneNightOldDirIsFresh(t *testing.T) {
	dir := t.TempDir()
	// One night back under the unset two-night default: younger than the
	// threshold, so the collection reads fresh.
	plantRun(t, dir, "run-1night", 24*time.Hour)
	code, _, stderr := benchExport(t, nil, "--freshness", dir)
	if code != 0 {
		t.Errorf("--freshness with a 1-night-old run folder under the 2-night default: exit status got %d, want 0 (stderr %q)", code, stderr)
	}
	if stderr != "" {
		t.Errorf("--freshness with a 1-night-old run folder under the 2-night default: stderr got %q, want it empty — gh is never required for a fresh collection", stderr)
	}
}

func TestFreshnessThreeNightsOldDirIsFreshAtFiveNights(t *testing.T) {
	dir := t.TempDir()
	// Three nights back read as fresh only through the env knob: the
	// threshold that runs is BENCH_FRESHNESS_DAYS=5, not the default.
	plantRun(t, dir, "run-3nights", 3*24*time.Hour)
	code, _, stderr := benchExport(t, map[string]string{"BENCH_FRESHNESS_DAYS": "5"}, "--freshness", dir)
	if code != 0 {
		t.Errorf("--freshness with a 3-night-old run folder under BENCH_FRESHNESS_DAYS=5: exit status got %d, want 0 (stderr %q)", code, stderr)
	}
	if stderr != "" {
		t.Errorf("--freshness with a 3-night-old run folder under BENCH_FRESHNESS_DAYS=5: stderr got %q, want it empty — gh is never required for a fresh collection", stderr)
	}
}

// TestFreshnessYoungRunWithoutDoneIsStale pins the every-night-fails
// collection: the only run folder is young but holds no done marker, because
// its nights wrote no metrics, so the collection reads stale — however many
// failing nights pass, freshness must keep reaching gh, never read healthy.
func TestFreshnessYoungRunWithoutDoneIsStale(t *testing.T) {
	dir := t.TempDir()
	run := filepath.Join(dir, "run-now-unfinished")
	if err := os.MkdirAll(run, 0o755); err != nil {
		t.Fatalf("MkdirAll %s: %v", run, err)
	}
	code, _, stderr := benchExport(t, nil, "--freshness", dir)
	if code != 2 {
		t.Errorf("--freshness with a young run folder without done: exit status got %d, want 2 — an unfinished night must never read as a fresh collection", code)
	}
	if !strings.Contains(stderr, "gh issue list failed") {
		t.Errorf("--freshness with a young run folder without done: stderr got %q, want it to contain \"gh issue list failed\"", stderr)
	}
}

// TestFreshnessOlderDoneRunReadsFresh proves the marker, not the folder
// mtime, judges the last good night: next to a younger unfinished folder, the
// run that finished a night ago is what freshness reads — and it reads fresh
// under the two-night default.
func TestFreshnessOlderDoneRunReadsFresh(t *testing.T) {
	dir := t.TempDir()
	younger := filepath.Join(dir, "run-now-unfinished")
	if err := os.MkdirAll(younger, 0o755); err != nil {
		t.Fatalf("MkdirAll %s: %v", younger, err)
	}
	plantRun(t, dir, "run-1night-finished", 24*time.Hour)
	code, _, stderr := benchExport(t, nil, "--freshness", dir)
	if code != 0 {
		t.Errorf("--freshness with a 1-night-old finished run next to a younger unfinished one: exit status got %d, want 0 (stderr %q)", code, stderr)
	}
	if stderr != "" {
		t.Errorf("--freshness with a 1-night-old finished run next to a younger unfinished one: stderr got %q, want it empty — gh is never required for a fresh collection", stderr)
	}
}

func TestFreshnessStaleDirReachesGh(t *testing.T) {
	dir := t.TempDir()
	// Three nights back with BENCH_FRESHNESS_DAYS unset: the two-night default
	// is what runs, and the folder is older than it.
	plantRun(t, dir, "run-3nights-old", 3*24*time.Hour)
	// The cut env leaves gh missing or unauthenticated, exactly as on a machine
	// that never logged in, and the refusal must name it either way.
	code, _, stderr := benchExport(t, nil, "--freshness", dir)
	if code != 2 {
		t.Errorf("--freshness with a 3-night-old run folder: exit status got %d, want 2", code)
	}
	if !strings.Contains(stderr, "gh issue list failed") {
		t.Errorf("--freshness with a 3-night-old run folder: stderr got %q, want it to contain \"gh issue list failed\"", stderr)
	}
}

func TestFreshnessEmptyDirIsStale(t *testing.T) {
	dir := t.TempDir()
	code, _, stderr := benchExport(t, nil, "--freshness", dir)
	if code != 2 {
		t.Errorf("--freshness on an empty dir: exit status got %d, want 2 — a dead collection must never read as healthy", code)
	}
	if !strings.Contains(stderr, "gh issue list failed") {
		t.Errorf("--freshness on an empty dir: stderr got %q, want it to contain \"gh issue list failed\"", stderr)
	}
}
