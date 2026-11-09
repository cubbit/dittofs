package config

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"go/parser"
	"go/token"
	"math/big"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

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

// Validate loads the TLS files: a valid pair and client CA are accepted, and
// a missing or garbage file is refused naming its field.
func TestValidateLoadsTheTLSFiles(t *testing.T) {
	dir := t.TempDir()
	cert, key := selfSigned(t, dir)
	garbage, missing := filepath.Join(dir, "garbage"), filepath.Join(dir, "missing")
	write(t, garbage, []byte("not a certificate"))
	tlsOn := func(cert, key, ca string) NFS {
		s := validNFS()
		s.TLS.CertFile, s.TLS.KeyFile, s.TLS.ClientCA = cert, key, ca
		return s
	}
	if err := Validate(tlsOn(cert, key, cert)); err != nil {
		t.Fatalf("a self-signed pair and CA: %v, want accepted", err)
	}
	for _, c := range []struct {
		name, field string
		s           NFS
	}{
		{"missing cert", "TLS.CertFile", tlsOn(missing, key, "")},
		{"garbage cert", "TLS.CertFile", tlsOn(garbage, key, "")},
		{"missing key", "TLS.KeyFile", tlsOn(cert, missing, "")},
		{"garbage key", "TLS.KeyFile", tlsOn(cert, garbage, "")},
		{"missing CA", "TLS.ClientCA", tlsOn(cert, key, missing)},
		{"garbage CA", "TLS.ClientCA", tlsOn(cert, key, garbage)},
	} {
		err := Validate(c.s)
		if err == nil || !strings.Contains(err.Error(), "config: "+c.field+": ") {
			t.Errorf("%s: %v, want refused naming %s", c.name, err, c.field)
		}
	}
}

// pkg/adapter/config is a leaf: it imports only the standard library.
func TestImportsOnlyTheStandardLibrary(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(token.NewFileSet(), f, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		for _, imp := range file.Imports {
			p, _ := strconv.Unquote(imp.Path.Value)
			if strings.Contains(strings.Split(p, "/")[0], ".") {
				t.Errorf("%s imports %s; pkg/adapter/config imports only the standard library", f, p)
			}
		}
	}
}
