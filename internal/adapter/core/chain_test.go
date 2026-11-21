package core_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
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
	"go.opentelemetry.io/otel/trace"

	"github.com/cubbit/dittofs/internal/adapter/adaptertest/testproto"
	"github.com/cubbit/dittofs/internal/adapter/core"
	"github.com/cubbit/dittofs/pkg/adapter"
	"github.com/cubbit/dittofs/pkg/metrics"
	"github.com/cubbit/dittofs/pkg/resources"
	"github.com/cubbit/dittofs/pkg/vfs"
	"github.com/cubbit/dittofs/pkg/vfs/vfstest"
	"github.com/cubbit/dittofs/pkg/vfs/vfstest/memory"
)

// inherited is a related member's session: the one of the member before it.
const inherited = 0xffffffff

// newChained is newServed in version 2, the version that chains: a
// connection negotiated, with session 7 and tree 1, over memory.New with
// faults.
func newChained(t *testing.T, reply int64, d adapter.Deps, edit ...func(*core.Stages)) *served {
	t.Helper()
	return newChainedOn(t, reply, d, func(p core.Protocol) core.Protocol {
		for _, e := range edit {
			p = edited{p, e}
		}
		return p
	})
}

// newChainedOn is newChained on the protocol wrap makes of testproto's.
func newChainedOn(t *testing.T, reply int64, d adapter.Deps, wrap func(core.Protocol) core.Protocol) *served {
	t.Helper()
	svc := memory.New(t)
	root, _, err := svc.Root(t.Context(), svc.Admin(), vfs.ShareRef{Name: svc.Store.Share.Name})
	if err != nil {
		t.Fatal(err)
	}
	faults := vfstest.Faults(svc)
	d.VFS = faults
	tp := testproto.New(d).WithCall(svc.Admin())
	proto := wrap(tp)
	s := &served{seam: newSeamWith(t, proto, reply, d), svc: svc, faults: faults, root: root, proto: proto, tp: tp}
	s.setUp(7)
	return s
}

// another is a second connection of s's server, from remote, on version 2
// with session and tree 1.
func (s *served) another(remote netip.Addr, session uint32) *served {
	s.t.Helper()
	o := &served{seam: s.seam.another(s.proto, remote), svc: s.svc, faults: s.faults, root: s.root, proto: s.proto}
	o.setUp(session)
	return o
}

// setUp negotiates version 2 and sets up session and tree 1.
func (s *served) setUp(session uint32) {
	s.t.Helper()
	for _, m := range []testproto.Msg{
		{Op: testproto.OpNegotiate, Version: 2},
		{Op: testproto.OpSession, Version: 2, Session: session},
		{Op: testproto.OpTree, Version: 2, Session: session, Tree: 1},
	} {
		s.serve(m)
		if r := s.read(); r.Status != testproto.StatusOK {
			s.t.Fatalf("setting up the connection: %+v", r)
		}
	}
}

// A version 2 chain of a PutFH and a related Create makes the file under the
// handle the first set, both replies in one record whose checksums verify.
func TestARelatedChainRunsUnderItsFirstMembersHandle(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := newChained(t, 1<<20, adapter.Deps{})
		s.serve(
			testproto.Msg{Op: testproto.OpPutFH, Version: 2, ID: 10, Session: 7, Tree: 1, Handle: s.root},
			testproto.Msg{Op: testproto.OpCreate, Version: 2, ID: 11, Session: inherited, Tree: 1, Name: "a", Related: true},
		)
		rs, err := s.client.Recv()
		if err != nil || len(rs) != 2 || rs[0].Status != testproto.StatusOK || rs[1].Status != testproto.StatusOK || s.creates() != 1 {
			t.Errorf("the chain is answered %+v, %v, with %d files made; want one record of two successes and the file", rs, err, s.creates())
		}
	})
}

// With the PutFH refused, the related Create is answered the PutFH's status
// with no call, in the same record, and makes no file.
func TestARelatedMemberAfterAFailureIsAnsweredItsStatus(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := newChained(t, 1<<20, adapter.Deps{})
		s.serve(
			testproto.Msg{Op: testproto.OpPutFH, Version: 2, ID: 20, Session: 7, Handle: s.root},
			testproto.Msg{Op: testproto.OpCreate, Version: 2, ID: 21, Session: inherited, Tree: 1, Name: "b", Related: true},
		)
		rs, err := s.client.Recv()
		if err != nil || len(rs) != 2 || rs[0].Status != testproto.StatusNoTree || rs[1].Status != testproto.StatusNoTree || s.creates() != 0 {
			t.Errorf("the chain is answered %+v, %v, with %d files made; want the PutFH's no-tree status twice and no file", rs, err, s.creates())
		}
	})
}

// A related member after one answered a NotFailure row's status runs: a
// PutFH, a related Echo answered it and a related Create make the file.
func TestARelatedMemberRunsPastANotFailureStatus(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := newChained(t, 1<<20, adapter.Deps{})
		s.serve(
			testproto.Msg{Op: testproto.OpPutFH, Version: 2, ID: 30, Session: 7, Tree: 1, Handle: s.root},
			testproto.Msg{Op: testproto.OpEcho, Version: 2, ID: 31, Session: inherited, Tree: 1, Payload: []byte(testproto.NotFailure), Related: true},
			testproto.Msg{Op: testproto.OpCreate, Version: 2, ID: 32, Session: inherited, Tree: 1, Name: "c", Related: true},
		)
		rs, err := s.client.Recv()
		if err != nil || len(rs) != 3 || rs[0].Status != testproto.StatusOK || rs[1].Status != testproto.StatusNotFailure || rs[2].Status != testproto.StatusOK || s.creates() != 1 {
			t.Errorf("the chain is answered %+v, %v, with %d files made; want the NotFailure status, then the Create run and its file", rs, err, s.creates())
		}
	})
}

// chainOf is the body of a record chaining msgs, as the seam serves it, and
// the offset of each member's header in it.
func chainOf(f core.Framer, msgs ...testproto.Msg) ([]byte, []int) {
	b := testproto.Append(nil, msgs...)[f.HeaderLen():]
	offs := []int{0}
	for off := 0; ; {
		next := int(binary.BigEndian.Uint32(b[off:]))
		if next == 0 {
			return b, offs
		}
		offs = append(offs, next)
		off = next
	}
}

// echoIn is a version 2 Echo under session, related to the member before
// where session is inherited.
func echoIn(id uint64, session uint32) testproto.Msg {
	return testproto.Msg{Op: testproto.OpEcho, Version: 2, ID: id, Session: session, Related: session == inherited, Payload: []byte("xy")}
}

// headerLen is testproto's message header.
const headerLen = 40

// recvOne reads the next record and wants it to hold one reply.
func (s *served) recvOne() testproto.Reply {
	s.t.Helper()
	rs, err := s.client.Recv()
	if err != nil || len(rs) != 1 {
		s.t.Fatalf("a record of %d replies, %v: want one", len(rs), err)
	}
	return rs[0]
}

// A chain whose second member, unrelated and under another session, fails
// its checksum goes out in three records, counted unwrap: the first
// member's reply sealed under its session, the refusal signed under the
// second's and not sealed, and the third's reply sealed under the first's.
// Every member's credit comes back.
func TestAnUnrelatedMemberThatUnwrapRefusesSplitsTheChain(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m := metrics.New("v", "c")
		s := newChained(t, 1<<20, adapter.Deps{Metrics: m})
		s.serve(testproto.Msg{Op: testproto.OpSession, Version: 2, Session: 9})
		if r := s.read(); r.Status != testproto.StatusOK {
			t.Fatalf("session 9: %+v", r)
		}
		before := s.client.Balance()
		b, offs := chainOf(s.f, echoIn(30, 7), echoIn(31, 9), echoIn(32, 7))
		b[offs[1]+headerLen+1] ^= 1 // a byte of the second's payload
		s.serveBody(b)
		r1, r2, r3 := s.recvOne(), s.recvOne(), s.recvOne()
		if r1.ID != 30 || r1.Status != testproto.StatusOK || r1.Session != 7 || r1.Checksum == 0 || r1.Signed {
			t.Errorf("the first record is %+v, want the first Echo sealed under session 7", r1)
		}
		if r2.ID != 31 || r2.Status != testproto.StatusChecksum || r2.Session != 9 || !r2.Signed {
			t.Errorf("the second record is %+v, want the refusal signed under session 9", r2)
		}
		if r3.ID != 32 || r3.Status != testproto.StatusOK || r3.Session != 7 || r3.Checksum == 0 || r3.Signed {
			t.Errorf("the third record is %+v, want the third Echo sealed under session 7", r3)
		}
		if got := s.client.Balance() - before; got != 3 {
			t.Errorf("the chain gave back %d credits, want one per member", got)
		}
		got, _ := scrape(t, m)
		if k := "adapter_chain_splits_total{protocol=" + testproto.Name + ",reason=unwrap}"; got[k] != 1 {
			t.Errorf("%s = %v, want 1", k, got[k])
		}
	})
}

// A related member whose checksum fails is refused the same way, and a
// related member after it is answered that status, with no call, in the
// refusal's record.
func TestARelatedMemberAfterAnUnwrapRefusalSharesItsRecord(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := newChained(t, 1<<20, adapter.Deps{})
		b, offs := chainOf(s.f, echoIn(40, 7), echoIn(41, inherited), echoIn(42, inherited))
		b[offs[1]+headerLen+1] ^= 1
		s.serveBody(b)
		r1 := s.recvOne()
		rs, err := s.client.Recv()
		if r1.ID != 40 || r1.Status != testproto.StatusOK {
			t.Errorf("the first record is %+v, want the first Echo", r1)
		}
		if err != nil || len(rs) != 2 || rs[0].ID != 41 || rs[0].Status != testproto.StatusChecksum || !rs[0].Signed ||
			rs[1].ID != 42 || rs[1].Status != testproto.StatusChecksum {
			t.Errorf("the second record is %+v, %v: want the refusal signed, and the related member after it answered its status", rs, err)
		}
	})
}

// A chain whose second member's next points behind it sends two records,
// counted decode: the first member's reply alone, sealed, and the second's
// refusal, its checksum 0; the connection then closes.
func TestABadStepEndsTheChainAfterWhatItHolds(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m := metrics.New("v", "c")
		s := newChained(t, 1<<20, adapter.Deps{Metrics: m})
		b, offs := chainOf(s.f, echoIn(50, 7), echoIn(51, 7))
		binary.BigEndian.PutUint32(b[offs[1]:], 3) // the second's next, behind it
		s.serveBody(b)
		r1, r2 := s.recvOne(), s.recvOne()
		if r1.ID != 50 || r1.Status != testproto.StatusOK || r1.Checksum == 0 {
			t.Errorf("the first record is %+v, want the first Echo sealed", r1)
		}
		if r2.ID != 51 || r2.Status != testproto.StatusDecode || r2.Checksum != 0 {
			t.Errorf("the second record is %+v, want the decode refusal, checksum 0", r2)
		}
		if err := s.closeErr(t); err == nil {
			t.Error("the connection closed with no error after a bad step")
		}
		got, _ := scrape(t, m)
		if k := "adapter_chain_splits_total{protocol=" + testproto.Name + ",reason=decode}"; got[k] != 1 {
			t.Errorf("%s = %v, want 1", k, got[k])
		}
	})
}

// armedSecurity is the stages' own Security, which once armed answers the
// second Unwrap it is asked, past its own check, with the header and
// ErrDecode, as SMB's check of an encrypted record's inner headers does: a
// fault injection.
type armedSecurity struct {
	core.Security
	armed atomic.Bool
	n     atomic.Int32
}

func (a *armedSecurity) Unwrap(h core.Header, rec *resources.Buf, prev core.Header) (core.Header, error) {
	u, err := a.Security.Unwrap(h, rec, prev)
	if err == nil && a.armed.Load() && a.n.Add(1) == 2 {
		return u, core.ErrDecode
	}
	return u, err
}

// A chain whose second member the stages' Security refuses with ErrDecode,
// after testproto's own check, sends two records counted unwrap, not
// decode: the first member's reply, and the refusal answered ErrDecode's
// status, sealed under the session, its checksum verifying.
func TestAnUnwrapRefusalWithErrDecodeIsCountedUnwrap(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m := metrics.New("v", "c")
		sec := &armedSecurity{}
		s := newChained(t, 1<<20, adapter.Deps{Metrics: m}, func(st *core.Stages) {
			sec.Security = st.Security
			st.Security = sec
		})
		sec.armed.Store(true)
		s.serve(echoIn(60, 7), echoIn(61, 7))
		r1, r2 := s.recvOne(), s.recvOne()
		if r1.ID != 60 || r1.Status != testproto.StatusOK {
			t.Errorf("the first record is %+v, want the first Echo", r1)
		}
		if r2.ID != 61 || r2.Status != testproto.StatusDecode || r2.Session != 7 || r2.Checksum == 0 || r2.Signed {
			t.Errorf("the second record is %+v, want the refusal sealed under session 7", r2)
		}
		got, _ := scrape(t, m)
		k := "adapter_chain_splits_total{protocol=" + testproto.Name + ",reason="
		if got[k+"unwrap}"] != 1 || got[k+"decode}"] != 0 {
			t.Errorf("the split counts %v under unwrap and %v under decode, want 1 and 0", got[k+"unwrap}"], got[k+"decode}"])
		}
	})
}

// gatedFlow is the stages' own FlowControl, which once armed answers every
// admission after the first as full: a try under an ended context is told
// it would wait, and a wait under a live one is held until gate closes or
// its context ends. A fault injection.
type gatedFlow struct {
	core.FlowControl
	armed atomic.Bool
	n     atomic.Int32
	gate  chan struct{}
}

func (f *gatedFlow) Admit(ctx context.Context, h core.Header) (func(), error) {
	if f.armed.Load() && f.n.Add(1) > 1 {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		select {
		case <-f.gate:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return f.FlowControl.Admit(ctx, h)
}

// A chain whose first or second member waits in Admit when the connection
// closes ends with nothing counted: the close's cancel is no refusal and no
// error no row maps, and nothing logs at error.
func TestAChainMembersAdmissionTheCloseEndsIsNoError(t *testing.T) {
	for _, c := range []struct {
		name  string
		serve func(s *served)
	}{
		{"its second member", func(s *served) {
			s.serve(echoIn(80, 7), echoIn(81, 7))
			if r := s.recvOne(); r.ID != 80 {
				s.t.Fatalf("the first record is %+v, want the first Echo, sent before the second waits", r)
			}
		}},
		{"its first member", func(s *served) {
			// A lone Echo takes the one admission that passes.
			s.serve(echoIn(80, 7))
			if r := s.recvOne(); r.ID != 80 {
				s.t.Fatalf("the first record is %+v, want the lone Echo", r)
			}
			// The reader's goroutine, which the chain's first admission holds.
			go s.serve(echoIn(81, 7), echoIn(82, 7))
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				m, logs := metrics.New("v", "c"), &logSink{}
				flow := &gatedFlow{gate: make(chan struct{})}
				s := newChained(t, 1<<20, adapter.Deps{Metrics: m, Logger: logs.logger()}, func(st *core.Stages) {
					flow.FlowControl = st.Flow
					st.Flow = flow
				})
				flow.armed.Store(true)
				c.serve(s)
				synctest.Wait()
				s.p.Close()
				if err := s.p.Wait(context.Background()); err != nil {
					t.Fatal(err)
				}
				got, _ := scrape(t, m)
				for _, k := range []string{
					"adapter_unmapped_errors_total{protocol=" + testproto.Name + "}",
					"adapter_refusals_total{protocol=" + testproto.Name + ",stage=admit}",
				} {
					if got[k] != 0 {
						t.Errorf("%s = %v after the close ended a member's admission, want 0", k, got[k])
					}
				}
				if e := logs.at(t, "ERROR"); len(e) != 0 {
					t.Errorf("a member's admission the close ended logged at error: %v", e)
				}
			})
		})
	}
}

// A chain whose second member would wait in Admit sends the first member's
// reply as a record of its own, counted flow, before that wait, and both are
// answered once the wait ends.
func TestAChainSendsWhatItHoldsBeforeWaitingOnFlow(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m := metrics.New("v", "c")
		flow := &gatedFlow{gate: make(chan struct{})}
		s := newChained(t, 1<<20, adapter.Deps{Metrics: m}, func(st *core.Stages) {
			flow.FlowControl = st.Flow
			st.Flow = flow
		})
		flow.armed.Store(true)
		s.serve(echoIn(70, 7), echoIn(71, 7))
		if r := s.recvOne(); r.ID != 70 || r.Status != testproto.StatusOK {
			t.Errorf("the first record is %+v, want the first Echo, sent before the second waits", r)
		}
		close(flow.gate)
		if r := s.recvOne(); r.ID != 71 || r.Status != testproto.StatusOK {
			t.Errorf("the second record is %+v, want the second Echo", r)
		}
		got, _ := scrape(t, m)
		if k := "adapter_chain_splits_total{protocol=" + testproto.Name + ",reason=flow}"; got[k] != 1 {
			t.Errorf("%s = %v, want 1", k, got[k])
		}
	})
}

// A chain of two Echo whose charges each pass a quarter of the reply pool's
// ceiling, on an idle pool, goes out in two records, counted quarter.
func TestAChainHoldsNoMoreThanAQuarterOfThePool(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m := metrics.New("v", "c")
		s := newChained(t, 200_000, adapter.Deps{Metrics: m})
		big := func(id uint64) testproto.Msg { e := echoIn(id, 7); e.Payload = make([]byte, 5000); return e }
		s.serve(big(80), big(81))
		if r1, r2 := s.recvOne(), s.recvOne(); r1.ID != 80 || r2.ID != 81 || r1.Status != testproto.StatusOK || r2.Status != testproto.StatusOK {
			t.Errorf("the records are %+v and %+v, want each Echo alone", r1, r2)
		}
		got, _ := scrape(t, m)
		if k := "adapter_chain_splits_total{protocol=" + testproto.Name + ",reason=quarter}"; got[k] != 1 {
			t.Errorf("%s = %v, want 1", k, got[k])
		}
	})
}

// On a server with a large reply budget, all of it but three Echo replies'
// charges held by a raw reservation, a chain of four Echo on Bare is answered
// in two records, the first three replies and then the fourth, counted
// reply_pool: the quarter of that ceiling is far above four charges, so only
// the pool's room splits the chain. An Echo on a second connection, sent
// while the chain holds its three, is answered too, with no time passing.
func TestAChainNeverWaitsOnThePoolHoldingIt(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const budget = 400_000
		m := metrics.New("v", "c")
		d := adapter.Deps{Metrics: m}
		proto := testproto.New(d).Bare()
		s := newSeamWith(t, proto, budget, d)
		small := s.reply.Charge(0)
		rest, err := s.budgets.Reply.Reserve(context.Background(), peer, budget-3*small)
		if err != nil {
			t.Fatal(err)
		}
		defer rest.Release()
		start := time.Now()
		echo := func(id uint64) testproto.Msg { return testproto.Msg{Op: testproto.OpEcho, Version: 1, ID: id} }
		s.serve(echo(90), echo(91), echo(92), echo(93))
		synctest.Wait()
		o := s.another(proto, netip.MustParseAddr("192.0.2.2"))
		o.serve(echo(94))
		rs, err := s.client.Recv()
		ids := func(rs []testproto.Reply) (out []uint64) {
			for _, r := range rs {
				out = append(out, r.ID)
			}
			return out
		}
		if err != nil || !slices.Equal(ids(rs), []uint64{90, 91, 92}) {
			t.Errorf("the first record holds %v, %v: want the first three", ids(rs), err)
		}
		if rs, err := s.client.Recv(); err != nil || !slices.Equal(ids(rs), []uint64{93}) {
			t.Errorf("the second record holds %v, %v: want the fourth", ids(rs), err)
		}
		if rs, err := o.client.Recv(); err != nil || !slices.Equal(ids(rs), []uint64{94}) {
			t.Errorf("the second connection's record holds %v, %v: want its Echo", ids(rs), err)
		}
		if waited := time.Since(start); waited != 0 {
			t.Errorf("the replies took %v on the clock, want none", waited)
		}
		got, _ := scrape(t, m)
		if k := "adapter_chain_splits_total{protocol=" + testproto.Name + ",reason=reply_pool}"; got[k] != 1 {
			t.Errorf("%s = %v, want 1", k, got[k])
		}
	})
}

// readOffsets is the stages' own HeaderCodec, recording the offset of each
// header Decode is asked for: a pass-through, where which headers are read
// is what is checked.
type readOffsets struct {
	core.HeaderCodec
	mu   sync.Mutex
	offs []int
}

func (r *readOffsets) Decode(rec *resources.Buf, off int) (core.Header, error) {
	r.mu.Lock()
	r.offs = append(r.offs, off)
	r.mu.Unlock()
	return r.HeaderCodec.Decode(rec, off)
}

// A chain of three whose second member's next points at its own header
// sends two records, counted decode: the first member's reply, then the
// second's refusal; the third member's header is never read, and the
// connection closes.
func TestANextOntoItsOwnHeaderEndsTheChain(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m := metrics.New("v", "c")
		codec := &readOffsets{}
		s := newChained(t, 1<<20, adapter.Deps{Metrics: m}, func(st *core.Stages) {
			codec.HeaderCodec = st.Header
			st.Header = codec
		})
		codec.mu.Lock()
		codec.offs = nil
		codec.mu.Unlock()
		b, offs := chainOf(s.f, echoIn(110, 7), echoIn(111, 7), echoIn(112, 7))
		binary.BigEndian.PutUint32(b[offs[1]:], uint32(offs[1]))
		s.serveBody(b)
		r1, r2 := s.recvOne(), s.recvOne()
		if r1.ID != 110 || r1.Status != testproto.StatusOK || r2.ID != 111 || r2.Status != testproto.StatusDecode {
			t.Errorf("the records are %+v and %+v, want the first Echo and the second's decode refusal", r1, r2)
		}
		if err := s.closeErr(t); err == nil {
			t.Error("the connection closed with no error")
		}
		codec.mu.Lock()
		read := slices.Clone(codec.offs)
		codec.mu.Unlock()
		if slices.Contains(read, offs[2]) || !slices.Contains(read, offs[1]) {
			t.Errorf("headers read at %v, want the second's at %d and never the third's at %d", read, offs[1], offs[2])
		}
		got, _ := scrape(t, m)
		if k := "adapter_chain_splits_total{protocol=" + testproto.Name + ",reason=decode}"; got[k] != 1 {
			t.Errorf("%s = %v, want 1", k, got[k])
		}
	})
}

// A chain of 65 Echo on Bare, past the connection's cap of 64, is answered
// whole: each member's admission is released once its reply is encoded, so
// the chain holds one place at a time.
func TestAChainLongerThanTheCapIsAnswered(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := newSeam(t, testproto.New(adapter.Deps{}).Bare(), 64<<20)
		var msgs []testproto.Msg
		for i := range 65 {
			msgs = append(msgs, testproto.Msg{Op: testproto.OpEcho, Version: 1, ID: uint64(i + 1)})
		}
		s.serve(msgs...)
		n := 0
		for n < 65 {
			rs, err := s.client.Recv()
			if err != nil {
				t.Fatal(err)
			}
			n += len(rs)
		}
	})
}

// While draining, a chain whose second of four members is refused answers
// the first and sends one interim, chained after it, granting the refused
// member's credits and those of the last two, and nothing after: the
// client's balance gains the five the chain charged.
func TestADrainedChainMemberGrantsTheCreditsAfterIt(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := newChained(t, 1<<20, adapter.Deps{})
		s.p.Drain()
		before := s.client.Balance()
		in := func(id uint64, credits uint16, role core.Role) testproto.Msg {
			e := echoIn(id, 7)
			e.Credits, e.Role = credits, role
			return e
		}
		s.serve(in(100, 1, core.RoleContext), in(101, 2, core.RoleNone), in(102, 1, core.RoleContext), in(103, 1, core.RoleContext))
		rs, err := s.client.Recv()
		if err != nil || len(rs) != 2 || rs[0].ID != 100 || rs[0].Status != testproto.StatusOK || !rs[1].Interim {
			t.Errorf("the chain is answered %+v, %v: want the first Echo and an interim, chained", rs, err)
		}
		// The seam hands the record to the pipeline past the client, which
		// charges nothing: the balance rises by what the replies grant.
		if after := s.client.Balance(); after-before != 5 {
			t.Errorf("the balance is %d after the chain, %d before: want the 5 credits the chain charged granted", after, before)
		}
		if !s.sentNothing() {
			t.Error("a member after the refused one was answered")
		}
	})
}

// Two chains on one connection, the first's Create held in the service, are
// answered second first.
func TestTwoChainsAreAnsweredOutOfOrder(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := newChained(t, 1<<20, adapter.Deps{})
		hold := s.faults.Hold(vfs.OpCreate)
		pair := func(id uint64, name string) []testproto.Msg {
			return []testproto.Msg{
				{Op: testproto.OpPutFH, Version: 2, ID: id, Session: 7, Tree: 1, Handle: s.root},
				{Op: testproto.OpCreate, Version: 2, ID: id + 1, Session: inherited, Tree: 1, Name: name, Related: true},
			}
		}
		s.serve(pair(120, "first")...)
		select {
		case <-hold.Reached():
		case <-time.After(time.Hour):
			t.Fatal("the first chain's Create never reached the service")
		}
		s.serve(pair(130, "second")...)
		rs, err := s.client.Recv()
		if err != nil || len(rs) != 2 || rs[0].ID != 130 {
			t.Errorf("the first record answered is %+v, %v: want the second chain's", rs, err)
		}
		hold.Release()
		if rs, err := s.client.Recv(); err != nil || len(rs) != 2 || rs[0].ID != 120 || rs[1].Status != testproto.StatusOK {
			t.Errorf("the second record answered is %+v, %v: want the first chain's", rs, err)
		}
	})
}

// A chain whose Create is in the service when the drain begins is answered.
func TestAChainInTheServiceAtTheDrainIsAnswered(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := newChained(t, 1<<20, adapter.Deps{})
		hold := s.faults.Hold(vfs.OpCreate)
		s.serve(
			testproto.Msg{Op: testproto.OpPutFH, Version: 2, ID: 140, Session: 7, Tree: 1, Handle: s.root},
			testproto.Msg{Op: testproto.OpCreate, Version: 2, ID: 141, Session: inherited, Tree: 1, Name: "d", Related: true},
		)
		<-hold.Reached()
		s.p.Drain()
		hold.Release()
		if rs, err := s.client.Recv(); err != nil || len(rs) != 2 || rs[1].Status != testproto.StatusOK || s.creates() != 1 {
			t.Errorf("the chain is answered %+v, %v, with %d files made; want both and the file", rs, err, s.creates())
		}
	})
}

// A sequenced chain sent twice is answered twice, its Create run once and
// its reply the second time from the replay cache.
func TestASequencedChainSentTwiceRunsOnce(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m := metrics.New("v", "c")
		// The replay cache copies a reply from the server's reply budget under no
		// client's key, so the chain takes the copy into a buffer of its own.
		s := newChained(t, 1<<20, adapter.Deps{Metrics: m, Budgets: serverBudgets(1 << 20)})
		chain := sequenced(s, 150)
		for range 2 {
			s.serve(chain...)
			if rs, err := s.client.Recv(); err != nil || len(rs) != 2 || rs[1].Status != testproto.StatusOK {
				t.Errorf("the chain is answered %+v, %v", rs, err)
			}
		}
		if s.creates() != 1 {
			t.Errorf("the Create ran %d times, want once", s.creates())
		}
		got, _ := scrape(t, m)
		var hits float64
		for k, v := range got {
			if strings.HasPrefix(k, "adapter_replay_hits_total{") {
				hits += v
			}
		}
		if hits != 1 {
			t.Errorf("the replay cache answered %v times, want once", hits)
		}
	})
}

// Two servers over two services each apply their own chain's Create.
func TestTwoServersApplyTheirOwnChains(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		a, b := newChained(t, 1<<20, adapter.Deps{}), newChained(t, 1<<20, adapter.Deps{})
		for _, s := range []*served{a, b} {
			s.serve(
				testproto.Msg{Op: testproto.OpPutFH, Version: 2, ID: 160, Session: 7, Tree: 1, Handle: s.root},
				testproto.Msg{Op: testproto.OpCreate, Version: 2, ID: 161, Session: inherited, Tree: 1, Name: "own", Related: true},
			)
			if rs, err := s.client.Recv(); err != nil || len(rs) != 2 || rs[1].Status != testproto.StatusOK {
				t.Errorf("a server's chain is answered %+v, %v", rs, err)
			}
		}
		if a.creates() != 1 || b.creates() != 1 {
			t.Errorf("the services made %d and %d files, want one each", a.creates(), b.creates())
		}
	})
}

// Two chains of a PutFH, a related Create and an Echo, on two connections of a
// server whose reply budget, all but the four replies before the Echo held by
// a raw reservation, leaves neither Echo room while the other chain holds its
// two, each Create held in the service until both are reached, are both
// answered once the test releases them, with no time passing on the clock.
func TestTwoChainsOnTwoConnectionsShareThePool(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const budget = 400_000
		a := newChained(t, budget, adapter.Deps{})
		b := a.another(netip.MustParseAddr("192.0.2.2"), 8)
		small := a.reply.Charge(0)
		rest, err := a.budgets.Reply.Reserve(context.Background(), peer, budget-4*small)
		if err != nil {
			t.Fatal(err)
		}
		defer rest.Release()
		start := time.Now()
		holds := []*vfstest.Hold{a.faults.Hold(vfs.OpCreate), a.faults.Hold(vfs.OpCreate)}
		chain := func(s *served, session uint32, id uint64, name string) {
			s.serve(
				testproto.Msg{Op: testproto.OpPutFH, Version: 2, ID: id, Session: session, Tree: 1, Handle: s.root},
				testproto.Msg{Op: testproto.OpCreate, Version: 2, ID: id + 1, Session: inherited, Tree: 1, Name: name, Related: true},
				echoIn(id+2, session),
			)
		}
		chain(a, 7, 170, "a")
		chain(b, 8, 180, "b")
		for _, h := range holds {
			<-h.Reached()
		}
		for _, h := range holds {
			h.Release()
		}
		for _, s := range []*served{a, b} {
			n := 0
			for n < 3 {
				rs, err := s.client.Recv()
				if err != nil {
					t.Fatal(err)
				}
				for _, r := range rs {
					if r.Status != testproto.StatusOK {
						t.Errorf("a chain's reply is %+v", r)
					}
				}
				n += len(rs)
			}
		}
		if waited := time.Since(start); waited != 0 {
			t.Errorf("the chains took %v on the clock, want none", waited)
		}
		if a.creates() != 2 {
			t.Errorf("the service made %d files, want both", a.creates())
		}
		// The second connection's cleanup reads the shared budget: wait for
		// the first's recorder to release what it last wrote.
		synctest.Wait()
	})
}

// A chain of two Echo and an unknown code on Bare, on a pool with room for
// two replies, sends the two Echo as a record of their own, counted
// reply_pool, before the refusal waits for its status's room.
func TestARefusalWithNoRoomSendsTheChainFirst(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const budget = 400_000
		m := metrics.New("v", "c")
		d := adapter.Deps{Metrics: m}
		s := newSeamWith(t, testproto.New(d).Bare(), budget, d)
		small := s.reply.Charge(0)
		rest, err := s.budgets.Reply.Reserve(context.Background(), peer, budget-2*small)
		if err != nil {
			t.Fatal(err)
		}
		defer rest.Release()
		s.serve(
			testproto.Msg{Op: testproto.OpEcho, Version: 1, ID: 200},
			testproto.Msg{Op: testproto.OpEcho, Version: 1, ID: 201},
			testproto.Msg{Op: 99, Version: 1, ID: 202},
		)
		if rs, err := s.client.Recv(); err != nil || len(rs) != 2 || rs[0].ID != 200 || rs[1].ID != 201 {
			t.Errorf("the first record is %+v, %v: want both Echo", rs, err)
		}
		if r := s.read(); r.ID != 202 || r.Status != testproto.StatusNotSupported {
			t.Errorf("the second record is %+v, want the unknown code refused", r)
		}
		got, _ := scrape(t, m)
		if k := "adapter_chain_splits_total{protocol=" + testproto.Name + ",reason=reply_pool}"; got[k] != 1 {
			t.Errorf("%s = %v, want 1", k, got[k])
		}
	})
}

// serverBudgets is a server's budgets, reply bytes for replies, for a test
// whose stages draw on the server's reply budget as the pipeline does.
func serverBudgets(reply int64) *resources.Budgets {
	return &resources.Budgets{Receive: resources.NewWaitBudget("receive", 64<<20), Reply: resources.NewWaitBudget("reply", reply)}
}

// sequenced is a chain of a PutFH under session 7 and a related Create of
// sequence 9, and then more.
func sequenced(s *served, id uint64, more ...testproto.Msg) []testproto.Msg {
	return append([]testproto.Msg{
		{Op: testproto.OpPutFH, Version: 2, ID: id, Session: 7, Tree: 1, Handle: s.root},
		{Op: testproto.OpCreate, Version: 2, ID: id + 1, Session: inherited, Tree: 1, Name: "once", Related: true, Seq: 9},
	}, more...)
}

// recvIDs reads n records, each reply in them answered OK, and answers the
// IDs each holds.
func recvIDs(t *testing.T, s *served, n int) [][]uint64 {
	t.Helper()
	var out [][]uint64
	for range n {
		rs, err := s.client.Recv()
		if err != nil {
			t.Fatal(err)
		}
		var ids []uint64
		for _, r := range rs {
			if r.Status != testproto.StatusOK {
				t.Errorf("a reply is %+v, want it answered OK", r)
			}
			ids = append(ids, r.ID)
		}
		out = append(out, ids)
	}
	return out
}

// A sequenced chain sent again on a reply pool with room for one reply, the
// PutFH's: the Create's hit would wait for its copy's room, so the PutFH's
// reply goes out first, counted reply_pool, and only then is the cache asked
// under the member's deadline; with no room left for the chain's own copy,
// the hit goes in a record of its own, with no time passing.
func TestAHitThatWouldWaitSendsTheChainFirst(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const budget = 400_000
		m := metrics.New("v", "c")
		s := newChained(t, budget, adapter.Deps{Metrics: m, Budgets: serverBudgets(budget)})
		s.serve(sequenced(s, 150)...)
		recvIDs(t, s, 1)
		small := s.reply.Charge(0)
		rest, err := s.budgets.Reply.Reserve(context.Background(), peer, budget-small)
		if err != nil {
			t.Fatal(err)
		}
		defer rest.Release()
		start := time.Now()
		s.serve(sequenced(s, 150)...)
		if got := recvIDs(t, s, 2); !slices.Equal(got[0], []uint64{150}) || !slices.Equal(got[1], []uint64{151}) {
			t.Errorf("the records hold %v, want the PutFH, then the Create", got)
		}
		if waited := time.Since(start); waited != 0 {
			t.Errorf("the chain took %v on the clock, want none", waited)
		}
		if s.creates() != 1 {
			t.Errorf("the Create ran %d times, want once", s.creates())
		}
		got, _ := scrape(t, m)
		if k := "adapter_chain_splits_total{protocol=" + testproto.Name + ",reason=reply_pool}"; got[k] != 1 {
			t.Errorf("%s = %v, want 1", k, got[k])
		}
	})
}

// A sequenced chain of a PutFH, a Create and an Echo sent again on a reply
// pool with room for two replies: the PutFH's and the cache's copy fill it,
// so the chain's own copy does not fit, the PutFH's reply goes out first,
// counted reply_pool, and the hit goes in a record of its own, the Echo in
// the next.
func TestAHitWithNoRoomForItsCopyGoesAlone(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const budget = 400_000
		m := metrics.New("v", "c")
		s := newChained(t, budget, adapter.Deps{Metrics: m, Budgets: serverBudgets(budget)})
		s.serve(sequenced(s, 150, echoIn(152, 7))...)
		recvIDs(t, s, 1)
		small := s.reply.Charge(0)
		rest, err := s.budgets.Reply.Reserve(context.Background(), peer, budget-2*small)
		if err != nil {
			t.Fatal(err)
		}
		defer rest.Release()
		s.serve(sequenced(s, 150, echoIn(152, 7))...)
		if got := recvIDs(t, s, 3); !slices.Equal(got[0], []uint64{150}) || !slices.Equal(got[1], []uint64{151}) || !slices.Equal(got[2], []uint64{152}) {
			t.Errorf("the records hold %v, want the PutFH, the Create, then the Echo", got)
		}
		got, _ := scrape(t, m)
		if k := "adapter_chain_splits_total{protocol=" + testproto.Name + ",reason=reply_pool}"; got[k] != 1 {
			t.Errorf("%s = %v, want 1", k, got[k])
		}
	})
}

// laxHeader is testproto's header with the next step its bytes give,
// unchecked, at off in its record.
type laxHeader struct {
	core.Header
	off, next int
}

func (h laxHeader) Next() int { return h.next }

// inner is h as testproto reads it.
func inner(h core.Header) core.Header {
	if l, ok := h.(laxHeader); ok {
		return l.Header
	}
	return h
}

// laxRead is one header the lax codec was asked for: its offset, the step it
// named, and whether it decoded.
type laxRead struct {
	off, next int
	decoded   bool
}

// laxCodec is testproto's HeaderCodec, declared in the test, taking each Next
// as the bytes give it: an input standing for a lax protocol, whose steps
// only core's walk bounds. It records each header it is asked for, and
// refuses one that is not forward of the one before and inside the record,
// so a walk past its bounds is recorded and ends.
type laxCodec struct {
	core.HeaderCodec
	mu    sync.Mutex
	reads []laxRead
}

func (c *laxCodec) Decode(rec *resources.Buf, off int) (core.Header, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	r := laxRead{off: off}
	if off >= 0 && off+4 <= len(rec.B) {
		r.next = int(binary.BigEndian.Uint32(rec.B[off:]))
	}
	forward := len(c.reads) == 0 || off > c.reads[len(c.reads)-1].off
	var h core.Header
	err := errors.New("a header behind the one before or past the record")
	if forward && off < len(rec.B) {
		if h, err = c.HeaderCodec.Decode(rec, off); h != nil {
			h, err, r.decoded = laxHeader{h, off, r.next}, nil, true
		}
	}
	c.reads = append(c.reads, r)
	return h, err
}

func (c *laxCodec) Encode(h core.Header, s core.Status, out *resources.Buf) error {
	return c.HeaderCodec.Encode(inner(h), s, out)
}

func (c *laxCodec) ReplyLen(h core.Header) int { return c.HeaderCodec.ReplyLen(inner(h)) }

// Args is the bytes after h's header, to its next where that is forward
// inside the record and to the record's end otherwise.
func (c *laxCodec) Args(rec *resources.Buf, h core.Header) []byte {
	l, ok := h.(laxHeader)
	if !ok {
		return c.HeaderCodec.Args(rec, h)
	}
	start, end := min(l.off+headerLen, len(rec.B)), len(rec.B)
	if l.next >= start && l.next < end {
		end = l.next
	}
	return rec.B[start:end]
}

// laxRouter routes testproto's header under a lax one.
type laxRouter struct{ core.Router }

func (r laxRouter) Route(h core.Header, prev *core.Request) (*core.OpTable, *core.Request, error) {
	return r.Router.Route(inner(h), prev)
}

// walkChain serves body as one record on Bare through the lax codec and
// checks the walk: it ends, it reads the first header at 0 and each one after
// inside the record and past the one before, and a header that decoded
// naming a bad step closes the connection with ErrDecode.
func walkChain(t *testing.T, body []byte) {
	synctest.Test(t, func(t *testing.T) {
		codec := &laxCodec{}
		s := newSeam(t, edited{testproto.New(adapter.Deps{}).Bare(), func(st *core.Stages) {
			codec.HeaderCodec = st.Header
			st.Header = codec
			st.Router = laxRouter{st.Router}
		}}, 1<<20)
		go func() {
			for {
				if _, err := s.client.Recv(); err != nil {
					return
				}
			}
		}()
		s.serveBody(body)
		synctest.Wait()
		codec.mu.Lock()
		reads := slices.Clone(codec.reads)
		codec.mu.Unlock()
		for i, r := range reads {
			if (i == 0 && r.off != 0) || (i > 0 && (r.off <= reads[i-1].off || r.off >= len(body))) {
				t.Fatalf("the walk read headers at %v in a record of %d bytes: want the first at 0 and each after it forward inside the record", reads, len(body))
			}
		}
		last := reads[len(reads)-1]
		if !last.decoded || last.next == 0 || (last.next > last.off && last.next < len(body)) {
			return
		}
		select {
		case err := <-s.closed:
			if !errors.Is(err, core.ErrDecode) {
				t.Fatalf("a step from %d to %d in %d bytes closed the connection with %v, want ErrDecode", last.off, last.next, len(body), err)
			}
		default:
			t.Fatalf("a step from %d to %d in %d bytes left the connection open", last.off, last.next, len(body))
		}
	})
}

// A chain's members are each traced as a request alone: a chain of two Echo
// ends two request spans, each named after its operation and neither a
// child, where a compound's members are children of its one span; each
// carries its status, its reply's bytes, and its own message's bytes.
func TestEachChainMemberHasItsOwnRequestSpan(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		spans := tracetest.NewSpanRecorder()
		tracer := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(spans)).Tracer("test")
		d := adapter.Deps{Tracer: tracer, SampleRatio: 1}
		s := newSeamWith(t, testproto.New(d).Bare(), 1<<20, d)
		b, offs := chainOf(s.f, testproto.Msg{Op: testproto.OpEcho, Version: 1, ID: 1, Payload: []byte("a")}, testproto.Msg{Op: testproto.OpEcho, Version: 1, ID: 2, Payload: []byte("bcd")})
		s.serveBody(b)
		if rs, err := s.client.Recv(); err != nil || len(rs) != 2 {
			t.Fatalf("the chain is answered %+v, %v", rs, err)
		}
		s.finish()
		ended := spans.Ended()
		if len(ended) != 2 {
			t.Fatalf("%d spans ended for a chain of two", len(ended))
		}
		sizes := map[string]bool{strconv.Itoa(offs[1]): true, strconv.Itoa(len(b) - offs[1]): true}
		for _, sd := range ended {
			a := map[string]string{}
			for _, kv := range sd.Attributes() {
				a[string(kv.Key)] = kv.Value.String()
			}
			if sd.Name() != "adapter."+testproto.Name+".Echo" || sd.Parent().IsValid() || a["status"] != "0" || a["reply_bytes"] == "" || a["reply_bytes"] == "0" || !sizes[a["request_bytes"]] {
				t.Errorf("a member's span is %q under %v with %v, want its own request span with its status and sizes, its request one of %v", sd.Name(), sd.Parent().SpanID(), a, sizes)
			}
			delete(sizes, a["request_bytes"])
		}
	})
}

// A chain member's span carries the version Route accepted, and only then
// (§10.2): of a chain of an Echo at version 1 and one at 3, outside the row,
// the first span carries version 1 and the second, refused at route, none.
func TestAChainMembersVersionIsTheOneRouteAccepted(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		spans := tracetest.NewSpanRecorder()
		tracer := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(spans)).Tracer("test")
		d := adapter.Deps{Tracer: tracer, SampleRatio: 1}
		s := newSeamWith(t, testproto.New(d).Bare(), 1<<20, d)
		b, _ := chainOf(s.f, testproto.Msg{Op: testproto.OpEcho, Version: 1, ID: 1, Payload: []byte("a")}, testproto.Msg{Op: testproto.OpEcho, Version: 3, ID: 2})
		s.serveBody(b)
		if rs, err := s.client.Recv(); err != nil || len(rs) != 2 {
			t.Fatalf("the chain is answered %+v, %v", rs, err)
		}
		s.finish()
		versions := map[string]string{} // by stage, "" for the served member
		for _, sd := range spans.Ended() {
			a := map[string]string{}
			for _, kv := range sd.Attributes() {
				a[string(kv.Key)] = kv.Value.String()
			}
			versions[a["stage"]] = a["version"]
		}
		if len(versions) != 2 || versions[""] != "1" || versions["route"] != "" {
			t.Errorf("the members' versions by refusing stage are %v, want 1 for the served Echo and none for the one refused at route", versions)
		}
	})
}

// A chain member whose call fails with an error no row maps is answered the
// fault status in the chain's record, after the member before it.
func TestAChainMembersUnmappedErrorIsAnsweredTheFault(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m := metrics.New("v", "c")
		s := newChained(t, 1<<20, adapter.Deps{Metrics: m})
		s.faults.Fail(vfs.OpCreate, errors.New("a service error no row maps"))
		s.serve(
			testproto.Msg{Op: testproto.OpPutFH, Version: 2, ID: 190, Session: 7, Tree: 1, Handle: s.root},
			testproto.Msg{Op: testproto.OpCreate, Version: 2, ID: 191, Session: inherited, Tree: 1, Name: "f", Related: true},
		)
		rs, err := s.client.Recv()
		if err != nil || len(rs) != 2 || rs[0].Status != testproto.StatusOK || rs[1].ID != 191 || rs[1].Status != testproto.StatusFault {
			t.Errorf("the chain is answered %+v, %v: want the PutFH, then the Create answered the fault", rs, err)
		}
		got, _ := scrape(t, m)
		if k := "adapter_unmapped_errors_total{protocol=" + testproto.Name + "}"; got[k] != 1 {
			t.Errorf("%s = %v, want 1", k, got[k])
		}
	})
}

// A chain of three small Echo on Bare, each try finding another connection's
// request waiting on the reply pool ahead of it, goes out one record per
// member, counted reply_pool twice: a try is refused while anything waits,
// however much room is left, so the replies before go out first. Each waiter
// is a raw reservation of the whole ceiling under another key, standing for
// that request; the test arms the next one behind the member before letting
// the one ahead go, so one is always waiting when a member tries.
func TestAChainBehindAWaiterGoesOutOneRecordPerMember(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const budget = 400_000
		m := metrics.New("v", "c")
		d := adapter.Deps{Metrics: m}
		s := newSeamWith(t, testproto.New(d).Bare(), budget, d)
		other := netip.MustParseAddr("192.0.2.2")
		hold, err := s.budgets.Reply.Reserve(context.Background(), other, 1)
		if err != nil {
			t.Fatal(err)
		}
		// A waiter leaves once its goroutine has ended: a release can grant
		// it the room before it runs to see its cancel, and it holds that
		// room until it runs.
		waiter := func() (leave func()) {
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan struct{})
			go func() {
				defer close(done)
				if r, err := s.budgets.Reply.Reserve(ctx, other, budget); err == nil {
					r.Release()
				}
			}()
			synctest.Wait()
			return func() { cancel(); <-done }
		}
		ahead := waiter()
		echo := func(id uint64) testproto.Msg { return testproto.Msg{Op: testproto.OpEcho, Version: 1, ID: id} }
		s.serve(echo(1), echo(2), echo(3))
		synctest.Wait()
		for range 3 {
			next := waiter()
			ahead()
			synctest.Wait()
			ahead = next
		}
		ahead()
		hold.Release()
		for i := range uint64(3) {
			if rs, err := s.client.Recv(); err != nil || len(rs) != 1 || rs[0].ID != i+1 {
				t.Errorf("record %d holds %+v, %v: want member %d alone", i+1, rs, err, i+1)
			}
		}
		got, _ := scrape(t, m)
		if k := "adapter_chain_splits_total{protocol=" + testproto.Name + ",reason=reply_pool}"; got[k] != 2 {
			t.Errorf("%s = %v, want 2", k, got[k])
		}
	})
}

// A chain whose first member is refused on the reader is answered that
// refusal and runs on: one whose checksum fails goes out in a record of its
// own, the next Echo in the next; one that overdraws its credits is answered
// the overdraw status beside the next Echo's reply.
func TestAChainsFirstMemberRefusalIsAnswered(t *testing.T) {
	t.Run("unwrap", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			s := newChained(t, 1<<20, adapter.Deps{})
			b, _ := chainOf(s.f, echoIn(40, 7), echoIn(41, 7))
			b[headerLen+1] ^= 1 // a byte of the first's payload
			s.serveBody(b)
			if r := s.recvOne(); r.ID != 40 || r.Status != testproto.StatusChecksum {
				t.Errorf("the first record is %+v, want the first refused its checksum", r)
			}
			if r := s.recvOne(); r.ID != 41 || r.Status != testproto.StatusOK {
				t.Errorf("the second record is %+v, want the second Echo", r)
			}
		})
	})
	t.Run("admit", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			s := newChained(t, 1<<20, adapter.Deps{})
			over := echoIn(42, 7)
			over.Credits = 1000
			s.serve(over, echoIn(43, 7))
			rs, err := s.client.Recv()
			if err != nil || len(rs) != 2 || rs[0].ID != 42 || rs[0].Status != testproto.StatusOverdraw || rs[1].ID != 43 || rs[1].Status != testproto.StatusOK {
				t.Errorf("the chain is answered %+v, %v: want the overdraw refused, then the Echo", rs, err)
			}
		})
	})
}

// A Cancel in a chain, first or later, is answered nothing, and the chain's
// other member is answered.
func TestAChainsCancelIsAnsweredNothing(t *testing.T) {
	cancel := testproto.Msg{Op: testproto.OpCancel, Version: 2, Session: 7, Tree: 1, Target: 9}
	for _, c := range []struct {
		name string
		msgs []testproto.Msg
	}{
		{"first", []testproto.Msg{cancel, echoIn(44, 7)}},
		{"later", []testproto.Msg{echoIn(44, 7), cancel}},
	} {
		t.Run(c.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				s := newChained(t, 1<<20, adapter.Deps{})
				s.serve(c.msgs...)
				rs, err := s.client.Recv()
				if err != nil || len(rs) != 1 || rs[0].ID != 44 || rs[0].Status != testproto.StatusOK {
					t.Errorf("the chain is answered %+v, %v: want the Echo alone", rs, err)
				}
			})
		})
	}
}

// A related member after a cancel starts from no request: the cancel is the
// member before it and sets no handle (§4.7, Related), so in the chain
// [PutFH, an unrelated Cancel, a related Create] the Create is refused for
// having no handle and makes no file.
func TestARelatedMemberAfterACancelStartsFromNoRequest(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := newChained(t, 1<<20, adapter.Deps{})
		s.serve(
			testproto.Msg{Op: testproto.OpPutFH, Version: 2, ID: 170, Session: 7, Tree: 1, Handle: s.root},
			testproto.Msg{Op: testproto.OpCancel, Version: 2, Session: 7, Tree: 1, Target: 9},
			testproto.Msg{Op: testproto.OpCreate, Version: 2, ID: 172, Session: inherited, Tree: 1, Name: "after", Related: true},
		)
		rs, err := s.client.Recv()
		if err != nil || len(rs) != 2 || rs[1].ID != 172 || rs[1].Status != testproto.StatusNoFH {
			t.Errorf("the chain is answered %+v, %v: want the related Create after the cancel refused for no handle", rs, err)
		}
		if n := s.creates(); n != 0 {
			t.Errorf("the related Create after the cancel made %d files, want none", n)
		}
	})
}

// A cancel is the member before the next, so a related member after it is
// not answered the failure of the member before the cancel (§4.3).
func TestARelatedMemberAfterACancelIsNotFailedByTheMemberBeforeIt(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := newChained(t, 1<<20, adapter.Deps{})
		s.serve(
			testproto.Msg{Op: testproto.OpCreate, Version: 2, ID: 180, Session: 7, Tree: 1, Name: "nohandle"},
			testproto.Msg{Op: testproto.OpCancel, Version: 2, Session: 7, Tree: 1, Target: 9},
			testproto.Msg{Op: testproto.OpPutFH, Version: 2, ID: 182, Session: inherited, Tree: 1, Handle: s.root, Related: true},
		)
		rs, err := s.client.Recv()
		if err != nil || len(rs) != 2 || rs[0].Status != testproto.StatusNoFH || rs[1].ID != 182 || rs[1].Status != 0 {
			t.Errorf("the chain is answered %+v, %v: want the Create refused for no handle and the related PutFH after the cancel answered success", rs, err)
		}
	})
}

// A chain admitted after close is not run: no Route, no call, no reply.
func TestAChainAfterCloseIsNotRun(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		routers := make(chan *countsRoutes, 1)
		var s *seam
		closing := edited{testproto.New(adapter.Deps{}), func(st *core.Stages) {
			w := &countsRoutes{Router: st.Router}
			st.Router = w
			routers <- w
			st.Security = closesOnUnwrap{st.Security, func() { s.p.Close() }}
		}}
		s = newSeam(t, closing, 1<<20)
		w := <-routers
		s.serve(testproto.Msg{Op: testproto.OpEcho, Version: 1, ID: 1}, testproto.Msg{Op: testproto.OpEcho, Version: 1, ID: 2})
		synctest.Wait()
		if n := w.routes.Load(); n != 0 {
			t.Errorf("a chain admitted after close was routed %d times", n)
		}
		if !s.sentNothing() {
			t.Error("a chain admitted after close was answered")
		}
	})
}

// A related member is unwrapped under the member just before it: after an
// Echo under session 7 and one under session 9, a third that inherits its
// session is answered under 9.
func TestARelatedMemberInheritsFromTheMemberJustBefore(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := newChained(t, 1<<20, adapter.Deps{})
		s.serve(testproto.Msg{Op: testproto.OpSession, Version: 2, Session: 9})
		if r := s.read(); r.Status != testproto.StatusOK {
			t.Fatalf("session 9: %+v", r)
		}
		s.serve(echoIn(50, 7), echoIn(51, 9), echoIn(52, inherited))
		rs, err := s.client.Recv()
		if err != nil || len(rs) != 3 || rs[2].ID != 52 || rs[2].Status != testproto.StatusOK || rs[2].Session != 9 {
			t.Errorf("the chain is answered %+v, %v: want the third answered under session 9", rs, err)
		}
	})
}

// Two members of one chain that Unwrap refuses are logged once: a client
// that sends what its connection's security refuses is logged per
// connection, not per request.
func TestUnwrapRefusalsInAChainAreLoggedOnce(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		logs := &logSink{}
		s := newChained(t, 1<<20, adapter.Deps{Logger: logs.logger()})
		b, offs := chainOf(s.f, echoIn(60, 7), echoIn(61, 7), echoIn(62, 7))
		b[headerLen+1] ^= 1
		b[offs[1]+headerLen+1] ^= 1
		s.serveBody(b)
		for range 3 {
			s.recvOne()
		}
		n := 0
		for _, r := range logs.at(t, "WARN") {
			if r["msg"] == "a message the connection's security refused" {
				n++
			}
		}
		if n != 1 {
			t.Errorf("the refusals were logged %d times, want once", n)
		}
	})
}

// A chain of a failed member and 65 related ones after it, on Bare, is
// answered whole: each related member's admission is released when it is
// answered the failure's status, so the chain holds one place at a time.
func TestAChainOfFailuresLongerThanTheCapIsAnswered(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := newSeam(t, testproto.New(adapter.Deps{}).Bare(), 64<<20)
		msgs := []testproto.Msg{{Op: 99, Version: 1, ID: 1}}
		for i := range 65 {
			msgs = append(msgs, testproto.Msg{Op: testproto.OpEcho, Version: 1, ID: uint64(i + 2), Related: true})
		}
		s.serve(msgs...)
		n := 0
		for n < 66 {
			rs, err := s.client.Recv()
			if err != nil {
				t.Fatal(err)
			}
			for _, r := range rs {
				if r.Status != testproto.StatusNotSupported {
					t.Fatalf("a member is answered %+v, want the failure's status", r)
				}
			}
			n += len(rs)
		}
	})
}

// On a reply pool whose quarter is two small replies, a chain of three small
// Echo on Bare joins the first two, whose charges reach the quarter but do
// not pass it, and sends the third in a record of its own, counted quarter.
func TestAChainJoinsUpToAQuarterOfThePool(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m := metrics.New("v", "c")
		d := adapter.Deps{Metrics: m}
		s := newSeamWith(t, testproto.New(d).Bare(), 8*smallCharge, d)
		if c := s.reply.Charge(0); c != smallCharge {
			t.Fatalf("a small reply charges %d, want %d", c, smallCharge)
		}
		s.serve(testproto.Msg{Op: testproto.OpEcho, Version: 1, ID: 1}, testproto.Msg{Op: testproto.OpEcho, Version: 1, ID: 2}, testproto.Msg{Op: testproto.OpEcho, Version: 1, ID: 3})
		if rs, err := s.client.Recv(); err != nil || len(rs) != 2 || rs[0].ID != 1 || rs[1].ID != 2 {
			t.Errorf("the first record holds %+v, %v: want the first two", rs, err)
		}
		if r := s.read(); r.ID != 3 {
			t.Errorf("the second record is %+v, want the third", r)
		}
		got, _ := scrape(t, m)
		if k := "adapter_chain_splits_total{protocol=" + testproto.Name + ",reason=quarter}"; got[k] != 1 {
			t.Errorf("%s = %v, want 1", k, got[k])
		}
	})
}

// growsWrap is the stages' Security whose Wrap adds the bytes its header's
// ReplyLen bounds before sealing: an input standing for a protocol whose
// Wrap adds a header of its own, as SMB's transform header is.
type growsWrap struct {
	core.Security
	codec core.HeaderCodec
}

func (g growsWrap) Wrap(h core.Header, reply *resources.Buf) error {
	reply.B = append(reply.B, make([]byte, g.codec.ReplyLen(h))...)
	return g.Security.Wrap(h, reply)
}

// A chain's record, as join frames it, stays within the protocol's record
// cap (§4.7): its replies as encoded, Chain's padding between them, and
// what Wrap adds, which the first member's ReplyLen bounds. Two Echo, their
// replies grown to some half of testproto's 1 MiB cap, join in one record
// framed at the cap, and go out in two, counted record, framed one byte
// past it, or with the replies alone at the cap, which Wrap would take past
// the cap the client reads.
func TestAChainSplitsAtTheRecordCap(t *testing.T) {
	const grown = 500 << 10
	limit, _ := testproto.New(adapter.Deps{}).Framer().MaxRecord()
	framed := limit - core.ChainPadding - headerLen // two replies' bytes whose record join frames at the cap
	for _, c := range []struct {
		name    string
		encoded int // the two replies' bytes, as encoded
		records int
	}{
		{"framed at the cap", framed, 1},
		{"framed one byte past the cap", framed + 1, 2},
		{"encoded at the cap", limit, 2},
	} {
		t.Run(c.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				grow := func(inv core.Invocation) core.Invocation {
					enc, bound := inv.Encode, inv.Bound
					inv.Encode = func(out *resources.Buf, v core.Version) error {
						if err := enc(out, v); err != nil {
							return err
						}
						out.B = append(out.B, make([]byte, grown)...)
						return nil
					}
					inv.Bound = func(r *core.Request, room int) (int, bool) {
						n, ok := bound(r, room)
						return n + grown, ok
					}
					return inv
				}
				m := metrics.New("v", "c")
				d := adapter.Deps{Metrics: m}
				proto := edited{wrapRows(testproto.New(d), grow), func(st *core.Stages) {
					st.Security = growsWrap{Security: st.Security, codec: st.Header}
				}}
				s := newSeamWith(t, proto, 64<<20, d)
				payload := make([]byte, c.encoded-2*(headerLen+grown)) // the second Echo's: an Echo's reply is its header and its payload
				s.serve(testproto.Msg{Op: testproto.OpEcho, Version: 1, ID: 1}, testproto.Msg{Op: testproto.OpEcho, Version: 1, ID: 2, Payload: payload})
				var ids []uint64
				records := 0
				for len(ids) < 2 {
					rs, err := s.client.Recv()
					if err != nil {
						t.Fatalf("record %d after replies %v: %v", records+1, ids, err)
					}
					records++
					for _, r := range rs {
						ids = append(ids, r.ID)
					}
				}
				if records != c.records || !slices.Equal(ids, []uint64{1, 2}) {
					t.Errorf("the chain is answered in %d records, replies %v: want %d, replies [1 2]", records, ids, c.records)
				}
				got, _ := scrape(t, m)
				for reason, want := range map[string]float64{"record": float64(c.records - 1), "quarter": 0} {
					if k := "adapter_chain_splits_total{protocol=" + testproto.Name + ",reason=" + reason + "}"; got[k] != want {
						t.Errorf("%s = %v, want %v", k, got[k], want)
					}
				}
			})
		})
	}
}

// A member answered with a status alone keeps the chain's record within the
// protocol's record cap as a reply does (§4.7): its status's buffer, its
// header's ReplyLen, is counted with the replies held, Chain's padding and
// what Wrap adds. An Echo, then an operation the table lacks, whose refusal's
// status joins the Echo's reply in one record framed at the cap, and goes
// out in a record of its own, counted record, framed one byte past it.
func TestAStatusSplitsAChainAtTheRecordCap(t *testing.T) {
	limit, _ := testproto.New(adapter.Deps{}).Framer().MaxRecord()
	// The Echo's reply whose record join frames at the cap: Chain's padding,
	// the status's ReplyLen and the Echo's, which bounds what Wrap adds, on top.
	framed := limit - core.ChainPadding - 2*headerLen
	for _, c := range []struct {
		name    string
		reply   int // the Echo's: its header and its payload
		records int
	}{
		{"framed at the cap", framed, 1},
		{"framed one byte past the cap", framed + 1, 2},
	} {
		t.Run(c.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				m := metrics.New("v", "c")
				d := adapter.Deps{Metrics: m}
				proto := edited{testproto.New(d), func(st *core.Stages) {
					st.Security = growsWrap{Security: st.Security, codec: st.Header}
				}}
				s := newSeamWith(t, proto, 64<<20, d)
				s.serve(testproto.Msg{Op: testproto.OpEcho, Version: 1, ID: 1, Payload: make([]byte, c.reply-headerLen)}, testproto.Msg{Op: 99, Version: 1, ID: 2})
				var got []testproto.Reply
				records := 0
				for len(got) < 2 {
					rs, err := s.client.Recv()
					if err != nil {
						t.Fatalf("record %d after replies %+v: %v", records+1, got, err)
					}
					records++
					got = append(got, rs...)
				}
				if records != c.records || got[0].ID != 1 || got[1].ID != 2 || got[1].Status != testproto.StatusNotSupported {
					t.Errorf("the chain is answered in %d records, the second reply %d of status %d: want %d, the refusal of 2 second", records, got[1].ID, got[1].Status, c.records)
				}
				vals, _ := scrape(t, m)
				for reason, want := range map[string]float64{"record": float64(c.records - 1), "quarter": 0} {
					if k := "adapter_chain_splits_total{protocol=" + testproto.Name + ",reason=" + reason + "}"; vals[k] != want {
						t.Errorf("%s = %v, want %v", k, vals[k], want)
					}
				}
			})
		})
	}
}

// wrapsUnder is the stages' Security, recording the operation of the header
// each Wrap is handed: a pass-through, where which header Wrap gets is what
// is checked.
type wrapsUnder struct {
	core.Security
	mu  sync.Mutex
	ops []uint32
}

func (w *wrapsUnder) Wrap(h core.Header, reply *resources.Buf) error {
	w.mu.Lock()
	w.ops = append(w.ops, h.Op())
	w.mu.Unlock()
	return w.Security.Wrap(h, reply)
}

// A chain's joined record is wrapped once, under its first member's header.
func TestAChainsRecordIsWrappedOnceUnderItsFirstHeader(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := &wrapsUnder{}
		s := newChained(t, 1<<20, adapter.Deps{}, func(st *core.Stages) {
			w.Security = st.Security
			st.Security = w
		})
		w.mu.Lock()
		w.ops = nil
		w.mu.Unlock()
		s.serve(
			testproto.Msg{Op: testproto.OpPutFH, Version: 2, ID: 70, Session: 7, Tree: 1, Handle: s.root},
			testproto.Msg{Op: testproto.OpCreate, Version: 2, ID: 71, Session: inherited, Tree: 1, Name: "w", Related: true},
		)
		if rs, err := s.client.Recv(); err != nil || len(rs) != 2 {
			t.Fatalf("the chain is answered %+v, %v", rs, err)
		}
		w.mu.Lock()
		defer w.mu.Unlock()
		if !slices.Equal(w.ops, []uint32{testproto.OpPutFH}) {
			t.Errorf("Wrap was handed headers of operations %v, want the PutFH's once", w.ops)
		}
	})
}

// undecryptableAfter is the stages' Security whose Unwrap says close, a nil
// header, from its call after the first n: an input.
type undecryptableAfter struct {
	core.Security
	n     int32
	calls atomic.Int32
}

func (u *undecryptableAfter) Unwrap(h core.Header, rec *resources.Buf, prev core.Header) (core.Header, error) {
	if u.calls.Add(1) > u.n {
		return nil, errUndecryptable
	}
	return u.Security.Unwrap(h, rec, prev)
}

// A chain whose second member Unwrap says to close closes the connection
// with Unwrap's error, and sends nothing, the first member's reply included,
// whose reservation goes back.
func TestAChainThatUnwrapClosesSendsNothing(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := newSeam(t, edited{testproto.New(adapter.Deps{}), func(st *core.Stages) {
			st.Security = &undecryptableAfter{Security: st.Security, n: 1}
		}}, 1<<20)
		s.serve(testproto.Msg{Op: testproto.OpEcho, Version: 1, ID: 1}, testproto.Msg{Op: testproto.OpEcho, Version: 1, ID: 2})
		synctest.Wait()
		select {
		case err := <-s.closed:
			if !errors.Is(err, errUndecryptable) {
				t.Errorf("the connection closed with %v, want Unwrap's error", err)
			}
		default:
			t.Error("the connection was left open")
		}
		if !s.sentNothing() {
			t.Error("a chain Unwrap closed was answered")
		}
	})
}

// failsEncode is the stages' HeaderCodec whose Encode fails once armed, at
// its call numbered at: an input standing for a status that does not encode.
type failsEncode struct {
	core.HeaderCodec
	armed atomic.Bool
	n     atomic.Int32
	at    int32
}

var errEncode = errors.New("a status that does not encode")

func (f *failsEncode) Encode(h core.Header, s core.Status, out *resources.Buf) error {
	if f.armed.Load() && f.n.Add(1) == f.at {
		return errEncode
	}
	return f.HeaderCodec.Encode(h, s, out)
}

// A chain whose status does not encode, a reply held before it, closes the
// connection with the encoder's error, sends nothing, releases the held
// reply's reservation and ends its request.
func TestAChainWhoseStatusDoesNotEncodeReleasesWhatItHolds(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m := metrics.New("v", "c")
		fe := &failsEncode{at: 2}
		s := newChained(t, 1<<20, adapter.Deps{Metrics: m}, func(st *core.Stages) {
			fe.HeaderCodec = st.Header
			st.Header = fe
		})
		fe.armed.Store(true)
		s.serve(
			testproto.Msg{Op: testproto.OpPutFH, Version: 2, ID: 20, Session: 7, Handle: s.root},
			testproto.Msg{Op: testproto.OpCreate, Version: 2, ID: 21, Session: inherited, Tree: 1, Name: "b", Related: true},
		)
		if err := s.closeErr(t); err == nil || !strings.Contains(err.Error(), "a wiring fault: a status that does not encode") {
			t.Errorf("the connection closed with %v, want the wiring fault the encoder's error wraps", err)
		}
		synctest.Wait()
		if r := s.budgets.Reply.Reserved(); r != 0 {
			t.Errorf("the reply budget holds %d bytes after the chain ended", r)
		}
		got, _ := scrape(t, m)
		if k := "adapter_requests_in_flight{protocol=" + testproto.Name + "}"; got[k] != 0 {
			t.Errorf("%s = %v, want 0", k, got[k])
		}
		if !s.sentNothing() {
			t.Error("a chain that closed on its status sent a reply")
		}
	})
}

// A chain member admitted after close does not run, and neither does any
// member after it: no file is made, nothing is sent, and the reply held
// before it is released.
func TestAChainMemberAdmittedAfterCloseDoesNotRun(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var unwraps atomic.Int32
		var closeIt func()
		s := newChained(t, 1<<20, adapter.Deps{}, func(st *core.Stages) {
			st.Security = closesOnUnwrap{st.Security, func() {
				if unwraps.Add(1) == 2 && closeIt != nil {
					closeIt()
				}
			}}
		})
		closeIt = func() { s.p.Close() }
		unwraps.Store(0)
		s.serve(
			echoIn(1, 7),
			testproto.Msg{Op: testproto.OpPutFH, Version: 2, ID: 2, Session: 7, Tree: 1, Handle: s.root},
			testproto.Msg{Op: testproto.OpCreate, Version: 2, ID: 3, Session: inherited, Tree: 1, Name: "late", Related: true},
		)
		synctest.Wait()
		if n := s.creates(); n != 0 {
			t.Errorf("a member admitted after close made %d files", n)
		}
		if r := s.budgets.Reply.Reserved(); r != 0 {
			t.Errorf("the reply budget holds %d bytes after the chain ended", r)
		}
		if !s.sentNothing() {
			t.Error("a chain closed under it sent a reply")
		}
	})
}

// A replay hit's copy and a refusal's status keep to a quarter of the reply
// pool as a reply does: on a pool whose quarter is 16 KiB, a reply of 12
// KiB held before either sends that reply first, counted quarter.
func TestAHitAndAStatusKeepToAQuarterOfThePool(t *testing.T) {
	const budget = 64 << 10
	quarter := func(t *testing.T, m *metrics.Registry) float64 {
		got, _ := scrape(t, m)
		return got["adapter_chain_splits_total{protocol="+testproto.Name+",reason=quarter}"]
	}
	t.Run("hit", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			m := metrics.New("v", "c")
			s := newChained(t, budget, adapter.Deps{Metrics: m, Budgets: serverBudgets(budget)})
			s.serve(sequenced(s, 150)...)
			recvIDs(t, s, 2)
			before := quarter(t, m)
			s.serve(sequenced(s, 150)...)
			if got := recvIDs(t, s, 2); !slices.Equal(got[0], []uint64{150}) || !slices.Equal(got[1], []uint64{151}) {
				t.Errorf("the records hold %v, want the PutFH, then the hit", got)
			}
			if n := quarter(t, m) - before; n != 1 {
				t.Errorf("the hit counted %v quarter splits, want 1", n)
			}
		})
	})
	t.Run("status", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			m := metrics.New("v", "c")
			d := adapter.Deps{Metrics: m}
			s := newSeamWith(t, testproto.New(d).Bare(), budget, d)
			s.serve(testproto.Msg{Op: testproto.OpEcho, Version: 1, ID: 1}, testproto.Msg{Op: 99, Version: 1, ID: 2})
			if r := s.read(); r.ID != 1 {
				t.Errorf("the first record is %+v, want the Echo alone", r)
			}
			if r := s.read(); r.ID != 2 || r.Status != testproto.StatusNotSupported {
				t.Errorf("the second record is %+v, want the refusal", r)
			}
			if n := quarter(t, m); n != 1 {
				t.Errorf("the status counted %v quarter splits, want 1", n)
			}
		})
	})
}

// overruns is the stages' HeaderCodec whose Encode, once armed, appends 20
// KiB past each reply: an input standing for an encoder whose bound is short.
type overruns struct {
	core.HeaderCodec
	armed atomic.Bool
}

func (o *overruns) Encode(h core.Header, s core.Status, out *resources.Buf) error {
	if err := o.HeaderCodec.Encode(h, s, out); err != nil || !o.armed.Load() {
		return err
	}
	out.B = append(out.B, make([]byte, 20<<10)...)
	return nil
}

// A chain whose replies an encoder grew past their charges goes out as one
// record, as a request alone with such a reply does: the record's room never
// passes its members' charges.
func TestAChainOfOverrunRepliesGoesOut(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		o := &overruns{}
		s := newChained(t, 1<<20, adapter.Deps{}, func(st *core.Stages) {
			o.HeaderCodec = st.Header
			st.Header = o
		})
		o.armed.Store(true)
		s.serve(echoIn(1, 7), echoIn(2, 7))
		if rs, err := s.client.Recv(); err != nil || len(rs) != 2 || rs[0].ID != 1 || rs[1].ID != 2 {
			t.Errorf("the chain is answered %+v, %v: want both replies in one record", rs, err)
		}
	})
}

// prevsOf is the stages' Security, recording the member before each Unwrap
// is handed: a pass-through, where which header Unwrap gets is what is
// checked.
type prevsOf struct {
	core.Security
	mu    sync.Mutex
	prevs []core.Header
}

func (r *prevsOf) Unwrap(h core.Header, rec *resources.Buf, prev core.Header) (core.Header, error) {
	r.mu.Lock()
	r.prevs = append(r.prevs, prev)
	r.mu.Unlock()
	return r.Security.Unwrap(h, rec, prev)
}

// refusesAt is the stages' HeaderCodec refusing the header at off, its next
// kept: an input standing for a header whose fields do not decode while its
// step does. It keeps the header it returned at off.
type refusesAt struct {
	core.HeaderCodec
	off     int
	mu      sync.Mutex
	refused core.Header
}

func (c *refusesAt) Decode(rec *resources.Buf, off int) (core.Header, error) {
	h, err := c.HeaderCodec.Decode(rec, off)
	if err == nil && off == c.off {
		c.mu.Lock()
		c.refused = h
		c.mu.Unlock()
		return h, errors.New("a header refused, its next kept")
	}
	return h, err
}

// A related member after a member whose header Decode refused is unwrapped
// under that member's header, not the one before it.
func TestARelatedMemberAfterADecodeRefusalUnwrapsUnderIt(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		prevs, codec := &prevsOf{}, &refusesAt{off: -1}
		s := newChained(t, 1<<20, adapter.Deps{}, func(st *core.Stages) {
			prevs.Security = st.Security
			st.Security = prevs
			codec.HeaderCodec = st.Header
			st.Header = codec
		})
		b, offs := chainOf(s.f, echoIn(1, 7), echoIn(2, 7), echoIn(3, inherited))
		codec.off = offs[1]
		s.serveBody(b)
		for range 2 {
			if _, err := s.client.Recv(); err != nil {
				t.Fatal(err)
			}
		}
		prevs.mu.Lock()
		defer prevs.mu.Unlock()
		codec.mu.Lock()
		defer codec.mu.Unlock()
		if last := prevs.prevs[len(prevs.prevs)-1]; codec.refused == nil || last != codec.refused {
			t.Errorf("the third member was unwrapped under %v, want the refused second's %v", last, codec.refused)
		}
	})
}

// A related member after a refused first member, its status with no room
// while the refusal's is held, sends that refusal first, counted reply_pool
// under its own request: the chain's member is the one it refuses.
func TestARefusedMembersSplitCountsUnderIt(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const budget = 400_000
		m := metrics.New("v", "c")
		s := newChained(t, budget, adapter.Deps{Metrics: m})
		rest, err := s.budgets.Reply.Reserve(context.Background(), peer, budget-s.reply.Charge(0))
		if err != nil {
			t.Fatal(err)
		}
		b, _ := chainOf(s.f, echoIn(80, 7), echoIn(81, inherited))
		b[headerLen+1] ^= 1 // a byte of the first's payload
		s.serveBody(b)
		synctest.Wait()
		rest.Release()
		if r := s.recvOne(); r.ID != 80 || r.Status != testproto.StatusChecksum {
			t.Errorf("the first record is %+v, want the first refused its checksum", r)
		}
		if r := s.recvOne(); r.ID != 81 || r.Status != testproto.StatusChecksum {
			t.Errorf("the second record is %+v, want the related member answered the refusal's status", r)
		}
		got, _ := scrape(t, m)
		if k := "adapter_chain_splits_total{protocol=" + testproto.Name + ",reason=reply_pool}"; got[k] != 1 {
			t.Errorf("%s = %v, want 1", k, got[k])
		}
	})
}

// A success clears a chain's failure: after a member refused and an
// unrelated one answered, a related member runs and is answered its own
// reply, not the refusal's status.
func TestASuccessClearsAChainsFailure(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := newSeam(t, testproto.New(adapter.Deps{}).Bare(), 1<<20)
		s.serve(
			testproto.Msg{Op: 99, Version: 1, ID: 1},
			testproto.Msg{Op: testproto.OpEcho, Version: 1, ID: 2},
			testproto.Msg{Op: testproto.OpEcho, Version: 1, ID: 3, Related: true, Payload: []byte("z")},
		)
		rs, err := s.client.Recv()
		if err != nil || len(rs) != 3 || rs[0].Status != testproto.StatusNotSupported || rs[1].Status != testproto.StatusOK || rs[2].ID != 3 || rs[2].Status != testproto.StatusOK || string(rs[2].Body) != "z" {
			t.Errorf("the chain is answered %+v, %v: want the refusal, then both Echo answered", rs, err)
		}
	})
}

// The headers a drain's interim is handed stop at a step that is not
// forward: through a lax codec, a third member whose next points back at the
// second's header ends them, each header read once.
func TestLaterHeadersStopAtABadStep(t *testing.T) {
	codec := &laxCodec{}
	st := testproto.New(adapter.Deps{}).Bare().NewConn(nil)
	codec.HeaderCodec = st.Header
	st.Header = codec
	echo := func(id uint64) testproto.Msg { return testproto.Msg{Op: testproto.OpEcho, Version: 1, ID: id} }
	b, offs := chainOf(testproto.New(adapter.Deps{}).Framer(), echo(1), echo(2), echo(3))
	binary.BigEndian.PutUint32(b[offs[2]:], uint32(offs[1]))
	rec := &resources.Buf{B: b}
	first, err := codec.Decode(rec, 0)
	if err != nil {
		t.Fatal(err)
	}
	codec.reads = nil
	hs := core.Later(st, rec, 0, first)
	read := make([]int, len(codec.reads))
	for i, r := range codec.reads {
		read[i] = r.off
	}
	if len(hs) != 2 || !slices.Equal(read, []int{offs[1], offs[2]}) {
		t.Errorf("the interim is handed %d headers, read at %v: want the second's and the third's, at %d and %d", len(hs), read, offs[1], offs[2])
	}
}

// failsWrap is the stages' Security whose Wrap fails once armed: an input
// standing for a record that does not seal.
type failsWrap struct {
	core.Security
	armed atomic.Bool
}

var errWrap = errors.New("a record that does not wrap")

func (f *failsWrap) Wrap(h core.Header, reply *resources.Buf) error {
	if f.armed.Load() {
		return errWrap
	}
	return f.Security.Wrap(h, reply)
}

// A split whose record does not wrap closes the connection, and the chain
// runs nothing more: the member after the split neither calls nor holds a
// reply, every reservation is released and every request ended.
func TestASplitThatDoesNotWrapStopsTheChain(t *testing.T) {
	for _, c := range []struct {
		name   string
		budget int64
		serve  func(s *served)
	}{
		{"unwrap", 1 << 20, func(s *served) {
			b, offs := chainOf(s.f, echoIn(1, 7), echoIn(2, 7))
			b[offs[1]+headerLen+1] ^= 1 // a byte of the second's payload
			s.serveBody(b)
		}},
		{"quarter", 64 << 10, func(s *served) {
			s.serve(
				testproto.Msg{Op: testproto.OpPutFH, Version: 2, ID: 10, Session: 7, Tree: 1, Handle: s.root},
				testproto.Msg{Op: testproto.OpCreate, Version: 2, ID: 11, Session: inherited, Tree: 1, Name: "late", Related: true},
			)
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				m := metrics.New("v", "c")
				fw := &failsWrap{}
				s := newChained(t, c.budget, adapter.Deps{Metrics: m}, func(st *core.Stages) {
					fw.Security = st.Security
					st.Security = fw
				})
				fw.armed.Store(true)
				c.serve(s)
				if err := s.closeErr(t); err == nil || !strings.Contains(err.Error(), "a wiring fault: a record that does not wrap") {
					t.Errorf("the connection closed with %v, want the wiring fault Wrap's error wraps", err)
				}
				synctest.Wait()
				if n := s.creates(); n != 0 {
					t.Errorf("a member after the split made %d files", n)
				}
				if r := s.budgets.Reply.Reserved(); r != 0 {
					t.Errorf("the reply budget holds %d bytes after the chain stopped", r)
				}
				got, _ := scrape(t, m)
				for k, v := range got {
					if strings.HasPrefix(k, "adapter_requests_in_flight{") && v != 0 {
						t.Errorf("%s = %v after the chain stopped", k, v)
					}
				}
			})
		})
	}
}

// recordsOf reads n records and answers each one's IDs and whether each
// reply carries a checksum.
func recordsOf(t *testing.T, s *served, n int) (ids [][]uint64, sums [][]bool) {
	t.Helper()
	for range n {
		rs, err := s.client.Recv()
		if err != nil {
			t.Fatal(err)
		}
		var i []uint64
		var c []bool
		for _, r := range rs {
			i, c = append(i, r.ID), append(c, r.Checksum != 0)
		}
		ids, sums = append(ids, i), append(sums, c)
	}
	return ids, sums
}

// A refusal's record keeps its marks across a split inside it: on a pool
// whose quarter is two small replies, a refused member, two related ones and
// an unrelated fifth go out as the refusal and the first related, the second
// related alone, and the fifth alone. After an Unwrap refusal each of them is
// wrapped; after a Decode refusal the refusal's records are not, and the
// fifth is.
func TestARefusalsRecordKeepsItsMarksAcrossASplit(t *testing.T) {
	want := [][]uint64{{1}, {2, 3}, {4}, {5}}
	t.Run("unwrap", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			s := newChained(t, 8*smallCharge, adapter.Deps{})
			s.serve(testproto.Msg{Op: testproto.OpSession, Version: 2, Session: 9})
			if r := s.read(); r.Status != testproto.StatusOK {
				t.Fatalf("session 9: %+v", r)
			}
			b, offs := chainOf(s.f, echoIn(1, 7), echoIn(2, 9), echoIn(3, inherited), echoIn(4, inherited), echoIn(5, 7))
			b[offs[1]+headerLen+1] ^= 1
			s.serveBody(b)
			if ids, sums := recordsOf(t, s, 4); !slices.EqualFunc(ids, want, slices.Equal) || !sums[2][0] || !sums[3][0] {
				t.Errorf("the records are %v, checksummed %v: want %v, each wrapped", ids, sums, want)
			}
		})
	})
	t.Run("decode", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			codec := &refusesAt{off: -1}
			s := newChained(t, 8*smallCharge, adapter.Deps{}, func(st *core.Stages) {
				codec.HeaderCodec = st.Header
				st.Header = codec
			})
			b, offs := chainOf(s.f, echoIn(1, 7), echoIn(2, 7), echoIn(3, inherited), echoIn(4, inherited), echoIn(5, 7))
			codec.off = offs[1]
			s.serveBody(b)
			ids, sums := recordsOf(t, s, 4)
			if !slices.EqualFunc(ids, want, slices.Equal) || sums[1][0] || sums[1][1] || sums[2][0] || !sums[3][0] {
				t.Errorf("the records are %v, checksummed %v: want %v, the refusal's two unwrapped and the fifth wrapped", ids, sums, want)
			}
		})
	})
}

// A refusal of a header Decode refused, its status finding no room at once,
// goes out unwrapped: the flush before its wait holds nothing and changes
// nothing.
func TestADecodeRefusalThatWaitsGoesOutUnwrapped(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const budget = 400_000
		codec := &refusesAt{off: -1}
		s := newChained(t, budget, adapter.Deps{}, func(st *core.Stages) {
			codec.HeaderCodec = st.Header
			st.Header = codec
		})
		rest, err := s.budgets.Reply.Reserve(context.Background(), peer, budget-s.reply.Charge(0))
		if err != nil {
			t.Fatal(err)
		}
		b, offs := chainOf(s.f, echoIn(1, 7), echoIn(2, 7))
		codec.off = offs[1]
		s.serveBody(b)
		synctest.Wait()
		r1, r2 := s.recvOne(), s.recvOne()
		rest.Release()
		if r1.ID != 1 || r1.Checksum == 0 || r2.ID != 2 || r2.Checksum != 0 {
			t.Errorf("the records are %+v and %+v, want the Echo wrapped, then the refusal unwrapped", r1, r2)
		}
	})
}

// wrapRoom is the stages' Security recording, at each Wrap, the room past
// the record's bytes and what the stages' HeaderCodec bounds for the header
// it is handed: a pass-through, where the room Wrap gets is what is checked.
type wrapRoom struct {
	core.Security
	codec core.HeaderCodec
	mu    sync.Mutex
	room  []int
	need  []int
}

func (w *wrapRoom) Wrap(h core.Header, reply *resources.Buf) error {
	w.mu.Lock()
	w.room = append(w.room, cap(reply.B)-len(reply.B))
	w.need = append(w.need, w.codec.ReplyLen(h))
	w.mu.Unlock()
	return w.Security.Wrap(h, reply)
}

// A chain's joined record leaves Wrap the room its first member's ReplyLen
// counts, which bounds what Wrap adds in place.
func TestAJoinedRecordLeavesWrapItsRoom(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := &wrapRoom{}
		s := newChained(t, 1<<20, adapter.Deps{}, func(st *core.Stages) {
			w.Security, w.codec = st.Security, st.Header
			st.Security = w
		})
		w.mu.Lock()
		w.room, w.need = nil, nil
		w.mu.Unlock()
		s.serve(echoIn(1, 7), echoIn(2, 7), echoIn(3, 7))
		if rs, err := s.client.Recv(); err != nil || len(rs) != 3 {
			t.Fatalf("the chain is answered %+v, %v", rs, err)
		}
		w.mu.Lock()
		defer w.mu.Unlock()
		if len(w.room) != 1 || w.room[0] < w.need[0] {
			t.Errorf("Wrap was handed rooms %v, want one record's of at least %v", w.room, w.need)
		}
	})
}

// nilAt is the stages' HeaderCodec answering no header at off: an input
// standing for a header that does not decode at all.
type nilAt struct {
	core.HeaderCodec
	off int
}

func (c *nilAt) Decode(rec *resources.Buf, off int) (core.Header, error) {
	if off == c.off {
		return nil, errors.New("a header that does not decode at all")
	}
	return c.HeaderCodec.Decode(rec, off)
}

// A chain whose second header does not decode at all closes the connection
// with ErrDecode and sends nothing, the first member's reply included.
func TestAChainWhoseHeaderDoesNotDecodeSendsNothing(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		codec := &nilAt{off: -1}
		s := newChained(t, 1<<20, adapter.Deps{}, func(st *core.Stages) {
			codec.HeaderCodec = st.Header
			st.Header = codec
		})
		b, offs := chainOf(s.f, echoIn(1, 7), echoIn(2, 7))
		codec.off = offs[1]
		s.serveBody(b)
		if err := s.closeErr(t); !errors.Is(err, core.ErrDecode) {
			t.Errorf("the connection closed with %v, want ErrDecode", err)
		}
		if !s.sentNothing() {
			t.Error("a chain whose header did not decode sent a reply")
		}
	})
}

// An unrelated member after a member whose header Decode refused goes out in
// a record of its own, wrapped, after the refusal's, which is not.
func TestAnUnrelatedMemberAfterADecodeRefusalIsAlone(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		codec := &refusesAt{off: -1}
		s := newChained(t, 1<<20, adapter.Deps{}, func(st *core.Stages) {
			codec.HeaderCodec = st.Header
			st.Header = codec
		})
		b, offs := chainOf(s.f, echoIn(1, 7), echoIn(2, 7), echoIn(3, 7))
		codec.off = offs[1]
		s.serveBody(b)
		ids, sums := recordsOf(t, s, 3)
		if !slices.EqualFunc(ids, [][]uint64{{1}, {2}, {3}}, slices.Equal) || sums[1][0] || !sums[2][0] {
			t.Errorf("the records are %v, checksummed %v: want each member alone, the refusal unwrapped and the third wrapped", ids, sums)
		}
	})
}

// The headers a drain's interim is handed end at one that comes back with
// an error.
func TestLaterHeadersEndAtOneThatDoesNotDecode(t *testing.T) {
	codec := &refusesAt{off: -1}
	st := testproto.New(adapter.Deps{}).Bare().NewConn(nil)
	codec.HeaderCodec = st.Header
	st.Header = codec
	echo := func(id uint64) testproto.Msg { return testproto.Msg{Op: testproto.OpEcho, Version: 1, ID: id} }
	b, offs := chainOf(testproto.New(adapter.Deps{}).Framer(), echo(1), echo(2), echo(3))
	codec.off = offs[2]
	rec := &resources.Buf{B: b}
	first, err := codec.Decode(rec, 0)
	if err != nil {
		t.Fatal(err)
	}
	if hs := core.Later(st, rec, 0, first); len(hs) != 1 {
		t.Errorf("the interim is handed %d headers, want the second's alone", len(hs))
	}
}

// A flow split whose record does not wrap closes the connection, and the
// member that was to wait in Admit is dropped once its wait ends on the
// close: it is neither refused nor answered, and no error is mapped for it.
func TestAFlowSplitThatDoesNotWrapDropsTheMemberWaiting(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m := metrics.New("v", "c")
		flow, fw := &gatedFlow{gate: make(chan struct{})}, &failsWrap{}
		s := newChained(t, 1<<20, adapter.Deps{Metrics: m}, func(st *core.Stages) {
			flow.FlowControl, fw.Security = st.Flow, st.Security
			st.Flow, st.Security = flow, fw
		})
		flow.armed.Store(true)
		fw.armed.Store(true)
		s.serve(echoIn(70, 7), echoIn(71, 7))
		if err := s.closeErr(t); err == nil || !strings.Contains(err.Error(), "a wiring fault: a record that does not wrap") {
			t.Errorf("the connection closed with %v, want the wiring fault Wrap's error wraps", err)
		}
		synctest.Wait()
		if r := s.budgets.Reply.Reserved(); r != 0 {
			t.Errorf("the reply budget holds %d bytes after the chain stopped", r)
		}
		got, _ := scrape(t, m)
		if k := "adapter_unmapped_errors_total{protocol=" + testproto.Name + "}"; got[k] != 0 {
			t.Errorf("%s = %v, want 0", k, got[k])
		}
		for k, v := range got {
			if strings.HasPrefix(k, "adapter_requests_in_flight{") && v != 0 {
				t.Errorf("%s = %v after the chain stopped", k, v)
			}
		}
	})
}

// closeErr is the error the connection closed with, failing the test once an
// hour passes on the bubble's clock with no close.
func (s *seam) closeErr(t *testing.T) error {
	t.Helper()
	select {
	case err := <-s.closed:
		return err
	case <-time.After(time.Hour):
		t.Fatal("the connection never closed")
		return nil
	}
}

// A chain runs off the reader: serving it returns while its member is held
// in the service, so the connection is read on.
func TestAChainRunsOffTheReader(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := newChained(t, 1<<20, adapter.Deps{})
		hold := s.faults.Hold(vfs.OpCreate)
		served := make(chan struct{})
		go func() {
			s.serve(
				testproto.Msg{Op: testproto.OpPutFH, Version: 2, ID: 90, Session: 7, Tree: 1, Handle: s.root},
				testproto.Msg{Op: testproto.OpCreate, Version: 2, ID: 91, Session: inherited, Tree: 1, Name: "off", Related: true},
			)
			close(served)
		}()
		<-hold.Reached()
		// A second on the bubble's clock is well inside the held call's own
		// deadline, which would otherwise end the hold and the chain.
		select {
		case <-served:
		case <-time.After(time.Second):
			t.Error("serving the chain did not return while its member was held")
		}
		hold.Release()
		if rs, err := s.client.Recv(); err != nil || len(rs) != 2 {
			t.Errorf("the chain is answered %+v, %v", rs, err)
		}
	})
}

// An Unwrap refusal of a related member after a Decode refusal's record goes
// out in a record of its own, wrapped, where the Decode refusal's is not.
func TestAnUnwrapRefusalAfterADecodeRefusalIsWrapped(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		codec := &refusesAt{off: -1}
		s := newChained(t, 1<<20, adapter.Deps{}, func(st *core.Stages) {
			codec.HeaderCodec = st.Header
			st.Header = codec
		})
		b, offs := chainOf(s.f, echoIn(1, 7), echoIn(2, 7), echoIn(3, inherited))
		codec.off = offs[1]
		b[offs[2]+headerLen+1] ^= 1 // a byte of the third's payload
		s.serveBody(b)
		ids, sums := recordsOf(t, s, 3)
		if !slices.EqualFunc(ids, [][]uint64{{1}, {2}, {3}}, slices.Equal) || sums[1][0] || !sums[2][0] {
			t.Errorf("the records are %v, checksummed %v: want each member alone, the Decode refusal unwrapped and the Unwrap refusal wrapped", ids, sums)
		}
	})
}

// A later member Admit refuses outright, not for a wait, is answered its
// refusal in the chain's record, with no split: only a wait splits.
func TestALaterMemberAdmitRefusesSharesTheRecord(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m := metrics.New("v", "c")
		s := newChained(t, 1<<20, adapter.Deps{Metrics: m})
		over := echoIn(2, 7)
		over.Credits = 1000
		s.serve(echoIn(1, 7), over)
		rs, err := s.client.Recv()
		if err != nil || len(rs) != 2 || rs[0].Status != testproto.StatusOK || rs[1].ID != 2 || rs[1].Status != testproto.StatusOverdraw {
			t.Errorf("the chain is answered %+v, %v: want the Echo and the overdraw refused, in one record", rs, err)
		}
		got, _ := scrape(t, m)
		if k := "adapter_chain_splits_total{protocol=" + testproto.Name + ",reason=flow}"; got[k] != 0 {
			t.Errorf("%s = %v, want 0", k, got[k])
		}
	})
}

// With the reply pool full, chains whose first member Unwrap refuses hold
// the connection's one status wait, as refusals alone do: a reader serving
// ten in turn is held at the second until the first's status is reserved,
// so no more than one record waits for a status, and all ten are served once
// the pool has room.
func TestChainsWaitingForAStatusHoldTheReader(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := newChained(t, 1<<20, adapter.Deps{})
		synctest.Wait() // the setup's last reply released, so the pool is filled whole
		held, err := s.budgets.Reply.Reserve(context.Background(), peer, (1<<20)-s.budgets.Reply.Reserved())
		if err != nil {
			t.Fatal(err)
		}
		var served atomic.Int32
		done := make(chan struct{})
		go func() {
			defer close(done)
			for i := range uint64(10) {
				b, offs := chainOf(s.f, echoIn(10*i+1, 7), echoIn(10*i+2, 7))
				b[headerLen+1] ^= 1         // the first member fails Unwrap
				b[offs[1]+headerLen+1] ^= 1 // and so does the second
				s.serveBody(b)
				served.Add(1)
			}
		}()
		synctest.Wait()
		if n := served.Load(); n != 1 {
			t.Errorf("the reader served %d chains while one waited for a status, want 1", n)
		}
		held.Release()
		<-done
	})
}

// A chain's later member waiting for its status holds the connection's one
// status wait too: with the pool's room taken by the chain's first reply,
// unread, a refusal alone served after it holds the reader until the
// chain's status is reserved.
func TestAChainsLaterStatusHoldsTheOneStatusWait(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := newChained(t, 1<<20, adapter.Deps{})
		synctest.Wait() // the setup's last reply released, so the pool is filled exactly
		held, err := s.budgets.Reply.Reserve(context.Background(), peer, (1<<20)-s.budgets.Reply.Reserved()-s.reply.Charge(0))
		if err != nil {
			t.Fatal(err)
		}
		b, offs := chainOf(s.f, echoIn(1, 7), echoIn(2, 7))
		b[offs[1]+headerLen+1] ^= 1 // the second member fails Unwrap
		s.serveBody(b)
		synctest.Wait()
		lone := body(s.f, echoIn(3, 7))
		lone[len(lone)-1] ^= 0x01
		served := make(chan struct{})
		go func() {
			s.serveBody(lone)
			close(served)
		}()
		synctest.Wait()
		select {
		case <-served:
			t.Error("a refusal alone was served while the chain's member waited for its status")
		default:
		}
		held.Release()
		<-served
	})
}

// statusWaiters is each place a status waits for the connection's one
// status wait: on the reader, for a refusal alone and for a chain's first
// member, and on its chain's goroutine for a later member, whose chain's
// first reply, unread, takes the room the test leaves.
var statusWaiters = []struct {
	name   string
	room   bool // the test leaves the room of one reply, the chain's first
	record func(f core.Framer) []byte
}{
	{"a refusal alone", false, func(f core.Framer) []byte {
		b := body(f, echoIn(1, 7))
		b[len(b)-1] ^= 0x01 // it fails Unwrap
		return b
	}},
	{"a chain's first member", false, func(f core.Framer) []byte {
		b, _ := chainOf(f, echoIn(1, 7), echoIn(2, 7))
		b[headerLen+1] ^= 1 // the first member fails Unwrap
		return b
	}},
	{"a chain's later member", true, func(f core.Framer) []byte {
		b, offs := chainOf(f, echoIn(1, 7), echoIn(2, 7))
		b[offs[1]+headerLen+1] ^= 1 // the second member fails Unwrap
		return b
	}},
}

// fillBut fills s's reply pool but for the room of one small reply where
// room, and returns what frees it.
func fillBut(t *testing.T, s *served, room bool) func() {
	t.Helper()
	synctest.Wait() // the setup's last reply released, so the pool is filled exactly
	left := int64(0)
	if room {
		left = s.reply.Charge(0)
	}
	held, err := s.budgets.Reply.Reserve(context.Background(), peer, (1<<20)-s.budgets.Reply.Reserved()-left)
	if err != nil {
		t.Fatal(err)
	}
	return held.Release
}

// A status still waiting for the connection's one status wait 30 s after it
// began to wait, by the server's clock, closes the connection with the
// status-wait error, at 30 s and not before, wherever it waits. The test
// holds the status wait: a holder that outlasts the status (RFC 20 §6.4,
// §10.1).
func TestAStatusWaitingForTheStatusWaitClosesAt30s(t *testing.T) {
	for _, w := range statusWaiters {
		t.Run(w.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				clk := newManualClock()
				s := newHeld(t, 1<<20, adapter.Deps{}, clk)
				give := s.p.HoldStatusWait()
				defer give()
				defer fillBut(t, s, w.room)()
				served := start(func() { s.serveBody(w.record(s.f)) })
				synctest.Wait() // the status waits for the status wait, its deadline armed
				clk.Advance(30*time.Second - time.Nanosecond)
				synctest.Wait()
				select {
				case err := <-s.closed:
					t.Fatalf("the connection was closed with %v before 30 s", err)
				default:
				}
				clk.Advance(time.Nanosecond)
				synctest.Wait()
				select {
				case err := <-s.closed:
					if !errors.Is(err, core.ErrStatusWait) {
						t.Errorf("the connection was closed with %v, want the status-wait error", err)
					}
				default:
					t.Fatal("the status still waits at 30 s, want its connection closed")
				}
				<-served.done
			})
		})
	}
}

// A status that took the connection's one status wait after waiting 10 s
// for it, and finds no buffer, closes the connection with the reply-wait
// error 30 s after it began to wait for the status wait, not 30 s after it
// took it, wherever it waits (RFC 20 §6.4; RFC 24 §2.6).
func TestAStatusWaitsOne30sForTheStatusWaitAndItsBuffer(t *testing.T) {
	for _, w := range statusWaiters {
		t.Run(w.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				s := newChained(t, 1<<20, adapter.Deps{})
				give := s.p.HoldStatusWait()
				defer fillBut(t, s, w.room)()
				begin := time.Now()
				served := start(func() { s.serveBody(w.record(s.f)) })
				select {
				case err := <-s.closed:
					t.Fatalf("the connection was closed with %v while the status waited for the status wait, want it open 10 s on", err)
				case <-time.After(10 * time.Second):
				}
				give()
				select {
				case err := <-s.closed:
					if !errors.Is(err, core.ErrReplyWait) {
						t.Errorf("the connection was closed with %v, want the reply-wait error", err)
					}
				case <-time.After(time.Minute):
					t.Fatal("the status still waits a minute on, want its connection closed at 30 s")
				}
				if waited := time.Since(begin); waited != 30*time.Second {
					t.Errorf("the connection was closed %v after the status began to wait for the status wait, want 30s", waited)
				}
				<-served.done
			})
		})
	}
}

// A related member after a replay hit is routed under the request from
// before the hit: sent again, a sequenced chain's Create is a hit, and the
// related Create after it still finds the PutFH's handle.
func TestARelatedMemberAfterAHitKeepsTheHandle(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := newChained(t, 1<<20, adapter.Deps{Budgets: serverBudgets(1 << 20)})
		after := func(name string) testproto.Msg {
			return testproto.Msg{Op: testproto.OpCreate, Version: 2, ID: 152, Session: inherited, Tree: 1, Name: name, Related: true}
		}
		s.serve(sequenced(s, 150, after("first"))...)
		recvIDs(t, s, 1)
		s.serve(sequenced(s, 150, after("second"))...)
		if rs, err := s.client.Recv(); err != nil || len(rs) != 3 || rs[2].Status != testproto.StatusOK {
			t.Errorf("the chain sent again is answered %+v, %v: want the Create after the hit answered OK", rs, err)
		}
	})
}

// A related member after an unrelated replay hit starts from no request, as
// the cached call did: sent again, the chain [PutFH, an unrelated sequenced
// Echo, cacheable here, a related Create] answers the Create as the first
// send did, with no handle, rather than running it under the PutFH's. The
// Echo's hit is answered OK, so the Create's answer rests on the request it
// is routed under alone.
func TestARelatedMemberAfterAnUnrelatedHitStartsFromNoRequest(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		// Echo's row marked Cacheable: an input standing for a protocol whose
		// cacheable row needs no handle, so a hit of one is answered OK.
		s := newChainedOn(t, 1<<20, adapter.Deps{Budgets: serverBudgets(1 << 20)}, func(p core.Protocol) core.Protocol {
			return retable(p, func(op *core.Op, member bool) {
				if !member && op.Code == testproto.OpEcho {
					op.Cacheable = true
				}
			})
		})
		chain := []testproto.Msg{
			{Op: testproto.OpPutFH, Version: 2, ID: 160, Session: 7, Tree: 1, Handle: s.root},
			{Op: testproto.OpEcho, Version: 2, ID: 161, Session: 7, Tree: 1, Seq: 9},
			{Op: testproto.OpCreate, Version: 2, ID: 162, Session: inherited, Tree: 1, Name: "after", Related: true},
		}
		var sends [2][]core.Status
		for i := range sends {
			s.serve(chain...)
			rs, err := s.client.Recv()
			if err != nil || len(rs) != 3 {
				t.Fatalf("send %d is answered %+v, %v: want three replies", i+1, rs, err)
			}
			for _, r := range rs {
				sends[i] = append(sends[i], r.Status)
			}
		}
		want := []core.Status{testproto.StatusOK, testproto.StatusOK, testproto.StatusNoFH}
		for i, got := range sends {
			if !slices.Equal(got, want) {
				t.Errorf("send %d is answered %v, want %v: the related Create after the unrelated Echo has no handle", i+1, got, want)
			}
		}
	})
}

// A compound row reached as a chain's member is refused not supported, and
// none of its members runs.
func TestACompoundAsAChainMemberIsRefused(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := newSeam(t, testproto.New(adapter.Deps{}).Bare(), 1<<20)
		s.serve(
			testproto.Msg{Op: testproto.OpEcho, Version: 1, ID: 1},
			testproto.Msg{Op: testproto.OpCompound, Version: 1, ID: 2, Members: []testproto.Msg{{Op: testproto.OpEcho, Payload: []byte("m")}}, Tag: []byte{1, 2, 3, 4}},
		)
		rs, err := s.client.Recv()
		if err != nil || len(rs) != 2 || rs[1].ID != 2 || rs[1].Status != testproto.StatusNotSupported {
			t.Fatalf("the chain is answered %+v, %v: want the compound refused not supported", rs, err)
		}
		if _, results, _ := testproto.Results(rs[1].Body); len(results) != 0 {
			t.Errorf("the refused compound answered %d results, want none", len(results))
		}
	})
}

// A compound row refused as a chain's member is answered its status alone:
// its own Invocation's Encode, which here appends a marker, writes nothing
// into a refusal sized only by its header's ReplyLen.
func TestACompoundRefusedAsAChainMemberEncodesNothingOfItsOwn(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		marker := []byte("compound's own encode")
		mark := func(inv core.Invocation) core.Invocation {
			if inv.EncodeHead == nil { // only a compound row sets a head
				return inv
			}
			inv.Encode = func(out *resources.Buf, _ core.Version) error {
				out.B = append(out.B, marker...)
				return nil
			}
			return inv
		}
		s := newSeam(t, wrapRows(testproto.New(adapter.Deps{}).Bare(), mark), 1<<20)
		s.serve(
			testproto.Msg{Op: testproto.OpEcho, Version: 1, ID: 1},
			testproto.Msg{Op: testproto.OpCompound, Version: 1, ID: 2, Members: []testproto.Msg{{Op: testproto.OpEcho, Payload: []byte("m")}}},
		)
		rs, err := s.client.Recv()
		if err != nil || len(rs) != 2 || rs[1].Status != testproto.StatusNotSupported {
			t.Fatalf("the chain is answered %+v, %v: want the compound refused not supported", rs, err)
		}
		if bytes.Contains(rs[1].Body, marker) {
			t.Errorf("the refused compound's reply %q carries its own Encode's bytes", rs[1].Body)
		}
	})
}

// tracingStages is the stages' FlowControl and ReplayCache, each tracing
// its own work as a protocol's stage may: Admit and Lookup start a span
// under the context they are handed and end it as they return. An input
// standing for such a protocol.
type tracingStages struct {
	core.FlowControl
	core.ReplayCache
	tracer trace.Tracer
}

func (t tracingStages) Admit(ctx context.Context, h core.Header) (func(), error) {
	_, span := t.tracer.Start(ctx, "admit")
	defer span.End()
	return t.FlowControl.Admit(ctx, h)
}

func (t tracingStages) Lookup(ctx context.Context, h core.Header) (*resources.Buf, error) {
	_, span := t.tracer.Start(ctx, "lookup")
	defer span.End()
	return t.ReplayCache.Lookup(ctx, h)
}

// A chain's member is tried at flow control and the replay cache under its
// own context, ended: a stage that traces its work there has its span in
// the member's trace, a child of the member's request span, and so does a
// request alone's, admitted under its context and tried at the cache under
// its ended one (RFC 20 sections 4.7 and 10.2).
func TestAChainMembersTriesAreInItsTrace(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		spans := tracetest.NewSpanRecorder()
		tracer := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(spans)).Tracer("test")
		s := newChained(t, 1<<20, adapter.Deps{Tracer: tracer, SampleRatio: 1}, func(st *core.Stages) {
			tr := tracingStages{FlowControl: st.Flow, ReplayCache: st.Replay, tracer: tracer}
			st.Flow, st.Replay = tr, tr
		})
		s.serve(echoIn(1, 7), echoIn(2, 7))
		if rs, err := s.client.Recv(); err != nil || len(rs) != 2 {
			t.Fatalf("the chain is answered %+v, %v: want both Echo in one record", rs, err)
		}
		s.serve(echoIn(3, 7))
		if r := s.recvOne(); r.ID != 3 || r.Status != testproto.StatusOK {
			t.Fatalf("the Echo alone is answered %+v, want its success", r)
		}
		s.finish()
		var members []sdktrace.ReadOnlySpan
		children := map[trace.SpanID][]string{}
		for _, sd := range spans.Ended() {
			switch sd.Name() {
			case "adapter." + testproto.Name + ".Echo":
				members = append(members, sd)
			case "admit", "lookup":
				children[sd.Parent().SpanID()] = append(children[sd.Parent().SpanID()], sd.Name())
			}
		}
		if len(members) != 3 {
			t.Fatalf("%d Echo spans ended, want the chain's two and the request alone's", len(members))
		}
		for i, sd := range members {
			got := children[sd.SpanContext().SpanID()]
			slices.Sort(got)
			if !slices.Equal(got, []string{"admit", "lookup"}) {
				t.Errorf("Echo %d's span has the stage spans %v under it, want its admit and its lookup", i+1, got)
			}
		}
	})
}

// With a client that reads slowly, chains whose later member waits for its
// status hold no more records at once than the flow cap bounds: an Echo
// whose checksum fails after a valid Echo, and a related Echo after a member
// refused at route, each wait while their chain holds one place.
func TestChainsWaitingForAStatusStayWithinTheFlowCap(t *testing.T) {
	for _, c := range []struct {
		name  string
		chain func(f core.Framer, i uint64) []byte
	}{
		{"an Unwrap refusal after an Echo", func(f core.Framer, i uint64) []byte {
			b, offs := chainOf(f, echoIn(10*i+1, 7), echoIn(10*i+2, 7))
			b[offs[1]+headerLen+1] ^= 1 // the second member fails Unwrap
			return b
		}},
		{"a related member after a failure", func(f core.Framer, i uint64) []byte {
			b, _ := chainOf(f, testproto.Msg{Op: 99, Version: 2, ID: 10*i + 1, Session: 7, Tree: 1}, echoIn(10*i+2, inherited))
			return b
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				const room = 70
				s := newChained(t, room*smallCharge, adapter.Deps{}, func(st *core.Stages) { st.Flow = core.NewCapped() })
				synctest.Wait()
				per := s.recv.Charge(len(c.chain(s.f, 0)))
				const chains = 1000
				go func() {
					for i := range uint64(chains) {
						s.serveBody(c.chain(s.f, i))
					}
				}()
				most := int64(0)
				for replies := 0; replies < 2*chains; {
					synctest.Wait()
					if held := s.budgets.Receive.Reserved() / per; held > most {
						most = held
					}
					rs, err := s.client.Recv()
					if err != nil {
						t.Fatal(err)
					}
					replies += len(rs)
				}
				// At most 2×64+1: the chains holding a place under the cap of
				// 64, as many waiting at flow control with their records, and
				// the reader's own. Without the kept place they grow with the
				// chains sent.
				if most > 2*64+1 {
					t.Errorf("%d records held at once, past twice the flow cap and one", most)
				}
			})
		})
	}
}

// placesFlow is the stages' FlowControl counting the places held at once:
// a pass-through, where how many a chain holds is what is checked.
type placesFlow struct {
	core.FlowControl
	mu   sync.Mutex
	held int
}

func (f *placesFlow) Admit(ctx context.Context, h core.Header) (func(), error) {
	release, err := f.FlowControl.Admit(ctx, h)
	if err != nil {
		return release, err
	}
	f.mu.Lock()
	f.held++
	f.mu.Unlock()
	return func() {
		f.mu.Lock()
		f.held--
		f.mu.Unlock()
		release()
	}, nil
}

func (f *placesFlow) places() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.held
}

// A chain never waits at flow control holding a place, and splits only
// where it would wait without one: under a flow of one place, a chain of
// five Echo is answered in one record, each member admitted in the place the
// one before it gave back, with no split.
func TestAChainNeverWaitsForAPlaceHoldingOne(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m := metrics.New("v", "c")
		s := newSeamWith(t, edited{testproto.New(adapter.Deps{}).Bare(), func(st *core.Stages) { st.Flow = core.NewCappedN(1) }}, 1<<20, adapter.Deps{Metrics: m})
		var msgs []testproto.Msg
		for i := range 5 {
			msgs = append(msgs, testproto.Msg{Op: testproto.OpEcho, Version: 1, ID: uint64(i + 1)})
		}
		s.serve(msgs...)
		if rs, err := s.client.Recv(); err != nil || len(rs) != 5 {
			t.Errorf("the chain's first record holds %d replies (%v), want all 5", len(rs), err)
		}
		got, _ := scrape(t, m)
		if k := "adapter_chain_splits_total{protocol=" + testproto.Name + ",reason=flow}"; got[k] != 0 {
			t.Errorf("%s = %v, want 0", k, got[k])
		}
	})
}

// A later member refused while its chain's earlier reply holds the pool's
// last room waits for its status holding the chain's one place: refused by
// Unwrap after an Echo, answered a failure's status after a member refused
// at route, or refused by Admit for an overdraw.
func TestARefusedLaterMemberWaitsHoldingOnePlace(t *testing.T) {
	over := echoIn(2, 7)
	over.Credits = 1000
	for _, c := range []struct {
		name  string
		chain func(f core.Framer) []byte
	}{
		{"an Unwrap refusal after an Echo", func(f core.Framer) []byte {
			b, offs := chainOf(f, echoIn(1, 7), echoIn(2, 7))
			b[offs[1]+headerLen+1] ^= 1
			return b
		}},
		{"a related member after a failure", func(f core.Framer) []byte {
			b, _ := chainOf(f, testproto.Msg{Op: 99, Version: 2, ID: 1, Session: 7, Tree: 1}, echoIn(2, inherited))
			return b
		}},
		{"an Admit refusal after an Echo", func(f core.Framer) []byte {
			b, _ := chainOf(f, echoIn(1, 7), over)
			return b
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				flow := &placesFlow{}
				s := newChained(t, 1<<20, adapter.Deps{}, func(st *core.Stages) {
					flow.FlowControl = st.Flow
					st.Flow = flow
				})
				synctest.Wait()
				held, err := s.budgets.Reply.Reserve(context.Background(), peer, (1<<20)-s.budgets.Reply.Reserved()-s.reply.Charge(0))
				if err != nil {
					t.Fatal(err)
				}
				s.serveBody(c.chain(s.f))
				synctest.Wait() // the second member waits for its status, the first's reply unread
				if n := flow.places(); n != 1 {
					t.Errorf("while the refused member waits for its status the chain holds %d places, want 1", n)
				}
				held.Release()
				for replies := 0; replies < 2; {
					rs, err := s.client.Recv()
					if err != nil {
						t.Fatal(err)
					}
					replies += len(rs)
				}
			})
		})
	}
}

// A member in its call holds the chain's one place: the place of the member
// before it is given back once this one is admitted.
func TestAMemberInItsCallHoldsTheChainsOnePlace(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		flow := &placesFlow{}
		s := newChained(t, 1<<20, adapter.Deps{}, func(st *core.Stages) {
			flow.FlowControl = st.Flow
			st.Flow = flow
		})
		hold := s.faults.Hold(vfs.OpCreate)
		s.serve(
			testproto.Msg{Op: testproto.OpPutFH, Version: 2, ID: 1, Session: 7, Tree: 1, Handle: s.root},
			testproto.Msg{Op: testproto.OpCreate, Version: 2, ID: 2, Session: inherited, Tree: 1, Name: "one", Related: true},
		)
		<-hold.Reached()
		if n := flow.places(); n != 1 {
			t.Errorf("while the Create is in its call the chain holds %d places, want 1", n)
		}
		hold.Release()
		if rs, err := s.client.Recv(); err != nil || len(rs) != 2 {
			t.Errorf("the chain is answered %+v, %v", rs, err)
		}
	})
}

// cuedFlow is a FlowControl declared in the test: the protocol's own, its
// Admit call numbered at, counting from where seen was zeroed, held until
// open is closed.
type cuedFlow struct {
	core.FlowControl
	at, seen *atomic.Int64
	open     chan struct{}
}

func (f cuedFlow) Admit(ctx context.Context, h core.Header) (func(), error) {
	if f.seen.Add(1) == f.at.Load() {
		<-f.open
	}
	return f.FlowControl.Admit(ctx, h)
}

// A chain member's time in its own steps lands under their stages: 1 s held
// in the first member's Admit, on the reader, or in the second's, on the
// chain's goroutine, is observed under admit alone, and no later stage of
// the member absorbs it (RFC 20 §10.1).
func TestAChainMembersStagesAreTimedFromTheStepBeforeIt(t *testing.T) {
	for _, n := range []int64{1, 2} {
		t.Run("member "+strconv.FormatInt(n, 10)+" held in Admit", func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				at, seen, open := new(atomic.Int64), new(atomic.Int64), make(chan struct{})
				clk, m := newManualClock(), metrics.New("v", "c")
				s := newHeld(t, 1<<20, adapter.Deps{Metrics: m}, clk, func(st *core.Stages) {
					st.Flow = cuedFlow{st.Flow, at, seen, open}
				})
				before, _ := scrape(t, m)
				_, was := wireSeconds(before)
				seen.Store(0)
				at.Store(n)
				served := start(func() { s.serve(echoIn(1, 7), echoIn(2, inherited)) })
				synctest.Wait()
				clk.Advance(time.Second)
				close(open)
				if rs, err := s.client.Recv(); err != nil || len(rs) != 2 {
					t.Fatalf("the chain is answered %+v, %v; want both members in one record", rs, err)
				}
				<-served.done
				if err := s.p.Wait(context.Background()); err != nil {
					t.Fatal(err)
				}
				after, _ := scrape(t, m)
				_, sums := wireSeconds(after)
				for st, v := range sums {
					want := 0.0
					if st == "admit" {
						want = 1
					}
					if got := v - was[st]; got != want {
						t.Errorf("%s observed %v s over the chain, want %v", st, got, want)
					}
				}
			})
		})
	}
}
