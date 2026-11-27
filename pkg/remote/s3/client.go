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
// namespaces opened on it: one attempt per call, HTTPS and HTTP/1.1 only, and
// at most Limit connections to the service.
type Client struct {
	cfg  ClientConfig
	log  *slog.Logger
	rec  Recorder
	http *http.Client
	api  *s3.Client

	// peerClosed counts the connections the service closed: a read that ended
	// in EOF or a reset before the client closed the connection itself.
	peerClosed atomic.Int64
}

// NewClient builds the client of cfg. It refuses an endpoint that is not an
// https URL with a host, an empty bucket, a limit below 1, and a nil logger or
// recorder with ErrInvalid, before any connection is opened.
func NewClient(cfg ClientConfig, d Deps) (*Client, error) {
	switch err := checkEndpoint(cfg.Endpoint); {
	case err != nil:
		return nil, fmt.Errorf("remote/s3: endpoint %v: %w", err, remote.ErrInvalid)
	case cfg.Bucket == "":
		return nil, fmt.Errorf("remote/s3: bucket is empty: %w", remote.ErrInvalid)
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

	tr := &http.Transport{
		DialTLSContext:      c.dial,
		MaxConnsPerHost:     cfg.Limit,
		MaxIdleConnsPerHost: cfg.Limit,
		MaxIdleConns:        cfg.Limit,
		// HTTP/1.1 only, since the caps bind no HTTP/2 stream. dial's
		// connection is no *tls.Conn, so the transport could not take HTTP/2
		// on it anyway; these keep it so if dial ever returns one.
		ForceAttemptHTTP2: false,
		TLSNextProto:      map[string]func(string, *tls.Conn) http.RoundTripper{},
	}

	var rt http.RoundTripper = tr
	if d.Transport != nil {
		rt = d.Transport(rt)
	}

	c.http = &http.Client{
		Transport: rt,
		// A 3xx reaches the table as cause access, and the signed request
		// never reaches the host it names.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}

	c.api = s3.New(s3.Options{
		BaseEndpoint: aws.String(cfg.Endpoint),
		Region:       cfg.Region,
		UsePathStyle: true,
		Credentials:  credentials.NewStaticCredentialsProvider(cfg.AccessKeyID, cfg.SecretAccessKey, ""),
		HTTPClient:   c.http,
		Retryer:      aws.NopRetryer{},
		// Pinned, since the SDK's defaults have changed between releases: the
		// store sends exactly the integrity header its profile chose.
		RequestChecksumCalculation: aws.RequestChecksumCalculationWhenRequired,
		ResponseChecksumValidation: aws.ResponseChecksumValidationWhenRequired,
		APIOptions:                 []func(*middleware.Stack) error{v4.SwapComputePayloadSHA256ForUnsignedPayloadMiddleware},
	})

	return c, nil
}

// Close closes the client's idle connections. Its holder calls it once, after
// every store opened on it has closed.
func (c *Client) Close() error {
	c.http.CloseIdleConnections()

	return nil
}

// dial opens one TLS connection to addr, verified against the trust roots or
// the system's, and counts it.
func (c *Client) dial(ctx context.Context, network, addr string) (net.Conn, error) {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}

	raw, err := (&net.Dialer{}).DialContext(ctx, network, addr)
	if err != nil {
		return nil, err
	}

	c.rec.ConnOpened()

	tc := tls.Client(raw, &tls.Config{RootCAs: c.cfg.TrustRoots, ServerName: host})
	if err := tc.HandshakeContext(ctx); err != nil {
		_ = raw.Close()
		return nil, err
	}

	return &countedConn{Conn: tc, c: c}, nil
}

// countedConn counts the service closing it, once, and never a close the
// client started.
type countedConn struct {
	net.Conn
	c       *Client
	closing atomic.Bool
	counted atomic.Bool
}

func (k *countedConn) Read(p []byte) (int, error) {
	n, err := k.Conn.Read(p)
	if (errors.Is(err, io.EOF) || errors.Is(err, syscall.ECONNRESET)) && !k.closing.Load() && k.counted.CompareAndSwap(false, true) {
		k.c.peerClosed.Add(1)
	}

	return n, err
}

func (k *countedConn) Close() error {
	k.closing.Store(true)

	return k.Conn.Close()
}
