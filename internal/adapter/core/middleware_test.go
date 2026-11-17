package core_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	"github.com/cubbit/dittofs/internal/adapter/adaptertest/testproto"
	"github.com/cubbit/dittofs/internal/adapter/core"
	"github.com/cubbit/dittofs/internal/prof"
	"github.com/cubbit/dittofs/pkg/adapter"
	"github.com/cubbit/dittofs/pkg/metrics"
	"github.com/cubbit/dittofs/pkg/vfs"
)

var t0 = time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)

// manualClock is a clock the test moves: a timer fires when Advance reaches
// it, one armed already due too, never inline as it is armed, since a
// runtime timer fires on a goroutine of its own.
type manualClock struct {
	mu     sync.Mutex
	now    time.Time
	step   time.Duration // how far each read moves now on, firing no timer
	timers map[int]manualTimer
	next   int
}

type manualTimer struct {
	at time.Time
	f  func()
}

func newManualClock() *manualClock { return &manualClock{now: t0, timers: map[int]manualTimer{}} }

func (c *manualClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now
	c.now = c.now.Add(c.step)
	return now
}

func (c *manualClock) AfterFunc(d time.Duration, f func()) core.Timer {
	c.mu.Lock()
	defer c.mu.Unlock()
	id := c.next
	c.next++
	c.timers[id] = manualTimer{c.now.Add(d), f}
	return stopFunc(func() bool {
		c.mu.Lock()
		defer c.mu.Unlock()
		_, ok := c.timers[id]
		delete(c.timers, id)
		return ok
	})
}

// stopFunc is a timer whose Stop is the func.
type stopFunc func() bool

func (f stopFunc) Stop() bool { return f() }

func (c *manualClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	var due []func()
	for id, tm := range c.timers {
		if !tm.at.After(c.now) {
			due = append(due, tm.f)
			delete(c.timers, id)
		}
	}
	c.mu.Unlock()
	for _, f := range due {
		f()
	}
}

func (c *manualClock) pending() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.timers)
}

// The call's deadline is on the server's clock: 30 s from the attempt's
// start the context carries, or the hold's deadline where that is sooner,
// or 30 s from the clock's now with no start. The context is done when the
// clock reaches it, not before.
func TestDeadlineCountsFromTheAttemptStart(t *testing.T) {
	for _, c := range []struct {
		name string
		ctx  func(context.Context) context.Context
		want time.Duration // after t0
	}{
		{"a start 10 s before the call", func(ctx context.Context) context.Context { return core.WithAttemptStart(ctx, t0.Add(-10*time.Second)) }, 20 * time.Second},
		{"no start", func(ctx context.Context) context.Context { return ctx }, 30 * time.Second},
		{"a hold's deadline sooner", func(ctx context.Context) context.Context {
			return core.WithHoldDeadline(core.WithAttemptStart(ctx, t0.Add(-10*time.Second)), t0.Add(5*time.Second))
		}, 5 * time.Second},
		{"a hold's deadline later", func(ctx context.Context) context.Context {
			return core.WithHoldDeadline(core.WithAttemptStart(ctx, t0.Add(-10*time.Second)), t0.Add(40*time.Second))
		}, 20 * time.Second},
		{"a parent's deadline sooner", func(ctx context.Context) context.Context {
			return deadlineOnly{core.WithAttemptStart(ctx, t0.Add(-10*time.Second)), t0.Add(5 * time.Second)}
		}, 5 * time.Second},
	} {
		t.Run(c.name, func(t *testing.T) {
			clk := newManualClock()
			sh := core.NewShared(adapter.Deps{})
			core.SetClock(sh, clk)
			call := sh.Deadline(func(ctx context.Context, _ *core.Request) error {
				if at, ok := ctx.Deadline(); !ok || !at.Equal(t0.Add(c.want)) {
					t.Errorf("the call's deadline is %v (%v), want %v after t0", at, ok, c.want)
				}
				clk.Advance(c.want - time.Second)
				if ctx.Err() != nil {
					t.Errorf("a second before its deadline the context is done: %v", ctx.Err())
				}
				clk.Advance(time.Second)
				select {
				case <-ctx.Done():
				default:
					t.Fatal("the context is not done when the clock reaches its deadline")
				}
				if !errors.Is(ctx.Err(), context.DeadlineExceeded) {
					t.Errorf("at its deadline Err = %v, want DeadlineExceeded", ctx.Err())
				}
				return nil
			})
			if err := call(c.ctx(context.Background()), nil); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// The call's context ends with the call: Canceled, its timer stopped; and a
// parent's cancel reaches it.
func TestDeadlineEndsWithTheCall(t *testing.T) {
	clk := newManualClock()
	sh := core.NewShared(adapter.Deps{})
	core.SetClock(sh, clk)
	var seen context.Context
	if err := sh.Deadline(func(ctx context.Context, _ *core.Request) error { seen = ctx; return nil })(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	if !errors.Is(seen.Err(), context.Canceled) || clk.pending() != 0 {
		t.Errorf("after the call, its context's Err is %v and %d timers are armed; want Canceled and none", seen.Err(), clk.pending())
	}
	synctest.Test(t, func(t *testing.T) {
		parent, cancel := context.WithCancel(context.Background())
		_ = sh.Deadline(func(ctx context.Context, _ *core.Request) error {
			cancel()
			synctest.Wait()
			if !errors.Is(ctx.Err(), context.Canceled) {
				t.Errorf("after its parent's cancel Err = %v, want Canceled", ctx.Err())
			}
			return nil
		})(parent, nil)
	})
}

// A deadline already spent, or a parent already done, shows on the call's
// context as the call starts, not later from a timer's or a watch's
// goroutine (§6.4, §6.5); a parent's end shows with its own error.
func TestDeadlineSpentOrCancelledShowsAtOnce(t *testing.T) {
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	expired, cancelExpired := context.WithDeadline(context.Background(), time.Unix(0, 0))
	defer cancelExpired()
	for _, c := range []struct {
		name   string
		parent context.Context
		want   error
	}{
		{"an attempt started 30 s ago", core.WithAttemptStart(context.Background(), t0.Add(-30*time.Second)), context.DeadlineExceeded},
		{"a parent cancelled", cancelled, context.Canceled},
		{"a parent past its own deadline", expired, context.DeadlineExceeded},
	} {
		t.Run(c.name, func(t *testing.T) {
			sh := core.NewShared(adapter.Deps{})
			core.SetClock(sh, newManualClock())
			_ = sh.Deadline(func(ctx context.Context, _ *core.Request) error {
				select {
				case <-ctx.Done():
				default:
					t.Error("as the call starts its context is not done")
				}
				if err := ctx.Err(); !errors.Is(err, c.want) {
					t.Errorf("as the call starts its context's Err is %v, want %v", err, c.want)
				}
				return nil
			})(c.parent, nil)
		})
	}
}

// A context derived from the call's starts no goroutine to watch it, and
// ends when the clock reaches the call's deadline.
func TestDeadlineContextStartsNoGoroutine(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		clk := newManualClock()
		sh := core.NewShared(adapter.Deps{})
		core.SetClock(sh, clk)
		_ = sh.Deadline(func(ctx context.Context, _ *core.Request) error {
			synctest.Wait()
			before := runtime.NumGoroutine()
			child, cancel := context.WithCancel(ctx)
			defer cancel()
			synctest.Wait()
			if after := runtime.NumGoroutine(); after != before {
				t.Errorf("deriving a context started %d goroutines, want none", after-before)
			}
			clk.Advance(30 * time.Second)
			synctest.Wait()
			if !errors.Is(child.Err(), context.DeadlineExceeded) {
				t.Errorf("the derived context's Err at the deadline is %v, want DeadlineExceeded", child.Err())
			}
			return nil
		})(context.Background(), nil)
	})
}

// A panicking handler is answered ErrPanicked, logged once at error with its
// operation, the panic value's type and its stack, and counted under its
// operation alone. The value, which may hold a name a user chose, is logged
// at debug only (RFC 25 §5.4).
func TestRecoveryAnswersAPanic(t *testing.T) {
	var logged bytes.Buffer
	m := metrics.New("v", "c")
	sh := core.NewShared(adapter.Deps{Metrics: m, Logger: slog.New(slog.NewJSONHandler(&logged, &slog.HandlerOptions{Level: slog.LevelDebug}))})
	tb := routed(t)[0]
	ts, err := sh.Table(tb)
	if err != nil {
		t.Fatal(err)
	}
	echo := tb.Ops[testproto.OpEcho]
	const value = "open /exports/plans-2027.ods"
	err = sh.Recovery(echo, ts)(func(context.Context, *core.Request) error { panic(value) })(context.Background(), nil)
	if !errors.Is(err, core.ErrPanicked) {
		t.Errorf("a panic is answered %v, want ErrPanicked", err)
	}
	lines := strings.Split(strings.TrimSpace(logged.String()), "\n")
	var rec, debug struct {
		Level, Op, Stack, Panic string
		PanicType               string `json:"panic_type"`
	}
	if len(lines) != 2 || json.Unmarshal([]byte(lines[0]), &rec) != nil || json.Unmarshal([]byte(lines[1]), &debug) != nil {
		t.Fatalf("logged %q, want two records", logged.String())
	}
	if rec.Level != "ERROR" || rec.Op != echo.Name || rec.PanicType != "string" || !strings.Contains(rec.Stack, "TestRecoveryAnswersAPanic") {
		t.Errorf("logged %+v, want an error naming %s and the value's type, with the panicking frame in its stack", rec, echo.Name)
	}
	if strings.Contains(lines[0], "plans-2027") {
		t.Errorf("the error line carries the panic value: %s", lines[0])
	}
	if debug.Level != "DEBUG" || debug.Op != echo.Name || debug.Panic != value {
		t.Errorf("logged %+v at debug, want the panic value", debug)
	}
	got, _ := scrape(t, m)
	for _, op := range tb.Ops {
		want := 0.0
		if op == echo {
			want = 1
		}
		if k := "adapter_handler_panics_total{op=" + strings.ToLower(op.Name) + ",protocol=" + testproto.Name + "}"; got[k] != want {
			t.Errorf("%s = %v, want %v", k, got[k], want)
		}
	}
}

// A handler that returns is answered its own error, and nothing is logged or
// counted.
func TestRecoveryPassesAnErrorThrough(t *testing.T) {
	var logged bytes.Buffer
	sh := core.NewShared(adapter.Deps{Logger: slog.New(slog.NewJSONHandler(&logged, nil))})
	tb := routed(t)[0]
	ts, err := sh.Table(tb)
	if err != nil {
		t.Fatal(err)
	}
	if err := sh.Recovery(tb.Ops[testproto.OpEcho], ts)(func(context.Context, *core.Request) error { return errHandler })(context.Background(), nil); err != errHandler {
		t.Errorf("the handler's error came back as %v", err)
	}
	if logged.Len() != 0 {
		t.Errorf("a handler that returned logged %q", logged.String())
	}
}

// An operation's panic writes its error line at most once a minute, across
// the protocol's tables and a compound row's members, the next line carrying
// the count suppressed since the last, while the panic counter counts every
// panic (RFC 25 §5.2). Another operation's panic writes its own line, and so
// does the same operation's under another protocol.
func TestAPanicLineIsWrittenOnceAMinutePerOperation(t *testing.T) {
	tr := newTraced(t, false)
	v2 := routed(t)[1]
	v2s, err := tr.sh.Table(v2)
	if err != nil {
		t.Fatal(err)
	}
	alt := *routed(t)[1]
	alt.Protocol = "alt"
	alts, err := tr.sh.Table(&alt)
	if err != nil {
		t.Fatal(err)
	}
	v2Echo, other := v2.Ops[testproto.OpEcho], v2.Ops[testproto.OpPutFH]
	if v2Echo == nil || other == nil {
		t.Fatal("version 2 has no Echo or no PutFH")
	}
	panics := func(ts *core.TableSeries, op *core.Op) {
		t.Helper()
		r := request(func(context.Context, *core.Request) error { panic("boom") })
		if err := ts.Chain(op)(context.Background(), r); !errors.Is(err, core.ErrPanicked) {
			t.Fatalf("a panic is answered %v, want ErrPanicked", err)
		}
	}
	type line struct {
		Msg, Op    string
		Suppressed int64
	}
	lines := func() []line {
		t.Helper()
		var ls []line
		for _, s := range strings.Split(strings.TrimSpace(tr.logged.String()), "\n") {
			var l line
			if err := json.Unmarshal([]byte(s), &l); err != nil {
				t.Fatalf("logged %q: %v", s, err)
			}
			ls = append(ls, l)
		}
		tr.logged.Reset()
		return ls
	}
	counted := func() float64 {
		t.Helper()
		got, _ := scrape(t, tr.m)
		return got["adapter_handler_panics_total{op="+strings.ToLower(tr.topEcho.Name)+",protocol="+testproto.Name+"}"]
	}
	const msg = "handler panicked"
	echo := tr.topEcho.Name

	panics(tr.top, tr.topEcho)
	tr.clk.Advance(59 * time.Second)
	panics(tr.members, tr.mEcho)
	panics(v2s, v2Echo)
	panics(v2s, other)
	panics(alts, alt.Ops[testproto.OpEcho])
	if got, want := lines(), []line{{msg, echo, 0}, {msg, other.Name, 0}, {msg, echo, 0}}; !slices.Equal(got, want) {
		t.Errorf("three %s panics, one %s and one %s under another protocol within the minute wrote %+v, want %+v", echo, other.Name, echo, got, want)
	}
	if n := counted(); n != 3 {
		t.Errorf("three %s panics are counted %v, want 3", echo, n)
	}

	tr.clk.Advance(time.Second)
	panics(tr.top, tr.topEcho)
	if got, want := lines(), []line{{msg, echo, 2}}; !slices.Equal(got, want) {
		t.Errorf("a %s panic a minute after its line wrote %+v, want %+v", echo, got, want)
	}
	if n := counted(); n != 4 {
		t.Errorf("four %s panics are counted %v, want 4", echo, n)
	}

	tr.clk.Advance(30 * time.Second)
	panics(tr.members, tr.mEcho)
	tr.clk.Advance(30 * time.Second)
	panics(tr.top, tr.topEcho)
	if got, want := lines(), []line{{msg, echo, 1}}; !slices.Equal(got, want) {
		t.Errorf("the next minute's %s line wrote %+v, want %+v, the one since the last line", echo, got, want)
	}
}

var errHandler = errors.New("the handler's own error")

// traced is a server on a recording tracer, a test clock and a registry of
// its own, with the series of testproto's version 1 table and of its
// compound members.
type traced struct {
	sh             *core.Shared
	spans          *tracetest.SpanRecorder
	tracer         trace.Tracer
	clk            *manualClock
	m              *metrics.Registry
	logged         *bytes.Buffer
	top, members   *core.TableSeries
	topEcho, mEcho *core.Op
}

func newTraced(t *testing.T, tracing bool) traced {
	t.Helper()
	tr := traced{spans: tracetest.NewSpanRecorder(), clk: newManualClock(), m: metrics.New("v", "c"), logged: &bytes.Buffer{}}
	tr.tracer = sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(tr.spans)).Tracer("test")
	d := adapter.Deps{Metrics: tr.m, Logger: slog.New(slog.NewJSONHandler(tr.logged, nil))}
	if tracing {
		d.Tracer = tr.tracer
	}
	tr.sh = core.NewShared(d)
	core.SetClock(tr.sh, tr.clk)
	tb := routed(t)[0]
	members := tb.Ops[testproto.OpCompound].Members
	var err error
	if tr.top, err = tr.sh.Table(tb); err != nil {
		t.Fatal(err)
	}
	if tr.members, err = tr.sh.Table(members); err != nil {
		t.Fatal(err)
	}
	tr.topEcho, tr.mEcho = tb.Ops[testproto.OpEcho], members.Ops[testproto.OpEcho]
	return tr
}

// request is a request routed at version 1 whose resolve_ids began 2 ms
// before t0, its handler h.
func request(h core.Call) *core.Request {
	r := &core.Request{Version: 1}
	core.SetResolveStart(r, t0.Add(-2*time.Millisecond))
	core.SetInvoke(r, h)
	return r
}

// §10.2's span attribute keys, as §10.1's labels write them.
var spanKeys = []string{"protocol", "version", "op", "status", "result", "request_bytes", "reply_bytes", "stage"}

// A compound member's call runs in a span of its own, a child of the
// request's named adapter.<protocol>.<operation>, with §10.2's attributes
// only: the result ok, or the service error's name, and none for an error
// outside the service's set.
func TestChainOpensASpanPerCompoundMember(t *testing.T) {
	tr := newTraced(t, true)
	reqCtx, reqSpan := tr.tracer.Start(context.Background(), "request")
	for _, answer := range []error{nil, vfs.ErrNotFound, errHandler} {
		err := tr.members.Chain(tr.mEcho)(reqCtx, request(func(ctx context.Context, _ *core.Request) error {
			if sc := trace.SpanFromContext(ctx); sc.SpanContext().SpanID() == reqSpan.SpanContext().SpanID() || !sc.IsRecording() {
				t.Error("a member's call runs under the request's span, not a recording span of its own")
			}
			return answer
		}))
		if err != answer {
			t.Errorf("the chain answered %v, want the handler's %v", err, answer)
		}
	}
	reqSpan.End()
	var results []string
	for _, s := range tr.spans.Ended() {
		if s.Name() != "adapter."+testproto.Name+"."+tr.mEcho.Name {
			continue
		}
		if s.Parent().SpanID() != reqSpan.SpanContext().SpanID() {
			t.Errorf("the member's span's parent is %v, want the request's", s.Parent().SpanID())
		}
		attrs := map[string]string{}
		for _, kv := range s.Attributes() {
			if !slices.Contains(spanKeys, string(kv.Key)) {
				t.Errorf("the member's span carries %s, outside §10.2's keys", kv.Key)
			}
			attrs[string(kv.Key)] = kv.Value.String()
		}
		if attrs["protocol"] != testproto.Name || attrs["version"] != "1" || attrs["op"] != "echo" {
			t.Errorf("the member's span's attributes are %v", attrs)
		}
		result, ok := attrs["result"]
		if !ok {
			result = "none"
		}
		results = append(results, result)
	}
	if !slices.Equal(results, []string{"ok", "not_found", "none"}) {
		t.Errorf("the members' spans' results are %v, want ok, not_found and none", results)
	}
}

// Outside a compound the call runs under the request's span, and the chain
// opens none.
func TestChainOutsideACompoundOpensNoSpan(t *testing.T) {
	tr := newTraced(t, true)
	reqCtx, reqSpan := tr.tracer.Start(context.Background(), "request")
	_ = tr.top.Chain(tr.topEcho)(reqCtx, request(func(ctx context.Context, _ *core.Request) error {
		if trace.SpanFromContext(ctx).SpanContext().SpanID() != reqSpan.SpanContext().SpanID() {
			t.Error("a single operation's call runs under a span of its own")
		}
		return nil
	}))
	if n := len(tr.spans.Ended()); n != 0 {
		t.Errorf("the chain ended %d spans, want none", n)
	}
}

// unsampled is the context of a request sampled at ratio 0: its span context
// is valid, and not sampled.
func unsampled(t *testing.T) context.Context {
	t.Helper()
	tracer := sdktrace.NewTracerProvider(sdktrace.WithSampler(sdktrace.TraceIDRatioBased(0))).Tracer("test")
	ctx, span := tracer.Start(context.Background(), "request")
	if sc := span.SpanContext(); !sc.IsValid() || sc.IsSampled() {
		t.Fatalf("the request's span context is %v, want valid and not sampled", sc)
	}
	return ctx
}

// A member of a request not sampled runs under the request's span context:
// the chain starts no span for it, not even one that records nothing
// (RFC 25 §6.3).
func TestChainUnsampledOpensNoSpan(t *testing.T) {
	tr := newTraced(t, true)
	reqCtx := unsampled(t)
	_ = tr.members.Chain(tr.mEcho)(reqCtx, request(func(ctx context.Context, _ *core.Request) error {
		if got := trace.SpanContextFromContext(ctx); !got.Equal(trace.SpanContextFromContext(reqCtx)) {
			t.Errorf("an unsampled member's call runs under span %v, want the request's %v", got.SpanID(), trace.SpanContextFromContext(reqCtx).SpanID())
		}
		return nil
	}))
}

// A member of a request not sampled allocates no more with a tracer than
// with none: RFC 25 §6.3's tracing off against on at ratio 0.
func TestChainUnsampledAllocatesNothingMore(t *testing.T) {
	if prof.RaceEnabled {
		t.Skip("the race detector changes allocation counts")
	}
	ctx := unsampled(t)
	allocs := map[bool]float64{}
	for _, tracing := range []bool{false, true} {
		tr := newTraced(t, tracing)
		call, r := tr.members.Chain(tr.mEcho), request(func(context.Context, *core.Request) error { return nil })
		allocs[tracing] = testing.AllocsPerRun(100, func() { _ = call(ctx, r) })
	}
	if allocs[true] != allocs[false] {
		t.Errorf("an unsampled member's call allocates %v times with a tracer, %v with none; want the same", allocs[true], allocs[false])
	}
}

// The chain's fixed order (§4.7), seen from a member's call that panics:
// the handler's context is the deadline's own, done at 30 s on the clock
// while the span is still open; the panic, which recovery turns into
// ErrPanicked outermost, reaches the span first, which ends with the fault
// recorded, and the metrics hinge observed resolve_ids at entry and marks
// the call's end on the way out.
func TestChainOrder(t *testing.T) {
	tr := newTraced(t, true)
	reqCtx, _ := tr.tracer.Start(context.Background(), "request")
	r := request(func(ctx context.Context, _ *core.Request) error {
		if !core.OnTheDeadline(ctx) {
			t.Error("the handler's context is not the deadline's own: the deadline is not innermost")
		}
		tr.clk.Advance(30 * time.Second)
		if ctx.Err() == nil || !trace.SpanFromContext(ctx).IsRecording() {
			t.Errorf("at 30 s the context's Err is %v and the span recording %v; want done inside an open span", ctx.Err(), trace.SpanFromContext(ctx).IsRecording())
		}
		panic("boom")
	})
	if err := tr.members.Chain(tr.mEcho)(reqCtx, r); !errors.Is(err, core.ErrPanicked) {
		t.Fatalf("a panic is answered %v, want ErrPanicked", err)
	}
	ended := tr.spans.Ended()
	if len(ended) != 1 || len(ended[0].Events()) != 1 || ended[0].Events()[0].Name != "exception" {
		t.Errorf("the member's span ended %d times; want once, with the fault recorded", len(ended))
	}
	if got := core.CallEnd(r); !got.Equal(t0.Add(30 * time.Second)) {
		t.Errorf("the call's end is marked at %v, want the clock's %v", got, t0.Add(30*time.Second))
	}
	got, _ := scrape(t, tr.m)
	k := "adapter_wire_seconds{protocol=" + testproto.Name + ",stage=resolve_ids}"
	if got[k] != 1 || got["adapter_wire_seconds_sum{protocol="+testproto.Name+",stage=resolve_ids}"] != 0.002 {
		t.Errorf("resolve_ids observed %v times, summing %v; want once, 2 ms", got[k], got["adapter_wire_seconds_sum{protocol="+testproto.Name+",stage=resolve_ids}"])
	}
	if !strings.Contains(tr.logged.String(), `"level":"ERROR"`) {
		t.Errorf("the panic was not logged at error: %q", tr.logged.String())
	}
}

// The metrics hinge runs before the deadline (§4.7), so resolve_ids ends as
// the chain reaches the hinge: on a clock that moves 1 ms each time it is
// read, it is the request's own 2 ms, with none of the deadline's reads.
func TestChainOrderHingeBeforeDeadline(t *testing.T) {
	tr := newTraced(t, false)
	tr.clk.step = time.Millisecond
	if err := tr.top.Chain(tr.topEcho)(context.Background(), request(func(context.Context, *core.Request) error { return nil })); err != nil {
		t.Fatal(err)
	}
	got, _ := scrape(t, tr.m)
	if sum := got["adapter_wire_seconds_sum{protocol="+testproto.Name+",stage=resolve_ids}"]; sum != 0.002 {
		t.Errorf("resolve_ids sums %v s, want the request's 0.002: the deadline's clock reads are counted in it", sum)
	}
}

// With no tracer a member's call runs under the request's context, and the
// rest of the chain still runs.
func TestChainWithoutATracer(t *testing.T) {
	tr := newTraced(t, false)
	reqCtx, reqSpan := tr.tracer.Start(context.Background(), "request")
	err := tr.members.Chain(tr.mEcho)(reqCtx, request(func(ctx context.Context, _ *core.Request) error {
		if trace.SpanFromContext(ctx).SpanContext().SpanID() != reqSpan.SpanContext().SpanID() {
			t.Error("with no tracer a member's call runs under a span of its own")
		}
		return errHandler
	}))
	if err != errHandler {
		t.Errorf("the chain answered %v, want the handler's", err)
	}
}

// deadlineOnly is a parent that reports a deadline and never ends of
// itself: an input standing for a parent on another clock.
type deadlineOnly struct {
	context.Context
	at time.Time
}

func (d deadlineOnly) Deadline() (time.Time, bool) { return d.at, true }

// A call under a parent that is itself the deadline's own context leaves the
// parent nothing to run once the call returns: its watch of the parent is
// stopped, so a connection's or a held request's context gains nothing per
// call.
func TestADeadlineStopsWatchingItsParent(t *testing.T) {
	sh := core.NewShared(adapter.Deps{})
	core.SetClock(sh, newManualClock())
	left := -2
	outer := sh.Deadline(func(ctx context.Context, r *core.Request) error {
		inner := sh.Deadline(func(context.Context, *core.Request) error { return nil })
		if err := inner(ctx, r); err != nil {
			return err
		}
		left = core.Watches(ctx)
		return nil
	})
	if err := outer(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	if left != 0 {
		t.Errorf("the parent still runs %d functions after the call, want none", left)
	}
}
