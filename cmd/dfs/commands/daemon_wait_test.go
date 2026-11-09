package commands

import (
	"bytes"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestWaitForDaemon(t *testing.T) {
	t.Run("reports ready as soon as the probe passes", func(t *testing.T) {
		exited := make(chan error, 1)
		calls := 0
		ready := func() bool { calls++; return calls == 3 }
		got, err := waitForDaemon(exited, ready, time.Second, time.Millisecond)
		if got != daemonReady || err != nil {
			t.Fatalf("got (%v, %v), want (daemonReady, nil)", got, err)
		}
	})

	t.Run("reports an exit before ready", func(t *testing.T) {
		exited := make(chan error, 1)
		exited <- errors.New("exit status 1")
		got, err := waitForDaemon(exited, func() bool { return false }, time.Second, time.Hour)
		if got != daemonExited || err == nil {
			t.Fatalf("got (%v, %v), want (daemonExited, exit status 1)", got, err)
		}
	})

	t.Run("a process still alive at the deadline is not a failure", func(t *testing.T) {
		exited := make(chan error, 1)
		got, err := waitForDaemon(exited, nil, 20*time.Millisecond, time.Millisecond)
		if got != daemonAlive || err != nil {
			t.Fatalf("got (%v, %v), want (daemonAlive, nil)", got, err)
		}
	})
}

func TestHealthReadyFunc(t *testing.T) {
	launched := time.Date(2026, 1, 2, 3, 4, 5, 700_000_000, time.UTC)

	serve := func(status int, startedAt string) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(status)
			_, _ = fmt.Fprintf(w, `{"status":"healthy","data":{"started_at":%q}}`, startedAt)
		}))
	}

	tests := []struct {
		name      string
		status    int
		startedAt string
		want      bool
	}{
		{"server started after the launch", 200, "2026-01-02T03:04:06Z", true},
		{"server started in the same second as the launch", 200, "2026-01-02T03:04:05Z", true},
		// A server already bound to the port answers /health, but it is not the
		// one that was just launched.
		{"server that was already running", 200, "2026-01-02T03:00:00Z", false},
		{"not ready", 503, "2026-01-02T03:04:06Z", false},
		{"no started_at", 200, "", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := serve(tc.status, tc.startedAt)
			defer srv.Close()
			if got := healthReadyFunc(srv.Client(), srv.URL, launched)(); got != tc.want {
				t.Fatalf("ready = %v, want %v", got, tc.want)
			}
		})
	}

	t.Run("nothing listening", func(t *testing.T) {
		srv := serve(200, "")
		url := srv.URL
		srv.Close()
		if healthReadyFunc(http.DefaultClient, url, launched)() {
			t.Fatal("ready with no server listening")
		}
	})
}

func TestHealthReadyFunc_TLS(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, `{"status":"healthy","data":{"started_at":"2026-01-02T03:04:06Z"}}`)
	}))
	defer srv.Close()
	launched := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

	// The probe skips certificate verification, as healthProbe's client does.
	insecure := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}} //nolint:gosec // test
	if !healthReadyFunc(insecure, srv.URL, launched)() {
		t.Fatal("not ready over https with verification skipped")
	}
}

func TestReadLogTail(t *testing.T) {
	write := func(content string) *os.File {
		t.Helper()
		path := filepath.Join(t.TempDir(), "dittofs.log")
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		f, err := os.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = f.Close() })
		return f
	}

	// Output from an earlier run sits before the position the handle starts at.
	old := "old run: bind: address already in use\n"
	f := write(old + "line 1\nline 2\nline 3\n")
	if _, err := f.Seek(int64(len(old)), io.SeekStart); err != nil {
		t.Fatal(err)
	}
	if got, want := readLogTail(f, 2), "line 2\nline 3"; got != want {
		t.Errorf("tail = %q, want %q", got, want)
	}

	f = write("line 1\nline 2\n")
	if got, want := readLogTail(f, 10), "line 1\nline 2"; got != want {
		t.Errorf("tail = %q, want %q", got, want)
	}
}

// TestHelperDaemon is the child process of TestAwaitDaemonStart: it is not a
// test unless DFS_TEST_DAEMON says how to behave.
func TestHelperDaemon(t *testing.T) {
	switch os.Getenv("DFS_TEST_DAEMON") {
	case "":
		t.Skip("helper process")
	case "fail":
		fmt.Println("Error: API server failed: listen tcp 127.0.0.1:8080: bind: address already in use")
		os.Exit(3)
	case "run":
		time.Sleep(time.Minute)
		os.Exit(0)
	}
}

func startHelperDaemon(t *testing.T, mode string) (cmd *exec.Cmd, pidPath, logPath string, rf *os.File) {
	t.Helper()
	dir := t.TempDir()
	logPath = filepath.Join(dir, "dittofs.log")
	pidPath = filepath.Join(dir, "dittofs.pid")

	lf, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = lf.Close() })
	// Output of an earlier run, which the tail must not include.
	if _, err := lf.WriteString("earlier run\n"); err != nil {
		t.Fatal(err)
	}
	rf, err = os.Open(logPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = rf.Close() })
	if _, err := rf.Seek(0, io.SeekEnd); err != nil {
		t.Fatal(err)
	}

	cmd = exec.Command(os.Args[0], "-test.run=^TestHelperDaemon$")
	cmd.Env = append(os.Environ(), "DFS_TEST_DAEMON="+mode)
	cmd.Stdout, cmd.Stderr = lf, lf
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pidPath, []byte(fmt.Sprint(cmd.Process.Pid)), 0o644); err != nil {
		t.Fatal(err)
	}
	return cmd, pidPath, logPath, rf
}

func TestAwaitDaemonStart_ChildExits(t *testing.T) {
	cmd, pidPath, logPath, rf := startHelperDaemon(t, "fail")
	var stderr bytes.Buffer

	err := awaitDaemonStart(cmd, func() bool { return false }, pidPath, logPath, rf, &stderr)

	var st *exitStatus
	if !errors.As(err, &st) || st.code != 3 {
		t.Fatalf("err = %v, want *exitStatus with the child's code 3", err)
	}
	out := stderr.String()
	for _, want := range []string{"failed to start", "address already in use", "Log file: " + logPath} {
		if !strings.Contains(out, want) {
			t.Errorf("stderr missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "earlier run") {
		t.Errorf("stderr includes output from an earlier run:\n%s", out)
	}
	if _, err := os.Stat(pidPath); !os.IsNotExist(err) {
		t.Errorf("PID file still present after a failed start (stat err: %v)", err)
	}
}

func TestAwaitDaemonStart_Ready(t *testing.T) {
	cmd, pidPath, logPath, rf := startHelperDaemon(t, "run")
	t.Cleanup(func() { _ = cmd.Process.Kill() })
	var stderr bytes.Buffer

	if err := awaitDaemonStart(cmd, func() bool { return true }, pidPath, logPath, rf, &stderr); err != nil {
		t.Fatalf("err = %v, want nil for a server that is ready", err)
	}
	if _, err := os.Stat(pidPath); err != nil {
		t.Errorf("PID file removed although the server is running: %v", err)
	}
}
