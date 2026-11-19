package directory_test

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
	"math/big"
	"net"
	"testing"
	"time"

	"github.com/cubbit/dittofs/internal/identity/directory"
)

// issuer makes certificates: a CA, or, with a parent, a leaf naming hosts.
type issuer struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
	pem  string
}

func newCert(t *testing.T, parent *issuer, hosts ...string) *issuer {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: "test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		DNSNames:     hosts,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	signer, signKey := tmpl, k
	if parent == nil {
		tmpl.IsCA, tmpl.BasicConstraintsValid, tmpl.KeyUsage = true, true, x509.KeyUsageCertSign
	} else {
		signer, signKey = parent.cert, parent.key
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, signer, &k.PublicKey, signKey)
	if err != nil {
		t.Fatal(err)
	}
	c, _ := x509.ParseCertificate(der)
	return &issuer{cert: c, key: k, pem: string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))}
}

// serve accepts one connection on 127.0.0.1:0 over TLS with leaf, and sends
// on the returned channel its handshake's error, then whether it read any
// application byte before the client went away.
func serve(t *testing.T, leaf *issuer) (addr string, handshake <-chan error, read <-chan bool) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	hs, rd := make(chan error, 1), make(chan bool, 1)
	go func() {
		raw, err := ln.Accept()
		if err != nil {
			hs <- err
			return
		}
		defer func() { _ = raw.Close() }()
		srv := tls.Server(raw, &tls.Config{Certificates: []tls.Certificate{{
			Certificate: [][]byte{leaf.cert.Raw}, PrivateKey: leaf.key}}})
		hs <- srv.Handshake()
		n, _ := io.Copy(io.Discard, srv)
		rd <- n > 0
	}()
	_, port, _ := net.SplitHostPort(ln.Addr().String())
	return "ldaps://localhost:" + port, hs, rd
}

// DialTLS refuses a server whose chain does not end in the record's trust
// roots, or whose certificate names another host, at the handshake, so no
// byte of a bind is ever sent; it accepts one the trust roots signed.
func TestDialTLSVerifiesTheServer(t *testing.T) {
	ca := newCert(t, nil)
	for _, tc := range []struct {
		name   string
		leaf   *issuer
		accept bool
	}{
		{"self-signed", newCert(t, nil, "localhost"), false},
		{"another host's certificate", newCert(t, ca, "dc.other.example"), false},
		{"signed by the trust roots for this host", newCert(t, ca, "localhost"), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			addr, handshake, read := serve(t, tc.leaf)
			rec := directory.Record{Address: addr, TrustRoots: ca.pem}
			c, err := directory.DialTLS(context.Background(), rec)
			if tc.accept {
				if err != nil {
					t.Fatalf("DialTLS to a server the trust roots signed: %v", err)
				}
				if err := <-handshake; err != nil {
					t.Errorf("the server's handshake: %v", err)
				}
				_ = c.Close()
				return
			}
			if err == nil {
				_ = c.Close()
				t.Fatalf("DialTLS to a %s server: accepted, want refused", tc.name)
			}
			if err := <-handshake; err == nil {
				t.Errorf("the %s server's handshake succeeded, want it failed by the client", tc.name)
			}
			if <-read {
				t.Errorf("the %s server read application bytes, want none sent", tc.name)
			}
		})
	}
}

// DialTLS refuses an address that is neither ldaps:// nor ldap:// without
// connecting to it, so no other scheme ever reaches a server.
func TestDialTLSRefusesAnotherScheme(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	// The timeout is only a backstop: a dial that went through would wait
	// for an answer this listener never sends.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if c, err := directory.DialTLS(ctx, directory.Record{Address: "http://" + ln.Addr().String()}); err == nil {
		_ = c.Close()
		t.Error("DialTLS of an http:// address: accepted, want refused")
	}
	// A connection of DialTLS's, had it made one, is queued before this
	// sentinel, and Accept returns connections in the order they came.
	sentinel, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sentinel.Close() }()
	first, err := ln.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = first.Close() }()
	if first.RemoteAddr().String() != sentinel.LocalAddr().String() {
		t.Error("DialTLS of an http:// address connected to it, want no connection")
	}
}

// A call whose context ends while the directory has not answered returns the
// context's error, the connection closed under it: here StartTLS on an ldap://
// address, sent to a listener that reads it and never answers.
func TestDialTLSEndsWithItsContext(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	read := make(chan struct{})
	go func() {
		raw, err := ln.Accept()
		if err != nil {
			return
		}
		t.Cleanup(func() { _ = raw.Close() })
		if _, err := raw.Read(make([]byte, 1)); err == nil {
			close(read)
		}
	}()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		c, err := directory.DialTLS(ctx, directory.Record{Address: "ldap://" + ln.Addr().String()})
		if err == nil {
			_ = c.Close()
		}
		done <- err
	}()
	select {
	case <-read: // the StartTLS request is on the wire
	case err := <-done:
		t.Fatalf("DialTLS ended before it sent StartTLS: %v", err)
	}
	cancel()
	// The bound is the assertion: go-ldap's own timeout, a minute, is what
	// ends a StartTLS nothing closed, and no clock reaches it.
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("DialTLS whose context ended during StartTLS: err %v, want %v", err, context.Canceled)
		}
	case <-time.After(10 * time.Second):
		t.Error("DialTLS outlived its context by 10s, want it ended with the context")
	}
}
