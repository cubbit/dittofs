package journal

import (
	"bytes"
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
	cmd := exec.Command("/bin/sh", append([]string{"bench-run.sh"}, args...)...)
	// go test runs this package in test/journal, so "." is where the script lives.
	cmd.Dir = "."
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
		exitErr, ok := err.(*exec.ExitError)
		if !ok {
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
