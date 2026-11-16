// Package api is the node's HTTP surface: the API listener, which answers the
// health probes, and the metrics listener.
package api

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cubbit/dittofs/pkg/metrics"
)

// Deps is what the API serves from.
type Deps struct {
	// Ready is the node's readiness word: ready, starting, draining or
	// not_ready.
	Ready func() string
	// Live reports whether the node's watchdog holds.
	Live func() bool
	// Addr is the API listener's address, MetricsAddr the metrics
	// listener's: each host:port, a port of 0 picking a free one.
	Addr, MetricsAddr string
	// Metrics is the registry the metrics listener serves.
	Metrics *metrics.Registry
	// Logger is where a listener that fails while serving is logged.
	Logger *slog.Logger
}

// Server is the node's two listeners.
type Server struct {
	d        Deps
	api, met *http.Server
	serving  sync.WaitGroup
	stopped  atomic.Bool
}

// New returns the server over d, listening on nothing until Start.
func New(d Deps) *Server {
	mux := http.NewServeMux()
	mux.Handle("/metrics", d.Metrics.Handler())
	// decision: the metrics listener asks for no token, because a scraper
	// must reach every node and holds no API account. What it exposes to
	// anyone who reaches the address is the series' labels: share and remote
	// store names and counts, never a file name, user content or a secret.
	// Overturned if those names are themselves sensitive in a deployment,
	// and then by binding the address to a management network or by
	// requiring a token.
	return &Server{
		d:   d,
		api: &http.Server{Handler: http.HandlerFunc(d.health), ReadHeaderTimeout: 10 * time.Second, WriteTimeout: 10 * time.Second, IdleTimeout: 30 * time.Second},
		met: &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 30 * time.Second},
	}
}

// Start opens the API and the metrics listeners and serves both, returning
// the addresses they bound. When either fails to open, neither is left open.
// A server once stopped does not start again.
func (s *Server) Start() (addr, metricsAddr net.Addr, err error) {
	if s.stopped.Load() {
		return nil, nil, errors.New("api: server stopped")
	}
	al, err := net.Listen("tcp", s.d.Addr)
	if err != nil {
		return nil, nil, err
	}
	ml, err := net.Listen("tcp", s.d.MetricsAddr)
	if err != nil {
		return nil, nil, errors.Join(err, al.Close())
	}
	s.serving.Go(func() { s.serve(s.api, al) })
	s.serving.Go(func() { s.serve(s.met, ml) })
	return al.Addr(), ml.Addr(), nil
}

func (s *Server) serve(srv *http.Server, l net.Listener) {
	if err := srv.Serve(l); !errors.Is(err, http.ErrServerClosed) {
		s.d.Logger.Error("node api: listener failed", "addr", l.Addr().String(), "err", err)
	}
}

// Stop closes both listeners, waiting for the requests in flight until ctx
// ends, and then for both servers to return.
func (s *Server) Stop(ctx context.Context) error {
	s.stopped.Store(true)
	err := errors.Join(s.api.Shutdown(ctx), s.met.Shutdown(ctx))
	s.serving.Wait()
	return err
}

// health answers the two health probes, each with one word and no header but
// its type and length, and every other path 404. A stalled watchdog makes the
// node neither live nor ready, whatever its readiness word.
func (d Deps) health(w http.ResponseWriter, r *http.Request) {
	live := d.Live()
	switch r.URL.Path {
	case "/health/live":
		if live {
			word(w, http.StatusOK, "live")
		} else {
			word(w, http.StatusServiceUnavailable, "stalled")
		}
	case "/health/ready":
		ready := d.Ready()
		switch {
		case !live:
			word(w, http.StatusServiceUnavailable, "not_ready")
		case ready == "ready":
			word(w, http.StatusOK, ready)
		default:
			word(w, http.StatusServiceUnavailable, ready)
		}
	default:
		word(w, http.StatusNotFound, "not_found")
	}
}

// word writes body with status, as plain text, and suppresses the Date header
// the server would add.
func word(w http.ResponseWriter, status int, body string) {
	h := w.Header()
	h["Date"] = nil
	h.Set("Content-Type", "text/plain; charset=utf-8")
	h.Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(status)
	_, _ = w.Write([]byte(body))
}
