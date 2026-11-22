// Package api is the node's HTTP surface: the API listener, which answers the
// health probes and the management API, and the metrics listener.
package api

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/cubbit/dittofs/internal/node/api/auth"
	"github.com/cubbit/dittofs/internal/tlsconfig"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/metrics"
	"github.com/cubbit/dittofs/pkg/node/apiv1"
	"github.com/prometheus/client_golang/prometheus"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"
)

// Deps is what the API serves from.
type Deps struct {
	// Ready is the node's readiness word: ready, starting, draining or
	// not_ready.
	Ready func() string
	// Served reports whether the node has been ready since it started.
	//
	// decision: nil answers that it has, so a server built without a
	// lifecycle, the package's own tests among them, serves its calls. A
	// second production builder of Deps that leaves it nil would serve the
	// writes of a node whose start failed, which nothing then applies; the
	// node is the one builder today. Make nil refuse once there is another.
	Served func() bool
	// Live reports whether the node's watchdog holds.
	Live func() bool
	// Addr is the API listener's address, MetricsAddr the metrics
	// listener's: each host:port, a port of 0 picking a free one. Each is
	// served over TLS unless it binds a loopback address.
	Addr, MetricsAddr string
	// TLS is the certificate and key, bootstrap facts of the host, that
	// both listeners serve under on an address but loopback; with neither,
	// a start on such an address is refused, and with a ClientCA, any start.
	TLS tlsconfig.Config
	// Socket is the local socket's path, a bootstrap fact of the host, where
	// the API is also served without TLS, to the server's user only; empty,
	// there is none.
	Socket string
	// Metrics is the registry the metrics listener serves.
	Metrics *metrics.Registry
	// Logger is where a listener that fails while serving is logged, and
	// an error the problem table does not name.
	Logger *slog.Logger
	// Authenticate answers who a request is; nil, every route but the
	// public ones answers 401.
	Authenticate func(*http.Request) (auth.Caller, error)
	// Credentials signs a caller in and refreshes its session; nil, sign-in
	// and refresh answer 503.
	Credentials signIns
	// Store is the metadata store's control plane, which every read and
	// write of the API reaches; nil, each answers 503.
	Store controlPlane
	// SigningKey returns the installation's access-token signing key at the
	// version that signs now, which keys each idempotency key's digest; nil,
	// a write carrying an Idempotency-Key answers 503.
	SigningKey func(context.Context) ([]byte, error)
	// Nodes is the store's reads of the nodes: their records, and every
	// registered node's status, which a listing computes each item's applied
	// from; nil, each listing answers 503.
	Nodes nodeReads
	// Progress is an operation's progress, read from what its owner
	// records, by the operation's kind; nil, every operation's progress is
	// unknown. It runs on every read of an operation, each re-read of a
	// parked poll included, and never inside a write's transaction; it
	// must not write.
	Progress func(context.Context, entity.Operation) apiv1.Progress
	// Health is the node's health conditions, its own and the
	// installation's; nil, it holds none.
	Health conditions
	// Connections is the connections open on this node now, as each of its
	// adapters lists them, which GET /nodes/{id}/connections pages through
	// (RFC 23 §4.2).
	//
	// ponytail: only this node's connections are read: an ID naming another
	// node is answered 404, as a node that never registered is, so in a
	// cluster a node's connections are read through that node's own API.
	// The answer for another node's ID belongs to RFC 23, not RFC 15, whose
	// forwarding carries vfs calls only, and §4.2 does not give it yet;
	// replace the 404 with it once an installation runs a second node.
	Connections func() []apiv1.Connection
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
	// Settings is the settings table, which judges every write of a
	// setting; nil, the empty table, under which every key is refused.
	Settings settingsTable
	// Sealer seals a secret's value before the write that stores it; nil,
	// every write of a secret answers 503. A typed nil, a nil pointer in the
	// interface, must not be handed in: it is not nil here, and its Seal
	// would be called.
	Sealer sealer
	// Directory is identity's directory, asked the user SID a Kerberos name
	// links for before the write that stores it; nil, a write giving a user
	// a new Kerberos name answers 503.
	Directory directory
	// Joins is identity's hold of an online join's administrator credential,
	// in memory under the operation it is handed to; nil, join-domain,
	// online or offline, resolve-principal, test-directory and
	// rotate-machine-password answer 503.
	Joins joins
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
	// calls holds one token per API call running.
	calls chan struct{}
	// startRights is the right that may start each kind of operation, which
	// a cancel of one asks.
	startRights map[apiv1.OpKind]auth.Right
	// quit closes once the API listener shuts down, so a parked poll
	// answers rather than holding Stop until its wait has passed.
	quit chan struct{}

	// seen, nil but in a test, is handed each write's body as read, then
	// its decoded body, so a test can see each cleared once the write
	// returns.
	seen func(any)

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
		d:           d,
		parked:      make(chan struct{}, maxParked),
		calls:       make(chan struct{}, maxCalls),
		startRights: startRights(rows),
		quit:        make(chan struct{}),
		tracer:      d.Tracer,
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
	inFlight := prometheus.NewGaugeFunc(prometheus.GaugeOpts{Name: "dittofs_api_calls_in_flight",
		Help: "API calls running now, parked polls apart."}, func() float64 { return float64(len(s.calls)) })
	limit := prometheus.NewGaugeFunc(prometheus.GaugeOpts{Name: "dittofs_api_calls_in_flight_limit",
		Help: "How many API calls the node runs at once."}, func() float64 { return maxCalls })
	d.Metrics.Registry().MustRegister(s.requests, s.seconds, s.unmapped, s.preconditions, s.replays, s.idempotencyWritten, s.audits, newOpsCollector(s), inFlight, limit)

	api := http.NewServeMux()
	api.HandleFunc("/", d.health)
	api.HandleFunc("/api", d.health) // not redirected to /api/
	api.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) { s.handle(w, r, nil) })
	groups := map[string][]route{}
	for _, rt := range rows {
		key := rt.method + " " + rt.muxPattern()
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
	// The read timeout covers the body, so a client that sends its headers
	// and then stalls gives its call slot back.
	s.api = &http.Server{Handler: api, ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 10 * time.Second, IdleTimeout: 30 * time.Second,
		ConnContext: func(ctx context.Context, c net.Conn) context.Context {
			if _, ok := c.(*net.UnixConn); ok {
				return auth.WithLocal(ctx)
			}
			return ctx
		}}
	s.met = &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 30 * time.Second}
	s.api.RegisterOnShutdown(sync.OnceFunc(func() { close(s.quit) }))
	return s
}

// Start opens the API and the metrics listeners, and the local socket when
// Deps names one, and serves them, returning the addresses the two
// listeners bound. When any fails to open, none is left open. A server once
// stopped does not start again.
func (s *Server) Start() (addr, metricsAddr net.Addr, err error) {
	if s.stopped.Load() {
		return nil, nil, errors.New("api: server stopped")
	}
	// decision: the API's listeners take no client CA, which would make
	// every caller, the health probes and the metrics scraper among them,
	// present a client certificate. Overturned if the API is ever to be
	// reached by client certificate.
	if s.d.TLS.ClientCA != "" {
		return nil, nil, errors.New("api: the API's TLS takes no client CA")
	}
	tc, err := tlsconfig.ServerConfig(s.d.TLS)
	if err != nil {
		return nil, nil, fmt.Errorf("api: the bootstrap's TLS: %w", err)
	}
	al, err := listen(s.d.Addr, tc)
	if err != nil {
		return nil, nil, err
	}
	ml, err := listen(s.d.MetricsAddr, tc)
	if err != nil {
		return nil, nil, errors.Join(err, al.Close())
	}
	var sl net.Listener
	if s.d.Socket != "" {
		if sl, err = listenSocket(s.d.Socket); err != nil {
			return nil, nil, errors.Join(err, al.Close(), ml.Close())
		}
		s.serving.Go(func() { s.serve(s.api, sl) })
	}
	s.serving.Go(func() { s.serve(s.api, al) })
	s.serving.Go(func() { s.serve(s.met, ml) })
	return al.Addr(), ml.Addr(), nil
}

// listen opens the TCP address addr, served over tc unless it bound a
// loopback address, 127.0.0.0/8 or ::1, an IPv4-mapped one read as its IPv4
// address. With tc nil, an address but loopback is refused.
func listen(addr string, tc *tls.Config) (net.Listener, error) {
	l, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	if l.Addr().(*net.TCPAddr).IP.IsLoopback() {
		return l, nil
	}
	if tc == nil {
		return nil, errors.Join(fmt.Errorf("api: %s is not a loopback address, so it is served over TLS, which needs the API's TLS certificate and key (cert_file, key_file)", addr), l.Close())
	}
	return tls.NewListener(l, tc), nil
}

// listenSocket opens the local socket at path, readable and writable by the
// server's user only, replacing a socket a node that did not stop left
// there. Only a dial the path refuses marks the socket stale: a socket a node
// serves, one whose dial fails any other way (a full accept backlog, a mode
// the server's user may not connect through), or any other file at path
// refuses the start.
func listenSocket(path string) (net.Listener, error) {
	if fi, err := os.Lstat(path); err == nil && fi.Mode().Type() == fs.ModeSocket {
		c, err := net.Dial("unix", path)
		switch {
		case err == nil:
			return nil, errors.Join(fmt.Errorf("api: a node is serving the local socket %s", path), c.Close())
		case !errors.Is(err, syscall.ECONNREFUSED):
			return nil, fmt.Errorf("api: the local socket %s is not stale: %w", path, err)
		}
		if err := os.Remove(path); err != nil {
			return nil, err
		}
	}
	return listenPrivate(path)
}

func (s *Server) serve(srv *http.Server, l net.Listener) {
	if err := srv.Serve(l); !errors.Is(err, http.ErrServerClosed) {
		s.d.Logger.Error("node api: listener failed", "addr", l.Addr().String(), "err", err)
	}
}

// Stop closes the listeners, waiting for the requests in flight until ctx
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
