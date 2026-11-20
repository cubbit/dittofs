//go:build unix

package commands

import (
	"bufio"
	"context"
	"errors"
	"io"
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

// listening starts dfs start over an installation under t.TempDir() and
// returns it with the API address its log names, the log so far, and the
// rest of its output, delivered once the child ends.
func listening(t *testing.T) (cmd *exec.Cmd, addr, log string, rest <-chan string) {
	t.Helper()
	cmd, out := startChild(t, installed(t, t.TempDir()))
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
	cmd, addr, log, rest := listening(t)
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
}

// A SIGTERM before the node is ready, while Start runs, stops it and exits
// zero.
func TestStartSIGTERMWhileStarting(t *testing.T) {
	cmd, _, log, rest := listening(t)
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("SIGTERM: %v", err)
	}
	tail := <-rest
	if code := cmd.ProcessState.ExitCode(); code != 0 || !strings.Contains(tail, stopping) {
		t.Fatalf("dfs start on SIGTERM while starting: exit %d, want 0 after logging %q:\n%s%s", code, stopping, log, tail)
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

// The extra root of the start check is `blocks` beside the store directory
// when the store's path is absolute and its parent is below the root of the
// filesystem, and otherwise the first device, or none without a device: never
// an empty or relative root, which the check would read from the working
// directory, and never the filesystem's own `/blocks`.
func TestV1ProbeRoot(t *testing.T) {
	devices := []string{"/dev/one", "/dev/two"}

	for _, tc := range []struct {
		name    string
		store   string
		devices []string
		want    string
	}{
		{"its default place", "/state/dittofs/metadata", devices, "/state/dittofs/blocks"},
		{"a store path not clean", "/data/x/../meta/", devices, "/data/blocks"},
		{"a store one level below the root", "/x", devices, "/dev/one"},
		{"a store at the root", "/", devices, "/dev/one"},
		{"a relative store", "meta/store", devices, "/dev/one"},
		{"no store path", "", devices, "/dev/one"},
		{"no store path and no device", "", nil, ""},
		{"a relative store and no device", "meta", nil, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := v1ProbeRoot(tc.store, tc.devices); got != tc.want {
				t.Errorf("v1ProbeRoot(%q, %q): got %q, want %q", tc.store, tc.devices, got, tc.want)
			}
		})
	}
}
