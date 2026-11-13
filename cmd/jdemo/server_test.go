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
	"github.com/cubbit/dittofs/pkg/block/journal/journaltest"
)

// health is what GET /health answers.
type health struct {
	ID      string `json:"id"`
	Filesys string `json:"filesys"`
}

// tmpSocket returns a socket path under a short temp directory, because a
// unix domain socket's name is capped (104 bytes on macOS) and t.TempDir()'s
// path is longer than the cap for long test names.
func tmpSocket(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "jdemo")
	if err != nil {
		t.Fatalf("temp socket dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return filepath.Join(dir, "s.sock")
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
	defer func() { _ = resp.Body.Close() }()
	var h health
	if err := json.NewDecoder(resp.Body).Decode(&h); err != nil {
		t.Fatalf("decode /health: %v", err)
	}
	return h
}

func TestServerStartStop(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "journal")
	socket := tmpSocket(t)
	srv := &Server{Options: testJournalOptions(), Dir: dir, SocketPath: socket}

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
	socket := tmpSocket(t)

	first := &Server{Options: testJournalOptions(), Dir: dir, SocketPath: socket}
	if err := first.Start(context.Background()); err != nil {
		t.Fatalf("first start: %v", err)
	}
	id := getHealth(t, socket).ID
	if err := first.Shutdown(context.Background()); err != nil {
		t.Fatalf("first shutdown: %v", err)
	}

	second := &Server{Options: testJournalOptions(), Dir: dir, SocketPath: socket}
	err := second.Start(context.Background())
	if err != nil && !strings.Contains(err.Error(), "recovering") {
		t.Fatalf("second start (reopen): got err %v, want the journal's own answer", err)
	}
	if err == nil {
		// The rebase that lands T7.6 flips this test: recovery opens the
		// segments, and the assertion becomes "same journal id".
		defer func() { _ = second.Shutdown(context.Background()) }()
		if got := getHealth(t, socket).ID; got != id {
			t.Errorf("reopen: journal id: got %q, want %q", got, id)
		}
		return
	}
	// On this base Open refuses a directory holding segments — the spares
	// the first open wrote — because recovery's assembly is T7.6. The
	// refusal is surfaced verbatim, the demo's restart beat, and nothing is
	// touched: a third start answers the same.
	third := &Server{Options: testJournalOptions(), Dir: dir, SocketPath: socket}
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
	if _, err := journal.Init(context.Background(), dir, demoInstallID, journal.WithSettings(demoSettings), journaltest.SuitableMount()); err != nil {
		t.Fatalf("hand-crafted init: %v", err)
	}
	before, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("list dir: %v", err)
	}

	srv := &Server{Options: testJournalOptions(), Dir: dir, SocketPath: tmpSocket(t)}
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
	srv := &Server{Options: testJournalOptions(), Dir: dir, SocketPath: tmpSocket(t)}
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

func TestStartTrailingSlashKeepsStateSibling(t *testing.T) {
	base := t.TempDir()
	srv := &Server{Options: testJournalOptions(), Dir: filepath.Join(base, "journal") + "/", SocketPath: tmpSocket(t)}
	defer func() { _ = srv.Shutdown(context.Background()) }()
	if err := srv.Start(context.Background()); err != nil {
		t.Fatalf("start with trailing slash: %v", err)
	}
	// The state file is a sibling of the journal dir, never inside it —
	// that is what keeps the journal's own listing what the format owns.
	if _, err := os.Stat(filepath.Join(base, "journal.jdemo.json")); os.IsNotExist(err) {
		t.Errorf("state file: %s missing after a start", filepath.Join(base, "journal.jdemo.json"))
	}
	entries, err := os.ReadDir(filepath.Join(base, "journal"))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".jdemo.json") {
			t.Errorf("state file %s sits inside the journal dir, want a sibling", e.Name())
		}
	}
}

func TestStartRefusesBusySocketWithoutTouchingTheDir(t *testing.T) {
	// A live daemon owns the socket: a second start must refuse before it
	// initialises anything, or the talk's fresh dir is burned for nothing.
	busy := tmpSocket(t)
	ln, err := net.Listen("unix", busy)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()

	base := t.TempDir()
	srv := &Server{Options: testJournalOptions(), Dir: filepath.Join(base, "journal"), SocketPath: busy}
	err = srv.Start(context.Background())
	if err == nil || !strings.Contains(err.Error(), "already") {
		t.Fatalf("start against a live socket: got err %v, want a refusal naming the running daemon", err)
	}
	if entries, _ := os.ReadDir(filepath.Join(base, "journal")); len(entries) > 0 {
		t.Errorf("refused start left %d entries in the journal dir, want none", len(entries))
	}
}

func TestStartRemovesStaleSocket(t *testing.T) {
	// A crashed daemon leaves its socket file behind; the next start cleans
	// it up rather than failing.
	stale := tmpSocket(t)
	if err := os.WriteFile(stale, []byte("stale"), 0o600); err != nil {
		t.Fatal(err)
	}
	srv := &Server{Options: testJournalOptions(), Dir: filepath.Join(t.TempDir(), "journal"), SocketPath: stale}
	defer func() { _ = srv.Shutdown(context.Background()) }()
	if err := srv.Start(context.Background()); err != nil {
		t.Fatalf("start over a stale socket: %v (want it removed and the start to go on)", err)
	}
	resp, err := unixClient(t, stale).Get("http://jdaemon/health")
	if err != nil {
		t.Fatalf("health over the replaced socket: %v", err)
	}
	_ = resp.Body.Close()
}

// testJournalOptions passes journaltest's mount probe: a test cannot choose
// its host's filesystem, and the platform check refuses the CI runner's
// mounts (a barrier-less /tmp among them), so tests judge a suitable mount
// the same way the fixture does. The daemon's own start passes none.
func testJournalOptions() []journal.Option {
	return []journal.Option{journaltest.SuitableMount()}
}
