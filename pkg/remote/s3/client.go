package s3

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go/middleware"

	"github.com/cubbit/dittofs/pkg/remote"
)

// ClientConfig is one store configuration's client (RFC 4 §4.2, §4.10,
// Appendix C.3).
type ClientConfig struct {
	Endpoint        string         // https:// only
	Region          string         // the signing region; "" is us-east-1
	Bucket          string         // the bucket every namespace on the client lives in
	StorageClass    string         // the class every object is written in; "" is STANDARD
	AccessKeyID     string         // the credential's key
	SecretAccessKey string         // the credential's secret
	TrustRoots      *x509.CertPool // nil: the system's roots; never a way to skip verification
	Limit           int            // the derived connection limit (RFC 4 §4.10); at least 1
}

// Deps are what the root injects (RFC 25 §8.1).
type Deps struct {
	Logger   *slog.Logger
	Recorder Recorder
	// Transport wraps the client's real transport; nil in production. The
	// fault transport is passed here by every test that runs a fault row
	// (RFC 4 §7.2).
	Transport func(http.RoundTripper) http.RoundTripper
}

// Client is the S3 client one store configuration shares across the
// namespaces opened on it: HTTPS and HTTP/1.1 only, SDK retries off, and at
// most Limit connections to the service.
type Client struct {
	cfg ClientConfig
	log *slog.Logger
	rec Recorder
	tr  *http.Transport
	api *s3.Client

	// peerClosed counts the connections the service closed: a reply carrying
	// Connection: close, or a read that ended in EOF or a reset before the
	// client closed the connection itself. No metric reads it; the steady-load
	// test bounds the connections opened by it.
	peerClosed atomic.Int64
}

// Dial bounds, net/http's DefaultTransport's: the context a dial gets is
// detached from the request's, so without them a service that accepts and
// never answers would hold a connection slot for good.
const (
	dialTimeout      = 30 * time.Second
	handshakeTimeout = 10 * time.Second
)

// NewClient builds the client of cfg. It refuses an endpoint that is not an
// https URL with a host, an empty bucket, access key or secret, a limit below
// 1, and a nil logger or recorder with ErrInvalid, before any connection is
// opened.
func NewClient(cfg ClientConfig, d Deps) (*Client, error) {
	switch err := checkEndpoint(cfg.Endpoint); {
	case err != nil:
		return nil, fmt.Errorf("remote/s3: endpoint %v: %w", err, remote.ErrInvalid)
	case cfg.Bucket == "":
		return nil, fmt.Errorf("remote/s3: bucket is empty: %w", remote.ErrInvalid)
	case cfg.AccessKeyID == "" || cfg.SecretAccessKey == "":
		// The SDK would fail every call before sending it, which reads as
		// ErrTransient: a configuration mistake retried for ever.
		return nil, fmt.Errorf("remote/s3: the access key or the secret is empty: %w", remote.ErrInvalid)
	case cfg.Limit < 1:
		return nil, fmt.Errorf("remote/s3: limit %d is below 1: %w", cfg.Limit, remote.ErrInvalid)
	case d.Logger == nil || d.Recorder == nil:
		return nil, fmt.Errorf("remote/s3: a logger and a recorder are required: %w", remote.ErrInvalid)
	}

	if cfg.Region == "" {
		cfg.Region = "us-east-1"
	}

	if cfg.StorageClass == "" {
		cfg.StorageClass = "STANDARD"
	}

	c := &Client{cfg: cfg, log: d.Logger.With("component", "remote"), rec: d.Recorder}

	// decision: one attempt per call holds above net/http only. Its transport
	// resends a GET once, by itself, when the pooled connection it went out on
	// turns out closed by the service before any byte of the reply, so a Get,
	// GetControl or List can reach the service twice. No setting turns that
	// off short of disabling keep-alives, which the connection caps need; a
	// GET changes nothing, so the resend repeats only a read. Withdraw it if
	// a profile ever shows a resend that is not on an idle connection the
	// service dropped.
	//
	// decision: no forward proxy (Proxy is nil, so HTTPS_PROXY is not read):
	// net/http skips DialTLSContext on a proxied connection, which would drop
	// the trust roots, the counting and the HTTP/1.1 pin together.
	c.tr = &http.Transport{
		DialTLSContext:      c.dial,
		MaxConnsPerHost:     cfg.Limit,
		MaxIdleConnsPerHost: cfg.Limit,
		MaxIdleConns:        cfg.Limit,
		// HTTP/1.1 only: the caps bind no HTTP/2 stream.
		ForceAttemptHTTP2: false,
		TLSNextProto:      map[string]func(string, *tls.Conn) http.RoundTripper{},
	}

	var rt http.RoundTripper = closeCounter{c.tr, &c.peerClosed}
	if d.Transport != nil {
		rt = d.Transport(rt)
	}

	c.api = s3.New(s3.Options{
		BaseEndpoint: aws.String(cfg.Endpoint),
		Region:       cfg.Region,
		UsePathStyle: true,
		Credentials:  credentials.NewStaticCredentialsProvider(cfg.AccessKeyID, cfg.SecretAccessKey, ""),
		HTTPClient: &http.Client{
			Transport: rt,
			// A 3xx reaches the table as cause access, and the signed request
			// never reaches the host it names.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
		Retryer: aws.NopRetryer{},
		// Pinned rather than left to the SDK, whose defaults have changed
		// between releases: the store sends exactly the integrity header its
		// profile chose.
		RequestChecksumCalculation: aws.RequestChecksumCalculationWhenRequired,
		ResponseChecksumValidation: aws.ResponseChecksumValidationWhenRequired,
		// An S3 Express-shaped bucket name would make the SDK send a
		// CreateSession request of its own before the call.
		DisableS3ExpressSessionAuth: aws.Bool(true),
		APIOptions:                  []func(*middleware.Stack) error{v4.SwapComputePayloadSHA256ForUnsignedPayloadMiddleware},
	})

	return c, nil
}

// Close closes the client's idle connections. Its holder calls it once, after
// every store opened on it has closed.
func (c *Client) Close() error {
	c.tr.CloseIdleConnections()

	return nil
}

// dial opens one TLS connection to addr, verified against the trust roots or
// the system's, and counts it once its handshake succeeds, so a refused
// certificate opens nothing the pool could reuse.
func (c *Client) dial(ctx context.Context, network, addr string) (net.Conn, error) {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}

	raw, err := (&net.Dialer{Timeout: dialTimeout}).DialContext(ctx, network, addr)
	if err != nil {
		return nil, err
	}

	hctx, cancel := context.WithTimeout(ctx, handshakeTimeout)
	defer cancel()

	tc := tls.Client(raw, &tls.Config{RootCAs: c.cfg.TrustRoots, ServerName: host})
	if err := tc.HandshakeContext(hctx); err != nil {
		_ = raw.Close()
		return nil, err
	}

	c.rec.ConnOpened()

	return &countedConn{Conn: tc, peer: &c.peerClosed}, nil
}

// closeCounter counts a reply carrying Connection: close, after which the
// transport closes the connection itself without reading its end.
type closeCounter struct {
	next http.RoundTripper
	peer *atomic.Int64
}

func (cc closeCounter) RoundTrip(r *http.Request) (*http.Response, error) {
	resp, err := cc.next.RoundTrip(r)
	if resp != nil && resp.Close {
		cc.peer.Add(1)
	}

	return resp, err
}

// countedConn counts the service closing it, once, and never once the client
// has started closing it.
type countedConn struct {
	net.Conn
	peer *atomic.Int64
	done atomic.Bool // counted, or closing
}

func (k *countedConn) Read(p []byte) (int, error) {
	n, err := k.Conn.Read(p)
	if (errors.Is(err, io.EOF) || errors.Is(err, syscall.ECONNRESET)) && k.done.CompareAndSwap(false, true) {
		k.peer.Add(1)
	}

	return n, err
}

func (k *countedConn) Close() error {
	k.done.Store(true)

	return k.Conn.Close()
}
