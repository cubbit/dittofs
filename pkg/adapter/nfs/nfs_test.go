package nfs_test

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cubbit/dittofs/internal/vfs"
	"github.com/cubbit/dittofs/pkg/adapter"
	"github.com/cubbit/dittofs/pkg/adapter/config"
	"github.com/cubbit/dittofs/pkg/adapter/nfs"
	"github.com/cubbit/dittofs/pkg/resources"
)

func valid() config.NFS {
	s := config.DefaultNFS()
	s.Addrs = []netip.Addr{netip.MustParseAddr("127.0.0.1")}
	return s
}

// deps is the Deps New requires, VFS and Budgets, and nothing else. New only
// checks VFS is set, so a typed nil of the real filesystem serves.
func deps() adapter.Deps {
	return adapter.Deps{VFS: (*vfs.Filesystem)(nil), Budgets: &resources.Budgets{}}
}

// New refuses Deps missing VFS or Budgets, naming every field missing and
// no other, and accepts every other field nil.
func TestNewRequiresVFSAndBudgets(t *testing.T) {
	for _, c := range []struct {
		name         string
		d            adapter.Deps
		missing, set []string
	}{
		{"empty", adapter.Deps{}, []string{"Deps.VFS", "Deps.Budgets"}, nil},
		{"no VFS", adapter.Deps{Budgets: &resources.Budgets{}}, []string{"Deps.VFS"}, []string{"Deps.Budgets"}},
		{"no Budgets", adapter.Deps{VFS: deps().VFS}, []string{"Deps.Budgets"}, []string{"Deps.VFS"}},
	} {
		_, err := nfs.New(c.d, valid())
		if err == nil || errors.Is(err, errors.ErrUnsupported) {
			t.Errorf("%s: New: %v, want refused", c.name, err)
			continue
		}
		for _, f := range c.missing {
			if !strings.Contains(err.Error(), f) {
				t.Errorf("%s: New: %q, want it to name %s", c.name, err, f)
			}
		}
		for _, f := range c.set {
			if strings.Contains(err.Error(), f) {
				t.Errorf("%s: New: %q, want it not to name %s, which is set", c.name, err, f)
			}
		}
	}
	if _, err := nfs.New(deps(), valid()); !errors.Is(err, errors.ErrUnsupported) {
		t.Errorf("New with only VFS and Budgets: %v, want accepted as far as errors.ErrUnsupported", err)
	}
}

// New refuses an invalid setting with the validator's error, naming the field,
// and answers a valid one as not built yet.
func TestNewValidatesFirst(t *testing.T) {
	bad := valid()
	bad.TLS.Mode = "always"
	if a, err := nfs.New(deps(), bad); a != nil || err == nil || !strings.Contains(err.Error(), "TLS.Mode") || errors.Is(err, errors.ErrUnsupported) {
		t.Errorf("New with a bad TLS.Mode: %v, %v; want the validator's error naming TLS.Mode", a, err)
	}
	if a, err := nfs.New(deps(), valid()); a != nil || !errors.Is(err, errors.ErrUnsupported) {
		t.Errorf("New with valid settings: %v, %v; want errors.ErrUnsupported", a, err)
	}
}

// selfSigned writes a self-signed certificate and its key into dir.
func selfSigned(t *testing.T, dir string) (certFile, keyFile string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "dittofs-test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		IsCA:         true,
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	certFile, keyFile = filepath.Join(dir, "tls.crt"), filepath.Join(dir, "tls.key")
	write(t, certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
	write(t, keyFile, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}))
	return certFile, keyFile
}

func write(t *testing.T, path string, b []byte) {
	t.Helper()
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
}

// New loads the TLS files: a valid pair and client CA get as far as the
// unbuilt adapter, and a missing or garbage file is refused naming the file.
func TestNewLoadsTheTLSFiles(t *testing.T) {
	dir := t.TempDir()
	cert, key := selfSigned(t, dir)
	garbage, missing := filepath.Join(dir, "garbage"), filepath.Join(dir, "missing")
	write(t, garbage, []byte("not a certificate"))
	tlsOn := func(cert, key, ca string) config.NFS {
		s := valid()
		s.TLS = config.TLS{Mode: "require", CertFile: cert, KeyFile: key, ClientCA: ca}
		return s
	}
	if _, err := nfs.New(deps(), tlsOn(cert, key, cert)); !errors.Is(err, errors.ErrUnsupported) {
		t.Fatalf("a self-signed pair and CA: %v, want past loading to errors.ErrUnsupported", err)
	}
	for _, c := range []struct {
		name, file string
		s          config.NFS
	}{
		{"missing cert", missing, tlsOn(missing, key, "")},
		{"garbage cert", garbage, tlsOn(garbage, key, "")},
		{"missing key", missing, tlsOn(cert, missing, "")},
		{"garbage key", garbage, tlsOn(cert, garbage, "")},
		{"missing CA", missing, tlsOn(cert, key, missing)},
		{"garbage CA", garbage, tlsOn(cert, key, garbage)},
	} {
		_, err := nfs.New(deps(), c.s)
		if err == nil || errors.Is(err, errors.ErrUnsupported) || !strings.Contains(err.Error(), c.file) {
			t.Errorf("%s: %v, want refused naming %s", c.name, err, c.file)
		}
	}
}
