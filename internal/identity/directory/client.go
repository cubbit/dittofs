package directory

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/cubbit/dittofs/pkg/vfs/vfserr"
)

// Clock is the client's source of time: Now for an account's expiry, After
// for the query timeout.
type Clock interface {
	Now() time.Time
	After(d time.Duration) <-chan struct{} // fires after d on the monotonic clock
}

// Password resolves the bind password. It is called at each dial, so a
// rotated credential is read at the next one.
type Password func(ctx context.Context) ([]byte, error)

// queryTimeout bounds each dial, bind and search on the client's clock.
const queryTimeout = 5 * time.Second

// errTimeout is a call the query timeout ended.
var errTimeout = errors.New("identity.directory: no answer within the query timeout")

// Key is what a "who holds this" question looks up.
type Key int

// The keys a question looks up.
const (
	ByUserName  Key = iota + 1 // a user's name, under the record's user name attribute
	ByGroupName                // a group's name, under the record's group name attribute
	BySID                      // an objectSid, its binary form
	ByEntryID                  // an Answer's EntryID
)

// Answer is the one entry that holds what a question asked.
type Answer struct {
	DN    string
	Group bool   // a group's entry; else a user's
	Name  string // under the record's user or group name attribute
	SID   []byte // objectSid, binary; nil when the entry has none
	// EntryID is the entry's immutable ID: an ad entry's objectGUID, binary,
	// or an rfc2307 entry's entryUUID (RFC 4530), as text.
	EntryID []byte
	// Enabled is false for a user whose userAccountControl holds the
	// disable bit or whose accountExpires has passed; always true for a
	// group.
	Enabled  bool
	Mail     string  // "" when the entry has none
	UID, GID *uint32 // uidNumber and gidNumber, nil when the entry has none
}

// Client asks the directory a record names its questions, over one Conn it
// dials at its first question, never before, and dials again after a call on
// it fails.
//
// ponytail: one directory, read directly over LDAP: no forest trusts, no
// global catalogue, no second directory; NTLM pass-through and SID lookups
// reach only its domain. Follow trusts, and route pass-through to the
// trusted domain's controller, when a deployment has users in a second
// domain or forest.
type Client struct {
	rec      Record
	dial     Dialer
	password Password
	clock    Clock
	log      *slog.Logger

	connecting chan struct{} // held while a connection is opened or its domain SID read

	mu       sync.Mutex
	conn     Conn   // nil until the first question, and after a failed call
	sid      []byte // the domain SID, once read; never read for an rfc2307 record
	inflight int    // questions running
	closed   bool
}

// NewClient returns a client of the directory rec names, dialled through dial
// and bound as rec.BindName with the password password resolves. It dials
// nothing.
func NewClient(rec Record, dial Dialer, password Password, clock Clock, log *slog.Logger) *Client {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Client{rec: rec, dial: dial, password: password, clock: clock, log: log,
		connecting: make(chan struct{}, 1)}
}

// unavailable wraps err in vfserr.ErrUnavailable.
func unavailable(what string, err error) error {
	return fmt.Errorf("%w: identity.directory: %s: %w", vfserr.ErrUnavailable, what, err)
}

// Lookup answers "who holds this": the one entry below the record's base
// holding value under by, false when none does. A value more than one entry
// holds, a refusal, a referral and a timeout each fail ErrUnavailable,
// never "no such entry"; continuation references in the answer are ignored.
func (c *Client) Lookup(ctx context.Context, by Key, value []byte) (Answer, bool, error) {
	filter, err := c.rec.filter(by, value)
	if err != nil {
		return Answer{}, false, err
	}
	if filter == "" {
		return Answer{}, false, nil // an empty value: LDAP holds no empty one
	}
	c.begin()
	defer c.end()
	_, res, err := c.do(ctx, Request{Base: c.rec.Base, Scope: ScopeSubtree, Filter: filter, Attrs: c.rec.attrs()})
	if err != nil {
		return Answer{}, false, err
	}
	switch len(res.Entries) {
	case 0:
		return Answer{}, false, nil
	case 1:
		return c.answer(res.Entries[0]), true, nil
	}
	c.log.Warn("identity.directory: a lookup answered by more than one entry, refused",
		"first", res.Entries[0].DN, "second", res.Entries[1].DN, "entries", len(res.Entries))
	return Answer{}, false, unavailable("lookup", fmt.Errorf("%d entries answer it", len(res.Entries)))
}

// DomainSID is the objectSid of the entry the root DSE's defaultNamingContext
// names, read once per client before its first answer; nil for an rfc2307
// record, which reads none.
func (c *Client) DomainSID(ctx context.Context) ([]byte, error) {
	c.begin()
	defer c.end()
	if _, _, err := c.connect(ctx); err != nil {
		return nil, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.sid), nil
}

// Test is test-directory's call: on a connection of its own, closed after,
// it binds, reads the record's base entry and returns it.
func (c *Client) Test(ctx context.Context) (Entry, error) {
	conn, err := c.open(ctx)
	if err != nil {
		return Entry{}, err
	}
	defer func() { _ = conn.Close() }()
	// The connection is this call's alone, so its context may end it.
	res, err := c.searchOn(ctx, conn, Request{Base: c.rec.Base, Scope: ScopeBase, Filter: "(objectClass=*)", Attrs: []string{"objectClass"}})
	if err != nil {
		return Entry{}, err
	}
	if len(res.Entries) != 1 {
		return Entry{}, unavailable("test", fmt.Errorf("the base %q answered %d entries", c.rec.Base, len(res.Entries)))
	}
	return res.Entries[0], nil
}

// Close closes the client's connection once the questions running on it
// have returned. A question asked after Close still answers, on a connection
// closed when it returns, so a lookup that read the client before it was
// replaced finishes on it.
func (c *Client) Close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed = true
	c.closeIdle()
}

func (c *Client) begin() {
	c.mu.Lock()
	c.inflight++
	c.mu.Unlock()
}

func (c *Client) end() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.inflight--
	c.closeIdle()
}

// closeIdle closes the connection of a closed client no question is using;
// c.mu is held.
func (c *Client) closeIdle() {
	if c.closed && c.inflight == 0 && c.conn != nil {
		_ = c.conn.Close()
		c.conn = nil
	}
}

// bounded is ctx capped at the query timeout on the client's clock.
func (c *Client) bounded(ctx context.Context) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancelCause(ctx)
	fired := c.clock.After(queryTimeout)
	go func() {
		select {
		case <-fired:
			cancel(errTimeout)
		case <-ctx.Done():
		}
	}()
	return ctx, func() { cancel(nil) }
}

// timedOut is err, or errTimeout when the query timeout ended ctx.
func timedOut(ctx context.Context, err error) error {
	if context.Cause(ctx) == errTimeout {
		return errTimeout
	}
	return err
}

// detach runs f on ctx without its cancellation and waits for it or for ctx
// to end, so a caller that stops waiting leaves a call other questions share
// (the connection's dial, bind and domain SID read, a search on it) to finish
// or be ended by the query timeout, rather than close the connection under
// them.
func detach[T any](ctx context.Context, what string, f func(context.Context) (T, error)) (T, error) {
	type result struct {
		v   T
		err error
	}
	done := make(chan result, 1)
	go func() {
		v, err := f(context.WithoutCancel(ctx))
		done <- result{v, err}
	}()
	select {
	case r := <-done:
		return r.v, r.err
	case <-ctx.Done():
		var zero T
		return zero, unavailable(what, ctx.Err())
	}
}

// open resolves the bind password, refusing an empty one before any dial,
// then dials and binds a new connection.
func (c *Client) open(ctx context.Context) (Conn, error) {
	pctx, cancel := c.bounded(ctx)
	pw, err := c.password(pctx)
	err = timedOut(pctx, err)
	cancel()
	if err != nil {
		return nil, unavailable("bind credential", err)
	}
	// decision: an empty bind password is refused before the dial, not only
	// at Conn.Bind, so no connection is even opened with a credential that
	// would bind anonymously (RFC 4513 §5.1.2) on a permissive directory.
	if len(pw) == 0 {
		return nil, unavailable("bind credential", errors.New("it resolved to an empty password"))
	}
	dctx, cancel := c.bounded(ctx)
	conn, err := c.dial(dctx, c.rec)
	err = timedOut(dctx, err)
	cancel()
	if err != nil {
		c.log.Debug("identity.directory: dial failed", "address", c.rec.Address, "error", err)
		return nil, unavailable("dial", err)
	}
	bctx, cancel := c.bounded(ctx)
	err = timedOut(bctx, conn.Bind(bctx, c.rec.BindName, pw))
	cancel()
	if err != nil {
		_ = conn.Close()
		c.log.Debug("identity.directory: bind failed", "name", c.rec.BindName, "error", err)
		return nil, unavailable("bind", err)
	}
	return conn, nil
}

// connect returns the client's connection, opening it when there is none,
// with the domain SID read for an ad record before it is used, and whether
// it was opened by this call.
func (c *Client) connect(ctx context.Context) (Conn, bool, error) {
	if err := ctx.Err(); err != nil {
		return nil, false, unavailable("connect", err)
	}
	select {
	case c.connecting <- struct{}{}:
	case <-ctx.Done():
		return nil, false, unavailable("connect", ctx.Err())
	}
	type opened struct {
		conn  Conn
		fresh bool
	}
	o, err := detach(ctx, "connect", func(ctx context.Context) (opened, error) {
		defer func() { <-c.connecting }()
		conn, fresh, err := c.establish(ctx)
		return opened{conn, fresh}, err
	})
	return o.conn, o.fresh, err
}

// establish is connect's work, run while c.connecting is held.
func (c *Client) establish(ctx context.Context) (Conn, bool, error) {
	c.mu.Lock()
	conn, sid := c.conn, c.sid
	c.mu.Unlock()
	fresh := conn == nil
	if fresh {
		var err error
		if conn, err = c.open(ctx); err != nil {
			return nil, false, err
		}
		c.mu.Lock()
		c.conn = conn
		c.mu.Unlock()
	}
	if sid == nil && c.rec.IsAD() {
		sid, err := c.readDomainSID(ctx, conn)
		if err != nil {
			return nil, false, err
		}
		c.mu.Lock()
		c.sid = sid
		c.mu.Unlock()
	}
	return conn, fresh, nil
}

// readDomainSID reads the objectSid of the entry the root DSE's
// defaultNamingContext names.
func (c *Client) readDomainSID(ctx context.Context, conn Conn) ([]byte, error) {
	res, err := c.search(ctx, conn, Request{Scope: ScopeBase, Filter: "(objectClass=*)", Attrs: []string{"defaultNamingContext"}})
	if err != nil {
		return nil, err
	}
	var nc []byte
	if len(res.Entries) == 1 {
		nc = first(res.Entries[0], "defaultNamingContext")
	}
	if len(nc) == 0 {
		return nil, unavailable("domain SID", errors.New("the root DSE names no defaultNamingContext"))
	}
	res, err = c.search(ctx, conn, Request{Base: string(nc), Scope: ScopeBase, Filter: "(objectSid=*)", Attrs: []string{"objectSid"}})
	if err != nil {
		return nil, err
	}
	var sid []byte
	if len(res.Entries) == 1 {
		sid = first(res.Entries[0], "objectSid")
	}
	if len(sid) == 0 {
		return nil, unavailable("domain SID", fmt.Errorf("the domain object %q holds no objectSid", nc))
	}
	return sid, nil
}

// search sends req on conn with the domain-scope control, under the query
// timeout alone: a caller whose context ends stops waiting, and the search
// runs on. A failed one drops conn, so the next question dials again.
//
// ponytail: any failed call drops the connection, a refusal the directory
// answered (no such object, a referral) as much as a broken transport, so such
// a question costs the next one a dial. Tell the two apart when the dial rate
// shows it.
func (c *Client) search(ctx context.Context, conn Conn, req Request) (Result, error) {
	return detach(ctx, "search", func(ctx context.Context) (Result, error) { return c.searchOn(ctx, conn, req) })
}

// searchOn is search's work, on a context no caller cancels.
func (c *Client) searchOn(ctx context.Context, conn Conn, req Request) (Result, error) {
	req.DomainScope = true
	sctx, cancel := c.bounded(ctx)
	defer cancel()
	res, err := conn.Search(sctx, req)
	if err != nil {
		c.drop(conn)
		return Result{}, unavailable("search", timedOut(sctx, err))
	}
	return res, nil
}

// drop closes conn, so the next question dials again.
func (c *Client) drop(conn Conn) {
	c.mu.Lock()
	if c.conn == conn {
		c.conn = nil
	}
	c.mu.Unlock()
	_ = conn.Close()
}

// do sends req on the client's connection, opening it when there is none,
// and returns the connection that answered. A held connection the directory
// closed while idle fails the first query on it, so that one is sent again,
// once, on a new one; a fresh connection's failure is the directory's
// answer, not retried.
func (c *Client) do(ctx context.Context, req Request) (Conn, Result, error) {
	conn, fresh, err := c.connect(ctx)
	if err != nil {
		return nil, Result{}, err
	}
	res, err := c.search(ctx, conn, req)
	if err != nil && !fresh && errors.Is(err, ErrClosed) {
		if conn, _, err = c.connect(ctx); err != nil {
			return nil, Result{}, err
		}
		res, err = c.search(ctx, conn, req)
	}
	return conn, res, err
}

// all sends req as an RFC 2696 paged search and follows its pages to the
// end; a page that fails, or more than maxPages, fails the whole search. A
// later page that finds its connection closed, by the directory or by
// another question's failed call, restarts the search once from its first
// page on a new connection.
func (c *Client) all(ctx context.Context, req Request) ([]Entry, error) {
	req.PageSize = pageSize
	out, later, err := c.pages(ctx, req)
	if later && errors.Is(err, ErrClosed) {
		out, _, err = c.pages(ctx, req)
	}
	return out, err
}

// pages is one run of all: the first page through do, every later one on the
// connection that answered it, since a directory holds a paged search's
// cookie per connection and refuses it on another. later reports that a
// later page failed.
func (c *Client) pages(ctx context.Context, req Request) (out []Entry, later bool, err error) {
	conn, res, err := c.do(ctx, req)
	if err != nil {
		return nil, false, err
	}
	out = res.Entries
	for n := 1; len(res.Cookie) > 0; n++ {
		if n == maxPages {
			// The search is left open on the directory, which counts it
			// against the connection's open result sets; closing the
			// connection ends it.
			c.drop(conn)
			return nil, false, unavailable("paged search", fmt.Errorf("more than %d pages", maxPages))
		}
		req.Cookie = res.Cookie
		if res, err = c.search(ctx, conn, req); err != nil {
			return nil, true, err
		}
		out = append(out, res.Entries...)
	}
	return out, false, nil
}

// answer reads an entry into an Answer.
func (c *Client) answer(e Entry) Answer {
	a := Answer{DN: e.DN, Enabled: true, Mail: string(first(e, "mail")), SID: first(e, "objectSid")}
	for _, oc := range values(e, "objectClass") {
		if strings.EqualFold(string(oc), "group") || strings.EqualFold(string(oc), "posixGroup") {
			a.Group = true
		}
	}
	if a.Group {
		a.Name = string(first(e, c.rec.GroupAttr()))
	} else {
		a.Name = string(first(e, c.rec.UserAttr()))
		// decision: userAccountControl and accountExpires are the whole
		// enabled rule, so an rfc2307 user, which holds neither, answers
		// enabled. Reading shadowExpire, or another POSIX account state, is
		// what would overturn this.
		a.Enabled = enabled(e, c.clock.Now())
	}
	a.EntryID = first(e, c.rec.entryIDAttr())
	a.UID, a.GID = number(first(e, "uidNumber")), number(first(e, "gidNumber"))
	return a
}

// accountDisable is userAccountControl's disable bit (MS-ADTS 2.2.16).
const accountDisable = 0x2

// enabled reports whether e's userAccountControl lacks the disable bit and
// its accountExpires, if any, has not passed at now. A value that does not
// parse reads as disabled.
func enabled(e Entry, now time.Time) bool {
	if v := first(e, "userAccountControl"); v != nil {
		uac, err := strconv.ParseInt(string(v), 10, 64)
		if err != nil || uac&accountDisable != 0 {
			return false
		}
	}
	if v := first(e, "accountExpires"); v != nil {
		exp, err := strconv.ParseInt(string(v), 10, 64)
		if err != nil {
			return false
		}
		// 0 and the largest value both mean never (MS-ADA1 accountExpires).
		if exp != 0 && exp != math.MaxInt64 && !now.Before(fileTime(exp)) {
			return false
		}
	}
	return true
}

// fileTime is a Windows FILETIME: 100-nanosecond intervals since 1601-01-01 UTC.
func fileTime(ft int64) time.Time {
	const toUnix = 116444736000000000 // 1601 to 1970, in 100 ns
	ft -= toUnix
	return time.Unix(ft/1e7, ft%1e7*100).UTC()
}

// number is v as a uint32, nil when absent or not one.
func number(v []byte) *uint32 {
	n, err := strconv.ParseUint(string(v), 10, 32)
	if err != nil {
		return nil
	}
	u := uint32(n)
	return &u
}

// values is e's values of attr, its name compared case-insensitively.
func values(e Entry, attr string) [][]byte {
	for k, v := range e.Attrs {
		if strings.EqualFold(k, attr) {
			return v
		}
	}
	return nil
}

// first is e's first value of attr, nil when it has none.
func first(e Entry, attr string) []byte {
	if v := values(e, attr); len(v) > 0 && len(v[0]) > 0 {
		return bytes.Clone(v[0])
	}
	return nil
}
