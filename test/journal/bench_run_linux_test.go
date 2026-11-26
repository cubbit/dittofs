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
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		_ = f.Close()
		t.Fatalf("Flock %s: %v", lockFile, err)
	}
	// The holder releases into a channel, and the cleanup joins it before
	// closing the file: the close and the unlock touch one descriptor from
	// one place, never concurrently.
	released := make(chan error, 1)
	go func() {
		time.Sleep(1500 * time.Millisecond)
		released <- syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	}()
	t.Cleanup(func() {
		if err := <-released; err != nil {
			t.Errorf("Flock %s unlock: %v", lockFile, err)
		}
		if err := f.Close(); err != nil {
			t.Errorf("Close %s: %v", lockFile, err)
		}
	})

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

// TestBenchRunQuietWaitEndsAtLatestStart pins the deadline check inside the
// quiet wait itself: the wait now runs under the host's lock, so it must
// never sit past the latest start even when its own budget is far away —
// the lock has to be free in time. The load never drops under the planted
// threshold, so only the in-loop deadline check can end the wait; the
// budget, set to an hour, would keep the loop going. The interval between
// the wait's checks is BENCH_QUIET_POLL, the input the test advances time
// with, so the case runs in seconds instead of sleeping real 300s ticks.
func TestBenchRunQuietWaitEndsAtLatestStart(t *testing.T) {
	root := t.TempDir()
	checkout := t.TempDir()
	// A load threshold of 0 against any real /proc/loadavg reading: the
	// machine's own load, whatever it is, keeps the loop looping.
	if load, err := os.ReadFile("/proc/loadavg"); err != nil || len(load) == 0 {
		t.Skip("no readable /proc/loadavg to plant a busy host")
	}
	env := map[string]string{
		"BENCH_RUN_ROOT":            root,
		"BENCH_CHECKOUT":            checkout,
		"BENCH_QUIET_LOAD":          "0",
		"BENCH_QUIET_LOAD_MAX_WAIT": "3600",
		"BENCH_QUIET_POLL":          "2",
		// Three seconds out: the wait's first tick passes it, the second
		// lands past it, so only the in-loop check can refuse.
		"BENCH_NOT_AFTER": strconv.FormatInt(time.Now().Add(3*time.Second).Unix(), 10),
	}
	code, _, stderr := benchRun(t, env, "small")
	if code != 2 {
		t.Errorf("bench-run.sh small with the quiet wait outliving the latest start: exit status got %d, want 2 (stderr %q)", code, stderr)
	}
	if !strings.Contains(stderr, "too late to start before the nightly work") {
		t.Errorf("bench-run.sh small with the quiet wait outliving the latest start: stderr got %q, want it to contain \"too late to start before the nightly work\" — the wait runs under the lock and must free it in time", stderr)
	}
	assertStillEmpty(t, root, "run root")
}
