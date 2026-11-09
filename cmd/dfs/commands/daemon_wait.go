package commands

import (
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/cubbit/dittofs/internal/cli/health"
	"github.com/cubbit/dittofs/pkg/config"
	"github.com/cubbit/dittofs/pkg/controlplane/api"
)

const (
	// daemonStartTimeout bounds how long `dfs start` waits for the background
	// server to answer /health. A server still alive at the deadline is reported
	// as started, with a note: a slow start (a large store being recovered) is
	// not a failure.
	daemonStartTimeout = 30 * time.Second

	// daemonStartGrace is how long the child must stay alive when /health cannot
	// be probed (the API requires a client certificate), which only catches
	// early exits.
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
// process being launched now answers /health. A server that was already
// running on the same port answers too, so the reported started_at must not
// predate the launch; otherwise a port clash would look like a successful
// start.
//
// ponytail: an API that requires a client certificate (mTLS) is not probed, so
// nil is returned and the caller falls back to the short liveness grace. A
// probe would need the operator's client certificate; withdraw this when
// `dfs start` has one to present.
func healthProbe() func() bool {
	launched := time.Now()
	cfg, err := config.Load(GetConfigFile())
	if err != nil || cfg.ControlPlane.TLS.ClientCA != "" {
		return nil
	}
	scheme := "http"
	client := &http.Client{Timeout: time.Second}
	if cfg.ControlPlane.TLS.Enabled() {
		scheme = "https"
		// The probe sends no credentials and reads only started_at, so it does
		// not need to verify the server certificate (it may be self-signed or
		// issued for another name than localhost).
		client.Transport = &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}} //nolint:gosec // see above
	}
	addr := api.DisplayAddr(cfg.ControlPlane.Host, cfg.ControlPlane.Port)
	return healthReadyFunc(client, scheme+"://"+addr+"/health", launched)
}

// healthReadyFunc reports whether url answers 200 with a started_at that is
// not before since.
func healthReadyFunc(client *http.Client, url string, since time.Time) func() bool {
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

// awaitDaemonStart blocks until the background server `cmd` is ready or has
// failed. ready is the readiness probe, or nil to only watch for an early exit.
// log is a read handle on the file the child writes to, positioned where this
// run's output begins. When the child exits first, the PID file is removed, the
// tail of the log is written to stderr and the returned *exitStatus carries the
// child's exit code, so that `dfs start` exits with it.
func awaitDaemonStart(cmd *exec.Cmd, ready func() bool, pidPath, logPath string, log *os.File, stderr io.Writer) error {
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()

	timeout := daemonStartTimeout
	if ready == nil {
		timeout = daemonStartGrace
	}

	outcome, waitErr := waitForDaemon(exited, ready, timeout, daemonStartPoll)
	if outcome == daemonExited {
		_ = os.Remove(pidPath)
		code := 1
		var exitErr *exec.ExitError
		if errors.As(waitErr, &exitErr) && exitErr.ExitCode() > 0 {
			code = exitErr.ExitCode()
		}
		msg := "DittoFS server failed to start"
		if waitErr != nil {
			msg += fmt.Sprintf(" (%v)", waitErr)
		}
		if tail := readLogTail(log, daemonLogTailLines); tail != "" {
			msg += ":\n" + tail
		}
		_, _ = fmt.Fprintf(stderr, "Error: %s\nLog file: %s\n", msg, logPath)
		return &exitStatus{code: code}
	}

	// decision: a server that is alive but has not answered /health when the
	// wait runs out is not reported as failed. A large store can legitimately
	// take longer than this to open, and failing the command would make
	// supervisors retry a start that is still in progress. The message says
	// "still starting" instead of "started", so nobody reads it as ready. Withdraw
	// this if slow starts that later die turn out to be common.
	if outcome == daemonAlive && ready != nil {
		fmt.Printf("DittoFS server is still starting (PID: %d); it did not answer /health within %s\n", cmd.Process.Pid, timeout)
	} else {
		fmt.Printf("DittoFS server started (PID: %d)\n", cmd.Process.Pid)
	}
	fmt.Printf("Log file: %s\n", logPath)
	fmt.Printf("PID file: %s\n", pidPath)
	return nil
}

// readLogTail returns the last n lines available from the current position of
// log. Reading through the handle opened before the child started, not the
// path, keeps the tail correct if the log is rotated during startup, and the
// position keeps output from earlier runs out of it.
func readLogTail(log *os.File, n int) string {
	data, err := io.ReadAll(log)
	if err != nil {
		return ""
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.TrimSpace(strings.Join(lines, "\n"))
}
