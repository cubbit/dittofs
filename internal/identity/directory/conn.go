package directory

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/url"
	"time"

	"github.com/go-ldap/ldap/v3"
)

// ceiling bounds each dial and call whose context sets no earlier deadline, so
// one to a directory that accepts and never answers still ends.
//
// decision: a real-clock backstop at RFC 18 §5.1's 20-second bound, above its
// 5-second query timeout: a caller's context carries those and ends no later,
// and the ceiling bounds a caller whose context carries none. v1 bounded each
// request at 10 s and each dial at 60 s. It is a constant; tests dial with a
// lower one through the package's test hook.
const ceiling = 20 * time.Second

// Conn is a connection to the directory: the two calls identity makes of it.
// Its types are the package's own, so a directory that is not LDAP, such as
// identitytest's in-memory one, stands in without importing LDAP.
type Conn interface {
	// Bind sends a simple bind of name, a distinguished name or a UPN, with
	// password.
	Bind(ctx context.Context, name string, password []byte) error
	// Search runs one search. A directory's continuation references, to
	// parts of the answer it holds elsewhere, are handed back in
	// Result.Referrals, and a referral, the whole answer elsewhere, is an
	// error; neither is followed.
	Search(ctx context.Context, req Request) (Result, error)
	// Close closes the connection.
	Close() error
}

// Dialer opens a connection to the directory rec names. DialTLS is the
// production one.
type Dialer func(ctx context.Context, rec Record) (Conn, error)

// Scope is how far below Request.Base a search reaches, numbered as RFC 4511
// numbers it.
type Scope int

// The three scopes.
const (
	ScopeBase    Scope = 0 // the base entry alone
	ScopeOne     Scope = 1 // the base's immediate children
	ScopeSubtree Scope = 2 // the base and every entry below it
)

// Request is one search.
type Request struct {
	Base   string // the entry the search starts from; "" with ScopeBase reads the root DSE
	Scope  Scope
	Filter string   // an RFC 4515 filter
	Attrs  []string // the attributes to return; none returns every user attribute
	// PageSize, when positive, asks for an RFC 2696 page of at most that many
	// entries, continuing the search Cookie names; a nil Cookie starts it.
	PageSize int
	Cookie   []byte
}

// Result is a search's answer.
type Result struct {
	Entries []Entry
	// Cookie continues a paged search; empty when it is done.
	Cookie []byte
	// Referrals are the URIs of the continuation references: where the
	// directory says more of the answer may be, such as Active Directory's
	// configuration and DNS partitions below its domain.
	Referrals []string
}

// Entry is one entry of a result.
type Entry struct {
	DN string
	// Attrs holds each returned attribute's values, keyed by the name the
	// directory returned it under.
	Attrs map[string][][]byte
}

// DialTLS dials the directory rec names over TLS: ldaps:// with TLS from the
// first byte, ldap:// upgraded by StartTLS before the connection is returned,
// so no bind can cross it in clear. The server's chain is verified against
// rec.TrustRoots, or the host's roots when it names none, and its name
// against the address's host. When rec.DNSServer is set, the host is resolved
// through it rather than the host's resolver.
//
// A call ends when its context does or ceiling passes, whichever is first,
// and one ended so closes the connection, which is then of no further use.
func DialTLS(ctx context.Context, rec Record) (Conn, error) {
	return dialTLS(ctx, rec, ceiling)
}

// dialTLS is DialTLS with the ceiling bounding the dial and each call on the
// connection it returns.
func dialTLS(ctx context.Context, rec Record, ceiling time.Duration) (Conn, error) {
	if err := rec.Validate(); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, ceiling)
	defer cancel()
	u, err := url.Parse(rec.Address)
	if err != nil {
		return nil, err
	}
	host, port := u.Hostname(), u.Port()
	if port == "" {
		port = "389"
		if u.Scheme == "ldaps" {
			port = "636"
		}
	}

	cfg := &tls.Config{ServerName: host, MinVersion: tls.VersionTLS12}
	if rec.TrustRoots != "" {
		cfg.RootCAs = x509.NewCertPool()
		cfg.RootCAs.AppendCertsFromPEM([]byte(rec.TrustRoots))
	}

	d := &net.Dialer{}
	if rec.DNSServer != "" {
		server := rec.DNSServer
		if _, _, err := net.SplitHostPort(server); err != nil {
			server = net.JoinHostPort(server, "53")
		}
		d.Resolver = &net.Resolver{PreferGo: true, Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, server)
		}}
	}
	raw, err := d.DialContext(ctx, "tcp", net.JoinHostPort(host, port))
	if err != nil {
		return nil, fmt.Errorf("identity.directory: dial %s: %w", rec.Address, err)
	}

	if u.Scheme == "ldaps" {
		tc := tls.Client(raw, cfg)
		if err := tc.HandshakeContext(ctx); err != nil {
			_ = raw.Close()
			return nil, fmt.Errorf("identity.directory: TLS to %s: %w", rec.Address, err)
		}
		l := ldap.NewConn(tc, true)
		l.Start()
		return &ldapConn{l: l, ceiling: ceiling}, nil
	}

	l := ldap.NewConn(raw, false)
	l.Start()
	// decision: StartTLS is ended by a deadline on the socket, never by a
	// Close from another goroutine: once its handshake is done, go-ldap's
	// StartTLS swaps in the TLS connection without a lock, and Close reads it.
	stop := context.AfterFunc(ctx, func() { _ = raw.SetDeadline(time.Now()) })
	err = l.StartTLS(cfg)
	if !stop() {
		err = ctx.Err()
	}
	if err != nil {
		_ = l.Close()
		return nil, fmt.Errorf("identity.directory: StartTLS to %s: %w", rec.Address, err)
	}
	return &ldapConn{l: l, ceiling: ceiling}, nil
}

// ldapConn is a Conn over go-ldap.
type ldapConn struct {
	l       *ldap.Conn
	ceiling time.Duration // bounds each call whose context sets no earlier deadline
}

// during runs f, which go-ldap gives no context, closing the connection if
// ctx ends or the ceiling passes first, so a call outlives neither; the error
// is then the context's.
func (c *ldapConn) during(ctx context.Context, f func() error) error {
	ctx, cancel := context.WithTimeout(ctx, c.ceiling)
	defer cancel()
	stop := context.AfterFunc(ctx, func() { _ = c.l.Close() })
	err := f()
	if !stop() {
		return ctx.Err()
	}
	return err
}

func (c *ldapConn) Bind(ctx context.Context, name string, password []byte) error {
	// decision: a bind with an empty password is refused here, before any
	// byte is sent, so no unauthenticated bind ever reaches the directory. A
	// name with an empty password is an unauthenticated bind (RFC 4513
	// §5.1.2), which a permissive server answers with success, so a credential
	// that resolved to an empty secret would look bound while holding only
	// anonymous access. go-ldap refuses it too: v3.4.15's SimpleBind returns
	// ErrorEmptyPassword before sending when AllowEmptyPassword is false, as
	// the request below spells out. This check makes the rule ours, so it
	// holds across a library upgrade that changes that default or that check,
	// and it answers with go-ldap's own error class for the refusal. Drop it
	// only if identity ever needs an anonymous bind, and then send it as one,
	// never as a named bind with no password.
	if len(password) == 0 {
		return ldap.NewError(ldap.ErrorEmptyPassword,
			errors.New("identity.directory: a bind with an empty password is refused"))
	}
	return c.during(ctx, func() error {
		_, err := c.l.SimpleBind(&ldap.SimpleBindRequest{
			Username: name, Password: string(password), AllowEmptyPassword: false})
		return err
	})
}

func (c *ldapConn) Search(ctx context.Context, req Request) (Result, error) {
	var controls []ldap.Control
	var paging *ldap.ControlPaging
	if req.PageSize > 0 {
		paging = ldap.NewControlPaging(uint32(req.PageSize))
		paging.SetCookie(req.Cookie)
		controls = append(controls, paging)
	}
	sr := ldap.NewSearchRequest(req.Base, int(req.Scope), ldap.NeverDerefAliases, 0, 0, false,
		req.Filter, req.Attrs, controls)

	var res *ldap.SearchResult
	err := c.during(ctx, func() (err error) {
		res, err = c.l.Search(sr)
		return err
	})
	if err != nil {
		return Result{}, err
	}

	out := Result{Referrals: res.Referrals}
	for _, e := range res.Entries {
		attrs := make(map[string][][]byte, len(e.Attributes))
		for _, a := range e.Attributes {
			attrs[a.Name] = a.ByteValues
		}
		out.Entries = append(out.Entries, Entry{DN: e.DN, Attrs: attrs})
	}
	if paging != nil {
		if p, ok := ldap.FindControl(res.Controls, ldap.ControlTypePaging).(*ldap.ControlPaging); ok {
			out.Cookie = p.Cookie
		}
	}
	return out, nil
}

func (c *ldapConn) Close() error { return c.l.Close() }
