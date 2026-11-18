package core_test

import (
	"context"
	"errors"
	"maps"
	"net"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cubbit/dittofs/internal/adapter/adaptertest"
	"github.com/cubbit/dittofs/internal/adapter/adaptertest/testproto"
	"github.com/cubbit/dittofs/internal/adapter/core"
	"github.com/cubbit/dittofs/pkg/adapter"
	"github.com/cubbit/dittofs/pkg/adapter/settings"
	"github.com/cubbit/dittofs/pkg/resources"
)

// Reconfigure (RFC 20 sections 3.1, 3.4 and 3.6): a refusal leaves the server
// as it was; every setting but the port applies, to connections set up after
// the change; an added address binds before Reconfigure returns and a hold on
// it fails with nothing changed; a removed address's listeners and connections
// close and are torn down before it returns; while configured nothing is
// bound, while draining or stopped it is refused.

// bogusShape is another type a Reconfigure could be handed: not the server's
// shape, though it carries testproto's fields.
type bogusShape struct{ testproto.Settings }

// drainOn is testproto's shape on addrs, its drain deadline at the 200 ms the
// draining fixtures hold.
func drainOn(addrs ...string) testproto.Settings {
	s := on(addrs...)
	s.DrainTimeout = 200 * time.Millisecond
	return s
}

// servingOn starts a loopback server on testproto's full protocol under snap,
// one listener: Negotiate answers the settings' cap, which is how the tests
// watch the snapshot move.
func servingOn(t *testing.T, b *resources.Budgets, snap testproto.Settings) *core.Server {
	t.Helper()
	srv, err := core.NewServer(adapter.Deps{Budgets: b}, snap, core.Listener{Services: []string{"test"}, Network: "tcp", Protocol: testproto.New(adapter.Deps{})})
	if err != nil {
		t.Fatal(err)
	}
	stopAtEnd(t, srv)
	if err := srv.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	return srv
}

// servingCapturing is servingOn with its connections kept where a test
// reaches them, to park a drain on one or read back the settings an
// accepted connection carries.
func servingCapturing(t *testing.T, b *resources.Budgets, snap testproto.Settings) (*core.Server, *capturing) {
	t.Helper()
	p := newCapturing(testproto.New(adapter.Deps{}))
	srv, err := core.NewServer(adapter.Deps{Budgets: b}, snap, core.Listener{Services: []string{"test"}, Network: "tcp", Protocol: p})
	if err != nil {
		t.Fatal(err)
	}
	stopAtEnd(t, srv)
	if err := srv.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	return srv, p
}

// negotiateOn dials the server's one bound address and negotiates up to
// version v, returning the version the server picked: its snapshot's cap,
// unchanged where a refusal left it.
func negotiateOn(t *testing.T, srv *core.Server, v core.Version) core.Version {
	t.Helper()
	return negotiateAt(t, testAddr(t, srv), v)
}

// negotiateAt dials at and negotiates up to version v, returning the version
// the server picked: its snapshot's cap, unchanged where a refusal left it.
func negotiateAt(t *testing.T, at netip.AddrPort, v core.Version) core.Version {
	t.Helper()
	c, _ := clientAt(t, at)
	if err := c.Send(testproto.Msg{Op: testproto.OpNegotiate, Version: v, ID: 1}); err != nil {
		t.Fatal(err)
	}
	rs, err := c.Recv()
	if err != nil {
		t.Fatal(err)
	}
	if len(rs) != 1 || len(rs[0].Body) != 1 {
		t.Fatalf("Negotiate answered %+v, want the version it picked in one byte", rs)
	}
	return core.Version(rs[0].Body[0])
}

// sameAddrs tells whether the two address maps match, slices included.
func sameAddrs(a, b map[adapter.Socket][]netip.AddrPort) bool {
	return maps.EqualFunc(a, b, slices.Equal)
}

// Reconfigure refuses the other protocol's shape, any other type, nil, what
// Validate refuses, and a changed Port, the last with
// adapter.ErrPortChanged; each refusal leaves the server as it was: Addrs
// keeps what it held, and a connection accepted after negotiates the same
// cap, the snapshot never swapped. A check left out answers as another's
// error does, naming a field that is not its own; a snapshot swapped before
// a refusal moves the cap.
func TestReconfigureRefusesAndLeavesTheServerAsItWas(t *testing.T) {
	b := budgets()
	adaptertest.CheckLeaks(t, b)
	snap := drainOn("127.0.0.1")
	snap.MaxVersion = 2
	srv, conns := servingCapturing(t, b, snap)
	before := srv.Addrs()

	refused := snap
	refused.MaxVersion = 3
	for _, c := range []struct {
		name string
		want string // what the refusal it names says
		next settings.Settings
	}{
		{"no settings", "no settings", nil},
		{"a typed-nil settings", "no settings", (*testproto.Settings)(nil)},
		{"the other protocol's shape", "another protocol's settings", settings.SMB{}},
		{"any other type", "another protocol's settings", bogusShape{}},
		{"what Validate refuses", "max version 3", refused},
	} {
		err := srv.Reconfigure(context.Background(), c.next)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: Reconfigure(%+v) = %v, want refused naming %q", c.name, c.next, err, c.want)
			continue
		}
		if got := srv.Addrs(); !sameAddrs(got, before) {
			t.Errorf("%s: Addrs after the refusal %v, want the server as it was %v", c.name, got, before)
		}
		from := len(conns.list())
		if v := negotiateOn(t, srv, 2); v != 2 {
			t.Errorf("%s: a connection accepted after the refusal negotiates %d, want 2: the snapshot moved", c.name, v)
		}
		waitsFor(t, c.name+"'s served connection to be captured", func() bool {
			return len(conns.list()) > from
		})
		if got := conns.list()[from].Settings().(testproto.Settings).MaxVersion; got != 2 {
			t.Errorf("%s: a connection accepted after the refusal carries MaxVersion %d, want 2: the snapshot moved", c.name, got)
		}
	}
	portChanged := snap
	portChanged.Port = 2049
	portChanged.MaxVersion = 1
	if err := srv.Reconfigure(context.Background(), portChanged); !errors.Is(err, adapter.ErrPortChanged) {
		t.Errorf("a changed Port: Reconfigure = %v, want adapter.ErrPortChanged", err)
	}
	if got := srv.Addrs(); !sameAddrs(got, before) {
		t.Errorf("a changed Port: Addrs after the refusal %v, want the server as it was %v", got, before)
	}
	from := len(conns.list())
	if v := negotiateOn(t, srv, 2); v != 2 {
		t.Errorf("a changed Port: a connection accepted after the refusal negotiates %d, want 2", v)
	}
	waitsFor(t, "the port refusal's served connection to be captured", func() bool {
		return len(conns.list()) > from
	})
	if got := conns.list()[from].Settings().(testproto.Settings).MaxVersion; got != 2 {
		t.Errorf("a changed Port: a connection accepted after the refusal carries MaxVersion %d, want 2: the snapshot moved", got)
	}
}

// A live setting applies to connections set up after the change: a
// connection open before keeps the cap it negotiated, and one accepted after
// a MaxVersion change from 2 to 1 picks 1. A change refused for what it
// would reach, or a snapshot read live by the connection open before it,
// fails here.
func TestALiveSettingReachesConnectionsAcceptedAfterTheChange(t *testing.T) {
	b := budgets()
	adaptertest.CheckLeaks(t, b)
	cap2 := drainOn("127.0.0.1")
	cap2.MaxVersion = 2
	srv := servingOn(t, b, cap2)
	before := srv.Addrs()
	if v := negotiateOn(t, srv, 2); v != 2 {
		t.Fatalf("a connection before the change negotiates %d, want 2", v)
	}
	cap1 := cap2
	cap1.MaxVersion = 1
	if err := srv.Reconfigure(context.Background(), cap1); err != nil {
		t.Fatalf("Reconfigure to MaxVersion 1: %v", err)
	}
	if v := negotiateOn(t, srv, 2); v != 1 {
		t.Errorf("a connection accepted after the change negotiates %d, want 1", v)
	}
	if got := srv.Addrs(); !sameAddrs(got, before) {
		t.Errorf("a Reconfigure no address changes moved Addrs from %v to %v", before, got)
	}
}

// Adding an address binds it before Reconfigure returns: Addrs reports
// 127.0.0.1 and the added ::1 together, both at the one port the port-0
// listener holds, and a dial to each completes. A bind deferred past the
// return fails the dial; missing the port the listener holds fails the ports.
func TestAnAddedAddressBindsBeforeReconfigureReturns(t *testing.T) {
	needIPv6Loopback(t)
	b := budgets()
	adaptertest.CheckLeaks(t, b)
	srv := servingOn(t, b, drainOn("127.0.0.1"))
	if err := srv.Reconfigure(context.Background(), drainOn("127.0.0.1", "::1")); err != nil {
		t.Fatalf("Reconfigure adding ::1: %v", err)
	}
	got := srv.Addrs()[key("test")]
	if len(got) != 2 || got[0].Addr() != netip.MustParseAddr("127.0.0.1") || got[1].Addr() != netip.IPv6Loopback() || got[0].Port() != got[1].Port() {
		t.Fatalf("Addrs after adding ::1 = %v, want 127.0.0.1 and ::1 at one port, in the settings' order", got)
	}
	for _, a := range got {
		dialOK(t, a)
	}
}

// A hold on an added address fails Reconfigure with nothing changed: of the
// two added sockets the held port's is refused, so the other is closed behind
// it, no address is added, and the snapshot keeps its cap. A partial bind
// kept open, or a snapshot swapped before the bind that fails, fails the
// dials or the cap.
func TestAHeldPortOnAnAddedAddressChangesNothing(t *testing.T) {
	needIPv6Loopback(t)
	b := budgets()
	adaptertest.CheckLeaks(t, b)
	ls := []core.Listener{
		{Services: []string{"a"}, Network: "tcp", Protocol: testproto.New(adapter.Deps{})},
		{Services: []string{"b"}, Network: "tcp", Protocol: testproto.New(adapter.Deps{})},
	}
	snap := drainOn("127.0.0.1")
	snap.MaxVersion = 2
	srv, err := core.NewServer(adapter.Deps{Budgets: b}, snap, ls...)
	if err != nil {
		t.Fatal(err)
	}
	stopAtEnd(t, srv)
	if err := srv.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	before := srv.Addrs()
	pa, pb := before[key("a")][0].Port(), before[key("b")][0].Port()
	if pa == pb {
		t.Fatalf("the two port-0 listeners share a port, %d", pa)
	}
	held, lerr := net.Listen("tcp6", netip.AddrPortFrom(netip.IPv6Loopback(), pb).String())
	if lerr != nil {
		t.Skipf("::1's port cannot be held here: %v", lerr)
	}
	defer func() { _ = held.Close() }()

	// The refused shape lowers MaxVersion, so the cap a snapshot swapped
	// before the failing bind would store is a number the negotiation sees.
	next := drainOn("127.0.0.1", "::1")
	next.MaxVersion = 1
	if err := srv.Reconfigure(context.Background(), next); err == nil || !strings.Contains(err.Error(), netip.AddrPortFrom(netip.IPv6Loopback(), pb).String()) {
		t.Fatalf("Reconfigure over a held port: %v, want the refusal naming [::1]:%d", err, pb)
	}
	if got := srv.Addrs(); !sameAddrs(got, before) {
		t.Errorf("a failed Reconfigure moved Addrs from %v to %v", before, got)
	}
	// The half of the bind that worked, listener a's socket on ::1 at its
	// own port, closed behind the refusal; nothing stays bound at either
	// port of the added address.
	_ = held.Close()
	dialRefused(t, netip.AddrPortFrom(netip.IPv6Loopback(), pa))
	dialRefused(t, netip.AddrPortFrom(netip.IPv6Loopback(), pb))
	dialOK(t, before[key("a")][0])
	if v := negotiateAt(t, before[key("a")][0], 2); v != 2 {
		t.Errorf("a connection accepted after the refusal negotiates %d, want 2: the snapshot moved", v)
	}
}

// Removing an address closes its listeners and its connections, torn down
// before Reconfigure returns: the removed connection is out of the
// server's connection list before the call ends and its client's read sees
// the end, while the kept address answers and still dials, and its
// listeners close: a dial to the removed address no longer completes.
// Connections torn down after the return, or the kept address closed with
// them, fail here.
func TestARemovedAddressClosesItsConnectionsBeforeReconfigureReturns(t *testing.T) {
	b := budgets()
	adaptertest.CheckLeaks(t, b)

	// The second address is whichever of the loopback's the runtime owns:
	// 127.0.0.2 where its /8 aliasing is on, ::1 where it is not. With
	// neither the removal has no address to bind and the test skips.
	var second string
	probe, perr := net.Listen("tcp4", "127.0.0.2:0")
	if perr == nil {
		_ = probe.Close()
		second = "127.0.0.2"
	} else {
		needIPv6Loopback(t)
		second = "::1"
	}
	srv := servingOn(t, b, drainOn("127.0.0.1", second))
	at := srv.Addrs()[key("test")]
	if len(at) != 2 || at[0].Addr() != netip.MustParseAddr("127.0.0.1") || at[1].Addr() != netip.MustParseAddr(second) {
		t.Fatalf("Addrs %v, want 127.0.0.1 and %s", at, second)
	}
	kept, kerr := net.Dial("tcp", at[0].String())
	if kerr != nil {
		t.Fatal(kerr)
	}
	defer func() { _ = kept.Close() }()
	c := testproto.NewClient(kept)
	if kerr := kept.SetDeadline(time.Now().Add(10 * time.Second)); kerr != nil {
		t.Fatal(kerr)
	}
	gone, gerr := net.Dial("tcp", at[1].String())
	if gerr != nil {
		t.Fatal(gerr)
	}
	defer func() { _ = gone.Close() }()
	if gerr := gone.SetDeadline(time.Now().Add(10 * time.Second)); gerr != nil {
		t.Fatal(gerr)
	}
	waitsFor(t, "both connections to be registered", func() bool {
		n := 0
		for range srv.Connections() {
			n++
		}
		return n == 2
	})
	if err := srv.Reconfigure(context.Background(), drainOn("127.0.0.1")); err != nil {
		t.Fatalf("Reconfigure removing %s: %v", second, err)
	}
	// A connection's client address is the kept client's own, its local end.
	n, keeping := 0, false
	for spied := range srv.Connections() {
		n++
		if spied.Remote == kept.LocalAddr().(*net.TCPAddr).AddrPort() {
			keeping = true
		}
	}
	if n != 1 || !keeping {
		t.Fatalf("after Reconfigure %d connections are registered, the kept one among them %v, want it alone", n, keeping)
	}
	var buf [64]byte
	if m, rerr := gone.Read(buf[:]); m > 0 {
		t.Fatalf("the removed connection's read got %d bytes, want none", m)
	} else if rerr == nil {
		t.Fatal("the removed connection's read blocked at Reconfigure's return: its teardown had not ended")
	} else {
		var ne net.Error
		if errors.As(rerr, &ne) && ne.Timeout() {
			t.Fatal("the removed connection's read timed out at Reconfigure's return: its teardown had not ended")
		}
	}
	dialRefused(t, at[1])
	dialOK(t, at[0])
	if kerr := c.Send(testproto.Msg{Op: testproto.OpEcho, Version: 1, ID: 7, Payload: []byte("hi")}); kerr != nil {
		t.Fatal(kerr)
	}
	if rs, rerr := c.Recv(); rerr != nil || len(rs) != 1 || rs[0].ID != 7 || rs[0].Status != testproto.StatusOK {
		t.Errorf("the kept connection's Echo after the removal is answered %+v (%v)", rs, rerr)
	}
}

// While configured Reconfigure binds nothing and replaces the settings Start
// will bind: adding ::1 leaves the server bound nowhere and configured, and
// Start then binds what the reconfigured settings name. A bind before Start
// fails the empty Addrs; a reconfigure that changes nothing Start binds fails
// the ::1 dial.
func TestWhileConfiguredReconfigureBindsWhatStartWill(t *testing.T) {
	needIPv6Loopback(t)
	b := budgets()
	adaptertest.CheckLeaks(t, b)
	snap := drainOn("::1")
	snap.MaxVersion = 2
	srv, err := core.NewServer(adapter.Deps{Budgets: b}, snap, core.Listener{Services: []string{"test"}, Network: "tcp", Protocol: testproto.New(adapter.Deps{})})
	if err != nil {
		t.Fatal(err)
	}
	stopAtEnd(t, srv)
	if err := srv.Reconfigure(context.Background(), snap); err != nil {
		t.Fatalf("Reconfigure while configured: %v", err)
	}
	if h := srv.Health(); h.State != adapter.StateConfigured {
		t.Errorf("after a configured Reconfigure the server is %s, want configured", h.State)
	}
	if got := srv.Addrs(); len(got) != 0 {
		t.Errorf("a configured server's Addrs %v, want none: nothing is bound", got)
	}
	if err := srv.Start(t.Context()); err != nil {
		t.Fatalf("Start on the reconfigured settings: %v", err)
	}
	got := srv.Addrs()[key("test")]
	if len(got) != 1 || got[0].Addr() != netip.IPv6Loopback() {
		t.Fatalf("after Start Addrs = %v, want ::1 alone at the port bound", got)
	}
	dialOK(t, got[0])
	if v := negotiateOn(t, srv, 2); v != 2 {
		t.Errorf("a connection accepted after Start negotiates %d, want 2 from the reconfigured settings", v)
	}
}

// Reconfigure is refused while draining, naming the state: the drain holds
// on a connection, Health reports it, and freeing the room once the refusal is
// in lets the server stop. A draining server that binds what the call adds
// fails by refusing or by binding.
func TestAReconfigureWhileDrainingIsRefused(t *testing.T) {
	b := budgets()
	b.Reply = resources.NewWaitBudget("reply", chargeOfReply(t, 8))
	adaptertest.CheckLeaks(t, b)
	snap := drainOn("127.0.0.1")
	snap.MaxVersion = 2
	srv, p := servingCapturing(t, b, snap)
	c, _ := clientAt(t, testAddr(t, srv))
	conn := theConn(t, p)
	registered(t, srv, p)
	free := holdsRoom(t, b, 1)
	if err := c.Send(testproto.Msg{Op: testproto.OpEcho, Version: 1, ID: 7, Role: core.RoleRelease, Payload: []byte("hi")}); err != nil {
		t.Fatal(err)
	}
	waitsFor(t, "the Echo to wait for its reply room", func() bool {
		return pipelineRuns(conn) == 1
	})
	stopped := make(chan error, 1)
	go func() { stopped <- srv.Stop(context.Background()) }()
	waitsFor(t, "the server to drain", func() bool {
		return srv.Health().State == adapter.StateDraining
	})
	err := srv.Reconfigure(context.Background(), snap)
	if err == nil || !strings.Contains(err.Error(), "draining") {
		t.Fatalf("Reconfigure while draining: %v, want refused naming draining", err)
	}
	free()
	select {
	case err := <-stopped:
		if err != nil {
			t.Fatalf("Stop: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Stop never returned: the drain's quiet never ended it")
	}
	if h := srv.Health(); h.State != adapter.StateStopped {
		t.Errorf("after Stop the server is %s, want stopped", h.State)
	}
}

// A stopped server is never reconfigured: the call is refused naming the
// state. A bind on a stopped server fails by binding or by passing.
func TestAStoppedServerIsNeverReconfigured(t *testing.T) {
	b := budgets()
	adaptertest.CheckLeaks(t, b)
	snap := drainOn("127.0.0.1")
	snap.MaxVersion = 2
	srv := servingOn(t, b, snap)
	if err := srv.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	err := srv.Reconfigure(context.Background(), snap)
	if err == nil || !strings.Contains(err.Error(), "stopped") {
		t.Fatalf("Reconfigure on a stopped server: %v, want refused naming stopped", err)
	}
	if h := srv.Health(); h.State != adapter.StateStopped {
		t.Errorf("after the refusal the server is %s, want stopped", h.State)
	}
}

// gatedHandover is a protocol that stops at its first accepted connection,
// asking the test before it wires the stages: the accept loop then sits in
// its hand-over, which parks a Stop between the closure of its sockets and
// its final registry - the gap a Reconfigure arriving under the Stop must
// be refused across.
type gatedHandover struct {
	core.Protocol
	atHandover chan struct{}
	through    chan struct{}
}

func (p gatedHandover) NewConn(c *core.Conn) core.Stages {
	if c != nil {
		p.atHandover <- struct{}{}
		<-p.through
	}
	return p.Protocol.NewConn(c)
}

// A Reconfigure arriving while a Stop is ending its accept loops is refused
// as stopping, and the server ends stopped with no socket left and no
// address: a Stop moves the state under the server's lock before it closes
// a listener, so a Reconfigure that reaches the server across one meets
// the refusal naming the state the whole Stop window carries. A Stop a
// Reconfigure can run alongside takes this test's Reconfigure in its
// window: it binds what it names, and the refusal's absence, the address
// left listening and the dial completing each fail it.
func TestAReconfigureUnderAStopIsRefusedAsStopping(t *testing.T) {
	needIPv6Loopback(t)
	b := budgets()
	adaptertest.CheckLeaks(t, b)
	gate := gatedHandover{Protocol: testproto.New(adapter.Deps{}), atHandover: make(chan struct{}), through: make(chan struct{})}
	srv, err := core.NewServer(adapter.Deps{Budgets: b}, drainOn("127.0.0.1"), core.Listener{Services: []string{"test"}, Network: "tcp", Protocol: gate})
	if err != nil {
		t.Fatal(err)
	}
	stopAtEnd(t, srv)
	if err := srv.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	at := testAddr(t, srv)
	_, _ = clientAt(t, at)
	<-gate.atHandover // the first accepted connection parks in its hand-over

	// The gate lets go from the cleanup too, so a failure before the flow
	// reaches it cannot leave the teardown's Stop waiting a hand-over the
	// test holds.
	var openGate sync.Once
	release := func() { openGate.Do(func() { close(gate.through) }) }
	t.Cleanup(release)

	stopped := make(chan error, 1)
	go func() { stopped <- srv.Stop(context.Background()) }()
	waitsFor(t, "the Stop to close the listener", func() bool {
		c, derr := net.Dial("tcp", at.String())
		if derr != nil {
			return true
		}
		_ = c.Close()
		return false
	})
	err = srv.Reconfigure(context.Background(), drainOn("127.0.0.1", "::1"))
	if err == nil || !strings.Contains(err.Error(), "stopping") {
		t.Fatalf("Reconfigure under a Stop: %v, want refused naming stopping", err)
	}
	release()
	if err := <-stopped; err != nil {
		t.Fatalf("the parked Stop: %v", err)
	}
	if got := srv.Addrs(); len(got) != 0 {
		t.Errorf("after the stopped server's refused Reconfigure, Addrs = %v, want none", got)
	}
	six := netip.AddrPortFrom(netip.IPv6Loopback(), at.Port())
	dialRefused(t, six)
	dialRefused(t, at)
}

// A Reconfigure that took the lock first finishes what it names: the Stop
// behind it waits, then closes the socket that Reconfigure bound, and the
// server ends stopped with every address refusing. A Stop that skips the
// order, closing below what a Reconfigure under it had already bound, takes
// the dial that owes to refuse here.
func TestAStopWaitsThenClosesWhatAReconfigureBound(t *testing.T) {
	needIPv6Loopback(t)
	b := budgets()
	adaptertest.CheckLeaks(t, b)
	snap := drainOn("127.0.0.1")
	srv := servingOn(t, b, snap)
	before := srv.Addrs()
	inBind := make(chan struct{})
	throughBind := make(chan struct{})
	core.SetListen(srv, func(ctx context.Context, network, addr string) (net.Listener, error) {
		ln, err := net.Listen(network, addr)
		if err != nil {
			return nil, err
		}
		if strings.Contains(addr, "::1") {
			inBind <- struct{}{}
			<-throughBind
		}
		return ln, nil
	})

	reconfigured := make(chan error, 1)
	go func() { reconfigured <- srv.Reconfigure(context.Background(), drainOn("127.0.0.1", "::1")) }()
	<-inBind // the Reconfigure holds the lock, inside the address it adds

	var openBind sync.Once
	through := func() { openBind.Do(func() { close(throughBind) }) }
	t.Cleanup(through)

	stopped := make(chan error, 1)
	go func() { stopped <- srv.Stop(context.Background()) }()
	through()
	if err := <-reconfigured; err != nil {
		t.Fatalf("the Reconfigure that held the lock: %v", err)
	}
	if err := <-stopped; err != nil {
		t.Fatalf("the Stop that waited it out: %v", err)
	}
	if got := srv.Addrs(); len(got) != 0 {
		t.Errorf("after the Stop behind the Reconfigure, Addrs = %v, want none", got)
	}
	dialRefused(t, before[key("test")][0])
	dialRefused(t, netip.AddrPortFrom(netip.IPv6Loopback(), before[key("test")][0].Port()))
}

// One address spelt mapped and spelt plainly is one address: the Reconfigure
// that names the same set in the other spelling binds and drops nothing,
// where raw spellings read one add and one drop and the add failed on the
// port its own drop was about to free (bind and Validate already unmap).
func TestAMappedSpellingOfOneAddressReconfiguresAsItself(t *testing.T) {
	b := budgets()
	adaptertest.CheckLeaks(t, b)
	srv := servingOn(t, b, drainOn("::ffff:127.0.0.1"))
	before := srv.Addrs()
	if err := srv.Reconfigure(context.Background(), drainOn("127.0.0.1")); err != nil {
		t.Fatalf("Reconfigure with the plain spelling: %v", err)
	}
	if got := srv.Addrs(); !sameAddrs(got, before) {
		t.Errorf("a Reconfigure between spellings of one address moved it: %v -> %v", before, got)
	}
}

// An address spelt mapped closes when the settings drop it spelt plainly:
// the sweep matches what the address unmaps to, not the spelling the bound
// kept, so a respelled address cannot keep its socket serving past its
// removal, where the raw match leaves the listener accepting.
func TestARespelledAddressClosesWhenTheSettingsDropIt(t *testing.T) {
	b := budgets()
	adaptertest.CheckLeaks(t, b)
	srv := servingOn(t, b, drainOn("::ffff:127.0.0.1"))
	before := srv.Addrs()
	if err := srv.Reconfigure(context.Background(), drainOn("127.0.0.1")); err != nil {
		t.Fatalf("Reconfigure with the plain spelling: %v", err)
	}
	if err := srv.Reconfigure(context.Background(), drainOn()); err != nil {
		t.Fatalf("Reconfigure to no address: %v", err)
	}
	if got := srv.Addrs(); len(got) != 0 {
		t.Errorf("after dropping the address, Addrs = %v, want none", got)
	}
	dialRefused(t, before[key("test")][0])
}

// While a Stop runs, even over a configured server, its marker refuses a
// Start and a Reconfigure: a Start would bind loops Stop's snapshot never
// closes, and the refusal keeps the description's word.
func TestAStartAndAReconfigureAreRefusedWhileAConfiguredServerStops(t *testing.T) {
	b := budgets()
	adaptertest.CheckLeaks(t, b)
	srv, err := core.NewServer(adapter.Deps{Budgets: b}, settings.DefaultNFS(), core.Listener{Services: []string{"test"}, Network: "tcp", Protocol: testproto.New(adapter.Deps{})})
	if err != nil {
		t.Fatal(err)
	}
	core.Stopping(srv, make(chan struct{}))
	if err := srv.Start(context.Background()); err == nil {
		t.Error("a Start during a configured server's Stop returned nil")
	}
	if err := srv.Reconfigure(context.Background(), settings.DefaultNFS()); err == nil {
		t.Error("a Reconfigure during a configured server's Stop returned nil")
	}
}

// gatedListener is the first bind's socket, an injection: its Accept parks
// until the test opens the gate, holding the accept loop open through the
// listener's close, so a Reconfigure that removes it waits mid-sweep.
type gatedListener struct {
	gate chan struct{}
}

func (l *gatedListener) Accept() (net.Conn, error) {
	<-l.gate
	return nil, net.ErrClosed
}
func (l *gatedListener) Close() error   { return nil }
func (l *gatedListener) Addr() net.Addr { return &net.TCPAddr{} }

// A Reconfigure's sweep closes only the connections the bound it removed
// accepted: a second Reconfigure that adds the dropped address back binds
// its own socket, and the connection it accepted survives the first sweep,
// where matching by the address would close it (section 3.6).
func TestASweepClosesOnlyWhatItsDroppedBoundAccepted(t *testing.T) {
	b := budgets()
	adaptertest.CheckLeaks(t, b)
	gate := make(chan struct{})
	gated := &gatedListener{gate: gate}
	var real bool
	srv, err := core.NewServer(adapter.Deps{Budgets: b}, drainOn("::1"), core.Listener{Services: []string{"test"}, Network: "tcp", Protocol: testproto.New(adapter.Deps{})})
	if err != nil {
		t.Fatal(err)
	}
	core.SetListen(srv, func(ctx context.Context, network, addr string) (net.Listener, error) {
		if !real {
			real = true
			return gated, nil
		}
		return (&net.ListenConfig{}).Listen(ctx, network, addr)
	})
	stopAtEnd(t, srv)
	if err := srv.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- srv.Reconfigure(context.Background(), drainOn()) }()
	select {
	case err := <-done:
		t.Fatalf("the removal returned before the gated loop ended: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	if err := srv.Reconfigure(context.Background(), drainOn("::1")); err != nil {
		t.Fatalf("Reconfigure adding the address back: %v", err)
	}
	c, _ := clientAt(t, testAddr(t, srv))
	close(gate)
	if err := <-done; err != nil {
		t.Fatalf("the first Reconfigure: %v", err)
	}
	// The sweep closed only the connections the gated bound accepted: the
	// connection the later bind accepted answers after it, where matching
	// by the address would have closed it (section 3.6).
	if err := c.Send(testproto.Msg{Op: testproto.OpNegotiate, Version: 2, ID: 2}); err != nil {
		t.Fatalf("the negotiation after the sweep: %v", err)
	}
	if rs, err := c.Recv(); err != nil || len(rs) != 1 || len(rs[0].Body) != 1 {
		t.Fatalf("the connection the second bind accepted was closed by the sweep: %+v (%v)", rs, err)
	}
}
