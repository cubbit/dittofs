package core

import (
	"context"
	"errors"
	"fmt"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

// Call is one step of the middleware chain: the handler, or what wraps it.
type Call func(ctx context.Context, r *Request) error

// Middleware wraps a Call: recovery, trace span, metrics, deadline, in that
// fixed order.
type Middleware func(next Call) Call

// chain is op's middleware around the request's own handler, built once per
// row in their fixed order (§4.7): recovery outermost, so it sees a panic
// only after the span and the clock have recorded it; then the trace span,
// the metrics hinge, and the deadline, which the handler's context derives
// from.
func (sh *shared) chain(op *Op, ts *tableSeries) Call {
	order := []Middleware{sh.recovery(op, ts), sh.trace(op, ts), sh.metrics(ts), sh.deadline}
	call := Call(invoke)
	for i := len(order) - 1; i >= 0; i-- {
		call = order[i](call)
	}
	return call
}

// invoke is where every chain ends: the handler the pipeline gave the
// request.
func invoke(ctx context.Context, r *Request) error { return r.invoke(ctx, r) }

// trace opens a compound member's span, a child of the request's, named
// adapter.<protocol>.<operation> with §10.2's attributes, and ends it in a
// deferred call that records the fault if the call panicked. A call outside
// a compound runs under the request's span, and with no tracer there is
// none (§10.2). A request not sampled opens none either, so it costs nothing
// (RFC 25 §6.3).
func (sh *shared) trace(op *Op, ts *tableSeries) Middleware {
	if sh.tracer == nil || !ts.member {
		return func(next Call) Call { return next }
	}
	name, label := "adapter."+ts.protocol+"."+op.Name, strings.ToLower(op.Name)
	return func(next Call) Call {
		return func(ctx context.Context, r *Request) (err error) {
			if !trace.SpanContextFromContext(ctx).IsSampled() {
				return next(ctx, r)
			}
			ctx, span := sh.tracer.Start(ctx, name)
			returned := false
			defer func() {
				switch {
				case !returned:
					span.RecordError(errPanicked)
				case span.IsRecording():
					if i := serviceError(err); err == nil || i >= 0 {
						result := "ok"
						if err != nil {
							result = serviceErrors[i].name
						}
						span.SetAttributes(attribute.String("result", result))
					}
				}
				span.End()
			}()
			if span.IsRecording() {
				span.SetAttributes(attribute.String("protocol", ts.protocol), attribute.Int64("version", int64(r.Version)), attribute.String("op", label))
			}
			err = next(ctx, r)
			returned = true
			return err
		}
	}
}

// metrics is the stage clock's hinge (§10.1): as the call starts it
// observes resolve_ids, from the start the request keeps; as the call
// returns or panics it marks the call's end, from which the pipeline times
// map_error. The call itself is the service's to time.
func (sh *shared) metrics(ts *tableSeries) Middleware {
	return func(next Call) Call {
		return func(ctx context.Context, r *Request) error {
			ts.observe(stageResolveIDs, sh.clock.Now().Sub(r.resolveStart))
			defer func() { r.callEnd = sh.clock.Now() }()
			return next(ctx, r)
		}
	}
}

// attemptDeadline is how long an attempt of a service call may run, from the
// attempt's start (§6.4).
const attemptDeadline = 30 * time.Second

// clock is the time the pipeline reads and arms its timers on.
//
// decision: one clock per server, the runtime's in a server and a test's own
// through hooks_test.go, so a test at the seam moves time instead of waiting
// it out; the attempt's deadline is a context on it, since one on the
// runtime's time cannot be moved. Overturned if the seam tests run in a
// testing/synctest bubble whole, where the runtime's time is the test's.
type clock interface {
	Now() time.Time
	AfterFunc(d time.Duration, f func()) timer
}

// timer is f armed on a clock; Stop reports whether it kept f from running.
type timer interface{ Stop() bool }

type runtimeClock struct{}

func (runtimeClock) Now() time.Time                            { return time.Now() }
func (runtimeClock) AfterFunc(d time.Duration, f func()) timer { return time.AfterFunc(d, f) }

// The pipeline keeps an attempt's start and the hold's deadline in the
// context under these keys, before the attempt's reply reservation (§6.4).
type (
	attemptStartKey struct{}
	holdDeadlineKey struct{}
)

// deadline gives the call a deadline on the server's clock: 30 s from the
// attempt's start the context carries, or the hold's deadline it carries
// where that is sooner; with no start, from the clock's now.
func (sh *shared) deadline(next Call) Call {
	return func(ctx context.Context, r *Request) error {
		start, ok := ctx.Value(attemptStartKey{}).(time.Time)
		if !ok {
			start = sh.clock.Now()
		}
		at := start.Add(attemptDeadline)
		if hold, ok := ctx.Value(holdDeadlineKey{}).(time.Time); ok && hold.Before(at) {
			at = hold
		}
		ctx, cancel := withClockDeadline(ctx, sh.clock, at)
		defer cancel()
		return next(ctx, r)
	}
}

// errPanicked is recovery's answer for a handler that panicked: the pipeline
// answers it the table's fault status without Map, so a panic is not also
// counted as an unmapped error.
var errPanicked = errors.New("core: handler panicked")

// recovery turns op's panic into errPanicked, counted under op in ts and
// logged at error with the operation, the panic value's type and the stack,
// at most once a minute per operation with the count suppressed since the
// last line. The value is the handler's own text and may hold a name a user
// chose, which RFC 25 §5.4 keeps below Info, so it is logged at debug.
//
// decision: an operation's panic writes its error line once a minute, as
// RFC 25 §5.2 writes a noisy subject, where RFC 20 §4.2 asks a line per
// panic: a client that can make a handler panic would otherwise make it
// write a stack per request. The panic counter counts every one. Overturned
// if an incident needs every panic's stack.
func (sh *shared) recovery(op *Op, ts *tableSeries) Middleware {
	line := ts.panicLines[op.Code]
	return func(next Call) Call {
		return func(ctx context.Context, r *Request) (err error) {
			defer func() {
				if p := recover(); p != nil {
					if ok, suppressed := line.take(sh.clock.Now()); ok {
						sh.log.ErrorContext(ctx, "handler panicked", "op", op.Name, "panic_type", fmt.Sprintf("%T", p), "suppressed", suppressed, "stack", string(debug.Stack()))
					}
					sh.log.DebugContext(ctx, "handler panicked", "op", op.Name, "panic", p)
					ts.panicked(op)
					err = errPanicked
				}
			}()
			return next(ctx, r)
		}
	}
}

// panicLineEvery is how often an operation's panic writes its error line
// (RFC 25 §5.2).
const panicLineEvery = time.Minute

// lineLimiter decides when a noisy subject's line is written: the first,
// then the first a minute or more after the last, with the count of those
// in between.
type lineLimiter struct {
	mu         sync.Mutex
	last       time.Time
	suppressed int64
}

// take reports whether a line at now is written, and how many were
// suppressed since the last.
func (l *lineLimiter) take(now time.Time) (bool, int64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.last.IsZero() && now.Sub(l.last) < panicLineEvery {
		l.suppressed++
		return false, 0
	}
	n := l.suppressed
	l.last, l.suppressed = now, 0
	return true, n
}

// panicLine is the limiter of op's panic line under protocol p, one per
// operation across a protocol's tables and a compound row's members. The
// caller holds sh.mu.
func (sh *shared) panicLine(p, op string) *lineLimiter {
	k := [2]string{p, op}
	if l, ok := sh.panics[k]; ok {
		return l
	}
	l := &lineLimiter{}
	sh.panics[k] = l
	return l
}

// clockCtx is a context whose deadline is on a clock: its Deadline, Done and
// Err follow the clock, or its parent where that ends first. It implements
// AfterFunc, which the standard library looks for on a parent that is not
// its own, so a context derived from it starts no goroutine to watch it.
type clockCtx struct {
	context.Context
	at   time.Time
	done chan struct{}

	mu      sync.Mutex
	err     error
	timer   timer       // the clock's, at at
	unwatch func() bool // stops the parent's watch
	funcs   map[int]func()
	next    int
}

// withClockDeadline is parent with a deadline at at on clk; cancel ends it
// as Canceled and stops its timer. A parent already done, or a deadline
// already spent, ends it before it is returned, not later from a watch's or
// a timer's goroutine, so the call sees either from its start (§6.4, §6.5).
func withClockDeadline(parent context.Context, clk clock, at time.Time) (ctx context.Context, cancel func()) {
	if d, ok := parent.Deadline(); ok && d.Before(at) {
		at = d
	}
	c := &clockCtx{Context: parent, at: at, done: make(chan struct{})}
	cancel = func() { c.end(context.Canceled) }
	if err := parent.Err(); err != nil {
		c.end(err)
		return c, cancel
	}
	now := clk.Now()
	if !at.After(now) {
		c.end(context.DeadlineExceeded)
		return c, cancel
	}
	unwatch := context.AfterFunc(parent, func() { c.end(parent.Err()) })
	t := clk.AfterFunc(at.Sub(now), func() { c.end(context.DeadlineExceeded) })
	c.mu.Lock()
	c.timer, c.unwatch = t, unwatch
	ended := c.err != nil
	c.mu.Unlock()
	if ended {
		t.Stop()
		unwatch()
	}
	return c, cancel
}

func (c *clockCtx) Deadline() (time.Time, bool) { return c.at, true }
func (c *clockCtx) Done() <-chan struct{}       { return c.done }

func (c *clockCtx) Err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.err
}

// AfterFunc runs f in its own goroutine once c is done, as context.AfterFunc
// does; stop reports whether it kept f from running.
func (c *clockCtx) AfterFunc(f func()) (stop func() bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.err != nil {
		go f()
		return func() bool { return false }
	}
	if c.funcs == nil {
		c.funcs = map[int]func(){}
	}
	id := c.next
	c.next++
	c.funcs[id] = f
	return func() bool {
		c.mu.Lock()
		defer c.mu.Unlock()
		_, ok := c.funcs[id]
		delete(c.funcs, id)
		return ok
	}
}

// end makes c done with err, once.
func (c *clockCtx) end(err error) {
	c.mu.Lock()
	if c.err != nil {
		c.mu.Unlock()
		return
	}
	c.err = err
	close(c.done)
	t, unwatch, funcs := c.timer, c.unwatch, c.funcs
	c.funcs = nil
	c.mu.Unlock()
	if t != nil {
		t.Stop()
		unwatch()
	}
	for _, f := range funcs {
		go f()
	}
}
