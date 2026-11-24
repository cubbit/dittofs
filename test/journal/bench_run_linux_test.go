//go:build linux

package journal

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestBenchRunDeadlinePassesDuringLockWait pins the post-waits deadline check
// on its own: the pre-wait check has passed and the lock is taken, but the
// wait eats past BENCH_NOT_AFTER, so only the second check can refuse the
// run. The lock is held 1.5s — past the 1s deadline, shorter than the 2s
// wait — then released, so the script acquires it and must be refused by the
// check after the waits, not by the lock branch or the first check. The
// script has no injectable clock, so the holder's hold time is real time;
// this sleep is the scenario itself, not a wait for a goroutine.
func TestBenchRunDeadlinePassesDuringLockWait(t *testing.T) {
	root := t.TempDir()
	checkout := t.TempDir()
	lockFile := filepath.Join(t.TempDir(), "bench.lock")

	f, err := os.OpenFile(lockFile, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		t.Fatalf("OpenFile %s: %v", lockFile, err)
	}
	t.Cleanup(func() { f.Close() })
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatalf("Flock %s: %v", lockFile, err)
	}
	go func() {
		time.Sleep(1500 * time.Millisecond)
		syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	}()

	env := map[string]string{
		"BENCH_RUN_ROOT":   root,
		"BENCH_CHECKOUT":   checkout,
		"BENCH_QUIET_LOAD": "99999",
		"BENCH_LOCK":       lockFile,
		"BENCH_LOCK_WAIT":  "2",
		// One second out: the run may still start when the script begins,
		// but not once the lock wait has burned its 1.5s.
		"BENCH_NOT_AFTER": strconv.FormatInt(time.Now().Add(time.Second).Unix(), 10),
	}
	code, _, stderr := benchRun(t, env, "small")
	if code != 2 {
		t.Errorf("bench-run.sh small with the deadline passing during the lock wait: exit status got %d, want 2 (stderr %q)", code, stderr)
	}
	if !strings.Contains(stderr, "too late to start before the nightly work") {
		t.Errorf("bench-run.sh small with the deadline passing during the lock wait: stderr got %q, want it to contain \"too late to start before the nightly work\"", stderr)
	}
	assertStillEmpty(t, root, "run root")
}
