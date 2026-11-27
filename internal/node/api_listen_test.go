package node_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/cubbit/dittofs/internal/node"
	"github.com/cubbit/dittofs/internal/node/nodetest"
)

// selfSigned writes a self-signed certificate for 127.0.0.1 and its key
// under t.TempDir(), returning their paths and a pool trusting the
// certificate.
func selfSigned(t *testing.T) (certFile, keyFile string, pool *x509.CertPool) {
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
	certFile, keyFile = filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")
	for path, b := range map[string]*pem.Block{certFile: {Type: "CERTIFICATE", Bytes: der}, keyFile: {Type: "EC PRIVATE KEY", Bytes: kder}} {
		if err := os.WriteFile(path, pem.EncodeToMemory(b), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	pool = x509.NewCertPool()
	pool.AddCert(cert)
	return certFile, keyFile, pool
}

// startOrFail starts the harness's node, naming what the start was for when
// it is refused, and stops it when the test ends.
func startOrFail(t *testing.T, h *nodetest.Harness, what string) {
	t.Helper()
	if err := h.Node().Start(t.Context()); err != nil {
		t.Fatalf("Start with %s: %v, want it started", what, err)
	}
	t.Cleanup(func() {
		if err := h.Node().Stop(context.Background()); err != nil {
			t.Errorf("Stop: %v", err)
		}
	})
}

// A node whose bootstrap names an api.listen address but loopback and
// api.tls's certificate and key starts, and serves its API over TLS with
// that certificate. The address is every interface's, the one way to bind
// an address but loopback on any host; the call reaches it on 127.0.0.1.
func TestTheAPIServesItsTLS(t *testing.T) {
	certFile, keyFile, pool := selfSigned(t)
	h := nodetest.New(t, nodetest.Watch(), nodetest.WithNew(func(cfg node.Config) (*node.Node, error) {
		cfg.Bootstrap.API.Listen = []string{"0.0.0.0:0"}
		cfg.Bootstrap.API.TLS.CertFile, cfg.Bootstrap.API.TLS.KeyFile = certFile, keyFile
		return node.New(cfg)
	}))
	startOrFail(t, h, "api.listen 0.0.0.0:0 and api.tls's certificate and key")
	addr, _ := h.Node().Addrs()
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("the API's address %q: %v", addr, err)
	}
	c := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{
		TLSClientConfig: &tls.Config{RootCAs: pool}, DisableKeepAlives: true}}
	resp, err := c.Get("https://" + net.JoinHostPort("127.0.0.1", port) + "/api/version")
	if err != nil {
		t.Fatalf("GET /api/version over TLS on port %s: %v, want 200 under api.tls's certificate", port, err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || resp.TLS == nil {
		t.Errorf("GET /api/version over TLS: status %d, TLS %v, want 200 over TLS", resp.StatusCode, resp.TLS != nil)
	}
}

// A node whose bootstrap names api.socket serves its API on that socket too.
// The test's name is short because a socket's path is capped at 104 bytes
// on some hosts, and t.TempDir() names its directory after the test.
func TestTheAPIServesItsSocket(t *testing.T) {
	path := filepath.Join(t.TempDir(), "api.sock")
	h := nodetest.New(t, nodetest.Watch(), nodetest.WithNew(func(cfg node.Config) (*node.Node, error) {
		cfg.Bootstrap.API.Socket = path
		return node.New(cfg)
	}))
	startOrFail(t, h, "api.socket "+path)
	c := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{DisableKeepAlives: true,
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", path)
		}}}
	resp, err := c.Get("http://node/api/version")
	if err != nil {
		t.Fatalf("GET /api/version on the bootstrap's api.socket %s: %v, want 200", path, err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("GET /api/version on api.socket: status %d, want %d", resp.StatusCode, http.StatusOK)
	}
}
