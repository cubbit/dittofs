// Package api is the node's HTTP surface: the API listener, which answers the
// health probes and the management API, and the metrics listener.
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

	"github.com/cubbit/dittofs/internal/node/api/auth"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/metrics"
	"github.com/prometheus/client_golang/prometheus"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"
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
	// Logger is where a listener that fails while serving is logged, and
	// an error the problem table does not name.
	Logger *slog.Logger
	// Authenticate answers who a request is; nil, every route but the
	// public ones answers 401.
	Authenticate func(*http.Request) (auth.Caller, error)
	// Store is the metadata store's control plane, which every read and
	// write of the API reaches; nil, each answers 503.
	Store controlPlane
	// Clock is the API's time, which a parked poll waits on; nil, a read
	// that would park answers 503.
	Clock clock
	// Node is the node serving the API, as its audit records name it.
	Node entity.NodeID
	// Applied returns once the node has applied what was committed before
	// it was called, or with ctx's error; nil, a write answers at its
	// commit.
	Applied func(ctx context.Context) error
	// Tracer opens each call's span; nil, the no-op tracer.
	Tracer trace.Tracer
}

// controlPlane is the metadata store's control plane as the API reaches it:
// its reads; every change through Write, which commits it with its records;
// and an idempotency key's record looked up by Replay before a step Write
// must not repeat.
type controlPlane interface {
	reads
	Write(ctx context.Context, meta entity.WriteMeta, fn func(entity.ControlPlaneTx) error) error
	Replay(ctx context.Context, i entity.IdempotencyRecord) error
}

// Server is the node's two listeners.
type Server struct {
	d        Deps
	api, met *http.Server
	serving  sync.WaitGroup
	stopped  atomic.Bool
	parked   chan struct{}
	// quit closes once the API listener shuts down, so a parked poll
	// answers rather than holding Stop until its wait has passed.
	quit chan struct{}

	tracer             trace.Tracer
	requests           *prometheus.CounterVec
	seconds            *prometheus.HistogramVec
	unmapped           prometheus.Counter
	preconditions      *prometheus.CounterVec
	replays            prometheus.Counter
	idempotencyWritten prometheus.Counter
	audits             *prometheus.CounterVec
}

// New returns the server over d, listening on nothing until Start.
func New(d Deps) *Server { return newServer(d, routes) }

// requestBuckets is the request bucket set, in seconds.
var requestBuckets = []float64{100e-6, 250e-6, 500e-6, 1e-3, 2.5e-3, 5e-3, 10e-3, 25e-3, 50e-3, 100e-3, 250e-3, 500e-3, 1, 2.5, 5, 10, 30, 60}

// newServer is New over the route table rows.
func newServer(d Deps, rows []route) *Server {
	s := &Server{
		d:      d,
		parked: make(chan struct{}, maxParked),
		quit:   make(chan struct{}),
		tracer: d.Tracer,
		requests: prometheus.NewCounterVec(prometheus.CounterOpts{Name: "dittofs_api_requests_total",
			Help: "API calls, by op and outcome: ok or the problem's code."}, []string{"op", "outcome"}),
		seconds: prometheus.NewHistogramVec(prometheus.HistogramOpts{Name: "dittofs_api_request_seconds",
			Help: "How long API calls take, by op.", Buckets: requestBuckets}, []string{"op"}),
		unmapped: prometheus.NewCounter(prometheus.CounterOpts{Name: "dittofs_api_unmapped_errors_total",
			Help: "API errors answered 500 because the problem table names none of them; zero on a healthy node."}),
		preconditions: prometheus.NewCounterVec(prometheus.CounterOpts{Name: "dittofs_api_precondition_failures_total",
			Help: "Writes refused on a precondition, by kind: generation, idempotency or missing."}, []string{"kind"}),
		replays: prometheus.NewCounter(prometheus.CounterOpts{Name: "dittofs_api_idempotent_replays_total",
			Help: "Writes answered from their idempotency record."}),
		idempotencyWritten: prometheus.NewCounter(prometheus.CounterOpts{Name: "dittofs_api_idempotency_records_written_total",
			Help: "Idempotency records written with the writes they answer."}),
		audits: prometheus.NewCounterVec(prometheus.CounterOpts{Name: "dittofs_audit_records_total",
			Help: "Audit records written, by outcome: applied or refused."}, []string{"outcome"}),
	}
	if s.tracer == nil {
		s.tracer = noop.NewTracerProvider().Tracer("")
	}
	d.Metrics.Registry().MustRegister(s.requests, s.seconds, s.unmapped, s.preconditions, s.replays, s.idempotencyWritten, s.audits)

	api := http.NewServeMux()
	api.HandleFunc("/", d.health)
	api.HandleFunc("/api", d.health) // not redirected to /api/
	api.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) { s.handle(w, r, nil) })
	groups := map[string][]route{}
	for _, rt := range rows {
		key := rt.method + " " + rt.pattern
		groups[key] = append(groups[key], rt)
	}
	for key, g := range groups {
		api.HandleFunc(key, func(w http.ResponseWriter, r *http.Request) { s.handle(w, r, g) })
	}

	mux := http.NewServeMux()
	mux.Handle("/metrics", d.Metrics.Handler())
	// decision: the metrics listener asks for no token, because a scraper
	// must reach every node and holds no API account. What it exposes to
	// anyone who reaches the address is the series' labels: share and remote
	// store names and counts, never a file name, user content or a secret.
	// Overturned if those names are themselves sensitive in a deployment,
	// and then by binding the address to a management network or by
	// requiring a token.
	s.api = &http.Server{Handler: api, ReadHeaderTimeout: 10 * time.Second, WriteTimeout: 10 * time.Second, IdleTimeout: 30 * time.Second}
	s.met = &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 30 * time.Second}
	s.api.RegisterOnShutdown(sync.OnceFunc(func() { close(s.quit) }))
	return s
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
// ends, a parked poll answering its last read at once, and then for both
// servers to return.
func (s *Server) Stop(ctx context.Context) error {
	s.stopped.Store(true)
	err := errors.Join(s.api.Shutdown(ctx), s.met.Shutdown(ctx))
	s.serving.Wait()
	return err
}

// health answers the two health probes, each with one word and no header but
// its type and length, and every other path outside /api/ 404. A stalled watchdog makes the
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
