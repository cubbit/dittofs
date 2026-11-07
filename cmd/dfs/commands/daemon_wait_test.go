package commands

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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
			fmt.Fprintf(w, `{"status":"healthy","data":{"started_at":%q}}`, startedAt)
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
			if got := healthReadyFunc(srv.URL, launched)(); got != tc.want {
				t.Fatalf("ready = %v, want %v", got, tc.want)
			}
		})
	}

	t.Run("nothing listening", func(t *testing.T) {
		srv := serve(200, "")
		url := srv.URL
		srv.Close()
		if healthReadyFunc(url, launched)() {
			t.Fatal("ready with no server listening")
		}
	})
}

func TestReadLogTail(t *testing.T) {
	path := filepath.Join(t.TempDir(), "dittofs.log")
	old := "old run: bind: address already in use\n"
	if err := os.WriteFile(path, []byte(old+"line 1\nline 2\nline 3\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if got, want := readLogTail(path, int64(len(old)), 2), "line 2\nline 3"; got != want {
		t.Errorf("tail = %q, want %q", got, want)
	}
	if got, want := readLogTail(path, int64(len(old)), 10), "line 1\nline 2\nline 3"; got != want {
		t.Errorf("tail = %q, want %q", got, want)
	}
	if got := readLogTail(filepath.Join(t.TempDir(), "missing.log"), 0, 10); got != "" {
		t.Errorf("tail of a missing log = %q, want empty", got)
	}
}
