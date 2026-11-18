package api_test

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
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cubbit/dittofs/internal/node/api"
	"github.com/cubbit/dittofs/internal/node/api/auth"
	"github.com/cubbit/dittofs/internal/tlsconfig"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/metadata/kv/kvtest"
	"github.com/cubbit/dittofs/pkg/metrics"
	"github.com/cubbit/dittofs/pkg/node/apiv1"
	"github.com/cubbit/dittofs/pkg/sim"
	"github.com/cubbit/dittofs/pkg/vfs/vfserr"
	"github.com/google/uuid"
)

// certFiles writes a self-signed certificate for 127.0.0.1 and its key, and
// returns them and a pool trusting the certificate.
func certFiles(t *testing.T) (tlsconfig.Config, *x509.CertPool) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "node"},
		IPAddresses: []net.IP{net.IPv4(127, 0, 0, 1)}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	kder, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	c := tlsconfig.Config{CertFile: filepath.Join(dir, "cert.pem"), KeyFile: filepath.Join(dir, "key.pem")}
	for path, b := range map[string]*pem.Block{c.CertFile: {Type: "CERTIFICATE", Bytes: der}, c.KeyFile: {Type: "EC PRIVATE KEY", Bytes: kder}} {
		if err := os.WriteFile(path, pem.EncodeToMemory(b), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(cert)
	return c, pool
}

// answers200 reports whether GET url answered 200 through client.
func answers200(client *http.Client, url string) bool {
	resp, err := client.Get(url)
	if err != nil {
		return false
	}
	_ = resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

// Both listeners are served over TLS on every address but loopback, and
// plainly on a loopback one, an IPv4-mapped loopback address and localhost
// among them. A host with no IPv6 loopback skips the ::1 row.
func TestListenersServeTLSButOnLoopback(t *testing.T) {
	certs, pool := certFiles(t)
	tr := &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool}}
	t.Cleanup(tr.CloseIdleConnections)
	client := &http.Client{Transport: tr}
	for _, tc := range []struct {
		addr    string
		wantTLS bool
	}{
		{"0.0.0.0:0", true}, {":0", true},
		{"127.0.0.1:0", false}, {"localhost:0", false}, {"[::1]:0", false}, {"[::ffff:127.0.0.1]:0", false},
	} {
		t.Run(tc.addr, func(t *testing.T) {
			if tc.addr == "[::1]:0" {
				l, err := net.Listen("tcp", tc.addr)
				if err != nil {
					t.Skipf("the host has no IPv6 loopback ::1: %v", err)
				}
				_ = l.Close()
			}
			s := api.New(api.Deps{Ready: func() string { return "ready" }, Live: func() bool { return true },
				Addr: tc.addr, MetricsAddr: tc.addr, TLS: certs, Metrics: metrics.New("test", "test"), Logger: slog.New(slog.NewTextHandler(t.Output(), nil))})
			a, m, err := s.Start()
			if err != nil {
				t.Fatalf("Start on %s: %v", tc.addr, err)
			}
			for _, l := range []struct {
				addr net.Addr
				path string
			}{{a, "/health/live"}, {m, "/metrics"}} {
				ta := l.addr.(*net.TCPAddr)
				host := ta.IP.String()
				if ta.IP.IsUnspecified() {
					host = "127.0.0.1"
				}
				hp := net.JoinHostPort(host, strconv.Itoa(ta.Port))
				gotTLS, gotPlain := answers200(client, "https://"+hp+l.path), answers200(client, "http://"+hp+l.path)
				if gotTLS != tc.wantTLS || gotPlain == tc.wantTLS {
					t.Errorf("%s on %s: answered over TLS %v and plainly %v, want TLS %v", l.path, tc.addr, gotTLS, gotPlain, tc.wantTLS)
				}
			}
			if err := s.Stop(context.Background()); err != nil {
				t.Errorf("Stop on %s: %v", tc.addr, err)
			}
		})
	}
}

// A start on an address but loopback with no certificate is refused, naming
// the bootstrap's keys, and a start with a client CA is refused on any
// address, since it would ask every caller for a client certificate.
func TestStartRefusesTLS(t *testing.T) {
	certs, _ := certFiles(t)
	withCA := certs
	withCA.ClientCA = certs.CertFile
	for _, tc := range []struct {
		d    api.Deps
		want string
	}{
		{api.Deps{Addr: "0.0.0.0:0", MetricsAddr: "127.0.0.1:0"}, "the API's TLS certificate and key (cert_file, key_file)"},
		{api.Deps{Addr: "127.0.0.1:0", MetricsAddr: "0.0.0.0:0"}, "the API's TLS certificate and key (cert_file, key_file)"},
		{api.Deps{Addr: "127.0.0.1:0", MetricsAddr: "127.0.0.1:0", TLS: withCA}, "no client CA"},
	} {
		tc.d.Metrics = metrics.New("test", "test")
		s := api.New(tc.d)
		_, _, err := s.Start()
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("Start with the API on %s, metrics on %s and client CA %q: got %v, want a refusal naming %q", tc.d.Addr, tc.d.MetricsAddr, tc.d.TLS.ClientCA, err, tc.want)
		}
		if err := s.Stop(context.Background()); err != nil {
			t.Errorf("Stop: %v", err)
		}
	}
}

// unixClient is a client whose every request goes to the socket at path.
func unixClient(t *testing.T, path string) *http.Client {
	tr := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", path)
	}}
	t.Cleanup(tr.CloseIdleConnections)
	return &http.Client{Transport: tr}
}

// The local socket is readable and writable by the server's user only, and
// a call on it reaches Authenticate marked local, which a call over TCP is
// not.
func TestLocalSocket(t *testing.T) {
	path := filepath.Join(t.TempDir(), "api.sock")
	// The caller's role is admin on the socket and viewer elsewhere, so the
	// answer, which names the role, says whether Authenticate saw the mark.
	byPlace := func(r *http.Request) (auth.Caller, error) {
		role := entity.RoleViewer
		if auth.Local(r.Context()) {
			role = entity.RoleAdmin
		}
		return auth.Caller{Principal: entity.Principal{Kind: entity.PrincipalUser, ID: entity.NewPrincipalID()}, Role: role}, nil
	}
	s := serve(t, byPlace, func(d *api.Deps) { d.Socket = path })
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Type() != fs.ModeSocket || fi.Mode().Perm() != 0o600 {
		t.Errorf("socket mode: got %v, want a socket of 0600", fi.Mode())
	}
	resp, err := unixClient(t, path).Get("http://node" + thing)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil || !strings.Contains(string(body), `"role":"admin"`) {
		t.Errorf("GET %s on the socket: answered %d %s %v, want the caller marked local", thing, resp.StatusCode, body, err)
	}
	if r := s.do(t, http.MethodGet, thing, ""); !strings.Contains(string(r.body), `"role":"viewer"`) {
		t.Errorf("GET %s over TCP: answered %d %s, want the caller not marked local", thing, r.status, r.body)
	}
}

// freeAddr is a loopback address a listener bound and released. Another
// process taking it before the test binds it fails the test rather than
// passing it.
func freeAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	return l.Addr().String()
}

// bindable fails the test for each of addrs that cannot be bound at once,
// which a listener a refused Start left open would hold.
func bindable(t *testing.T, addrs ...string) {
	t.Helper()
	for _, a := range addrs {
		l, err := net.Listen("tcp", a)
		if err != nil {
			t.Errorf("bind %s after the refused Start: %v, want it free", a, err)
			continue
		}
		_ = l.Close()
	}
}

// A socket a node that did not stop left at the path is replaced; a socket
// whose dial fails other than refused, here one its mode forbids connecting
// through, and any other file there refuse the start, leave the file in
// place, and leave neither TCP address bound. The sockets' files are named
// briefly because a socket path is bounded at 104 bytes on some hosts, and
// t.TempDir() holds it.
func TestStaleSocket(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root connects through any socket's mode, so the locked socket would dial refused")
	}
	dir := t.TempDir()
	left := func(name string) string {
		p := filepath.Join(dir, name)
		l, err := net.Listen("unix", p)
		if err != nil {
			t.Fatal(err)
		}
		l.(*net.UnixListener).SetUnlinkOnClose(false)
		_ = l.Close()
		return p
	}
	stale, locked := left("s"), left("l")
	if err := os.Chmod(locked, 0); err != nil {
		t.Fatal(err)
	}
	plain := filepath.Join(dir, "plain")
	if err := os.WriteFile(plain, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		path   string
		starts bool
	}{{stale, true}, {locked, false}, {plain, false}} {
		addr, maddr := freeAddr(t), freeAddr(t)
		s := api.New(api.Deps{Addr: addr, MetricsAddr: maddr, Socket: tc.path, Metrics: metrics.New("test", "test")})
		_, _, err := s.Start()
		if (err == nil) != tc.starts {
			t.Errorf("Start with the socket at %s: got %v, want it to start: %v", filepath.Base(tc.path), err, tc.starts)
		}
		if !tc.starts {
			bindable(t, addr, maddr)
			if _, err := os.Lstat(tc.path); err != nil {
				t.Errorf("%s after the refused start: %v, want it left in place", filepath.Base(tc.path), err)
			}
		}
		if err := s.Stop(context.Background()); err != nil {
			t.Errorf("Stop: %v", err)
		}
	}
	if b, err := os.ReadFile(plain); err != nil || string(b) != "x" {
		t.Errorf("the plain file after the refused start: %q %v, want it untouched", b, err)
	}
}

// A node runs at most 64 API calls at once: with 64 held, the next is 503
// unavailable with Retry-After, a public route's among them, while a health
// probe still answers, and the series say 64 of 64. Once they are let go, a
// call is answered again.
func TestCallBound(t *testing.T) {
	hold := api.NewHold(nil)
	s := serve(t, as(entity.RoleOperator, false), func(d *api.Deps) { d.Applied = hold.Applied })
	// Registered after serve, so it runs before Stop waits for the held
	// writes, even when an assertion ends the test early.
	let := sync.OnceFunc(func() { close(hold.Release) })
	t.Cleanup(let)
	held := make(chan reply, 64)
	for range 64 {
		go func() {
			r, err := s.send(t.Context(), http.MethodPut, knobAt(uuid.NewString()), `{"spec":{"value":"on"}}`, "If-None-Match", "*")
			if err != nil {
				t.Error(err)
			}
			held <- r
		}()
	}
	for range 64 {
		select {
		case <-hold.Reached:
		case <-time.After(10 * time.Second):
			t.Fatal("64 writes never reached Applied")
		}
	}
	r := s.do(t, http.MethodGet, thing, "")
	if p := r.problem(t); r.status != http.StatusServiceUnavailable || p.Code != apiv1.CodeUnavailable || r.header.Get("Retry-After") != "1" {
		t.Errorf("the 65th call: answered %d %s Retry-After %q, want 503 unavailable with Retry-After 1", r.status, r.body, r.header.Get("Retry-After"))
	}
	if r := s.do(t, http.MethodGet, "/api/version", ""); r.status != http.StatusServiceUnavailable || r.problem(t).Code != apiv1.CodeUnavailable {
		t.Errorf("GET /api/version at the bound: answered %d %s, want 503 unavailable", r.status, r.body)
	}
	if r := s.do(t, http.MethodGet, "/health/live", ""); r.status != http.StatusOK {
		t.Errorf("GET /health/live at the bound: answered %d %s, want 200", r.status, r.body)
	}
	for name, want := range map[string]float64{"dittofs_api_calls_in_flight": 64, "dittofs_api_calls_in_flight_limit": 64} {
		if got := sample(t, s.reg, name, nil); got != want {
			t.Errorf("%s at the bound: got %v, want %v", name, got, want)
		}
	}
	let()
	for range 64 {
		if r := <-held; r.status != http.StatusOK {
			t.Errorf("a held write once let go: answered %d %s, want 200", r.status, r.body)
		}
	}
	eventually(t, "dittofs_api_calls_in_flight back to 0 once the held calls answered", func() bool {
		return sample(t, s.reg, "dittofs_api_calls_in_flight", nil) == 0
	})
	if got := sample(t, s.reg, "dittofs_api_calls_in_flight_limit", nil); got != 64 {
		t.Errorf("dittofs_api_calls_in_flight_limit once the held calls answered: got %v, want 64", got)
	}
	if r := s.do(t, http.MethodGet, thing, ""); r.status != http.StatusOK {
		t.Errorf("a call once the held ones answered: %d %s, want 200", r.status, r.body)
	}
}

// While the node is starting, every /api/v1 call but its health is 503
// starting with Retry-After, before the caller is authenticated; the public
// version and the probes answer. Ready, draining or not_ready, the same
// call is answered.
func TestStartingRefusesCalls(t *testing.T) {
	var word atomic.Value
	word.Store("starting")
	var authenticated atomic.Int32
	caller := func(r *http.Request) (auth.Caller, error) {
		authenticated.Add(1)
		return as(entity.RoleViewer, false)(r)
	}
	s := serve(t, caller, func(d *api.Deps) { d.Ready = func() string { return word.Load().(string) } })
	for _, path := range []string{thing, "/api/v1/no-such-thing"} {
		r := s.do(t, http.MethodGet, path, "")
		if p := r.problem(t); r.status != http.StatusServiceUnavailable || p.Code != apiv1.CodeStarting || r.header.Get("Retry-After") != "1" {
			t.Errorf("GET %s while starting: answered %d %s Retry-After %q, want 503 starting with Retry-After 1", path, r.status, r.body, r.header.Get("Retry-After"))
		}
	}
	if n := authenticated.Load(); n != 0 {
		t.Errorf("calls authenticated while starting: got %d, want 0", n)
	}
	for _, path := range []string{"/api/v1/health", "/api/version", "/health/live"} {
		if r := s.do(t, http.MethodGet, path, ""); r.status != http.StatusOK {
			t.Errorf("GET %s while starting: answered %d %s, want 200", path, r.status, r.body)
		}
	}
	for _, w := range []string{"ready", "draining", "not_ready"} {
		word.Store(w)
		if r := s.do(t, http.MethodGet, thing, ""); r.status != http.StatusOK {
			t.Errorf("GET %s while %s: answered %d %s, want 200", thing, w, r.status, r.body)
		}
	}
}

// A live node's socket at the path refuses a second node's start, and the
// second's Stop leaves the first serving on it. The refused start leaves
// neither of its TCP addresses bound.
func TestLiveSocket(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s")
	serve(t, as(entity.RoleViewer, false), func(d *api.Deps) { d.Socket = path })
	addr, maddr := freeAddr(t), freeAddr(t)
	second := api.New(api.Deps{Addr: addr, MetricsAddr: maddr, Socket: path, Metrics: metrics.New("test", "test")})
	if _, _, err := second.Start(); err == nil || !strings.Contains(err.Error(), "a node is serving the local socket "+path) {
		t.Errorf("a second Start on a live socket: got %v, want a refusal naming the socket", err)
	}
	bindable(t, addr, maddr)
	if err := second.Stop(context.Background()); err != nil {
		t.Errorf("the second's Stop: %v", err)
	}
	resp, err := unixClient(t, path).Get("http://node" + thing)
	if err != nil {
		t.Fatalf("GET %s on the first node's socket after the second's Stop: %v", thing, err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("GET %s on the first node's socket after the second's Stop: answered %d, want 200", thing, resp.StatusCode)
	}
}

// A store read that fails unavailable answers 503 unavailable with
// Retry-After, on Badger on disk with every read faulted.
func TestStoreReadUnavailable(t *testing.T) {
	disk := newOnDisk(t)
	s := serve(t, as(entity.RoleViewer, false), disk.use)
	sched, err := sim.NewSchedule(1, nil, sim.Rule{Op: kvtest.OpView, Key: "*", Action: sim.FaultAction{Err: vfserr.ErrUnavailable}})
	if err != nil {
		t.Fatal(err)
	}
	disk.fault.SetFault(sched)
	if r := s.do(t, http.MethodGet, knobAt(uuid.NewString()), ""); r.status != http.StatusServiceUnavailable || r.problem(t).Code != apiv1.CodeUnavailable || r.header.Get("Retry-After") != "1" {
		t.Errorf("GET knob with the store's reads faulted: answered %d %s Retry-After %q, want 503 unavailable with Retry-After 1", r.status, r.body, r.header.Get("Retry-After"))
	}
}

// A refusal's Retry-After is the wait its error carries, rounded up to whole
// seconds.
func TestRetryAfterFromError(t *testing.T) {
	s := serve(t, as(entity.RoleViewer, false))
	if r := s.do(t, http.MethodGet, "/api/v1/wait", ""); r.status != http.StatusServiceUnavailable || r.header.Get("Retry-After") != "7" {
		t.Errorf("GET /api/v1/wait, failing with a wait of 6.5s: answered %d %s Retry-After %q, want 503 with Retry-After 7", r.status, r.body, r.header.Get("Retry-After"))
	}
}

// A call whose client sends its headers and one byte of its body, then
// stalls, gives its call slot back once the read timeout passes: with 64
// stalled, the next call is refused, and once each stalled one is answered,
// the next is answered.
func TestStalledBodiesLeaveTheBound(t *testing.T) {
	// The read timeout is the HTTP server's, on the wall clock, which no test
	// clock moves. It is long enough for the 64 calls to be seen holding
	// their slots before it passes.
	s := serveWith(t, as(entity.RoleOperator, false), func(srv *api.Server) { srv.ShortenReadTimeout(3 * time.Second) })
	conns := make([]net.Conn, 64)
	for i := range conns {
		c, err := net.Dial("tcp", strings.TrimPrefix(s.url, "http://"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = c.Close() })
		if _, err := fmt.Fprintf(c, "PUT %s HTTP/1.1\r\nHost: node\r\nIf-None-Match: *\r\nContent-Length: 30\r\n\r\n{", knobAt(uuid.NewString())); err != nil {
			t.Fatal(err)
		}
		conns[i] = c
	}
	eventually(t, "64 stalled calls hold their slots", func() bool { return sample(t, s.reg, "dittofs_api_calls_in_flight", nil) == 64 })
	if r := s.do(t, http.MethodGet, thing, ""); r.status != http.StatusServiceUnavailable {
		t.Errorf("a call with 64 stalled: answered %d %s, want 503", r.status, r.body)
	}
	for i, c := range conns {
		if err := c.SetReadDeadline(time.Now().Add(10 * time.Second)); err != nil {
			t.Fatal(err)
		}
		if _, err := c.Read(make([]byte, 1)); errors.Is(err, os.ErrDeadlineExceeded) {
			t.Fatalf("stalled call %d: not answered within 10s, want it answered once the read timeout passes", i)
		}
	}
	if r := s.do(t, http.MethodGet, thing, ""); r.status != http.StatusOK {
		t.Errorf("a call once the stalled ones were answered: %d %s, want 200", r.status, r.body)
	}
}
