package main

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/cubbit/dittofs/pkg/block/journal"
)

// socketTimeout bounds one client round trip to the daemon.
const socketTimeout = 3 * time.Second

// versionWindow is how far each start raises the version limit past the open's
// floor, mirroring what a production caller records durably before raising
// its limit. A talk spends at most a few thousand of it.
const versionWindow uint64 = 1 << 32

// demoSettings keep the journal small on a laptop: two streams, one spare,
// a small headroom, and a bounded footprint. The maximum sits above the
// spare pool's targets by a wide margin — the open resolves the pool's
// targets and reserves them against the maximum, and one below the targets
// leaves the pool permanently short, refusing every write.
var demoSettings = journal.Settings{
	Streams:         2,
	SpareTarget:     1,
	HeadroomRecords: 1024,
	MaxBytes:        4 << 30,
}

// demoInstallID is the fixed installation ID every jdemo-initiated journal
// carries; the demo is one installation, so a constant is honest.
var demoInstallID = func() (id journal.InstallationID) {
	copy(id[:], "jdemo-install")
	return id
}()

// Server is the daemon: the one owner of a journal directory. It opens (or
// initialises) the journal, raises and records the version limit, and serves
// the CLI's requests over a unix socket. Everything else — the client
// binaries the panes run — only talks to the socket.
type Server struct {
	Dir        string // the journal directory
	SocketPath string
	// OffloadDir holds offloaded payloads the offload command writes.
	OffloadDir string

	mu      sync.Mutex
	j       *journal.Journal
	id      journal.JournalID
	handles map[journal.ShareTag]*journal.Handle

	srv http.Server
	ln  net.Listener
}

// Options are the journal options Start opens with, appended after its own
// clock and settings.
func (s *Server) journalOptions() []journal.Option {
	return []journal.Option{
		journal.WithClock(realClock{}),
		journal.WithSettings(demoSettings),
	}
}

// Start initialises or opens the journal and serves on the socket until
// Shutdown. A fresh, empty directory is initialised; a journal with a state
// file is reopened through recovery; anything else is refused.
func (s *Server) Start(ctx context.Context) error {
	dir := s.Dir
	passed, err := loadState(statePath(dir))
	var (
		j     *journal.Journal
		id    journal.JournalID
		floor uint64
	)
	switch {
	case err == nil:
		id = passed.ID
		j, err = journal.Open(ctx, dir, journal.Version{Counter: passed.Limit}, id, nil, s.journalOptions()...)
		if err != nil {
			return fmt.Errorf("jdemo: open %s: %w", dir, err)
		}
		floor = passed.Limit

	case errors.Is(err, fs.ErrNotExist):
		empty, lerr := dirEmpty(dir)
		if lerr != nil {
			return fmt.Errorf("jdemo: %s: %w", dir, lerr)
		}
		if !empty {
			return fmt.Errorf("jdemo: %s holds a journal but the state file %s is missing; refusing to initialise over it", dir, statePath(dir))
		}
		// Init requires the directory to exist; an empty listing saw to
		// nothing else being in it.
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("jdemo: %s: %w", dir, err)
		}
		id, err = journal.Init(ctx, dir, demoInstallID, s.journalOptions()...)
		if err != nil {
			return fmt.Errorf("jdemo: init %s: %w", dir, err)
		}
		j, err = journal.Open(ctx, dir, journal.Version{}, id, nil, s.journalOptions()...)
		if err != nil {
			return fmt.Errorf("jdemo: open fresh %s: %w", dir, err)
		}

	default:
		return fmt.Errorf("jdemo: refusing to start, %w", err)
	}

	limit := journal.Version{Counter: floor + versionWindow}
	if err := j.SetVersionLimit(limit); err != nil {
		_ = j.Close()
		return fmt.Errorf("jdemo: setting version limit: %w", err)
	}
	// The pool makes its spares asynchronously once the open returns, and a
	// write racing it answers "refused by the spare limit"; the demo's
	// first command is at the talk, so Start waits them out.
	if err := awaitSpares(ctx, j); err != nil {
		_ = j.Close()
		return fmt.Errorf("jdemo: %w", err)
	}
	if err := saveState(statePath(dir), stateFile{ID: id, Limit: limit.Counter}); err != nil {
		_ = j.Close()
		return err
	}

	ln, err := net.Listen("unix", s.SocketPath)
	if err != nil {
		_ = j.Close()
		return fmt.Errorf("jdemo: listen %s: %w", s.SocketPath, err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", s.health)
	s.mountRoutes(mux)

	s.mu.Lock()
	s.j = j
	s.id = id
	s.handles = make(map[journal.ShareTag]*journal.Handle)
	s.ln = ln
	s.srv = http.Server{Handler: mux, ReadHeaderTimeout: socketTimeout}
	s.mu.Unlock()

	serveErr := make(chan error, 1)
	go func() { serveErr <- s.srv.Serve(ln) }()
	go func() {
		// The socket stops serving when Shutdown runs; the first answer
		// Serve returns (http.ErrServerClosed) is expected and dropped.
		if err := <-serveErr; err != nil && !errors.Is(err, http.ErrServerClosed) {
			// Nothing else can be told at this point; the process is
			// about to end, and stderr is where the presenter looks.
			fmt.Fprintf(os.Stderr, "jdemo: serve: %v\n", err)
		}
	}()
	return nil
}

// Shutdown stops serving, closes the journal and removes the socket. After it
// the Server answers ErrClosed through the journal's own contract.
func (s *Server) Shutdown(ctx context.Context) error {
	s.mu.Lock()
	ln, j := s.ln, s.j
	s.ln, s.j = nil, nil
	s.mu.Unlock()

	var firstErr error
	join := func(err error, what string) {
		if err != nil {
			firstErr = errors.Join(firstErr, fmt.Errorf("jdemo: %s: %w", what, err))
		}
	}
	if ln != nil {
		join(s.srv.Shutdown(ctx), "http shutdown")
		_ = ln.Close()
		if err := os.Remove(s.SocketPath); !os.IsNotExist(err) {
			join(err, "removing socket "+s.SocketPath)
		}
	}
	if j != nil {
		join(j.Close(), "journal close")
	}
	return firstErr
}

// health answers GET /health with the journal's identity.
func (s *Server) health(w http.ResponseWriter, _ *http.Request) {
	s.mu.Lock()
	id := s.id
	s.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"id":%q,"filesys":"ok"}`, hex.EncodeToString(id[:]))
}

// dirEmpty answers whether dir holds no entry, existing or not: a directory
// that does not exist is one Init may create.
//
// decision: the daemon initialises only where it can see the directory to be
// empty, rather than trusting Init to refuse a directory holding a journal,
// so the demo can never initialise over data it failed to recognise.
func dirEmpty(dir string) (bool, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return true, nil
		}
		return false, err
	}
	return len(entries) == 0, nil
}

// sparesDeadline bounds how long Start waits for the pool's first spare; a
// generous backstop over a condition polled on, not slept through.
const sparesDeadline = 30 * time.Second

// awaitSpares waits until the journal's spare pool holds its first content
// spare. The pool's ready hook takes a type only the journal's tree can name,
// so Stats is the wait's eye on it — a poll of the real condition, and the
// deadline only the backstop.
func awaitSpares(ctx context.Context, j *journal.Journal) error {
	deadline := time.Now().Add(sparesDeadline)
	for {
		if st := j.Stats(); st.Spares >= 1 {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("spare pool not ready after %s (Spares=%d)", sparesDeadline, j.Stats().Spares)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
	}
}
