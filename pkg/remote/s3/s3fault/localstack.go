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
// test that needs it skipped.
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
	addr := u.Host
	if u.Port() == "" {
		addr = net.JoinHostPort(u.Hostname(), "443")
	}
	// decision: this one handshake records the served certificate without
	// verifying it, since it is what every later connection verifies
	// against. Trusting it on first use is sound for a container the harness
	// started on loopback; LocalStack's certificate changes per container
	// (downloaded, or self-signed when the download fails), so nothing can
	// pin it ahead.
	conn, err := tls.DialWithDialer(&net.Dialer{Timeout: 10 * time.Second}, "tcp", addr,
		&tls.Config{ServerName: u.Hostname(), InsecureSkipVerify: true})
	if err != nil {
		tb.Fatalf("reading LocalStack's certificate at %s: %v", addr, err)
	}
	leaf := conn.ConnectionState().PeerCertificates[0]
	_ = conn.Close()
	roots = x509.NewCertPool()
	roots.AddCert(leaf)
	return endpoint, roots
}
