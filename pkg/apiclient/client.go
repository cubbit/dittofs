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
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/cubbit/dittofs/pkg/node/apiv1"
)

// Client is one server's API client. Its methods are safe for concurrent use.
type Client struct {
	base   string
	http   *http.Client
	tokens TokenSource
	clock  Clock

	// checking is held while the server's version is checked, and checked
	// is read and written only while it is held.
	checking chan struct{}
	checked  bool
}

// TokenSource hands the client the access token each call carries, and a
// fresh one in place of a token the server refused 401.
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
type Option func(*Client)

// WithTLS sets the TLS configuration an https server is reached with, such
// as the pool of CAs its certificate is checked against.
func WithTLS(cfg *tls.Config) Option {
	return func(c *Client) { c.http.Transport.(*http.Transport).TLSClientConfig = cfg }
}

// WithTokenSource makes every call carry the source's token. A call refused
// 401 asks the source to refresh once and is sent again, with its body and
// idempotency key unchanged.
func WithTokenSource(ts TokenSource) Option { return func(c *Client) { c.tokens = ts } }

// WithClock sets the clock a retry waits on; without it, the wall clock.
func WithClock(clock Clock) Option { return func(c *Client) { c.clock = clock } }

// New returns a client of server: an https URL, an http URL whose host is a
// loopback address, either with no path but "/" and no credentials, query
// or fragment; or the path of a node's local socket, which holds a "/". It
// contacts nothing until its first call, and follows no redirect: an answer
// 3xx is a refusal.
func New(server string, opts ...Option) (*Client, error) {
	tr := transport(http.DefaultTransport)
	// ponytail: every request goes on a connection of its own, since
	// net/http resends a bodyless GET or a keyed request by itself, at once,
	// when its answer is lost on a reused connection, outside do's wait and
	// count. A listing's pages and a wait's polls each pay a handshake; keep
	// connections alive, folding that resend into do's bound, when they show
	// in a profile.
	tr.DisableKeepAlives = true
	c := &Client{
		http: &http.Client{Transport: tr, CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		}},
		clock:    wallClock{},
		checking: make(chan struct{}, 1),
	}
	for _, opt := range opts {
		opt(c)
	}
	refused := fmt.Errorf("apiclient: %q is neither an https URL, an http URL on a loopback address, nor a socket's path", server)
	if !strings.Contains(server, "://") {
		if !strings.Contains(server, "/") {
			return nil, refused
		}
		tr.Proxy = nil
		tr.DialContext = func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", server)
		}
		c.base = "http://socket"
		return c, nil
	}
	u, err := url.Parse(server)
	if err != nil || u.User != nil || u.Opaque != "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" ||
		u.Path != "" && u.Path != "/" || u.Hostname() == "" ||
		u.Scheme != "https" && (u.Scheme != "http" || !loopback(u.Hostname())) {
		return nil, refused
	}
	c.base = u.Scheme + "://" + u.Host
	return c, nil
}

// transport is a clone of rt when it is an *http.Transport; otherwise, as
// when a program wraps http.DefaultTransport for tracing, a transport of its
// own that still honours the proxy environment and bounds the dial and the
// TLS handshake as http.DefaultTransport does.
func transport(rt http.RoundTripper) *http.Transport {
	if tr, ok := rt.(*http.Transport); ok {
		return tr.Clone()
	}
	return &http.Transport{
		Proxy:               http.ProxyFromEnvironment,
		DialContext:         (&net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		TLSHandshakeTimeout: 10 * time.Second,
	}
}

// loopback reports whether host is a loopback address. A name, even
// localhost, is not one: it resolves to whatever the resolver says.
func loopback(host string) bool {
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// CallOption configures one call.
type CallOption func(*call)

// WithoutIdempotencyKey sends a POST or a DELETE with no Idempotency-Key, as
// a call that carries or issues a credential must be: join-domain, an API
// token's creation, :set-password. Such a call is never sent twice after a
// lost answer or a retryable refusal, since its second sending would be a
// second call.
func WithoutIdempotencyKey() CallOption { return func(c *call) { c.noKey = true } }

// WithQuery adds q to the call's query, such as a share deletion's
// ?snapshots=.
func WithQuery(q url.Values) CallOption { return func(c *call) { c.query = q } }

// call is one logical API call: its method, its path with its query, the
// query its options add, its JSON body, nil for none, the tag it sends in
// If-Match, "*" sending If-None-Match: * instead, whether it goes without an
// idempotency key, and whether a 401 is returned as it is rather than
// refreshed.
type call struct {
	method, path string
	query        url.Values
	body         []byte
	etag         string
	noKey        bool
	noRefresh    bool
}

// reply is what one sending of a call was answered.
type reply struct {
	status int
	header http.Header
	body   []byte
}

// Retry bounds: how many times one call is sent at most, a refreshed 401
// counted, and how long it waits before sending again after a lost answer,
// or after a retryable refusal naming no Retry-After.
//
// ponytail: a fixed count and a fixed wait, no backoff or jitter. Grow the
// wait when many clients retrying in step show as load on a node that is
// already refusing them.
const (
	attempts  = 5
	retryWait = time.Second
)

// maxBody is the longest answer the client reads; a longer one is an error,
// and the call is not sent again.
const maxBody = 16 << 20

var errTooLong = errors.New("apiclient: the answer is longer than 16 MiB")

// do sends c, checking the server's version first, and returns its answer,
// a 2xx; or the problem it was refused with, a 3xx included, as a
// *apiv1.Problem; or the error that left it unanswered. A POST or a DELETE
// carries one fresh Idempotency-Key, which every sending of the call reuses.
// A call is sent again, after its wait on the client's clock, when it was
// refused with a retryable problem, after the Retry-After the problem names;
// and when its answer was lost, if it is a GET or carries a key, which the
// server answers from its record. A call refused 401 is sent again once,
// with the token the source refreshes. No call is sent more than 5 times.
//
// decision: a keyless call (join-domain, :set-password, an API token's
// creation) is still refreshed and sent again after a 401, since a 401 is an
// answer that applied nothing. Sign-in, refresh and a password change are
// not: their 401 is a wrong credential, so they are sent with refresh off.
// Overturned if a server ever applies a call it then answers 401.
func (c *Client) do(ctx context.Context, cl call) (reply, error) {
	if err := c.version(ctx); err != nil {
		return reply{}, err
	}
	if len(cl.query) > 0 {
		sep := "?"
		if strings.Contains(cl.path, "?") {
			sep = "&"
		}
		cl.path += sep + cl.query.Encode()
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
			if ctx.Err() != nil || errors.Is(err, errTooLong) || n == attempts || (key == "" && cl.method != http.MethodGet) {
				return reply{}, err
			}
		case r.status == http.StatusUnauthorized && c.tokens != nil && !refreshed && !cl.noRefresh && n < attempts:
			refreshed = true
			fresh, err := c.tokens.Refresh(ctx, token)
			if err != nil {
				return reply{}, errors.Join(problemOf(r), err)
			}
			token = fresh
			continue
		case r.status >= 300:
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
// decision: net/http never sends a request a second time itself: GetBody is
// cleared, so a request with a body is never rewound, and no connection is
// reused, so a lost answer is never resent at once. Every second sending is
// do's, under the one rule do states. Overturned if a transport needs
// GetBody, which only a followed redirect would, and the client follows
// none.
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
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
	if err == nil && len(b) > maxBody {
		err = errTooLong
	}
	return reply{resp.StatusCode, resp.Header, b}, err
}

// version checks, once per client, that the server serves v1. A check that
// fails to complete runs again on the next call; a caller waiting on
// another's check stops when its context ends.
func (c *Client) version(ctx context.Context) error {
	select {
	case c.checking <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	defer func() { <-c.checking }()
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
	if err := checkVersion(r.body); err != nil {
		return err
	}
	c.checked = true
	return nil
}

// checkVersion refuses a version body that names no v1, with an error
// naming both, or that is no version, quoting at most its first 64 bytes.
func checkVersion(body []byte) error {
	var v apiv1.Version
	if err := json.Unmarshal(body, &v); err != nil {
		return fmt.Errorf("apiclient: the server's version %q: %w", body[:min(len(body), 64)], err)
	}
	if !slices.Contains(v.Majors, "v1") {
		return fmt.Errorf("apiclient: the server serves API versions %q, and this client speaks only v1", v.Majors)
	}
	return nil
}

// wallClock is the wall clock's time.
type wallClock struct{}

func (wallClock) After(d time.Duration) <-chan struct{} {
	ch := make(chan struct{})
	time.AfterFunc(d, func() { close(ch) })
	return ch
}
