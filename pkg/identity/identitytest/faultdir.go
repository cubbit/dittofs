package identitytest

import (
	"context"
	"errors"
	"slices"
	"sync"

	"github.com/cubbit/dittofs/internal/identity/directory"
	"github.com/cubbit/dittofs/pkg/sim"
)

// ErrHold, as a fault's Err, holds the dial or call until its context ends, and
// then fails it with the context's error: a directory that never answers.
var ErrHold = errors.New("identitytest: held until the context ends")

// FaultyDirectory wraps a directory.Dialer and every Conn it returns. Each
// dial, bind and search is first sent to what it wraps, then handed to the
// fault as the op "dial" keyed by the record's address, "bind" keyed by the
// bind name, or "search" keyed by the filter. The fault's delay is waited out
// on its own clock; its Err fails the call, ErrHold holding it until its context
// ends; a search's tearAfter n renames, in the answer's first n entries, the
// attribute TearAttr names. So the directory has really answered, and a test
// whose code ignored the answer still fails.
type FaultyDirectory struct {
	next  directory.Dialer
	fault sim.Fault

	mu       sync.Mutex
	queries  []directory.Request
	tearFrom string
	tearTo   string
}

// NewFaultyDirectory wraps next with fault.
func NewFaultyDirectory(next directory.Dialer, fault sim.Fault) *FaultyDirectory {
	return &FaultyDirectory{next: next, fault: fault}
}

// TearAttr sets the attribute a torn search answer renames, from, and the
// name it is given, to, such as a member;range=1500-1499 its range parser
// must refuse.
func (f *FaultyDirectory) TearAttr(from, to string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tearFrom, f.tearTo = from, to
}

// Queries returns every search request sent to the wrapped directory, in the
// order sent.
func (f *FaultyDirectory) Queries() []directory.Request {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.queries)
}

// Dial is a directory.Dialer.
func (f *FaultyDirectory) Dial(ctx context.Context, rec directory.Record) (directory.Conn, error) {
	c, err := f.next(ctx, rec)
	if ferr := f.at(ctx, "dial", rec.Address, nil); ferr != nil {
		if c != nil {
			_ = c.Close()
		}
		return nil, ferr
	}
	if err != nil {
		return nil, err
	}
	return &faultyConn{f: f, next: c}, nil
}

var _ directory.Dialer = (*FaultyDirectory)(nil).Dial

// at consults the fault for op on key and returns the error it decides, or,
// for ErrHold, ctx's once it ends. A tear is returned through tear for a search;
// a tear of any other op, or a loss, is a mistake in the test, and panics.
func (f *FaultyDirectory) at(ctx context.Context, op, key string, tear *int) error {
	_, tearAfter, err := f.fault.At(op, []byte(key))
	if tearAfter != 0 {
		if tear == nil || tearAfter < 0 {
			panic("identitytest: a tear or loss of the op " + op + ": only a search takes a tear, and nothing a loss")
		}
		*tear = tearAfter
	}
	if errors.Is(err, ErrHold) {
		<-ctx.Done()
		return ctx.Err()
	}
	return err
}

// faultyConn is one Conn of a FaultyDirectory.
type faultyConn struct {
	f    *FaultyDirectory
	next directory.Conn
}

func (c *faultyConn) Bind(ctx context.Context, name string, password []byte) error {
	err := c.next.Bind(ctx, name, password)
	if ferr := c.f.at(ctx, "bind", name, nil); ferr != nil {
		return ferr
	}
	return err
}

func (c *faultyConn) Search(ctx context.Context, req directory.Request) (directory.Result, error) {
	c.f.mu.Lock()
	c.f.queries = append(c.f.queries, req)
	c.f.mu.Unlock()

	res, err := c.next.Search(ctx, req)
	tear := 0
	if ferr := c.f.at(ctx, "search", req.Filter, &tear); ferr != nil {
		return directory.Result{}, ferr
	}
	if tear > 0 {
		c.f.mu.Lock()
		from, to := c.f.tearFrom, c.f.tearTo
		c.f.mu.Unlock()
		for i := range min(tear, len(res.Entries)) {
			if v, ok := res.Entries[i].Attrs[from]; ok {
				delete(res.Entries[i].Attrs, from)
				res.Entries[i].Attrs[to] = v
			}
		}
	}
	return res, err
}

func (c *faultyConn) Close() error { return c.next.Close() }
