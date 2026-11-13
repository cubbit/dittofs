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
	"path/filepath"
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
	Streams: 2,
	// SpareTarget 2: a reservation takes spares for the rolls its writes
	// may make, so a pool of one refuses a two-write reserve at once.
	SpareTarget:     2,
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

	// Options are extra journal Options Start opens with, appended after
	// its own clock and settings. Tests pass the platform probe they cannot
	// choose on their host; the daemon's start adds none, so a real
	// directory is judged by the real mount it lives on.
	Options []journal.Option

	mu      sync.Mutex
	j       *journal.Journal
	id      journal.JournalID
	handles map[journal.ShareTag]*journal.Handle

	// reservations are the open Reserve answers, keyed by rid; nextRes hands
	// the next one its number. The registry dies with the journal: its own
	// Close releases every reservation still open.
	reservations map[uint32]*journal.Reservation
	nextRes      uint32

	srv http.Server
	ln  net.Listener
}

// journalOptions are the options Start opens with: its own clock and
// settings, then whatever extra Options the Server carries.
func (s *Server) journalOptions() []journal.Option {
	opts := []journal.Option{
		journal.WithClock(realClock{}),
		journal.WithSettings(demoSettings),
	}
	return append(opts, s.Options...)
}

// takeSocket refuses a start whose socket something already listens on, and
// silently removes a stale socket file a crashed daemon left behind, so the
// next start is not refused by a dead predecessor. The dial is the whole
// question: what accepts is an owner, whatever it speaks; what refuses
// nothing left but the file.
func (s *Server) takeSocket() error {
	if _, err := os.Stat(s.SocketPath); os.IsNotExist(err) {
		return nil
	}
	conn, err := net.DialTimeout("unix", s.SocketPath, 500*time.Millisecond)
	if err == nil {
		_ = conn.Close()
		return fmt.Errorf("another daemon already owns %s; stop it or choose another socket", s.SocketPath)
	}
	if err := os.Remove(s.SocketPath); err != nil {
		return fmt.Errorf("removing stale socket %s: %w", s.SocketPath, err)
	}
	return nil
}

// Start takes the socket first, then initialises or opens the journal and
// serves until Shutdown. A fresh, empty directory is initialised; a journal
// with a state file is reopened through recovery; anything else is refused.
// The socket-first order is deliberate: a start that fails at the socket
// must not have burned a fresh directory first, and a journal directory a
// second daemon would touch stays owned by the one that holds the socket.
func (s *Server) Start(ctx context.Context) error {
	dir := filepath.Clean(s.Dir)

	if err := s.takeSocket(); err != nil {
		return err
	}
	ln, err := net.Listen("unix", s.SocketPath)
	if err != nil {
		return fmt.Errorf("listen %s: %w", s.SocketPath, err)
	}
	dropSocket := func() {
		_ = ln.Close()
		_ = os.Remove(s.SocketPath)
	}

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
			dropSocket()
			return fmt.Errorf("open %s: %w", dir, err)
		}
		floor = passed.Limit

	case errors.Is(err, fs.ErrNotExist):
		empty, lerr := dirEmpty(dir)
		if lerr != nil {
			dropSocket()
			return fmt.Errorf("%s: %w", dir, lerr)
		}
		if !empty {
			dropSocket()
			return fmt.Errorf("%s holds a journal but the state file %s is missing; refusing to initialise over it", dir, statePath(dir))
		}
		// Init requires the directory to exist; an empty listing saw to
		// nothing else being in it.
		if err := os.MkdirAll(dir, 0o755); err != nil {
			dropSocket()
			return fmt.Errorf("%s: %w", dir, err)
		}
		id, err = journal.Init(ctx, dir, demoInstallID, s.journalOptions()...)
		if err != nil {
			dropSocket()
			return fmt.Errorf("init %s: %w", dir, err)
		}
		// The identity is durable the moment it exists: a later refusal or
		// a ctrl-c during the open must leave a journal Open can name on the
		// next start, not a directory no state file describes.
		if err := saveState(statePath(dir), stateFile{ID: id}); err != nil {
			dropSocket()
			return err
		}
		j, err = journal.Open(ctx, dir, journal.Version{}, id, nil, s.journalOptions()...)
		if err != nil {
			dropSocket()
			return fmt.Errorf("open fresh %s: %w", dir, err)
		}

	default:
		dropSocket()
		return fmt.Errorf("refusing to start, %w", err)
	}

	limit := journal.Version{Counter: floor + versionWindow}
	if err := j.SetVersionLimit(limit); err != nil {
		dropSocket()
		_ = j.Close()
		return fmt.Errorf("setting version limit: %w", err)
	}
	// The pool makes its spares asynchronously once the open returns, and a
	// write racing it answers "refused by the spare limit"; the demo's
	// first command is at the talk, so Start waits them out.
	if err := awaitSpares(ctx, j); err != nil {
		dropSocket()
		_ = j.Close()
		return fmt.Errorf("%w", err)
	}
	if err := saveState(statePath(dir), stateFile{ID: id, Limit: limit.Counter}); err != nil {
		dropSocket()
		_ = j.Close()
		return err
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", s.health)
	s.mountRoutes(mux)
	s.mountRemovalRoutes(mux)
	s.mountOffloadRoutes(mux)
	s.mountChangeRoutes(mux)
	s.mountViewRoutes(mux)

	s.mu.Lock()
	s.j = j
	s.id = id
	s.handles = make(map[journal.ShareTag]*journal.Handle)
	s.reservations = make(map[uint32]*journal.Reservation)
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
			firstErr = errors.Join(firstErr, fmt.Errorf("%s: %w", what, err))
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
	// A response whose client hangs up half-written has nowhere better to
	// be reported than a swallowed write error.
	panePrint(w, `{"id":%q,"filesys":"ok"}`, hex.EncodeToString(id[:]))
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
		if st := j.Stats(); st.Spares >= demoSettings.SpareTarget {
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
