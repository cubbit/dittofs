package nfs

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net/netip"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/cubbit/dittofs/internal/adapter/adaptertest/nfsclient"
	"github.com/cubbit/dittofs/internal/adapter/core"
	"github.com/cubbit/dittofs/internal/adapter/nfs/protocol"
	"github.com/cubbit/dittofs/pkg/adapter"
	"github.com/cubbit/dittofs/pkg/adapter/settings"
	"github.com/cubbit/dittofs/pkg/metrics"
	"github.com/cubbit/dittofs/pkg/resources"
	"github.com/cubbit/dittofs/pkg/vfs"
	"github.com/cubbit/dittofs/pkg/vfs/vfstest/memory"
)

// The procedures the replay tests address beside NFSv3's NULL: NFSv4's one
// procedure, RFC 7530 section 16.5's COMPOUND, with its members' free test
// codes named for the members they hold: SEQUENCE and GETATTR of RFC 8881
// section 18, and one that answers a delay, at a code no row of the members'
// table holds.
const (
	procCompound = 1
	opSequence   = 53
	opGetattr    = 4
	opDelay      = 999
)

// replayShape is what a replay test may change in the server it is given: its
// tables, over the memory service the rows mutate, and the protocol it wraps.
type replayShape struct {
	t    *testing.T
	mem  *memory.Service
	ts   *protocol.Tables
	wrap func(core.Protocol) core.Protocol
}

// replace is a fresh table over old's errors and label holding rows, in the
// place of old in the shape's tables.
func (s *replayShape) replace(old *core.OpTable, rows ...*core.Op) *core.OpTable {
	tab := core.NewOpTable(old.Errors, false, rows...)
	tab.Protocol = old.Protocol

	return tab
}

// member puts row at the compound members' code, refusing a code the table
// already holds.
func (s *replayShape) member(code uint32, row *core.Op) {
	members := s.ts.V4.Ops[procCompound].Members
	if _, taken := members.Ops[code]; taken {
		s.t.Fatalf("the members' table holds a row at %d: pick a code no row holds", code)
	}

	row.Code = code
	members.Ops[code] = row
}

// replaying serves the mutating server the replay tests send twice: NFSv3's
// NULL and the COMPOUND's member at opAccess, each a cacheable row whose one
// call is a Create, over the range versions, shaped by shape before it starts.
// It returns the service whose Mutations the rows fill, the deps holding the
// budgets and the metrics the tests read, and the server itself, whose nfs
// address serving serves.
func replaying(
	t *testing.T, versions settings.VersionRange, shape func(*replayShape),
) (*memory.Service, adapter.Deps, *core.Server) {
	t.Helper()

	d := leakChecked(t)
	mem := d.VFS.(*memory.Service)
	ts := tables(d)

	nfs3 := mutatingRow(mem)
	nfs3.Cacheable = true
	nfs3.Versions = core.Range{First: 3, Last: 3}
	ts.V3 = core.NewOpTable(ts.V3.Errors, false, nfs3)
	ts.V3.Protocol = "nfs3"

	member := mutatingRow(mem)
	member.Code, member.Name = opAccess, "access"
	member.Cacheable = true
	member.Versions = core.Range{First: 0, Last: 2}

	members := ts.V4.Ops[procCompound].Members
	if _, taken := members.Ops[opAccess]; taken {
		t.Fatalf("the members' table holds a row at %d: pick a code no row holds", opAccess)
	}

	members.Ops[opAccess] = member

	s := &replayShape{t: t, mem: mem, ts: &ts}
	if shape != nil {
		shape(s)
	}

	var p core.Protocol = protocol.New(ts, d.Budgets)
	if s.wrap != nil {
		p = s.wrap(p)
	}

	srv, err := core.NewServer(d, nfsSettings(versions), core.Listener{
		Services: []string{
			"nfs",
			"mount",
			"nlm",
			"nsm",
		}, Network: "tcp", Protocol: p,
	})
	if err != nil {
		t.Fatal(err)
	}

	start(t, srv)

	return mem, d, srv
}

// serving is srv's nfs address over tcp.
func serving(t *testing.T, srv *core.Server) netip.AddrPort {
	t.Helper()

	addrs := srv.Addrs()[adapter.Socket{Service: "nfs", Network: "tcp"}]
	if len(addrs) == 0 {
		t.Fatal("no address serves nfs over tcp")
	}

	return addrs[0]
}

// blockingRow is a cacheable mutating row whose call announces itself on
// started and answers only once release closes, so a test holds its request
// in progress and lets it go.
func blockingRow(mem *memory.Service, started chan<- struct{}, release <-chan struct{}) *core.Op {
	row := core.Row(0, "null", core.NeedsNothing,
		func(ctx context.Context, r *core.Request, _ void) (void, error) {
			_, _ = mem.Create(
				ctx,
				r.Call,
				mem.Store.Root,
				[]byte("f"),
				vfs.CreateArgs{Kind: vfs.CreateRegular},
			)

			close(started)
			<-release

			return void{}, nil
		})
	row.Cacheable = true

	return row
}

// abandonedRow is a cacheable mutating row that waits for its request's
// context to end, so a test can abandon the request and watch the pipeline
// drop it from the cache once the row returns.
func abandonedRow(mem *memory.Service, started chan<- struct{}) *core.Op {
	row := core.Row(0, "null", core.NeedsNothing,
		func(ctx context.Context, r *core.Request, _ void) (void, error) {
			_, _ = mem.Create(
				ctx,
				r.Call,
				mem.Store.Root,
				[]byte("f"),
				vfs.CreateArgs{Kind: vfs.CreateRegular},
			)

			close(started)
			<-ctx.Done()

			return void{}, nil
		})
	row.Cacheable = true
	row.Versions = core.Range{First: 3, Last: 3}

	return row
}

// answeringRow is a cacheable mutating row whose first call returns first and
// whose later ones run the Create, so a test sends the call twice and the
// retransmission is answered the second call's success, never an error the
// first call was answered and the cache kept.
func answeringRow(mem *memory.Service, first error) *core.Op {
	var calls int

	row := core.Row(0, "null", core.NeedsNothing,
		func(ctx context.Context, r *core.Request, _ void) (void, error) {
			calls++
			if calls == 1 {
				return void{}, first
			}

			_, _ = mem.Create(
				ctx,
				r.Call,
				mem.Store.Root,
				[]byte("f"),
				vfs.CreateArgs{Kind: vfs.CreateRegular},
			)

			return void{}, nil
		})
	row.Cacheable = true
	row.Versions = core.Range{First: 3, Last: 3}

	return row
}

// cacheable marks row one the duplicate-request cache serves.
func cacheable(row *core.Op) *core.Op {
	row.Cacheable = true

	return row
}

// errorRow is a member row over the minor versions whose every call returns
// err, so a compound running it answers a failure its table maps, having run
// its members before it.
func errorRow(name string, err error) *core.Op {
	row := core.Row(0, name, core.NeedsNothing,
		func(context.Context, *core.Request, void) (void, error) {
			return void{}, err
		})
	row.Versions = core.Range{First: 0, Last: 2}

	return row
}

// nullCall is one cacheable NFSv3 NULL.
func nullCall() nfsclient.Call {
	return nfsclient.Call{Prog: progNFS, Vers: 3, Proc: 0}
}

// compoundCall is one COMPOUND of minor version minor whose members are the
// free codes ops, of the member rows the shape put there.
func compoundCall(minor uint32, ops ...uint32) nfsclient.Call {
	call := nfsclient.Call{Prog: progNFS, Vers: 4, Proc: procCompound}

	call.Args = append(words(0, minor, uint32(len(ops))), words(ops...)...)

	return call
}

// doAnswer is what one background call came back with, its reply or its
// error, never a t method, so a negative wait can read it with a timeout.
type doAnswer struct {
	reply nfsclient.Reply
	err   error
}

// sendAsync answers c.Do's result through a channel, its goroutine touching
// nothing else, so a test's negative wait reads the answer with a timeout and
// joins it after.
func sendAsync(
	ctx context.Context, t *testing.T, c *nfsclient.Client, call nfsclient.Call,
) <-chan doAnswer {
	t.Helper()

	out := make(chan doAnswer, 1)

	go func() {
		reply, err := c.Do(ctx, call)
		out <- doAnswer{reply: reply, err: err}
	}()

	return out
}

// nothingAnswers fails t unless the answer channel holds nothing for the
// bounded wait it gives. It is the assertion where no clock can be injected
// and no event exposed: the pool a copy of a cached reply is charged to waits
// under the request's own context, whose 30 s attempt deadline no test shortens,
// so a held pool's answer is that nothing is granted to it, and the wait names
// that. A granted answer inside the wait fails the test instead.
func nothingAnswers(t *testing.T, out <-chan doAnswer, name string) {
	t.Helper()

	select {
	case got := <-out:
		t.Fatalf(
			"%s: answered %v, %v while the reply pool was held, want nothing",
			name,
			got.reply,
			got.err,
		)
	case <-time.After(300 * time.Millisecond):
	}
}

// inFlight is the adapter's requests admitted and not yet answered under
// nfs3: a test's join, since a request drops its reply from the cache before
// it ends, so the gauge returning to its baseline proves the drop already ran.
func inFlight(t *testing.T, m *metrics.Registry) float64 {
	t.Helper()

	mfs, err := m.Registry().Gather()
	if err != nil {
		t.Fatal(err)
	}

	var sum float64

	for _, mf := range mfs {
		if mf.GetName() != "dittofs_adapter_requests_in_flight" {
			continue
		}

		for _, mt := range mf.GetMetric() {
			if mt.GetGauge() == nil {
				continue
			}

			for _, lp := range mt.GetLabel() {
				if lp.GetName() == "protocol" && lp.GetValue() == "nfs3" {
					sum += mt.GetGauge().GetValue()
				}
			}
		}
	}

	return sum
}

// refused is the count adapter_refusals_total records under a protocol and a
// stage, which the negative waits read: a held pool's parked wait refuses
// nothing, so the count staying put is part of the design's own answer.
func refused(t *testing.T, m *metrics.Registry, protocol, stage string) float64 {
	t.Helper()

	mfs, err := m.Registry().Gather()
	if err != nil {
		t.Fatal(err)
	}

	var sum float64

	for _, mf := range mfs {
		if mf.GetName() != "dittofs_adapter_refusals_total" {
			continue
		}

		for _, mt := range mf.GetMetric() {
			var pl, sl bool

			for _, lp := range mt.GetLabel() {
				if lp.GetName() == "protocol" && lp.GetValue() == protocol {
					pl = true
				}

				if lp.GetName() == "stage" && lp.GetValue() == stage {
					sl = true
				}
			}

			if pl && sl {
				sum += mt.GetCounter().GetValue()
			}
		}
	}

	return sum
}

// settled waits until the requests in flight return to base, polling their
// gauge: the condition itself, its limit t.Context's child deadline, never a
// fixed sleep.
func settled(t *testing.T, m *metrics.Registry, base float64) {
	t.Helper()

	deadline := time.Now().Add(5 * time.Second)
	for inFlight(t, m) != base {
		if time.Now().After(deadline) {
			t.Fatalf("requests in flight stayed above %v, want the baseline back", base)
		}

		time.Sleep(time.Millisecond)
	}
}

// wantCreates fails t unless the service's Mutations record holds n Creates
// and nothing else.
func wantCreates(t *testing.T, name string, mem *memory.Service, n int) {
	t.Helper()

	ops := mem.Ops()
	if len(ops) != n {
		t.Errorf("%s: the service served %v, want %d Creates", name, ops, n)

		return
	}

	for _, op := range ops {
		if op != vfs.OpCreate {
			t.Errorf("%s: the service served %v, want Creates alone", name, ops)
		}
	}
}

// replayNotes is what the recording wrapper took of the calls the pipeline
// made to its cache, each call's name in the order it arrived.
type replayNotes struct {
	mu    sync.Mutex
	calls []string
}

func (n *replayNotes) note(name string) {
	n.mu.Lock()
	defer n.mu.Unlock()

	n.calls = append(n.calls, name)
}

func (n *replayNotes) take() []string {
	n.mu.Lock()
	defer n.mu.Unlock()

	got := n.calls
	n.calls = nil

	return got
}

// recordingProtocol wraps the whole protocol so its NewConn can wrap the
// replay stage it builds: each call the pipeline makes to the cache is
// recorded and passed through, the recording kind the plan allows, since the
// call reaching the cache is itself the requirement.
type recordingProtocol struct {
	core.Protocol
	notes *replayNotes
}

// NewConn is the protocol's own, with the replay stage it built wrapped in
// the recorder.
func (p *recordingProtocol) NewConn(c *core.Conn) core.Stages {
	st := p.Protocol.NewConn(c)
	if st.Replay != nil {
		st.Replay = recordingReplay{ReplayCache: st.Replay, notes: p.notes}
	}

	return st
}

// recordingReplay records each call it passes to the cache it wraps.
type recordingReplay struct {
	core.ReplayCache
	notes *replayNotes
}

func (r recordingReplay) Lookup(
	ctx context.Context, h core.Header,
) (*resources.Buf, error) {
	r.notes.note("lookup")

	return r.ReplayCache.Lookup(ctx, h)
}

func (r recordingReplay) Store(h core.Header, reply *resources.Buf) {
	r.notes.note("store")
	r.ReplayCache.Store(h, reply)
}

func (r recordingReplay) Drop(h core.Header) {
	r.notes.note("drop")
	r.ReplayCache.Drop(h)
}

// A call retransmitted over another connection is answered the reply the
// first got, once: the service sees one Create, the two replies are equal and
// the hit the cache counted is exactly one.
func TestACacheableReplyIsAnsweredToItsRetransmission(t *testing.T) {
	mem, d, srv := replaying(t, allVersions, nil)
	addr := serving(t, srv)
	c1, c2 := client(t, addr), client(t, addr)

	hits := recorded(t, d.Metrics, "adapter_replay_hits_total", "nfs3")

	xid := uint32(1)
	call := nfsclient.Call{XID: &xid, Prog: progNFS, Vers: 3, Proc: 0}

	first, err := c1.Do(within(t), call)
	if err != nil {
		t.Fatal(err)
	}

	second, err := c2.Do(within(t), call)
	if err != nil {
		t.Fatal(err)
	}

	if !reflect.DeepEqual(first, second) {
		t.Errorf("the retransmission was answered %+v, want the first's %+v", second, first)
	}

	wantCreates(t, "the retransmission", mem, 1)

	if got := recorded(t, d.Metrics, "adapter_replay_hits_total", "nfs3") - hits; got != 1 {
		t.Errorf("the cache counted %v hits, want one", got)
	}
}

// A COMPOUND whose one member is the cacheable row answers its retransmission
// from the cache the same way: one Create, equal replies, one hit.
func TestACompoundOfACacheableMemberIsAnsweredToItsRetransmission(t *testing.T) {
	mem, d, srv := replaying(t, allVersions, nil)
	addr := serving(t, srv)
	c1, c2 := client(t, addr), client(t, addr)

	hits := recorded(t, d.Metrics, "adapter_replay_hits_total", "nfs3")

	xid := uint32(2)
	call := compoundCall(0, opAccess)
	call.XID = &xid

	first, err := c1.Do(within(t), call)
	if err != nil {
		t.Fatal(err)
	}

	second, err := c2.Do(within(t), call)
	if err != nil {
		t.Fatal(err)
	}

	if !reflect.DeepEqual(first, second) {
		t.Errorf("the retransmission was answered %+v, want the first's %+v", second, first)
	}

	wantCreates(t, "the retransmission", mem, 1)

	if got := recorded(t, d.Metrics, "adapter_replay_hits_total", "nfs3") - hits; got != 1 {
		t.Errorf("the cache counted %v hits, want one", got)
	}
}

// A COMPOUND whose member is not cacheable is never answered from the cache:
// its retransmission runs again, the service seeing a second Create, and no
// hit is counted.
func TestACompoundOfAnUncacheableMemberIsNeverAnsweredFromTheCache(t *testing.T) {
	mem, d, srv := replaying(t, allVersions, func(s *replayShape) {
		s.ts.V4.Ops[procCompound].Members.Ops[opAccess].Cacheable = false
	})
	addr := serving(t, srv)
	c1, c2 := client(t, addr), client(t, addr)

	hits := recorded(t, d.Metrics, "adapter_replay_hits_total", "nfs3")

	xid := uint32(3)
	call := compoundCall(0, opAccess)
	call.XID = &xid

	first, err := c1.Do(within(t), call)
	if err != nil {
		t.Fatal(err)
	}

	second, err := c2.Do(within(t), call)
	if err != nil {
		t.Fatal(err)
	}

	if !reflect.DeepEqual(first, second) {
		t.Errorf("the retransmission was answered %+v, want the first's %+v", second, first)
	}

	wantCreates(t, "the retransmission", mem, 2)

	if got := recorded(t, d.Metrics, "adapter_replay_hits_total", "nfs3") - hits; got != 0 {
		t.Errorf("the cache counted %v hits, want none", got)
	}
}

// A COMPOUND of NFSv4.1 is never answered from the NFSv4.0 cache this
// subtask builds: its retransmission runs again, leaving a second Create, and
// no hit is counted.
func TestACompoundOfALaterMinorIsNeverAnsweredFromTheCache(t *testing.T) {
	mem, d, srv := replaying(t, allVersions, nil)
	addr := serving(t, srv)
	c1, c2 := client(t, addr), client(t, addr)

	hits := recorded(t, d.Metrics, "adapter_replay_hits_total", "nfs3")

	xid := uint32(4)
	call := compoundCall(1, opAccess)
	call.XID = &xid

	first, err := c1.Do(within(t), call)
	if err != nil {
		t.Fatal(err)
	}

	second, err := c2.Do(within(t), call)
	if err != nil {
		t.Fatal(err)
	}

	if !reflect.DeepEqual(first, second) {
		t.Errorf("the retransmission was answered %+v, want the first's %+v", second, first)
	}

	wantCreates(t, "the retransmission", mem, 2)

	if got := recorded(t, d.Metrics, "adapter_replay_hits_total", "nfs3") - hits; got != 0 {
		t.Errorf("the cache counted %v hits, want none", got)
	}
}

// A retransmission from another address is another call: the cache never
// names the connection, but its key names the client's address, so the second
// send runs again, leaving a second Create, and counts no hit.
func TestARetransmissionFromAnotherAddressIsAnotherCall(t *testing.T) {
	mem, d, srv := replaying(t, allVersions, nil)
	addr := serving(t, srv)

	c1, err := nfsclient.DialFrom(within(t), netip.MustParseAddr("127.0.0.1"), addr)
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() {
		_ = c1.Close()
	})

	c2, err := nfsclient.DialFrom(within(t), netip.MustParseAddr("127.0.0.2"), addr)
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() {
		_ = c2.Close()
	})

	hits := recorded(t, d.Metrics, "adapter_replay_hits_total", "nfs3")

	xid := uint32(5)
	call := nullCall()
	call.XID = &xid

	if _, err := c1.Do(within(t), call); err != nil {
		t.Fatal(err)
	}

	if _, err := c2.Do(within(t), call); err != nil {
		t.Fatal(err)
	}

	wantCreates(t, "the other address", mem, 2)

	if got := recorded(t, d.Metrics, "adapter_replay_hits_total", "nfs3") - hits; got != 0 {
		t.Errorf("the cache counted %v hits, want none", got)
	}
}

// A retransmission of a call refused at its header or its credential is
// refused its own answer again, never one the cache kept for a call that ran:
// the refusals below never reach the cache's map, so both sends of each are
// answered the same bytes and no hit is counted nor anything run.
func TestARefusedRetransmissionIsRefusedAgain(t *testing.T) {
	mem, d, srv := replaying(t, allVersions, nil)
	c := dial(t, srv)

	hits := recorded(t, d.Metrics, "adapter_replay_hits_total", "nfs3")

	// sys is AUTH_SYS claiming 17 groups, one past RFC 5531's 16.
	sys := append(words(0), opaque(nil)...)
	sys = append(sys, words(0, 0, 17)...)
	sys = append(sys, make([]byte, 17*4)...)

	rpcv3 := call(71, progNFS, 3, 0, none) // its RPC version patched, second send the same bytes.
	binary.BigEndian.PutUint32(rpcv3[8:], 3)

	seventeen := call(72, progNFS, 3, 0, append(words(authSys), opaque(sys)...))

	// verf is AUTH_NONE's credential with a verifier of flavour AUTH_SYS and
	// no body, refused AUTH_BADVERF by the credential check.
	verf := append(words(73, 0, 2, progNFS, 3, 0), none...)
	verf = append(verf, words(authSys, 0)...)

	for _, tc := range []struct {
		name string
		call []byte
		want []byte
	}{
		{
			"an RPC version 3 call",
			rpcv3,
			words(71, 1, 1, rpcMismatch, 2, 2), // REPLY, MSG_DENIED, RPC_MISMATCH, versions 2 to 2
		},
		{
			"an AUTH_SYS credential of 17 groups",
			seventeen,
			words(72, 1, 1, 1, authBadCred), // REPLY, MSG_DENIED, AUTH_ERROR
		},
		{
			"an empty AUTH_SYS verifier",
			verf,
			words(73, 1, 1, 1, authBadVerf), // REPLY, MSG_DENIED, AUTH_ERROR
		},
	} {
		first := ask(t, c, tc.call)
		if !bytes.Equal(first, tc.want) {
			t.Errorf("%s: answered %x, want %x", tc.name, first, tc.want)
		}

		second := ask(t, c, tc.call)
		if !bytes.Equal(second, tc.want) {
			t.Errorf(
				"%s: the retransmission was refused %x, want the first's %x",
				tc.name,
				second,
				tc.want,
			)
		}
	}

	if got := recorded(t, d.Metrics, "adapter_replay_hits_total", "nfs3") - hits; got != 0 {
		t.Errorf("the cache counted %v hits, want none", got)
	}

	wantCreates(t, "the refusals", mem, 0)
}

// A retransmission of a call still running answers nothing, in flight: the
// second send gets no reply, nothing the server holds is unmapped, and once
// the row is released the send after it is answered the stored reply.
func TestARetransmissionOfARunningCallIsAnsweredNothing(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	mem, d, srv := replaying(t, allVersions, func(s *replayShape) {
		row := blockingRow(s.mem, started, release)
		row.Versions = core.Range{First: 3, Last: 3}
		s.ts.V3 = s.replace(s.ts.V3, row)
	})
	addr := serving(t, srv)
	c1, c2 := client(t, addr), client(t, addr)

	hits := recorded(t, d.Metrics, "adapter_replay_hits_total", "nfs3")
	unmapped := recorded(t, d.Metrics, "adapter_unmapped_errors_total", "nfs3")

	xid := uint32(7)
	call := nullCall()
	call.XID = &xid

	first := sendAsync(within(t), t, c1, call)

	<-started

	// The retransmission is answered nothing for as long as the ctx below
	// waits: a bounded wait, since no event and no injectable clock name a
	// no-reply the design answers with silence.
	ctx, cancel := context.WithTimeout(t.Context(), 500*time.Millisecond)
	t.Cleanup(cancel)

	if _, err := c2.Do(ctx, call); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("the retransmission was answered %v, want nothing before its deadline", err)
	}

	close(release)

	got := <-first
	if got.err != nil {
		t.Fatal(got.err)
	}

	third, err := c2.Do(within(t), call)
	if err != nil {
		t.Fatal(err)
	}

	if !reflect.DeepEqual(third, got.reply) {
		t.Errorf(
			"the send after the release was answered %+v, want the first's %+v",
			third,
			got.reply,
		)
	}

	wantCreates(t, "the call in progress", mem, 1)

	if got := recorded(t, d.Metrics, "adapter_unmapped_errors_total", "nfs3") - unmapped; got != 0 {
		t.Errorf("the no-reply answer unmapped %v errors, want none", got)
	}

	if got := recorded(t, d.Metrics, "adapter_replay_hits_total", "nfs3") - hits; got != 1 {
		t.Errorf("the cache counted %v hits, want one", got)
	}
}

// blockingEachRow is blockingRow whose started channel carries one signal a
// run: a copy the cache wrongly admits runs the handler again, and the second
// signal names it.
func blockingEachRow(
	mem *memory.Service, started chan<- struct{}, release <-chan struct{},
) *core.Op {
	row := core.Row(0, "null", core.NeedsNothing,
		func(ctx context.Context, r *core.Request, _ void) (void, error) {
			_, _ = mem.Create(
				ctx,
				r.Call,
				mem.Store.Root,
				[]byte("f"),
				vfs.CreateArgs{Kind: vfs.CreateRegular},
			)

			started <- struct{}{}

			<-release

			return void{}, nil
		})
	row.Cacheable = true

	return row
}

// The second retransmission of a call still running is answered nothing too:
// a retransmission refused by the replay check ends clearing no mark, since
// it made none, so the mark the running call left keeps answering its own
// copies. Were the first retransmission's end to clear that mark, the second
// would miss the cache and run the mutating handler again.
func TestTheSecondRetransmissionOfARunningCallRunsNothing(t *testing.T) {
	started := make(chan struct{}, 4)
	release := make(chan struct{})
	mem, _, srv := replaying(t, allVersions, func(s *replayShape) {
		row := blockingEachRow(s.mem, started, release)
		row.Versions = core.Range{First: 3, Last: 3}
		s.ts.V3 = s.replace(s.ts.V3, row)
	})
	addr := serving(t, srv)
	c1, c2, c3 := client(t, addr), client(t, addr), client(t, addr)

	xid := uint32(21)
	call := nullCall()
	call.XID = &xid

	first := sendAsync(within(t), t, c1, call)

	<-started

	for i, c := range []*nfsclient.Client{c2, c3} {
		// Each retransmission is answered nothing for as long as its bounded
		// ctx waits, as long as the release below stays closed: a bounded
		// wait, since no event and no injectable clock name the silence.
		ctx, cancel := context.WithTimeout(t.Context(), 400*time.Millisecond)
		t.Cleanup(cancel)

		name := [...]string{"first", "second"}[i]
		if _, err := c.Do(ctx, call); !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("the %s retransmission was answered %v, want nothing", name, err)
		}
	}

	close(release)

	if got := <-first; got.err != nil {
		t.Fatal(got.err)
	}

	wantCreates(t, "the call still running under two retransmissions", mem, 1)
}

// The second retransmission of a COMPOUND still running closes its connection
// too: the first retransmission's end leaves the mark the running compound
// made, so the second meets the mark, not an empty cache, and never runs a
// member again.
func TestTheSecondRetransmissionOfARunningCompoundRunsNothing(t *testing.T) {
	started := make(chan struct{}, 4)
	release := make(chan struct{})
	mem, _, srv := replaying(t, allVersions, func(s *replayShape) {
		row := blockingEachRow(s.mem, started, release)
		row.Code, row.Versions = opAccess, core.Range{First: 0, Last: 2}
		s.ts.V4.Ops[procCompound].Members.Ops[opAccess] = row // in place of the builder's row
	})
	addr := serving(t, srv)
	c1, c2, c3 := client(t, addr), client(t, addr), client(t, addr)

	xid := uint32(22)
	call := compoundCall(0, opAccess)
	call.XID = &xid

	first := sendAsync(within(t), t, c1, call)

	<-started

	for _, c := range []*nfsclient.Client{c2, c3} {
		// The close of the refused retransmission's connection is an
		// event, so within's hang guard serves both: the refusal comes
		// back at once, and a rerun, which the mark the pipeline keeps
		// never allows, holds to its deadline.
		if _, err := c.Do(within(t), call); !errors.Is(err, io.EOF) {
			t.Errorf(
				"a retransmission was answered %v, want the connection closed with nothing written",
				err,
			)
		}
	}

	close(release)

	if got := <-first; got.err != nil {
		t.Fatal(got.err)
	}

	wantCreates(t, "the compound still running under two retransmissions", mem, 1)
}

// A retransmission of a COMPOUND still running closes the connection it came
// on, writing nothing (RFC 7530 section 3.1.1): a resend after the release, on
// a new connection, is answered the stored reply.
func TestARetransmissionOfARunningCompoundClosesItsConnection(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	mem, d, srv := replaying(t, allVersions, func(s *replayShape) {
		row := blockingRow(s.mem, started, release)
		row.Code, row.Versions = opAccess, core.Range{First: 0, Last: 2}
		s.ts.V4.Ops[procCompound].Members.Ops[opAccess] = row // in place of the builder's row
	})
	addr := serving(t, srv)
	c1, c2 := client(t, addr), client(t, addr)

	hits := recorded(t, d.Metrics, "adapter_replay_hits_total", "nfs3")

	xid := uint32(8)
	call := compoundCall(0, opAccess)
	call.XID = &xid

	first := sendAsync(within(t), t, c1, call)

	<-started

	if _, err := c2.Do(within(t), call); !errors.Is(err, io.EOF) {
		t.Errorf(
			"the retransmission answered %v, want its connection closed with nothing written",
			err,
		)
	}

	close(release)

	got := <-first
	if got.err != nil {
		t.Fatal(got.err)
	}

	c3 := client(t, addr)

	third, err := c3.Do(within(t), call)
	if err != nil {
		t.Fatal(err)
	}

	if !reflect.DeepEqual(third, got.reply) {
		t.Errorf(
			"the resend on a new connection was answered %+v, want the first's %+v",
			third,
			got.reply,
		)
	}

	wantCreates(t, "the compound in progress", mem, 1)

	if got := recorded(t, d.Metrics, "adapter_replay_hits_total", "nfs3") - hits; got != 1 {
		t.Errorf("the cache counted %v hits, want one", got)
	}
}

// A request whose context the client's close cancelled runs again when its
// call is retransmitted: nothing is left for it in the cache, not even the
// mark, since the pipeline drops it once its handler answers, which the gauge
// of the requests in flight returning to its baseline names.
func TestARetransmissionOfAnAbandonedCallRunsAgain(t *testing.T) {
	started := make(chan struct{})
	mem, d, srv := replaying(t, allVersions, func(s *replayShape) {
		s.ts.V3 = s.replace(s.ts.V3, abandonedRow(s.mem, started))
	})
	addr := serving(t, srv)
	c1 := client(t, addr)

	hits := recorded(t, d.Metrics, "adapter_replay_hits_total", "nfs3")
	base := inFlight(t, d.Metrics)

	xid := uint32(9)
	call := nullCall()
	call.XID = &xid

	first := sendAsync(within(t), t, c1, call)

	<-started

	if err := c1.Close(); err != nil {
		t.Fatal(err)
	}

	if got := <-first; got.err == nil {
		t.Errorf("the abandoned call was answered %+v, want nothing", got.reply)
	}

	settled(t, d.Metrics, base)

	c2 := client(t, addr)
	if _, err := c2.Do(within(t), call); err != nil {
		t.Fatal(err)
	}

	wantCreates(t, "the abandoned call", mem, 2)

	if got := recorded(t, d.Metrics, "adapter_replay_hits_total", "nfs3") - hits; got != 0 {
		t.Errorf("the cache counted %v hits, want none", got)
	}
}

// A call a delay or a fault answered is never answered from the cache: the
// retransmission runs, the service seeing its Create, and is answered the
// second call's success, never the first call's stored error.
func TestARetransmissionOfADelayOrAFaultRunsAgain(t *testing.T) {
	for _, tc := range []struct {
		name  string
		first error
	}{
		{"a delay", vfs.ErrDelay},
		{"a fault", vfs.ErrIO},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mem, d, srv := replaying(t, allVersions, func(s *replayShape) {
				s.ts.V3 = s.replace(s.ts.V3, answeringRow(s.mem, tc.first))
			})
			addr := serving(t, srv)
			c := client(t, addr)

			hits := recorded(t, d.Metrics, "adapter_replay_hits_total", "nfs3")

			xid := uint32(10)
			call := nullCall()
			call.XID = &xid

			if _, err := c.Do(within(t), call); err != nil {
				t.Fatal(err)
			}

			second, err := c.Do(within(t), call)
			if err != nil {
				t.Fatal(err)
			}

			if want := (nfsclient.Reply{XID: xid}); !reflect.DeepEqual(second, want) {
				t.Errorf(
					"the retransmission was answered %+v, want the second run's %+v",
					second,
					want,
				)
			}

			wantCreates(t, "the retransmission", mem, 1)

			if got := recorded(t, d.Metrics, "adapter_replay_hits_total", "nfs3") - hits; got != 0 {
				t.Errorf("the cache counted %v hits, want none", got)
			}
		})
	}
}

// A call the service delayed, whose row's NoReply answer leaves the client
// with nothing, still ends in the cache: its own mark, the one its miss made,
// is dropped, so the retransmission runs the call again and is answered the
// second run's success, never held as a call still running.
func TestARetransmissionOfACallTheServiceDelayedRunsAgain(t *testing.T) {
	mem, _, srv := replaying(t, allVersions, func(s *replayShape) {
		row := answeringRow(s.mem, &vfs.Delay{Cause: vfs.DelayResources})
		tab := core.NewOpTable(core.NewErrorTable(core.Status(0x40000005),
			core.ErrorRow{Err: core.ErrDecode, Status: 1},
			core.ErrorRow{Err: &vfs.Delay{Cause: vfs.DelayResources}, NoReply: true}), false, row)
		tab.Protocol = s.ts.V3.Protocol
		s.ts.V3 = tab
	})
	addr := serving(t, srv)
	c := client(t, addr)

	xid := uint32(23)
	call := nullCall()
	call.XID = &xid

	// The bounded wait is the assertion: a NoReply row names no event and
	// no injectable clock names its silence.
	ctx, cancel := context.WithTimeout(t.Context(), 400*time.Millisecond)
	t.Cleanup(cancel)

	if _, err := c.Do(ctx, call); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("the delayed call was answered %v, want the NoReply silence", err)
	}

	second, err := c.Do(within(t), call)
	if err != nil {
		t.Errorf(
			"the retransmission of a call the service delayed, answered %v, want it run again",
			err,
		)
	}

	if want := (nfsclient.Reply{XID: xid}); !reflect.DeepEqual(second, want) {
		t.Errorf("the retransmission was answered %+v, want the second run's %+v", second, want)
	}

	wantCreates(t, "the retransmission of a call the service delayed", mem, 1)
}

// The same for a COMPOUND whose member the service delayed: the retransmission
// would be answered only the connection's close were the unanswered request's
// mark left behind, so it runs the member again and is answered the second
// run's success.
func TestARetransmissionOfACompoundTheServiceDelayedRunsAgain(t *testing.T) {
	mem, _, srv := replaying(t, allVersions, func(s *replayShape) {
		row := answeringRow(s.mem, &vfs.Delay{Cause: vfs.DelayResources})
		row.Code, row.Name = opAccess, "access"
		row.Versions = core.Range{First: 0, Last: 2}
		members := s.ts.V4.Ops[procCompound].Members
		// The table itself stays: NewOpTable fills none of a members'
		// ReadOp or EncodeResult, which the builder sets after it.
		members.Errors = core.NewErrorTable(core.Status(0x40000005),
			core.ErrorRow{Err: core.ErrDecode, Status: 1},
			core.ErrorRow{Err: &vfs.Delay{Cause: vfs.DelayResources}, NoReply: true})
		members.Ops[opAccess] = row
	})
	addr := serving(t, srv)
	c := client(t, addr)

	xid := uint32(24)
	call := compoundCall(0, opAccess)
	call.XID = &xid

	// The bounded wait is the assertion: a NoReply row names no event and
	// no injectable clock names its silence.
	ctx, cancel := context.WithTimeout(t.Context(), 400*time.Millisecond)
	t.Cleanup(cancel)

	if _, err := c.Do(ctx, call); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("the delayed compound was answered %v, want the NoReply silence", err)
	}

	second, err := c.Do(within(t), call)
	if err != nil {
		t.Errorf(
			"the retransmission of a compound the service delayed, answered %v, want it run again",
			err,
		)
	}

	if second.XID != xid {
		t.Errorf(
			"the retransmission was answered xid %d, want the second run's %d",
			second.XID,
			xid,
		)
	}

	wantCreates(t, "the retransmission of a compound the service delayed", mem, 1)
}

// A retransmission whose copied reply has no room in the reply pool waits for
// it rather than refuse: while the pool is held its client is answered
// nothing and no refusal and no unmapped error is counted, and once the pool
// is released the same send is answered the cached reply, byte for byte.
func TestARetransmissionWaitsForTheReplyPoolItIsCharged(t *testing.T) {
	mem, d, srv := replaying(t, allVersions, nil)
	addr := serving(t, srv)
	c1, c2 := client(t, addr), client(t, addr)

	hits := recorded(t, d.Metrics, "adapter_replay_hits_total", "nfs3")

	xid := uint32(12)
	call := nullCall()
	call.XID = &xid

	first, err := c1.Do(within(t), call)
	if err != nil {
		t.Fatal(err)
	}

	held, err := d.Budgets.Reply.Reserve(within(t), netip.Addr{}, 8<<20)
	if err != nil {
		t.Fatal(err)
	}

	second := sendAsync(within(t), t, c2, call)
	nothingAnswers(t, second, "the held pool")

	if got := refused(t, d.Metrics, "nfs3", "replay_check"); got != 0 {
		t.Errorf("the parked copy refused %v times, want none", got)
	}

	if got := recorded(t, d.Metrics, "adapter_unmapped_errors_total", "nfs3"); got != 0 {
		t.Errorf("the parked copy unmapped %v errors, want none", got)
	}

	held.Release()

	got := <-second
	if got.err != nil {
		t.Fatal(got.err)
	}

	if !reflect.DeepEqual(got.reply, first) {
		t.Errorf("the released pool answered %+v, want the cached %+v", got.reply, first)
	}

	wantCreates(t, "the parked retransmission", mem, 1)

	if got := recorded(t, d.Metrics, "adapter_replay_hits_total", "nfs3") - hits; got != 1 {
		t.Errorf("the cache counted %v hits, want one", got)
	}
}

// A resend of a stored COMPOUND whose copy cannot take its charge from the
// held reply pool is dropped silently, its connection closed with nothing
// written (RFC 7530 3.1.1), where the retransmission of a call still running
// was closed for its mark; the COMPOUND resent once the pool frees is
// answered the cached reply.
func TestAResentCompoundIsDroppedForAHeldReplyPool(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	mem, d, srv := replaying(t, allVersions, func(s *replayShape) {
		row := blockingRow(s.mem, started, release)
		row.Code, row.Versions = opAccess, core.Range{First: 0, Last: 2}
		s.ts.V4.Ops[procCompound].Members.Ops[opAccess] = row // in place of the builder's row
	})
	addr := serving(t, srv)
	c1, c2 := client(t, addr), client(t, addr)

	hits := recorded(t, d.Metrics, "adapter_replay_hits_total", "nfs3")

	xid := uint32(13)
	call := compoundCall(0, opAccess)
	call.XID = &xid

	first := sendAsync(within(t), t, c1, call)

	<-started

	if _, err := c2.Do(within(t), call); !errors.Is(err, io.EOF) {
		t.Errorf(
			"the retransmission answered %v, want its connection closed with nothing written",
			err,
		)
	}

	close(release)

	got := <-first
	if got.err != nil {
		t.Fatal(got.err)
	}

	// The pool held below holds the whole budget, which fits now that the
	// first call's reply reservation went back with its send. The copy of a
	// compound that cannot take its charge is severed at once, nothing
	// written (RFC 7530 3.1.1), and the close ends that probe by core's own
	// design, so the cut itself is counted as no refusal.
	held, err := d.Budgets.Reply.Reserve(within(t), netip.Addr{}, 8<<20)
	if err != nil {
		t.Fatal(err)
	}

	c3 := client(t, addr)
	if _, err := c3.Do(within(t), call); !errors.Is(err, io.EOF) {
		t.Errorf(
			"the resend answered %v while the reply pool was held, want its "+
				"connection closed with nothing written",
			err,
		)
	}
	// The one refusal counted at the replay check is c2's errReplaying, the
	// call still running; c3's cut counts nothing, and its delay answers
	// nothing: the connection is already closed.
	if got := refused(t, d.Metrics, "nfs3", "replay_check"); got != 1 {
		t.Errorf("the dropped resend refused %v times, want only c2's refusal counted", got)
	}

	held.Release()

	c4 := client(t, addr)

	again, err := c4.Do(within(t), call)
	if err != nil {
		t.Fatal(err)
	}

	if !reflect.DeepEqual(again, got.reply) {
		t.Errorf("the freed pool answered %+v, want the cached %+v", again, got.reply)
	}

	wantCreates(t, "the resent compound", mem, 1)

	if got := recorded(t, d.Metrics, "adapter_replay_hits_total", "nfs3") - hits; got != 1 {
		t.Errorf("the cache counted %v hits, want one", got)
	}
}

// A COMPOUND of a later minor version reaches the cache through its SEQUENCE
// row the way a 4.0 one does, never stored by this subtask's key but handed
// to Store by core's member rule: the recording wrapper shows each compound
// stored through its cacheable member and no drop after it, and the delay
// member the second compound runs past does not turn it into one.
func TestALaterMinorCompoundIsHandedToStoreThroughItsRows(t *testing.T) {
	notes := new(replayNotes)
	mem, _, srv := replaying(t, allVersions, func(s *replayShape) {
		sequence := mutatingRow(s.mem)
		sequence.Name = "sequence"
		sequence.Versions = core.Range{First: 0, Last: 2}
		s.member(opSequence, cacheable(sequence))
		getattr := mutatingRow(s.mem)
		getattr.Name = "getattr"
		getattr.Versions = core.Range{First: 0, Last: 2}
		s.member(opGetattr, getattr)
		s.member(opDelay, errorRow("delay", vfs.ErrDelay))
		s.wrap = func(p core.Protocol) core.Protocol {
			return &recordingProtocol{Protocol: p, notes: notes}
		}
	})
	addr := serving(t, srv)
	c := client(t, addr)

	xid := uint32(14)
	call := compoundCall(1, opSequence, opGetattr)

	call.XID = &xid
	if _, err := c.Do(within(t), call); err != nil {
		t.Fatal(err)
	}

	xid = 15
	call = compoundCall(1, opSequence, opDelay)

	call.XID = &xid
	if _, err := c.Do(within(t), call); err != nil {
		t.Fatal(err)
	}

	got := notes.take()

	want := []string{"lookup", "store", "lookup", "store"}

	if !reflect.DeepEqual(got, want) {
		t.Errorf("the pipeline called the cache %v, want %v, and no drop", got, want)
	}

	wantCreates(t, "the two compounds", mem, 3)
}

// MOUNT's, NLM's and NSM's calls, and an NFSv3 row that is not cacheable, are
// never answered from the cache: each retransmission runs again, so every
// program the adapter serves twice leaves two runs and no hit is counted.
func TestTheAuxiliaryProgramsAndAnUncacheableRowAreNeverCached(t *testing.T) {
	mem, d, srv := replaying(t, allVersions, func(s *replayShape) {
		row := func(code uint32, name string, vers uint32) *core.Op {
			op := mutatingRow(s.mem)
			op.Code, op.Name = code, name
			op.Cacheable = true
			op.Versions = core.Range{First: vers, Last: vers}

			return op
		}
		read := mutatingRow(s.mem)
		read.Code, read.Name = 6, "read" // NFSv3's READ, left not cacheable
		read.Versions = core.Range{First: 3, Last: 3}
		null := row(0, "null", 3)
		s.ts.V3 = s.replace(s.ts.V3, read, null)
		// MOUNT's NULL sits at procedure 0, the number of the cacheable NFSv3
		// row, so the test would show the cache answering a retransmission of
		// another program the moment the program gate was gone.
		s.ts.Mount = s.replace(s.ts.Mount, row(0, "mnt", 3))
		s.ts.NLM = s.replace(s.ts.NLM, row(1, "test", 4))
		s.ts.NSM = s.replace(s.ts.NSM, row(6, "sm_notify", 1))
	})
	addr := serving(t, srv)
	c := client(t, addr)

	hits := recorded(t, d.Metrics, "adapter_replay_hits_total", "nfs3")

	for _, tc := range []struct {
		name string
		call nfsclient.Call
	}{
		{"the NFSv3 READ row", nfsclient.Call{Prog: progNFS, Vers: 3, Proc: 6}},
		{"MOUNT's NULL", nfsclient.Call{Prog: progMount, Vers: 3, Proc: 0}},
		{"NLM's TEST", nfsclient.Call{Prog: progNLM, Vers: 4, Proc: 1}},
		{"NSM's SM_NOTIFY", nfsclient.Call{Prog: progNSM, Vers: 1, Proc: 6}},
	} {
		xid := uint32(16)

		tc.call.XID = &xid
		for range 2 {
			if _, err := c.Do(within(t), tc.call); err != nil {
				t.Fatalf("%s: %v", tc.name, err)
			}
		}
	}

	wantCreates(t, "every program's retransmission", mem, 8)

	if got := recorded(t, d.Metrics, "adapter_replay_hits_total", "nfs3") - hits; got != 0 {
		t.Errorf("the cache counted %v hits, want none", got)
	}
}
