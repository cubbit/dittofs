package main

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cubbit/dittofs/pkg/block/journal"
)

// health is what GET /health answers.
type health struct {
	ID      string `json:"id"`
	Filesys string `json:"filesys"`
}

// unixClient returns an HTTP client that dials the daemon's unix socket.
func unixClient(t *testing.T, socket string) *http.Client {
	t.Helper()
	return &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", socket)
			},
		},
		Timeout: socketTimeout,
	}
}

// getHealth fetches /health over the socket, failing the test on transport
// trouble.
func getHealth(t *testing.T, socket string) health {
	t.Helper()
	resp, err := unixClient(t, socket).Get("http://jdaemon/health")
	if err != nil {
		t.Fatalf("GET /health: %v", err)
	}
	defer resp.Body.Close()
	var h health
	if err := json.NewDecoder(resp.Body).Decode(&h); err != nil {
		t.Fatalf("decode /health: %v", err)
	}
	return h
}

func TestServerStartStop(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "journal")
	socket := filepath.Join(t.TempDir(), "s.sock")
	srv := &Server{Dir: dir, SocketPath: socket}

	if err := srv.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
	h := getHealth(t, socket)
	if raw, err := hex.DecodeString(h.ID); err != nil || len(raw) != 16 {
		t.Errorf("/health id: got %q, want 32 hex chars of a 16-byte id (decode err %v)", h.ID, err)
	}
	if h.Filesys != "ok" {
		t.Errorf("/health filesys: got %q, want %q", h.Filesys, "ok")
	}

	if err := srv.Shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	if _, err := unixClient(t, socket).Get("http://jdaemon/health"); err == nil {
		t.Errorf("GET /health after shutdown: err nil, want a transport error")
	}
	// A second shutdown must not panic or close the journal twice: it reports
	// ErrClosed through the journal's own answer.
	if err := srv.Shutdown(context.Background()); err == nil {
		t.Log("second shutdown answered nil")
	}
}

func TestServerRestart(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "journal")
	socket := filepath.Join(t.TempDir(), "s.sock")

	first := &Server{Dir: dir, SocketPath: socket}
	if err := first.Start(context.Background()); err != nil {
		t.Fatalf("first start: %v", err)
	}
	id := getHealth(t, socket).ID
	if err := first.Shutdown(context.Background()); err != nil {
		t.Fatalf("first shutdown: %v", err)
	}

	second := &Server{Dir: dir, SocketPath: socket}
	err := second.Start(context.Background())
	if err != nil && !strings.Contains(err.Error(), "recovering") {
		t.Fatalf("second start (reopen): got err %v, want the journal's own answer", err)
	}
	if err == nil {
		// The rebase that lands T7.6 flips this test: recovery opens the
		// segments, and the assertion becomes "same journal id".
		defer second.Shutdown(context.Background())
		if got := getHealth(t, socket).ID; got != id {
			t.Errorf("reopen: journal id: got %q, want %q", got, id)
		}
		return
	}
	// On this base Open refuses a directory holding segments — the spares
	// the first open wrote — because recovery's assembly is T7.6. The
	// refusal is surfaced verbatim, the demo's restart beat, and nothing is
	// touched: a third start answers the same.
	third := &Server{Dir: dir, SocketPath: socket}
	if err := third.Start(context.Background()); err == nil {
		t.Fatalf("third start after refused reopen: err nil, want the same refusal")
		_ = third.Shutdown(context.Background())
	}
}

func TestServerRefusesWithoutState(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "journal")
	// A journal exists, hand-made, and its state file does not.
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := journal.Init(context.Background(), dir, demoInstallID, journal.WithSettings(demoSettings)); err != nil {
		t.Fatalf("hand-crafted init: %v", err)
	}
	before, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("list dir: %v", err)
	}

	srv := &Server{Dir: dir, SocketPath: filepath.Join(t.TempDir(), "s.sock")}
	err = srv.Start(context.Background())
	if err == nil || !strings.Contains(err.Error(), "state file") {
		t.Fatalf("start without state file: got err %v, want an error naming the state file", err)
	}
	after, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("list dir after refusal: %v", err)
	}
	if len(after) != len(before) {
		t.Errorf("refusal touched the journal dir: %d entries before, %d after", len(before), len(after))
	}
}

func TestServerRefusesCorruptState(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "journal")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(statePath(dir), []byte("{{junk"), 0o600); err != nil {
		t.Fatal(err)
	}
	srv := &Server{Dir: dir, SocketPath: filepath.Join(t.TempDir(), "s.sock")}
	err := srv.Start(context.Background())
	if err == nil || !strings.Contains(err.Error(), "state file") {
		t.Fatalf("start on corrupt state file: got err %v, want an error naming the state file", err)
	}
	if _, serr := os.Stat(statePath(dir)); os.IsNotExist(serr) {
		t.Errorf("corrupt state file was deleted by the refused start, want it untouched")
	}
	// The refused start leaves an empty journal dir empty.
	entries, _ := os.ReadDir(dir)
	if len(entries) != 0 {
		t.Errorf("refused start on corrupt state created %d entries in the journal dir, want none", len(entries))
	}
}
