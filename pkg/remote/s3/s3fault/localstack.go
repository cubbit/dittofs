// Package s3fault is test support for the S3 store: LocalStack over HTTPS
// (RFC 4 §7.2, §7.4).
package s3fault

import (
	"crypto/tls"
	"crypto/x509"
	"net"
	"net/url"
	"os"
	"testing"
	"time"
)

// LocalStack returns LocalStack's HTTPS endpoint, from
// LOCALSTACK_HTTPS_ENDPOINT, and a pool holding only the certificate it
// serves there, for a client to verify every connection against.
//
// decision: with the variable unset it skips only outside CI. When CI is set
// it fails, so a cell that lost its LocalStack can't stay green with every
// test that needs it skipped. A root cell's sudo passes no CI, so this holds
// only for a suite that doesn't run as root, as integration doesn't.
func LocalStack(tb testing.TB) (endpoint string, roots *x509.CertPool) {
	tb.Helper()
	endpoint = os.Getenv("LOCALSTACK_HTTPS_ENDPOINT")
	if endpoint == "" {
		const why = "LOCALSTACK_HTTPS_ENDPOINT isn't set: run dt services up localstack"
		if os.Getenv("CI") != "" {
			tb.Fatal(why)
		}
		tb.Skip(why)
	}
	u, err := url.Parse(endpoint)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" {
		tb.Fatalf("LOCALSTACK_HTTPS_ENDPOINT %q isn't an https URL", endpoint)
	}
	// decision: this one handshake records the served certificate without
	// verifying it, since it is what every later connection verifies
	// against. Trusting it on first use is sound only on loopback, so any
	// other peer is refused; LocalStack's certificate changes per container
	// (downloaded, or self-signed when the download fails), so nothing can
	// pin it ahead.
	conn, err := tls.DialWithDialer(&net.Dialer{Timeout: 10 * time.Second}, "tcp", u.Host,
		&tls.Config{ServerName: u.Hostname(), InsecureSkipVerify: true})
	if err != nil {
		tb.Fatalf("reading LocalStack's certificate at %s: %v", u.Host, err)
	}
	defer func() { _ = conn.Close() }()
	if peer, ok := conn.RemoteAddr().(*net.TCPAddr); !ok || !peer.IP.IsLoopback() {
		tb.Fatalf("LOCALSTACK_HTTPS_ENDPOINT %s reached %v, not loopback, whose certificate isn't trusted on first use", endpoint, conn.RemoteAddr())
	}
	// A completed handshake always carries the server's certificate.
	leaf := conn.ConnectionState().PeerCertificates[0]
	roots = x509.NewCertPool()
	roots.AddCert(leaf)
	return endpoint, roots
}
