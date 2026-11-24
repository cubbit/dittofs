package journal

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// benchRun runs the script with env, from this package's directory, and returns
// exit status, stdout, stderr. It strips BENCH_RUN_ROOT from the inherited env
// unless the case sets it, so the guard is tested against a real inheritance.
func benchRun(t *testing.T, env map[string]string, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	// go test runs this package in test/journal, so "." is where the script lives.
	cmd := exec.Command("/bin/sh", append([]string{"bench-run.sh"}, args...)...)
	cmdEnv := make([]string, 0, len(os.Environ())+len(env))
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "BENCH_RUN_ROOT=") {
			continue
		}
		cmdEnv = append(cmdEnv, kv)
	}
	for k, v := range env {
		cmdEnv = append(cmdEnv, k+"="+v)
	}
	cmd.Env = cmdEnv
	var out, errBuf bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errBuf
	if err := cmd.Run(); err != nil {
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) {
			t.Fatalf("bench-run.sh %s: %v", strings.Join(args, " "), err)
		}
		code = exitErr.ExitCode()
	}
	return code, out.String(), errBuf.String()
}

func TestBenchRunUsage(t *testing.T) {
	cases := []struct {
		name string
		args []string
	}{
		{"no argument", nil},
		{"unknown word", []string{"bogus"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, _, stderr := benchRun(t, nil, tc.args...)
			if code != 2 {
				t.Errorf("bench-run.sh %s: exit status got %d, want 2", strings.Join(tc.args, " "), code)
			}
			if !strings.Contains(stderr, "usage: bench-run.sh") {
				t.Errorf("bench-run.sh %s: stderr got %q, want it to contain \"usage: bench-run.sh\"", strings.Join(tc.args, " "), stderr)
			}
		})
	}
}

func TestBenchRunRequiresRunRoot(t *testing.T) {
	for _, kind := range []string{"small", "full"} {
		t.Run(kind, func(t *testing.T) {
			code, _, stderr := benchRun(t, nil, kind)
			if code != 2 {
				t.Errorf("bench-run.sh %s without BENCH_RUN_ROOT: exit status got %d, want 2", kind, code)
			}
			if !strings.Contains(stderr, "set BENCH_RUN_ROOT") {
				t.Errorf("bench-run.sh %s without BENCH_RUN_ROOT: stderr got %q, want it to contain \"set BENCH_RUN_ROOT\"", kind, stderr)
			}
		})
	}
}

func TestBenchRunPruneRemovesOnlyOldRunFolders(t *testing.T) {
	root := t.TempDir()
	old := time.Now().Add(-40 * 24 * time.Hour)
	plant := func(t *testing.T, dir string, mtime time.Time) {
		t.Helper()
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("MkdirAll %s: %v", dir, err)
		}
		if err := os.Chtimes(dir, mtime, mtime); err != nil {
			t.Fatalf("Chtimes %s: %v", dir, err)
		}
	}
	plant(t, filepath.Join(root, "small", "young-a"), time.Now())
	plant(t, filepath.Join(root, "full", "young-b"), time.Now())
	plant(t, filepath.Join(root, "small", "old-a"), old)
	plant(t, filepath.Join(root, "full", "old-b"), old)
	// The dt results tree outside the pruned kinds: even 40 days old it must survive.
	plant(t, filepath.Join(root, ".dt-results", "old-c"), old)

	code, _, stderr := benchRun(t, map[string]string{"BENCH_RUN_ROOT": root}, "prune")
	if code != 0 {
		t.Errorf("prune: exit status got %d, want 0 (stderr %q)", code, stderr)
	}
	gone := []string{filepath.Join(root, "small", "old-a"), filepath.Join(root, "full", "old-b")}
	for _, dir := range gone {
		if _, err := os.Stat(dir); !os.IsNotExist(err) {
			t.Errorf("prune: %s still present, want it removed as older than 30 days", dir)
		}
	}
	present := []string{
		filepath.Join(root, "small", "young-a"),
		filepath.Join(root, "full", "young-b"),
		filepath.Join(root, ".dt-results", "old-c"),
	}
	for _, dir := range present {
		if _, err := os.Stat(dir); err != nil {
			t.Errorf("prune: %s not present, want it kept (err %v)", dir, err)
		}
	}
}

func TestBenchRunPruneRequiresRunRoot(t *testing.T) {
	code, _, stderr := benchRun(t, nil, "prune")
	if code != 2 {
		t.Errorf("prune without BENCH_RUN_ROOT: exit status got %d, want 2", code)
	}
	if !strings.Contains(stderr, "set BENCH_RUN_ROOT") {
		t.Errorf("prune without BENCH_RUN_ROOT: stderr got %q, want it to contain \"set BENCH_RUN_ROOT\"", stderr)
	}
}

func TestBenchRunPruneMissingKindDirs(t *testing.T) {
	root := t.TempDir()
	// A root other things live under, but neither pruned kind ever having run.
	resultsTree := filepath.Join(root, ".dt-results", "old-c")
	if err := os.MkdirAll(resultsTree, 0o755); err != nil {
		t.Fatalf("MkdirAll %s: %v", resultsTree, err)
	}
	code, _, stderr := benchRun(t, map[string]string{"BENCH_RUN_ROOT": root}, "prune")
	if code != 0 {
		t.Errorf("prune with neither small/ nor full/ present: exit status got %d, want 0 (stderr %q)", code, stderr)
	}
	if _, err := os.Stat(resultsTree); err != nil {
		t.Errorf("prune with neither small/ nor full/ present: .dt-results was touched, want it untouched (err %v)", err)
	}
}

func TestBenchRunPruneEmptyRunRoot(t *testing.T) {
	root := t.TempDir()
	code, _, stderr := benchRun(t, map[string]string{"BENCH_RUN_ROOT": root}, "prune")
	if code != 0 {
		t.Errorf("prune on an empty root: exit status got %d, want 0 (stderr %q)", code, stderr)
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("ReadDir %s: %v", root, err)
	}
	if len(entries) != 0 {
		t.Errorf("prune on an empty root: got %d entries under root, want 0 created by prune", len(entries))
	}
}

// benchExport runs bench-export.sh with the env cut to PATH, a fresh gh config
// dir and no token, plus whatever the case sets, so nothing ambient (an
// authenticated gh, a stray BENCH_EXPORT_DEST or BENCH_FRESHNESS_DAYS) can reach
// the script, and returns exit status, stdout, stderr. The freshness decision
// tests rely on gh failing in this env the way it fails on a machine that never
// logged in.
func benchExport(t *testing.T, env map[string]string, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	cmd := exec.Command("/bin/sh", append([]string{"bench-export.sh"}, args...)...)
	cmdEnv := []string{"PATH=/bin"}
	for k, v := range env {
		if k == "GH_TOKEN" || k == "GH_CONFIG_DIR" {
			continue
		}
		cmdEnv = append(cmdEnv, k+"="+v)
	}
	// A fresh gh config dir and no token make the subprocess unauthenticated
	// even where a merged-usr PATH finds a real gh binary: the refusal the
	// freshness tests assert on must come from the script, not from ambient
	// login. The env is filtered, not faked — gh itself still runs, if present.
	cmdEnv = append(cmdEnv, "GH_CONFIG_DIR="+t.TempDir())
	cmd.Env = cmdEnv
	var out, errBuf bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errBuf
	if err := cmd.Run(); err != nil {
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) {
			t.Fatalf("bench-export.sh %s: %v", strings.Join(args, " "), err)
		}
		code = exitErr.ExitCode()
	}
	return code, out.String(), errBuf.String()
}

func TestBenchExportUsage(t *testing.T) {
	code, _, stderr := benchExport(t, nil)
	if code != 2 {
		t.Errorf("bench-export.sh with no arguments: exit status got %d, want 2", code)
	}
	if !strings.Contains(stderr, "usage: bench-export.sh") {
		t.Errorf("bench-export.sh with no arguments: stderr got %q, want it to contain \"usage: bench-export.sh\"", stderr)
	}
}

func TestBenchExportRequiresDest(t *testing.T) {
	dir := t.TempDir()
	code, _, stderr := benchExport(t, nil, dir)
	if code != 2 {
		t.Errorf("bench-export.sh %s without BENCH_EXPORT_DEST: exit status got %d, want 2", dir, code)
	}
	if !strings.Contains(stderr, "set BENCH_EXPORT_DEST") {
		t.Errorf("bench-export.sh %s without BENCH_EXPORT_DEST: stderr got %q, want it to contain \"set BENCH_EXPORT_DEST\"", dir, stderr)
	}
}

func TestBenchExportMissingDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "absent")
	code, _, stderr := benchExport(t, map[string]string{"BENCH_EXPORT_DEST": "s3://bench-bucket/runs"}, dir)
	if code != 2 {
		t.Errorf("bench-export.sh %s with BENCH_EXPORT_DEST set: exit status got %d, want 2", dir, code)
	}
	if !strings.Contains(stderr, "no such run directory") {
		t.Errorf("bench-export.sh %s with BENCH_EXPORT_DEST set: stderr got %q, want it to contain \"no such run directory\"", dir, stderr)
	}
}
