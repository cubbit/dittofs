package main

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"
)

func TestClientDaemonDown(t *testing.T) {
	socket := filepath.Join(t.TempDir(), "missing.sock")
	c := Dial(socket)
	err := c.Do(context.Background(), "GET", "/health", nil, nil)
	if err == nil {
		t.Fatalf("Do against no daemon: err nil, want an unreachable error")
	}
	var buf bytes.Buffer
	friendly(&buf, err)
	want := "jdemo: daemon not reachable at " + socket + " — run 'jdemo start' first"
	if got := buf.String(); got != want+"\n" {
		t.Errorf("friendly(unreachable): got %q, want %q", got, want)
	}
}

func TestRunCLIDaemonDown(t *testing.T) {
	socket := filepath.Join(t.TempDir(), "missing.sock")
	var out, errw bytes.Buffer
	code := runCLI(&out, &errw, []string{"--socket", socket, "draw"})
	if code != 1 {
		t.Errorf("runCLI exit code: got %d, want 1", code)
	}
	want := "jdemo: daemon not reachable at " + socket + " — run 'jdemo start' first\n"
	if got := errw.String(); got != want {
		t.Errorf("runCLI on a stopped daemon: got stderr %q, want exactly %q", got, want)
	}
	for name, s := range map[string]string{"stdout": out.String(), "stderr": errw.String()} {
		if strings.Contains(s, "Usage") {
			t.Errorf("runCLI printed a usage dump to %s on a stopped daemon:\n%s", name, s)
		}
	}
}

func TestRunCLIJournalErrorIsOneLine(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "journal")
	socket := tmpSocket(t)
	srv := &Server{Dir: dir, SocketPath: socket}
	if err := srv.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer srv.Shutdown(context.Background())
	if out2, err := exec(t, socket, "share", "--tag", "1"); err != nil {
		t.Fatalf("share: %v (out %q)", err, out2)
	}

	// A refused op prints the journal's own answer and nothing else: no
	// usage block, no duplicated error line.
	var out, errw bytes.Buffer
	code := runCLI(&out, &errw, []string{"--socket", socket, "sync", "--tag", "1"})
	if code != 1 {
		t.Errorf("runCLI exit code: got %d, want 1", code)
	}
	want := "jdemo: journal: not implemented\n"
	if got := errw.String(); got != want {
		t.Errorf("runCLI on a refused op: got stderr %q, want exactly %q", got, want)
	}
	if strings.Contains(out.String()+errw.String(), "Usage") {
		t.Errorf("runCLI printed a usage dump on a refused op:\n%s%s", out.String(), errw.String())
	}
}
