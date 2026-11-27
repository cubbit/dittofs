package s3

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/cubbit/dittofs/pkg/remote"
)

// sink is a Recorder that keeps what it was given.
type sink struct {
	opened, foreign atomic.Int64

	mu    sync.Mutex
	calls []string
}

func (s *sink) Call(op, result, cause string, _ time.Duration, _ int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, op+" "+result+" "+cause)
}

func (s *sink) ConnOpened() { s.opened.Add(1) }
func (s *sink) ForeignKey() { s.foreign.Add(1) }

// tlsServer serves h over TLS on 127.0.0.1:0, offering HTTP/2 when h2 is set,
// with cert in place of httptest's when it is not nil.
func tlsServer(t *testing.T, h http.HandlerFunc, h2 bool, cert *tls.Certificate) *httptest.Server {
	t.Helper()

	srv := httptest.NewUnstartedServer(h)
	srv.EnableHTTP2 = h2
	if cert != nil {
		srv.TLS = &tls.Config{Certificates: []tls.Certificate{*cert}}
	}
	srv.StartTLS()
	t.Cleanup(srv.Close)

	return srv
}

// testClient is a client of srv's endpoint trusting srv's certificate, with
// static fake credentials and limit 4, as mod changes it.
func testClient(t *testing.T, srv *httptest.Server, mod func(*ClientConfig)) (*Client, *sink) {
	t.Helper()

	roots := x509.NewCertPool()
	roots.AddCert(srv.Certificate())
	cfg := ClientConfig{
		Endpoint:        srv.URL,
		Bucket:          "b",
		AccessKeyID:     "AKIDFAKE",
		SecretAccessKey: "fake-secret",
		TrustRoots:      roots,
		Limit:           4,
	}
	if mod != nil {
		mod(&cfg)
	}

	s := &sink{}
	c, err := NewClient(cfg, Deps{Logger: slog.New(slog.DiscardHandler), Recorder: s})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })

	return c, s
}

// ok answers 200, with an empty listing to a ListObjectsV2.
func ok(w http.ResponseWriter, r *http.Request) {
	if r.URL.Query().Has("list-type") {
		_, _ = io.WriteString(w, `<ListBucketResult></ListBucketResult>`)
	}
}

func TestNewClientRefuses(t *testing.T) {
	good := ClientConfig{Endpoint: "https://127.0.0.1:1", Bucket: "b", Limit: 1}
	deps := Deps{Logger: slog.New(slog.DiscardHandler), Recorder: &sink{}}

	for _, tc := range []struct {
		name string
		mod  func(*ClientConfig, *Deps)
	}{
		{"an http endpoint", func(c *ClientConfig, _ *Deps) { c.Endpoint = "http://127.0.0.1:1" }},
		{"an endpoint with no host", func(c *ClientConfig, _ *Deps) { c.Endpoint = "https://" }},
		{"an endpoint with user info", func(c *ClientConfig, _ *Deps) { c.Endpoint = "https://k:s@127.0.0.1:1" }},
		{"an empty bucket", func(c *ClientConfig, _ *Deps) { c.Bucket = "" }},
		{"a limit of 0", func(c *ClientConfig, _ *Deps) { c.Limit = 0 }},
		{"a negative limit", func(c *ClientConfig, _ *Deps) { c.Limit = -1 }},
		{"a nil recorder", func(_ *ClientConfig, d *Deps) { d.Recorder = nil }},
		{"a nil logger", func(_ *ClientConfig, d *Deps) { d.Logger = nil }},
	} {
		cfg, d := good, deps
		tc.mod(&cfg, &d)
		c, err := NewClient(cfg, d)
		if !errors.Is(err, remote.ErrInvalid) || c != nil {
			t.Errorf("NewClient with %s: got (%v, %v), want (nil, ErrInvalid)", tc.name, c, err)
		}
		if err != nil && strings.Contains(err.Error(), "k:s") {
			t.Errorf("NewClient with %s: error %q prints the endpoint's user info", tc.name, err)
		}
	}

	if c, err := NewClient(good, deps); err != nil {
		t.Errorf("NewClient with a good config: %v", err)
	} else {
		_ = c.Close()
	}
}

// A server offering HTTP/2 is spoken to in HTTP/1.1.
func TestClientSpeaksHTTP11(t *testing.T) {
	var proto atomic.Int64
	srv := tlsServer(t, func(w http.ResponseWriter, r *http.Request) {
		proto.Store(int64(r.ProtoMajor))
		ok(w, r)
	}, true, nil)
	if !slices.Contains(srv.TLS.NextProtos, "h2") {
		t.Fatalf("the server offers %q, want h2 among them", srv.TLS.NextProtos)
	}

	c, _ := testClient(t, srv, nil)
	if err := call(t.Context(), c.api, opGet); err != nil {
		t.Fatalf("get: %v", err)
	}
	if got := proto.Load(); got != 1 {
		t.Errorf("the request arrived as HTTP/%d, want HTTP/1", got)
	}
}

// With limit L, two bursts of 2L concurrent requests, each held until L have
// arrived, never have more than L in flight and open L connections in all.
func TestClientConnectionCaps(t *testing.T) {
	const limit = 4

	var (
		mu       sync.Mutex
		inflight int
		most     int
		gate     chan struct{}
	)
	arrived := make(chan struct{}, 2*limit)
	srv := tlsServer(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		inflight++
		most = max(most, inflight)
		g := gate
		mu.Unlock()

		arrived <- struct{}{}
		<-g

		mu.Lock()
		inflight--
		mu.Unlock()
		ok(w, r)
	}, false, nil)
	c, s := testClient(t, srv, func(cfg *ClientConfig) { cfg.Limit = limit })

	for burst := 1; burst <= 2; burst++ {
		mu.Lock()
		gate = make(chan struct{})
		mu.Unlock()

		var wg sync.WaitGroup
		errs := make(chan error, 2*limit)
		for range 2 * limit {
			wg.Go(func() { errs <- call(t.Context(), c.api, opGet) })
		}
		for range limit {
			select {
			case <-arrived:
			case <-time.After(30 * time.Second):
				t.Fatalf("burst %d: fewer than %d requests arrived", burst, limit)
			}
		}
		mu.Lock()
		close(gate)
		mu.Unlock()
		wg.Wait()
		close(errs)
		for err := range errs {
			if err != nil {
				t.Fatalf("burst %d: get: %v", burst, err)
			}
		}
		for len(arrived) > 0 {
			<-arrived
		}

		mu.Lock()
		m := most
		mu.Unlock()
		if m > limit {
			t.Errorf("burst %d: %d requests in flight at once, want at most %d", burst, m, limit)
		}
		if got := s.opened.Load(); got != limit {
			t.Errorf("after burst %d: %d connections opened, want %d", burst, got, limit)
		}
	}
}

// Every request is signed UNSIGNED-PAYLOAD for the default region with the
// configured key, and carries no checksum header the store did not ask for.
func TestClientSignsUnsignedPayloadAndSendsNoChecksum(t *testing.T) {
	var (
		mu   sync.Mutex
		seen []http.Header
	)
	srv := tlsServer(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.Header.Clone())
		mu.Unlock()
		ok(w, r)
	}, false, nil)
	c, _ := testClient(t, srv, nil)

	ops := []opKind{opPut, opGet, opDelete, opList}
	for _, op := range ops {
		if err := call(t.Context(), c.api, op); err != nil {
			t.Fatalf("%s: %v", op, err)
		}
	}

	mu.Lock()
	defer mu.Unlock()
	if len(seen) != len(ops) {
		t.Fatalf("the server saw %d requests, want %d", len(seen), len(ops))
	}
	for i, h := range seen {
		if got := h.Get("X-Amz-Content-Sha256"); got != "UNSIGNED-PAYLOAD" {
			t.Errorf("%s: x-amz-content-sha256 %q, want UNSIGNED-PAYLOAD", ops[i], got)
		}
		if got := h.Get("Authorization"); !strings.Contains(got, "Credential=AKIDFAKE/") || !strings.Contains(got, "/us-east-1/s3/") {
			t.Errorf("%s: Authorization %q, want the fake key's credential scoped to us-east-1", ops[i], got)
		}
		for k := range h {
			if lk := strings.ToLower(k); strings.HasPrefix(lk, "x-amz-checksum") || lk == "x-amz-sdk-checksum-algorithm" || lk == "content-md5" || lk == "x-amz-trailer" {
				t.Errorf("%s: carries %s, which the store did not ask for", ops[i], k)
			}
		}
	}
}

// localhostCert is a self-signed certificate for localhost.
func localhostCert(t *testing.T) *tls.Certificate {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "localhost"},
		DNSNames:     []string{"localhost"},
		IPAddresses:  []net.IP{net.IPv4(127, 0, 0, 1)},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		IsCA:         true,
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, key.Public(), key)
	if err != nil {
		t.Fatal(err)
	}

	return &tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

// A bucket, dotted or not, is addressed on the path of the endpoint's host,
// never as a subdomain of it. The endpoint names a host, not an address, since
// the SDK addresses an IP endpoint by path whatever it is told.
func TestClientAddressesTheBucketByPath(t *testing.T) {
	for _, bucket := range []string{"b", "bucket.with.dots"} {
		var (
			mu    sync.Mutex
			paths []string
		)
		srv := tlsServer(t, func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			paths = append(paths, r.Host+" "+r.URL.Path)
			mu.Unlock()
			ok(w, r)
		}, false, localhostCert(t))
		endpoint := strings.Replace(srv.URL, "127.0.0.1", "localhost", 1)
		c, _ := testClient(t, srv, func(cfg *ClientConfig) { cfg.Endpoint, cfg.Bucket = endpoint, bucket })

		out, err := c.api.GetObject(t.Context(), &s3.GetObjectInput{Bucket: aws.String(bucket), Key: aws.String("blocks/00")})
		if err == nil {
			_ = out.Body.Close()
		}

		mu.Lock()
		want := []string{strings.TrimPrefix(endpoint, "https://") + " /" + bucket + "/blocks/00"}
		if !slices.Equal(paths, want) {
			t.Errorf("bucket %q: the server saw %q (get: %v), want %q", bucket, paths, err, want)
		}
		mu.Unlock()
	}
}

// A 3xx is cause access, and the host it names sees no request: the signed
// request is not followed there.
func TestClientFollowsNoRedirect(t *testing.T) {
	for _, tc := range []struct {
		op     opKind
		status int
	}{
		{opGet, http.StatusMovedPermanently},
		{opPut, http.StatusTemporaryRedirect},
		{opPut, http.StatusPermanentRedirect},
	} {
		var elsewhere atomic.Int64
		other := tlsServer(t, func(w http.ResponseWriter, r *http.Request) {
			elsewhere.Add(1)
			ok(w, r)
		}, false, nil)
		srv := tlsServer(t, func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, other.URL+r.URL.Path, tc.status)
		}, false, nil)
		c, _ := testClient(t, srv, nil)

		ctx := t.Context()
		err := fromSDK(ctx, tc.op, recorded{}, nil, call(ctx, c.api, tc.op))
		if d, ok := errors.AsType[*remote.DeniedError](err); !ok || d.Cause != remote.CauseAccess {
			t.Errorf("%s answered %d: got %v, want ErrDenied of cause access", tc.op, tc.status, err)
		}
		if n := elsewhere.Load(); n != 0 {
			t.Errorf("%s answered %d: the redirect's host saw %d requests, want 0", tc.op, tc.status, n)
		}
	}
}

// A failing request reaches the service once: the SDK does not retry it.
func TestClientMakesOneAttempt(t *testing.T) {
	for _, tc := range []struct {
		status int
		body   string
		want   error
	}{
		{http.StatusInternalServerError, xmlError("InternalError"), remote.ErrTransient},
		{http.StatusServiceUnavailable, xmlError("SlowDown"), remote.ErrThrottled},
	} {
		var n atomic.Int64
		srv := tlsServer(t, func(w http.ResponseWriter, _ *http.Request) {
			n.Add(1)
			w.Header().Set("Content-Type", "application/xml")
			w.WriteHeader(tc.status)
			_, _ = io.WriteString(w, tc.body)
		}, false, nil)
		c, _ := testClient(t, srv, nil)

		ctx := t.Context()
		if err := fromSDK(ctx, opGet, recorded{}, nil, call(ctx, c.api, opGet)); !errors.Is(err, tc.want) {
			t.Errorf("a get answered %d: got %v, want %v", tc.status, err, tc.want)
		}
		if got := n.Load(); got != 1 {
			t.Errorf("a get answered %d reached the server %d times, want 1", tc.status, got)
		}
	}
}

// A service whose certificate the client's roots do not hold is refused
// before its handler sees a request, as ErrTransient.
func TestClientVerifiesTheServiceCertificate(t *testing.T) {
	block, _ := pem.Decode([]byte(testCert(t)))
	other, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	otherRoots := x509.NewCertPool()
	otherRoots.AddCert(other)

	for _, tc := range []struct {
		name  string
		roots *x509.CertPool
	}{
		{"trust roots holding another root", otherRoots},
		{"the system's roots", nil},
	} {
		var n atomic.Int64
		srv := tlsServer(t, func(w http.ResponseWriter, r *http.Request) {
			n.Add(1)
			ok(w, r)
		}, false, nil)
		c, _ := testClient(t, srv, func(cfg *ClientConfig) { cfg.TrustRoots = tc.roots })

		ctx := t.Context()
		if err := fromSDK(ctx, opGet, recorded{}, nil, call(ctx, c.api, opGet)); !errors.Is(err, remote.ErrTransient) {
			t.Errorf("%s: got %v, want ErrTransient", tc.name, err)
		}
		if got := n.Load(); got != 0 {
			t.Errorf("%s: the handler saw %d requests, want 0", tc.name, got)
		}
	}
}

// A connection the service closes is counted once; one the client closes is
// not.
func TestClientCountsConnectionsTheServiceClosed(t *testing.T) {
	var first atomic.Bool
	srv := tlsServer(t, func(w http.ResponseWriter, r *http.Request) {
		if first.CompareAndSwap(false, true) {
			conn, _, err := http.NewResponseController(w).Hijack()
			if err != nil {
				t.Errorf("hijack: %v", err)
				return
			}
			_ = conn.Close()
			return
		}
		ok(w, r)
	}, false, nil)
	c, s := testClient(t, srv, nil)

	ctx := context.Background()
	if err := call(ctx, c.api, opGet); err == nil {
		t.Fatal("a get the service hung up on: got nil, want an error")
	}
	if err := call(ctx, c.api, opGet); err != nil {
		t.Fatalf("a second get: %v", err)
	}
	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if got := c.peerClosed.Load(); got != 1 {
		t.Errorf("%d connections counted as closed by the service, want 1", got)
	}
	if got := s.opened.Load(); got != 2 {
		t.Errorf("%d connections opened, want 2", got)
	}
}
