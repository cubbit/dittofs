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
	"strings"
	"testing"
	"time"

	"github.com/go-ldap/ldap/v3"

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

// Bind with an empty password, nil or zero-length, is refused before a byte
// of it reaches the directory: a name with no password is an unauthenticated
// bind, which a permissive server accepts. The test pins that rule, whichever
// layer refuses: Bind's own check and go-ldap's, which the request's spelled-out
// AllowEmptyPassword: false keeps on, each refuse with the same error class, so
// it fails only when both go.
func TestBindRefusesAnEmptyPassword(t *testing.T) {
	ca := newCert(t, nil)
	addr, handshake, read := serve(t, newCert(t, ca, "localhost"))
	c, err := directory.DialTLS(context.Background(), directory.Record{Address: addr, TrustRoots: ca.pem})
	if err != nil {
		t.Fatalf("DialTLS: %v", err)
	}
	if err := <-handshake; err != nil {
		t.Fatalf("the server's handshake: %v", err)
	}
	// The timeout is only a backstop: the server never answers, so a bind
	// that was sent would wait for it.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for _, pw := range [][]byte{nil, {}} {
		err := c.Bind(ctx, "CN=someone,DC=example,DC=com", pw)
		if !ldap.IsErrorWithCode(err, ldap.ErrorEmptyPassword) {
			t.Errorf("Bind with password %#v: err %v, want ldap result code %d (empty password)", pw, err, ldap.ErrorEmptyPassword)
		}
	}
	_ = c.Close()
	if <-read {
		t.Error("the server read application bytes after binds with an empty password, want none sent")
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
	// The bound is the assertion: go-ldap sets no request timeout of its own,
	// so only the context ends a StartTLS, and the ceiling is past the bound.
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("DialTLS whose context ended during StartTLS: err %v, want %v", err, context.Canceled)
		}
	case <-time.After(10 * time.Second):
		t.Error("DialTLS outlived its context by 10s, want it ended with the context")
	}
}

// A dial or call whose context sets no deadline ends at the ceiling with the
// context's error when the directory never answers: the ldaps:// handshake
// and the ldap:// StartTLS to a listener that never accepts, where the
// system completes the connection and nothing answers, and a bind once
// connected to a server that never answers it.
func TestCallsEndAtTheCeiling(t *testing.T) {
	const ceiling = 100 * time.Millisecond
	// endsAtCeiling runs call, which dials or calls under ceiling; the 10s
	// bound only stops one that never ends.
	endsAtCeiling := func(t *testing.T, call func() error) {
		t.Helper()
		done := make(chan error, 1)
		go func() { done <- call() }()
		select {
		case err := <-done:
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Errorf("err %v, want %v", err, context.DeadlineExceeded)
			}
		case <-time.After(10 * time.Second):
			t.Errorf("still running 10s past a ceiling of %v, want it ended at the ceiling", ceiling)
		}
	}
	for _, tc := range []struct{ name, scheme string }{
		{"ldaps handshake", "ldaps://"},
		{"ldap StartTLS", "ldap://"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ln, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = ln.Close() })
			endsAtCeiling(t, func() error {
				c, err := directory.DialTLSWithin(context.Background(), directory.Record{Address: tc.scheme + ln.Addr().String()}, ceiling)
				if err == nil {
					_ = c.Close()
				}
				return err
			})
		})
	}
	t.Run("bind once connected", func(t *testing.T) {
		ca := newCert(t, nil)
		addr, _, _ := serve(t, newCert(t, ca, "localhost"))
		c, err := directory.DialTLSWithin(context.Background(), directory.Record{Address: addr, TrustRoots: ca.pem}, ceiling)
		if err != nil {
			t.Fatalf("DialTLS: %v", err)
		}
		defer func() { _ = c.Close() }()
		endsAtCeiling(t, func() error {
			return c.Bind(context.Background(), "CN=someone,DC=example,DC=com", []byte("password"))
		})
	})
}

// holdLast passes the server's writes through but the last byte of its
// second, a TLS 1.2 server's final flight, which it hands to held as a func
// that sends it.
type holdLast struct {
	net.Conn
	writes int
	held   chan<- func()
}

func (c *holdLast) Write(b []byte) (int, error) {
	c.writes++
	if c.writes != 2 {
		return c.Conn.Write(b)
	}
	if _, err := c.Conn.Write(b[:len(b)-1]); err != nil {
		return 0, err
	}
	last := b[len(b)-1]
	c.held <- func() { _, _ = c.Conn.Write([]byte{last}) }
	return len(b), nil
}

// A DialTLS whose context ends as StartTLS's handshake completes returns a
// connection or the context's error, and nothing closes the connection while
// go-ldap swaps in the TLS one; only the race detector, which the unit tests
// run under, sees such a Close. Over TLS 1.2 the server speaks last, so each
// round holds back the last byte of its final flight, sends it, and cancels
// at once, as the client completes its handshake.
func TestDialTLSEndsAsStartTLSCompletes(t *testing.T) {
	ca := newCert(t, nil)
	leaf := newCert(t, ca, "localhost")
	cfg := &tls.Config{MaxVersion: tls.VersionTLS12,
		Certificates: []tls.Certificate{{Certificate: [][]byte{leaf.cert.Raw}, PrivateKey: leaf.key}}}
	// The success reply to message 1, go-ldap's first: StartTLS.
	startTLSOK := []byte{0x30, 0x0c, 0x02, 0x01, 0x01, 0x78, 0x07, 0x0a, 0x01, 0x00, 0x04, 0x00, 0x04, 0x00}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	held := make(chan func(), 1)
	go func() {
		for {
			raw, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer func() { _ = raw.Close() }()
				// The StartTLS request, short enough that its second byte is
				// the length of the rest.
				hdr := make([]byte, 2)
				if _, err := io.ReadFull(raw, hdr); err != nil {
					return
				}
				if _, err := io.ReadFull(raw, make([]byte, hdr[1])); err != nil {
					return
				}
				if _, err := raw.Write(startTLSOK); err != nil {
					return
				}
				if tls.Server(&holdLast{Conn: raw, held: held}, cfg).Handshake() == nil {
					_, _ = io.Copy(io.Discard, raw)
				}
			}()
		}
	}()
	_, port, _ := net.SplitHostPort(ln.Addr().String())
	rec := directory.Record{Address: "ldap://localhost:" + port, TrustRoots: ca.pem}

	// With more than one P, a round lands on the swap one time in two or three.
	for round := range 50 {
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() {
			c, err := directory.DialTLS(ctx, rec)
			if err == nil {
				_ = c.Close()
			}
			done <- err
		}()
		select {
		case release := <-held:
			release()
			cancel()
		case err := <-done:
			cancel()
			t.Fatalf("round %d: DialTLS ended before the server's last byte: %v", round, err)
		}
		if err := <-done; err != nil && !errors.Is(err, context.Canceled) {
			t.Fatalf("round %d: DialTLS cancelled as StartTLS completes: err %v, want a connection or %v", round, err, context.Canceled)
		}
	}
}

// A request asking for domain scope carries the domain-scope control, not
// critical, so a directory that does not know it still answers; one that
// does not ask carries none.
func TestSearchRequestDomainScope(t *testing.T) {
	sr := directory.SearchRequest(directory.Request{Base: "DC=x", Filter: "(cn=a)", DomainScope: true})
	found := false
	for _, c := range sr.Controls {
		if c.GetControlType() == "1.2.840.113556.1.4.1339" {
			found = true
			if cs, ok := c.(*ldap.ControlString); !ok || cs.Criticality || cs.ControlValue != "" {
				t.Errorf("domain-scope control %v, want it not critical and with no value", c)
			}
		}
	}
	if !found {
		t.Errorf("controls %v, want the domain-scope control", sr.Controls)
	}
	if sr := directory.SearchRequest(directory.Request{Base: "DC=x", Filter: "(cn=a)"}); len(sr.Controls) != 0 {
		t.Errorf("controls %v without DomainScope, want none", sr.Controls)
	}
}

// A search answered by a referral, an LDAPResult of code 10, fails with
// ErrReferral; another refusal does not.
func TestSearchErrorReferral(t *testing.T) {
	ref := ldap.NewError(ldap.LDAPResultReferral, errors.New("ldap://elsewhere.example/DC=x"))
	if err := directory.SearchError(ref); !errors.Is(err, directory.ErrReferral) || !strings.Contains(err.Error(), "elsewhere.example") {
		t.Errorf("a code-10 result = %v, want ErrReferral naming the host", err)
	}
	if err := directory.SearchError(ldap.NewError(ldap.LDAPResultNoSuchObject, errors.New("no such object"))); errors.Is(err, directory.ErrReferral) {
		t.Errorf("a code-32 result = %v, want it not ErrReferral", err)
	}
}
