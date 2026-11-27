package keyprovider

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gemalto/kmip-go/kmip14"
)

// TestKMIP_ConfigValidation runs without a live KMIP server — it
// exercises the up-front config checks in newKMIPProvider that have to
// fail fast before any network I/O.
func TestKMIP_ConfigValidation(t *testing.T) {
	cases := []struct {
		name string
		cfg  Config
	}{
		{"missing endpoint", Config{Kind: KindKMIP, KeyUID: "x", ClientCert: "/a", ClientKey: "/b"}},
		{"missing key_uid", Config{Kind: KindKMIP, Endpoint: "host:5696", ClientCert: "/a", ClientKey: "/b"}},
		{"missing client cert", Config{Kind: KindKMIP, Endpoint: "host:5696", KeyUID: "x", ClientKey: "/b"}},
		{"missing client key", Config{Kind: KindKMIP, Endpoint: "host:5696", KeyUID: "x", ClientCert: "/a"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := newKMIPProvider(context.Background(), tc.cfg)
			if !errors.Is(err, ErrInvalidConfig) {
				t.Fatalf("got %v, want ErrInvalidConfig", err)
			}
		})
	}
}

// The state decisions over every KMIP state: the current key is refused
// unless Active, and a retired key only when its material is destroyed, so a
// Compromised or Deactivated retired key still reads the blocks under it.
func TestKMIP_StateDecisions(t *testing.T) {
	for _, tc := range []struct {
		state                          kmip14.State
		currentRefused, retiredRefused bool
	}{
		{kmip14.StatePreActive, true, false},
		{kmip14.StateActive, false, false},
		{kmip14.StateDeactivated, true, false},
		{kmip14.StateCompromised, true, false},
		{kmip14.StateDestroyed, true, true},
		{kmip14.StateDestroyedCompromised, true, true},
		{kmip14.State(0x99), true, false},
	} {
		t.Run(tc.state.String(), func(t *testing.T) {
			for _, d := range []struct {
				name    string
				err     error
				refused bool
			}{
				{"current", currentKeyStateErr("uid-1", tc.state), tc.currentRefused},
				{"retired", retiredKeyStateErr("uid-1", tc.state), tc.retiredRefused},
			} {
				if got := d.err != nil; got != d.refused {
					t.Fatalf("%s key in state %s: refused = %v (%v), want %v", d.name, tc.state, got, d.err, d.refused)
				}
				if d.err == nil {
					continue
				}
				if !errors.Is(d.err, ErrKeyStateUnusable) {
					t.Fatalf("%s key in state %s: error %v, want ErrKeyStateUnusable", d.name, tc.state, d.err)
				}
				if !strings.Contains(d.err.Error(), tc.state.String()) || !strings.Contains(d.err.Error(), "uid-1") {
					t.Fatalf("%s key: error %q should name the state %q and the uid", d.name, d.err, tc.state)
				}
			}
		})
	}
}

// A server CA file that holds no certificate is refused, not taken for the
// system roots, and a client certificate that does not load is refused, each
// naming what failed.
func TestBuildKMIPTLSConfig(t *testing.T) {
	dir := t.TempDir()
	empty := filepath.Join(dir, "empty.pem")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatalf("write empty file: %v", err)
	}
	cert, key := writeClientCert(t, dir)
	if _, err := buildKMIPTLSConfig(Config{ClientCert: cert, ClientKey: key}); err != nil {
		t.Fatalf("buildKMIPTLSConfig of a valid certificate: %v", err)
	}
	for _, tc := range []struct {
		name string
		cfg  Config
		want string
	}{
		{"missing client cert", Config{ClientCert: filepath.Join(dir, "none"), ClientKey: empty}, "load kmip client cert"},
		{"client cert not PEM", Config{ClientCert: empty, ClientKey: empty}, "kmip: load client cert"},
		{"missing server CA", Config{ClientCert: empty, ClientKey: empty, ServerCA: filepath.Join(dir, "none")}, "read kmip server CA"},
		{"empty server CA", Config{ClientCert: cert, ClientKey: key, ServerCA: empty}, "kmip: server CA has no PEM certs"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := buildKMIPTLSConfig(tc.cfg)
			if err == nil || !strings.HasPrefix(err.Error(), "keyprovider: ") || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("buildKMIPTLSConfig: error %v, want one starting keyprovider: and naming %q", err, tc.want)
			}
		})
	}
}

// writeClientCert writes a self-signed ECDSA certificate and its key under
// dir and returns their paths.
func writeClientCert(t *testing.T, dir string) (certPath, keyPath string) {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	tmpl := x509.Certificate{SerialNumber: big.NewInt(1), NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &priv.PublicKey, priv)
	if err != nil {
		t.Fatalf("create cert: %v", err)
	}
	keyDER, err := x509.MarshalECPrivateKey(priv)
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}
	certPath, keyPath = filepath.Join(dir, "client-cert.pem"), filepath.Join(dir, "client-key.pem")
	if err := os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatalf("write cert: %v", err)
	}
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		t.Fatalf("write key: %v", err)
	}
	return certPath, keyPath
}
