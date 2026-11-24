package journal

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// benchRun runs the script with env, from this package's directory, and returns
// exit status, stdout, stderr. It strips the BENCH_* variables from the
// inherited env unless the case sets them, so each guard is tested against a
// real inheritance.
func benchRun(t *testing.T, env map[string]string, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	// go test runs this package in test/journal, so "." is where the script lives.
	cmd := exec.Command("/bin/sh", append([]string{"bench-run.sh"}, args...)...)
	cmdEnv := make([]string, 0, len(os.Environ())+len(env))
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "BENCH_") {
			continue
		}
		cmdEnv = append(cmdEnv, kv)
	}
	for k, v := range env {
		cmdEnv = append(cmdEnv, k+"="+v)
	}
	// git must never find the repository this test runs in: if the script's
	// cd into BENCH_CHECKOUT ever regresses, its git steps run here.
	wd, _ := os.Getwd()
	cmdEnv = append(cmdEnv, "GIT_CEILING_DIRECTORIES="+filepath.Dir(wd))
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

// assertStillEmpty fails when dir holds any entry, naming what was expected
// to stay empty and how many entries appeared instead.
func assertStillEmpty(t *testing.T, dir, what string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir %s: %v", dir, err)
	}
	if len(entries) != 0 {
		t.Errorf("%s after the refused run: got %d entries, want it left empty", what, len(entries))
	}
}

// TestBenchRunSmallGuards walks the small kind's guard chain in order: each
// case is a fully configured run minus one guard, so the run's exit names
// the guard that was missing, and nothing is created anywhere while refusing.
func TestBenchRunSmallGuards(t *testing.T) {
	root := t.TempDir()
	checkout := t.TempDir()
	notAfterAhead := strconv.FormatInt(time.Now().Add(time.Hour).Unix(), 10)
	notAfterPast := strconv.FormatInt(time.Now().Add(-time.Minute).Unix(), 10)
	cases := []struct {
		name string
		env  map[string]string
		want string
	}{
		{
			name: "BENCH_NOT_AFTER unset",
			env:  map[string]string{"BENCH_RUN_ROOT": root},
			want: "set BENCH_NOT_AFTER",
		},
		{
			name: "BENCH_CHECKOUT unset",
			env:  map[string]string{"BENCH_RUN_ROOT": root, "BENCH_NOT_AFTER": notAfterAhead},
			want: "set BENCH_CHECKOUT",
		},
		{
			name: "BENCH_CHECKOUT directory missing",
			env: map[string]string{
				"BENCH_RUN_ROOT":  root,
				"BENCH_NOT_AFTER": notAfterAhead,
				"BENCH_CHECKOUT":  filepath.Join(t.TempDir(), "absent"),
			},
			want: "no such checkout directory",
		},
		{
			// The pre-wait check: a past deadline refuses at once, without
			// sitting in the quiet wait or touching either directory.
			name: "start deadline in the past",
			env: map[string]string{
				"BENCH_RUN_ROOT":  root,
				"BENCH_NOT_AFTER": notAfterPast,
				"BENCH_CHECKOUT":  checkout,
			},
			want: "too late to start before the nightly work",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, _, stderr := benchRun(t, tc.env, "small")
			if code != 2 {
				t.Errorf("bench-run.sh small with %s: exit status got %d, want 2", tc.name, code)
			}
			if !strings.Contains(stderr, tc.want) {
				t.Errorf("bench-run.sh small with %s: stderr got %q, want it to contain %q", tc.name, stderr, tc.want)
			}
			assertStillEmpty(t, root, "run root")
			assertStillEmpty(t, checkout, "checkout")
		})
	}
}

// TestBenchRunSmallReachesGitInsideCheckout proves the guard chain lets a
// legitimate configured run proceed past every guard: it enters the named
// checkout and dies at its git fetch, creating nothing under the run root or
// the checkout. The quiet wait never starts (the threshold sits far above
// any load; one loop iteration would sleep 300s) and the lock is unset.
func TestBenchRunSmallReachesGitInsideCheckout(t *testing.T) {
	root := t.TempDir()
	checkout := t.TempDir()
	env := map[string]string{
		"BENCH_RUN_ROOT":   root,
		"BENCH_NOT_AFTER":  strconv.FormatInt(time.Now().Add(time.Hour).Unix(), 10),
		"BENCH_CHECKOUT":   checkout,
		"BENCH_QUIET_LOAD": "99999",
		// A safety net for the threshold above: if the wait is ever entered
		// anyway, its budget is one second, not the default four hours.
		"BENCH_QUIET_LOAD_MAX_WAIT": "1",
	}
	code, _, stderr := benchRun(t, env, "small")
	if code != 2 {
		t.Errorf("bench-run.sh small with every guard satisfied: exit status got %d, want 2 (stderr %q)", code, stderr)
	}
	if !strings.Contains(stderr, "git fetch failed") {
		t.Errorf("bench-run.sh small with every guard satisfied: stderr got %q, want it to contain \"git fetch failed\"", stderr)
	}
	assertStillEmpty(t, root, "run root")
	assertStillEmpty(t, checkout, "checkout")
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
	plant(t, filepath.Join(root, ".dt-results", "old-c"), old)
	plant(t, filepath.Join(root, ".dt-results", "young-c"), time.Now())
	// An arbitrary other root a hand run left under the run root: even 40
	// days old it must survive.
	plant(t, filepath.Join(root, "manual", "old-d"), old)

	code, _, stderr := benchRun(t, map[string]string{"BENCH_RUN_ROOT": root}, "prune")
	if code != 0 {
		t.Errorf("prune: exit status got %d, want 0 (stderr %q)", code, stderr)
	}
	gone := []string{
		filepath.Join(root, "small", "old-a"),
		filepath.Join(root, "full", "old-b"),
		filepath.Join(root, ".dt-results", "old-c"),
	}
	for _, dir := range gone {
		if _, err := os.Stat(dir); !os.IsNotExist(err) {
			t.Errorf("prune: %s still present, want it removed as older than 30 days", dir)
		}
	}
	present := []string{
		filepath.Join(root, "small", "young-a"),
		filepath.Join(root, "full", "young-b"),
		filepath.Join(root, ".dt-results", "young-c"),
		filepath.Join(root, "manual", "old-d"),
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
	manualDir := filepath.Join(root, "manual", "old-c")
	if err := os.MkdirAll(manualDir, 0o755); err != nil {
		t.Fatalf("MkdirAll %s: %v", manualDir, err)
	}
	code, _, stderr := benchRun(t, map[string]string{"BENCH_RUN_ROOT": root}, "prune")
	if code != 0 {
		t.Errorf("prune with neither small/ nor full/ present: exit status got %d, want 0 (stderr %q)", code, stderr)
	}
	if _, err := os.Stat(manualDir); err != nil {
		t.Errorf("prune with neither small/ nor full/ present: manual/ was touched, want it untouched (err %v)", err)
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
