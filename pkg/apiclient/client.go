// Package apiclient is the management API's client: it reaches a node over
// https, over http on loopback, or over the node's local socket, checks at
// first contact that the node serves a version it speaks, and sends every
// call with the headers the API requires, retrying what the API says may be
// retried.
package apiclient

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/cubbit/dittofs/pkg/node/apiv1"
)

// Client is one server's API client. Its methods are safe for concurrent use.
type Client struct {
	base   string
	http   *http.Client
	tokens TokenSource
	clock  Clock

	mu      sync.Mutex
	checked bool
}

// TokenSource hands the client the access token each call carries, and a
// fresh one when the server refuses the token failed 401.
type TokenSource interface {
	Token(ctx context.Context) (string, error)
	Refresh(ctx context.Context, failed string) (string, error)
}

// Clock is the time a retry waits on: a channel that closes once d has
// passed.
type Clock interface {
	After(d time.Duration) <-chan struct{}
}

// Option configures a Client.
type Option func(*options)

type options struct {
	tls    *tls.Config
	tokens TokenSource
	clock  Clock
}

// WithTLS sets the TLS configuration an https server is reached with, such
// as the pool of CAs its certificate is checked against.
func WithTLS(cfg *tls.Config) Option { return func(o *options) { o.tls = cfg } }

// WithTokenSource makes every call carry the source's token. A call refused
// 401 asks the source to refresh once and is sent again, with its body and
// idempotency key unchanged.
func WithTokenSource(ts TokenSource) Option { return func(o *options) { o.tokens = ts } }

// WithClock sets the clock a retry waits on; without it, the wall clock.
func WithClock(c Clock) Option { return func(o *options) { o.clock = c } }

// New returns a client of server: an https URL, an http URL whose host is
// loopback, or the path of a node's local socket. It contacts nothing until
// its first call.
func New(server string, opts ...Option) (*Client, error) {
	var o options
	for _, opt := range opts {
		opt(&o)
	}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.TLSClientConfig = o.tls
	c := &Client{base: strings.TrimSuffix(server, "/"), http: &http.Client{Transport: tr}, tokens: o.tokens, clock: o.clock}
	if c.clock == nil {
		c.clock = wallClock{}
	}
	scheme, rest, found := strings.Cut(server, "://")
	host, _, _ := strings.Cut(rest, "/")
	switch {
	case !found:
		tr.Proxy = nil
		tr.DialContext = func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", server)
		}
		c.base = "http://socket"
	case scheme == "https":
	case scheme == "http" && loopback(host):
	default:
		return nil, fmt.Errorf("apiclient: %q is neither an https URL, an http URL on loopback, nor a socket's path", server)
	}
	return c, nil
}

// loopback reports whether hostport's host is this machine.
func loopback(hostport string) bool {
	h := hostport
	if sh, _, err := net.SplitHostPort(hostport); err == nil {
		h = sh
	}
	ip := net.ParseIP(strings.Trim(h, "[]"))
	return h == "localhost" || ip != nil && ip.IsLoopback()
}

// CallOption configures one call.
type CallOption func(*call)

// WithoutIdempotencyKey sends a POST or a DELETE with no Idempotency-Key, as
// a call that carries or issues a credential must be: join-domain, an API
// token's creation, :set-password. Such a call is never sent twice after a
// lost answer or a retryable refusal, since its second sending would be a
// second call.
func WithoutIdempotencyKey() CallOption { return func(c *call) { c.noKey = true } }

// call is one logical API call: its method, its path with its query, its
// JSON body, nil for none, the tag it sends in If-Match, "*" sending
// If-None-Match: * instead, and whether it goes without an idempotency key.
type call struct {
	method, path string
	body         []byte
	etag         string
	noKey        bool
}

// reply is what one sending of a call was answered.
type reply struct {
	status int
	header http.Header
	body   []byte
}

// Retry bounds: how many times one call is sent at most, and how long it
// waits before sending again after a lost answer, or after a retryable
// refusal naming no Retry-After.
//
// ponytail: a fixed count and a fixed wait, no backoff or jitter. Grow the
// wait when many clients retrying in step show as load on a node that is
// already refusing them.
const (
	attempts  = 5
	retryWait = time.Second
)

// do sends c, checking the server's version first, and returns its answer,
// a 2xx; or the problem it was refused with as a *apiv1.Problem; or the
// error that left it unanswered. A POST or a DELETE carries one fresh
// Idempotency-Key, which every sending of the call reuses. A call is sent
// again, after its wait on the client's clock, when it was refused with a
// retryable problem, after the Retry-After the problem names; and when its
// answer was lost, if it is a GET or carries a key, which the server answers
// from its record. A call refused 401 is sent again once, with the token the
// source refreshes.
//
// decision: a call without a key, a credential call, is still refreshed and
// sent again after a 401, since a 401 is an answer that applied nothing, so
// the second sending is not a retry of a call that may have run. Overturned
// if a server ever applies a credential call it then answers 401.
func (c *Client) do(ctx context.Context, cl call) (reply, error) {
	if err := c.version(ctx); err != nil {
		return reply{}, err
	}
	var key string
	if (cl.method == http.MethodPost || cl.method == http.MethodDelete) && !cl.noKey {
		key = rand.Text()
	}
	var token string
	if c.tokens != nil {
		var err error
		if token, err = c.tokens.Token(ctx); err != nil {
			return reply{}, err
		}
	}
	refreshed := false
	for n := 1; ; n++ {
		r, err := c.send(ctx, cl, key, token)
		wait := retryWait
		switch {
		case err != nil:
			if ctx.Err() != nil || n == attempts || (key == "" && cl.method != http.MethodGet) {
				return reply{}, err
			}
		case r.status == http.StatusUnauthorized && c.tokens != nil && !refreshed:
			refreshed = true
			fresh, err := c.tokens.Refresh(ctx, token)
			if err != nil {
				return reply{}, errors.Join(problemOf(r), err)
			}
			token = fresh
			continue
		case r.status >= 400:
			p := problemOf(r)
			if !p.Retryable || cl.noKey || n == attempts {
				return reply{}, p
			}
			wait = retryAfter(r.header)
		default:
			return r, nil
		}
		select {
		case <-ctx.Done():
			return reply{}, ctx.Err()
		case <-c.clock.After(wait):
		}
	}
}

// send sends c once, with key and token when they are set.
//
// decision: the request's GetBody is cleared, so net/http never sends a
// request with a body a second time itself; every second sending is do's,
// under the one rule do states. Overturned if a transport needs GetBody,
// such as for a redirect, which the API never answers.
func (c *Client) send(ctx context.Context, cl call, key, token string) (reply, error) {
	req, err := http.NewRequestWithContext(ctx, cl.method, c.base+cl.path, bytes.NewReader(cl.body))
	if err != nil {
		return reply{}, err
	}
	req.GetBody = nil
	if cl.body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	switch cl.etag {
	case "":
	case "*":
		req.Header.Set("If-None-Match", "*")
	default:
		req.Header.Set("If-Match", cl.etag)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return reply{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	return reply{resp.StatusCode, resp.Header, b}, err
}

// version checks, once per client, that the server serves v1, refusing one
// that does not with an error naming both. A check that fails to complete
// runs again on the next call.
func (c *Client) version(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.checked {
		return nil
	}
	r, err := c.send(ctx, call{method: http.MethodGet, path: "/api/version"}, "", "")
	if err != nil {
		return err
	}
	if r.status != http.StatusOK {
		return problemOf(r)
	}
	var v apiv1.Version
	if err := json.Unmarshal(r.body, &v); err != nil {
		return fmt.Errorf("apiclient: the server's version %s: %w", r.body, err)
	}
	if !slices.Contains(v.Majors, "v1") {
		return fmt.Errorf("apiclient: the server serves API versions %q, and this client speaks only v1", v.Majors)
	}
	c.checked = true
	return nil
}

// wallClock is the wall clock's time.
type wallClock struct{}

func (wallClock) After(d time.Duration) <-chan struct{} {
	ch := make(chan struct{})
	time.AfterFunc(d, func() { close(ch) })
	return ch
}
