//go:build unix

package commands

import (
	"bufio"
	"context"
	"io"
	"net/http"
	"os/exec"
	"regexp"
	"strings"
	"syscall"
	"testing"
	"time"
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
