package core_test

import (
	"context"
	"encoding/binary"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"runtime"
	rtmetrics "runtime/metrics"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"

	"github.com/cubbit/dittofs/internal/adapter/adaptertest"
	"github.com/cubbit/dittofs/internal/adapter/adaptertest/testproto"
	"github.com/cubbit/dittofs/internal/adapter/core"
	"github.com/cubbit/dittofs/internal/prof"
	"github.com/cubbit/dittofs/pkg/adapter"
	"github.com/cubbit/dittofs/pkg/adapter/settings"
	"github.com/cubbit/dittofs/pkg/metrics"
	"github.com/cubbit/dittofs/pkg/resources"
	"github.com/cubbit/dittofs/pkg/vfs"
	"github.com/cubbit/dittofs/pkg/vfs/vfstest"
)

// The pipeline benchmark and its allocation gate (RFC 20 §6.10): the whole
// pipeline over loopback against vfstest.Null, a load and not a double, on a
// server with a registry and a logger at a level that drops every line.

// passThrough is the null service reached as a real service is: each Create
// derives one context of its own from the call's and calls the null service
// under it, then answers err in place of its success where err is set. err
// is set before the first call, so it arms nothing per call.
type passThrough struct {
	vfstest.Null
	err error
}

func (s passThrough) Create(ctx context.Context, c vfs.Call, dir vfs.Handle, name []byte, a vfs.CreateArgs) (vfs.CreateResult, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	res, err := s.Null.Create(ctx, c, dir, name, a)
	if s.err != nil {
		return vfs.CreateResult{}, s.err
	}
	return res, err
}

// pipelineCase is one record, sent on a connection of the full protocol set
// up at version, or of Bare.
type pipelineCase struct {
	name    string
	bare    bool
	version core.Version
	err     error // what Create answers; nil: success
	msgs    []testproto.Msg
	status  core.Status // the last reply's, or a compound's head's
}

// compoundV1 is version 1's compound of a PutFH and a Create, Create last.
var compoundV1 = []testproto.Msg{{Op: testproto.OpCompound, Version: 1, ID: 9, Session: 7, Tree: 1, Members: []testproto.Msg{
	{Op: testproto.OpPutFH, Handle: vfs.Handle("dir")},
	{Op: testproto.OpCreate, Name: "f"},
}}}

// echoBare is an Echo over Bare, which reaches no service.
var echoBare = pipelineCase{name: "echo-bare", bare: true, msgs: []testproto.Msg{{Op: testproto.OpEcho, Version: 1, ID: 9, Payload: []byte("hi")}}}

// gated is the cases TestPipelineAllocs counts, named as testdata/allocs
// names them.
var gated = []pipelineCase{
	echoBare,
	{name: "compound-v1", version: 1, msgs: compoundV1},
	// Version 1 answers ErrDelay at once, by a sentinel's row late in Map's
	// scan.
	{name: "compound-v1-delay", version: 1, err: vfs.ErrDelay, msgs: compoundV1, status: testproto.StatusDelay},
	// Version 2's table holds, so the interim timer is armed per call. It has
	// no compound row: the same two members go as a chain.
	{name: "chain-v2", version: 2, msgs: []testproto.Msg{
		{Op: testproto.OpPutFH, Version: 2, ID: 9, Session: 7, Tree: 1, Handle: vfs.Handle("dir")},
		{Op: testproto.OpCreate, Version: 2, ID: 10, Session: inherited, Tree: 1, Name: "f", Related: true},
	}},
}

// loopback is a server on the null service for one case, whose connections
// are dialled over loopback.
type loopback struct {
	tb  testing.TB
	c   pipelineCase
	srv *core.Server
	l   core.Listener
	ln  net.Listener
}

// newLoopback starts a server for c on b's budgets, tracing as tracing's
// Tracer, SampleRatio and TraceIDs say (zero: off).
func newLoopback(tb testing.TB, c pipelineCase, b *resources.Budgets, tracing adapter.Deps) *loopback {
	tb.Helper()
	d := tracing
	d.VFS, d.Budgets, d.Metrics = passThrough{err: c.err}, b, metrics.New("v", "c")
	d.Logger = slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError + 1}))
	var proto core.Protocol = testproto.New(d)
	if c.bare {
		proto = testproto.New(d).Bare()
	}
	lb := &loopback{tb: tb, c: c, l: core.Listener{Services: []string{"test"}, Network: "tcp", Protocol: proto}}
	// decision: the connections run with idle_timeout 0, so no idle deadline
	// is armed. A connection re-arms it each moment nothing runs, after its
	// pipeline's count of what runs falls to zero, so a run could not tell
	// its request's end from a re-arm still to come without recording the
	// connection's timer; the re-arm is the connection's cost per idle moment,
	// not the pipeline's per request. Overturned if the gate must count it.
	set := settings.DefaultNFS()
	set.IdleTimeout = 0
	var err error
	if lb.srv, err = core.NewServer(d, set, lb.l); err != nil {
		tb.Fatal(err)
	}
	core.Serving(lb.srv)
	if lb.ln, err = net.Listen("tcp", "127.0.0.1:0"); err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(func() {
		_ = lb.ln.Close()
		_ = lb.srv.Stop(context.Background())
	})
	return lb
}

// client is one connection's client end: it sends one record, encoded once,
// and reads each reply record into one buffer, so it allocates nothing.
type client struct {
	conn *core.Conn
	nc   net.Conn
	req  []byte
	n    int // the replies the record is answered
	buf  []byte
}

// dial opens a connection over loopback, hands the server its end and sets
// it up: Negotiate, Session and Tree on the full protocol, established on
// Bare as a protocol's first routed call establishes it. It sends the case's
// record once, read by testproto's client, and fails tb unless its last
// reply, or a compound's head, carries the case's status.
func (lb *loopback) dial() *client {
	tb := lb.tb
	tb.Helper()
	nc, err := net.Dial("tcp", lb.ln.Addr().String())
	if err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(func() { _ = nc.Close() })
	sc, err := lb.ln.Accept()
	if err != nil {
		tb.Fatal(err)
	}
	cl := &client{conn: core.ServeConn(lb.srv, sc, &lb.l), nc: nc, req: testproto.Append(nil, lb.c.msgs...), n: len(lb.c.msgs), buf: make([]byte, 64<<10)}
	tc := testproto.NewClient(nc)
	var msgs []testproto.Msg
	if lb.c.bare {
		cl.conn.Established()
	} else {
		msgs = []testproto.Msg{
			{Op: testproto.OpNegotiate, Version: lb.c.version, ID: 1},
			{Op: testproto.OpSession, Version: lb.c.version, ID: 2, Session: 7},
			{Op: testproto.OpTree, Version: lb.c.version, ID: 3, Session: 7, Tree: 1},
		}
	}
	for _, m := range msgs {
		if err := tc.Send(m); err != nil {
			tb.Fatal(err)
		}
		if rs, err := tc.Recv(); err != nil || len(rs) != 1 || rs[0].Status != testproto.StatusOK {
			tb.Fatalf("setting up the connection: %+v (%v)", rs, err)
		}
	}
	if err := tc.Send(lb.c.msgs...); err != nil {
		tb.Fatal(err)
	}
	var rs []testproto.Reply
	for len(rs) < cl.n {
		got, err := tc.Recv()
		if err != nil {
			tb.Fatal(err)
		}
		rs = append(rs, got...)
	}
	last := rs[len(rs)-1]
	if lb.c.msgs[0].Op == testproto.OpCompound {
		head, _, err := testproto.Results(last.Body)
		if err != nil {
			tb.Fatal(err)
		}
		last.Status = head.Status
	}
	if last.Status != lb.c.status {
		tb.Fatalf("%s is answered status %d (%+v), want status %d", lb.c.name, last.Status, rs, lb.c.status)
	}
	return cl
}

// exchange sends the record and reads its replies, walking each reply record
// by the next reply's offset, which opens every reply's header in
// testproto's format. It returns its error, since a parallel benchmark's
// goroutines may not call Fatal.
func (cl *client) exchange() error {
	if _, err := cl.nc.Write(cl.req); err != nil {
		return err
	}
	for got := 0; got < cl.n; {
		if _, err := io.ReadFull(cl.nc, cl.buf[:4]); err != nil {
			return err
		}
		body := cl.buf[:binary.BigEndian.Uint32(cl.buf)]
		if _, err := io.ReadFull(cl.nc, body); err != nil {
			return err
		}
		for off := 0; ; {
			got++
			if off = int(binary.BigEndian.Uint32(body[off:])); off == 0 {
				break
			}
		}
	}
	return nil
}

// gateRuns is how many times allocs runs a case, past AllocsPerRun's warm-up.
const gateRuns = 1000

// allocs is the allocations per record sent, its members together, of
// gateRuns runs of the case on its own server, tracing as tracing says. Each
// run waits for its request's end on the server before it returns, so the
// last run's tail is counted: nothing running on its pipeline, and the reply
// budget, which nothing else holds, back at zero, the writer having released
// each reply once written. Over the same runs, the warm-up's included, each
// record starts one goroutine, its request's or its chain's, by the
// runtime's count of goroutines created, and leaves none running once its
// request has ended; t fails otherwise (RFC 20 §6.9, §6.10; RFC 24 §6.1).
//
// decision: both goroutine counts are the process's, averaged over the runs
// and rounded down as AllocsPerRun rounds its count, so a goroutine the
// process starts elsewhere meanwhile, a timer's, or the last run's own still
// returning, is not charged to a record, and a goroutine a request starts or
// leaves on fewer than every run passes. Overturned if a request is seen to
// start or leave one on some runs only.
func (c pipelineCase) allocs(t *testing.T, b *resources.Budgets, tracing adapter.Deps) float64 {
	t.Helper()
	cl := newLoopback(t, c, b, tracing).dial()
	p := core.ConnPipeline(cl.conn)
	before, live := goroutinesStarted(), runtime.NumGoroutine()
	n := testing.AllocsPerRun(gateRuns, func() {
		if err := cl.exchange(); err != nil {
			t.Fatal(err)
		}
		for i := 0; p.Runs() > 0 || b.Reply.Reserved() > 0; i++ {
			if i == 1<<24 {
				t.Fatalf("%s: the request did not end: %d running, %d reply bytes reserved", c.name, p.Runs(), b.Reply.Reserved())
			}
			runtime.Gosched()
		}
	})
	if g := (goroutinesStarted() - before) / (gateRuns + 1); g != 1 {
		t.Errorf("%s starts %d goroutines per record, want one: its request's or its chain's", c.name, g)
	}
	if g := (runtime.NumGoroutine() - live) / (gateRuns + 1); g > 0 {
		t.Errorf("%s leaves %d goroutines running per record once its request has ended, want none", c.name, g)
	}
	return n
}

// goroutinesStarted is the runtime's count of the goroutines the process
// has started.
func goroutinesStarted() uint64 {
	s := []rtmetrics.Sample{{Name: "/sched/goroutines-created:goroutines"}}
	rtmetrics.Read(s)
	return s[0].Value.Uint64()
}

// checkedIn is testdata/allocs: each case's allocations per record sent.
func checkedIn(t *testing.T) map[string]float64 {
	t.Helper()
	b, err := os.ReadFile("testdata/allocs")
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]float64{}
	for line := range strings.Lines(string(b)) {
		if strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 2 {
			t.Fatalf("testdata/allocs: %q is no case and count", line)
		}
		n, err := strconv.ParseFloat(fields[1], 64)
		if err != nil {
			t.Fatalf("testdata/allocs: %q: %v", line, err)
		}
		want[fields[0]] = n
	}
	return want
}

// The pipeline's allocations per record sent, at the numbers checked in, which
// may only fall: a case above its number fails, and so does one below it,
// until the number is lowered. An unsampled request allocates nothing for
// tracing: under a real provider at ratio 0, and at ratio 0.01 with every
// trace ID one the ratio rule rejects, each case counts what it counts with
// tracing off (RFC 20 §6.10, §10.2). Create refused ErrDelay of
// DelayResources, which both of testproto's tables map to StatusDelay at
// once, an expected refusal logged at debug, counts what its successful run
// does at a level that drops every line, so no line is built for it
// (RFC 20 §10.3). In every run a record starts one goroutine and leaves none
// running, so a goroutine a request starts, which a heap count may miss, fails
// here too (RFC 20 §6.10; RFC 24 §6.1).
func TestPipelineAllocs(t *testing.T) {
	if prof.RaceEnabled {
		t.Skip("the race detector changes allocation counts")
	}
	b := budgets()
	adaptertest.CheckLeaks(t, b)
	want := checkedIn(t)
	tracer, src := sdktrace.NewTracerProvider().Tracer("gate"), seeded(7)
	unsampled := map[string]adapter.Deps{
		"at ratio 0": {Tracer: tracer},
		// Bit 63 of each ID's low eight bytes is set, so the ratio rule
		// rejects every request, the warm-up's and the dial's included.
		"at ratio 0.01": {Tracer: tracer, SampleRatio: 0.01, TraceIDs: func() trace.TraceID {
			id := src()
			id[8] |= 0x80
			return id
		}},
	}
	off := map[string]float64{}
	for _, c := range gated {
		off[c.name] = c.allocs(t, b, adapter.Deps{})
		w, ok := want[c.name]
		switch {
		case !ok:
			t.Errorf("%s allocates %v times per record, and testdata/allocs has no number for it", c.name, off[c.name])
		case off[c.name] > w:
			t.Errorf("%s allocates %v times per record, past the %v checked in", c.name, off[c.name], w)
		case off[c.name] < w:
			t.Errorf("%s allocates %v times per record, below the %v checked in: lower it in testdata/allocs", c.name, off[c.name], w)
		}
		for name, d := range unsampled {
			if n := c.allocs(t, b, d); n != off[c.name] {
				t.Errorf("%s unsampled %s allocates %v times per record, %v with tracing off; want the same", c.name, name, n, off[c.name])
			}
		}
	}
	refused := pipelineCase{name: "compound-v1-refused", version: 1, err: &vfs.Delay{Cause: vfs.DelayResources}, msgs: compoundV1, status: testproto.StatusDelay}
	if n := refused.allocs(t, b, adapter.Deps{}); n != off["compound-v1"] {
		t.Errorf("the compound refused ErrDelay of DelayResources allocates %v times per record, %v answered success; want the same", n, off["compound-v1"])
	}
}

// BenchmarkPipeline reports ns and allocations per record sent: the gated
// cases; an Echo with 64 requests in flight on one connection; a chain of
// three Echo, idle and with the reply pool held to one reply's room; and an
// Echo on each of GOMAXPROCS connections.
func BenchmarkPipeline(b *testing.B) {
	for _, c := range gated {
		b.Run(c.name, func(b *testing.B) {
			exchanges(b, newLoopback(b, c, budgets(), adapter.Deps{}).dial())
		})
	}
	b.Run("echo-bare-64-in-flight", benchInFlight)
	chain := pipelineCase{name: "chain-of-3-echo", bare: true, msgs: []testproto.Msg{
		{Op: testproto.OpEcho, Version: 1, ID: 1, Payload: []byte("hi")},
		{Op: testproto.OpEcho, Version: 1, ID: 2, Payload: []byte("hi")},
		{Op: testproto.OpEcho, Version: 1, ID: 3, Payload: []byte("hi")},
	}}
	b.Run("chain-of-3-echo-idle", func(b *testing.B) {
		exchanges(b, newLoopback(b, chain, budgets(), adapter.Deps{}).dial())
	})
	b.Run("chain-of-3-echo-one-reply-of-room", func(b *testing.B) {
		bs := budgets()
		cl := newLoopback(b, chain, bs, adapter.Deps{}).dial()
		// The reply pool held to the room of one Echo's reply, its payload and
		// a 40-byte header: each member after the first finds no room at once
		// while its chain holds a reply, so the chain sends the replies before
		// it first, then waits for room (RFC 20 §4.7).
		pool := resources.NewBufPool(bs.Reply)
		hold, err := bs.Reply.Reserve(context.Background(), netip.MustParseAddr("192.0.2.2"), pool.Ceiling()-pool.Charge(len("hi")+40))
		if err != nil {
			b.Fatal(err)
		}
		defer hold.Release()
		exchanges(b, cl)
	})
	b.Run("echo-bare-per-proc", func(b *testing.B) {
		lb := newLoopback(b, echoBare, budgets(), adapter.Deps{})
		var cls []*client
		for range runtime.GOMAXPROCS(0) {
			cls = append(cls, lb.dial())
		}
		var next atomic.Int64
		b.ReportAllocs()
		b.ResetTimer()
		b.RunParallel(func(pb *testing.PB) {
			cl := cls[next.Add(1)-1]
			for pb.Next() {
				if err := cl.exchange(); err != nil {
					b.Error(err)
					return
				}
			}
		})
	})
}

// exchanges times b.N exchanges on cl.
func exchanges(b *testing.B, cl *client) {
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		if err := cl.exchange(); err != nil {
			b.Fatal(err)
		}
	}
}

// benchInFlight runs Echo over Bare from at least 64 goroutines on one
// connection, each with a message ID of its own: Bare's flow control admits
// 64 at once, so 64 requests are in flight. One goroutine reads every reply
// and wakes the goroutine its message ID names, at byte 12 of its header in
// testproto's format.
func benchInFlight(b *testing.B) {
	lb := newLoopback(b, echoBare, budgets(), adapter.Deps{})
	cl := lb.dial()
	procs := runtime.GOMAXPROCS(0)
	n := (64 + procs - 1) / procs * procs
	reqs, woken := make([][]byte, n), make([]chan struct{}, n)
	for i := range n {
		reqs[i] = testproto.Append(nil, testproto.Msg{Op: testproto.OpEcho, Version: 1, ID: uint64(i), Payload: []byte("hi")})
		woken[i] = make(chan struct{}, 1)
	}
	go func() {
		buf := make([]byte, 64<<10)
		for {
			if _, err := io.ReadFull(cl.nc, buf[:4]); err != nil {
				return
			}
			body := buf[:binary.BigEndian.Uint32(buf)]
			if _, err := io.ReadFull(cl.nc, body); err != nil {
				return
			}
			woken[binary.BigEndian.Uint64(body[12:])] <- struct{}{}
		}
	}()
	var next atomic.Int64
	b.SetParallelism(n / procs)
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		i := next.Add(1) - 1
		for pb.Next() {
			if _, err := cl.nc.Write(reqs[i]); err != nil {
				b.Error(err)
				return
			}
			<-woken[i]
		}
	})
}
