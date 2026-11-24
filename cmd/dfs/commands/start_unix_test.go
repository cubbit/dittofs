//go:build unix

package commands

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/metadata/kv/badgerkv"
	"github.com/cubbit/dittofs/pkg/metadata/kvstore"
)

// apiAddr matches the record naming the API's bound address.
var apiAddr = regexp.MustCompile(`node: listening api=(\S+)`)

// stopping is the record dfs start logs once Stop returned with every part
// stopped.
const stopping = "dfs: stopped"

// listening starts dfs start over the bootstrap at config and returns it
// with the API address its log names, the log so far, and the rest of its
// output, delivered once the child ends.
func listening(t *testing.T, config string) (cmd *exec.Cmd, addr, log string, rest <-chan string) {
	t.Helper()
	cmd, out := startChild(t, config)
	lines := bufio.NewScanner(out)
	var b strings.Builder
	for addr == "" && lines.Scan() {
		b.WriteString(lines.Text() + "\n")
		if m := apiAddr.FindStringSubmatch(lines.Text()); m != nil {
			addr = m[1]
		}
	}
	if addr == "" {
		t.Fatalf("dfs start logged no API address before it ended (%v):\n%s", lines.Err(), b.String())
	}
	ch := make(chan string, 1)
	go func() {
		tail, _ := io.ReadAll(out)
		ch <- string(tail)
	}()
	return cmd, addr, b.String(), ch
}

// Over an initialised installation, dfs start answers ready, logging the
// address it bound, and on SIGTERM stops and exits zero.
func TestStartServesUntilSIGTERM(t *testing.T) {
	serveUntilSIGTERM(t, installed(t, t.TempDir()))
}

// serveUntilSIGTERM runs dfs start over the bootstrap at config until it
// answers ready, then sends it SIGTERM and fails the test unless it stops
// and exits zero, and fails it if the stop is logged as a start the signal
// ended: the signal came after ready.
func serveUntilSIGTERM(t *testing.T, config string) {
	t.Helper()
	cmd, addr, log, rest := listening(t, config)
	// Readiness is polled until it answers ready; the child's bound ends a
	// child that never does, and the poll with it.
	for readiness(t, addr) != "ready" {
		time.Sleep(10 * time.Millisecond)
	}
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("SIGTERM: %v", err)
	}
	tail := <-rest
	if code := cmd.ProcessState.ExitCode(); code != 0 || !strings.Contains(tail, stopping) {
		t.Fatalf("dfs start on SIGTERM: exit %d, want 0 after logging %q:\n%s%s", code, stopping, log, tail)
	}
	if out := log + tail; strings.Contains(out, endedOnSignal) {
		t.Errorf("dfs start on SIGTERM after ready logged %q, want it only for a start the signal ended:\n%s", endedOnSignal, out)
	}
}

// endedOnSignal is the record dfs logs when the termination signal, not a
// failure of the start's own, ended Start.
const endedOnSignal = "dfs: the start ended on a termination signal"

// A SIGTERM that lands as Start begins ends the start on the signal, stops
// it and exits zero. The child signals itself at the start stage, so the
// signal lands before the node is ready at no timing.
func TestStartSIGTERMWhileStarting(t *testing.T) {
	code, out := runChild(t, installed(t, t.TempDir()), signalAtEnv+"="+stageStart)
	if code != 0 || !strings.Contains(out, endedOnSignal) || !strings.Contains(out, stopping) {
		t.Fatalf("dfs start on SIGTERM while starting: exit %d, want 0 after logging %q and %q:\n%s", code, endedOnSignal, stopping, out)
	}
}

// A SIGTERM during the start check ends the start on the signal and exits
// zero: nothing is open yet, so there is nothing to stop.
func TestStartSIGTERMDuringTheStartCheck(t *testing.T) {
	code, out := runChild(t, installed(t, t.TempDir()), signalAtEnv+"="+stagePreflight)
	if code != 0 || !strings.Contains(out, endedOnSignal) {
		t.Fatalf("dfs start on SIGTERM during the start check: exit %d, want 0 after logging %q:\n%s", code, endedOnSignal, out)
	}
}

// A start check that refuses the store for its own reason, here an
// uninitialised node, exits non-zero naming it though a SIGTERM arrived once
// the check returned: the signal did not end it.
func TestStartCheckRefusingUnderSIGTERMExitsNonZero(t *testing.T) {
	dir := t.TempDir()
	b := bootstrapFile{store: filepath.Join(dir, "missing"), journal: filepath.Join(dir, "device"), node: nodeUUID}
	code, out := runChild(t, b.write(t, dir), signalAtEnv+"="+stagePreflightReturned)
	if code == 0 || !strings.Contains(out, "Error: the start check: the node is not initialised") {
		t.Fatalf("dfs start with its check refusing under a SIGTERM: exit %d, output:\n%s\nwant non-zero naming the check's refusal", code, out)
	}
	if strings.Contains(out, endedOnSignal) {
		t.Errorf("dfs start with its check refusing under a SIGTERM logged %q, want the refusal not called the signal's:\n%s", endedOnSignal, out)
	}
}

// A start that fails for its own reason, its API's port already taken, exits
// non-zero naming the failure though a SIGTERM arrived before the command
// answered: the signal did not end it. The child signals itself once Start
// returned, so the signal lands at no timing.
func TestStartFailingUnderSIGTERMExitsNonZero(t *testing.T) {
	taken, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = taken.Close() })
	dir := t.TempDir()
	b := installation(t, dir)
	b.api = taken.Addr().String()
	code, out := runChild(t, b.write(t, dir), signalAtEnv+"="+stageStartReturned)
	if code == 0 || !strings.Contains(out, "dfs: stopping on a termination signal") ||
		!strings.Contains(out, "Error: starting the node: node: start step listeners:") {
		t.Fatalf("dfs start with its API port taken under a SIGTERM: exit %d, output:\n%s\nwant non-zero naming the listeners' failure after logging the signal", code, out)
	}
	if strings.Contains(out, endedOnSignal) {
		t.Errorf("dfs start with its API port taken under a SIGTERM logged %q, want the failure not called the signal's:\n%s", endedOnSignal, out)
	}
}

func readiness(t *testing.T, addr string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+addr+"/health/ready", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /health/ready on %s: %v", addr, err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return strings.TrimSpace(string(b))
}

// installedNoDevice is an installation with no journal device: the store
// initialized as installed leaves it, and a bootstrap that names no journal.
func installedNoDevice(t *testing.T) (store string, config string) {
	t.Helper()
	ctx := t.Context()
	dir := t.TempDir()
	store = filepath.Join(dir, "store")
	install := [16]byte{'s', 't', 'a', 'r', 't'}
	db, err := badgerkv.Open(badgerkv.Options{Dir: store, NodeID: testNode})
	if err != nil {
		t.Fatalf("open the KV: %v", err)
	}
	defer func() {
		if err := db.Close(); err != nil {
			t.Fatalf("close the KV: %v", err)
		}
	}()
	if err := kvstore.Init(ctx, db, install); err != nil {
		t.Fatalf("store init: %v", err)
	}
	return store, bootstrapFile{store: store, node: nodeUUID}.write(t, dir)
}

// dfs start over a bootstrap that names no journal device refuses naming it,
// whatever else it finds: the refusal reads no storage.
func TestStartRefusesNoJournalDevice(t *testing.T) {
	for _, tc := range []struct {
		name    string
		prepare func(t *testing.T) (store string, config string)
	}{
		{"without a store directory", func(t *testing.T) (string, string) {
			dir := t.TempDir()
			store := filepath.Join(dir, "store")
			return store, bootstrapFile{store: store, node: nodeUUID}.write(t, dir)
		}},
		{"over a store", func(t *testing.T) (string, string) {
			return installedNoDevice(t)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store, config := tc.prepare(t)
			code, out := runChild(t, config)
			// "the start check:" is the prefix only the check the run begins
			// with adds: node.New's own no-device refusal carries the words
			// but not the prefix, so this row fails if the call before
			// openKV ever goes.
			if code == 0 || !strings.Contains(out, "Error: the start check: no journal device") {
				t.Fatalf("dfs start with no journal device (%s): exit %d, output:\n%s\nwant non-zero naming the start check's refusal", tc.name, code, out)
			}
			if _, err := os.Stat(store); !errors.Is(err, os.ErrNotExist) && tc.name == "without a store directory" {
				t.Errorf("the store directory %s exists after the refusal (%v), want none: the refusal creates nothing", store, err)
			}
		})
	}
}
