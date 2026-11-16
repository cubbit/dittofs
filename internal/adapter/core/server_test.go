package core_test

import (
	"context"
	"errors"
	"maps"
	"net"
	"net/netip"
	"slices"
	"strings"
	"syscall"
	"testing"

	"github.com/cubbit/dittofs/internal/adapter/adaptertest"
	"github.com/cubbit/dittofs/internal/adapter/adaptertest/testproto"
	"github.com/cubbit/dittofs/internal/adapter/core"
	"github.com/cubbit/dittofs/pkg/adapter"
	"github.com/cubbit/dittofs/pkg/adapter/settings"
	"github.com/cubbit/dittofs/pkg/resources"
	"github.com/cubbit/dittofs/pkg/vfs/vfstest"
)

// newDeps is a server's dependencies, with the four budgets the leak check
// reads.
func newDeps() adapter.Deps {
	return adapter.Deps{VFS: vfstest.Null{}, Budgets: &resources.Budgets{
		Receive:    resources.NewWaitBudget("receive", 8<<20),
		Reply:      resources.NewWaitBudget("reply", 8<<20),
		Connection: resources.NewBudget("connection", 1<<20),
		Hold:       resources.NewBudget("hold", 1<<20),
	}}
}

// on is testproto's settings, binding each of addrs.
func on(addrs ...string) testproto.Settings {
	s := testproto.Settings{Adapter: settings.DefaultNFS().Adapter}
	for _, a := range addrs {
		s.Addrs = append(s.Addrs, netip.MustParseAddr(a))
	}
	return s
}

// tcp is a listener of p at port carrying services.
func tcp(p core.Protocol, port uint16, services ...string) core.Listener {
	return core.Listener{Services: services, Network: "tcp", Port: port, Protocol: p}
}

// key is the Socket service is reported under on tcp.
func key(service string) adapter.Socket { return adapter.Socket{Service: service, Network: "tcp"} }

// stopAtEnd stops srv at the test's end, before the leak check reads what
// is still open, so a test that fails early leaves no socket behind.
func stopAtEnd(t *testing.T, srv *core.Server) {
	t.Cleanup(func() { _ = srv.Stop(context.Background()) })
}

// needIPv6Loopback skips t where ::1 cannot be bound.
func needIPv6Loopback(t *testing.T) {
	t.Helper()
	ln, err := net.Listen("tcp6", "[::1]:0")
	if err != nil {
		t.Skipf("::1 cannot be bound here: %v", err)
	}
	_ = ln.Close()
}

// dialOK fails t unless a dial to a connects: a socket listens there. Nothing
// accepts, so the connection completes in the socket's backlog.
func dialOK(t *testing.T, a netip.AddrPort) {
	t.Helper()
	c, err := net.Dial("tcp", a.String())
	if err != nil {
		t.Errorf("dial %s: %v; want it to connect, a socket bound there", a, err)
		return
	}
	_ = c.Close()
}

// dialRefused fails t unless a dial to a is refused: nothing listens there.
func dialRefused(t *testing.T, a netip.AddrPort) {
	t.Helper()
	c, err := net.Dial("tcp", a.String())
	if err == nil {
		_ = c.Close()
		t.Errorf("dial %s connected; want it refused, nothing bound there", a)
		return
	}
	if !errors.Is(err, syscall.ECONNREFUSED) {
		t.Errorf("dial %s: %v; want connection refused", a, err)
	}
}

// NewServer refuses no listener, each malformed set of listeners, naming the
// listener, and a listener on udp as unsupported; a valid set, two listeners
// at port 0 among them, is a configured server bound nowhere.
func TestNewServerRefusesMalformedListeners(t *testing.T) {
	deps := newDeps()
	adaptertest.CheckLeaks(t, deps.Budgets)
	p := testproto.New(deps)
	set := on("127.0.0.1")

	for _, tc := range []struct {
		name        string
		ls          []core.Listener
		want        string // what the refusal names
		unsupported bool   // wraps errors.ErrUnsupported
	}{
		{"no listener", nil, "no listener", false},
		{"no service", []core.Listener{tcp(p, 0)}, "listener 0", false},
		{"an empty service", []core.Listener{tcp(p, 0, "a", "")}, "listener 0", false},
		{"a service twice", []core.Listener{tcp(p, 0, "a", "b", "a")}, "listener 0", false},
		{"two listeners on one Socket", []core.Listener{tcp(p, 0, "a"), tcp(p, 0, "b", "a")}, "listener 1", false},
		{"two listeners on one port", []core.Listener{tcp(p, 2049, "a"), tcp(p, 2049, "b")}, "listener 1", false},
		{"a udp listener", []core.Listener{tcp(p, 0, "a"), {Services: []string{"b"}, Network: "udp", Protocol: p}}, "listener 1", true},
	} {
		srv, err := core.NewServer(deps, set, tc.ls...)
		if err == nil || srv != nil {
			t.Errorf("%s: NewServer returned %v, %v; want it refused", tc.name, srv, err)
			continue
		}
		if !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: NewServer: %v; want the refusal to name %s", tc.name, err, tc.want)
		}
		if got := errors.Is(err, errors.ErrUnsupported); got != tc.unsupported {
			t.Errorf("%s: NewServer: %v; wraps errors.ErrUnsupported %v, want %v", tc.name, err, got, tc.unsupported)
		}
	}

	srv, err := core.NewServer(deps, set, tcp(p, 0, "a", "b"), tcp(p, 0, "c"), tcp(p, 2049, "d"))
	if err != nil {
		t.Fatalf("NewServer with a valid set: %v", err)
	}
	stopAtEnd(t, srv)
	if got := srv.Health().State; got != adapter.StateConfigured {
		t.Errorf("a new server is %s, want configured", got)
	}
	if got := srv.Addrs(); len(got) != 0 {
		t.Errorf("a new server's Addrs %v, want empty: nothing is bound before Start", got)
	}
}

// Start binds every listener on each address, on its family alone: two
// listeners on 127.0.0.1 and ::1 are four sockets, each service reported
// under its Socket at each address with the port bound there.
func TestStartBindsEachListenerOnEachAddress(t *testing.T) {
	needIPv6Loopback(t)
	deps := newDeps()
	adaptertest.CheckLeaks(t, deps.Budgets)
	p := testproto.New(deps)

	srv, err := core.NewServer(deps, on("127.0.0.1", "::1"), tcp(p, 0, "a", "b"), tcp(p, 0, "c"))
	if err != nil {
		t.Fatal(err)
	}
	stopAtEnd(t, srv)
	if err := srv.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	got := srv.Addrs()
	if len(got) != 3 || got[key("a")] == nil || got[key("b")] == nil || got[key("c")] == nil {
		t.Fatalf("Addrs %v, want a, b and c over tcp", got)
	}
	sockets := map[netip.AddrPort]bool{}
	for k, as := range got {
		if len(as) != 2 || as[0].Addr() != netip.MustParseAddr("127.0.0.1") || as[1].Addr() != netip.IPv6Loopback() {
			t.Errorf("Addrs[%v] %v, want 127.0.0.1 and ::1, in the settings' order", k, as)
			continue
		}
		for _, a := range as {
			if a.Port() == 0 {
				t.Errorf("Addrs[%v] has %v, want the port bound there", k, a)
			}
			sockets[a] = true
		}
	}
	if !slices.Equal(got[key("a")], got[key("b")]) {
		t.Errorf("Addrs: a at %v, b at %v; want one listener's services at its sockets", got[key("a")], got[key("b")])
	}
	if len(sockets) != 4 {
		t.Errorf("Addrs reports %d sockets %v, want 4: two listeners on two addresses", len(sockets), sockets)
	}
	for a := range sockets {
		dialOK(t, a)
	}
}

// A wildcard is bound on its own family: 0.0.0.0 is reported as itself, at
// the port bound, and that port answers no IPv6 dial.
func TestStartBindsAWildcardOnItsFamilyAlone(t *testing.T) {
	needIPv6Loopback(t)
	deps := newDeps()
	adaptertest.CheckLeaks(t, deps.Budgets)
	p := testproto.New(deps)

	srv, err := core.NewServer(deps, on("0.0.0.0"), tcp(p, 0, "a"))
	if err != nil {
		t.Fatal(err)
	}
	stopAtEnd(t, srv)
	if err := srv.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	got := srv.Addrs()[key("a")]
	if len(got) != 1 || got[0].Addr() != netip.IPv4Unspecified() || got[0].Port() == 0 {
		t.Fatalf("Addrs[a] %v, want 0.0.0.0 at the port bound", got)
	}
	port := got[0].Port()
	dialOK(t, netip.AddrPortFrom(netip.MustParseAddr("127.0.0.1"), port))
	dialRefused(t, netip.AddrPortFrom(netip.IPv6Loopback(), port))
}

// An IPv4-mapped address binds the IPv4 address it maps and is reported as
// the settings name it, at the port bound.
func TestStartBindsAMappedAddressAsIPv4(t *testing.T) {
	deps := newDeps()
	adaptertest.CheckLeaks(t, deps.Budgets)
	p := testproto.New(deps)

	srv, err := core.NewServer(deps, on("::ffff:127.0.0.1"), tcp(p, 0, "a"))
	if err != nil {
		t.Fatal(err)
	}
	stopAtEnd(t, srv)
	if err := srv.Start(t.Context()); err != nil {
		t.Fatalf("Start on ::ffff:127.0.0.1: %v", err)
	}

	got := srv.Addrs()[key("a")]
	if len(got) != 1 || got[0].Addr() != netip.MustParseAddr("::ffff:127.0.0.1") || got[0].Port() == 0 {
		t.Fatalf("Addrs[a] %v, want ::ffff:127.0.0.1 as the settings name it, at the port bound", got)
	}
	dialOK(t, netip.AddrPortFrom(netip.MustParseAddr("127.0.0.1"), got[0].Port()))
}

// With no address, Start binds nothing and serves: the server is ready, and
// Addrs is empty.
func TestStartWithNoAddressServesBoundNowhere(t *testing.T) {
	deps := newDeps()
	adaptertest.CheckLeaks(t, deps.Budgets)
	p := testproto.New(deps)

	srv, err := core.NewServer(deps, on(), tcp(p, 0, "a"))
	if err != nil {
		t.Fatal(err)
	}
	stopAtEnd(t, srv)
	if err := srv.Start(t.Context()); err != nil {
		t.Fatalf("Start with no address: %v, want it to serve", err)
	}
	if h := srv.Health(); h.State != adapter.StateServing || !h.Ready() {
		t.Errorf("Health %+v with no address, want serving and ready", h)
	}
	if got := srv.Addrs(); len(got) != 0 {
		t.Errorf("Addrs %v with no address, want empty", got)
	}
}

// NewServer keeps its own copy of the listeners: what the caller changes in
// its slice afterwards, a listener replaced or a service renamed in place,
// reaches neither the check nor Addrs.
func TestNewServerKeepsItsOwnListeners(t *testing.T) {
	deps := newDeps()
	adaptertest.CheckLeaks(t, deps.Budgets)
	p := testproto.New(deps)

	ls := []core.Listener{tcp(p, 0, "a"), tcp(p, 0, "b")}
	srv, err := core.NewServer(deps, on("127.0.0.1"), ls...)
	if err != nil {
		t.Fatal(err)
	}
	stopAtEnd(t, srv)
	ls[0].Services[0] = "b" // now two listeners name b over tcp, which NewServer refuses
	ls[1] = tcp(p, 0, "c")
	if err := srv.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	got := srv.Addrs()
	if len(got) != 2 || len(got[key("a")]) != 1 || len(got[key("b")]) != 1 {
		t.Errorf("Addrs %v after the caller changed its listeners, want a and b at one address each, as NewServer checked them", got)
	}
}

// A bind that fails fails Start, naming the listener and the address, and
// closes what Start opened: the server is configured and bound nowhere.
func TestStartThatFailsClosesWhatItOpened(t *testing.T) {
	deps := newDeps()
	adaptertest.CheckLeaks(t, deps.Budgets)
	p := testproto.New(deps)

	held, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = held.Close() })
	heldAt := held.Addr().(*net.TCPAddr).AddrPort()
	// The first listener's port, known so a dial can find its socket closed.
	free, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	freeAt := free.Addr().(*net.TCPAddr).AddrPort()
	_ = free.Close()

	srv, err := core.NewServer(deps, on("127.0.0.1"), tcp(p, freeAt.Port(), "a"), tcp(p, heldAt.Port(), "b"))
	if err != nil {
		t.Fatal(err)
	}
	stopAtEnd(t, srv)
	err = srv.Start(t.Context())
	if err == nil {
		t.Fatal("Start bound a port the test holds")
	}
	if !strings.Contains(err.Error(), `["b"]`) || !strings.Contains(err.Error(), heldAt.String()) {
		t.Errorf("Start: %v; want it to name the listener [\"b\"] and the address %s", err, heldAt)
	}
	if got := srv.Health().State; got != adapter.StateConfigured {
		t.Errorf("after a failed Start the server is %s, want configured", got)
	}
	if got := srv.Addrs(); len(got) != 0 {
		t.Errorf("after a failed Start Addrs is %v, want empty", got)
	}
	dialRefused(t, freeAt)
}

// Addrs hands out a fresh map each call, slices included: what a caller does
// to one leaves the next as it was.
func TestAddrsIsTheCallersOwn(t *testing.T) {
	deps := newDeps()
	adaptertest.CheckLeaks(t, deps.Budgets)
	p := testproto.New(deps)

	srv, err := core.NewServer(deps, on("127.0.0.1"), tcp(p, 0, "a", "b"))
	if err != nil {
		t.Fatal(err)
	}
	stopAtEnd(t, srv)
	if err := srv.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	first := srv.Addrs()
	want := make(map[adapter.Socket][]netip.AddrPort)
	for k, as := range first {
		want[k] = slices.Clone(as)
	}
	first[key("a")][0] = netip.AddrPort{}
	first[key("a")] = append(first[key("a")], netip.MustParseAddrPort("192.0.2.1:1"))
	delete(first, key("b"))
	first[key("x")] = nil

	if got := srv.Addrs(); !maps.EqualFunc(got, want, slices.Equal) {
		t.Errorf("Addrs after the caller changed the last one's map %v, want %v as before", got, want)
	}
}

// Stop closes every socket and moves to stopped; a second Stop returns nil,
// and a stopped server is never started again. Start on a serving server is
// refused too, naming the state, and the server is ready only while serving.
func TestStopTwiceAndStartAfterStop(t *testing.T) {
	deps := newDeps()
	adaptertest.CheckLeaks(t, deps.Budgets)
	p := testproto.New(deps)

	srv, err := core.NewServer(deps, on("127.0.0.1"), tcp(p, 0, "a"))
	if err != nil {
		t.Fatal(err)
	}
	stopAtEnd(t, srv)
	if h := srv.Health(); h.Ready() {
		t.Errorf("a configured server's Health %+v is ready, want not", h)
	}
	if err := srv.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if h := srv.Health(); h.State != adapter.StateServing || !h.Ready() {
		t.Errorf("a started server's Health %+v, want serving and ready", h)
	}
	if err := srv.Start(t.Context()); err == nil || !strings.Contains(err.Error(), "serving") {
		t.Errorf("Start on a serving server: %v; want it refused naming serving", err)
	}
	at := srv.Addrs()[key("a")]
	if len(at) != 1 {
		t.Fatalf("Addrs[a] %v, want one address", at)
	}

	if err := srv.Stop(t.Context()); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if h := srv.Health(); h.State != adapter.StateStopped || h.Ready() {
		t.Errorf("a stopped server's Health %+v, want stopped and not ready", h)
	}
	if got := srv.Addrs(); len(got) != 0 {
		t.Errorf("a stopped server's Addrs %v, want empty", got)
	}
	dialRefused(t, at[0])

	if err := srv.Stop(t.Context()); err != nil {
		t.Errorf("a second Stop: %v, want nil", err)
	}
	if err := srv.Start(t.Context()); err == nil || !strings.Contains(err.Error(), "stopped") {
		t.Errorf("Start after Stop: %v; want it refused naming stopped", err)
	}
	if got := srv.Addrs(); len(got) != 0 {
		t.Errorf("Addrs after a refused Start %v, want empty", got)
	}
}

// Stop on a configured server moves it to stopped, with nothing to drain, and
// it is never started.
func TestStopWhileConfigured(t *testing.T) {
	deps := newDeps()
	adaptertest.CheckLeaks(t, deps.Budgets)
	p := testproto.New(deps)

	srv, err := core.NewServer(deps, on("127.0.0.1"), tcp(p, 0, "a"))
	if err != nil {
		t.Fatal(err)
	}
	stopAtEnd(t, srv)
	if err := srv.Stop(t.Context()); err != nil {
		t.Fatalf("Stop on a configured server: %v", err)
	}
	if got := srv.Health().State; got != adapter.StateStopped {
		t.Errorf("after Stop the server is %s, want stopped", got)
	}
	if err := srv.Start(t.Context()); err == nil || !strings.Contains(err.Error(), "stopped") {
		t.Errorf("Start after Stop on a configured server: %v; want it refused naming stopped", err)
	}
	if got := srv.Addrs(); len(got) != 0 {
		t.Errorf("Addrs after a refused Start %v, want empty", got)
	}
}
