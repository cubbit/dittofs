package commands

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/cubbit/dittofs/internal/cli/health"
	"github.com/cubbit/dittofs/pkg/config"
)

const (
	// daemonStartTimeout bounds how long `dfs start` waits for the background
	// server to answer /health. A server still alive at the deadline is reported
	// as started, with a note: a slow start (a large store being recovered) is
	// not a failure.
	daemonStartTimeout = 30 * time.Second

	// daemonStartGrace is how long the child must stay alive when /health cannot
	// be probed (the API is served over TLS), which only catches early exits.
	daemonStartGrace = 3 * time.Second

	daemonStartPoll = 100 * time.Millisecond

	// daemonLogTailLines is how many lines of the child's output are shown when
	// it exits before it is ready.
	daemonLogTailLines = 10
)

// daemonOutcome is how the wait for a freshly started background server ended.
type daemonOutcome int

const (
	daemonReady  daemonOutcome = iota // /health answered from the new process
	daemonAlive                       // still running when the wait ran out
	daemonExited                      // the process ended before it was ready
)

// waitForDaemon waits until ready reports true, the child exits, or timeout
// passes, whichever comes first. exited carries the result of cmd.Wait. A nil
// ready only waits for an early exit.
func waitForDaemon(exited <-chan error, ready func() bool, timeout, poll time.Duration) (daemonOutcome, error) {
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	tick := time.NewTicker(poll)
	defer tick.Stop()
	for {
		select {
		case err := <-exited:
			return daemonExited, err
		case <-deadline.C:
			return daemonAlive, nil
		case <-tick.C:
			if ready != nil && ready() {
				return daemonReady, nil
			}
		}
	}
}

// healthProbe returns a function that reports whether the API server of the
// process launched at `since` answers /health. A server that was already
// running on the same port answers too, so the reported started_at must not
// predate the launch; otherwise a port clash would look like a successful
// start. It returns nil when the API is served over TLS, where a plain probe
// cannot be made.
func healthProbe(since time.Time) func() bool {
	cfg, err := config.Load(GetConfigFile())
	if err != nil || cfg.ControlPlane.TLS.CertFile != "" {
		return nil
	}
	host := cfg.ControlPlane.Host
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	return healthReadyFunc("http://"+net.JoinHostPort(host, strconv.Itoa(cfg.ControlPlane.Port))+"/health", since)
}

// healthReadyFunc reports whether url answers 200 with a started_at not before
// since.
func healthReadyFunc(url string, since time.Time) func() bool {
	client := &http.Client{Timeout: time.Second}
	// started_at has one-second resolution.
	since = since.Truncate(time.Second)

	return func() bool {
		resp, err := client.Get(url)
		if err != nil {
			return false
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusOK {
			return false
		}
		var body health.Response
		if json.NewDecoder(resp.Body).Decode(&body) != nil {
			return false
		}
		startedAt, err := time.Parse(time.RFC3339, body.Data.StartedAt)
		return err == nil && !startedAt.Before(since)
	}
}

// awaitDaemonStart blocks until the background server `cmd` is up or has
// failed, and prints the outcome. It returns an error, so that `dfs start`
// exits non-zero, when the process exits first.
func awaitDaemonStart(cmd *exec.Cmd, pidPath, logPath string, logOffset int64, launched time.Time) error {
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()

	ready, timeout := healthProbe(launched), daemonStartTimeout
	if ready == nil {
		timeout = daemonStartGrace
	}

	outcome, waitErr := waitForDaemon(exited, ready, timeout, daemonStartPoll)
	if outcome == daemonExited {
		_ = os.Remove(pidPath)
		msg := "DittoFS server failed to start"
		if waitErr != nil {
			msg += fmt.Sprintf(" (%v)", waitErr)
		}
		if tail := readLogTail(logPath, logOffset, daemonLogTailLines); tail != "" {
			msg += ":\n" + tail
		}
		return fmt.Errorf("%s\nLog file: %s", msg, logPath)
	}

	fmt.Printf("DittoFS server started (PID: %d)\n", cmd.Process.Pid)
	fmt.Printf("Log file: %s\n", logPath)
	fmt.Printf("PID file: %s\n", pidPath)
	if outcome == daemonAlive && ready != nil {
		fmt.Fprintf(os.Stderr, "Warning: the server did not answer /health within %s; it is still starting. Check the log file.\n", timeout)
	}
	return nil
}

// readLogTail returns the last n lines written to path after offset, so output
// from earlier runs in an appended log is not mistaken for this one's.
func readLogTail(path string, offset int64, n int) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer func() { _ = f.Close() }()
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return ""
	}
	data, err := io.ReadAll(f)
	if err != nil {
		return ""
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.TrimSpace(strings.Join(lines, "\n"))
}
