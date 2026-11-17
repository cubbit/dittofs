package apiclient_test

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"maps"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/cubbit/dittofs/internal/node/api"
	"github.com/cubbit/dittofs/internal/node/api/auth"
	"github.com/cubbit/dittofs/internal/node/nodetest"
	"github.com/cubbit/dittofs/pkg/apiclient"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/metadata/kvstore"
	"github.com/cubbit/dittofs/pkg/metrics"
	"github.com/cubbit/dittofs/pkg/node/apiv1"
	"github.com/cubbit/dittofs/pkg/sim"
	"github.com/google/uuid"
)

// served is the API over a harness's store, reached through a front.
type served struct {
	store kvstore.ControlPlane
	nodes kvstore.Nodes
	// clock is the server's, on which a parked poll waits.
	clock *sim.TestClock
	front *front
	// url is the front's http URL on 127.0.0.1.
	url string
}

// admin authenticates every request as one admin account, so the
// idempotency keys a client sends are all its own.
//
// decision: the caller is a fixed input, as the API's own tests make it; the
// real Authenticate arrives with sign-in.
func admin() func(*http.Request) (auth.Caller, error) {
	p := entity.Principal{Kind: entity.PrincipalUser, ID: entity.NewPrincipalID()}
	return func(*http.Request) (auth.Caller, error) {
		return auth.Caller{Principal: p, Name: "eli", Credential: "session-1", Role: entity.RoleAdmin}, nil
	}
}

// serve starts the API on 127.0.0.1:0 over an in-memory harness, with
// authenticate, nil refusing every caller 401, and a front on 127.0.0.1:0
// passing every request to it. The test fails if a fault added to the front
// never matched a request.
//
// ponytail: the client reaches the API through the front's own listeners,
// plain, TLS and a socket, not the node's. Drive the node's own socket and
// TLS listener instead once the API serves them.
func serve(t *testing.T, authenticate func(*http.Request) (auth.Caller, error)) *served {
	t.Helper()
	h := nodetest.New(t, nodetest.InMemory())
	srv := api.New(api.Deps{
		Ready:        func() string { return "ready" },
		Live:         func() bool { return true },
		Addr:         "127.0.0.1:0",
		MetricsAddr:  "127.0.0.1:0",
		Metrics:      metrics.New("test", "test"),
		Logger:       slog.New(slog.NewTextHandler(t.Output(), nil)),
		Authenticate: authenticate,
		Store:        h.Store().ControlPlane(),
		SigningKey:   func(context.Context) ([]byte, error) { return []byte("installation"), nil },
		Nodes:        h.Store().Nodes(),
		Clock:        h.Clock(),
		Node:         entity.NodeID{'c', 'l', 'i'},
	})
	addr, _, err := srv.Start()
	if err != nil {
		t.Fatalf("start the API: %v", err)
	}
	t.Cleanup(func() {
		if err := srv.Stop(context.Background()); err != nil {
			t.Errorf("stop the API: %v", err)
		}
	})
	f := &front{backend: "http://" + addr.String(), tr: &http.Transport{}}
	t.Cleanup(f.tr.CloseIdleConnections)
	hs := httptest.NewServer(f)
	t.Cleanup(hs.Close)
	t.Cleanup(func() {
		f.mu.Lock()
		defer f.mu.Unlock()
		for _, ft := range f.faults {
			t.Errorf("a fault added to the front never matched a request: %+v", ft)
		}
	})
	return &served{store: h.Store().ControlPlane(), nodes: h.Store().Nodes(), clock: h.Clock(), front: f, url: hs.URL}
}

// socket serves the front on a unix socket too, and returns its path.
func (s *served) socket(t *testing.T) string {
	t.Helper()
	// A directory of its own under the temporary root, not t.TempDir(),
	// whose path can pass the length a socket's path may have.
	dir, err := os.MkdirTemp("", "apiclient")
	if err != nil {
		t.Fatalf("make the socket's directory: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	path := filepath.Join(dir, "api.sock")
	l, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("listen on %s: %v", path, err)
	}
	hs := &http.Server{Handler: s.front}
	go func() { _ = hs.Serve(l) }()
	t.Cleanup(func() { _ = hs.Close() })
	return path
}

// client is a client of url waiting its retries on clock.
func client(t *testing.T, url string, clock apiclient.Clock, opts ...apiclient.Option) *apiclient.Client {
	t.Helper()
	c, err := apiclient.New(url, append([]apiclient.Option{apiclient.WithClock(clock)}, opts...)...)
	if err != nil {
		t.Fatalf("New(%q): %v", url, err)
	}
	return c
}

func newClock() *sim.TestClock { return sim.NewTestClock(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)) }

// namespace writes a namespace through the store, as the namespace routes,
// which do not exist yet, would.
func (s *served) namespace(t *testing.T) string {
	t.Helper()
	id := entity.NamespaceID(uuid.New())
	if _, err := s.store.PutNamespace(t.Context(), entity.Namespace{ID: id, Store: uuid.New()}, 0); err != nil {
		t.Fatalf("put namespace: %v", err)
	}
	return uuid.UUID(id).String()
}

// shareIDs is the ID of every share the store holds.
func (s *served) shareIDs(t *testing.T) []string {
	t.Helper()
	var ids []string
	for sh, err := range s.store.Shares(t.Context()) {
		if err != nil {
			t.Fatalf("list shares: %v", err)
		}
		ids = append(ids, sh.ID.String())
	}
	return ids
}

var policy = apiv1.ExportPolicySpec{Flavors: []string{"sys"}, Clients: []apiv1.ClientRule{{CIDR: "10.0.0.0/8", Access: "read-write"}}}

func newShare(name, ns string) apiv1.NewShare {
	return apiv1.NewShare{Spec: apiv1.ShareSpec{Name: name, Path: "/" + name, Namespace: ns}, ExportPolicy: policy}
}

// front is a pass-through wrapper in front of the API: it records each
// request it is sent, and applies the next fault whose match admits it.
type front struct {
	backend string
	tr      *http.Transport

	mu     sync.Mutex
	seen   []seen
	faults []fault
}

// seen is one request the front was sent.
type seen struct {
	method, uri string
	header      http.Header
	body        string
}

// fault is what the front does, once, to the first request match admits:
// answer it with status, header and body without passing it on, when status
// is set; or hold it until hold closes and then pass it on; or pass it on
// and then close the connection without answering, the answer lost; or pass
// it on and answer what edit makes of the answer's body, given the request.
type fault struct {
	match  func(*http.Request) bool
	status int
	header http.Header
	body   string
	hold   chan struct{}
	drop   bool
	edit   func(*http.Request, []byte) []byte
}

func (f *front) add(ft fault) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.faults = append(f.faults, ft)
}

// requests is every request the front was sent whose method is method.
func (f *front) requests(method string) []seen {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.DeleteFunc(slices.Clone(f.seen), func(s seen) bool { return s.method != method })
}

func (f *front) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	f.seen = append(f.seen, seen{r.Method, r.URL.RequestURI(), r.Header.Clone(), string(body)})
	var ft fault
	for i, x := range f.faults {
		if x.match(r) {
			ft = x
			f.faults = slices.Delete(f.faults, i, i+1)
			break
		}
	}
	f.mu.Unlock()
	if ft.status != 0 {
		maps.Copy(w.Header(), ft.header)
		w.WriteHeader(ft.status)
		_, _ = io.WriteString(w, ft.body)
		return
	}
	if ft.hold != nil {
		select {
		case <-ft.hold:
		case <-r.Context().Done():
			return
		}
	}
	req, err := http.NewRequestWithContext(r.Context(), r.Method, f.backend+r.URL.RequestURI(), bytes.NewReader(body))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	req.Header = r.Header.Clone()
	resp, err := f.tr.RoundTrip(req)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	b, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if ft.drop {
		if conn, _, err := http.NewResponseController(w).Hijack(); err == nil {
			_ = conn.Close()
		}
		return
	}
	if ft.edit != nil {
		b = ft.edit(r, b)
	}
	maps.Copy(w.Header(), resp.Header)
	w.Header().Set("Content-Length", strconv.Itoa(len(b)))
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(b)
}

// on matches a request by its method and its path.
func on(method, path string) func(*http.Request) bool {
	return func(r *http.Request) bool { return r.Method == method && r.URL.Path == path }
}
