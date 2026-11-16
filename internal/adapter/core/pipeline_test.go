package core_test

import (
	"context"
	"encoding/binary"
	"errors"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/cubbit/dittofs/internal/adapter/adaptertest/testproto"
	"github.com/cubbit/dittofs/internal/adapter/core"
	"github.com/cubbit/dittofs/pkg/adapter"
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
}

// newSeam builds a seam for proto's stages, its reply budget of reply bytes.
func newSeam(t *testing.T, proto core.Protocol, reply int64) *seam {
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
	s.p = core.NewPipeline(core.NewShared(adapter.Deps{}, s.reply), st, context.Background(), peer, out, func(err error) {
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

// A message whose Unwrap returns no header closes the connection with
// Unwrap's error, and nothing is sent.
func TestUnwrapsNilHeaderClosesTheConnection(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := newSeam(t, edited{testproto.New(adapter.Deps{}), func(st *core.Stages) { st.Security = undecryptable{st.Security} }}, 1<<20)
		s.serve(testproto.Msg{Op: testproto.OpEcho, Version: 1, ID: 1})
		if err := <-s.closed; !errors.Is(err, errUndecryptable) {
			t.Errorf("the connection closed with %v, want Unwrap's error", err)
		}
		if !s.sentNothing() {
			t.Error("a message Unwrap said close for was answered")
		}
	})
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

// A bad checksum is refused before its credits are charged: the Echo after
// it, charging the connection's one credit, is served.
func TestABadChecksumIsRefusedBeforeAdmit(t *testing.T) {
	s := newSeam(t, testproto.New(adapter.Deps{}), 1<<20)
	bad := body(s.f, testproto.Msg{Op: testproto.OpEcho, Version: 1, ID: 1, Session: 5, Credits: 1})
	bad[len(bad)-1] ^= 0x01
	s.serveBody(bad)
	if r := s.read(); r.Status != testproto.StatusChecksum {
		t.Fatalf("a bad checksum is answered %+v", r)
	}
	s.serve(testproto.Msg{Op: testproto.OpEcho, Version: 1, ID: 2, Credits: 1})
	if r := s.read(); r.Status != testproto.StatusOK {
		t.Errorf("the Echo after it is answered %+v: the refused message's credits were charged", r)
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

// A record that chains a second message closes the connection, answered
// nothing, until every member of a chain is served.
func TestAChainedRecordClosesTheConnection(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := newSeam(t, testproto.New(adapter.Deps{}).Bare(), 1<<20)
		s.serve(testproto.Msg{Op: testproto.OpEcho, Version: 1, ID: 1}, testproto.Msg{Op: testproto.OpEcho, Version: 1, ID: 2})
		if err := <-s.closed; !errors.Is(err, core.ErrChain) {
			t.Errorf("a chained record closed the connection with %v", err)
		}
		if !s.sentNothing() {
			t.Error("a chained record was answered")
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

// wrapped is a Router whose tables' rows hand each Invocation they decode to
// wrap: a pass-through the test reads where the call itself is what is
// checked. It counts the Routes it is asked.
type wrapped struct {
	core.Router
	wrap   func(core.Invocation) core.Invocation
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
		got := map[uint64]testproto.Reply{}
		for range 3 {
			r := s.read()
			got[r.ID&^(1<<63)] = r
		}
		if r, ok := got[2]; !ok || !r.Interim || r.Credits != 1 {
			t.Errorf("the drained request got %+v, want an interim granting its credit", r)
		}
	})
}
