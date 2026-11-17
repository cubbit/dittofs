package core_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace/noop"

	"github.com/cubbit/dittofs/internal/adapter/adaptertest/testproto"
	"github.com/cubbit/dittofs/internal/adapter/core"
	"github.com/cubbit/dittofs/pkg/adapter"
	"github.com/cubbit/dittofs/pkg/adapter/settings"
	"github.com/cubbit/dittofs/pkg/metrics"
	"github.com/cubbit/dittofs/pkg/resources"
	"github.com/cubbit/dittofs/pkg/vfs"
)

var peer = netip.MustParseAddr("192.0.2.1")

// smallCharge is what a small-class reply buffer reserves: 4 KiB and the
// per-request overhead.
const smallCharge = 12 << 10

// started is a call run on its own goroutine; done is closed once it returns.
type started struct{ done chan struct{} }

func start(f func()) started {
	s := started{done: make(chan struct{})}
	go func() {
		defer close(s.done)
		f()
	}()
	return s
}

func (s started) returned() bool {
	select {
	case <-s.done:
		return true
	default:
		return false
	}
}

// admit is an Admit that must return a place at once.
func admit(t *testing.T, ctx context.Context, f core.FlowControl) func() {
	t.Helper()
	release, err := f.Admit(ctx, nil)
	if err != nil {
		t.Fatalf("Admit with a place free = %v, want a place", err)
	}
	return release
}

// The cap admits 64 requests; the 65th waits until a release frees a place.
func TestCapAdmits64(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := core.NewCapped()
		releases := make([]func(), 64)
		for i := range releases {
			releases[i] = admit(t, t.Context(), f)
		}
		var last func()
		s := start(func() { last, _ = f.Admit(t.Context(), nil) })
		synctest.Wait()
		if s.returned() {
			t.Fatal("the 65th Admit returned with 64 in flight, want it waiting")
		}
		releases[0]()
		synctest.Wait()
		if !s.returned() || last == nil {
			t.Fatal("the 65th Admit still waits after a release, want it admitted")
		}
		last()
		for _, release := range releases[1:] {
			release()
		}
	})
}

// A release called twice frees one place: after it, one Admit is let in and
// the next waits.
func TestCapReleaseFreesOnePlace(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := core.NewCapped()
		releases := make([]func(), 64)
		for i := range releases {
			releases[i] = admit(t, t.Context(), f)
		}
		releases[0]()
		releases[0]()
		releases[0] = admit(t, t.Context(), f)
		s := start(func() {
			if release, err := f.Admit(t.Context(), nil); err == nil {
				release()
			}
		})
		synctest.Wait()
		if s.returned() {
			t.Fatal("a second Admit returned after one release called twice, want it waiting: the release freed two places")
		}
		for _, release := range releases {
			release()
		}
	})
}

// Under a context already ended, Admit takes a free place, and with none
// returns the context's error at once.
func TestCapUnderAnEndedContextTriesOnce(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cancelled, cancel := context.WithCancel(t.Context())
		cancel()
		late, stop := context.WithDeadline(t.Context(), time.Now().Add(-time.Second))
		defer stop()
		for _, ctx := range []context.Context{cancelled, late} {
			f := core.NewCapped()
			releases := make([]func(), 64)
			for i := range releases {
				releases[i] = admit(t, ctx, f)
			}
			var err error
			s := start(func() { _, err = f.Admit(ctx, nil) })
			synctest.Wait()
			if !s.returned() {
				t.Error("Admit at the cap under an ended context waits, want it refused at once")
			} else if !errors.Is(err, ctx.Err()) {
				t.Errorf("Admit at the cap under an ended context = %v, want %v", err, ctx.Err())
			}
			for _, release := range releases {
				release()
			}
			<-s.done
		}
	})
}

// reserving is a reserveReply run on its own goroutine.
type reserving struct {
	started
	buf *resources.Buf
	err error
}

func startReserving(ctx context.Context, pool *resources.BufPool, bound int) *reserving {
	r := &reserving{}
	r.started = start(func() { r.buf, r.err = core.NewShared(adapter.Deps{}, pool).ReserveReply(ctx, peer, bound) })
	return r
}

// With the reply budget full, a reply reservation waits: its context's
// deadline ends it with the adapter's ErrDelay of DelayResources, a cancel
// with the context's error, and room freeing first with the buffer. The
// budget holds nothing for a wait that ended.
func TestReserveReplyWaitsForRoom(t *testing.T) {
	for _, end := range []string{"deadline", "cancel", "room"} {
		t.Run(end, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				budget := resources.NewWaitBudget("reply", smallCharge)
				pool := resources.NewBufPool(budget)
				held, ok := pool.TryGet(peer, 0)
				if !ok {
					t.Fatal("TryGet on an empty budget refused")
				}
				ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
				defer cancel()
				r := startReserving(ctx, pool, 0)
				synctest.Wait()
				if r.returned() {
					t.Fatalf("reserveReply on a full budget returned (%v), want it waiting", r.err)
				}

				switch end {
				case "deadline":
					<-r.done
					if !errors.Is(r.err, &vfs.Delay{Cause: vfs.DelayResources}) {
						t.Errorf("at the deadline reserveReply = %v, want ErrDelay of DelayResources", r.err)
					}
				case "cancel":
					cancel()
					<-r.done
					if !errors.Is(r.err, context.Canceled) || errors.Is(r.err, vfs.ErrDelay) {
						t.Errorf("cancelled, reserveReply = %v, want the context's error", r.err)
					}
				case "room":
					held.Release()
					held = nil
					<-r.done
					if r.err != nil || r.buf == nil || budget.Reserved() != smallCharge {
						t.Fatalf("with room, reserveReply = %v with %d reserved, want the buffer charged %d", r.err, budget.Reserved(), smallCharge)
					}
					r.buf.Release()
				}
				if held != nil {
					if r.buf != nil || budget.Reserved() != smallCharge {
						t.Errorf("after the wait ended: buffer %v, %d reserved; want none and the held %d", r.buf != nil, budget.Reserved(), smallCharge)
					}
					held.Release()
				}
				if got := budget.Reserved(); got != 0 {
					t.Errorf("at the end %d reserved, want 0", got)
				}
			})
		})
	}
}

// A bound past the reply pool's ceiling is ErrReplyTooBig at once, under a
// context with no deadline, with nothing reserved.
func TestReserveReplyPastTheCeilingIsTooBig(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		budget := resources.NewWaitBudget("reply", 16<<20)
		pool := resources.NewBufPool(budget)
		r := startReserving(t.Context(), pool, 16<<20)
		synctest.Wait()
		if !r.returned() {
			t.Fatal("reserveReply past the ceiling still waits, want it refused at once")
		}
		if !errors.Is(r.err, core.ErrReplyTooBig) || r.buf != nil || budget.Reserved() != 0 {
			t.Errorf("reserveReply past the ceiling = %v with %d reserved, want ErrReplyTooBig and 0", r.err, budget.Reserved())
		}
	})
}

// With the reply budget full, a status's buffer waits under the connection's
// context: it is handed out once room frees; at 30 s from the start of its
// wait the connection is closed with the reply-wait error; when the
// connection's context ends first the wait returns at once without closing.
// The budget holds nothing for a wait that ended.
func TestReserveStatusWaitsAtMost30s(t *testing.T) {
	for _, end := range []string{"room", "30 s", "closed"} {
		t.Run(end, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				budget := resources.NewWaitBudget("reply", smallCharge)
				pool := resources.NewBufPool(budget)
				held, ok := pool.TryGet(peer, 0)
				if !ok {
					t.Fatal("TryGet on an empty budget refused")
				}
				conn, cancel := context.WithCancel(t.Context())
				defer cancel()
				var closed []error
				var buf *resources.Buf
				begin := time.Now()
				s := start(func() {
					buf = core.NewShared(adapter.Deps{}, pool).ReserveStatus(conn, peer, func(err error) { closed = append(closed, err) })
				})
				synctest.Wait()
				if s.returned() {
					t.Fatal("reserveStatus on a full budget returned, want it waiting")
				}

				switch end {
				case "room":
					held.Release()
					held = nil
					<-s.done
					if buf == nil || budget.Reserved() != smallCharge {
						t.Fatalf("with room, reserveStatus handed out %v with %d reserved, want a buffer charged %d", buf != nil, budget.Reserved(), smallCharge)
					}
					buf.Release()
				case "30 s":
					select {
					case <-s.done:
					case <-time.After(time.Minute):
						t.Fatal("reserveStatus still waits a minute on, want it ended at 30 s")
					}
					if waited := time.Since(begin); waited != 30*time.Second {
						t.Errorf("reserveStatus ended after %v, want 30s", waited)
					}
					if len(closed) != 1 || !errors.Is(closed[0], core.ErrReplyWait) {
						t.Errorf("at 30 s the connection was closed with %v, want once with the reply-wait error", closed)
					}
				case "closed":
					cancel()
					synctest.Wait()
					if !s.returned() || time.Since(begin) != 0 {
						t.Fatalf("after the connection's end reserveStatus returned %v at %v, want at once", s.returned(), time.Since(begin))
					}
				}
				if end != "30 s" && len(closed) != 0 {
					t.Errorf("reserveStatus closed the connection with %v, want no close", closed)
				}
				if held != nil {
					if buf != nil || budget.Reserved() != smallCharge {
						t.Errorf("after the wait ended: buffer %v, %d reserved; want none and the held %d", buf != nil, budget.Reserved(), smallCharge)
					}
					held.Release()
				}
				if got := budget.Reserved(); got != 0 {
					t.Errorf("at the end %d reserved, want 0", got)
				}
			})
		})
	}
}

// seam is one connection's pipeline at its seam: a server's shared state over
// budgets the test holds, the recorder in out's place, and a testproto client
// reading what the recorder writes. What the pipeline's link asked to close
// with is on closed.
type seam struct {
	t       *testing.T
	budgets *resources.Budgets
	recv    *resources.BufPool
	reply   *resources.BufPool
	p       *core.Pipeline
	client  *testproto.Client
	closed  chan error
	f       core.Framer
	stop    func(error)
	// sent, when a test sets it before serving, sees each message as it is
	// handed to out: a pass-through, where what is sent is what is checked.
	sent func(msg *resources.Buf)
}

// newSeam builds a seam for proto's stages, its reply budget of reply bytes.
func newSeam(t *testing.T, proto core.Protocol, reply int64) *seam {
	t.Helper()
	return newSeamWith(t, proto, reply, adapter.Deps{})
}

// newSeamWith is newSeam on a server whose Deps are d: its registry, logger
// and tracer.
func newSeamWith(t *testing.T, proto core.Protocol, reply int64, d adapter.Deps) *seam {
	t.Helper()
	return newSeamSet(t, proto, reply, d, nil)
}

// newSeamSet is newSeamWith on a connection that loaded set at accept.
func newSeamSet(t *testing.T, proto core.Protocol, reply int64, d adapter.Deps, set settings.Settings) *seam {
	t.Helper()
	s := &seam{
		t:       t,
		budgets: &resources.Budgets{Receive: resources.NewWaitBudget("receive", 64<<20), Reply: resources.NewWaitBudget("reply", reply)},
		closed:  make(chan error, 8),
		f:       proto.Framer(),
	}
	s.recv = resources.NewBufPool(s.budgets.Receive)
	s.reply = resources.NewBufPool(s.budgets.Reply)
	st, err := core.WithDefaults(proto.NewConn(nil))
	if err != nil {
		t.Fatal(err)
	}
	out, stop, conn, _ := newRecorder(s.f)
	s.stop, s.client = stop, testproto.NewClient(conn)
	sent := func(msg *resources.Buf, key, before uint64) error {
		if s.sent != nil {
			s.sent(msg)
		}
		return out(msg, key, before)
	}
	s.p = core.NewPipeline(core.NewShared(d, s.reply), st, context.Background(), peer, set, sent, func(err error) {
		select {
		case s.closed <- err:
		default:
		}
		s.p.Close()
	})
	t.Cleanup(s.finish)
	return s
}

// finish closes the pipeline, waits for what runs, and stops the recorder; a
// budget still holding bytes then fails the test.
func (s *seam) finish() {
	s.p.Close()
	if err := s.p.Wait(context.Background()); err != nil {
		s.t.Error(err)
	}
	s.stop(nil)
	for name, b := range map[string]*resources.WaitBudget{"receive": s.budgets.Receive, "reply": s.budgets.Reply} {
		if r := b.Reserved(); r != 0 {
			s.t.Errorf("the %s budget still holds %d bytes", name, r)
		}
	}
}

// serve hands the pipeline a record carrying msgs, as a reader would.
func (s *seam) serve(msgs ...testproto.Msg) {
	s.serveBody(testproto.Append(nil, msgs...)[s.f.HeaderLen():])
}

// serveBody hands the pipeline a record of body, charged to the receive
// budget.
func (s *seam) serveBody(body []byte) {
	s.t.Helper()
	rec, err := s.recv.Get(context.Background(), peer, len(body))
	if err != nil {
		s.t.Fatal(err)
	}
	rec.B = append(rec.B[:0], body...)
	s.p.Serve(rec)
}

// read is the next record's one reply.
func (s *seam) read() testproto.Reply {
	s.t.Helper()
	replies, err := s.client.Recv()
	if err != nil || len(replies) != 1 {
		s.t.Fatalf("a record of %d replies (%v), want one", len(replies), err)
	}
	return replies[0]
}

// body is the body of a record carrying m, for a test to change.
func body(f core.Framer, m testproto.Msg) []byte { return testproto.Append(nil, m)[f.HeaderLen():] }

// An Echo is answered its payload, over Bare and over the full protocol.
func TestEchoIsAnsweredItsPayload(t *testing.T) {
	p := testproto.New(adapter.Deps{})
	for name, proto := range map[string]core.Protocol{"bare": p.Bare(), "full": p} {
		t.Run(name, func(t *testing.T) {
			s := newSeam(t, proto, 1<<20)
			s.serve(testproto.Msg{Op: testproto.OpEcho, Version: 1, ID: 7, Payload: []byte("hi")})
			if r := s.read(); r.ID != 7 || r.Status != testproto.StatusOK || string(r.Body) != "hi" {
				t.Errorf("an Echo is answered %+v, want its payload", r)
			}
		})
	}
}

// Arguments that do not decode are answered the decode status on their own
// message, and the connection serves on; a header whose next is bad is
// answered the same, and the connection then closes. Neither is the fault.
func TestDecodeErrorsAreAnsweredTheDecodeStatus(t *testing.T) {
	s := newSeam(t, testproto.New(adapter.Deps{}).Bare(), 1<<20)
	noRole := body(s.f, testproto.Msg{Op: testproto.OpEcho, Version: 1, ID: 3})
	s.serveBody(noRole[:len(noRole)-1])
	if r := s.read(); r.ID != 3 || r.Status != testproto.StatusDecode {
		t.Errorf("an Echo with no role byte is answered %+v, want the decode status", r)
	}
	s.serve(testproto.Msg{Op: testproto.OpEcho, Version: 1, ID: 4, Payload: []byte("on")})
	if r := s.read(); r.ID != 4 || r.Status != testproto.StatusOK {
		t.Errorf("the Echo after it is answered %+v, want served", r)
	}
	if len(s.closed) != 0 {
		t.Fatalf("a decode error closed the connection: %v", <-s.closed)
	}

	badNext := body(s.f, testproto.Msg{Op: testproto.OpEcho, Version: 1, ID: 5})
	binary.BigEndian.PutUint32(badNext, 39) // inside its own header
	s.serveBody(badNext)
	if r := s.read(); r.ID != 5 || r.Status != testproto.StatusDecode {
		t.Errorf("a header whose next is bad is answered %+v, want the decode status", r)
	}
	if err := <-s.closed; !errors.Is(err, core.ErrDecode) {
		t.Errorf("after a bad next the connection closed with %v, want a decode error", err)
	}
}

// A bad checksum, an overdraw and a version the router refuses are each
// answered a status on their own message, the connection kept; a record too
// short for a header closes it, answered nothing.
func TestRefusalsBeforeRouteAreAnswered(t *testing.T) {
	s := newSeam(t, testproto.New(adapter.Deps{}), 1<<20)
	bad := body(s.f, testproto.Msg{Op: testproto.OpEcho, Version: 1, ID: 1, Session: 5})
	bad[len(bad)-1] ^= 0x01
	s.serveBody(bad)
	s.serve(testproto.Msg{Op: testproto.OpEcho, Version: 1, ID: 2, Credits: 5})
	s.serve(testproto.Msg{Op: testproto.OpEcho, Version: 3, ID: 3})
	for id, want := range []core.Status{1: testproto.StatusChecksum, 2: testproto.StatusOverdraw, 3: testproto.StatusVersion} {
		if id == 0 {
			continue
		}
		if r := s.read(); r.ID != uint64(id) || r.Status != want {
			t.Errorf("refusal %d is answered %+v, want status %d on its own message", id, r, want)
		}
	}
	if len(s.closed) != 0 {
		t.Fatalf("a refusal closed the connection: %v", <-s.closed)
	}
	s.serveBody(make([]byte, 10))
	if err := <-s.closed; err == nil {
		t.Error("a record too short for a header did not close the connection")
	}
}

// A refusal goes out as Unwrap's verdict says: a bad checksum under session 5
// signed under it and not sealed; a message sent clear under it neither; an
// overdraw after Unwrap passed it sealed under it, as any reply; and the
// refusal of a header whose next is bad, which Unwrap never saw, with no
// checksum and its arguments clear.
func TestARefusalGoesOutAsItsVerdictSays(t *testing.T) {
	s := newSeam(t, testproto.New(adapter.Deps{}), 1<<20)
	bad := body(s.f, testproto.Msg{Op: testproto.OpEcho, Version: 1, ID: 1, Session: 5})
	bad[len(bad)-1] ^= 0x01
	s.serveBody(bad)
	if r := s.read(); r.Session != 5 || !r.Signed || r.Checksum == 0 {
		t.Errorf("a bad checksum is answered %+v, want signed under session 5 and not sealed", r)
	}
	s.serve(testproto.Msg{Op: testproto.OpEcho, Version: 1, ID: 2, Session: 5, Clear: true})
	if r := s.read(); r.Session != 0 || r.Signed || r.Checksum != 0 {
		t.Errorf("a message sent clear is answered %+v, want neither signed nor sealed", r)
	}
	s.serve(testproto.Msg{Op: testproto.OpEcho, Version: 1, ID: 3, Session: 5, Credits: 9})
	if r := s.read(); r.Status != testproto.StatusOverdraw || r.Session != 5 || r.Signed || r.Checksum == 0 {
		t.Errorf("an overdraw is answered %+v, want sealed under session 5", r)
	}
	badNext := body(s.f, testproto.Msg{Op: testproto.OpEcho, Version: 1, ID: 4, Session: 5})
	binary.BigEndian.PutUint32(badNext, 39)
	s.serveBody(badNext)
	if r := s.read(); r.Status != testproto.StatusDecode || r.Signed || r.Checksum != 0 {
		t.Errorf("a bad next is answered %+v, want no checksum and its arguments clear", r)
	}
}

// edited is a protocol declared in the test: testproto's stages, changed by
// edit. It is an input to the pipeline, not a double.
type edited struct {
	core.Protocol
	edit func(*core.Stages)
}

func (p edited) NewConn(c *core.Conn) core.Stages {
	st := p.Protocol.NewConn(c)
	p.edit(&st)
	return st
}

var errUndecryptable = errors.New("a message that does not decrypt")

// undecryptable is a Security whose Unwrap says close: a nil header.
type undecryptable struct{ core.Security }

func (undecryptable) Unwrap(core.Header, *resources.Buf, core.Header) (core.Header, error) {
	return nil, errUndecryptable
}

// unlabelled is a Router whose tables carry no Protocol.
type unlabelled struct{ core.Router }

func (u unlabelled) Route(h core.Header, prev *core.Request) (*core.OpTable, *core.Request, error) {
	tb, r, err := u.Router.Route(h, prev)
	if tb != nil {
		cp := *tb
		cp.Protocol = ""
		tb = &cp
	}
	return tb, r, err
}

// sentNothing reports, inside a synctest bubble, whether the client has no
// record to read once everything the pipeline started has settled, and then
// ends the seam.
func (s *seam) sentNothing() bool {
	synctest.Wait()
	read := start(func() { _, _ = s.client.Recv() })
	synctest.Wait()
	sent := read.returned()
	s.finish()
	<-read.done
	return !sent
}

// unwrapsNeither is a Security whose Unwrap breaks its contract, returning
// neither a header nor an error: an input.
type unwrapsNeither struct{ core.Security }

func (unwrapsNeither) Unwrap(core.Header, *resources.Buf, core.Header) (core.Header, error) {
	return nil, nil
}

// A message whose Unwrap returns no header closes the connection with
// Unwrap's error, and nothing is sent; one whose Unwrap returns neither a
// header nor an error closes it the same way, with core's own error.
func TestUnwrapsNilHeaderClosesTheConnection(t *testing.T) {
	for _, c := range []struct {
		name string
		sec  func(core.Security) core.Security
		want error
	}{
		{"no header", func(s core.Security) core.Security { return undecryptable{s} }, errUndecryptable},
		{"neither", func(s core.Security) core.Security { return unwrapsNeither{s} }, core.ErrUnwrapNeither},
	} {
		t.Run(c.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				s := newSeam(t, edited{testproto.New(adapter.Deps{}), func(st *core.Stages) { st.Security = c.sec(st.Security) }}, 1<<20)
				s.serve(testproto.Msg{Op: testproto.OpEcho, Version: 1, ID: 1})
				if err := <-s.closed; !errors.Is(err, c.want) {
					t.Errorf("the connection closed with %v, want %v", err, c.want)
				}
				if !s.sentNothing() {
					t.Error("a message Unwrap said close for was answered")
				}
			})
		})
	}
}

// Stages without Errors, or without Protocol, are refused, naming the field.
func TestWithDefaultsRefusesStagesMissingAField(t *testing.T) {
	for field, strip := range map[string]func(*core.Stages){
		"Errors":   func(st *core.Stages) { st.Errors = nil },
		"Protocol": func(st *core.Stages) { st.Protocol = "" },
		"Header":   func(st *core.Stages) { st.Header = nil },
		"Router":   func(st *core.Stages) { st.Router = nil },
	} {
		st := testproto.New(adapter.Deps{}).NewConn(nil)
		strip(&st)
		if _, err := core.WithDefaults(st); err == nil || !strings.Contains(err.Error(), field) {
			t.Errorf("stages without %s: WithDefaults = %v, want an error naming it", field, err)
		}
	}
}

// While draining, an Echo that says none gets nothing over version 1 and an
// interim granting its credits over version 2; one that says context, flush
// or release is answered.
func TestDrainServesByRole(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := newSeam(t, testproto.New(adapter.Deps{}), 1<<20)
		s.p.Drain()
		s.serve(testproto.Msg{Op: testproto.OpEcho, Version: 1, ID: 1, Role: core.RoleNone})
		s.serve(testproto.Msg{Op: testproto.OpEcho, Version: 2, ID: 2, Role: core.RoleNone, Credits: 1})
		for id, role := range map[uint64]core.Role{3: core.RoleContext, 4: core.RoleFlush, 5: core.RoleRelease} {
			s.serve(testproto.Msg{Op: testproto.OpEcho, Version: 1, ID: id, Role: role})
		}
		got := map[uint64]testproto.Reply{}
		for range 4 {
			r := s.read()
			got[r.ID&^(1<<63)] = r
		}
		if r := got[2]; !r.Interim || r.Credits != 1 {
			t.Errorf("version 2's refused Echo got %+v, want an interim granting its credit", r)
		}
		for id := uint64(3); id <= 5; id++ {
			if r, ok := got[id]; !ok || r.Status != testproto.StatusOK {
				t.Errorf("Echo %d, whose role is served while draining, got %+v", id, r)
			}
		}
		if _, ok := got[1]; ok || !s.sentNothing() {
			t.Errorf("version 1's Echo that says none was answered: %+v", got[1])
		}
	})
}

// A Panic is answered the fault status, and an Echo after it on the same
// connection is answered.
func TestPanicIsAnsweredTheFault(t *testing.T) {
	s := newSeam(t, testproto.New(adapter.Deps{}).Bare(), 1<<20)
	s.serve(testproto.Msg{Op: testproto.OpPanic, Version: 1, ID: 1})
	if r := s.read(); r.ID != 1 || r.Status != testproto.StatusFault {
		t.Errorf("a Panic is answered %+v, want the fault status", r)
	}
	s.serve(testproto.Msg{Op: testproto.OpEcho, Version: 1, ID: 2})
	if r := s.read(); r.ID != 2 || r.Status != testproto.StatusOK {
		t.Errorf("the Echo after a Panic is answered %+v", r)
	}
}

// askedReplay is the stages' own replay cache, counting its Lookups: a
// pass-through, where whether the cache is asked is what is checked.
type askedReplay struct {
	core.ReplayCache
	asked atomic.Int32
}

func (a *askedReplay) Lookup(ctx context.Context, h core.Header) (*resources.Buf, error) {
	a.asked.Add(1)
	return a.ReplayCache.Lookup(ctx, h)
}

// A bad checksum is refused before the replay cache is asked and before its
// credits are charged: the Echo after it, charging the connection's one
// credit, is served, and is the first request the cache is asked about.
func TestABadChecksumIsRefusedBeforeAdmit(t *testing.T) {
	replay := &askedReplay{}
	s := newSeam(t, edited{testproto.New(adapter.Deps{}), func(st *core.Stages) {
		replay.ReplayCache = st.Replay
		st.Replay = replay
	}}, 1<<20)
	bad := body(s.f, testproto.Msg{Op: testproto.OpEcho, Version: 1, ID: 1, Session: 5, Credits: 1})
	bad[len(bad)-1] ^= 0x01
	s.serveBody(bad)
	if r := s.read(); r.Status != testproto.StatusChecksum {
		t.Fatalf("a bad checksum is answered %+v", r)
	}
	if n := replay.asked.Load(); n != 0 {
		t.Errorf("the replay cache was asked %d times about a bad checksum, want none", n)
	}
	s.serve(testproto.Msg{Op: testproto.OpEcho, Version: 1, ID: 2, Credits: 1})
	if r := s.read(); r.Status != testproto.StatusOK {
		t.Errorf("the Echo after it is answered %+v: the refused message's credits were charged", r)
	}
	if n := replay.asked.Load(); n != 1 {
		t.Errorf("the replay cache was asked %d times once the Echo was served, want once", n)
	}
}

// Version 2 on stages with no Hold closes the connection; a table whose
// Protocol is empty closes it, naming the table.
func TestTablesTheStagesCannotServeCloseTheConnection(t *testing.T) {
	p := testproto.New(adapter.Deps{})
	s := newSeam(t, edited{p, func(st *core.Stages) { st.Hold = nil }}, 1<<20)
	s.serve(testproto.Msg{Op: testproto.OpEcho, Version: 2, ID: 1})
	if err := <-s.closed; !errors.Is(err, core.ErrNoHold) {
		t.Errorf("a holding table on stages with no Hold closed with %v", err)
	}
	s = newSeam(t, edited{p, func(st *core.Stages) { st.Router = unlabelled{st.Router} }}, 1<<20)
	s.serve(testproto.Msg{Op: testproto.OpEcho, Version: 1, ID: 1})
	if err := <-s.closed; err == nil || !strings.Contains(err.Error(), "Echo") {
		t.Errorf("a table with no Protocol closed with %v, want an error naming it", err)
	}
}

// full fills the seam's reply budget, its ceiling one small buffer, and
// returns what frees it.
func (s *seam) full() func() {
	s.t.Helper()
	held, ok := s.reply.TryGet(peer, 0)
	if !ok {
		s.t.Fatal("the reply budget is not free to fill")
	}
	return held.Release
}

// On Bare with the reply budget full, 64 of 65 Echo are admitted and the
// 65th waits in Admit until one completes; all 65 are answered once room is
// freed.
func TestTheCapAdmits64(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := newSeam(t, testproto.New(adapter.Deps{}).Bare(), smallCharge)
		free := s.full()
		var served atomic.Int64
		all := start(func() {
			for i := range 65 {
				s.serve(testproto.Msg{Op: testproto.OpEcho, Version: 1, ID: uint64(i + 1)})
				served.Add(1)
			}
		})
		synctest.Wait()
		if n := served.Load(); n != 64 || all.returned() {
			t.Fatalf("%d serves returned with the reply budget full, want 64, the 65th held in Admit", n)
		}
		free()
		for range 65 {
			if r := s.read(); r.Status != testproto.StatusOK {
				t.Errorf("Echo %d is answered %+v", r.ID, r)
			}
		}
		<-all.done
	})
}

// close never blocks, called from the test or from Unwrap on the reader,
// and wait returns only once the Echo waiting for its reply buffer has ended.
func TestCloseNeverBlocks(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := newSeam(t, testproto.New(adapter.Deps{}).Bare(), smallCharge)
		free := s.full()
		s.serve(testproto.Msg{Op: testproto.OpEcho, Version: 1, ID: 1})
		waited := start(func() { _ = s.p.Wait(context.Background()) })
		synctest.Wait()
		if waited.returned() {
			t.Fatal("wait returned while an Echo waits for its reply buffer")
		}
		s.p.Close()
		synctest.Wait()
		if !waited.returned() {
			t.Error("wait did not return once close ended the Echo")
		}
		free()
		if !s.sentNothing() {
			t.Error("the Echo close ended was answered on a closed connection")
		}
	})
	synctest.Test(t, func(t *testing.T) {
		var s *seam
		s = newSeam(t, edited{testproto.New(adapter.Deps{}), func(st *core.Stages) { st.Security = closesOnUnwrap{st.Security, func() { s.p.Close() }} }}, 1<<20)
		served := start(func() { s.serve(testproto.Msg{Op: testproto.OpEcho, Version: 1, ID: 1}) })
		synctest.Wait()
		if !served.returned() {
			t.Error("close called from Unwrap, on the reader, blocked serve")
		}
		if !s.sentNothing() {
			t.Error("a request on a closed connection was answered")
		}
	})
}

// closesOnUnwrap closes the pipeline from Unwrap, on the reader.
type closesOnUnwrap struct {
	core.Security
	close func()
}

func (c closesOnUnwrap) Unwrap(h core.Header, rec *resources.Buf, prev core.Header) (core.Header, error) {
	c.close()
	return c.Security.Unwrap(h, rec, prev)
}

// With the reply budget full, a header refused on the reader waits for its
// status's buffer on its own goroutine, keeping its record's receive charge:
// serve returns, a Cancel served next returns too, and the receive budget
// holds the record's charge until the status goes out once room is freed.
func TestAStatusWaitsOnItsOwnGoroutine(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := newSeam(t, testproto.New(adapter.Deps{}), smallCharge)
		free := s.full()
		badNext := body(s.f, testproto.Msg{Op: testproto.OpEcho, Version: 1, ID: 1})
		binary.BigEndian.PutUint32(badNext, 39)
		refused := start(func() { s.serveBody(badNext) })
		cancel := start(func() { s.serve(testproto.Msg{Op: testproto.OpCancel, Version: 2, Target: 9}) })
		synctest.Wait()
		if !refused.returned() || !cancel.returned() {
			t.Fatalf("serve returned %v for the refused header and %v for the Cancel; want both", refused.returned(), cancel.returned())
		}
		if s.budgets.Receive.Reserved() == 0 {
			t.Error("the refused header's record was released while its status waits")
		}
		free()
		if r := s.read(); r.ID != 1 || r.Status != testproto.StatusDecode {
			t.Errorf("the refused header is answered %+v", r)
		}
		synctest.Wait()
		if r := s.budgets.Receive.Reserved(); r != 0 {
			t.Errorf("the receive budget holds %d bytes after the status went out", r)
		}
	})
}

// With the reply budget full a second refused header waits on the reader
// until the first one's status goes out; a close during a wait releases the
// record, and wait returns only after.
func TestAConnectionHasOneStatusWait(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := newSeam(t, testproto.New(adapter.Deps{}), smallCharge)
		free := s.full()
		bad := func(id uint64) []byte {
			b := body(s.f, testproto.Msg{Op: testproto.OpEcho, Version: 1, ID: id, Session: 5})
			b[len(b)-1] ^= 0x01
			return b
		}
		s.serveBody(bad(1))
		second := start(func() { s.serveBody(bad(2)) })
		synctest.Wait()
		if second.returned() {
			t.Fatal("a second refused header was served while the first one's status waits")
		}
		free()
		if r := s.read(); r.ID != 1 {
			t.Fatalf("the first status out is %+v", r)
		}
		if r := s.read(); r.ID != 2 {
			t.Fatalf("the second status out is %+v", r)
		}
		<-second.done
		synctest.Wait() // the recorder releases a reply's buffer once written

		free = s.full()
		defer free()
		s.serveBody(bad(3))
		synctest.Wait()
		if s.budgets.Receive.Reserved() == 0 {
			t.Fatal("no record held while its status waits")
		}
		s.p.Close()
		if err := s.p.Wait(context.Background()); err != nil {
			t.Fatal(err)
		}
		if r := s.budgets.Receive.Reserved(); r != 0 {
			t.Errorf("after a close during the wait, wait returned with %d bytes of the record held", r)
		}
	})
}

// A Cancel ends the hold it names and is answered nothing.
func TestACancelIsAnsweredNothing(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := newSeam(t, testproto.New(adapter.Deps{}), 1<<20)
		s.serve(testproto.Msg{Op: testproto.OpCancel, Version: 2, Target: 9})
		if !s.sentNothing() {
			t.Error("a Cancel was answered")
		}
	})
}

// refusingFlow is a Flow that refuses every request with err: an input.
type refusingFlow struct{ err error }

func (f refusingFlow) Admit(context.Context, core.Header) (func(), error) { return nil, f.err }

var errUnmappable = errors.New("a refusal no row maps")

// logSink is a logger whose records, from debug up, the test reads back.
type logSink struct{ buf bytes.Buffer }

func (l *logSink) logger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(&l.buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

// at is the records logged at level.
func (l *logSink) at(t *testing.T, level string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(l.buf.String()), "\n") {
		if line == "" {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("a log line %q: %v", line, err)
		}
		if rec["level"] == level {
			out = append(out, rec)
		}
	}
	return out
}

// An error no row maps is answered the fault status, logged once at error
// with the request's trace ID, and counted.
func TestAnUnmappedErrorIsAnsweredTheFaultAndCounted(t *testing.T) {
	m, logs := metrics.New("v", "c"), &logSink{}
	s := newSeamWith(t, edited{testproto.New(adapter.Deps{}), func(st *core.Stages) { st.Flow = refusingFlow{errUnmappable} }}, 1<<20, adapter.Deps{Metrics: m, Logger: logs.logger()})
	s.serve(testproto.Msg{Op: testproto.OpEcho, Version: 1, ID: 1})
	if r := s.read(); r.Status != testproto.StatusFault {
		t.Errorf("an unmapped refusal is answered %+v, want the fault status", r)
	}
	errs := logs.at(t, "ERROR")
	if len(errs) != 1 || errs[0]["trace_id"] == "" || errs[0]["trace_id"] == strings.Repeat("0", 32) {
		t.Errorf("logged at error %v, want one line with the request's trace ID", errs)
	}
	got, _ := scrape(t, m)
	if k := "adapter_unmapped_errors_total{protocol=" + testproto.Name + "}"; got[k] != 1 {
		t.Errorf("%s = %v, want 1", k, got[k])
	}
}

// A wait the connection's close ends, for the request's reply reservation
// or for a replay copy's, is no error: the request counts neither as an error
// no row maps nor as a refusal, and logs nothing at error (RFC 20 §10.1).
func TestAWaitTheCloseEndsIsNoError(t *testing.T) {
	setup := []testproto.Msg{
		{Op: testproto.OpNegotiate, Version: 1, ID: 1},
		{Op: testproto.OpSession, Version: 1, ID: 2, Session: 7},
		{Op: testproto.OpTree, Version: 1, ID: 3, Session: 7, Tree: 1},
	}
	// A Create with no filehandle is refused, and its reply stored for its
	// sequence: the same Create again is answered from the replay cache.
	create := testproto.Msg{Op: testproto.OpCreate, Version: 1, ID: 4, Session: 7, Tree: 1, Seq: 1, Name: "f"}
	for _, c := range []struct {
		name  string
		first []testproto.Msg // served and answered before the pool fills
		waits testproto.Msg   // served with the pool full, then the close
		fill  func(s *seam, copies *resources.BufPool) func()
	}{
		{"the reply reservation", nil, testproto.Msg{Op: testproto.OpEcho, Version: 1, ID: 5},
			func(s *seam, _ *resources.BufPool) func() { return s.full() }},
		{"a replay copy", append(setup, create), create,
			func(s *seam, copies *resources.BufPool) func() {
				held, ok := copies.TryGet(peer, 0)
				if !ok {
					t.Fatal("the replay copies' budget is not free to fill")
				}
				return held.Release
			}},
	} {
		t.Run(c.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				m, logs := metrics.New("v", "c"), &logSink{}
				copies := resources.NewWaitBudget("reply", smallCharge)
				proto := testproto.New(adapter.Deps{Budgets: &resources.Budgets{Reply: copies}})
				s := newSeamWith(t, proto, smallCharge, adapter.Deps{Metrics: m, Logger: logs.logger()})
				for _, msg := range c.first {
					s.serve(msg)
					s.read()
				}
				synctest.Wait() // the last reply's buffer is given back after its send
				free := c.fill(s, resources.NewBufPool(copies))
				defer free()
				s.serve(c.waits)
				synctest.Wait()
				s.p.Close()
				if err := s.p.Wait(context.Background()); err != nil {
					t.Fatal(err)
				}
				got, _ := scrape(t, m)
				for _, k := range []string{
					"adapter_unmapped_errors_total{protocol=" + testproto.Name + "}",
					"adapter_refusals_total{protocol=" + testproto.Name + ",stage=replay_check}",
				} {
					if got[k] != 0 {
						t.Errorf("%s = %v after a wait the close ended, want 0", k, got[k])
					}
				}
				if e := logs.at(t, "ERROR"); len(e) != 0 {
					t.Errorf("a wait the close ended logged at error: %v", e)
				}
			})
		})
	}
}

// A connection at its cap closed while the next request waits in Admit, and
// the 64 admitted wait for their reply buffers: none of those waits is an
// error. Nothing counts as refused or as an error no row maps, and nothing
// logs at error.
func TestAnAdmissionTheCloseEndsIsNoError(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m, logs := metrics.New("v", "c"), &logSink{}
		s := newSeamWith(t, testproto.New(adapter.Deps{}).Bare(), smallCharge, adapter.Deps{Metrics: m, Logger: logs.logger()})
		free := s.full()
		defer free()
		var served atomic.Int64
		all := start(func() {
			for i := range 65 {
				s.serve(testproto.Msg{Op: testproto.OpEcho, Version: 1, ID: uint64(i + 1)})
				served.Add(1)
			}
		})
		synctest.Wait()
		if n := served.Load(); n != 64 {
			t.Fatalf("%d serves returned with the reply budget full, want 64, the 65th held in Admit", n)
		}
		s.p.Close()
		<-all.done
		if err := s.p.Wait(context.Background()); err != nil {
			t.Fatal(err)
		}
		got, _ := scrape(t, m)
		for _, k := range []string{
			"adapter_unmapped_errors_total{protocol=" + testproto.Name + "}",
			"adapter_refusals_total{protocol=" + testproto.Name + ",stage=admit}",
		} {
			if got[k] != 0 {
				t.Errorf("%s = %v after the close ended the waits, want 0", k, got[k])
			}
		}
		if e := logs.at(t, "ERROR"); len(e) != 0 {
			t.Errorf("the waits the close ended logged at error: %v", e)
		}
	})
}

// failingLookup wraps the stages' replay cache, whose Lookup it fails with
// err, after closing the connection where close is set.
type failingLookup struct {
	core.ReplayCache
	err   error
	close func()
}

func (l failingLookup) Lookup(context.Context, core.Header) (*resources.Buf, error) {
	if l.close != nil {
		l.close()
	}
	return nil, l.err
}

// Only a wait the close ended is no error. A cancel on a connection still
// open is answered, here the fault status of an error no row maps; and an
// error no row maps that meets the close is counted, as a bug is.
func TestOnlyAWaitTheCloseEndedIsNoError(t *testing.T) {
	unmapped := "adapter_unmapped_errors_total{protocol=" + testproto.Name + "}"
	t.Run("a cancel on an open connection", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			m := metrics.New("v", "c")
			s := newSeamWith(t, edited{testproto.New(adapter.Deps{}), func(st *core.Stages) { st.Replay = failingLookup{ReplayCache: st.Replay, err: context.Canceled} }}, 1<<20, adapter.Deps{Metrics: m})
			s.serve(testproto.Msg{Op: testproto.OpEcho, Version: 1, ID: 1})
			synctest.Wait()
			if got, _ := scrape(t, m); got[unmapped] != 1 {
				t.Fatalf("%s = %v, want 1: the request ended unanswered", unmapped, got[unmapped])
			}
			if r := s.read(); r.ID != 1 || r.Status != testproto.StatusFault {
				t.Errorf("a cancel on an open connection is answered %+v, want the fault status", r)
			}
		})
	})
	t.Run("an error no row maps at the close", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			m := metrics.New("v", "c")
			var s *seam
			closing := edited{testproto.New(adapter.Deps{}), func(st *core.Stages) {
				st.Replay = failingLookup{ReplayCache: st.Replay, err: errUnmappable, close: func() { s.p.Close() }}
			}}
			s = newSeamWith(t, closing, 1<<20, adapter.Deps{Metrics: m})
			s.serve(testproto.Msg{Op: testproto.OpEcho, Version: 1, ID: 1})
			if err := s.p.Wait(context.Background()); err != nil {
				t.Fatal(err)
			}
			if got, _ := scrape(t, m); got[unmapped] != 1 {
				t.Errorf("%s = %v for an error no row maps at the close, want 1", unmapped, got[unmapped])
			}
		})
	})
}

// A service error a row answers counts under its name, here the reply
// copy's delay that the stages' table maps, and logs at debug.
func TestAMappedServiceErrorCountsUnderItsName(t *testing.T) {
	m, logs := metrics.New("v", "c"), &logSink{}
	spent := &vfs.Delay{Cause: vfs.DelayResources}
	s := newSeamWith(t, edited{testproto.New(adapter.Deps{}), func(st *core.Stages) { st.Flow = refusingFlow{spent} }}, 1<<20, adapter.Deps{Metrics: m, Logger: logs.logger()})
	s.serve(testproto.Msg{Op: testproto.OpEcho, Version: 1, ID: 1})
	if r := s.read(); r.Status != testproto.StatusDelay {
		t.Errorf("a mapped delay is answered %+v", r)
	}
	got, _ := scrape(t, m)
	if k := "adapter_status_mapped_total{error=delay,protocol=" + testproto.Name + "}"; got[k] != 1 {
		t.Errorf("%s = %v, want 1", k, got[k])
	}
	if d := logs.at(t, "DEBUG"); len(d) != 1 || len(logs.at(t, "ERROR")) != 0 {
		t.Errorf("a mapped error logged %d debug lines and some at error, want one at debug", len(d))
	}
}

// Each refusal counts under the stage that refused it (RFC 20 §10.1's
// values).
func TestEachRefusalCountsUnderItsStage(t *testing.T) {
	m := metrics.New("v", "c")
	s := newSeamWith(t, testproto.New(adapter.Deps{}), 64<<10, adapter.Deps{Metrics: m})
	bad := body(s.f, testproto.Msg{Op: testproto.OpEcho, Version: 1, ID: 1, Session: 5})
	bad[len(bad)-1] ^= 0x01
	noRole := body(s.f, testproto.Msg{Op: testproto.OpEcho, Version: 1, ID: 5})
	for _, c := range []struct {
		stage string
		send  func()
	}{
		{"unwrap", func() { s.serveBody(bad) }},
		{"admit", func() { s.serve(testproto.Msg{Op: testproto.OpEcho, Version: 1, ID: 2, Credits: 9}) }},
		{"route", func() { s.serve(testproto.Msg{Op: testproto.OpEcho, Version: 3, ID: 3}) }},
		{"route", func() { s.serve(testproto.Msg{Op: 99, Version: 1, ID: 4}) }},
		{"decode_args", func() { s.serveBody(noRole[:len(noRole)-1]) }},
		{"reply_too_big", func() {
			s.serve(testproto.Msg{Op: testproto.OpEcho, Version: 1, ID: 6, Payload: make([]byte, 100<<10)})
		}},
	} {
		c.send()
		s.read()
	}
	s.p.Drain()
	s.serve(testproto.Msg{Op: testproto.OpEcho, Version: 1, ID: 7, Role: core.RoleNone})
	s.serve(testproto.Msg{Op: testproto.OpEcho, Version: 1, ID: 8, Role: core.RoleContext})
	s.read()
	badNext := body(s.f, testproto.Msg{Op: testproto.OpEcho, Version: 1, ID: 9})
	binary.BigEndian.PutUint32(badNext, 39)
	s.serveBody(badNext)
	s.read()
	if err := s.p.Wait(context.Background()); err != nil { // the drained Echo, answered nothing, has ended
		t.Fatal(err)
	}
	got, _ := scrape(t, m)
	for stage, want := range map[string]float64{"unwrap": 1, "admit": 1, "route": 2, "decode_args": 1, "reply_too_big": 1, "resolve_ids": 1, "decode_header": 1} {
		if k := "adapter_refusals_total{protocol=" + testproto.Name + ",stage=" + stage + "}"; got[k] != want {
			t.Errorf("%s = %v, want %v", k, got[k], want)
		}
	}
}

// adapter_requests_in_flight reads 64 when the 64th of 65 requests has been
// admitted, and zero when all are answered.
func TestInFlightFollowsTheRequests(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m := metrics.New("v", "c")
		s := newSeamWith(t, testproto.New(adapter.Deps{}).Bare(), smallCharge, adapter.Deps{Metrics: m})
		free := s.full()
		all := start(func() {
			for i := range 65 {
				s.serve(testproto.Msg{Op: testproto.OpEcho, Version: 1, ID: uint64(i + 1)})
			}
		})
		synctest.Wait()
		k := "adapter_requests_in_flight{protocol=" + testproto.Name + "}"
		if got, _ := scrape(t, m); got[k] != 64 {
			t.Errorf("%s = %v with 64 admitted, want 64", k, got[k])
		}
		free()
		for range 65 {
			s.read()
		}
		<-all.done
		synctest.Wait()
		if got, _ := scrape(t, m); got[k] != 0 {
			t.Errorf("%s = %v once all are answered, want 0", k, got[k])
		}
	})
}

// cachedReplay is a ReplayCache that answers every header from one copy: an
// input.
type cachedReplay struct {
	pool  *resources.BufPool
	reply []byte
}

func (c cachedReplay) Lookup(ctx context.Context, _ core.Header) (*resources.Buf, error) {
	b, err := c.pool.Get(ctx, peer, len(c.reply))
	if err != nil {
		return nil, err
	}
	b.B = append(b.B[:0], c.reply...)
	return b, nil
}

func (cachedReplay) Store(core.Header, *resources.Buf) {}

// On stages and a table of different labels, a header the codec refuses and
// a replay hit count under the stages', and a routed request's in-flight
// count moves to its table's at Route, the stages' back at zero.
func TestTheTwoLabels(t *testing.T) {
	relabel := func(st *core.Stages) { st.Protocol = "stages" }
	synctest.Test(t, func(t *testing.T) {
		m := metrics.New("v", "c")
		s := newSeamWith(t, edited{testproto.New(adapter.Deps{}).Bare(), relabel}, smallCharge, adapter.Deps{Metrics: m})
		free := s.full()
		s.serve(testproto.Msg{Op: testproto.OpEcho, Version: 1, ID: 1})
		synctest.Wait()
		got, _ := scrape(t, m)
		if got["adapter_requests_in_flight{protocol=stages}"] != 0 || got["adapter_requests_in_flight{protocol="+testproto.Name+"}"] != 1 {
			t.Errorf("a routed request in flight: stages %v, table %v; want 0 and 1", got["adapter_requests_in_flight{protocol=stages}"], got["adapter_requests_in_flight{protocol="+testproto.Name+"}"])
		}
		free()
		s.read()
		badNext := body(s.f, testproto.Msg{Op: testproto.OpEcho, Version: 1, ID: 2})
		binary.BigEndian.PutUint32(badNext, 39)
		s.serveBody(badNext)
		s.read()
		if got, _ := scrape(t, m); got["adapter_refusals_total{protocol=stages,stage=decode_header}"] != 1 {
			t.Errorf("a header the codec refused is not counted under the stages' label: %v", got)
		}
	})
	m := metrics.New("v", "c")
	replyPool := resources.NewBufPool(resources.NewWaitBudget("replay", 1<<20))
	cached := body(testproto.New(adapter.Deps{}).Framer(), testproto.Msg{Op: testproto.OpEcho, Version: 1, ID: 42})
	s := newSeamWith(t, edited{testproto.New(adapter.Deps{}).Bare(), func(st *core.Stages) {
		relabel(st)
		st.Replay = cachedReplay{replyPool, cached}
	}}, 1<<20, adapter.Deps{Metrics: m})
	s.serve(testproto.Msg{Op: testproto.OpEcho, Version: 1, ID: 42})
	s.read()
	if got, _ := scrape(t, m); got["adapter_replay_hits_total{protocol=stages}"] != 1 {
		t.Errorf("a replay hit is not counted under the stages' label: %v", got)
	}
}

// Each request has one span, ended, named after its operation once routed,
// with §10.2's keys alone: a served Echo its status, result and sizes, a
// refusal at Unwrap the stage that refused it.
func TestEachRequestHasOneSpan(t *testing.T) {
	spans := tracetest.NewSpanRecorder()
	tracer := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(spans)).Tracer("test")
	s := newSeamWith(t, testproto.New(adapter.Deps{}), 1<<20, adapter.Deps{Tracer: tracer, SampleRatio: 1})
	echo := body(s.f, testproto.Msg{Op: testproto.OpEcho, Version: 1, ID: 1, Payload: []byte("hi")})
	s.serveBody(echo)
	s.read()
	bad := body(s.f, testproto.Msg{Op: testproto.OpEcho, Version: 1, ID: 2, Session: 5})
	bad[len(bad)-1] ^= 0x01
	s.serveBody(bad)
	s.read()
	// A record whose header does not decode at all names no version, no
	// operation and no status: it is dropped rather than refused, and the
	// connection is closed, so no reply comes back.
	s.serveBody([]byte{0x01})
	s.finish()
	ended := spans.Ended()
	if len(ended) != 3 {
		t.Fatalf("%d spans ended for three requests", len(ended))
	}
	// The spans are found by what they carry, not by the order they ended in:
	// a request's span ends on its own goroutine, after its reply is queued.
	names, attrs := make([]string, len(ended)), make([]map[string]string, len(ended))
	for i, sd := range ended {
		names[i], attrs[i] = sd.Name(), map[string]string{}
		for _, kv := range sd.Attributes() {
			if !slices.Contains(spanKeys, string(kv.Key)) {
				t.Errorf("span %q carries %s, outside §10.2's keys", sd.Name(), kv.Key)
			}
			attrs[i][string(kv.Key)] = kv.Value.String()
		}
	}
	used := make([]bool, len(ended))
	pick := func(what string, match func(i int) bool) (string, map[string]string) {
		t.Helper()
		for i := range ended {
			if !used[i] && match(i) {
				used[i] = true
				return names[i], attrs[i]
			}
		}
		t.Fatalf("no span %s among %v", what, names)
		return "", nil
	}
	echoed := "adapter." + testproto.Name + ".Echo"
	name, served := pick("named after its Echo", func(i int) bool { return names[i] == echoed })
	if name != echoed || served["op"] != "echo" || served["version"] != "1" || served["status"] != "0" || served["result"] != "ok" ||
		served["request_bytes"] != strconv.Itoa(len(echo)) || served["reply_bytes"] != "42" || served["stage"] != "" {
		t.Errorf("the served Echo's span is %q %v", name, served)
	}
	name, refused := pick("refused at unwrap", func(i int) bool { return attrs[i]["stage"] == "unwrap" })
	if name != "adapter."+testproto.Name || refused["op"] != "" || refused["result"] != "" {
		t.Errorf("the refused message's span is %q %v, want its stage and no operation or result", name, refused)
	}
	// A header that does not decode is dropped, not refused: no operation, no
	// version and no status, which only a reply carries.
	name, undecoded := pick("dropped", func(i int) bool { return true })
	if name != "adapter."+testproto.Name || undecoded["version"] != "" || undecoded["op"] != "" || undecoded["status"] != "" {
		t.Errorf("the undecodable header's span is %q %v, want no version, op or status", name, undecoded)
	}
}

// A reply whose reservation waits is timed under reply_wait, between
// decode_args and resolve_ids: the wait's own sample, not the request's, and
// its length the time it waited.
func TestAReplyWaitIsTimed(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m := metrics.New("v", "c")
		s := newSeamWith(t, testproto.New(adapter.Deps{}).Bare(), smallCharge, adapter.Deps{Metrics: m})
		free := s.full()
		s.serve(testproto.Msg{Op: testproto.OpEcho, Version: 1, ID: 1})
		synctest.Wait()
		// The request is blocked in its reservation; the clock moves while it
		// waits, so a sample that says how long it waited says 50ms.
		time.Sleep(50 * time.Millisecond)
		free()
		s.read()
		got, _ := scrape(t, m)
		k := "adapter_wire_seconds{protocol=" + testproto.Name + ",stage=reply_wait}"
		if got[k] != 1 {
			t.Errorf("a reservation that waited observed %v reply_wait samples, want one", got[k])
		}
		if sum := got["adapter_wire_seconds_sum{protocol="+testproto.Name+",stage=reply_wait}"]; sum < 0.049 || sum > 0.051 {
			t.Errorf("the wait's samples sum to %v seconds, want 0.05", sum)
		}
	})
}

// A refusal before the service carries no result, even where its error is one
// the service's closed set names: the pipeline raised it, the service did not
// (RFC 20 §10.2).
func TestARefusalCarriesNoResult(t *testing.T) {
	spans := tracetest.NewSpanRecorder()
	tracer := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(spans)).Tracer("test")
	spent := &vfs.Delay{Cause: vfs.DelayResources}
	s := newSeamWith(t, edited{testproto.New(adapter.Deps{}), func(st *core.Stages) { st.Flow = refusingFlow{spent} }}, 1<<20, adapter.Deps{Tracer: tracer, SampleRatio: 1})
	s.serve(testproto.Msg{Op: testproto.OpEcho, Version: 1, ID: 1})
	s.read()
	s.finish()
	ended := spans.Ended()
	if len(ended) != 1 {
		t.Fatalf("%d spans ended for one refused request", len(ended))
	}
	got := map[string]string{}
	for _, kv := range ended[0].Attributes() {
		got[string(kv.Key)] = kv.Value.String()
	}
	if got["stage"] != "admit" || got["status"] == "" || got["result"] != "" {
		t.Errorf("a delay refused at admit carries %v, want its stage and status and no result", got)
	}
}

// A replay hit's span carries the sizes but no status and no result: the
// pipeline sends a copy whose status it never read, and no operation ran.
func TestAReplayHitsSpanCarriesNoStatus(t *testing.T) {
	spans := tracetest.NewSpanRecorder()
	tracer := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(spans)).Tracer("test")
	replyPool := resources.NewBufPool(resources.NewWaitBudget("replay", 1<<20))
	cached := body(testproto.New(adapter.Deps{}).Framer(), testproto.Msg{Op: testproto.OpEcho, Version: 1, ID: 42})
	s := newSeamWith(t, edited{testproto.New(adapter.Deps{}).Bare(), func(st *core.Stages) {
		st.Replay = cachedReplay{replyPool, cached}
	}}, 1<<20, adapter.Deps{Tracer: tracer, SampleRatio: 1})
	s.serve(testproto.Msg{Op: testproto.OpEcho, Version: 1, ID: 42})
	s.read()
	s.finish()
	ended := spans.Ended()
	if len(ended) != 1 {
		t.Fatalf("%d spans ended for one replay hit", len(ended))
	}
	got := map[string]string{}
	for _, kv := range ended[0].Attributes() {
		got[string(kv.Key)] = kv.Value.String()
	}
	if got["reply_bytes"] == "" || got["status"] != "" || got["result"] != "" || got["op"] != "" {
		t.Errorf("a replay hit's span is %v, want its reply's size alone", got)
	}
}

// Every line logged for a request carries its trace ID, with a tracer or
// none; a second refusal at Unwrap on one connection logs nothing more; a
// mapped refusal logs at debug.
func TestLogLinesCarryTheTraceID(t *testing.T) {
	spans := tracetest.NewSpanRecorder()
	tracer := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(spans)).Tracer("test")
	for name, c := range map[string]struct {
		d    adapter.Deps
		span bool
	}{
		"a tracer":               {adapter.Deps{Tracer: tracer, SampleRatio: 1}, true},
		"no tracer":              {adapter.Deps{}, false},
		"a tracer drawing no ID": {adapter.Deps{Tracer: noop.NewTracerProvider().Tracer("test"), SampleRatio: 1}, false},
		"a tracer sampling none": {adapter.Deps{Tracer: tracer}, false},
	} {
		t.Run(name, func(t *testing.T) {
			d := c.d
			logs := &logSink{}
			d.Logger = logs.logger()
			s := newSeamWith(t, testproto.New(adapter.Deps{}), 1<<20, d)
			for id := range uint64(2) {
				bad := body(s.f, testproto.Msg{Op: testproto.OpEcho, Version: 1, ID: id, Session: 5})
				bad[len(bad)-1] ^= 0x01
				s.serveBody(bad)
				s.read()
			}
			s.serve(testproto.Msg{Op: testproto.OpEcho, Version: 3, ID: 3})
			s.read()
			s.finish()
			traced := map[string]bool{}
			for _, sp := range spans.Ended() {
				traced[sp.SpanContext().TraceID().String()] = true
			}
			warns, debugs := logs.at(t, "WARN"), logs.at(t, "DEBUG")
			if len(warns) != 1 {
				t.Errorf("two refusals at Unwrap logged %d warnings, want one per connection", len(warns))
			}
			if len(debugs) != 1 {
				t.Errorf("a mapped refusal logged %d debug lines, want one", len(debugs))
			}
			for _, rec := range append(warns, debugs...) {
				if id, _ := rec["trace_id"].(string); len(id) != 32 || id == strings.Repeat("0", 32) {
					t.Errorf("a line logged for a request carries trace ID %q", id)
				}
				id, has := rec["span_id"].(string)
				if c.span && (!has || len(id) != 16) {
					t.Errorf("a line logged under a span carries span ID %q", id)
				}
				if c.span && !traced[rec["trace_id"].(string)] {
					t.Errorf("a line logged under a span carries trace ID %v, not its span's", rec["trace_id"])
				}
				if !c.span {
					if _, ok := rec["span_id"]; ok {
						t.Errorf("a line logged with no span carries span_id %q", id)
					}
				}
			}
		})
	}
}

// The row's checks, each alone: an unknown code, whose arguments do not
// decode, and an Echo switched off by name answered not supported, beside a
// Negotiate served; Park in version 1 the version status; then, as the
// connection reaches each level, Tree before a session, PutFH before a tree
// and Create with no filehandle their own statuses, and Create before a
// session the session status, not the filehandle's.
func TestTheRowChecks(t *testing.T) {
	// Switched off in capitals, which matches Echo's row as the op label's
	// echo does: names match case aside, on both sides.
	off := testproto.Settings{Adapter: settings.Adapter{DisabledOperations: []string{"ECHO"}}}
	s := newSeamSet(t, testproto.New(adapter.Deps{}), 1<<20, adapter.Deps{}, off)
	noRole := body(s.f, testproto.Msg{Op: testproto.OpEcho, Version: 1, ID: 2})
	garbage := body(s.f, testproto.Msg{Op: 99, Version: 1, ID: 1})
	for _, c := range []struct {
		name string
		send func()
		want core.Status
	}{
		{"an unknown code with arguments that do not decode", func() { s.serveBody(append(garbage, 0xff)) }, testproto.StatusNotSupported},
		{"a switched-off Echo, its arguments never decoded", func() { s.serveBody(noRole[:len(noRole)-1]) }, testproto.StatusNotSupported},
		{"Park in version 1", func() { s.serve(testproto.Msg{Op: testproto.OpPark, Version: 1, ID: 3}) }, testproto.StatusVersion},
		{"Create before a session", func() { s.serve(testproto.Msg{Op: testproto.OpCreate, Version: 1, ID: 4, Name: "f"}) }, testproto.StatusNotNegotiated},
		{"Negotiate", func() { s.serve(testproto.Msg{Op: testproto.OpNegotiate, Version: 1, ID: 5}) }, testproto.StatusOK},
		{"Create before a session, negotiated", func() { s.serve(testproto.Msg{Op: testproto.OpCreate, Version: 1, ID: 6, Session: 7, Name: "f"}) }, testproto.StatusNoSession},
		{"Tree before a session", func() { s.serve(testproto.Msg{Op: testproto.OpTree, Version: 1, ID: 7, Session: 7}) }, testproto.StatusNoSession},
		{"Session", func() { s.serve(testproto.Msg{Op: testproto.OpSession, Version: 1, ID: 8, Session: 7}) }, testproto.StatusOK},
		{"PutFH before a tree", func() {
			s.serve(testproto.Msg{Op: testproto.OpPutFH, Version: 1, ID: 9, Session: 7, Handle: []byte("h")})
		}, testproto.StatusNoTree},
		{"Tree", func() { s.serve(testproto.Msg{Op: testproto.OpTree, Version: 1, ID: 10, Session: 7, Tree: 1}) }, testproto.StatusOK},
		{"Create with a tree and no filehandle", func() {
			s.serve(testproto.Msg{Op: testproto.OpCreate, Version: 1, ID: 11, Session: 7, Tree: 1, Name: "f"})
		}, testproto.StatusNoFH},
	} {
		c.send()
		if r := s.read(); r.Status != c.want {
			t.Errorf("%s: answered %d, want %d", c.name, r.Status, c.want)
		}
	}
}

// On an expired session Tree is answered the expired status, Session and
// Logoff run, and an Echo runs only when its argument says release: one that
// says flush, which the drain serves, is refused.
func TestAnExpiredCredential(t *testing.T) {
	s := newSeam(t, testproto.New(adapter.Deps{}), 1<<20)
	ended := time.Now().Add(-time.Hour)
	for _, c := range []struct {
		name string
		m    testproto.Msg
		want core.Status
	}{
		{"Negotiate", testproto.Msg{Op: testproto.OpNegotiate, Version: 1, ID: 1}, testproto.StatusOK},
		{"a session whose credential has ended", testproto.Msg{Op: testproto.OpSession, Version: 1, ID: 2, Session: 7, Expires: ended}, testproto.StatusOK},
		{"Tree", testproto.Msg{Op: testproto.OpTree, Version: 1, ID: 3, Session: 7, Tree: 1}, testproto.StatusExpired},
		{"an Echo that says flush", testproto.Msg{Op: testproto.OpEcho, Version: 1, ID: 4, Session: 7, Role: core.RoleFlush}, testproto.StatusExpired},
		{"an Echo that says none", testproto.Msg{Op: testproto.OpEcho, Version: 1, ID: 5, Session: 7, Role: core.RoleNone}, testproto.StatusExpired},
		{"an Echo that says release", testproto.Msg{Op: testproto.OpEcho, Version: 1, ID: 6, Session: 7, Role: core.RoleRelease}, testproto.StatusOK},
		{"Session again", testproto.Msg{Op: testproto.OpSession, Version: 1, ID: 7, Session: 7, Expires: ended}, testproto.StatusOK},
		{"Logoff", testproto.Msg{Op: testproto.OpLogoff, Version: 1, ID: 8, Session: 7}, testproto.StatusOK},
	} {
		s.serve(c.m)
		if r := s.read(); r.Status != c.want {
			t.Errorf("%s: answered %d, want %d", c.name, r.Status, c.want)
		}
	}
}

// wrapped is a Router whose tables' rows hand each Invocation they decode to
// wrap: a pass-through the test reads where the call itself is what is
// checked. It counts the Routes it is asked. With fail set, every row's
// Decode answers fail instead: a codec declared in the test, an input
// standing for arguments that do not decode.
type wrapped struct {
	core.Router
	wrap   func(core.Invocation) core.Invocation
	fail   error
	routes atomic.Int64
	mu     sync.Mutex
	tables map[*core.OpTable]*core.OpTable
}

func (w *wrapped) Route(h core.Header, prev *core.Request) (*core.OpTable, *core.Request, error) {
	w.routes.Add(1)
	tb, r, err := w.Router.Route(h, prev)
	if tb == nil {
		return tb, r, err
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.tables == nil {
		w.tables = map[*core.OpTable]*core.OpTable{}
	}
	cp, ok := w.tables[tb]
	if !ok {
		c := *tb
		c.Ops = make(map[uint32]*core.Op, len(tb.Ops))
		for code, op := range tb.Ops {
			o := *op
			dec := op.Decode
			o.Decode = func(b []byte, v core.Version) (core.Invocation, int, error) {
				if w.fail != nil {
					return core.Invocation{}, 0, w.fail
				}
				inv, n, err := dec(b, v)
				if err == nil {
					inv = w.wrap(inv)
				}
				return inv, n, err
			}
			c.Ops[code] = &o
		}
		cp = &c
		w.tables[tb] = cp
	}
	return cp, r, err
}

// wrapRows is a protocol whose routers wrap their rows' invocations with f;
// each connection's router is sent on routers.
func wrapRows(p core.Protocol, f func(core.Invocation) core.Invocation, routers chan<- *wrapped) edited {
	return edited{p, func(st *core.Stages) {
		w := &wrapped{Router: st.Router, wrap: f}
		st.Router = w
		if routers != nil {
			routers <- w
		}
	}}
}

// The role is asked only while draining: a request served normally pays for
// no call of Invocation.Role.
func TestTheRoleIsAskedOnlyWhileDraining(t *testing.T) {
	var calls atomic.Int64
	count := func(inv core.Invocation) core.Invocation {
		if role := inv.Role; role != nil {
			inv.Role = func(r *core.Request) core.Role {
				calls.Add(1)
				return role(r)
			}
		}
		return inv
	}
	s := newSeam(t, wrapRows(testproto.New(adapter.Deps{}).Bare(), count, nil), 1<<20)
	s.serve(testproto.Msg{Op: testproto.OpEcho, Version: 1, ID: 1, Role: core.RoleContext})
	s.read()
	if n := calls.Load(); n != 0 {
		t.Errorf("serving, the role was asked %d times, want none", n)
	}
	s.p.Drain()
	s.serve(testproto.Msg{Op: testproto.OpEcho, Version: 1, ID: 2, Role: core.RoleContext})
	s.read()
	if n := calls.Load(); n != 1 {
		t.Errorf("draining, the role was asked %d times, want once", n)
	}
}

// A request keeps its place under the cap through its call: with 65 Echo in
// the service, 64 are in their calls and the 65th waits in Admit.
func TestTheCapHoldsThroughTheCall(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var in atomic.Int64
		gate := make(chan struct{})
		hold := func(inv core.Invocation) core.Invocation {
			call := inv.Call
			inv.Call = func(ctx context.Context, r *core.Request) error {
				in.Add(1)
				<-gate
				return call(ctx, r)
			}
			return inv
		}
		s := newSeam(t, wrapRows(testproto.New(adapter.Deps{}).Bare(), hold, nil), 1<<30)
		all := start(func() {
			for i := range 65 {
				s.serve(testproto.Msg{Op: testproto.OpEcho, Version: 1, ID: uint64(i + 1)})
			}
		})
		synctest.Wait()
		if n := in.Load(); n != 64 || all.returned() {
			t.Fatalf("%d requests in their calls at once, want 64 with the 65th held in Admit", n)
		}
		close(gate)
		for range 65 {
			s.read()
		}
		<-all.done
	})
}

// routesWith is a Router that chooses the header's table and refuses with
// err: a refusal that comes back with its table.
type routesWith struct {
	core.Router
	err error
}

func (r routesWith) Route(h core.Header, prev *core.Request) (*core.OpTable, *core.Request, error) {
	tb, _, _ := r.Router.Route(h, prev)
	return tb, nil, r.err
}

// A refusal Route returns with its table is mapped by that table: no
// session, its no-session status, which the stages' own table does not map.
func TestARouteRefusalIsMappedByItsTable(t *testing.T) {
	s := newSeam(t, edited{testproto.New(adapter.Deps{}).Bare(), func(st *core.Stages) { st.Router = routesWith{st.Router, core.ErrNoSession} }}, 1<<20)
	s.serve(testproto.Msg{Op: testproto.OpEcho, Version: 1, ID: 1})
	if r := s.read(); r.Status != testproto.StatusNoSession {
		t.Errorf("a refusal with its table is answered %d, want the table's no-session status %d", r.Status, testproto.StatusNoSession)
	}
}

// A request admitted after close is not run: no Route, no call, no reply.
func TestARequestAfterCloseIsNotRun(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		routers := make(chan *wrapped, 1)
		var s *seam
		closing := edited{wrapRows(testproto.New(adapter.Deps{}), func(inv core.Invocation) core.Invocation { return inv }, routers), func(st *core.Stages) {
			st.Security = closesOnUnwrap{st.Security, func() { s.p.Close() }}
		}}
		s = newSeam(t, closing, 1<<20)
		w := <-routers
		s.serve(testproto.Msg{Op: testproto.OpEcho, Version: 1, ID: 1})
		synctest.Wait()
		if n := w.routes.Load(); n != 0 {
			t.Errorf("a request admitted after close was routed %d times", n)
		}
		if !s.sentNothing() {
			t.Error("a request admitted after close was answered")
		}
	})
}

// close called from a handler's own call returns: it never waits for the
// requests it ends.
func TestCloseFromAHandlerReturns(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var s *seam
		closes := func(inv core.Invocation) core.Invocation {
			call := inv.Call
			inv.Call = func(ctx context.Context, r *core.Request) error {
				s.p.Close()
				return call(ctx, r)
			}
			return inv
		}
		s = newSeam(t, wrapRows(testproto.New(adapter.Deps{}).Bare(), closes, nil), 1<<20)
		s.serve(testproto.Msg{Op: testproto.OpEcho, Version: 1, ID: 1})
		if err := s.p.Wait(context.Background()); err != nil {
			t.Fatal(err)
		}
		if !s.sentNothing() {
			t.Error("the reply of a request in its call when close came was sent")
		}
	})
}

// With the reply pool held full, a drained request whose table holds still
// gets its interim, its credit granted, though a request waits on the pool
// for the reservation it gives back.
func TestTheInterimGoesOutUnderContention(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := newSeam(t, testproto.New(adapter.Deps{}), 2*smallCharge)
		freeA, freeB := s.full(), s.full()
		s.p.Drain()
		for _, m := range []testproto.Msg{
			{Op: testproto.OpEcho, Version: 1, ID: 1, Role: core.RoleContext},
			{Op: testproto.OpEcho, Version: 2, ID: 2, Role: core.RoleNone, Credits: 1},
			{Op: testproto.OpEcho, Version: 1, ID: 3, Role: core.RoleContext},
		} {
			s.serve(m)
			synctest.Wait() // each waits on the pool in turn
		}
		freeA()
		freeB()
		synctest.Wait() // ID 2's interim queued before any reply is read
		got, order := map[uint64]testproto.Reply{}, []uint64{}
		for range 3 {
			r := s.read()
			got[r.ID&^(1<<63)] = r
			order = append(order, r.ID&^(1<<63))
		}
		if r, ok := got[2]; !ok || !r.Interim || r.Credits != 1 {
			t.Errorf("the drained request got %+v, want an interim granting its credit", r)
		}
		// The interim goes out in the reservation its request already held,
		// so it never waits behind ID 3 for the room that request gave back.
		if slices.Index(order, 2) > slices.Index(order, 3) {
			t.Errorf("the replies came %v: want the interim before ID 3's reply", order)
		}
	})
}

// A header that does not decode closes the connection with ErrDecode wrapping
// the codec's own error once: its text kept, and the cause not matched.
func TestAHeaderDecodeErrorWrapsItsCauseOnce(t *testing.T) {
	proto := testproto.New(adapter.Deps{}).Bare()
	s := newSeam(t, proto, 1<<20)
	short := []byte{1, 2, 3}
	rec, err := s.recv.Get(context.Background(), peer, len(short))
	if err != nil {
		t.Fatal(err)
	}
	rec.B = append(rec.B[:0], short...)
	h, cause := proto.NewConn(nil).Header.Decode(rec, 0)
	rec.Release()
	if h != nil || cause == nil || errors.Is(cause, core.ErrDecode) {
		t.Fatalf("the codec decoded %v, %v: want no header and its own error", h, cause)
	}
	s.serveBody(short)
	err = <-s.closed
	if !errors.Is(err, core.ErrDecode) || errors.Unwrap(err) != core.ErrDecode || err.Error() != core.ErrDecode.Error()+": "+cause.Error() {
		t.Errorf("the connection closed with %v, want ErrDecode wrapping %q once", err, cause)
	}
}

// faultyHeader is a HeaderCodec declared in the test whose Decode answers
// each header it decodes with fail: an input standing for a header whose
// fields decoded and whose rest did not.
type faultyHeader struct {
	core.HeaderCodec
	fail error
}

func (f faultyHeader) Decode(rec *resources.Buf, off int) (core.Header, error) {
	h, err := f.HeaderCodec.Decode(rec, off)
	if err == nil {
		err = f.fail
	}
	return h, err
}

// A header's or a row's Decode error that wraps ErrVersion, on tables whose
// ErrVersion row comes before ErrDecode's, is answered the decode status:
// the cause is not matched.
func TestADecodeErrorsCauseIsNotMatched(t *testing.T) {
	cause := fmt.Errorf("a field of another version: %w", core.ErrVersion)
	for _, c := range []struct {
		name string
		edit func(*core.Stages)
	}{
		{"a header's", func(st *core.Stages) {
			st.Header = faultyHeader{st.Header, cause}
			st.Errors = core.NewErrorTable(testproto.StatusFault,
				core.ErrorRow{Err: core.ErrVersion, Status: testproto.StatusVersion},
				core.ErrorRow{Err: core.ErrDecode, Status: testproto.StatusDecode})
		}},
		{"a row's", func(st *core.Stages) { st.Router = &wrapped{Router: st.Router, fail: cause} }},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := newSeam(t, edited{testproto.New(adapter.Deps{}).Bare(), c.edit}, 1<<20)
			s.serve(testproto.Msg{Op: testproto.OpEcho, Version: 1, ID: 1})
			if r := s.read(); r.Status != testproto.StatusDecode {
				t.Errorf("the request is answered %+v, want the decode status", r)
			}
		})
	}
}

// A refusal served after close, waiting for a status's room, is never
// counted as running: the pipeline's close is checked once it counts, so a
// wait for its runs is never handed one that starts later.
func TestARefusalAfterCloseIsNotRun(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var s *seam
		closing := edited{testproto.New(adapter.Deps{}), func(st *core.Stages) {
			st.Security = closesOnUnwrap{st.Security, func() { s.p.Close() }}
		}}
		s = newSeam(t, closing, smallCharge)
		free := s.full()
		defer free()
		bad := body(s.f, testproto.Msg{Op: testproto.OpEcho, Version: 1, ID: 1, Session: 5})
		bad[len(bad)-1] ^= 0x01
		for range 64 {
			s.serveBody(bad)
			if n := s.p.Runs(); n != 0 {
				t.Fatalf("after close a refusal counts %d as running, want none", n)
			}
		}
	})
}

// lastVersion is a Router declared in the test whose tables' Echo row ends
// at version 1: an input standing for a row a newer version withdraws.
type lastVersion struct {
	core.Router
	mu     sync.Mutex
	tables map[*core.OpTable]*core.OpTable
}

func (l *lastVersion) Route(h core.Header, prev *core.Request) (*core.OpTable, *core.Request, error) {
	tb, r, err := l.Router.Route(h, prev)
	if tb == nil {
		return tb, r, err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.tables == nil {
		l.tables = map[*core.OpTable]*core.OpTable{}
	}
	cp, ok := l.tables[tb]
	if !ok {
		c := *tb
		c.Ops = make(map[uint32]*core.Op, len(tb.Ops))
		for code, op := range tb.Ops {
			o := *op
			if code == testproto.OpEcho {
				o.Versions.Last = 1
			}
			c.Ops[code] = &o
		}
		cp = &c
		l.tables[tb] = cp
	}
	return cp, r, err
}

// A row whose versions end below the request's is refused the version status.
func TestARowPastItsLastVersionIsRefused(t *testing.T) {
	s := newSeam(t, edited{testproto.New(adapter.Deps{}), func(st *core.Stages) { st.Router = &lastVersion{Router: st.Router} }}, 1<<20)
	s.serve(testproto.Msg{Op: testproto.OpNegotiate, Version: 2, ID: 1})
	if r := s.read(); r.Status != testproto.StatusOK {
		t.Fatalf("negotiating version 2: %+v", r)
	}
	s.serve(testproto.Msg{Op: testproto.OpEcho, Version: 2, ID: 2})
	if r := s.read(); r.Status != testproto.StatusVersion {
		t.Errorf("an Echo in version 2, its row ending at 1, is answered %d, want the version status", r.Status)
	}
}
