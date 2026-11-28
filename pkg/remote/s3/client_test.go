package s3

import (
	"cmp"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
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

// sink is a Recorder that keeps the connections it was told of.
type sink struct{ opened atomic.Int64 }

func (*sink) Call(string, string, string, time.Duration, int64) {}
func (s *sink) ConnOpened()                                     { s.opened.Add(1) }
func (*sink) ForeignKey()                                       {}
func (*sink) Capability(string, string, bool)                   {}

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
		MaxBlockSize:    testMaxBlock,
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
	good := ClientConfig{Endpoint: "https://127.0.0.1:1", Bucket: "b", AccessKeyID: "AKIDFAKE", SecretAccessKey: "fake-secret", Limit: 1, MaxBlockSize: 1}
	deps := Deps{Logger: slog.New(slog.DiscardHandler), Recorder: &sink{}}

	for _, tc := range []struct {
		name string
		mod  func(*ClientConfig, *Deps)
	}{
		{"an http endpoint", func(c *ClientConfig, _ *Deps) { c.Endpoint = "http://127.0.0.1:1" }},
		{"an endpoint with no host", func(c *ClientConfig, _ *Deps) { c.Endpoint = "https://" }},
		{"an endpoint with user info", func(c *ClientConfig, _ *Deps) { c.Endpoint = "https://k:s@127.0.0.1:1" }},
		{"an empty bucket", func(c *ClientConfig, _ *Deps) { c.Bucket = "" }},
		{"an empty access key", func(c *ClientConfig, _ *Deps) { c.AccessKeyID = "" }},
		{"an empty secret", func(c *ClientConfig, _ *Deps) { c.SecretAccessKey = "" }},
		{"a limit of 0", func(c *ClientConfig, _ *Deps) { c.Limit = 0 }},
		{"a negative limit", func(c *ClientConfig, _ *Deps) { c.Limit = -1 }},
		{"a largest block of 0", func(c *ClientConfig, _ *Deps) { c.MaxBlockSize = 0 }},
		{"a negative largest block", func(c *ClientConfig, _ *Deps) { c.MaxBlockSize = -1 }},
		{"a nil recorder", func(_ *ClientConfig, d *Deps) { d.Recorder = nil }},
		{"a nil logger", func(_ *ClientConfig, d *Deps) { d.Logger = nil }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, d := good, deps
			tc.mod(&cfg, &d)
			c, err := NewClient(cfg, d)
			if !errors.Is(err, remote.ErrInvalid) || c != nil {
				t.Errorf("NewClient: got (%v, %v), want (nil, ErrInvalid)", c, err)
			}
			if err != nil && (strings.Contains(err.Error(), "k:s") || strings.Contains(err.Error(), "fake-secret")) {
				t.Errorf("NewClient: error %q prints a credential", err)
			}
		})
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
// arrived, open L connections in all: a cap above L opens more in the first
// burst, and an idle pool below L reopens some in the second. Over HTTP/1.1 a
// connection carries one request at a time, so the count is the cap.
func TestClientConnectionCaps(t *testing.T) {
	const limit = 4

	var (
		mu   sync.Mutex
		gate chan struct{}
	)
	arrived := make(chan struct{}, 2*limit)
	srv := tlsServer(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		g := gate
		mu.Unlock()

		arrived <- struct{}{}
		select {
		case <-g:
		case <-r.Context().Done():
			return
		}
		ok(w, r)
	}, false, nil)
	c, s := testClient(t, srv, func(cfg *ClientConfig) { cfg.Limit = limit })

	for burst := 1; burst <= 2; burst++ {
		mu.Lock()
		gate = make(chan struct{})
		mu.Unlock()

		var wg sync.WaitGroup
		for range 2 * limit {
			wg.Go(func() {
				if err := call(t.Context(), c.api, opGet); err != nil {
					t.Errorf("burst %d: get: %v", burst, err)
				}
			})
		}
		for range limit {
			select {
			case <-arrived:
			case <-time.After(30 * time.Second):
				// A backstop: a cap below L never lets L arrive. t.Context
				// ends before the cleanup closes the server, which frees
				// the handlers holding.
				t.Fatalf("burst %d: fewer than %d requests arrived", burst, limit)
			}
		}
		mu.Lock()
		close(gate)
		mu.Unlock()
		wg.Wait()
		for len(arrived) > 0 {
			<-arrived
		}

		if got := s.opened.Load(); got != limit {
			t.Errorf("after burst %d: %d connections opened, want %d", burst, got, limit)
		}
	}
}

// Every request is signed UNSIGNED-PAYLOAD with the configured key for the
// configured region, us-east-1 by default, and carries no checksum header the
// store did not ask for. The SDK through s3.New sends none by default today;
// the check catches the pins set to WhenSupported, or a default that changes.
func TestClientSignsUnsignedPayloadAndSendsNoChecksum(t *testing.T) {
	for _, region := range []string{"", "eu-south-1"} {
		signsFor(t, region)
	}
}

func signsFor(t *testing.T, region string) {
	t.Helper()

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
	c, _ := testClient(t, srv, func(cfg *ClientConfig) { cfg.Region = region })
	scope := cmp.Or(region, "us-east-1")

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
		if got := h.Get("Authorization"); !strings.Contains(got, "Credential=AKIDFAKE/") || !strings.Contains(got, "/"+scope+"/s3/") {
			t.Errorf("%s: Authorization %q, want the fake key's credential scoped to %s", ops[i], got, scope)
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
// never as a subdomain of it. The endpoint names a host, not an address, and
// one bucket is a name a subdomain could carry, since the SDK addresses an IP
// endpoint, and a bucket too short or dotted for a host name, by path whatever
// it is told.
func TestClientAddressesTheBucketByPath(t *testing.T) {
	for _, bucket := range []string{"bkt", "bucket.with.dots"} {
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
// before its handler sees a request, as ErrTransient, and opens no connection
// the pool counts.
func TestClientVerifiesTheServiceCertificate(t *testing.T) {
	otherRoots := x509.NewCertPool()
	if !otherRoots.AppendCertsFromPEM([]byte(testCert(t))) {
		t.Fatal("the other root does not parse")
	}

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
		c, s := testClient(t, srv, func(cfg *ClientConfig) { cfg.TrustRoots = tc.roots })

		ctx := t.Context()
		if err := fromSDK(ctx, opGet, recorded{}, nil, call(ctx, c.api, opGet)); !errors.Is(err, remote.ErrTransient) {
			t.Errorf("%s: got %v, want ErrTransient", tc.name, err)
		}
		if got := n.Load(); got != 0 {
			t.Errorf("%s: the handler saw %d requests, want 0", tc.name, got)
		}
		if got := s.opened.Load(); got != 0 {
			t.Errorf("%s: %d connections counted as opened, want 0: the refused handshake opened none", tc.name, got)
		}
	}
}

// A connection the service hangs up on, and one it closes with a reply
// carrying Connection: close, are each counted once.
func TestClientCountsConnectionsTheServiceClosed(t *testing.T) {
	var n atomic.Int64
	srv := tlsServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch n.Add(1) {
		case 1:
			conn, _, err := http.NewResponseController(w).Hijack()
			if err != nil {
				t.Errorf("hijack: %v", err)
				return
			}
			_ = conn.Close()
			return
		case 2, 3:
			w.Header().Set("Connection", "close")
		}
		ok(w, r)
	}, false, nil)
	c, s := testClient(t, srv, nil)

	ctx := t.Context()
	if err := call(ctx, c.api, opGet); err == nil {
		t.Fatal("a get the service hung up on: got nil, want an error")
	}
	for i := range 3 {
		if err := call(ctx, c.api, opGet); err != nil {
			t.Fatalf("get %d after the hang-up: %v", i+2, err)
		}
	}

	if got := c.peerClosed.Load(); got != 3 {
		t.Errorf("%d connections counted as closed by the service, want 3", got)
	}
	if got := s.opened.Load(); got != 4 {
		t.Errorf("%d connections opened, want 4", got)
	}
}

// A connection's end is counted once, and not at all once the client has
// started closing it, which a read in flight can see as the end: a closed
// peer, read twice, counts one; read after the client began its close, none.
func TestCountedConnCountsOnlyTheServiceClosing(t *testing.T) {
	for _, tc := range []struct {
		name    string
		closing bool
		want    int64
	}{
		{"the service closed it", false, 1},
		{"the client was closing it", true, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ln, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = ln.Close() })

			raw, err := net.Dial("tcp", ln.Addr().String())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = raw.Close() })
			peer, err := ln.Accept()
			if err != nil {
				t.Fatal(err)
			}
			_ = peer.Close()

			var count atomic.Int64
			k := &countedConn{Conn: raw, peer: &count}
			k.done.Store(tc.closing) // what Close sets before it closes
			for range 2 {
				if _, err := k.Read(make([]byte, 1)); !errors.Is(err, io.EOF) {
					t.Fatalf("read of a closed peer: got %v, want io.EOF", err)
				}
			}
			if got := count.Load(); got != tc.want {
				t.Errorf("counted %d closes, want %d", got, tc.want)
			}
		})
	}
}

// Close closes the idle connections of the real transport also when a test's
// wrapper, which need not pass Close on, sits in front of it.
func TestClientCloseUnderAWrappedTransport(t *testing.T) {
	closed := make(chan struct{}, 1)
	srv := httptest.NewUnstartedServer(http.HandlerFunc(ok))
	srv.Config.ConnState = func(_ net.Conn, st http.ConnState) {
		if st == http.StateClosed {
			closed <- struct{}{}
		}
	}
	srv.StartTLS()
	t.Cleanup(srv.Close)

	roots := x509.NewCertPool()
	roots.AddCert(srv.Certificate())
	c, err := NewClient(ClientConfig{Endpoint: srv.URL, Bucket: "b", AccessKeyID: "AKIDFAKE", SecretAccessKey: "fake-secret", TrustRoots: roots, Limit: 1, MaxBlockSize: 1},
		Deps{Logger: slog.New(slog.DiscardHandler), Recorder: &sink{}, Transport: func(rt http.RoundTripper) http.RoundTripper { return passThrough{rt} }})
	if err != nil {
		t.Fatal(err)
	}
	if err := call(t.Context(), c.api, opGet); err != nil {
		t.Fatalf("get: %v", err)
	}
	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	select {
	case <-closed:
	case <-time.After(30 * time.Second):
		t.Fatal("the idle connection stayed open after Close")
	}
}

// passThrough wraps a transport and hides its CloseIdleConnections, as a
// wrapping test transport may.
type passThrough struct{ next http.RoundTripper }

func (p passThrough) RoundTrip(r *http.Request) (*http.Response, error) { return p.next.RoundTrip(r) }

// An S3 Express-shaped bucket name sends the call alone, with no session
// request of the SDK's own ahead of it.
func TestClientSendsNoExpressSession(t *testing.T) {
	const bucket = "bkt--usw2-az1--x-s3"

	var (
		mu   sync.Mutex
		seen []string
	)
	srv := tlsServer(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.Method+" "+r.URL.RequestURI())
		mu.Unlock()
		ok(w, r)
	}, false, nil)
	c, _ := testClient(t, srv, func(cfg *ClientConfig) { cfg.Bucket = bucket })

	out, err := c.api.GetObject(t.Context(), &s3.GetObjectInput{Bucket: aws.String(bucket), Key: aws.String("blocks/00")})
	if err == nil {
		_ = out.Body.Close()
	}

	mu.Lock()
	defer mu.Unlock()
	if want := []string{"GET /" + bucket + "/blocks/00?x-id=GetObject"}; !slices.Equal(seen, want) {
		t.Errorf("the server saw %q (get: %v), want %q", seen, err, want)
	}
}
